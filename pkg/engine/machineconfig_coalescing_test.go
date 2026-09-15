package engine

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pkgcontext "github.com/kubevirt/virt-platform-autopilot/pkg/context"
)

func TestMachineConfigUpdateIsStagedUntilMatchingPoolUpdates(t *testing.T) {
	pool := testMCP("worker", "False")
	p := NewPatcher(fake.NewClientBuilder().WithRuntimeObjects(pool).Build(), nil, nil)
	desired, live := testMachineConfigs()
	apply, err := p.coalesceMachineConfigUpdate(context.Background(), desired, live, testRenderContext())
	if err != nil || apply {
		t.Fatalf("stable MCP: apply=%t err=%v, want false nil", apply, err)
	}
	stage, err := p.getMachineConfigStage(context.Background(), "openshift-cnv", desired.GetName())
	if err != nil || stage == nil {
		t.Fatalf("stage was not persisted: stage=%v err=%v", stage, err)
	}
	cm := &corev1.ConfigMap{}
	if err := p.client.Get(context.Background(), client.ObjectKey{Namespace: "openshift-cnv", Name: machineConfigStagingConfigMap}, cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "test-hco" {
		t.Fatalf("staging ConfigMap owner references = %#v, want HCO owner", cm.OwnerReferences)
	}

	pool = testMCP("worker", "True")
	current := testMCP("worker", "False")
	if err := p.client.Get(context.Background(), client.ObjectKey{Name: "worker"}, current); err != nil {
		t.Fatal(err)
	}
	current.Object["status"] = pool.Object["status"]
	if err := p.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	apply, err = p.coalesceMachineConfigUpdate(context.Background(), desired, live, testRenderContext())
	if err != nil || !apply {
		t.Fatalf("updating MCP: apply=%t err=%v, want true nil", apply, err)
	}
	stage, err = p.getMachineConfigStage(context.Background(), "openshift-cnv", desired.GetName())
	if err != nil || stage != nil {
		t.Fatalf("stage was not cleared: stage=%v err=%v", stage, err)
	}
}

func TestMachineConfigBypassIsPreservedAndImmediate(t *testing.T) {
	p := NewPatcher(fake.NewClientBuilder().Build(), nil, nil)
	desired, live := testMachineConfigs()
	live.SetAnnotations(map[string]string{MachineConfigCoalescingBypassAnnotation: "true"})
	apply, err := p.coalesceMachineConfigUpdate(context.Background(), desired, live, testRenderContext())
	if err != nil || !apply {
		t.Fatalf("bypass: apply=%t err=%v, want true nil", apply, err)
	}
	if desired.GetAnnotations()[MachineConfigCoalescingBypassAnnotation] != "true" {
		t.Fatal("bypass annotation was not preserved")
	}
}

func TestMachineConfigCreationIsNotStaged(t *testing.T) {
	p := NewPatcher(fake.NewClientBuilder().Build(), nil, nil)
	desired, _ := testMachineConfigs()
	apply, err := p.coalesceMachineConfigUpdate(context.Background(), desired, nil, testRenderContext())
	if err != nil || !apply {
		t.Fatalf("creation: apply=%t err=%v, want true nil", apply, err)
	}
}

func testMachineConfigs() (*unstructured.Unstructured, *unstructured.Unstructured) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "machineconfiguration.openshift.io/v1", "kind": "MachineConfig", "metadata": map[string]interface{}{"name": "99-test", "labels": map[string]interface{}{"machineconfiguration.openshift.io/role": "worker"}}, "spec": map[string]interface{}{"config": map[string]interface{}{"ignition": map[string]interface{}{"version": "3.5.0"}}}}}
	return obj.DeepCopy(), obj.DeepCopy()
}
func testMCP(name, updating string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "machineconfiguration.openshift.io/v1", "kind": "MachineConfigPool", "metadata": map[string]interface{}{"name": name}, "spec": map[string]interface{}{"machineConfigSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"machineconfiguration.openshift.io/role": "worker"}}}, "status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Updating", "status": updating}}}}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "machineconfiguration.openshift.io", Version: "v1", Kind: "MachineConfigPool"})
	return obj
}
func testRenderContext() *pkgcontext.RenderContext {
	hco := &unstructured.Unstructured{}
	hco.SetAPIVersion("hco.kubevirt.io/v1beta1")
	hco.SetKind("HyperConverged")
	hco.SetUID("test-hco")
	hco.SetNamespace("openshift-cnv")
	return &pkgcontext.RenderContext{HCO: hco}
}
