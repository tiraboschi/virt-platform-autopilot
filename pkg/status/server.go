// Package status exposes the supported, read-only Autopilot status API.
package status

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubevirt/virt-platform-autopilot/pkg/assets"
	pkgcontext "github.com/kubevirt/virt-platform-autopilot/pkg/context"
)

// Path is the current beta Console status API. A future stable v1 will use a
// new path rather than changing this contract in place.
const Path = "/status/v1beta1"

type Asset struct {
	ID                 string     `json:"id,omitempty"`
	APIVersion         string     `json:"apiVersion"`
	Kind               string     `json:"kind"`
	Name               string     `json:"name"`
	Namespace          string     `json:"namespace"`
	State              string     `json:"state"`
	Reason             string     `json:"reason,omitempty"`
	LastTransitionTime time.Time  `json:"lastTransitionTime"`
	LastDriftTime      *time.Time `json:"lastDriftTime,omitempty"`
	DesiredURL         string     `json:"desiredURL,omitempty"`
}

type Response struct {
	APIVersion              string     `json:"apiVersion"`
	ObservedAt              *time.Time `json:"observedAt,omitempty"`
	Ready                   bool       `json:"ready"`
	LastSuccessfulReconcile *time.Time `json:"lastSuccessfulReconcile,omitempty"`
	LastFailedReconcile     *time.Time `json:"lastFailedReconcile,omitempty"`
	Stale                   bool       `json:"stale"`
	Features                []Feature  `json:"features"`
	Assets                  []Asset    `json:"assets"`
}
type Feature struct {
	assets.FeatureStatus
	Enabled      bool              `json:"enabled"`
	Availability string            `json:"availability"`
	Reason       string            `json:"reason,omitempty"`
	Activation   *ActivationPlan   `json:"activation,omitempty"`
	Evidence     []FeatureEvidence `json:"evidence"`
}

