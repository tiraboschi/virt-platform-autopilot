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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pkgassets "github.com/kubevirt/virt-platform-autopilot/pkg/assets"
	pkgcontext "github.com/kubevirt/virt-platform-autopilot/pkg/context"
	"github.com/kubevirt/virt-platform-autopilot/pkg/observability"
	"github.com/kubevirt/virt-platform-autopilot/pkg/throttling"
	"github.com/kubevirt/virt-platform-autopilot/pkg/util"
)

// failingDriftChecker always returns an error simulating a broken webhook or TLS failure.
type failingDriftChecker struct{ err error }

func (f *failingDriftChecker) DetectDrift(_ context.Context, _, _ *unstructured.Unstructured) (bool, error) {
	return false, f.err
}

// alwaysDriftChecker reports drift on every call so the token-consumption step is always reached.
type alwaysDriftChecker struct{}

func (a *alwaysDriftChecker) DetectDrift(_ context.Context, _, _ *unstructured.Unstructured) (bool, error) {
	return true, nil
}

// countingRecorder implements events.EventRecorder and counts calls by reason.
type countingRecorder struct {
	counts map[string]int
}

func (r *countingRecorder) Eventf(_ runtime.Object, _ runtime.Object, _, reason, _, _ string, _ ...any) {
	r.counts[reason]++
}

// TestDriftDetectionFailureSetsComplianceToZero verifies that when the SSA dry-run used
// for drift detection fails persistently (e.g. webhook down), compliance_status is set to 0
// so that VirtPlatformAutopilotSyncFailed can fire. Before the fix, the error path returned without
// updating the metric, leaving it stuck at 1 (synced) indefinitely.
func TestDriftDetectionFailureSetsComplianceToZero(t *testing.T) {
	observability.ComplianceStatus.Reset()

	loader := pkgassets.NewLoader()
	renderer := NewRenderer(loader)

	assetMeta := &pkgassets.AssetMetadata{
		Name:           "psi-enable",
		Path:           "active/machine-config/04-psi-enable.yaml.tpl",
		Component:      "MachineConfig",
		TemplateParams: map[string]string{"role": "worker"},
	}

	hco := pkgcontext.NewMockHCO("kubevirt-hyperconverged", "kubevirt-hyperconverged")
	renderCtx := pkgcontext.NewRenderContext(hco)

	// Render once to get the exact object shape the patcher will use.
	desired, err := renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil {
		t.Fatalf("failed to render asset: %v", err)
	}

	// Create the live object pre-existing in the cluster so liveExists=true and
	// the drift-detection branch is reached.
	live := desired.DeepCopy()
	fakeClient := fake.NewClientBuilder().WithObjects(live).Build()

	p := &Patcher{
		renderer:          renderer,
		applier:           NewApplier(fakeClient, nil),
		driftDetector:     &failingDriftChecker{err: fmt.Errorf("webhook TLS failure")},
		throttle:          throttling.NewTokenBucket(),
		thrashingDetector: throttling.NewThrashingDetector(),
		client:            fakeClient,
	}

	_, reconcileErr := p.ReconcileAsset(context.Background(), assetMeta, renderCtx)
	if reconcileErr == nil {
		t.Fatal("expected ReconcileAsset to return an error when drift detection fails")
	}
	if !strings.Contains(reconcileErr.Error(), "drift detection failed") {
		t.Errorf("unexpected error message: %v", reconcileErr)
	}

	// compliance_status must be 0: the operator cannot guarantee compliance while
	// drift detection is broken and must signal that to the monitoring stack.
	gauge := observability.ComplianceStatus.WithLabelValues(
		desired.GetKind(),
		desired.GetName(),
		desired.GetNamespace(),
	)
	if val := testutil.ToFloat64(gauge); val != 0 {
		t.Errorf("compliance_status = %v, want 0 after drift detection failure", val)
	}
}

// capturingDriftChecker records the desired object seen by DetectDrift.
type capturingDriftChecker struct {
	desired *unstructured.Unstructured
}

func (c *capturingDriftChecker) DetectDrift(_ context.Context, desired, _ *unstructured.Unstructured) (bool, error) {
	c.desired = desired.DeepCopy()
	return false, nil
}

