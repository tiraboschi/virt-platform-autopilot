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

package assets

import (
	"testing"
)

func TestDeriveFeatureOptInIncludesUserFacingConditionTypes(t *testing.T) {
	resolved := []*AssetMetadata{
		{
			Name: "pci-passthrough",
			Conditions: []AssetCondition{
				{Type: ConditionTypeAnnotation, Key: "platform.kubevirt.io/openshift", Value: "true"},
				{Type: ConditionTypeHardwareDetection, Detector: "pciDevicesPresent"},
			},
		},
		{
			Name: "ksm-zero-only-master",
			Conditions: []AssetCondition{
				{Type: ConditionTypeAnnotation, Key: "platform.kubevirt.io/enable-ksm-zero-only", Value: "true"},
				{Type: ConditionTypeHCOFieldUnconfigured, Path: "spec.virtualization.ksmConfiguration"},
				{Type: ConditionTypeTopology, Field: "hasSchedulableMasters"},
			},
		},
	}

	got := deriveFeatureOptIn(resolved)
	if got == nil {
		t.Fatal("deriveFeatureOptIn() = nil, want conditions")
	}

	want := "platform.kubevirt.io/openshift=true, platform.kubevirt.io/enable-ksm-zero-only=true, hcoUnconfigured:spec.virtualization.ksmConfiguration"
	if *got != want {
		t.Errorf("deriveFeatureOptIn() = %q, want %q", *got, want)
	}
}

func TestDeriveFeatureOptInSkipsInternalConditionTypes(t *testing.T) {
	resolved := []*AssetMetadata{
		{
			Name: "psi-enable-master",
			Conditions: []AssetCondition{
				{Type: ConditionTypeTopology, Field: "hasSchedulableMasters"},
			},
		},
		{
			Name: "kubevirt-metrics-exporter",
			Conditions: []AssetCondition{
				{Type: ConditionTypeImage, Key: "kubevirt-metrics-exporter"},
			},
		},
	}

	if got := deriveFeatureOptIn(resolved); got != nil {
		t.Errorf("deriveFeatureOptIn() = %q, want nil", *got)
	}
}

func TestSortFeatureStatuses(t *testing.T) {
	statuses := []FeatureStatus{
		{Name: "Zebra", Maturity: "DP"},
		{Name: "Beta", Maturity: "GA"},
		{Name: "Alpha", Maturity: "GA"},
		{Name: "Charlie", Maturity: "TP"},
	}

	sortFeatureStatuses(statuses)

	want := []string{"Alpha", "Beta", "Charlie", "Zebra"}
	for i, name := range want {
		if statuses[i].Name != name {
			t.Fatalf("statuses[%d].Name = %q, want %q", i, statuses[i].Name, name)
		}
	}
}

func TestDeriveFeatureStatusesIncludesRecommended(t *testing.T) {
	catalog := &AssetCatalog{
		Assets: []AssetMetadata{
			{Name: "kubevirt-metrics-exporter", Install: InstallModeOptIn},
		},
		Features: []FeatureMetadata{
			{
				Name:        "KubeVirt Metrics Exporter",
				Description: "Per-node metrics exporter",
				Groups:      []string{},
				Assets:      []string{"kubevirt-metrics-exporter"},
				Recommended: []string{"Cluster Observability Operator"},
			},
		},
	}

	statuses, err := deriveFeatureStatuses(catalog)
	if err != nil {
		t.Fatalf("deriveFeatureStatuses() error = %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("deriveFeatureStatuses() returned %d statuses, want 1", len(statuses))
	}
	if len(statuses[0].Recommended) != 1 || statuses[0].Recommended[0] != "Cluster Observability Operator" {
		t.Fatalf("status.Recommended = %v, want [Cluster Observability Operator]", statuses[0].Recommended)
	}
}

func TestDeriveFeatureStatusesExposesFeatureAndAssetIdentifiers(t *testing.T) {
	catalog := &AssetCatalog{
		Assets: []AssetMetadata{
			{Name: "first-asset", Install: InstallModeOptIn},
			{Name: "second-asset", Install: InstallModeOptIn},
		},
		Features: []FeatureMetadata{{
			Name:   "Example Feature",
			Assets: []string{"second-asset", "first-asset"},
		}},
	}

	statuses, err := deriveFeatureStatuses(catalog)
	if err != nil {
		t.Fatalf("deriveFeatureStatuses() error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].ID != "example-feature" {
		t.Fatalf("unexpected feature identifier: %#v", statuses)
	}
	if got, want := statuses[0].Assets, []string{"first-asset", "second-asset"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("feature asset identifiers = %v, want %v", got, want)
	}
}

func TestRegistryFeatureCatalog(t *testing.T) {
	loader := NewLoader()
	registry, err := NewRegistry(loader)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	catalog, err := registry.FeatureCatalog()
	if err != nil {
		t.Fatalf("FeatureCatalog() error = %v", err)
	}

	if catalog.Framework.Maturity == "" {
		t.Fatal("framework maturity is empty")
	}
	if len(catalog.Features) == 0 {
		t.Fatal("expected non-empty feature list")
	}

	for i := 1; i < len(catalog.Features); i++ {
		prev := catalog.Features[i-1]
		curr := catalog.Features[i]
		prevRank := maturityRank(prev.Maturity)
		currRank := maturityRank(curr.Maturity)
		if prevRank > currRank {
			t.Fatalf("features not sorted by maturity: %q before %q", prev.Name, curr.Name)
		}
		if prevRank == currRank && prev.Name > curr.Name {
			t.Fatalf("features not sorted by name within maturity: %q before %q", prev.Name, curr.Name)
		}
	}
}
