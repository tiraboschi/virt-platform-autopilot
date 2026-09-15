/*
Copyright 2026 The Virt Platform Autopilot Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pkgcontext "github.com/kubevirt/virt-platform-autopilot/pkg/context"
	"github.com/kubevirt/virt-platform-autopilot/pkg/observability"
)

const (
	// MachineConfigCoalescingBypassAnnotation opts one managed MachineConfig out
	// of staging. It is deliberately read from (and preserved on) the live MC.
	MachineConfigCoalescingBypassAnnotation = "platform.kubevirt.io/bypass-mcp-rollout-coalescing"
	machineConfigStagingConfigMap           = "virt-platform-autopilot-mc-staging"
)

var machineConfigPoolGVK = schema.GroupVersionKind{Group: "machineconfiguration.openshift.io", Version: "v1", Kind: "MachineConfigPool"}

type machineConfigStage struct {
	StagedAt      time.Time `json:"stagedAt"`
	DesiredHash   string    `json:"desiredHash"`
	MatchingPools []string  `json:"matchingPools"`
}

// coalesceMachineConfigUpdate returns true when an existing MC update may be
// applied. Creation is never staged. The first matching MCP with
// Updating=True wins; this intentionally avoids waiting forever for every MCP
// which can select the same MachineConfig.
func (p *Patcher) coalesceMachineConfigUpdate(ctx context.Context, desired, live *unstructured.Unstructured, renderCtx *pkgcontext.RenderContext) (bool, error) {
	if desired.GetKind() != "MachineConfig" || desired.GroupVersionKind().Group != machineConfigPoolGVK.Group || live == nil {
		return true, nil
	}
	namespace := renderCtx.HCO.GetNamespace()
	if namespace == "" {
		return true, nil
	}
	if live.GetAnnotations()[MachineConfigCoalescingBypassAnnotation] == "true" {
		p.copyMachineConfigBypassAnnotation(desired, live)
		return true, p.clearMachineConfigStage(ctx, namespace, desired.GetName())
	}

	p.copyMachineConfigBypassAnnotation(desired, live)
	pools, err := p.matchingMachineConfigPools(ctx, desired)
	if err != nil {
		// The MCO is optional outside OpenShift. Absence must retain normal MC reconciliation.
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return true, nil
		}
		return false, err
	}
	if len(pools) == 0 {
		return true, p.clearMachineConfigStage(ctx, namespace, desired.GetName())
	}
	for _, pool := range pools {
		if machineConfigPoolUpdating(pool) {
			return true, p.clearMachineConfigStage(ctx, namespace, desired.GetName())
		}
	}

	hash, err := machineConfigDesiredHash(desired)
	if err != nil {
		return false, err
	}
	names := make([]string, 0, len(pools))
	for _, pool := range pools {
		names = append(names, pool.GetName())
	}
	sort.Strings(names)
	stage, err := p.getMachineConfigStage(ctx, namespace, desired.GetName())
	if err != nil {
		return false, err
	}
	if stage == nil || stage.DesiredHash != hash {
		stage = &machineConfigStage{StagedAt: time.Now().UTC(), DesiredHash: hash, MatchingPools: names}
		if err := p.putMachineConfigStage(ctx, namespace, desired.GetName(), stage, renderCtx.HCO); err != nil {
			return false, err
		}
	} else if !sameStrings(stage.MatchingPools, names) {
		stage.MatchingPools = names
		if err := p.putMachineConfigStage(ctx, namespace, desired.GetName(), stage, renderCtx.HCO); err != nil {
			return false, err
		}
	}
	observability.ClearMachineConfigUpdateStaged(desired.GetName())
	observability.SetMachineConfigUpdateStaged(desired.GetName(), names, stage.StagedAt)
	return false, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (p *Patcher) copyMachineConfigBypassAnnotation(desired, live *unstructured.Unstructured) {
	if value, ok := live.GetAnnotations()[MachineConfigCoalescingBypassAnnotation]; ok {
		annotations := desired.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[MachineConfigCoalescingBypassAnnotation] = value
		desired.SetAnnotations(annotations)
	}
}

func (p *Patcher) matchingMachineConfigPools(ctx context.Context, desired *unstructured.Unstructured) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(machineConfigPoolGVK.GroupVersion().WithKind("MachineConfigPoolList"))
	if err := p.applier.ListDirect(ctx, list); err != nil {
		return nil, err
	}
	matched := []unstructured.Unstructured{}
	for _, pool := range list.Items {
		raw, found, err := unstructured.NestedMap(pool.Object, "spec", "machineConfigSelector")
		if err != nil || !found {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: stringMap(raw, "matchLabels"), MatchExpressions: labelExpressions(raw)})
		if err != nil {
			return nil, fmt.Errorf("invalid selector on MCP %s: %w", pool.GetName(), err)
		}
		if selector.Matches(labels.Set(desired.GetLabels())) {
			matched = append(matched, pool)
		}
	}
	return matched, nil
}

func stringMap(raw map[string]interface{}, key string) map[string]string {
	value, _, _ := unstructured.NestedStringMap(raw, key)
	return value
}
func labelExpressions(raw map[string]interface{}) []metav1.LabelSelectorRequirement {
	items, _, _ := unstructured.NestedSlice(raw, "matchExpressions")
	result := make([]metav1.LabelSelectorRequirement, 0, len(items))
	for _, item := range items {
		if b, err := json.Marshal(item); err == nil {
			var r metav1.LabelSelectorRequirement
			if json.Unmarshal(b, &r) == nil {
				result = append(result, r)
			}
		}
	}
	return result
}
func machineConfigPoolUpdating(pool unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(pool.Object, "status", "conditions")
	for _, item := range conditions {
		if c, ok := item.(map[string]interface{}); ok && c["type"] == "Updating" && c["status"] == "True" {
			return true
		}
	}
	return false
}
func machineConfigDesiredHash(obj *unstructured.Unstructured) (string, error) {
	copy := obj.DeepCopy()
	copy.SetResourceVersion("")
	copy.SetManagedFields(nil)
	copy.SetUID("")
	copy.SetGeneration(0)
	copy.SetCreationTimestamp(metav1.Time{})
	b, err := json.Marshal(copy.Object)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
func (p *Patcher) stagingConfigMap(ctx context.Context, namespace string) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	// The first lookup must bypass the managed-by filtered cache: the ConfigMap
	// may have been created before its informer observes its label.
	err := p.applier.GetDirectObject(ctx, client.ObjectKey{Namespace: namespace, Name: machineConfigStagingConfigMap}, cm)
	if apierrors.IsNotFound(err) {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: machineConfigStagingConfigMap, Labels: map[string]string{ManagedByLabel: ManagedByValue}}, Data: map[string]string{}}, nil
	}
	return cm, err
}
func (p *Patcher) getMachineConfigStage(ctx context.Context, namespace, name string) (*machineConfigStage, error) {
	cm, err := p.stagingConfigMap(ctx, namespace)
	if err != nil {
		return nil, err
	}
	raw := cm.Data[name]
	if raw == "" {
		return nil, nil
	}
	var stage machineConfigStage
	if err := json.Unmarshal([]byte(raw), &stage); err != nil {
		return nil, fmt.Errorf("invalid staged MachineConfig state for %s: %w", name, err)
	}
	return &stage, nil
}
func (p *Patcher) putMachineConfigStage(ctx context.Context, namespace, name string, stage *machineConfigStage, owner *unstructured.Unstructured) error {
	cm, err := p.stagingConfigMap(ctx, namespace)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(stage)
	if err != nil {
		return err
	}
	cm.Data[name] = string(raw)
	// The ConfigMap is operator state, not user configuration. Give it the HCO
	// lifecycle where the HCO has a usable identity. The guard keeps fake-client
	// unit tests and early object-adoption paths free of invalid owner refs.
	if owner != nil && owner.GetUID() != "" && owner.GetAPIVersion() != "" && owner.GetKind() != "" && len(cm.OwnerReferences) == 0 {
		cm.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, owner.GroupVersionKind())}
	}
	if cm.ResourceVersion == "" {
		return p.client.Create(ctx, cm)
	}
	return p.client.Update(ctx, cm)
}
func (p *Patcher) clearMachineConfigStage(ctx context.Context, namespace, name string) error {
	cm, err := p.stagingConfigMap(ctx, namespace)
	if err != nil {
		return err
	}
	if cm.ResourceVersion == "" || cm.Data[name] == "" {
		observability.ClearMachineConfigUpdateStaged(name)
		return nil
	}
	delete(cm.Data, name)
	observability.ClearMachineConfigUpdateStaged(name)
	return p.client.Update(ctx, cm)
}