// TestReconcileAssetPreservesMachineConfigBypassBeforeDrift ensures the live
// admin coalescing bypass is copied onto desired before SSA drift detection.
// Without that ordering, every reconcile reports drift on desired:null vs
// live:"true" and re-applies the MachineConfig.
func TestReconcileAssetPreservesMachineConfigBypassBeforeDrift(t *testing.T) {
	loader := pkgassets.NewLoader()
	renderer := NewRenderer(loader)

	assetMeta := &pkgassets.AssetMetadata{
		Name:           "psi-enable",
		Path:           "active/machine-config/04-psi-enable.yaml.tpl",
		Component:      "MachineConfig",
		TemplateParams: map[string]string{"role": "worker"},
	}

	hco := pkgcontext.NewMockHCO("kubevirt-hyperconverged", "kubevirt-hyperconverged")
	renderCtx := pkgcontext.NewRenderContext(hco)

	desired, err := renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil {
		t.Fatalf("failed to render asset: %v", err)
	}
	if _, ok := desired.GetAnnotations()[MachineConfigCoalescingBypassAnnotation]; ok {
		t.Fatal("rendered MachineConfig unexpectedly carries bypass annotation")
	}

	live := desired.DeepCopy()
	live.SetAnnotations(map[string]string{MachineConfigCoalescingBypassAnnotation: "true"})
	fakeClient := fake.NewClientBuilder().WithObjects(live).Build()
	capturing := &capturingDriftChecker{}

	p := &Patcher{
		renderer:          renderer,
		applier:           NewApplier(fakeClient, nil),
		driftDetector:     capturing,
		throttle:          throttling.NewTokenBucket(),
		thrashingDetector: throttling.NewThrashingDetector(),
		client:            fakeClient,
	}

	applied, reconcileErr := p.ReconcileAsset(context.Background(), assetMeta, renderCtx)
	if reconcileErr != nil {
		t.Fatalf("ReconcileAsset: %v", reconcileErr)
	}
	if applied {
		t.Fatal("expected no apply when only the live bypass annotation differs")
	}
	if capturing.desired == nil {
		t.Fatal("DetectDrift was not called")
	}
	if got := capturing.desired.GetAnnotations()[MachineConfigCoalescingBypassAnnotation]; got != "true" {
		t.Fatalf("desired bypass at drift detection = %q, want true", got)
	}
}

func TestIsNamespaceNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "namespace 404 is recognized",
			err:  k8serrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "openshift-kube-descheduler-operator"),
			want: true,
		},
		{
			name: "resource-level 404 is not a namespace 404",
			err:  k8serrors.NewNotFound(schema.GroupResource{Group: "operator.openshift.io", Resource: "kubedeschedulers"}, "cluster"),
			want: false,
		},
		{
			name: "configmap 404 is not a namespace 404",
			err:  k8serrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "my-config"),
			want: false,
		},
		{
			name: "conflict error is not namespace 404",
			err:  k8serrors.NewConflict(schema.GroupResource{Resource: "kubedeschedulers"}, "cluster", fmt.Errorf("conflict")),
			want: false,
		},
		{
			name: "plain error with namespace-like message is not a namespace 404",
			err:  fmt.Errorf(`namespaces "foo" not found`),
			want: false, // IsNotFound returns false for plain errors
		},
		{
			name: "forbidden is not namespace 404",
			err:  k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "foo", fmt.Errorf("forbidden")),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNamespaceNotFound(tt.err); got != tt.want {
				t.Errorf("isNamespaceNotFound() = %v, want %v (err: %v)", got, tt.want, tt.err)
			}
		})
	}
}

// TestNamespaceNotFoundDoesNotConsumeTokens verifies that when the target namespace
// does not exist the patcher performs a cost-free soft skip: no rate-limit token is
// consumed and no error is returned. The API server returns only resource-not-found
// (never namespace-not-found) for GET operations, so the namespace must be checked
// explicitly before the token-consumption step (Pre-Step 6).
func TestNamespaceNotFoundDoesNotConsumeTokens(t *testing.T) {
	observability.ComplianceStatus.Reset()

	loader := pkgassets.NewLoader()
	renderer := NewRenderer(loader)

	// metrics-service is a namespaced asset (namespace comes from HCO).
	assetMeta := &pkgassets.AssetMetadata{
		Name:      "metrics-service",
		Path:      "active/observability/metrics-service.yaml.tpl",
		Component: "Service",
	}

	hco := pkgcontext.NewMockHCO("kubevirt-hyperconverged", "kubevirt-hyperconverged")
	renderCtx := pkgcontext.NewRenderContext(hco)

	// Render once to confirm the asset is namespaced — the guard under test is only
	// reachable for namespaced resources.
	desired, err := renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil {
		t.Fatalf("failed to render asset: %v", err)
	}
	if desired.GetNamespace() == "" {
		t.Fatal("asset must be namespaced for this test to exercise the namespace guard")
	}

	// Empty fake cluster — the namespace does not exist.
	fakeClient := fake.NewClientBuilder().Build()

	// Tiny token bucket: capacity 3, 1-minute window.
	// If any token is consumed, calls 4–10 below will be throttled.
	smallBucket := throttling.NewTokenBucketWithSettings(3, time.Minute)

	p := &Patcher{
		renderer:          renderer,
		applier:           NewApplier(fakeClient, nil),
		driftDetector:     NewDriftDetector(fakeClient),
		throttle:          smallBucket,
		thrashingDetector: throttling.NewThrashingDetector(),
		client:            fakeClient,
	}

	// 10 calls far exceeds the bucket capacity of 3.
	// Pre-Step 6 must short-circuit every call before any token is touched.
	for i := 0; i < 10; i++ {
		applied, err := p.ReconcileAsset(context.Background(), assetMeta, renderCtx)
		if err != nil {
			t.Fatalf("call %d: got error %v; namespace-not-found must be a free soft skip, not an error", i+1, err)
		}
		if applied {
			t.Fatalf("call %d: got applied=true; want false (namespace is missing)", i+1)
		}
	}
}

