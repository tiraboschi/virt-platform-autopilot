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

// Package rbac provides shared RBAC rule generation logic for virt-platform-autopilot.
// It derives ClusterRole rules by scanning asset files (via fs.FS) to discover the
// Kubernetes GVKs the operator manages, then combining them with static infrastructure rules.
//
// Both the rbac-gen tool (reads from the real filesystem) and the csv-generator
// (reads from the embedded asset FS) use this package to ensure generated RBAC is
// always consistent with the actual set of managed resources.
package rbac

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Resource represents a Kubernetes resource GVK discovered from an asset file.
type Resource struct {
	APIVersion  string
	Kind        string
	Name        string
	NeedsDelete bool // true if found in tombstones (requires delete verb)
}

// Rule represents a single ClusterRole policy rule.
type Rule struct {
	APIGroups     []string
	Resources     []string
	ResourceNames []string
	Verbs         []string
}

// sensitiveKinds are resource types that require resourceNames scoping to
// prevent privilege escalation via the operator's service account.
var sensitiveKinds = map[string]bool{
	"SecurityContextConstraints": true,
	"ClusterRole":                true,
	"ClusterRoleBinding":         true,
}

// StaticRules returns the fixed infrastructure RBAC rules that every release of
// virt-platform-autopilot requires, regardless of which asset templates are active.
// The order is stable; the comment formatter in cmd/rbac-gen writes each rule by index.
func StaticRules() []Rule {
	return []Rule{
		// Rule 0: Nodes (for hardware detection)
		{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// Rule 1: Events (for observability - legacy core/v1 API)
		{
			APIGroups: []string{""},
			Resources: []string{"events"},
			Verbs:     []string{"create", "patch"},
		},
		// Rule 2: Events (for observability - modern events.k8s.io/v1 API)
		{
			APIGroups: []string{"events.k8s.io"},
			Resources: []string{"events"},
			Verbs:     []string{"create", "patch"},
		},
		// Rule 3: Leader Election
		{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			Verbs:     []string{"create", "delete", "get", "list", "patch", "update", "watch"},
		},
		// Rule 4: CRD Discovery (for soft dependency detection and template introspection)
		{
			APIGroups: []string{"apiextensions.k8s.io"},
			Resources: []string{"customresourcedefinitions"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// Rule 5: OpenShift config CRs (for cluster topology detection: HCP, compact,
		// cloud provider via Infrastructure; schedulable masters via Scheduler; and
		// the cluster TLS security profile for the metrics endpoint via APIServer).
		// All are singletons (name="cluster") and are non-sensitive read-only.
		// Gracefully absent on non-OpenShift clusters — the operator handles NotFound.
		{
			APIGroups: []string{"config.openshift.io"},
			Resources: []string{"apiservers", "infrastructures", "schedulers"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// Rule 6: Metrics client CA read (the extension-apiserver-authentication
		// ConfigMap in kube-system holds client-ca-file, the trust anchor for
		// verifying metrics scraper client certificates). get is name-scoped to that
		// single ConfigMap; it is the authoritative read used by seedTLSState and the
		// MetricsTLSReconciler.
		{
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			ResourceNames: []string{"extension-apiserver-authentication"},
			Verbs:         []string{"get"},
		},
		// Rule 7: Metrics client CA watch. The MetricsTLSReconciler informer LISTs
		// then WATCHes the ConfigMap to refresh the trust pool on CA rotation.
		// RBAC cannot restrict list/watch to a resource name, so these are granted
		// at cluster scope (the informer itself is narrowed to the one ConfigMap via
		// a field selector). list is required because informers snapshot via LIST
		// before watching (only skippable with the WatchListClient streaming feature,
		// which is not guaranteed on every cluster). create is also included here
		// because Kubernetes cannot restrict create by resourceName; the staging
		// ConfigMap's get/patch/update permissions remain name-scoped below.
		{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"create", "list", "watch"},
		},
		// Rule 8: Namespaces (for pre-apply guard: verify the target namespace exists before
		// consuming a rate-limit token; avoids spurious throttling when an operator component
		// is not yet installed and its namespace is absent).
		{
			APIGroups: []string{""},
			Resources: []string{"namespaces"},
			Verbs:     []string{"get"},
		},
		// Rule 9: CDI StorageProfiles (for RWX StorageClass auto-detection in templates)
		{
			APIGroups: []string{"cdi.kubevirt.io"},
			Resources: []string{"storageprofiles"},
			Verbs:     []string{"list"},
		},
		// Rule 10: KubeVirt (for the kubevirt-feature-gate condition type)
		{
			APIGroups: []string{"kubevirt.io"},
			Resources: []string{"kubevirts"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// Rule 11: MachineConfigPools drive staged MachineConfig update release.
		{
			APIGroups: []string{"machineconfiguration.openshift.io"},
			Resources: []string{"machineconfigpools"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// Rule 12: all non-create staging ConfigMap access is name-scoped.
		{
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			ResourceNames: []string{"virt-platform-autopilot-mc-staging"},
			Verbs:         []string{"get", "patch", "update"},
		},
	}
}

// policyRule mirrors the Kubernetes PolicyRule structure for YAML unmarshalling.
type policyRule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	ResourceNames []string `yaml:"resourceNames,omitempty"`
	Verbs         []string `yaml:"verbs"`
}

// TransitiveRules scans ClusterRole and Role objects in the active/ subtree of fsys
// and returns the union of their policy rules. This satisfies Kubernetes privilege
// escalation prevention: the operator must hold every permission it grants.
//
// Rules are merged per API group (resources and verbs are unioned within each group)
// and returned in deterministic order.
func TransitiveRules(fsys fs.FS) ([]Rule, error) {
	var collected []policyRule
	err := fs.WalkDir(fsys, "active", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yaml.tpl") {
			return nil
		}
		content, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", path, err)
		}
		if strings.HasSuffix(path, ".tpl") {
			content = preprocessTemplate(content)
		}
		collectRoleRules(content, &collected)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to scan active directory for roles: %w", err)
	}
	return mergeTransitiveRules(collected), nil
}

// collectRoleRules appends policy rules from any ClusterRole or Role documents
// found in the given YAML content (supports multi-doc files).
func collectRoleRules(content []byte, rules *[]policyRule) {
	for _, docStr := range strings.Split(string(content), "\n---\n") {
		docStr = strings.TrimSpace(docStr)
		if docStr == "" {
			continue
		}
		var doc struct {
			Kind  string       `yaml:"kind"`
			Rules []policyRule `yaml:"rules"`
		}
		if err := yaml.Unmarshal([]byte(docStr), &doc); err != nil {
			continue
		}
		if doc.Kind != "ClusterRole" && doc.Kind != "Role" {
			continue
		}
		*rules = append(*rules, doc.Rules...)
	}
}

// mergeTransitiveRules groups policy rules by (API group, resourceNames), unions
// resources and verbs within each group, and returns a deterministically sorted
// slice of Rules. Rules with different resourceNames are kept separate.
func mergeTransitiveRules(collected []policyRule) []Rule {
	type mergeKey struct {
		apiGroup      string
		resourceNames string // sorted, comma-joined
	}
	type groupInfo struct {
		resourceNames []string
		resources     map[string]bool
		verbs         map[string]bool
	}
	groups := make(map[mergeKey]*groupInfo)

	for _, rule := range collected {
		sortedNames := make([]string, len(rule.ResourceNames))
		copy(sortedNames, rule.ResourceNames)
		sort.Strings(sortedNames)
		namesKey := strings.Join(sortedNames, ",")

		for _, apiGroup := range rule.APIGroups {
			key := mergeKey{apiGroup: apiGroup, resourceNames: namesKey}
			if groups[key] == nil {
				groups[key] = &groupInfo{
					resourceNames: sortedNames,
					resources:     make(map[string]bool),
					verbs:         make(map[string]bool),
				}
			}
			for _, r := range rule.Resources {
				groups[key].resources[r] = true
			}
			for _, v := range rule.Verbs {
				groups[key].verbs[v] = true
			}
		}
	}

	keys := make([]mergeKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].apiGroup != keys[j].apiGroup {
			return keys[i].apiGroup < keys[j].apiGroup
		}
		return keys[i].resourceNames < keys[j].resourceNames
	})

	var result []Rule
	for _, key := range keys {
		info := groups[key]

		resources := make([]string, 0, len(info.resources))
		for r := range info.resources {
			resources = append(resources, r)
		}
		sort.Strings(resources)

		verbs := make([]string, 0, len(info.verbs))
		for v := range info.verbs {
			verbs = append(verbs, v)
		}
		sort.Strings(verbs)

		rule := Rule{
			APIGroups: []string{key.apiGroup},
			Resources: resources,
			Verbs:     verbs,
		}
		if len(info.resourceNames) > 0 && info.resourceNames[0] != "" {
			rule.ResourceNames = info.resourceNames
		}
		result = append(result, rule)
	}
	return result
}

// DynamicRules returns only the rules derived from managed resource GVKs in the assets.
func DynamicRules(fsys fs.FS) ([]Rule, error) {
	resources, err := extractResources(fsys)
	if err != nil {
		return nil, err
	}
	return generateDynamicRules(resources), nil
}

// AllRules returns the complete set of RBAC rules for the given asset FS:
// static infrastructure rules, transitive rules from managed ClusterRole/Role assets,
// and dynamic rules derived from managed resource GVKs.
//
// fsys must be rooted at the assets directory: it should contain active/ and tombstones/
// as immediate subdirectories. Both os.DirFS("assets") and the embedded assets.EmbeddedFS
// satisfy this contract.
func AllRules(fsys fs.FS) ([]Rule, error) {
	transitive, err := TransitiveRules(fsys)
	if err != nil {
		return nil, err
	}
	dynamic, err := DynamicRules(fsys)
	if err != nil {
		return nil, err
	}
	rules := append(StaticRules(), transitive...)
	return append(rules, dynamic...), nil
}

// parseGVK extracts the API group, version, and plural resource name from an apiVersion
// string and a Kind string.
func parseGVK(apiVersion, kind string) (group, version, resource string) {
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 1 {
		// Core API group (e.g. "v1")
		return "", parts[0], pluralize(kind)
	}
	// Named API group (e.g. "hco.kubevirt.io/v1beta1")
	return parts[0], parts[1], pluralize(kind)
}

// pluralize converts a Kind to its lowercase plural resource name.
func pluralize(kind string) string {
	kind = strings.ToLower(kind)
	switch kind {
	case "nodehealthcheck":
		return "nodehealthchecks"
	case "machineconfig":
		return "machineconfigs"
	case "kubedescheduler":
		return "kubedeschedulers"
	case "securitycontextconstraints":
		return "securitycontextconstraints"
	default:
		if strings.HasSuffix(kind, "s") || strings.HasSuffix(kind, "x") || strings.HasSuffix(kind, "ch") {
			return kind + "es"
		}
		return kind + "s"
	}
}

// preprocessTemplate strips Go template directives and replaces template expressions
// with dummy values so the resulting bytes can be parsed as valid YAML.
func preprocessTemplate(content []byte) []byte {
	// Strip multi-line template comments ({{- /* ... */ -}}) that span multiple lines
	commentRe := regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	content = commentRe.ReplaceAll(content, nil)

	// Remove lines that are purely template control-flow directives (e.g. {{- if ... }})
	lines := strings.Split(string(content), "\n")
	var filtered []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") {
			continue
		}
		filtered = append(filtered, line)
	}
	content = []byte(strings.Join(filtered, "\n"))

	// Replace backtick raw strings (Prometheus template variables embedded in strings)
	backtickRe := regexp.MustCompile("\\{\\{`[^`]*`\\}\\}")
	content = backtickRe.ReplaceAll(content, []byte(`dummy-value`))

	// Replace remaining template expressions with a dummy YAML-safe value
	exprRe := regexp.MustCompile(`\{\{[^}]+\}\}`)
	return exprRe.ReplaceAll(content, []byte(`"dummy-value"`))
}

// parseAssetDoc extracts apiVersion, kind, and metadata.name from a single YAML
// document. Returns ok=false if the document is unparseable or irrelevant.
func parseAssetDoc(docStr string) (apiVersion, kind, name string, ok bool) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(docStr), &doc); err != nil {
		return "", "", "", false
	}

	apiVersion, ok1 := doc["apiVersion"].(string)
	kind, ok2 := doc["kind"].(string)
	if !ok1 || !ok2 || apiVersion == "" || kind == "" {
		return "", "", "", false
	}
	if kind == "List" || kind == "CustomResourceDefinition" {
		return "", "", "", false
	}

	if meta, mok := doc["metadata"].(map[string]any); mok {
		name, _ = meta["name"].(string)
	}
	if name == "dummy-value" {
		name = ""
	}
	return apiVersion, kind, name, true
}

