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

package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"sync"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/kubevirt/virt-platform-autopilot/pkg/assets"
	pkgcontext "github.com/kubevirt/virt-platform-autopilot/pkg/context"
	"github.com/kubevirt/virt-platform-autopilot/pkg/engine"
	"github.com/kubevirt/virt-platform-autopilot/pkg/overrides"
	"github.com/kubevirt/virt-platform-autopilot/pkg/resources"
	"github.com/kubevirt/virt-platform-autopilot/pkg/tlsprofile"
	"github.com/kubevirt/virt-platform-autopilot/pkg/util"
)

// PlatformReconciler reconciles the virt platform based on HCO state
type PlatformReconciler struct {
	client.Client
	Namespace        string
	tlsProfileEvents <-chan event.GenericEvent

	loader              *assets.Loader
	registry            *assets.Registry
	patcher             *engine.Patcher
	tombstoneReconciler *engine.TombstoneReconciler
	contextBuilder      *RenderContextBuilder
	conditionEvaluator  *assets.DefaultConditionEvaluator
	crdChecker          *util.CRDChecker
	eventRecorder       *util.EventRecorder
	watchedCRDs         map[string]bool    // Track CRDs we're watching to avoid restart loops
	watchedCRDsMu       sync.RWMutex       // Protects watchedCRDs from concurrent access
	shutdownFunc        context.CancelFunc // Graceful shutdown instead of os.Exit
	shutdownMu          sync.Mutex         // Protects shutdownFunc
}

// NewPlatformReconciler creates a new platform reconciler
// The apiReader enables object adoption (detecting and labeling unlabeled objects)
// For tests with fake clients, pass nil for apiReader
// The event recorder will be set automatically by SetupWithManager()
func NewPlatformReconciler(c client.Client, apiReader client.Reader, namespace string) (*PlatformReconciler, error) {
	loader := assets.NewLoader()

	registry, err := assets.NewRegistry(loader)
	if err != nil {
		return nil, fmt.Errorf("failed to create asset registry: %w", err)
	}

	return &PlatformReconciler{
		Client:              c,
		Namespace:           namespace,
		loader:              loader,
		registry:            registry,
		patcher:             engine.NewPatcher(c, apiReader, loader),
		tombstoneReconciler: engine.NewTombstoneReconciler(c, loader),
		contextBuilder:      NewRenderContextBuilder(c),
		conditionEvaluator:  &assets.DefaultConditionEvaluator{},
		crdChecker:          util.NewCRDChecker(apiReader), // Use apiReader (not cache-dependent)
		watchedCRDs:         make(map[string]bool),
	}, nil
}

// SetTLSProfileEvents supplies notifications emitted only after the metrics TLS
// controller has refreshed its in-memory APIServer policy cache.
func (r *PlatformReconciler) SetTLSProfileEvents(events <-chan event.GenericEvent) {
	r.tlsProfileEvents = events
}

// SetEventRecorder sets the event recorder for this reconciler
func (r *PlatformReconciler) SetEventRecorder(recorder *util.EventRecorder) {
	r.eventRecorder = recorder
	// Also set it on the patcher so it can emit events during reconciliation
	if r.patcher != nil {
		r.patcher.SetEventRecorder(recorder)
	}
	// Also set it on the tombstone reconciler for tombstone events
	if r.tombstoneReconciler != nil {
		r.tombstoneReconciler.SetEventRecorder(recorder)
	}
	// Also set it on the context builder for hardware detection events
	if r.contextBuilder != nil {
		r.contextBuilder.SetEventRecorder(recorder)
	}
}

// SetShutdownFunc sets the shutdown function for graceful operator restart
// This allows the reconciler to trigger graceful shutdown instead of os.Exit(0)
func (r *PlatformReconciler) SetShutdownFunc(shutdownFunc context.CancelFunc) {
	r.shutdownMu.Lock()
	defer r.shutdownMu.Unlock()
	r.shutdownFunc = shutdownFunc
}

// triggerShutdown initiates graceful shutdown (for CRD watch reconfiguration)
func (r *PlatformReconciler) triggerShutdown() {
	r.shutdownMu.Lock()
	defer r.shutdownMu.Unlock()
	if r.shutdownFunc != nil {
		r.shutdownFunc()
	} else {
		// Fallback to os.Exit if shutdown func not set (for tests or legacy usage)
		os.Exit(0)
	}
}