// TestThrashingEventEmittedOnlyOnce verifies that the ThrashingDetected event fires exactly
// once per edit-war episode, no matter how many reconciliation cycles execute while
// shouldPause is true. Before the fix, the event was emitted outside the ShouldEmitMetric
// gate and fired on every throttled call after the threshold was crossed.
func TestThrashingEventEmittedOnlyOnce(t *testing.T) {
	loader := pkgassets.NewLoader()
	renderer := NewRenderer(loader)

	// psi-enable is cluster-scoped: Pre-Step 6 (namespace guard) is skipped,
	// so every call reaches the anti-thrashing gate.
	assetMeta := &pkgassets.AssetMetadata{
		Name:           "psi-enable",
		Path:           "active/machine-config/04-psi-enable.yaml.tpl",
		Component:      "MachineConfig",
		TemplateParams: map[string]string{"role": "worker"},
	}

	hco := pkgcontext.NewMockHCO("kubevirt-hyperconverged", "kubevirt-hyperconverged")
	renderCtx := pkgcontext.NewRenderContext(hco)

	desired, err := renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil {
		t.Fatalf("failed to render asset: %v", err)
	}

	live := desired.DeepCopy()
	fakeClient := fake.NewClientBuilder().WithObjects(live).Build()

	rec := &countingRecorder{counts: make(map[string]int)}

	// Token bucket of capacity 1, long window so no refill during the test.
	// Call 1 consumes the only token; calls 2+ are all throttled.
	// ThrashingThreshold consecutive throttles trigger shouldPause=true.
	// The event must fire on exactly that one call and never again.
	p := &Patcher{
		renderer:          renderer,
		applier:           NewApplier(fakeClient, nil),
		driftDetector:     &alwaysDriftChecker{},
		throttle:          throttling.NewTokenBucketWithSettings(1, time.Hour),
		thrashingDetector: throttling.NewThrashingDetector(),
		client:            fakeClient,
	}
	p.SetEventRecorder(util.NewEventRecorder(rec))

	totalCalls := throttling.ThrashingThreshold + 5
	for i := 0; i < totalCalls; i++ {
		//nolint:errcheck // only the event count matters here
		p.ReconcileAsset(context.Background(), assetMeta, renderCtx)
	}

	if got := rec.counts[util.EventReasonThrashingDetected]; got != 1 {
		t.Errorf("ThrashingDetected event count = %d after %d calls, want 1 (must fire only once per episode)",
			got, totalCalls)
	}
}

