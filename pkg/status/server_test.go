package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubevirt/virt-platform-autopilot/pkg/assets"
)

type authorizationClient struct {
	client.Client
	authenticated, allowed bool
	sawTokenReview, sawSAR bool
}

func (c *authorizationClient) Create(_ context.Context, object client.Object, _ ...client.CreateOption) error {
	switch review := object.(type) {
	case *authenticationv1.TokenReview:
		c.sawTokenReview = true
		review.Status.Authenticated = c.authenticated
		review.Status.User.Username = "alice"
	case *authorizationv1.SubjectAccessReview:
		c.sawSAR = true
		review.Status.Allowed = c.allowed
	}
	return nil
}

func TestStoreRetainsLastDriftAfterSync(t *testing.T) {
	store := NewStore()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetName("example")
	store.Observe(obj, "drifted", "detected")
	store.Observe(obj, "synced", "")
	assets := store.List()
	if len(assets) != 1 || assets[0].State != "synced" || assets[0].LastDriftTime == nil {
		t.Fatalf("unexpected status snapshot: %#v", assets)
	}
}

func TestStoreLinksObservedAssetToDesiredConfiguration(t *testing.T) {
	store := NewStore()
	obj := &unstructured.Unstructured{}
	obj.SetKind("ConfigMap")
	obj.SetNamespace("openshift-cnv")
	obj.SetName("example")
	store.SetDesired("example-asset", obj)
	store.Observe(obj, "synced", "")
	got := store.List()
	if len(got) != 1 || got[0].ID != "example-asset" || got[0].DesiredURL != Path+"/assets/example-asset" {
		t.Fatalf("observed asset was not linked to its desired configuration: %#v", got)
	}
}

func TestStoreClearsDesiredConfiguration(t *testing.T) {
	store := NewStore()
	obj := &unstructured.Unstructured{}
	obj.SetKind("ConfigMap")
	obj.SetName("example")
	store.SetDesired("example-asset", obj)
	store.ObserveAsset("example-asset", obj, "synced", "")
	store.ClearDesired("example-asset")
	if _, ok := store.Desired("example-asset"); ok || store.List()[0].DesiredURL != "" {
		t.Fatalf("expected desired configuration to be removed: %#v", store.List())
	}
}

func TestActivationPlanIsSelfDescribingMergePatch(t *testing.T) {
	optIn := "platform.kubevirt.io/enable-example=true"
	hco := &unstructured.Unstructured{}
	hco.SetNamespace("openshift-cnv")
	hco.SetName("kubevirt-hyperconverged")
	plan := activationPlan(assets.FeatureStatus{OptIn: &optIn}, hco)
	if plan == nil || plan.Target.Group != "hco.kubevirt.io" || plan.Target.Namespace != "openshift-cnv" || plan.Target.Name != "kubevirt-hyperconverged" || plan.Method != "PATCH" || plan.PatchType != "application/merge-patch+json" {
		t.Fatalf("unexpected activation plan: %#v", plan)
	}
	metadata := plan.Patch["metadata"].(map[string]any)
	annotations := metadata["annotations"].(map[string]string)
	if annotations["platform.kubevirt.io/enable-example"] != "true" {
		t.Fatalf("unexpected activation patch: %#v", plan.Patch)
	}
}

func newTestServer(t *testing.T, allowed bool) *Server {
	t.Helper()
	registry, err := assets.NewRegistry(assets.NewLoader())
	if err != nil {
		t.Fatalf("create asset registry: %v", err)
	}
	server := NewServer(nil, registry, NewStore())
	server.authorize = func(context.Context, string) bool { return allowed }
	return server
}

func TestStatusEndpointRejectsWrongMethod(t *testing.T) {
	server := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodPost, Path, nil)
	res := httptest.NewRecorder()
	server.handle(res, req)
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusMethodNotAllowed)
	}
}

func TestStatusEndpointRejectsUnauthorizedRequest(t *testing.T) {
	server := newTestServer(t, false)
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	req.Header.Set("Authorization", "Bearer denied")
	res := httptest.NewRecorder()
	server.handle(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusForbidden)
	}
}