// Reconcile reconciles the virt platform
func (r *PlatformReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Reconciling virt platform",
		"namespace", req.Namespace,
		"name", req.Name,
	)

	// Get the HyperConverged instance
	hco, err := r.getHCO(ctx, req.NamespacedName)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("HCO not found, skipping reconciliation")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Track any HCO-level metrics TLS security profile override
	// (spec.security.tlsSecurityProfile). The metrics server reads the resolved
	// profile per new connection, so no restart or requeue is needed here.
	if changed, err := tlsprofile.SetHyperConvergedProfileFromUnstructured(hco); err != nil {
		logger.Error(err, "Failed to read spec.security.tlsSecurityProfile from HCO")
	} else if changed {
		logger.Info("Updated metrics TLS security profile override from HCO")
	}

	// Opt-out gate: the autopilot is GA and enabled by default. It stays idle only when
	// the annotation platform.kubevirt.io/autopilot on the HCO CR is explicitly set to
	// "false".
	if !overrides.IsAutopilotEnabled(hco) {
		logger.Info("Autopilot disabled via annotation, keeping idle.",
			"annotation", overrides.AnnotationAutopilotEnabled,
			"value", "false",
		)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Step 0: Process tombstones FIRST (before HCO reconciliation)
	logger.Info("Processing tombstones")
	deletedCount, err := r.tombstoneReconciler.ReconcileTombstones(ctx, hco)
	if err != nil {
		// Log error but don't fail reconciliation - tombstone cleanup is best-effort
		logger.Error(err, "Failed to process tombstones (continuing with reconciliation)")
	} else if deletedCount > 0 {
		logger.Info("Tombstone processing completed", "deleted", deletedCount)
	}

	// Step 1: Apply HCO golden config FIRST (reconcile_order: 0).
	logger.Info("Applying HCO golden configuration")
	if err := r.reconcileHCO(ctx, hco); err != nil {
		logger.Error(err, "Failed to reconcile HCO golden config")
		return ctrl.Result{}, err
	}

	// Re-fetch HCO to get effective state (after potential golden config application)
	if err := r.Get(ctx, req.NamespacedName, hco); err != nil {
		return ctrl.Result{}, err
	}

	// Step 2: Build RenderContext from effective HCO state
	logger.Info("Building render context from HCO state")
	renderCtx, err := r.contextBuilder.Build(ctx, hco)
	if err != nil {
		logger.Error(err, "Failed to build render context")
		return ctrl.Result{}, err
	}

	// Update condition evaluator with current context
	r.updateConditionEvaluator(hco, renderCtx)

	metricErr := r.reconcileDependencyMetrics(ctx)

	// Step 3: Reconcile all other assets in reconcile_order
	logger.Info("Reconciling platform assets")
	if err := r.reconcileAssets(ctx, renderCtx); err != nil {
		logger.Error(err, "Failed to reconcile assets")
		return ctrl.Result{}, err
	}
	if metricErr != nil {
		logger.Error(metricErr, "Dependency metric refresh incomplete, retrying reconciliation")
		return ctrl.Result{}, metricErr
	}

	logger.Info("Successfully reconciled virt platform")
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *PlatformReconciler) getHCO(ctx context.Context, name types.NamespacedName) (*unstructured.Unstructured, error) {
	hco := &unstructured.Unstructured{}
	hco.SetGroupVersionKind(pkgcontext.HCOGVK)

	err := r.Get(ctx, name, hco)
	return hco, err
}

// reconcileHCO applies the golden HCO configuration
func (r *PlatformReconciler) reconcileHCO(ctx context.Context, currentHCO *unstructured.Unstructured) error {
	logger := log.FromContext(ctx)

	// Get HCO asset from registry
	hcoAsset, err := r.registry.GetAsset("hco-golden-config")
	if err != nil {
		return fmt.Errorf("failed to get HCO asset: %w", err)
	}

	// Build minimal context for HCO rendering (use current HCO state)
	minimalCtx, err := r.contextBuilder.Build(ctx, currentHCO)
	if err != nil {
		return fmt.Errorf("failed to build context for HCO: %w", err)
	}

	// Reconcile HCO using Patched Baseline algorithm
	applied, err := r.patcher.ReconcileAsset(ctx, hcoAsset, minimalCtx)
	if err != nil {
		return fmt.Errorf("failed to reconcile HCO: %w", err)
	}

	if applied {
		logger.Info("Applied HCO golden configuration")
	} else {
		logger.V(1).Info("HCO golden configuration unchanged")
	}

	return nil
}