// FeatureEvidence lets clients explain an availability decision without
// reproducing the operator's feature-to-asset mapping.
type FeatureEvidence struct {
	AssetID string `json:"assetID"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
}
type ActivationPlan struct {
	Target    ResourceRef    `json:"target"`
	Method    string         `json:"method"`
	PatchType string         `json:"patchType"`
	Patch     map[string]any `json:"patch"`
}

// ResourceRef identifies the Kubernetes object on which Console performs an
// activation action. It is deliberately fully qualified so clients do not
// need to carry Autopilot's HCO topology as configuration.
type ResourceRef struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Resource  string `json:"resource"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type Store struct {
	mu                      sync.RWMutex
	assets                  map[string]Asset
	features                []Feature
	ready                   bool
	lastSuccessfulReconcile *time.Time
	lastFailedReconcile     *time.Time
	desired                 map[string]*unstructured.Unstructured
}

func (s *Store) SetFeatures(catalog *assets.FeatureCatalog, hco *unstructured.Unstructured, gates []string) {
	features := make([]Feature, 0, len(catalog.Features))
	for _, feature := range catalog.Features {
		enabled := feature.Install == "always"
		if feature.OptIn != nil {
			enabled = true
			for _, condition := range strings.Split(*feature.OptIn, ",") {
				condition = strings.TrimSpace(condition)
				if gate, ok := strings.CutPrefix(condition, "kubevirtFeatureGate:"); ok {
					if !contains(gates, gate) {
						enabled = false
						break
					}
					continue
				}
				if path, ok := strings.CutPrefix(condition, "hcoUnconfigured:"); ok {
					if hco == nil || fieldConfigured(hco.Object, strings.Split(strings.TrimPrefix(path, "/"), "/")) {
						enabled = false
						break
					}
					continue
				}
				parts := strings.SplitN(condition, "=", 2)
				if len(parts) != 2 || hco == nil || hco.GetAnnotations()[parts[0]] != parts[1] {
					enabled = false
					break
				}
			}
		}
		availability, reason := "unknown", "notAssessed"
		if !enabled {
			availability, reason = "disabled", "notConfigured"
		}
		features = append(features, Feature{FeatureStatus: feature, Enabled: enabled, Availability: availability, Reason: reason, Activation: activationPlan(feature, hco)})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.features = features
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func activationPlan(feature assets.FeatureStatus, hco *unstructured.Unstructured) *ActivationPlan {
	if feature.OptIn == nil || hco == nil {
		return nil
	}
	annotations := map[string]string{}
	for _, condition := range strings.Split(*feature.OptIn, ",") {
		parts := strings.SplitN(strings.TrimSpace(condition), "=", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[0], "platform.kubevirt.io/") {
			return nil
		}
		annotations[parts[0]] = parts[1]
	}
	return &ActivationPlan{
		Target: ResourceRef{
			Group: pkgcontext.HCOGroup, Version: pkgcontext.HCOVersion, Resource: "hyperconvergeds", Kind: pkgcontext.HCOKind,
			Namespace: hco.GetNamespace(), Name: hco.GetName(),
		},
		Method: "PATCH", PatchType: "application/merge-patch+json",
		Patch: map[string]any{"metadata": map[string]any{"annotations": annotations}},
	}
}
func fieldConfigured(value map[string]any, path []string) bool {
	var current any = value
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = object[key]
		if !ok {
			return false
		}
	}
	return current != nil
}
func (s *Store) Features() []Feature {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Feature(nil), s.features...)
}
func (s *Store) response() Response {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stale := s.lastFailedReconcile != nil && (s.lastSuccessfulReconcile == nil || s.lastFailedReconcile.After(*s.lastSuccessfulReconcile))
	return Response{APIVersion: "autopilot.kubevirt.io/status/v1beta1", ObservedAt: s.lastSuccessfulReconcile, Ready: s.ready, LastSuccessfulReconcile: s.lastSuccessfulReconcile, LastFailedReconcile: s.lastFailedReconcile, Stale: stale, Features: s.featuresWithAvailabilityLocked(), Assets: s.listLocked()}
}

func (s *Store) featuresWithAvailabilityLocked() []Feature {
	byID := make(map[string]Asset, len(s.assets))
	for _, asset := range s.assets {
		if asset.ID != "" {
			byID[asset.ID] = asset
		}
	}
	features := append([]Feature(nil), s.features...)
	for i := range features {
		feature := &features[i]
		if !feature.Enabled {
			feature.Availability, feature.Reason = "disabled", "notConfigured"
			continue
		}
		feature.Evidence = make([]FeatureEvidence, 0, len(feature.Assets))
		var synced, excluded, unmanaged, pending bool
		for _, id := range feature.Assets {
			asset, ok := byID[id]
			if !ok {
				feature.Evidence = append(feature.Evidence, FeatureEvidence{AssetID: id, State: "unobserved"})
				pending = true
				continue
			}
			feature.Evidence = append(feature.Evidence, FeatureEvidence{AssetID: id, State: asset.State, Reason: asset.Reason})
			switch asset.State {
			case "applyFailed", "drifted":
				feature.Availability, feature.Reason = "degraded", asset.State
				goto nextFeature
			case "staged":
				pending = true
			case "synced":
				synced = true
			case "excluded":
				excluded = true
			case "unmanaged":
				unmanaged = true
			}
		}
		switch {
		case pending:
			feature.Availability, feature.Reason = "pending", "awaitingReconciliation"
		case synced:
			feature.Availability, feature.Reason = "healthy", ""
		case unmanaged:
			feature.Availability, feature.Reason = "unmanaged", "userOptOut"
		case excluded:
			feature.Availability, feature.Reason = "blocked", "noApplicableAssets"
		default:
			feature.Availability, feature.Reason = "unknown", "notAssessed"
		}
	nextFeature:
	}
	return features
}
func (s *Store) MarkReconciled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.ready = true
	s.lastSuccessfulReconcile = &now
}
func (s *Store) MarkFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.lastFailedReconcile = &now
}

var DefaultStore = NewStore()

