/*
Copyright 2026 The Virt Platform Autopilot Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kubevirt/virt-platform-autopilot/cmd/render"
	"github.com/kubevirt/virt-platform-autopilot/pkg/assets"
	pkgcontext "github.com/kubevirt/virt-platform-autopilot/pkg/context"
	"github.com/kubevirt/virt-platform-autopilot/pkg/controller"
	"github.com/kubevirt/virt-platform-autopilot/pkg/debug"
	"github.com/kubevirt/virt-platform-autopilot/pkg/engine"
	"github.com/kubevirt/virt-platform-autopilot/pkg/metricstls"
	"github.com/kubevirt/virt-platform-autopilot/pkg/tlsprofile"
	"github.com/kubevirt/virt-platform-autopilot/pkg/util"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(eventsv1.AddToScheme(scheme))

	// Register Unstructured for HCO GVK so the manager can use it in ByObject cache config
	// This avoids REST mapping queries that would fail if the CRD doesn't exist yet
	hcoGV := schema.GroupVersion{Group: pkgcontext.HCOGroup, Version: pkgcontext.HCOVersion}
	scheme.AddKnownTypes(hcoGV, &unstructured.Unstructured{})
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "virt-platform-autopilot",
		Short: "Automated platform configuration for KubeVirt workloads",
		Long: `virt-platform-autopilot automatically configures OpenShift/Kubernetes
clusters for optimal virtualization workload performance by managing
platform-level resources based on HyperConverged configuration.`,
	}

	// Add subcommands
	rootCmd.AddCommand(newRunCommand())
	rootCmd.AddCommand(render.NewRenderCommand())

	// Default to run command if no subcommand specified (backward compatibility)
	if len(os.Args) == 1 || (len(os.Args) > 1 && os.Args[1][0] == '-') {
		// No subcommand or starts with flag - use run command
		os.Args = append([]string{os.Args[0], "run"}, os.Args[1:]...)
	}

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// newRunCommand creates the run subcommand for the controller
func newRunCommand() *cobra.Command {
	var metricsAddr string
	var debugAddr string
	var enableLeaderElection bool
	var probeAddr string
	var namespace string
	var crdValidationTimeout time.Duration
	var enableDebugServer bool
	var development bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the platform autopilot controller",
		Long:  `Start the controller manager that watches HyperConverged resources and manages platform configuration.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runController(
				metricsAddr,
				debugAddr,
				probeAddr,
				namespace,
				enableLeaderElection,
				enableDebugServer,
				development,
				crdValidationTimeout,
			)
		},
	}

	cmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", ":8443",
		"The address the HTTPS metrics endpoint binds to. Set to \"0\" to disable.")
	cmd.Flags().StringVar(&debugAddr, "debug-bind-address", "127.0.0.1:8081", "The address the debug endpoint binds to (localhost only for security).")
	cmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8082", "The address the probe endpoint binds to.")
	cmd.Flags().BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	cmd.Flags().StringVar(&namespace, "namespace", "openshift-cnv",
		"The namespace where HyperConverged CR is located.")
	cmd.Flags().DurationVar(&crdValidationTimeout, "crd-validation-timeout", 10*time.Second,
		"Timeout for validating that required CRDs exist at startup.")
	cmd.Flags().BoolVar(&enableDebugServer, "enable-debug-server", true,
		"Enable debug HTTP server with /debug/render, /debug/exclusions, and /debug/features endpoints.")
	cmd.Flags().BoolVar(&development, "development", true,
		"Enable development mode logging.")

	return cmd
}

// buildMetricsServerOptions builds the controller-runtime metrics server options
// and reports whether the endpoint is served securely. The metrics endpoint is
// served over HTTPS with native mTLS; the serving certificate is minted by the
// OpenShift service-ca operator into an optional secret volume, which only exists
// after the metrics Service is reconciled on first install. If it is not mounted
// yet, the endpoint stays disabled (never falls back to plaintext or a
// self-signed cert) and is restarted once it appears.
func buildMetricsServerOptions(metricsAddr string, caPool *metricstls.ClientCAPool) (metricsserver.Options, bool) {
	opts := metricsserver.Options{BindAddress: metricsAddr}
	secure := metricsAddr != "0" && certFilesPresent(metricstls.ServingCertDir)
	if secure {
		opts.SecureServing = true
		opts.CertDir = metricstls.ServingCertDir
		opts.FilterProvider = metricstls.AllowPrometheusK8s
		opts.TLSOpts = []func(*tls.Config){metricstls.ConfigureServerTLS(caPool.Get)}
	} else if metricsAddr != "0" {
		setupLog.Info("metrics serving certificate not present yet; keeping the metrics endpoint disabled until service-ca mints it",
			"certDir", metricstls.ServingCertDir)
		opts.BindAddress = "0"
	}
	return opts, secure
}

// cacheByObjectExemptions builds the per-type cache exemptions that override the
// managed-by DefaultLabelSelector for unlabeled objects the operator must observe.
func cacheByObjectExemptions(hcoForCache, kvForCache, mcpForCache, apiServerForCache client.Object, apiServerCRDInstalled, mcpCRDInstalled bool) map[client.Object]cache.ByObject {
	byObject := map[client.Object]cache.ByObject{
		// Watch all HCOs (labeled or not) to adopt pre-existing ones
		hcoForCache: {
			Label: labels.Everything(),
		},
		// Watch all KubeVirt (labeled or not) to adopt pre-existing ones
		kvForCache: {
			Label: labels.Everything(),
		},
		// Watch all CRDs for soft dependency detection
		// CRDs are managed by other operators and won't have our label
		&apiextensionsv1.CustomResourceDefinition{}: {
			Label: labels.Everything(),
		},
		// Watch only the authoritative metrics client-CA ConfigMap in
		// kube-system so CA rotation refreshes the mTLS trust pool on
		// change. Scoping the informer to this single object keeps the
		// cache tiny and overrides the managed-by DefaultLabelSelector,
		// which would otherwise filter out this unlabeled ConfigMap.
		&corev1.ConfigMap{}: {
			Namespaces: map[string]cache.Config{
				metricstls.ClientCAConfigMapNamespace: {
					LabelSelector: labels.Everything(),
					FieldSelector: fields.OneTermEqualSelector(
						"metadata.name", metricstls.ClientCAConfigMapName),
				},
			},
		},
	}
	if mcpCRDInstalled {
		byObject[mcpForCache] = cache.ByObject{Label: labels.Everything()}
	}
	// Watch the cluster APIServer CR (unlabeled, cluster-scoped singleton) so the
	// metrics TLS security profile is refreshed on change instead of polled
	// (MetricsTLSReconciler). Only add it when the CRD exists: an unstructured
	// ByObject key forces the manager to restmap the kind at startup, which fails
	// on clusters without config.openshift.io (e.g. Kind).
	if apiServerCRDInstalled {
		byObject[apiServerForCache] = cache.ByObject{
			Label: labels.Everything(),
		}
	}
	return byObject
}

// runController starts the controller manager
func runController(
	metricsAddr string,
	debugAddr string,
	probeAddr string,
	namespace string,
	enableLeaderElection bool,
	enableDebugServer bool,
	development bool,
	crdValidationTimeout time.Duration,
) error {
	// Setup logging
	opts := zap.Options{
		Development: development,
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Create label selector for cache filtering
	// Only cache resources managed by this autopilot (reduces memory in large clusters)
	managedByRequirement, err := labels.NewRequirement(
		engine.ManagedByLabel,
		selection.Equals,
		[]string{engine.ManagedByValue},
	)
	if err != nil {
		setupLog.Error(err, "unable to create label selector")
		return err
	}
	managedBySelector := labels.NewSelector().Add(*managedByRequirement)

	// Create unstructured object for HCO cache configuration
	// We registered Unstructured with the HCO GVK in init(), so this won't require API queries
	hcoForCache := &unstructured.Unstructured{}
	hcoForCache.SetGroupVersionKind(pkgcontext.HCOGVK)

	// Create unstructured object for KubeVirt cache configuration
	kvForCache := &unstructured.Unstructured{}
	kvForCache.SetGroupVersionKind(pkgcontext.KVGVK)
	mcpForCache := &unstructured.Unstructured{}
	mcpForCache.SetAPIVersion("machineconfiguration.openshift.io/v1")
	mcpForCache.SetKind("MachineConfigPool")

	// Create unstructured object for the cluster APIServer CR cache configuration.
	// It backs the metrics-TLS watch (MetricsTLSReconciler); the informer is only
	// created when that watch is registered (OpenShift), so this entry is inert
	// on clusters without the config.openshift.io APIServer CRD.
	apiServerForCache := &unstructured.Unstructured{}
	apiServerForCache.SetGroupVersionKind(tlsprofile.APIServerGVK)

	restConfig := ctrl.GetConfigOrDie()

	// Seed the TLS security profile and the metrics client-CA pool before the
	// manager starts, using a direct (uncached) client, so the very first scrape
	// is served with the correct policy. Best-effort: absent objects (e.g. on a
	// non-OpenShift cluster) just leave the Intermediate default in place.
	caPool := &metricstls.ClientCAPool{}
	bootstrapClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create bootstrap client for TLS setup")
		return err
	}
	seedTLSState(context.Background(), bootstrapClient, caPool)

	// Determine whether the OpenShift APIServer CR exists (config.openshift.io).
	// Its cache exemption and the metrics-TLS watch are only wired when the CRD is
	// present; on other clusters (e.g. Kind) the manager must not try to restmap a
	// kind that does not exist, and the APIServer TLS profile stays at its default.
	apiServerCRDInstalled, err := util.NewCRDChecker(bootstrapClient).IsCRDInstalled(
		context.Background(), "apiservers.config.openshift.io")
	if err != nil {
		setupLog.Error(err, "failed to check for APIServer CRD; APIServer TLS profile changes will not be watched")
		apiServerCRDInstalled = false
	}
	mcpCRDInstalled, err := util.NewCRDChecker(bootstrapClient).IsCRDInstalled(
		context.Background(), "machineconfigpools.machineconfiguration.openshift.io")
	if err != nil {
		setupLog.Error(err, "failed to check for MachineConfigPool CRD; MachineConfig update coalescing watch will be disabled")
		mcpCRDInstalled = false
	}

	metricsOpts, secureMetrics := buildMetricsServerOptions(metricsAddr, caPool)

	byObject := cacheByObjectExemptions(hcoForCache, kvForCache, mcpForCache, apiServerForCache, apiServerCRDInstalled, mcpCRDInstalled)

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOpts,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "virt-platform-autopilot.kubevirt.io",
		Cache: cache.Options{
			// By default, only cache objects with our managed-by label
			// This dramatically reduces memory usage in large clusters
			DefaultLabelSelector: managedBySelector,
			// IMPORTANT: Exempt certain resource types from label filtering
			ByObject: byObject,
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		return err
	}

	// Validate HCO CRD exists before proceeding
	// This operator requires the HyperConverged CRD to be installed by OLM
	setupLog.Info("Validating HCO CRD exists", "timeout", crdValidationTimeout)
	crdChecker := util.NewCRDChecker(mgr.GetAPIReader())
	// Use a short-lived context for validation (not the signal handler context)
	validateCtx, cancel := context.WithTimeout(context.Background(), crdValidationTimeout)
	defer cancel()
	hcoCRDInstalled, err := crdChecker.IsCRDInstalled(validateCtx, "hyperconvergeds.hco.kubevirt.io")
	if err != nil {
		setupLog.Error(err, "failed to check for HCO CRD")
		return err
	}
	if !hcoCRDInstalled {
		setupLog.Error(nil, "HyperConverged CRD not found - this component requires the HCO CRD to be installed by OLM")
		return fmt.Errorf("HCO CRD not found")
	}
	setupLog.Info("HCO CRD validation passed")

	// Setup platform controller
	// The API reader bypasses cache to detect and adopt unlabeled objects
	reconciler, err := controller.NewPlatformReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		namespace,
	)
	if err != nil {
		setupLog.Error(err, "unable to create platform reconciler")
		return err
	}
	tlsProfileEvents := make(chan event.GenericEvent, 1)
	reconciler.SetTLSProfileEvents(tlsProfileEvents)

	// Setup event recorder
	eventRecorder := util.NewEventRecorder(
		mgr.GetEventRecorder("virt-platform-autopilot"),
	)
	reconciler.SetEventRecorder(eventRecorder)

	// Create cancellable context for graceful shutdown
	// This allows the reconciler to trigger shutdown instead of calling os.Exit(0)
	signalCtx := ctrl.SetupSignalHandler()
	ctx, cancel := context.WithCancel(signalCtx)
	reconciler.SetShutdownFunc(cancel)

	if err = reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to setup platform controller")
		return err
	}

	// Keep the metrics TLS policy fresh, or bootstrap it on first install.
	// These run on every replica (independent of leader election) because each
	// pod serves its own metrics endpoint.
	if secureMetrics {
		// Watch the cluster APIServer profile and the client-CA ConfigMap so TLS
		// policy / CA rotation take effect on change (per-connection reads then
		// pick up the refreshed state). The APIServer watch is only wired when its
		// CRD exists (OpenShift, determined above); elsewhere the client-CA
		// ConfigMap is watched alone and the APIServer profile stays at its default.
		tlsWatcher := controller.NewMetricsTLSReconciler(mgr.GetAPIReader(), caPool, apiServerCRDInstalled, tlsProfileEvents)
		if err := tlsWatcher.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to setup metrics TLS watcher")
			return err
		}
	} else if metricsAddr != "0" {
		// First install: wait for the service-ca serving cert to be mounted, then
		// trigger a graceful restart so the manager comes back up serving HTTPS.
		if err := mgr.Add(nonLeaderRunnable(func(runCtx context.Context) error {
			return waitForServingCertThenRestart(runCtx, cancel)
		})); err != nil {
			setupLog.Error(err, "unable to add serving-cert watcher")
			return err
		}
	}

	// Setup debug server if enabled
	if enableDebugServer {
		setupLog.Info("Starting debug server", "address", debugAddr)
		loader := assets.NewLoader()
		registry, err := assets.NewRegistry(loader)
		if err != nil {
			setupLog.Error(err, "unable to load asset registry for debug server")
			return err
		}

		debugServer := debug.NewServer(mgr.GetClient(), loader, registry)
		debugMux := http.NewServeMux()
		debugServer.InstallHandlers(debugMux)

		httpServer := &http.Server{
			Addr:    debugAddr,
			Handler: debugMux,
		}

		go func() {
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				setupLog.Error(err, "debug server failed")
			}
		}()

		// Shutdown debug server on context cancellation
		go func() {
			<-ctx.Done()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				setupLog.Error(err, "debug server shutdown failed")
			}
		}()
	}

	// Setup health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		return err
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		return err
	}

	return nil
}

// servingCertPollInterval is how often the first-install bootstrap checks whether
// the service-ca serving certificate has been mounted.
const servingCertPollInterval = 15 * time.Second

// seedTLSState reads the cluster TLS security profile and the metrics client CA
// into their caches. It seeds the state before the manager (and its watches)
// start so the very first scrape is served correctly; afterwards the
// MetricsTLSReconciler keeps it fresh on change. Failures are logged, not fatal:
// a missing APIServer CR leaves the Intermediate default, and a missing client
// CA leaves an empty pool (mTLS then rejects all clients until the CA is read on
// a later refresh).
func seedTLSState(ctx context.Context, c client.Reader, caPool *metricstls.ClientCAPool) {
	if _, err := tlsprofile.RefreshAPIServer(ctx, c); err != nil {
		setupLog.Info("could not read APIServer TLS security profile; using default", "reason", err.Error())
	}
	if err := metricstls.RefreshClientCA(ctx, c, caPool); err != nil {
		setupLog.Info("could not read metrics client CA", "reason", err.Error())
	}
}

// waitForServingCertThenRestart polls for the service-ca serving certificate and
// triggers a graceful shutdown once it is mounted, so the manager restarts with
// SecureServing enabled.
func waitForServingCertThenRestart(ctx context.Context, restart context.CancelFunc) error {
	ticker := time.NewTicker(servingCertPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if certFilesPresent(metricstls.ServingCertDir) {
				setupLog.Info("metrics serving certificate is now present; restarting to enable HTTPS metrics",
					"certDir", metricstls.ServingCertDir)
				restart()
				return nil
			}
		}
	}
}

// certFilesPresent reports whether a non-empty tls.crt and tls.key exist in dir.
func certFilesPresent(dir string) bool {
	for _, name := range []string{"tls.crt", "tls.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() == 0 {
			return false
		}
	}
	return true
}

// nonLeaderRunnable adapts a function to a manager.Runnable that runs on every
// replica regardless of leader election (each pod serves its own metrics).
type nonLeaderRunnable func(context.Context) error

func (f nonLeaderRunnable) Start(ctx context.Context) error { return f(ctx) }

func (nonLeaderRunnable) NeedLeaderElection() bool { return false }