// reconcileDependencyMetrics updates kubevirt_autopilot_missing_dependency and
// kubevirt_autopilot_dependency_opted_in for all managed CRDs. CRD presence is always
// reported; the paired opt-in gauge lets alerts ignore features nobody enabled.
// Metric collection failures are logged, but returned after all assets have been
// reconciled so controller-runtime can retry the full reconciliation with backoff.
func (r *PlatformReconciler) reconcileDependencyMetrics(ctx context.Context) error {
	logger := log.FromContext(ctx)

	optInStates, err := r.registry.CRDOptInStates(ctx, r.conditionEvaluator)
	if err != nil {
		return fmt.Errorf("evaluate CRD opt-in states: %w", err)
	}

	var metricErrs []error
	for crdName, optedIn := range optInStates {
		installed, err := r.crdChecker.IsCRDInstalled(ctx, crdName)
		if err != nil {
			logger.Error(err, "Failed to check CRD availability, skipping dependency metric", "crd", crdName)
			metricErrs = append(metricErrs, fmt.Errorf("check CRD %q availability: %w", crdName, err))
			continue
		}

		r.crdChecker.ReportDependencyMetric(crdName, !installed, optedIn)
	}

	return stderrors.Join(metricErrs...)
}

// assetCRDsAvailable checks both the auto-detected RequiredCRD and the explicit GateCRD.
// Returns false (skip) if either CRD is absent or cannot be checked.
func (r *PlatformReconciler) assetCRDsAvailable(ctx context.Context, asset *assets.AssetMetadata, renderCtx *pkgcontext.RenderContext) bool {
	logger := log.FromContext(ctx)

	for _, entry := range []struct {
		crd  string
		desc string
	}{
		{asset.RequiredCRD, "CRD not installed, skipping asset (soft dependency)"},
		{asset.GateCRD, "Gate CRD not installed, skipping asset"},
	} {
		if entry.crd == "" {
			continue
		}
		installed, err := r.crdChecker.IsCRDInstalled(ctx, entry.crd)
		if err != nil {
			logger.Error(err, "Failed to check CRD availability, skipping asset",
				"asset", asset.Name,
				"crd", entry.crd,
			)
			return false
		}
		if !installed {
			logger.V(1).Info(entry.desc, "asset", asset.Name, "crd", entry.crd)
			if r.eventRecorder != nil {
				r.eventRecorder.CRDMissing(renderCtx.HCO, asset.Name, entry.crd)
			}
			return false
		}
	}
	return true
}

// reconcileAssets reconciles all non-HCO assets.
func (r *PlatformReconciler) reconcileAssets(ctx context.Context, renderCtx *pkgcontext.RenderContext) error {
	logger := log.FromContext(ctx)

	// Get all assets sorted by reconcile_order (HCO should be 0, others 1+)
	allAssets := r.registry.ListAssetsByReconcileOrder()

	// Filter out HCO (already reconciled) and check conditions
	var assetsToReconcile []assets.AssetMetadata
	for i := range allAssets {
		asset := &allAssets[i]

		// Skip HCO (already reconciled in step 1)
		if asset.ReconcileOrder == 0 {
			continue
		}

		if !r.assetCRDsAvailable(ctx, asset, renderCtx) {
			r.patcher.CleanupExcludedAsset(asset, renderCtx)
			continue
		}

		// Check if asset should be applied based on conditions
		shouldApply, err := r.registry.ShouldApply(ctx, asset, r.conditionEvaluator)
		if err != nil {
			logger.Error(err, "Failed to evaluate asset conditions, skipping",
				"asset", asset.Name,
			)
			continue
		}

		if !shouldApply {
			logger.V(1).Info("Asset conditions not met, skipping",
				"asset", asset.Name,
			)
			r.patcher.CleanupExcludedAsset(asset, renderCtx)
			continue
		}

		assetsToReconcile = append(assetsToReconcile, *asset)
	}

	// Reconcile all applicable assets
	appliedCount, err := r.patcher.ReconcileAssets(ctx, assetsToReconcile, renderCtx)
	logger.Info("Reconciled assets",
		"total", len(assetsToReconcile),
		"applied", appliedCount,
	)

	// Record reconciliation event
	if r.eventRecorder != nil && err == nil {
		r.eventRecorder.ReconcileSucceeded(renderCtx.HCO, appliedCount, len(assetsToReconcile))
	}

	return err
}

