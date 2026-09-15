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

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// Expected resource name created by operator asset
	driftMcName = "90-worker-swap-online"

	// Expected spec field value from the autopilot's asset
	driftExpectedIgnitionVersion = "3.5.0"

	// Managed-by label
	driftManagedByLabel   = "platform.kubevirt.io/managed-by"
	driftManagedByValue   = "virt-platform-autopilot"
	driftCoalescingBypass = "platform.kubevirt.io/bypass-mcp-rollout-coalescing"
)

var (
	driftMachineConfigGVK = schema.GroupVersionKind{
		Group:   "machineconfiguration.openshift.io",
		Version: "v1",
		Kind:    "MachineConfig",
	}
)

var _ = Describe("Drift Detection Tests", Ordered, func() {

	BeforeAll(func() {
		By("ensuring HCO instance exists")
		ensureHCOExists()
		patchAutopilotAndWait(autopilotEnabled)

		ensureCRDInstalled("machineconfigs.machineconfiguration.openshift.io")
		waitForOperatorHealthy()
	})

	It("should create the 90-worker-swap-online MachineConfig with managed-by label", func() {
		Eventually(func() error {
			_, err := getUnstructuredResource(driftMachineConfigGVK, driftMcName, "")
			return err
		}, timeout, interval).Should(Succeed(),
			"Operator should create the 90-worker-swap-online MachineConfig")

		mc, err := getUnstructuredResource(driftMachineConfigGVK, driftMcName, "")
		Expect(err).NotTo(HaveOccurred())
		// This test deliberately validates the documented escape hatch. On OCP a
		// stable MCP would otherwise correctly stage the update until its next
		// unrelated rollout, which is not suitable for a drift-correction test.
		annotations := mc.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[driftCoalescingBypass] = "true"
		mc.SetAnnotations(annotations)
		Expect(k8sClient.Update(ctx, mc)).To(Succeed())
		labels := mc.GetLabels()
		Expect(labels).To(HaveKeyWithValue(driftManagedByLabel, driftManagedByValue),
			"MachineConfig should have managed-by label")
	})

	It("should correct drift on MachineConfig spec and emit DriftCorrected event", func() {
		By("modifying ignition.version to simulate drift")
		mc, err := getUnstructuredResource(driftMachineConfigGVK, driftMcName, "")
		Expect(err).NotTo(HaveOccurred())

		// Modify spec.config.ignition.version
		Expect(setNestedField(mc, "2.0.0", "spec", "config", "ignition", "version")).To(Succeed())
		Expect(k8sClient.Update(ctx, mc)).To(Succeed())

		By("verifying operator corrects the drift back to expected version")
		Eventually(func() string {
			obj, err := getUnstructuredResource(driftMachineConfigGVK, driftMcName, "")
			if err != nil {
				return ""
			}
			val, _, _ := getNestedString(obj, "spec", "config", "ignition", "version")
			return val
		}, timeout, interval).Should(Equal(driftExpectedIgnitionVersion),
			"Operator should restore ignition.version to expected version")
		mc, err = getUnstructuredResource(driftMachineConfigGVK, driftMcName, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(mc.GetAnnotations()).To(HaveKeyWithValue(driftCoalescingBypass, "true"),
			"Operator should preserve the MachineConfig coalescing bypass annotation")

		By("checking for DriftCorrected event")
		Eventually(func() int {
			return len(findEvents(EventFilter{Reason: "DriftCorrected", Kind: "MachineConfig", Name: driftMcName}))
		}, timeout, interval).Should(BeNumerically(">=", 1),
			"At least one DriftCorrected event should exist for MachineConfig")
	})

	AfterAll(func() {
		waitForOperatorHealthy()
	})
})

// setNestedField sets a value in an unstructured object at the given field path.
func setNestedField(obj *unstructured.Unstructured, value any, fields ...string) error {
	return unstructured.SetNestedField(obj.Object, value, fields...)
}

// getNestedString reads a string value from an unstructured object at the given field path.
func getNestedString(obj *unstructured.Unstructured, fields ...string) (string, bool, error) {
	return unstructured.NestedString(obj.Object, fields...)
}
