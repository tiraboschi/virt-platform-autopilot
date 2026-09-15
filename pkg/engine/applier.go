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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// FieldManager is the field manager name used for SSA
	FieldManager = "virt-platform-autopilot"

	// ManagedByLabel is the label key used to mark managed objects
	ManagedByLabel = "platform.kubevirt.io/managed-by"

	// ManagedByValue is the label value for objects managed by this autopilot
	ManagedByValue = "virt-platform-autopilot"
)

// Applier handles Server-Side Apply operations
type Applier struct {
	client client.Client
	// apiReader provides direct API access bypassing cache for object adoption
	// If nil, GetDirect falls back to cached client (used in tests)
	apiReader client.Reader
}

// NewApplier creates a new SSA applier
// The apiReader enables object adoption by detecting unlabeled objects
// For tests with fake clients, pass nil for apiReader
func NewApplier(c client.Client, apiReader client.Reader) *Applier {
	return &Applier{
		client:    c,
		apiReader: apiReader,
	}
}

// Apply applies an object using Server-Side Apply
// Returns true if the object was created/updated, false if unchanged
// Automatically adds the managed-by label to track operator-managed objects
func (a *Applier) Apply(ctx context.Context, obj *unstructured.Unstructured, force bool) (bool, error) {
	logger := log.FromContext(ctx)

	if obj == nil {
		return false, fmt.Errorf("object is nil")
	}

	// Ensure object has required metadata
	if obj.GetName() == "" {
		return false, fmt.Errorf("object name is empty")
	}

	if obj.GetKind() == "" {
		return false, fmt.Errorf("object kind is empty")
	}

	// Create a copy to avoid modifying the input
	appliedObj := obj.DeepCopy()

	// Ensure managed-by label is present (GitOps best practice)
	ensureManagedByLabel(appliedObj)

	// Apply using SSA
	// Convert unstructured to ApplyConfiguration for the modern Apply() API
	applyOptions := []client.ApplyOption{
		client.ForceOwnership,
		client.FieldOwner(FieldManager),
	}

	logger.V(1).Info("Applying object",
		"kind", appliedObj.GetKind(),
		"namespace", appliedObj.GetNamespace(),
		"name", appliedObj.GetName(),
		"force", force,
	)

	// Use modern Apply() API with ApplyConfigurationFromUnstructured
	applyConfig := client.ApplyConfigurationFromUnstructured(appliedObj)
	err := a.client.Apply(ctx, applyConfig, applyOptions...)
	if err != nil {
		if errors.IsConflict(err) {
			return false, fmt.Errorf("field ownership conflict (another controller owns fields): %w", err)
		}
		return false, fmt.Errorf("failed to apply object: %w", err)
	}

	logger.V(1).Info("Successfully applied object",
		"kind", appliedObj.GetKind(),
		"namespace", appliedObj.GetNamespace(),
		"name", appliedObj.GetName(),
	)

	return true, nil
}

// Delete deletes an object if it exists
func (a *Applier) Delete(ctx context.Context, obj *unstructured.Unstructured) error {
	logger := log.FromContext(ctx)

	if obj == nil {
		return fmt.Errorf("object is nil")
	}

	logger.V(1).Info("Deleting object",
		"kind", obj.GetKind(),
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
	)

	err := a.client.Delete(ctx, obj)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil // Already deleted
		}
		return fmt.Errorf("failed to delete object: %w", err)
	}

	logger.V(1).Info("Successfully deleted object",
		"kind", obj.GetKind(),
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
	)

	return nil
}

// Get retrieves an object by key using the cached client
// Note: This may not find objects that lack the managed-by label if cache filtering is enabled
func (a *Applier) Get(ctx context.Context, key client.ObjectKey, obj *unstructured.Unstructured) error {
	return a.client.Get(ctx, key, obj)
}

// GetDirect retrieves an object directly from the API server, bypassing the cache
// This is used to detect objects that exist but lack the managed-by label (adoption scenario)
func (a *Applier) GetDirect(ctx context.Context, key client.ObjectKey, obj *unstructured.Unstructured) error {
	if a.apiReader != nil {
		// Use direct API reader to bypass cache
		return a.apiReader.Get(ctx, key, obj)
	}
	// Fallback to cached client if no API reader is configured
	// This happens in tests with fake clients
	return a.client.Get(ctx, key, obj)
}

// GetDirectObject is the typed counterpart of GetDirect.
func (a *Applier) GetDirectObject(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	if a.apiReader != nil {
		return a.apiReader.Get(ctx, key, obj)
	}
	return a.client.Get(ctx, key, obj)
}

// ListDirect lists objects directly from the API server, bypassing the cache.
// MCPs are intentionally not managed by this controller and are therefore not
// present in the managed-by filtered cache.
func (a *Applier) ListDirect(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if a.apiReader != nil {
		return a.apiReader.List(ctx, list, opts...)
	}
	return a.client.List(ctx, list, opts...)
}

// ensureManagedByLabel adds the managed-by label to an object
// This is a GitOps best practice and enables cache filtering
func ensureManagedByLabel(obj *unstructured.Unstructured) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[ManagedByLabel] = ManagedByValue
	obj.SetLabels(labels)
}

// HasManagedByLabel checks if an object has the managed-by label
func HasManagedByLabel(obj *unstructured.Unstructured) bool {
	labels := obj.GetLabels()
	if labels == nil {
		return false
	}
	return labels[ManagedByLabel] == ManagedByValue
}