// TestThrashingStateResetOnPauseAnnotationRemoval reproduces the race where a user removes
// the reconcile-paused annotation before the token bucket has refilled (< 6 s after pause).
// Without the fix the in-memory consecutiveThrottles stays >= ThrashingThreshold, so the very
// next throttled reconciliation immediately re-pauses the resource.
// With the fix both the thrashing state and the token bucket are reset as soon as the pause
// annotation is absent, so the next reconciliation succeeds cleanly.
func TestThrashingStateResetOnPauseAnnotationRemoval(t *testing.T) {
	loader := pkgassets.NewLoader()
	renderer := NewRenderer(loader)

	assetMeta := &pkgassets.AssetMetadata{
		Name:           "psi-enable",
		Path:           "active/machine-config/04-psi-enable.yaml.tpl",
		Component:      "MachineConfig",
		TemplateParams: map[string]string{"role": "worker"},
	}

	hco := pkgcontext.NewMockHCO("kubevirt-hyperconverged", "kubevirt-hyperconverged")
	renderCtx := pkgcontext.NewRenderContext(hco)

	desired, err := renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil {
		t.Fatalf("failed to render asset: %v", err)
	}

	// Bucket: capacity 1, very long window so no natural refill during the test.
	// Call 1 → token consumed; calls 2+ → throttled.
	// After ThrashingThreshold throttles the operator sets the pause annotation.
	smallBucket := throttling.NewTokenBucketWithSettings(1, time.Hour)
	td := throttling.NewThrashingDetector()

	live := desired.DeepCopy()
	fakeClient := fake.NewClientBuilder().WithObjects(live).Build()

	p := &Patcher{
		renderer:          renderer,
		applier:           NewApplier(fakeClient, nil),
		driftDetector:     &alwaysDriftChecker{},
		throttle:          smallBucket,
		thrashingDetector: td,
		client:            fakeClient,
	}

	// Drive the patcher until it sets the pause annotation.
	// Call 1 consumes the token; calls 2, 3, 4 are throttled and hit the threshold.
	for i := 0; i < throttling.ThrashingThreshold+1; i++ {
		//nolint:errcheck // we only care about side-effects here
		p.ReconcileAsset(context.Background(), assetMeta, renderCtx)
	}

	// Verify the pause annotation was set on the live object.
	paused := &unstructured.Unstructured{}
	paused.SetGroupVersionKind(live.GroupVersionKind())
	if err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: live.GetNamespace(),
		Name:      live.GetName(),
	}, paused); err != nil {
		t.Fatalf("failed to fetch live object: %v", err)
	}
	annotations := paused.GetAnnotations()
	if annotations == nil || annotations["platform.kubevirt.io/reconcile-paused"] != "true" {
		t.Fatal("expected pause annotation to be set after edit-war threshold; was not set")
	}

	// Simulate user removing the pause annotation (bucket still empty — the race window).
	delete(annotations, "platform.kubevirt.io/reconcile-paused")
	paused.SetAnnotations(annotations)
	if err := fakeClient.Update(context.Background(), paused); err != nil {
		t.Fatalf("failed to remove pause annotation: %v", err)
	}

	// One more reconciliation after unpause.
	// Before the fix: consecutiveThrottles was still >= threshold → immediate re-pause.
	// After the fix:  both thrashing state and token bucket are reset → reconciliation succeeds.
	_, reconcileErr := p.ReconcileAsset(context.Background(), assetMeta, renderCtx)

	if reconcileErr != nil {
		t.Fatalf("expected successful reconciliation after unpause, got: %v", reconcileErr)
	}

	afterRemoval := &unstructured.Unstructured{}
	afterRemoval.SetGroupVersionKind(live.GroupVersionKind())
	if err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: live.GetNamespace(),
		Name:      live.GetName(),
	}, afterRemoval); err != nil {
		t.Fatalf("failed to fetch live object after annotation removal: %v", err)
	}
	if ann := afterRemoval.GetAnnotations(); ann != nil && ann["platform.kubevirt.io/reconcile-paused"] == "true" {
		t.Error("resource was immediately re-paused after user removed the annotation; thrashing state was not cleared")
	}
}

// TestInvalidIgnoreFieldsEmitsEvent verifies that when the ignore-fields annotation
// contains an invalid JSON pointer (missing leading /), the patcher emits an
// InvalidIgnoreFields event before returning the error.
func TestInvalidIgnoreFieldsEmitsEvent(t *testing.T) {
	loader := pkgassets.NewLoader()
	renderer := NewRenderer(loader)

	assetMeta := &pkgassets.AssetMetadata{
		Name:           "psi-enable",
		Path:           "active/machine-config/04-psi-enable.yaml.tpl",
		Component:      "MachineConfig",
		TemplateParams: map[string]string{"role": "worker"},
	}

	hco := pkgcontext.NewMockHCO("kubevirt-hyperconverged", "kubevirt-hyperconverged")
	renderCtx := pkgcontext.NewRenderContext(hco)

	desired, err := renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil {
		t.Fatalf("failed to render asset: %v", err)
	}

	live := desired.DeepCopy()
	annotations := live.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["platform.kubevirt.io/ignore-fields"] = "spec/no-leading-slash"
	live.SetAnnotations(annotations)

	fakeClient := fake.NewClientBuilder().WithObjects(live).Build()
	rec := &countingRecorder{counts: make(map[string]int)}

	p := &Patcher{
		renderer:          renderer,
		applier:           NewApplier(fakeClient, nil),
		driftDetector:     &alwaysDriftChecker{},
		throttle:          throttling.NewTokenBucket(),
		thrashingDetector: throttling.NewThrashingDetector(),
		client:            fakeClient,
	}
	p.SetEventRecorder(util.NewEventRecorder(rec))

	_, reconcileErr := p.ReconcileAsset(context.Background(), assetMeta, renderCtx)
	if reconcileErr == nil {
		t.Fatal("expected error from invalid ignore-fields pointer, got nil")
	}
	if !strings.Contains(reconcileErr.Error(), "must start with /") {
		t.Errorf("unexpected error: %v", reconcileErr)
	}
	if got := rec.counts[util.EventReasonInvalidIgnoreFields]; got != 1 {
		t.Errorf("InvalidIgnoreFields event count = %d, want 1", got)
	}
}