// deduplicationKey returns the map key used to deduplicate resources.
// Sensitive kinds include the resource name so each named instance is tracked.
func deduplicationKey(apiVersion, kind, name string) string {
	if sensitiveKinds[kind] && name != "" {
		return apiVersion + "/" + kind + "/" + name
	}
	return apiVersion + "/" + kind
}

// upgradeNeedsDelete marks an already-seen resource as requiring delete permissions.
func upgradeNeedsDelete(resources *[]Resource, apiVersion, kind, name string) {
	matchByName := sensitiveKinds[kind] && name != ""
	for i := range *resources {
		r := &(*resources)[i]
		if r.APIVersion != apiVersion || r.Kind != kind {
			continue
		}
		if matchByName && r.Name != name {
			continue
		}
		r.NeedsDelete = true
		return
	}
}

// processAssetFile extracts Kubernetes GVKs from YAML content (supports multi-doc files).
// For sensitive kinds (SCCs, ClusterRoles, ClusterRoleBindings), each distinct named
// resource gets its own entry so that dynamic rules can be scoped with resourceNames.
func processAssetFile(content []byte, seen map[string]bool, resources *[]Resource, needsDelete bool) {
	for _, docStr := range strings.Split(string(content), "\n---\n") {
		docStr = strings.TrimSpace(docStr)
		if docStr == "" {
			continue
		}

		apiVersion, kind, name, ok := parseAssetDoc(docStr)
		if !ok {
			continue
		}

		key := deduplicationKey(apiVersion, kind, name)
		if !seen[key] {
			seen[key] = true
			*resources = append(*resources, Resource{
				APIVersion:  apiVersion,
				Kind:        kind,
				Name:        name,
				NeedsDelete: needsDelete,
			})
		} else if needsDelete {
			upgradeNeedsDelete(resources, apiVersion, kind, name)
		}
	}
}

