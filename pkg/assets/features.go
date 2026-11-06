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
	"fmt"
	"sort"
	"strings"
)

// FrameworkStatus describes the maturity and opt-in gate of the autopilot framework.
type FrameworkStatus struct {
	Maturity string  `json:"maturity"`
	OptIn    *string `json:"opt_in"`
}

// FeatureStatus is the derived user-facing status of a catalog feature.
type FeatureStatus struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Maturity    string   `json:"maturity"`
	Install     string   `json:"install"`
	OptIn       *string  `json:"opt_in"`
	Requires    []string `json:"requires,omitempty"`
	Recommended []string `json:"recommended,omitempty"`
	Assets      []string `json:"assets"`
}

// FeatureCatalog is the structured feature list exposed by generators and debug endpoints.
type FeatureCatalog struct {
	Framework FrameworkStatus `json:"framework"`
	Features  []FeatureStatus `json:"features"`
}

// FeatureCatalog returns the derived feature catalog from the registry metadata.
func (r *Registry) FeatureCatalog() (*FeatureCatalog, error) {
	return DeriveFeatureCatalog(r.catalog)
}

// ValidateFeatureCoverage ensures every asset is covered by a feature or excluded_assets.
func ValidateFeatureCoverage(catalog *AssetCatalog) error {
	covered := make(map[string]bool)

	for _, excl := range catalog.ExcludedAssets {
		covered[excl] = true
	}

	for _, feature := range catalog.Features {
		for _, name := range feature.Assets {
			covered[name] = true
		}
		for _, group := range feature.Groups {
			for _, a := range catalog.Assets {
				if a.Group == group {
					covered[a.Name] = true
				}
			}
		}
	}

	var uncovered []string
	for _, a := range catalog.Assets {
		if !covered[a.Name] {
			uncovered = append(uncovered, a.Name)
		}
	}

	if len(uncovered) > 0 {
		return fmt.Errorf("assets not covered by any feature or excluded_assets: %s\n"+
			"  Add them to a features entry or to excluded_assets in metadata.yaml",
			strings.Join(uncovered, ", "))
	}
	return nil
}

// DeriveFeatureCatalog builds the framework and feature status list from metadata.
func DeriveFeatureCatalog(catalog *AssetCatalog) (*FeatureCatalog, error) {
	statuses, err := deriveFeatureStatuses(catalog)
	if err != nil {
		return nil, err
	}
	return &FeatureCatalog{
		Framework: deriveFrameworkStatus(catalog),
		Features:  statuses,
	}, nil
}

func deriveFeatureStatuses(catalog *AssetCatalog) ([]FeatureStatus, error) {
	assetByName := make(map[string]*AssetMetadata, len(catalog.Assets))
	for i := range catalog.Assets {
		assetByName[catalog.Assets[i].Name] = &catalog.Assets[i]
	}

	var statuses []FeatureStatus
	for _, feature := range catalog.Features {
		resolved := resolveFeatureAssets(feature, catalog.Assets, assetByName)
		if len(resolved) == 0 {
			return nil, fmt.Errorf("feature %q resolves to zero assets", feature.Name)
		}

		assetNames := make([]string, 0, len(resolved))
		for _, asset := range resolved {
			assetNames = append(assetNames, asset.Name)
		}
		sort.Strings(assetNames)
		statuses = append(statuses, FeatureStatus{
			ID:          featureID(feature.Name),
			Name:        feature.Name,
			Description: feature.Description,
			Maturity:    deriveFeatureMaturity(feature, resolved),
			Install:     deriveFeatureInstall(resolved),
			OptIn:       deriveFeatureOptIn(resolved),
			Requires:    feature.Requires,
			Recommended: feature.Recommended,
			Assets:      assetNames,
		})
	}
	sortFeatureStatuses(statuses)
	return statuses, nil
}

func featureID(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func resolveFeatureAssets(feature FeatureMetadata, allAssets []AssetMetadata, byName map[string]*AssetMetadata) []*AssetMetadata {
	var resolved []*AssetMetadata
	seen := make(map[string]bool)

	for _, name := range feature.Assets {
		if a, ok := byName[name]; ok && !seen[name] {
			resolved = append(resolved, a)
			seen[name] = true
		}
	}

	for _, group := range feature.Groups {
		for i := range allAssets {
			if allAssets[i].Group == group && !seen[allAssets[i].Name] {
				resolved = append(resolved, &allAssets[i])
				seen[allAssets[i].Name] = true
			}
		}
	}

	return resolved
}

func deriveFeatureMaturity(feature FeatureMetadata, resolved []*AssetMetadata) string {
	if feature.Maturity != "" {
		return strings.ToUpper(feature.Maturity)
	}
	for _, a := range resolved {
		if a.Install == InstallModeOptIn {
			return "DP"
		}
	}
	return "GA"
}

func deriveFeatureInstall(resolved []*AssetMetadata) string {
	for _, a := range resolved {
		if a.Install == InstallModeOptIn {
			return "opt-in"
		}
	}
	return "always"
}

func deriveFeatureOptIn(resolved []*AssetMetadata) *string {
	var conditions []string
	seen := make(map[string]bool)

	for _, a := range resolved {
		for _, c := range a.Conditions {
			// Skip internal activation gates that are not user-facing opt-in requirements.
			switch c.Type {
			case ConditionTypeTopology,
				ConditionTypeHardwareDetection,
				ConditionTypeImage:
				continue
			}
			entry := FormatCondition(c)
			if entry != "" && !seen[entry] {
				conditions = append(conditions, entry)
				seen[entry] = true
			}
		}
	}

	if len(conditions) == 0 {
		return nil
	}
	result := strings.Join(conditions, ", ")
	return &result
}

func deriveFrameworkStatus(catalog *AssetCatalog) FrameworkStatus {
	maturity := "GA"
	if catalog.Framework.Maturity != "" {
		maturity = strings.ToUpper(catalog.Framework.Maturity)
	}
	var optIn *string
	if catalog.Framework.OptIn != "" {
		s := catalog.Framework.OptIn
		optIn = &s
	}
	return FrameworkStatus{Maturity: maturity, OptIn: optIn}
}

func maturityRank(maturity string) int {
	switch strings.ToUpper(maturity) {
	case "GA":
		return 0
	case "TP":
		return 1
	case "DP":
		return 2
	default:
		return 3
	}
}

func sortFeatureStatuses(statuses []FeatureStatus) {
	sort.Slice(statuses, func(i, j int) bool {
		rankI := maturityRank(statuses[i].Maturity)
		rankJ := maturityRank(statuses[j].Maturity)
		if rankI != rankJ {
			return rankI < rankJ
		}
		return statuses[i].Name < statuses[j].Name
	})
}
