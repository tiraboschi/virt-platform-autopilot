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

package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	namespace = "kubevirt"
	subsystem = "autopilot"
)

var (
	// ComplianceStatus tracks whether each managed resource is in sync with desired state.
	// 1 = Synced (Golden State matches Live), 0 = Drifted/Sync Failed
	// This is the core health indicator used by the VirtPlatformAutopilotSyncFailed alert.
	ComplianceStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "compliance_status",
			Help:      "Compliance status of managed resources (1=synced, 0=drifted/failed)",
		},
		[]string{"kind", "name", "namespace"},
	)

	// ThrashingTotal counts reconciliation throttling events (token bucket exhaustion).
	// Increments when the "Reconcile Gate" is hit (update budget exhausted).
	// Indicates an active "Edit War" between the autopilot and external changes.
	ThrashingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "thrashing_total",
			Help:      "Total number of reconciliation throttling events (anti-thrashing gate hits)",
		},
		[]string{"kind", "name", "namespace"},
	)

	// PausedResources tracks resources currently paused due to edit wars.
	// 1 = paused (reconcile-paused annotation set), 0 = active (annotation removed)
	// This gauge provides a stable signal for alerting on ongoing edit wars.
	PausedResources = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "paused_resources",
			Help:      "Resources currently paused due to edit war detection (1=paused, 0=active)",
		},
		[]string{"kind", "name", "namespace"},
	)

	// CustomizationInfo tracks intentional deviations from the Golden State.
	// Always set to 1 when customization exists. Type indicates: patch, ignore, or unmanaged.
	// Useful for Support to see "Is this cluster stock or customized?" without digging into YAML.
	CustomizationInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "customization_info",
			Help:      "Tracks intentional customizations (always 1 when present). Type: patch/ignore/unmanaged",
		},
		[]string{"kind", "name", "namespace", "type"},
	)

	// MissingDependency tracks managed CRD availability (soft dependencies).
	// 1 if the CRD is missing, 0 if present.
	// Distinguishes "Broken" (asset failed) from "Not Installed" (CRD missing).
	// Pair with DependencyOptedIn to tell an absent CRD that matters from one
	// belonging to a feature the user never enabled.
	MissingDependency = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "missing_dependency",
			Help:      "Indicates missing optional CRDs (1=missing, 0=present)",
		},
		[]string{"group", "version", "kind"},
	)

	// DependencyOptedIn tracks whether the feature requiring a managed CRD is enabled.
	// 1 if enabled, 0 otherwise. Always-install assets count as enabled.
	// Emitted for the same GVK set as MissingDependency so the two join cleanly;
	// opt-in state is a value rather than a label because it changes at runtime,
	// and a label would orphan the previous series on every toggle.
	DependencyOptedIn = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "dependency_opted_in",
			Help:      "Indicates whether the feature requiring a managed CRD is enabled (1=enabled, 0=not enabled)",
		},
		[]string{"group", "version", "kind"},
	)

	// ReconcileDuration tracks how long asset reconciliation takes.
	// Measures SSA apply and rendering logic duration.
	// High latency implies API server stress or complex asset rendering.
	ReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "reconcile_duration_seconds",
			Help:      "Duration of asset reconciliation operations (rendering + SSA apply)",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0},
		},
		[]string{"kind", "name", "namespace"},
	)

	// TombstoneStatus tracks tombstone deletion status.
	// 1 = exists (not yet deleted), 0 = deleted, -1 = error, -2 = skipped (label mismatch)
	// Used by VirtPlatformAutopilotTombstoneStuck alert to detect stuck tombstone deletions.
	TombstoneStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "tombstone_status",
			Help:      "Tombstone deletion status (1=exists, 0=deleted, -1=error, -2=skipped)",
		},
		[]string{"kind", "name", "namespace"},
	)

	// MachineConfigUpdateStaged reports a desired MachineConfig update held for
	// the next already-running MCP rollout. One series is emitted per matching
	// pool, making shared MachineConfig fan-out visible.
	MachineConfigUpdateStaged = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Namespace: namespace, Subsystem: subsystem, Name: "machineconfig_update_staged", Help: "MachineConfig updates staged for an active MCP rollout (1=staged)"},
		[]string{"machineconfig", "pool"},
	)
	MachineConfigUpdateStagedSinceSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Namespace: namespace, Subsystem: subsystem, Name: "machineconfig_update_staged_since_seconds", Help: "Unix time when a MachineConfig update was staged"},
		[]string{"machineconfig", "pool"},
	)
)

const (
	// Tombstone status values
	TombstoneExists  = 1.0
	TombstoneDeleted = 0.0
	TombstoneError   = -1.0
	TombstoneSkipped = -2.0
)

func init() {
	// Register all metrics with controller-runtime's metrics registry
	// This registry is automatically exposed over HTTPS (mTLS) on :8443/metrics
	// by the manager
	metrics.Registry.MustRegister(
		ComplianceStatus,
		ThrashingTotal,
		PausedResources,
		CustomizationInfo,
		MissingDependency,
		DependencyOptedIn,
		ReconcileDuration,
		TombstoneStatus,
		MachineConfigUpdateStaged,
		MachineConfigUpdateStagedSinceSeconds,
	)
}