// scanDirectory walks dir inside fsys and calls processAssetFile for every .yaml / .yaml.tpl.
func scanDirectory(fsys fs.FS, dir string, seen map[string]bool, resources *[]Resource, needsDelete bool) error {
	return fs.WalkDir(fsys, dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yaml.tpl") {
			return nil
		}

		content, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", path, err)
		}

		if strings.HasSuffix(path, ".tpl") {
			content = preprocessTemplate(content)
		}

		processAssetFile(content, seen, resources, needsDelete)
		return nil
	})
}

// extractResources scans active/ and tombstones/ inside fsys and returns all discovered GVKs.
func extractResources(fsys fs.FS) ([]Resource, error) {
	var resources []Resource
	seen := make(map[string]bool)

	if err := scanDirectory(fsys, "active", seen, &resources, false); err != nil {
		return nil, fmt.Errorf("failed to scan active directory: %w", err)
	}

	if err := scanDirectory(fsys, "tombstones", seen, &resources, true); err != nil {
		// Tolerate a missing tombstones directory (it may be empty or absent)
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("failed to scan tombstones directory: %w", err)
		}
	}

	return resources, nil
}

type normalGroupInfo struct {
	resources   map[string]bool
	needsDelete bool
}

type sensitiveResourceInfo struct {
	names       map[string]bool
	needsDelete bool
}

