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

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kubevirt/virt-platform-autopilot/pkg/assets"
	pkgcontext "github.com/kubevirt/virt-platform-autopilot/pkg/context"
	"github.com/kubevirt/virt-platform-autopilot/pkg/observability"
	"github.com/kubevirt/virt-platform-autopilot/pkg/overrides"
	"github.com/kubevirt/virt-platform-autopilot/pkg/throttling"
	"github.com/kubevirt/virt-platform-autopilot/pkg/util"
)

// driftChecker abstracts drift detection to allow injection in tests.
type driftChecker interface {
	DetectDrift(ctx context.Context, desired, live *unstructured.Unstructured) (bool, error)
}

// Patcher implements the Patched Baseline algorithm
type Patcher struct {
	renderer          *Renderer
	applier           *Applier
	driftDetector     driftChecker
	throttle          *throttling.TokenBucket
	thrashingDetector *throttling.ThrashingDetector
	client            client.Client
	eventRecorder     *util.EventRecorder
}

// NewPatcher creates a new patcher
// The apiReader enables object adoption (detecting and labeling unlabeled objects)
// For tests with fake clients, pass nil for apiReader
func NewPatcher(c client.Client, apiReader client.Reader, loader *assets.Loader) *Patcher {
	renderer := NewRenderer(loader)
	renderer.SetClient(c) // Enable CRD introspection and object queries in templates

	return &Patcher{
		renderer:          renderer,
		applier:           NewApplier(c, apiReader),
		driftDetector:     NewDriftDetector(c),
		throttle:          throttling.NewTokenBucket(),
		thrashingDetector: throttling.NewThrashingDetector(),
		client:            c,
	}
}

// SetEventRecorder sets the event recorder for this patcher
func (p *Patcher) SetEventRecorder(recorder *util.EventRecorder) {
	p.eventRecorder = recorder
}

// CleanupExcludedAsset deletes per-asset Prometheus metrics for an asset that is no
// longer in the active set (allowlist narrowed, CRD removed, condition no longer met).
// It renders the template to discover the resource's kind/name/namespace, then calls
// DeleteAssetMetrics. Silently returns if rendering fails or yields nothing — the metric
// series either never existed or the template cannot resolve, both are safe to ignore.
func (p *Patcher) CleanupExcludedAsset(assetMeta *assets.AssetMetadata, renderCtx *pkgcontext.RenderContext) {
	desired, err := p.renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil || desired == nil {
		return
	}
	observability.DeleteAssetMetrics(desired.GetKind(), desired.GetName(), desired.GetNamespace())
	if desired.GetKind() == "MachineConfig" && desired.GroupVersionKind().Group == "machineconfiguration.openshift.io" && renderCtx.HCO != nil {
		if err := p.clearMachineConfigStage(context.Background(), renderCtx.HCO.GetNamespace(), desired.GetName()); err != nil {
			log.Log.V(1).Info("Failed to clear staged MachineConfig update for excluded asset", "name", desired.GetName(), "error", err)
		}
	}
}