// TestCleanupExcludedAsset verifies that per-asset metric series are deleted when an
// asset is excluded from the active set (allowlist narrowed, CRD removed, etc.).
// Without the fix the compliance_status series lingers at its last value, misleading
// dashboards and alerts.
func TestCleanupExcludedAsset(t *testing.T) {
	observability.ComplianceStatus.Reset()
	observability.PausedResources.Reset()
	observability.CustomizationInfo.Reset()

	loader := pkgassets.NewLoader()
	renderer := NewRenderer(loader)

	assetMeta := &pkgassets.AssetMetadata{
		Name:           "psi-enable",
		Path:           "active/machine-config/04-psi-enable.yaml.tpl",
		Component:      "MachineConfig",
		TemplateParams: map[string]string{"role": "worker"},
	}

	hco := pkgcontext.NewMockHCO("kubevirt-hyperconverged", "kubevirt-hyperconverged")
	renderCtx := pkgcontext.NewRenderContext(hco)

	desired, err := renderer.RenderAsset(assetMeta, renderCtx)
	if err != nil {
		t.Fatalf("failed to render asset: %v", err)
	}

	live := desired.DeepCopy()
	fakeClient := fake.NewClientBuilder().WithObjects(live).Build()

	p := &Patcher{
		renderer:          renderer,
		applier:           NewApplier(fakeClient, nil),
		driftDetector:     &alwaysDriftChecker{},
		throttle:          throttling.NewTokenBucket(),
		thrashingDetector: throttling.NewThrashingDetector(),
		client:            fakeClient,
	}

	// First reconciliation: asset is active — compliance_status is set.
	_, _ = p.ReconcileAsset(context.Background(), assetMeta, renderCtx)

	if testutil.CollectAndCount(observability.ComplianceStatus) == 0 {
		t.Fatal("expected compliance_status series to exist after first reconciliation")
	}

	// Asset is now excluded (e.g. removed from the allowlist).
	p.CleanupExcludedAsset(assetMeta, renderCtx, "conditionNotMet")

	if count := testutil.CollectAndCount(observability.ComplianceStatus); count != 0 {
		t.Errorf("compliance_status series count = %d after CleanupExcludedAsset, want 0", count)
	}
	if count := testutil.CollectAndCount(observability.PausedResources); count != 0 {
		t.Errorf("paused_resources series count = %d after CleanupExcludedAsset, want 0", count)
	}
}

func TestCountJSONPatchOperations(t *testing.T) {
	tests := []struct {
		name     string
		patchStr string
		want     int
	}{
		{
			name:     "single operation",
			patchStr: `[{"op": "add", "path": "/spec/replicas", "value": 3}]`,
			want:     1,
		},
		{
			name: "multiple operations",
			patchStr: `[
				{"op": "add", "path": "/spec/replicas", "value": 3},
				{"op": "replace", "path": "/spec/image", "value": "nginx:latest"},
				{"op": "remove", "path": "/spec/nodeSelector"}
			]`,
			want: 3,
		},
		{
			name:     "empty patch array",
			patchStr: `[]`,
			want:     0,
		},
		{
			name:     "invalid JSON",
			patchStr: `invalid json`,
			want:     0,
		},
		{
			name:     "not an array",
			patchStr: `{"op": "add"}`,
			want:     0,
		},
		{
			name:     "empty string",
			patchStr: ``,
			want:     0,
		},
		{
			name: "complex patch with nested values",
			patchStr: `[
				{"op": "add", "path": "/spec/template/spec/containers/0", "value": {"name": "nginx", "image": "nginx:latest"}},
				{"op": "add", "path": "/metadata/labels/app", "value": "web"}
			]`,
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countJSONPatchOperations(tt.patchStr)
			if got != tt.want {
				t.Errorf("countJSONPatchOperations() = %v, want %v", got, tt.want)
			}
		})
	}
}