func TestAuthorizationUsesTokenAndSubjectAccessReviews(t *testing.T) {
	registry, err := assets.NewRegistry(assets.NewLoader())
	if err != nil {
		t.Fatal(err)
	}
	reviews := &authorizationClient{authenticated: true, allowed: true}
	server := NewServer(reviews, registry, NewStore())
	if !server.authorized(context.Background(), "Bearer token") || !reviews.sawTokenReview || !reviews.sawSAR {
		t.Fatal("expected successful TokenReview followed by SubjectAccessReview")
	}
	if server.authorized(context.Background(), "token") {
		t.Fatal("expected malformed bearer header to be rejected")
	}
}

func TestStatusEndpointRequiresAuthentication(t *testing.T) {
	server := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	res := httptest.NewRecorder()
	server.handle(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
	}
}

func TestStatusEndpointReturnsFeatureAndAssetSnapshot(t *testing.T) {
	server := newTestServer(t, true)
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetName("example")
	obj.SetNamespace("openshift-cnv")
	server.store.Observe(obj, "synced", "")

	req := httptest.NewRequest(http.MethodGet, Path, nil)
	req.Header.Set("Authorization", "Bearer allowed")
	res := httptest.NewRecorder()
	server.handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var response Response
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.APIVersion != "autopilot.kubevirt.io/status/v1beta1" || len(response.Features) == 0 || len(response.Assets) != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	if response.Assets[0].APIVersion != "v1" || response.Assets[0].Namespace != "openshift-cnv" || response.Assets[0].State != "synced" {
		t.Fatalf("unexpected asset: %#v", response.Assets[0])
	}
}

func TestFeatureAvailabilityUsesAssetEvidence(t *testing.T) {
	store := NewStore()
	store.features = []Feature{
		{FeatureStatus: assets.FeatureStatus{Assets: []string{"healthy"}}, Enabled: true},
		{FeatureStatus: assets.FeatureStatus{Assets: []string{"excluded"}}, Enabled: true},
		{FeatureStatus: assets.FeatureStatus{Assets: []string{"staged"}}, Enabled: true},
		{FeatureStatus: assets.FeatureStatus{Assets: []string{"failed"}}, Enabled: true},
		{FeatureStatus: assets.FeatureStatus{Assets: []string{"unmanaged"}}, Enabled: true},
	}
	for id, state := range map[string]string{"healthy": "synced", "excluded": "excluded", "staged": "staged", "failed": "applyFailed", "unmanaged": "unmanaged"} {
		obj := &unstructured.Unstructured{}
		obj.SetKind("ConfigMap")
		obj.SetName(id)
		store.ObserveAsset(id, obj, state, "testReason")
	}
	got := store.response().Features
	want := []string{"healthy", "blocked", "pending", "degraded", "unmanaged"}
	for i, availability := range want {
		if got[i].Availability != availability || len(got[i].Evidence) != 1 || got[i].Evidence[0].AssetID == "" {
			t.Fatalf("feature %d = %#v, want availability %q with evidence", i, got[i], availability)
		}
	}
}

func TestDesiredEndpointReturnsCachedEffectiveObject(t *testing.T) {
	server := newTestServer(t, true)
	object := &unstructured.Unstructured{}
	object.SetAPIVersion("v1")
	object.SetKind("ConfigMap")
	object.SetName("example")
	server.store.SetDesired("example-asset", object)
	req := httptest.NewRequest(http.MethodGet, Path+"/assets/example-asset", nil)
	req.Header.Set("Authorization", "Bearer allowed")
	res := httptest.NewRecorder()
	server.handleDesired(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "ConfigMap") {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
}

func TestStoreReturnsAssetsInStableOrder(t *testing.T) {
	store := NewStore()
	for _, name := range []string{"z", "a"} {
		obj := &unstructured.Unstructured{}
		obj.SetKind("ConfigMap")
		obj.SetName(name)
		store.Observe(obj, "synced", "")
	}
	assets := store.List()
	if assets[0].Name != "a" || assets[1].Name != "z" {
		t.Fatalf("assets were not sorted: %#v", assets)
	}
}

func TestSnapshotMarksFailedReconciliationAsStale(t *testing.T) {
	store := NewStore()
	store.MarkReconciled()
	store.MarkFailed()
	response := store.response()
	if !response.Ready || !response.Stale || response.ObservedAt == nil || response.LastFailedReconcile == nil {
		t.Fatalf("unexpected stale snapshot: %#v", response)
	}
}