// ReconcileAsset performs the full Patched Baseline algorithm for an asset
// Returns true if the asset was applied, false if skipped/unchanged
//
//nolint:gocognit // This function implements the 7-step Patched Baseline Algorithm which is inherently complex
func (p *Patcher) ReconcileAsset(ctx context.Context, assetMeta *assets.AssetMetadata, renderCtx *pkgcontext.RenderContext) (bool, error) {
	logger := log.FromContext(ctx)

	logger.V(1).Info("Reconciling asset",
		"name", assetMeta.Name,
		"path", assetMeta.Path,
		"component", assetMeta.Component,
	)

	// Step 1: Render asset template → Opinionated State
	desired, err := p.renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil {
		return false, fmt.Errorf("failed to render asset %s: %w", assetMeta.Name, err)
	}

	// Handle conditional assets that don't apply (template rendered empty)
	if desired == nil {
		logger.V(1).Info("Asset not applicable (conditions not met)",
			"name", assetMeta.Name,
		)
		return false, nil
	}

	// Root Exclusion: Check if this resource is explicitly disabled via annotation
	disabledAnnotation := renderCtx.HCO.GetAnnotations()[DisabledResourcesAnnotation]
	if disabledAnnotation != "" {
		rules, err := ParseDisabledResources(disabledAnnotation)
		if err != nil {
			logger.Error(err, "Invalid disabled-resources annotation, ignoring",
				"annotation", disabledAnnotation,
			)
		} else if IsResourceExcluded(desired.GetKind(), desired.GetNamespace(), desired.GetName(), rules) {
			logger.Info("Skipping resource due to Root Exclusion",
				"kind", desired.GetKind(),
				"namespace", desired.GetNamespace(),
				"name", desired.GetName(),
				"annotation", DisabledResourcesAnnotation,
			)
			// Clear metrics that would be misleading if stale:
			// - compliance_status: staying at 1 would imply the operator is
			//   actively verifying the asset, which it is not.
			// - paused_resources: staying at 1 would imply an active edit war.
			// customization_info is intentionally left untouched: the underlying
			// annotations persist on the object while excluded, so the metric
			// accurately reflects their presence. When exclusion is lifted the
			// managed reconcile re-evaluates them from scratch.
			observability.ClearCompliance(desired)
			observability.SetPaused(desired, false)
			return false, nil
		}
	}

	// Start reconciliation duration timer (will be observed at function exit)
	timer := observability.ReconcileDurationTimer(desired)
	defer timer.ObserveDuration()

	// Get live object from cluster
	// First try cached Get, then fall back to direct API call for adoption scenarios
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(desired.GroupVersionKind())
	objKey := client.ObjectKey{
		Namespace: desired.GetNamespace(),
		Name:      desired.GetName(),
	}

	err = p.applier.Get(ctx, objKey, live)
	liveExists := err == nil

	if errors.IsNotFound(err) {
		// Object not found in cache - might be unlabeled and filtered out
		// Try direct API call to check if it exists but lacks managed-by label
		directLive := &unstructured.Unstructured{}
		directLive.SetGroupVersionKind(desired.GroupVersionKind())
		directErr := p.applier.GetDirect(ctx, objKey, directLive)

		if directErr == nil {
			// Object exists but was not in cache (probably unlabeled)
			// We'll adopt it by adding the label during Apply
			logger.V(1).Info("Adopting unlabeled object",
				"kind", desired.GetKind(),
				"namespace", desired.GetNamespace(),
				"name", desired.GetName(),
			)
			live = directLive
			liveExists = true
		} else if !errors.IsNotFound(directErr) {
			// Some other error occurred
			return false, fmt.Errorf("failed to get live object: %w", directErr)
		}
		// If directErr is NotFound, object truly doesn't exist
	} else if err != nil {
		// Some other error occurred during cached Get
		return false, fmt.Errorf("failed to get live object: %w", err)
	}

	// Log if we need to re-label an existing object
	if liveExists && !HasManagedByLabel(live) {
		logger.V(1).Info("Re-labeling object with managed-by label",
			"kind", desired.GetKind(),
			"namespace", desired.GetNamespace(),
			"name", desired.GetName(),
		)
	}

	// Step 1.5: Check if reconciliation is paused due to edit war
	if liveExists && overrides.IsPaused(live) {
		logger.Info("Reconciliation paused due to edit war detection",
			"name", assetMeta.Name,
			"kind", desired.GetKind(),
			"namespace", desired.GetNamespace(),
			"objectName", desired.GetName(),
		)
		// Repopulate the gauge every reconcile: gauge.Set(1) is idempotent and has
		// zero side effects. Without this, the metric is absent after pod restart
		// because all in-memory gauges are lost on restart (CNV-94143).
		observability.SetPaused(desired, true)
		return false, nil
	}

	// Step 1.6: Clear stale thrashing state when the user removes the pause annotation.
	// If in-memory state already reached the threshold (meaning the operator previously
	// set the pause annotation) but the annotation is now absent, the user intentionally
	// resumed reconciliation. Reset both the thrashing counter and the token bucket
	// so the next cycle starts clean and does not immediately re-trigger a pause.
	if liveExists {
		earlyKey := throttling.MakeResourceKey(
			desired.GetNamespace(),
			desired.GetName(),
			desired.GetKind(),
		)
		if p.thrashingDetector.GetAttempts(earlyKey) >= throttling.ThrashingThreshold {
			logger.Info("Pause annotation removed by user, resetting thrashing state",
				"name", assetMeta.Name,
				"key", earlyKey,
			)
			p.thrashingDetector.Reset(earlyKey)
			p.throttle.Reset(earlyKey)
		}
	}

	// Clear all stale customization series before re-evaluating this cycle.
	// SetCustomization is additive; without this, a series set in a prior reconcile
	// (e.g. "unmanaged") survives at 1 indefinitely after the annotation is removed
	// and the asset transitions to a different type.
	if liveExists {
		for _, ct := range []string{"patch", "ignore", "unmanaged"} {
			observability.ClearCustomization(desired, ct)
		}
	}

	// Step 2: Check opt-out annotation (mode: unmanaged)
	if liveExists && overrides.IsUnmanaged(live) {
		logger.V(1).Info("Asset is unmanaged, skipping",
			"name", assetMeta.Name,
			"kind", desired.GetKind(),
			"namespace", desired.GetNamespace(),
			"objectName", desired.GetName(),
		)
		// Clear stale managed-state metrics so they don't persist across an
		// unmanaged period or a paused→unmanaged transition.
		observability.ClearCompliance(desired)
		observability.SetPaused(desired, false)
		// Track unmanaged customization
		observability.SetCustomization(desired, "unmanaged")

		// Record event about unmanaged mode
		if p.eventRecorder != nil && renderCtx.HCO != nil {
			p.eventRecorder.UnmanagedMode(renderCtx.HCO, desired.GetKind(), desired.GetNamespace(), desired.GetName())
		}
		return false, nil
	}

	// Step 3: Apply user patch (in-memory) → Modified State
	// Copy patch annotation from live to desired, then apply it
	if liveExists {
		liveAnnotations := live.GetAnnotations()
		if patchStr, exists := liveAnnotations[overrides.PatchAnnotation]; exists && patchStr != "" {
			// Track patch customization
			observability.SetCustomization(desired, "patch")

			// Copy patch annotation to desired temporarily
			desiredAnnotations := desired.GetAnnotations()
			if desiredAnnotations == nil {
				desiredAnnotations = make(map[string]string)
			}
			desiredAnnotations[overrides.PatchAnnotation] = patchStr
			desired.SetAnnotations(desiredAnnotations)

			// Validate patch security before applying
			if err := overrides.ValidateAnnotations(desired); err != nil {
				logger.Error(err, "Patch validation failed, using desired without patch",
					"name", assetMeta.Name,
					"kind", desired.GetKind(),
				)
				// Record event about invalid/insecure patch
				if p.eventRecorder != nil && renderCtx.HCO != nil {
					p.eventRecorder.InvalidPatch(renderCtx.HCO, desired.GetKind(), desired.GetNamespace(), desired.GetName(), err.Error())
				}
				// Remove invalid patch annotation and continue with unpatched desired
				delete(desiredAnnotations, overrides.PatchAnnotation)
				desired.SetAnnotations(desiredAnnotations)
			} else {
				// Apply the patch (modifies desired in-place)
				patched, err := overrides.ApplyJSONPatch(desired)
				if err != nil {
					logger.Error(err, "Failed to apply JSON patch, using desired without patch",
						"name", assetMeta.Name,
					)
					// Record event about invalid patch
					if p.eventRecorder != nil && renderCtx.HCO != nil {
						p.eventRecorder.InvalidPatch(renderCtx.HCO, desired.GetKind(), desired.GetNamespace(), desired.GetName(), err.Error())
					}
					// Continue with unpatched desired (don't fail reconciliation)
				} else if patched && p.eventRecorder != nil && renderCtx.HCO != nil {
					// Record successful patch application
					// Count operations in the patch string
					operations := countJSONPatchOperations(patchStr)
					p.eventRecorder.PatchApplied(renderCtx.HCO, desired.GetKind(), desired.GetNamespace(), desired.GetName(), operations)
				}
			}
		}
	}

	// Step 4: Mask ignored fields → Effective Desired State
	if liveExists {
		// Check if ignore-fields annotation exists
		liveAnnotations := live.GetAnnotations()
		if _, exists := liveAnnotations[overrides.AnnotationIgnoreFields]; exists {
			// Track ignore-fields customization
			observability.SetCustomization(desired, "ignore")
		}

		desired, err = overrides.MaskIgnoredFields(desired, live)
		if err != nil {
			if p.eventRecorder != nil && renderCtx.HCO != nil {
				p.eventRecorder.InvalidIgnoreFields(renderCtx.HCO, live.GetKind(), live.GetNamespace(), live.GetName(), err.Error())
			}
			return false, fmt.Errorf("failed to mask ignored fields: %w", err)
		}
	}

	// Step 5: Drift detection
	// Ensure the managed-by label is present on desired before comparison,
	// mirroring what Applier.Apply() does before the actual SSA apply.
	// Without this the label would always appear as a spurious diff.
	ensureManagedByLabel(desired)

	hasDrift := false
	if liveExists {
		hasDrift, err = p.driftDetector.DetectDrift(ctx, desired, live)
		if err != nil {
			// SSA dry-run failed due to an infrastructure error (e.g. webhook TLS issue,
			// admission webhook unreachable). Do NOT fall back to SimpleDriftCheck here:
			// SimpleDriftCheck compares the minimal rendered template against the fully
			// webhook-defaulted live object, so it would always report drift and cause a
			// false-positive cascade into the thrashing detector.
			// Propagate the error so controller-runtime retries with proper backoff.
			logger.Info("SSA dry-run failed, skipping reconciliation until resolved",
				"error", err.Error(),
			)
			// Mark compliance as failed: the operator cannot verify or enforce the desired
			// state while drift detection is broken, so the metric must signal degraded
			// status to allow VirtPlatformAutopilotSyncFailed to fire.
			observability.SetCompliance(desired, 0)
			return false, fmt.Errorf("drift detection failed: %w", err)
		}
	} else {
		// Object doesn't exist - needs creation
		hasDrift = true
	}

	if !hasDrift {
		logger.V(1).Info("No drift detected, skipping apply",
			"name", assetMeta.Name,
		)
		observability.SetCompliance(desired, 1)
		observability.SetPaused(desired, false)
		return false, nil
	}

	// Existing MachineConfigs are expensive because each update can reboot a
	// pool. Stage them until an MCP is already Updating, unless explicitly
	// bypassed. New MachineConfigs remain immediate.
	if liveExists {
		apply, err := p.coalesceMachineConfigUpdate(ctx, desired, live, renderCtx)
		if err != nil {
			return false, fmt.Errorf("machineconfig rollout coalescing: %w", err)
		}
		if !apply {
			// A staged update is an intentional, observable deferral rather than a
			// failed reconciliation. Its dedicated metric carries the pending work;
			// do not fire the generic sync-failed alert for it.
			observability.SetCompliance(desired, 1)
			return false, nil
		}
	}

	// Record drift detection (only when drift is found)
	if liveExists && p.eventRecorder != nil && renderCtx.HCO != nil {
		p.eventRecorder.DriftDetected(renderCtx.HCO, desired.GetKind(), desired.GetNamespace(), desired.GetName())
	}

	// Pre-Step 6: verify the target namespace exists before consuming a rate-limit token.
	if ns := desired.GetNamespace(); ns != "" {
		nsObj := &unstructured.Unstructured{}
		nsObj.SetAPIVersion("v1")
		nsObj.SetKind("Namespace")
		if nsErr := p.client.Get(ctx, client.ObjectKey{Name: ns}, nsObj); nsErr != nil {
			if errors.IsNotFound(nsErr) {
				logger.Info("Target namespace not found, skipping asset (operator likely not installed)",
					"name", assetMeta.Name,
					"namespace", ns,
				)
				return false, nil
			}
			return false, fmt.Errorf("failed to verify target namespace %s: %w", ns, nsErr)
		}
	}

	// Step 6: Anti-thrashing gate (two-level protection)
	resourceKey := throttling.MakeResourceKey(
		desired.GetNamespace(),
		desired.GetName(),
		desired.GetKind(),
	)

	if err := p.throttle.Record(resourceKey); err != nil {
		if throttling.IsThrottled(err) {
			// Token bucket exhausted - check thrashing detector
			shouldPause := p.thrashingDetector.RecordThrottle(resourceKey)

			if shouldPause {
				// Edit war detected - pause reconciliation
				logger.Info("Edit war detected, pausing reconciliation",
					"name", assetMeta.Name,
					"key", resourceKey,
					"attempts", p.thrashingDetector.GetAttempts(resourceKey),
				)

				// Emit metric and event only once when the threshold is first reached.
				// ShouldEmitMetric is a one-shot gate per episode; reuse it for the
				// event so a single edit war never floods the event store.
				if p.thrashingDetector.ShouldEmitMetric(resourceKey) {
					observability.IncThrashing(desired)
					if p.eventRecorder != nil && renderCtx.HCO != nil {
						p.eventRecorder.ThrashingDetected(
							renderCtx.HCO,
							desired.GetKind(),
							desired.GetNamespace(),
							desired.GetName(),
							p.thrashingDetector.GetAttempts(resourceKey),
						)
					}
				}

				// Set pause annotation on live object
				if liveExists {
					if err := p.setPauseAnnotation(ctx, live); err != nil {
						logger.Error(err, "Failed to set pause annotation", "key", resourceKey)
						// Continue anyway - operator will retry
					} else {
						// Record that this resource is now paused
						observability.SetPaused(desired, true)
					}
				}

				return false, fmt.Errorf("reconciliation paused due to edit war (threshold: %d throttles)", throttling.ThrashingThreshold)
			}

			// First or second throttle - log and continue
			logger.Info("Asset update throttled (anti-thrashing)",
				"name", assetMeta.Name,
				"key", resourceKey,
				"attempts", p.thrashingDetector.GetAttempts(resourceKey),
			)

			// Record throttling event (short-term rate limiting)
			if p.eventRecorder != nil && renderCtx.HCO != nil {
				throttledErr := err.(*throttling.ThrottledError)
				window := throttledErr.Window.String()
				p.eventRecorder.Throttled(renderCtx.HCO, desired.GetKind(), desired.GetNamespace(), desired.GetName(), throttledErr.Capacity, window)
			}
			return false, err
		}
		return false, err
	}

	// Step 7: Apply via Server-Side Apply
	applied, err := p.applier.Apply(ctx, desired, true)
	if err != nil {
		// If the target namespace doesn't exist, the operator is not installed —
		// the CRD is just a leftover. Treat this as a soft skip, same as a missing CRD.
		if desired.GetNamespace() != "" && isNamespaceNotFound(err) {
			logger.Info("Target namespace not found, skipping asset (operator likely not installed)",
				"name", assetMeta.Name,
				"namespace", desired.GetNamespace(),
			)
			return false, nil
		}

		// Set compliance status to failed (0)
		observability.SetCompliance(desired, 0)

		// Record apply failure event
		if p.eventRecorder != nil && renderCtx.HCO != nil {
			p.eventRecorder.ApplyFailed(renderCtx.HCO, assetMeta.Name, err.Error())
		}
		return false, fmt.Errorf("failed to apply asset %s: %w", assetMeta.Name, err)
	}

	if applied {
		logger.Info("Successfully applied asset",
			"name", assetMeta.Name,
			"kind", desired.GetKind(),
			"namespace", desired.GetNamespace(),
			"objectName", desired.GetName(),
		)
		// Set compliance status to synced (1)
		observability.SetCompliance(desired, 1)

		// Reset thrashing detector - successful reconciliation resolves edit war
		p.thrashingDetector.RecordSuccess(resourceKey)

		// Clear paused state - resource is now active
		observability.SetPaused(desired, false)

		// Record successful asset application
		if p.eventRecorder != nil && renderCtx.HCO != nil {
			p.eventRecorder.AssetApplied(renderCtx.HCO, assetMeta.Name, desired.GetKind(), desired.GetNamespace(), desired.GetName())
		}
		// Also record drift correction since we just fixed it
		if liveExists && p.eventRecorder != nil && renderCtx.HCO != nil {
			p.eventRecorder.DriftCorrected(renderCtx.HCO, desired.GetKind(), desired.GetNamespace(), desired.GetName())
		}
	} else {
		// No drift detected or skipped - still compliant
		observability.SetCompliance(desired, 1)
	}

	return applied, nil
}