// managementVerbs returns the standard verb set, optionally including delete.
func managementVerbs(needsDelete bool) []string {
	verbs := []string{"create", "get", "list", "patch", "update", "watch"}
	if needsDelete {
		verbs = append(verbs, "delete")
		sort.Strings(verbs)
	}
	return verbs
}

// scopedSensitiveRules generates rules for a single sensitive resource type
// within an API group. When resource names are known, it produces an unscoped
// create rule plus a scoped management rule; otherwise a single unscoped rule.
func scopedSensitiveRules(group, resource string, info *sensitiveResourceInfo) []Rule {
	names := sortedKeys(info.names)
	if len(names) == 0 {
		return []Rule{{
			APIGroups: []string{group},
			Resources: []string{resource},
			Verbs:     managementVerbs(info.needsDelete),
		}}
	}

	// list and watch are collection-level verbs: Kubernetes requires a
	// metadata.name field selector when they are scoped to resourceNames,
	// but informer-based watches never set one, so these must be unscoped.
	scopedVerbs := []string{"get", "patch", "update"}
	if info.needsDelete {
		scopedVerbs = append(scopedVerbs, "delete")
		sort.Strings(scopedVerbs)
	}
	return []Rule{
		{
			APIGroups: []string{group},
			Resources: []string{resource},
			Verbs:     []string{"create", "list", "watch"},
		},
		{
			APIGroups:     []string{group},
			Resources:     []string{resource},
			ResourceNames: names,
			Verbs:         scopedVerbs,
		},
	}
}