func NewStore() *Store {
	return &Store{assets: map[string]Asset{}, desired: map[string]*unstructured.Unstructured{}}
}
func (s *Store) SetDesired(asset string, object *unstructured.Unstructured) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desired[asset] = object.DeepCopy()
	for key, entry := range s.assets {
		if entry.Kind == object.GetKind() && entry.Name == object.GetName() && entry.Namespace == object.GetNamespace() {
			entry.ID, entry.DesiredURL = asset, Path+"/assets/"+asset
			s.assets[key] = entry
		}
	}
}
func (s *Store) Desired(asset string) (*unstructured.Unstructured, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	object, ok := s.desired[asset]
	return object.DeepCopy(), ok
}
func (s *Store) ClearDesired(asset string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.desired, asset)
	for key, entry := range s.assets {
		if entry.ID == asset {
			entry.DesiredURL = ""
			s.assets[key] = entry
		}
	}
}
func key(o *unstructured.Unstructured) string {
	return o.GetKind() + "/" + o.GetNamespace() + "/" + o.GetName()
}
func (s *Store) Observe(o *unstructured.Unstructured, state, reason string) {
	s.ObserveAsset("", o, state, reason)
}
func (s *Store) ObserveAsset(assetID string, o *unstructured.Unstructured, state, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	k := key(o)
	a := s.assets[k]
	a.APIVersion, a.Kind, a.Name, a.Namespace, a.State, a.Reason, a.LastTransitionTime = o.GetAPIVersion(), o.GetKind(), o.GetName(), o.GetNamespace(), state, reason, now
	if assetID != "" {
		a.ID = assetID
		if _, ok := s.desired[assetID]; ok {
			a.DesiredURL = Path + "/assets/" + assetID
		}
	}
	for asset, desired := range s.desired {
		if desired.GetKind() == a.Kind && desired.GetName() == a.Name && desired.GetNamespace() == a.Namespace {
			a.ID, a.DesiredURL = asset, Path+"/assets/"+asset
			break
		}
	}
	if state == "drifted" {
		a.LastDriftTime = &now
	}
	s.assets[k] = a
}
func (s *Store) List() []Asset {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocked()
}
func (s *Store) listLocked() []Asset {
	out := make([]Asset, 0, len(s.assets))
	for _, a := range s.assets {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

type Server struct {
	client    client.Client
	registry  *assets.Registry
	store     *Store
	authorize func(context.Context, string) bool
}

func NewServer(c client.Client, r *assets.Registry, s *Store) *Server {
	server := &Server{client: c, registry: r, store: s}
	server.authorize = server.authorized
	return server
}
func (s *Server) InstallHandlers(m *http.ServeMux) {
	m.HandleFunc(Path, s.handle)
	m.HandleFunc(Path+"/assets/", s.handleDesired)
}
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !s.authorize(r.Context(), r.Header.Get("Authorization")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	catalog, err := s.registry.FeatureCatalog()
	if err != nil {
		http.Error(w, "feature catalog unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if len(s.store.Features()) == 0 {
		s.store.SetFeatures(catalog, nil, nil)
	}
	_ = json.NewEncoder(w).Encode(s.store.response())
}
func (s *Server) handleDesired(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !s.authorize(r.Context(), r.Header.Get("Authorization")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	asset := strings.TrimPrefix(r.URL.Path, Path+"/assets/")
	if asset == "" || strings.Contains(asset, "/") {
		http.Error(w, "asset name required", http.StatusBadRequest)
		return
	}
	object, ok := s.store.Desired(asset)
	if !ok {
		http.Error(w, "desired configuration not available", http.StatusNotFound)
		return
	}
	if object.GetKind() == "Secret" {
		http.Error(w, "sensitive desired configuration is not exposed", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(object)
}
func (s *Server) authorized(ctx context.Context, header string) bool {
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" {
		return false
	}
	tr := &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token}}
	if err := s.client.Create(ctx, tr); err != nil || !tr.Status.Authenticated {
		return false
	}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: tr.Status.User.Username, Groups: tr.Status.User.Groups, NonResourceAttributes: &authorizationv1.NonResourceAttributes{Path: Path, Verb: "get"}}}
	extra := make(map[string]authorizationv1.ExtraValue, len(tr.Status.User.Extra))
	for key, value := range tr.Status.User.Extra {
		extra[key] = authorizationv1.ExtraValue(value)
	}
	sar.Spec.Extra = extra
	return s.client.Create(ctx, sar) == nil && sar.Status.Allowed
}