// ReconcileAssets reconciles multiple assets in order
func (p *Patcher) ReconcileAssets(ctx context.Context, assetMetas []assets.AssetMetadata, renderCtx *pkgcontext.RenderContext) (int, error) {
	// Opportunistically clean up stale throttle bucket entries (prevents memory leak)
	// This runs once per reconciliation loop to remove entries for deleted resources
	p.throttle.CleanupStale(throttling.DefaultTTL)

	appliedCount := 0
	var failedAssets []string
	var errors []error

	for i := range assetMetas {
		applied, err := p.ReconcileAsset(ctx, &assetMetas[i], renderCtx)
		if err != nil {
			// Collect error and failed asset name
			errors = append(errors, err)
			failedAssets = append(failedAssets, assetMetas[i].Name)

			// Continue with other assets even if one fails
			log.FromContext(ctx).Error(err, "Failed to reconcile asset, continuing with others",
				"asset", assetMetas[i].Name,
				"failedSoFar", len(failedAssets),
			)
			continue
		}

		if applied {
			appliedCount++
		}
	}

	// Return aggregated error if any assets failed
	// This ensures reconciliation fails and retries, but only after attempting all assets
	if len(errors) > 0 {
		// Build detailed error message with all failures
		var errMsgs []string
		for i, err := range errors {
			errMsgs = append(errMsgs, fmt.Sprintf("[%s: %v]", failedAssets[i], err))
		}

		return appliedCount, fmt.Errorf("failed to reconcile %d/%d assets: %s",
			len(failedAssets),
			len(assetMetas),
			strings.Join(errMsgs, "; "),
		)
	}

	return appliedCount, nil
}