// generateDynamicRules groups discovered resources by API group and produces
// deterministically ordered RBAC rules. Sensitive resource types (SCCs, RBAC)
// are scoped with resourceNames to prevent privilege escalation.
func generateDynamicRules(resources []Resource) []Rule {
	normalGrouped := make(map[string]*normalGroupInfo)
	sensitiveGrouped := make(map[string]map[string]*sensitiveResourceInfo)

	for _, res := range resources {
		group, _, resource := parseGVK(res.APIVersion, res.Kind)
		if sensitiveKinds[res.Kind] {
			if sensitiveGrouped[group] == nil {
				sensitiveGrouped[group] = make(map[string]*sensitiveResourceInfo)
			}
			if sensitiveGrouped[group][resource] == nil {
				sensitiveGrouped[group][resource] = &sensitiveResourceInfo{names: make(map[string]bool)}
			}
			if res.Name != "" {
				sensitiveGrouped[group][resource].names[res.Name] = true
			}
			if res.NeedsDelete {
				sensitiveGrouped[group][resource].needsDelete = true
			}
		} else {
			if normalGrouped[group] == nil {
				normalGrouped[group] = &normalGroupInfo{resources: make(map[string]bool)}
			}
			normalGrouped[group].resources[resource] = true
			if res.NeedsDelete {
				normalGrouped[group].needsDelete = true
			}
		}
	}

	allGroups := make(map[string]bool)
	for g := range normalGrouped {
		allGroups[g] = true
	}
	for g := range sensitiveGrouped {
		allGroups[g] = true
	}
	sortedGroups := sortedKeys(allGroups)

	var rules []Rule
	for _, group := range sortedGroups {
		if info, ok := normalGrouped[group]; ok {
			rules = append(rules, Rule{
				APIGroups: []string{group},
				Resources: sortedKeys(info.resources),
				Verbs:     managementVerbs(info.needsDelete),
			})
		}

		if resourceMap, ok := sensitiveGrouped[group]; ok {
			for _, resource := range sortedKeys(mapKeys(resourceMap)) {
				rules = append(rules, scopedSensitiveRules(group, resource, resourceMap[resource])...)
			}
		}
	}

	return rules
}

func mapKeys[V any](m map[string]V) map[string]bool {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