// updateConditionEvaluator updates the condition evaluator with current context
func (r *PlatformReconciler) updateConditionEvaluator(hco *unstructured.Unstructured, ctx *pkgcontext.RenderContext) {
	// Update hardware context
	r.conditionEvaluator.HardwareContext = ctx.Hardware.AsMap()

	// Extract feature gates from HCO
	r.conditionEvaluator.FeatureGates = resources.ExtractFeatureGates(hco)

	// Extract annotations from HCO
	r.conditionEvaluator.Annotations = hco.GetAnnotations()

	// Pass through available container images
	r.conditionEvaluator.Images = ctx.Images

	// Expose the raw HCO object for field inspection conditions
	r.conditionEvaluator.HCOObject = hco.Object

	r.conditionEvaluator.KVFeatureGates = ctx.KubeVirtFeatureGates

	// Topology for schedulable-master and other topology-gated assets
	if ctx.Topology != nil {
		r.conditionEvaluator.TopologyContext = ctx.Topology.AsMap()
	} else {
		r.conditionEvaluator.TopologyContext = nil
	}
}

// isManagedCRD checks if a CRD is required by at least one declared asset.
func (r *PlatformReconciler) isManagedCRD(crdName string) bool {
	return r.registry.IsManagedCRD(crdName)
}

func (r *PlatformReconciler) getAssetsByCRD(crdName string) []string {
	return r.registry.GetAssetsByCRD(crdName)
}

// isWatchedCRD safely checks if a CRD is currently being watched
func (r *PlatformReconciler) isWatchedCRD(crdName string) bool {
	r.watchedCRDsMu.RLock()
	defer r.watchedCRDsMu.RUnlock()
	return r.watchedCRDs[crdName]
}

// markCRDAsWatched safely marks a CRD as being watched
func (r *PlatformReconciler) markCRDAsWatched(crdName string) {
	r.watchedCRDsMu.Lock()
	defer r.watchedCRDsMu.Unlock()
	r.watchedCRDs[crdName] = true
}

// crdEventHandler handles CRD creation/update/deletion events
// On create/delete of managed CRDs: restart operator to reconfigure watches
// On update: invalidate cache and trigger reconciliation
func (r *PlatformReconciler) crdEventHandler(ctx context.Context) handler.EventHandler {
	logger := log.FromContext(ctx)

	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			crd, ok := e.Object.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return
			}

			// If this is a new managed CRD we aren't watching yet, restart to add watch
			if r.isManagedCRD(crd.Name) && !r.isWatchedCRD(crd.Name) {
				hco, err := r.getHCO(ctx, r.getHyperConvergedNamespacedName())
				if err == nil {

					r.eventRecorder.CRDDiscovered(hco, r.getAssetsByCRD(crd.Name), crd.Name)
				}

				logger.Info("New managed CRD created - restarting operator to configure watch for drift detection",
					"crd", crd.Name)
				// Trigger graceful shutdown so deployment restarts us with new watches
				r.triggerShutdown()
			}

			// For non-managed CRDs, just invalidate cache and trigger reconciliation
			r.crdChecker.InvalidateCache("")
			q.Add(reconcile.Request{
				NamespacedName: r.getHyperConvergedNamespacedName(),
			})
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			crd, ok := e.Object.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return
			}

			// If we were watching this CRD, restart to remove watch
			if r.isManagedCRD(crd.Name) && r.isWatchedCRD(crd.Name) {
				logger.Info("Watched CRD deleted - restarting operator to remove watch",
					"crd", crd.Name)
				// Trigger graceful shutdown so deployment restarts us without the watch
				r.triggerShutdown()
			}

			// For non-managed CRDs, just invalidate cache and trigger reconciliation
			r.crdChecker.InvalidateCache("")
			q.Add(reconcile.Request{
				NamespacedName: r.getHyperConvergedNamespacedName(),
			})
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			crd, ok := e.ObjectNew.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return
			}

			logger.Info("CRD updated, invalidating cache and triggering HCO reconciliation",
				"crd", crd.Name)

			// Invalidate cache and trigger reconciliation
			r.crdChecker.InvalidateCache("")
			q.Add(reconcile.Request{
				NamespacedName: r.getHyperConvergedNamespacedName(),
			})
		},
	}
}