// setPauseAnnotation sets the reconcile-paused annotation on a live object
// This annotation signals that reconciliation should stop due to an edit war
func (p *Patcher) setPauseAnnotation(ctx context.Context, obj *unstructured.Unstructured) error {
	// Get fresh copy to avoid conflicts
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(obj.GroupVersionKind())
	objKey := client.ObjectKey{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}

	if err := p.client.Get(ctx, objKey, fresh); err != nil {
		return fmt.Errorf("failed to get fresh copy for pause annotation: %w", err)
	}

	// Add pause annotation
	annotations := fresh.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[overrides.AnnotationReconcilePaused] = "true"
	fresh.SetAnnotations(annotations)

	// Update the object
	if err := p.client.Update(ctx, fresh); err != nil {
		return fmt.Errorf("failed to set pause annotation: %w", err)
	}

	return nil
}

// isNamespaceNotFound reports whether err is a 404 caused by a missing namespace.
// When the API server rejects an apply because the target namespace doesn't exist it
// returns a NotFound error whose message contains `namespaces "<name>" not found`,
// which is distinguishable from a resource-level 404.
func isNamespaceNotFound(err error) bool {
	return errors.IsNotFound(err) && strings.Contains(err.Error(), "namespaces \"")
}

// countJSONPatchOperations counts the number of operations in a JSON patch string
// Returns the count or 0 if parsing fails
func countJSONPatchOperations(patchStr string) int {
	var patch []map[string]any
	if err := json.Unmarshal([]byte(patchStr), &patch); err != nil {
		return 0
	}
	return len(patch)
}