// SetCompliance sets the compliance status for a managed resource.
// status: 1 = synced, 0 = drifted/failed
func SetCompliance(obj *unstructured.Unstructured, status float64) {
	ComplianceStatus.WithLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
	).Set(status)
}

// IncThrashing increments the thrashing counter for a managed resource.
// Called when token bucket is exhausted (anti-thrashing gate triggered).
func IncThrashing(obj *unstructured.Unstructured) {
	ThrashingTotal.WithLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
	).Inc()
}

// SetCustomization records an intentional customization on a managed resource.
// customizationType: "patch", "ignore", or "unmanaged"
func SetCustomization(obj *unstructured.Unstructured, customizationType string) {
	CustomizationInfo.WithLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
		customizationType,
	).Set(1)
}

// ClearCompliance removes the compliance_status series for a resource.
// Called when an asset enters unmanaged mode or is excluded via disabled-resources
// so the metric is absent from /metrics rather than retaining a stale value
// from the last managed reconcile cycle.
func ClearCompliance(obj *unstructured.Unstructured) {
	ComplianceStatus.DeleteLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
	)
}

// ClearCustomization removes a customization metric when the annotation is removed.
func ClearCustomization(obj *unstructured.Unstructured, customizationType string) {
	CustomizationInfo.DeleteLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
		customizationType,
	)
}

// SetDependency reports both facts about a managed CRD for one GVK.
// group, version, kind: the GVK of the dependency.
// missing: whether the CRD is absent from the cluster.
// optedIn: whether the feature requiring it is enabled (opt-in condition satisfied
// or always-install).
// Both series are always written together so the alert join cannot see one
// without the other.
func SetDependency(group, version, kind string, missing bool, optedIn bool) {
	MissingDependency.WithLabelValues(group, version, kind).Set(boolToFloat(missing))
	DependencyOptedIn.WithLabelValues(group, version, kind).Set(boolToFloat(optedIn))
}

func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

// ObserveReconcileDuration records the duration of a reconciliation operation.
// Use with prometheus.NewTimer() for automatic duration tracking.
func ObserveReconcileDuration(obj *unstructured.Unstructured, duration time.Duration) {
	ReconcileDuration.WithLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
	).Observe(duration.Seconds())
}

// ReconcileDurationTimer returns a prometheus.Timer for measuring reconciliation duration.
// Usage:
//
//	timer := ReconcileDurationTimer(obj)
//	defer timer.ObserveDuration()
func ReconcileDurationTimer(obj *unstructured.Unstructured) *prometheus.Timer {
	return prometheus.NewTimer(ReconcileDuration.WithLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
	))
}

// SetTombstoneStatus sets the tombstone deletion status for a resource.
// status: TombstoneExists (1), TombstoneDeleted (0), TombstoneError (-1), TombstoneSkipped (-2)
func SetTombstoneStatus(obj *unstructured.Unstructured, status float64) {
	TombstoneStatus.WithLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
	).Set(status)
}

// SetPaused sets the paused state for a resource.
// Called when edit war is detected (paused=true) or when annotation is removed (paused=false).
func SetPaused(obj *unstructured.Unstructured, paused bool) {
	value := 0.0
	if paused {
		value = 1.0
	}
	PausedResources.WithLabelValues(
		obj.GetKind(),
		obj.GetName(),
		obj.GetNamespace(),
	).Set(value)
}

// DeleteAssetMetrics removes all per-asset metric series for a resource.
// Called when an asset is removed from the active set (allowlist change, CRD absent,
// condition no longer met) so stale series no longer appear in /metrics.
func DeleteAssetMetrics(kind, name, namespace string) {
	ComplianceStatus.DeleteLabelValues(kind, name, namespace)
	PausedResources.DeleteLabelValues(kind, name, namespace)
	ReconcileDuration.DeleteLabelValues(kind, name, namespace)
	for _, customizationType := range []string{"patch", "ignore", "unmanaged"} {
		CustomizationInfo.DeleteLabelValues(kind, name, namespace, customizationType)
	}
}

// SetMachineConfigUpdateStaged refreshes the durable staging state in metrics.
func SetMachineConfigUpdateStaged(machineConfig string, pools []string, stagedAt time.Time) {
	for _, pool := range pools {
		MachineConfigUpdateStaged.WithLabelValues(machineConfig, pool).Set(1)
		MachineConfigUpdateStagedSinceSeconds.WithLabelValues(machineConfig, pool).Set(float64(stagedAt.Unix()))
	}
}

// ClearMachineConfigUpdateStaged removes every pool series for a MachineConfig.
// Pool names are not retained in-memory deliberately; DeletePartialMatch is safe
// and also clears obsolete pools after selectors change.
func ClearMachineConfigUpdateStaged(machineConfig string) {
	MachineConfigUpdateStaged.DeletePartialMatch(prometheus.Labels{"machineconfig": machineConfig})
	MachineConfigUpdateStagedSinceSeconds.DeletePartialMatch(prometheus.Labels{"machineconfig": machineConfig})
}