func (r *PlatformReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := mgr.GetLogger().WithName("setup")
	ctx := context.Background()

	// Create unstructured object for HCO
	hco := &unstructured.Unstructured{}
	hco.SetGroupVersionKind(pkgcontext.HCOGVK)

	// Create unstructured object for KubeVirt
	kv := &unstructured.Unstructured{}
	kv.SetGroupVersionKind(pkgcontext.KVGVK)
	mcp := &unstructured.Unstructured{}
	mcp.SetAPIVersion("machineconfiguration.openshift.io/v1")
	mcp.SetKind("MachineConfigPool")

	// Build controller with HCO watch
	builder := ctrl.NewControllerManagedBy(mgr).
		For(hco).
		Watches(
			&apiextensionsv1.CustomResourceDefinition{},
			r.crdEventHandler(ctx),
		).
		Watches(kv, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: r.getHyperConvergedNamespacedName()}}
		})).Named("platform")

	// MCO is absent on non-OpenShift clusters. Avoid registering an unserved
	// GVK there, while still watching it immediately on OpenShift.
	mcpInstalled, err := r.crdChecker.IsCRDInstalled(ctx, "machineconfigpools.machineconfiguration.openshift.io")
	if err != nil {
		logger.Error(err, "Failed to check MCP CRD; staged MachineConfig updates will use periodic reconciliation")
	} else if mcpInstalled {
		builder = builder.Watches(mcp, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: r.getHyperConvergedNamespacedName()}}
		}))
	}

	// The metrics TLS controller sends this only after it has atomically updated
	// the in-memory APIServer policy. Re-rendering KME from the channel therefore
	// needs no second API read and cannot observe stale policy due to queue order.
	if r.tlsProfileEvents != nil {
		builder = builder.WatchesRawSource(source.Channel(r.tlsProfileEvents,
			handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: r.getHyperConvergedNamespacedName()}}
			}),
		))
	}

	// Dynamically add watches for every CRD required by a declared asset.
	// RequiredCRD is derived from the asset template at load time, so no separate
	// mapping needs to be maintained when adding new asset files.
	logger.Info("Discovering managed resource types to watch")

	seenCRDs := make(map[string]bool)
	for _, asset := range r.registry.ListAssets(nil) {
		crdName := asset.RequiredCRD
		if crdName == "" || seenCRDs[crdName] {
			continue
		}
		seenCRDs[crdName] = true

		// Check if CRD is installed
		installed, err := r.crdChecker.IsCRDInstalled(ctx, crdName)
		if err != nil {
			logger.Error(err, "Failed to check CRD", "crd", crdName)
			continue
		}

		if !installed {
			logger.Info("CRD not installed, skipping watch", "crd", crdName)
			continue
		}

		// Fetch CRD to get GVK information
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
			logger.Error(err, "Failed to fetch CRD", "crd", crdName)
			continue
		}

		// Use the preferred version for the watch
		var version string
		for _, v := range crd.Spec.Versions {
			if v.Storage {
				version = v.Name
				break
			}
		}
		if version == "" && len(crd.Spec.Versions) > 0 {
			version = crd.Spec.Versions[0].Name
		}

		// Construct GVK
		gvk := schema.GroupVersionKind{
			Group:   crd.Spec.Group,
			Version: version,
			Kind:    crd.Spec.Names.Kind,
		}

		// Create unstructured object for this type
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)

		// Add watch - enqueue HCO for reconciliation when these resources change
		logger.Info("Adding watch for managed resource type", "gvk", gvk.String())

		// Track that we're watching this CRD
		r.markCRDAsWatched(crdName)

		builder = builder.Watches(
			obj,
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				// All managed resources trigger HCO reconciliation
				return []reconcile.Request{
					{
						NamespacedName: r.getHyperConvergedNamespacedName(),
					},
				}
			}),
		)
	}

	return builder.Complete(r)
}

func (r *PlatformReconciler) getHyperConvergedNamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      pkgcontext.HCOName,
		Namespace: r.Namespace,
	}
}

// Ensure PlatformReconciler implements reconcile.Reconciler
var _ reconcile.Reconciler = &PlatformReconciler{}
