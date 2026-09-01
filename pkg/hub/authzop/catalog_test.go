// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package authzop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Core catalog structural validation
// ---------------------------------------------------------------------------

// TestCatalogValidation validates every OperationSpec in the Catalog using the
// frozen CT1 deterministic validation. This is the proof that the catalog is
// structurally sound.
func TestCatalogValidation(t *testing.T) {
	if len(Catalog) == 0 {
		t.Fatal("catalog is empty — at least one operation must be cataloged")
	}
	if err := ValidateSpecs(Catalog); err != nil {
		t.Fatalf("catalog validation failed:\n%v", err)
	}
	t.Logf("catalog: %d operations validated", len(Catalog))
}

// TestCatalogNoDuplicateIDs ensures no two operations share the same ID.
func TestCatalogNoDuplicateIDs(t *testing.T) {
	seen := make(map[OperationID]int)
	for i, spec := range Catalog {
		if prev, ok := seen[spec.ID]; ok {
			t.Errorf("duplicate operation ID %q at index %d and %d", spec.ID, prev, i)
		}
		seen[spec.ID] = i
	}
}

// TestCatalogNoDuplicateEntryPoints ensures no two operations claim the same
// entry point.
func TestCatalogNoDuplicateEntryPoints(t *testing.T) {
	seen := make(map[string]OperationID)
	for _, spec := range Catalog {
		for _, ep := range spec.EntryPoints {
			key := string(ep.Kind) + ":" + ep.Method + ":" + ep.Pattern
			if owner, ok := seen[key]; ok {
				t.Errorf("entry point %s claimed by both %q and %q", key, owner, spec.ID)
			}
			seen[key] = spec.ID
		}
	}
}

// ---------------------------------------------------------------------------
// Entry point exemption validation
// ---------------------------------------------------------------------------

// TestEntryPointExemptionsAreValid ensures every exemption has required fields
// and uses a valid exemption kind.
func TestEntryPointExemptionsAreValid(t *testing.T) {
	if len(EntryPointExemptions) == 0 {
		t.Fatal("no entry point exemptions defined")
	}
	seen := make(map[string]bool)
	for i, ex := range EntryPointExemptions {
		if ex.Pattern == "" {
			t.Errorf("exemption [%d]: empty pattern", i)
		}
		if ex.Kind == "" {
			t.Errorf("exemption [%d] (%s): empty kind", i, ex.Pattern)
		} else if !validExemptionKinds[ex.Kind] {
			t.Errorf("exemption [%d] (%s): unknown kind %q", i, ex.Pattern, ex.Kind)
		}
		if ex.Reason == "" {
			t.Errorf("exemption [%d] (%s): empty reason", i, ex.Pattern)
		}
		if ex.Owner == "" {
			t.Errorf("exemption [%d] (%s): empty owner", i, ex.Pattern)
		}
		if seen[ex.Pattern] {
			t.Errorf("exemption [%d] (%s): duplicate pattern", i, ex.Pattern)
		}
		seen[ex.Pattern] = true
	}
	t.Logf("entry point exemptions: %d validated", len(EntryPointExemptions))
}

// TestNoExemptionOverlapWithCatalog ensures no route is both cataloged as an
// operation entry point and also listed as an exempt entry point.
func TestNoExemptionOverlapWithCatalog(t *testing.T) {
	catalogEPs := make(map[string]OperationID)
	for _, spec := range Catalog {
		for _, ep := range spec.EntryPoints {
			catalogEPs[ep.Pattern] = spec.ID
		}
	}
	for _, ex := range EntryPointExemptions {
		if opID, ok := catalogEPs[ex.Pattern]; ok {
			t.Errorf("pattern %q is both cataloged (operation %q) and exempted", ex.Pattern, opID)
		}
	}
}

// ---------------------------------------------------------------------------
// Permission coverage validation
// ---------------------------------------------------------------------------

// permissionRegistryIDs returns the set of permission IDs that are registered
// in the production permission registry. This reads the registry source file
// to avoid importing pkg/hub/permissions (which would create a cycle).
func permissionRegistryIDs(t *testing.T) map[string]bool {
	t.Helper()
	repoRoot := findRepoRoot(t)
	registryFile := filepath.Join(repoRoot, "pkg/hub/permissions/registry.go")
	content, err := os.ReadFile(registryFile)
	if err != nil {
		t.Fatalf("cannot read permission registry: %v", err)
	}

	ids := make(map[string]bool)
	// Parse registry entries looking for ID: "..." patterns
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{ID:") {
			continue
		}
		// Extract the ID value
		start := strings.Index(line, `"`)
		if start < 0 {
			continue
		}
		end := strings.Index(line[start+1:], `"`)
		if end < 0 {
			continue
		}
		id := line[start+1 : start+1+end]
		if id != "" {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		t.Fatal("found no permission IDs in registry — parser is broken")
	}
	return ids
}

// TestCatalogBasePermissionsExist verifies that every operation's base
// permission exists in the production permission registry.
func TestCatalogBasePermissionsExist(t *testing.T) {
	registryIDs := permissionRegistryIDs(t)

	for _, spec := range Catalog {
		if spec.BasePermission == "" {
			continue // waived via exemption
		}
		if !registryIDs[spec.BasePermission] {
			t.Errorf("operation %q references base permission %q which is not in the permission registry",
				spec.ID, spec.BasePermission)
		}
	}
}

// TestCatalogBasePermissionSemantics validates that each operation's base
// permission has compatible resource/action semantics. Assertive validation
// is in TestCatalogBasePermissionSemanticsAssertive; this test provides the
// detailed log output for debugging.
func TestCatalogBasePermissionSemantics(t *testing.T) {
	repoRoot := findRepoRoot(t)
	registryFile := filepath.Join(repoRoot, "pkg/hub/permissions/registry.go")
	content, err := os.ReadFile(registryFile)
	if err != nil {
		t.Fatalf("cannot read permission registry: %v", err)
	}

	type permInfo struct {
		resource string
		action   string
	}
	perms := make(map[string]permInfo)
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{ID:") {
			continue
		}
		id := extractQuoted(line, "ID:")
		resource := extractField(line, "Resource:")
		action := extractField(line, "Action:")
		if id != "" {
			perms[id] = permInfo{resource: resource, action: action}
		}
	}

	for _, spec := range Catalog {
		if spec.BasePermission == "" {
			continue
		}
		pi, ok := perms[spec.BasePermission]
		if !ok {
			continue // covered by TestCatalogBasePermissionsExist
		}
		t.Logf("operation %q: permission %q (resource=%s, action=%s)",
			spec.ID, spec.BasePermission, pi.resource, pi.action)
	}
}

// TestRegisteredPermissionsConsumed verifies that every permission registered
// in the production registry is consumed by at least one catalog operation or
// is explicitly documented as reserved/deferred.
func TestRegisteredPermissionsConsumed(t *testing.T) {
	registryIDs := permissionRegistryIDs(t)
	catalogPerms := CatalogBasePermissions()

	// Permissions not directly consumed as a BasePermission by any catalog
	// operation. Permissions already consumed are excluded from this map.
	reservedOrDeferred := map[string]string{
		// List permissions covered by the corresponding read operation
		// but registered as distinct permission IDs in the registry.
		"agent.list":               "List subset of agent.read operation",
		"skill.list":               "List subset of skill.read operation",
		"template.list":            "List subset of template.read operation",
		"harness_config.list":      "List subset of harnessconfig.read operation",
		"group.list":               "List subset of group.read operation",
		"gcp_service_account.list": "List subset of gcp.identity.read operation",
		"scheduled_event.list":     "List subset of schedule.event.read operation",
		"broker.list":              "List subset of broker.read operation",

		// Deprecated policy permissions (CO1 cutover, 410 Gone)
		"policy.create": "Deprecated (CO1 cutover, 410 Gone)",
		"policy.read":   "Deprecated (CO1 cutover, 410 Gone)",
		"policy.update": "Deprecated (CO1 cutover, 410 Gone)",
		"policy.delete": "Deprecated (CO1 cutover, 410 Gone)",
		"policy.list":   "Deprecated (CO1 cutover, 410 Gone)",

		// Broker internal operations — broker-HMAC only, not user-facing
		"broker.create":   "Broker-HMAC only, not user-facing",
		"broker.update":   "Broker-HMAC only, not user-facing",
		"broker.delete":   "Broker-HMAC only, not user-facing",
		"broker.dispatch": "Broker-HMAC dispatch, not user-facing",

		// Hub admin permissions — NonRouteUse only (no route declaration)
		"hub.settings.read":           "NonRouteUse only, no route declaration",
		"hub.settings.update":         "NonRouteUse only, no route declaration",
		"hub.admin_mode.read":         "NonRouteUse only, no route declaration",
		"hub.integrations.update":     "NonRouteUse only, no route declaration",
		"hub.lifecycle_hooks.update":  "NonRouteUse only, no route declaration",
		"hub.allow_list.read":         "NonRouteUse only, no route declaration",
		"hub.project_defaults.update": "NonRouteUse only, no route declaration",
		"hub.scheduler.update":        "NonRouteUse only, no route declaration",
		"hub.federation.read":         "NonRouteUse only, no route declaration",
		"hub.federation.update":       "NonRouteUse only, no route declaration",
		"hub.teams_manifest.update":   "NonRouteUse only, no route declaration",
		"hub.github_app.read":         "NonRouteUse only, no route declaration",
		"hub.github_app.update":       "NonRouteUse only, no route declaration",
		"hub.audit.read":              "Super-admin audit explain, NonRouteUse only",

		// User/project permissions — NonRouteUse only
		"user.list":     "NonRouteUse only, no route declaration",
		"project.clone": "NonRouteUse only, no route declaration",
		"project.list":  "NonRouteUse only, no route declaration",

		// Agent token scopes — not route-enforced
		"agent.status_update":  "Agent token scope, not route-enforced",
		"agent.log_append":     "Agent token scope, not route-enforced",
		"project.secret_read":  "Agent token scope, not route-enforced",
		"agent.notify":         "Agent token scope, not route-enforced",
		"agent.token_refresh":  "Agent token scope, not route-enforced",
		"agent.port_forward":   "Agent token scope, not route-enforced",
		"agent.identity_token": "Agent token scope, not route-enforced",
	}

	var unconsumed []string
	for id := range registryIDs {
		if _, inCatalog := catalogPerms[id]; inCatalog {
			continue
		}
		if _, reserved := reservedOrDeferred[id]; reserved {
			continue
		}
		unconsumed = append(unconsumed, id)
	}

	if len(unconsumed) > 0 {
		t.Errorf("permissions not consumed by any catalog operation and not reserved/deferred:\n  %s",
			strings.Join(unconsumed, "\n  "))
	}

	// Count for report
	consumed := 0
	for id := range registryIDs {
		if _, ok := catalogPerms[id]; ok {
			consumed++
		}
	}
	t.Logf("permission coverage: %d/%d consumed by catalog, %d reserved/deferred",
		consumed, len(registryIDs), len(reservedOrDeferred))
}

// ---------------------------------------------------------------------------
// Bidirectional entry-point reconciliation (R2)
// ---------------------------------------------------------------------------

// routeMetadataKeys reads route_metadata.go source and extracts the set of
// pattern keys from routeMetadataTable. This avoids importing pkg/hub (which
// would create a dependency cycle: authzop → hub).
func routeMetadataKeys(t *testing.T) map[string]bool {
	t.Helper()
	repoRoot := findRepoRoot(t)
	routeFile := filepath.Join(repoRoot, "pkg/hub/route_metadata.go")
	content, err := os.ReadFile(routeFile)
	if err != nil {
		t.Fatalf("cannot read route_metadata.go: %v", err)
	}

	keys := make(map[string]bool)
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Match lines like: "/api/v1/foo": RouteMetadata{...
		// or "/api/v1/foo": {
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		end := strings.Index(line[1:], `"`)
		if end < 0 {
			continue
		}
		key := line[1 : end+1]
		// Route patterns always start with "/" (or are "/healthz", etc.)
		if !strings.HasPrefix(key, "/") {
			continue
		}
		// Verify this is a map key (followed by ":")
		rest := strings.TrimSpace(line[end+2:])
		if !strings.HasPrefix(rest, ":") {
			continue
		}
		keys[key] = true
	}
	if len(keys) == 0 {
		t.Fatal("found no route patterns in route_metadata.go — parser is broken")
	}
	return keys
}

// catalogAndExemptionPatterns returns the combined set of path patterns from
// all catalog entry points and all entry-point exemptions. For catalog entry
// points, only the Pattern (without method prefix) is used.
func catalogAndExemptionPatterns() map[string]bool {
	patterns := make(map[string]bool)

	for _, spec := range Catalog {
		for _, ep := range spec.EntryPoints {
			patterns[ep.Pattern] = true
		}
	}
	for _, ex := range EntryPointExemptions {
		// Some exemptions use "METHOD /path" format; extract the path part
		pat := ex.Pattern
		if idx := strings.Index(pat, " "); idx >= 0 {
			pat = pat[idx+1:]
		}
		patterns[pat] = true
	}
	return patterns
}

// normalizeTrailingSlash converts a route-metadata trailing-slash pattern
// to the base prefix for matching. "/api/v1/agents/" → "/api/v1/agents/".
// Non-trailing-slash patterns are returned as-is.
func normalizeTrailingSlash(pattern string) (base string, hasTrailingSlash bool) {
	if strings.HasSuffix(pattern, "/") && len(pattern) > 1 {
		return pattern, true
	}
	return pattern, false
}

// TestEntryPointsCoverRouteMetadata verifies every route-metadata entry is
// covered by either a catalog entry-point pattern or an EntryPointExemption
// pattern. This is the R2 automated bidirectional reconciliation gate.
//
// Route-metadata trailing-slash patterns (e.g., "/api/v1/agents/") are
// covered when any catalog or exemption pattern starts with the same prefix
// (e.g., "/api/v1/agents/{id}", "/api/v1/agents/{id}/attach"). This accounts
// for the parameterized-vs-trailing-slash difference documented in F-AF1-01.
func TestEntryPointsCoverRouteMetadata(t *testing.T) {
	routeKeys := routeMetadataKeys(t)
	covered := catalogAndExemptionPatterns()

	uncoveredRoutes := findUncoveredRoutes(routeKeys, covered)
	if len(uncoveredRoutes) > 0 {
		t.Errorf("route-metadata entries not covered by any catalog operation or exemption (%d/%d):\n  %s",
			len(uncoveredRoutes), len(routeKeys), strings.Join(uncoveredRoutes, "\n  "))
	}

	t.Logf("route-metadata reconciliation: %d routes, %d covered, %d catalog+exemption patterns",
		len(routeKeys), len(routeKeys)-len(uncoveredRoutes), len(covered))
}

// TestStaleExemptionDetection verifies that no exemption pattern references a
// route that does not exist in routeMetadataTable (allowing for known non-route
// patterns like OIDC and method-prefixed entries).
func TestStaleExemptionDetection(t *testing.T) {
	routeKeys := routeMetadataKeys(t)
	stale := findStaleExemptions(EntryPointExemptions, routeKeys)
	if len(stale) > 0 {
		t.Errorf("stale exemption patterns (not in routeMetadataTable and not known non-route): %s",
			strings.Join(stale, ", "))
	}
}

// TestProjectMembershipGovernance validates that project membership operations
// declare peer_superior governance per the CT1 approved appendix.
func TestProjectMembershipGovernance(t *testing.T) {
	membershipOps := []OperationID{
		"project.membership.add",
		"project.membership.update",
		"project.membership.remove",
	}

	for _, opID := range membershipOps {
		spec := findSpec(opID)
		if spec == nil {
			t.Errorf("missing catalog entry for %q", opID)
			continue
		}
		if spec.Governance == nil {
			t.Errorf("operation %q must declare governance (CT1 approved appendix)", opID)
			continue
		}
		if spec.Governance.Kind != GovernancePeerSuperior {
			t.Errorf("operation %q: expected peer_superior governance, got %q", opID, spec.Governance.Kind)
		}
	}
}

// ---------------------------------------------------------------------------
// Test ref validation — verify referenced test packages/functions exist
// ---------------------------------------------------------------------------

// TestCatalogTestRefsExist validates that test refs reference packages and
// functions that exist in the repository.
func TestCatalogTestRefsExist(t *testing.T) {
	repoRoot := findRepoRoot(t)
	refs := collectTestRefs(Catalog)
	missing := findMissingTestRefs(repoRoot, refs)
	for _, m := range missing {
		t.Error(m)
	}
}

// ---------------------------------------------------------------------------
// Proof tests — deliberately introduced violations must be caught
// ---------------------------------------------------------------------------

// TestProofDuplicateEntryPointRejected proves that a deliberately introduced
// duplicate entry point is detected by ValidateSpecs. Renamed from
// TestProofUnclassifiedEntryPointRejected per R2 — the test verifies
// duplicate detection, not unclassified detection.
func TestProofDuplicateEntryPointRejected(t *testing.T) {
	proofSpec := OperationSpec{
		ID:          "proof.duplicate.entrypoint.a",
		Domain:      "proof",
		Description: "Proof test: duplicate entry point detection",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/proof/dup-ep", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "proof-resolver",
		BasePermission:   "proof.test",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestProofDuplicateEntryPointRejected"}},
	}

	if err := proofSpec.Validate(); err != nil {
		t.Fatalf("proof spec should be structurally valid: %v", err)
	}

	dup := proofSpec
	dup.ID = "proof.duplicate.entrypoint.b"
	allSpecs := make([]OperationSpec, len(Catalog)+2)
	copy(allSpecs, Catalog)
	allSpecs[len(Catalog)] = proofSpec
	allSpecs[len(Catalog)+1] = dup
	if err := ValidateSpecs(allSpecs); err == nil {
		t.Error("expected ValidateSpecs to catch duplicate entry point, but it passed")
	}
}

// TestProofDuplicateOperationIDRejected proves that a duplicate operation ID
// is detected by ValidateSpecs.
func TestProofDuplicateOperationIDRejected(t *testing.T) {
	if len(Catalog) < 1 {
		t.Skip("catalog is empty")
	}
	// Create a spec with the same ID as the first catalog entry
	dup := Catalog[0]
	dup.EntryPoints = []EntryPoint{
		{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/proof/dup-id", Method: "GET"},
	}
	allSpecs := append(append([]OperationSpec{}, Catalog...), dup)
	err := ValidateSpecs(allSpecs)
	if err == nil {
		t.Error("expected ValidateSpecs to catch duplicate operation ID, but it passed")
	}
	if !strings.Contains(err.Error(), "duplicate operation ID") {
		t.Errorf("expected 'duplicate operation ID' error, got: %v", err)
	}
}

// TestProofMissingGovernanceRejected proves that removing governance from a
// project membership operation fails validation, enforcing the CT1 appendix.
func TestProofMissingGovernanceRejected(t *testing.T) {
	spec := findSpec("project.membership.remove")
	if spec == nil {
		t.Fatal("project.membership.remove not found in catalog")
	}
	// Copy and remove governance
	modified := *spec
	modified.Governance = nil
	err := modified.Validate()
	if err == nil {
		t.Error("expected validation to fail when governance is removed from revoke-authority operation")
	}
	if !strings.Contains(err.Error(), "governance") {
		t.Errorf("expected governance-related error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Security mutation symbol validation
// ---------------------------------------------------------------------------

// TestSecurityMutationSymbolsNotEmpty ensures the mutation symbol set is
// populated.
func TestSecurityMutationSymbolsNotEmpty(t *testing.T) {
	if len(SecurityMutationSymbols) == 0 {
		t.Fatal("SecurityMutationSymbols is empty — no mutations to scan for")
	}
	t.Logf("security mutation symbols: %d defined", len(SecurityMutationSymbols))
}

// TestSecurityMutationScanProduction scans production source files for
// security-relevant mutation symbols and reports counts. This is an
// informational test that drives the mutation enumeration.
func TestSecurityMutationScanProduction(t *testing.T) {
	repoRoot := findRepoRoot(t)

	type callSite struct {
		file     string
		function string
		symbol   string
	}

	var sites []callSite

	scanRoots := []string{
		filepath.Join(repoRoot, "pkg/hub"),
		filepath.Join(repoRoot, "pkg/store"),
	}

	fset := token.NewFileSet()
	for _, root := range scanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip inaccessible dirs
			}
			if info.IsDir() {
				return nil // recurse
			}
			if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
				return nil
			}
			f, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				t.Errorf("parse error in %s: %v", path, parseErr)
				return nil
			}

			relPath, _ := filepath.Rel(repoRoot, path)

			// Walk AST to find call sites
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				var symbolName string
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					symbolName = fn.Sel.Name
				case *ast.Ident:
					symbolName = fn.Name
				}

				if symbolName == "" {
					return true
				}
				if _, isMutation := SecurityMutationSymbols[symbolName]; !isMutation {
					return true
				}

				enclosingFunc := findEnclosingFunc(fset, f, call.Pos())
				sites = append(sites, callSite{
					file:     relPath,
					function: enclosingFunc,
					symbol:   symbolName,
				})
				return true
			})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("cannot walk %s: %v", root, err)
		}
	}

	// Report counts by symbol
	bySym := make(map[string]int)
	for _, s := range sites {
		bySym[s.symbol]++
	}

	t.Logf("security mutation scan: %d call sites found across %d symbols", len(sites), len(bySym))
	for sym, count := range bySym {
		t.Logf("  %s: %d sites", sym, count)
	}
}

// ---------------------------------------------------------------------------
// Report generation
// ---------------------------------------------------------------------------

// TestGenerateCoverageReport generates the reviewer-facing operation/coverage
// report. The report is deterministic and can be checked for staleness.
func TestGenerateCoverageReport(t *testing.T) {
	report := RenderMarkdown(Catalog)
	if report == "" {
		t.Fatal("generated report is empty")
	}

	// Verify determinism
	report2 := RenderMarkdown(Catalog)
	if report != report2 {
		t.Error("report generation is not deterministic")
	}

	// Count operations by domain
	byDomain := make(map[string]int)
	for _, spec := range Catalog {
		byDomain[spec.Domain]++
	}

	// Count exemptions by kind
	exemptByKind := make(map[ExemptionKind]int)
	for _, ex := range EntryPointExemptions {
		exemptByKind[ex.Kind]++
	}

	t.Logf("Coverage Report Summary:")
	t.Logf("  Operations cataloged: %d", len(Catalog))
	t.Logf("  Entry point exemptions: %d", len(EntryPointExemptions))
	t.Logf("  Operations by domain:")
	for domain, count := range byDomain {
		t.Logf("    %s: %d", domain, count)
	}
	t.Logf("  Exemptions by kind:")
	for kind, count := range exemptByKind {
		t.Logf("    %s: %d", kind, count)
	}
}

// ---------------------------------------------------------------------------
// Shared production validation helpers
//
// These functions contain the production validation logic used by both the
// gate tests and the proof tests. Each proof test injects a violation into
// production-derived data and calls the same helper that the gate test uses.
// ---------------------------------------------------------------------------

// findUncoveredRoutes returns route-metadata keys not covered by any catalog
// entry-point or exemption pattern. Used by TestEntryPointsCoverRouteMetadata
// and TestProofUnclassifiedEntryPointDetected.
func findUncoveredRoutes(routeKeys map[string]bool, covered map[string]bool) []string {
	var uncovered []string
	for route := range routeKeys {
		base, hasSuffix := normalizeTrailingSlash(route)
		if hasSuffix {
			found := false
			if covered[base] {
				found = true
			}
			if !found {
				for pat := range covered {
					if strings.HasPrefix(pat, base) {
						found = true
						break
					}
				}
			}
			if !found {
				uncovered = append(uncovered, route)
			}
		} else {
			if !covered[route] {
				uncovered = append(uncovered, route)
			}
		}
	}
	return uncovered
}

// knownNonRouteExemptions lists exemption patterns that are valid but do not
// correspond to routeMetadataTable keys (OIDC, agent identity token).
var knownNonRouteExemptions = map[string]bool{
	"GET /.well-known/openid-configuration": true,
	"GET /.well-known/jwks.json":            true,
	"POST /api/v1/agent/identity-token":     true,
}

// findStaleExemptions returns exemption patterns not in routeMetadataTable
// and not in the known non-route set. Used by TestStaleExemptionDetection
// and TestProofStaleExemptionDetected.
func findStaleExemptions(exemptions []EntryPointExemption, routeKeys map[string]bool) []string {
	var stale []string
	for _, ex := range exemptions {
		if knownNonRouteExemptions[ex.Pattern] {
			continue
		}
		if routeKeys[ex.Pattern] {
			continue
		}
		stale = append(stale, ex.Pattern)
	}
	return stale
}

// mutationClassificationCounts builds the key→count map for classifications.
func mutationClassificationCounts(classifications []MutationClassification) map[string]int {
	counts := make(map[string]int)
	for _, mc := range classifications {
		key := mc.File + ":" + mc.Function + ":" + mc.Symbol
		counts[key]++
	}
	return counts
}

// findUnclassifiedMutations returns discovered call-site keys that have
// fewer classifications than discoveries. Used by
// TestMutationClassificationBidirectional and TestProofUnclassifiedMutationDetected.
func findUnclassifiedMutations(discoveredCounts, classifiedCounts map[string]int) []string {
	var unclassified []string
	for key, dCount := range discoveredCounts {
		if classifiedCounts[key] < dCount {
			unclassified = append(unclassified, key)
		}
	}
	return unclassified
}

// findStaleMutationClassifications returns classification keys that have
// fewer discoveries than classifications. Used by
// TestMutationClassificationBidirectional and TestProofStaleMutationClassificationDetected.
func findStaleMutationClassifications(discoveredCounts, classifiedCounts map[string]int) []string {
	var stale []string
	for key, cCount := range classifiedCounts {
		if discoveredCounts[key] < cCount {
			stale = append(stale, key)
		}
	}
	return stale
}

// collectTestRefs collects all unique package:function pairs from catalog test refs.
func collectTestRefs(catalog []OperationSpec) map[string][]OperationID {
	refs := make(map[string][]OperationID)
	for _, spec := range catalog {
		for _, tr := range spec.TestRefs {
			key := tr.Package + ":" + tr.Function
			refs[key] = append(refs[key], spec.ID)
		}
	}
	return refs
}

// findMissingTestRefs returns error messages for test refs that reference
// nonexistent packages or functions. Used by TestCatalogTestRefsExist and
// TestProofNonexistentTestRefDetected.
func findMissingTestRefs(repoRoot string, refs map[string][]OperationID) []string {
	var missing []string
	for ref, ops := range refs {
		parts := strings.SplitN(ref, ":", 2)
		pkg, fn := parts[0], parts[1]

		pkgDir := filepath.Join(repoRoot, pkg)
		if _, err := os.Stat(pkgDir); os.IsNotExist(err) {
			missing = append(missing, "test ref package "+pkg+" does not exist (referenced by "+opIDsStr(ops)+")")
			continue
		}

		found := false
		testFiles, _ := filepath.Glob(filepath.Join(pkgDir, "*_test.go"))
		for _, tf := range testFiles {
			content, err := os.ReadFile(tf)
			if err != nil {
				continue
			}
			if strings.Contains(string(content), "func "+fn+"(") {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, "test function "+fn+" not found in package "+pkg+" (referenced by "+opIDsStr(ops)+")")
		}
	}
	return missing
}

func opIDsStr(ops []OperationID) string {
	ss := make([]string, len(ops))
	for i, id := range ops {
		ss[i] = string(id)
	}
	return strings.Join(ss, ", ")
}

// permissionResourceTypes reads the permission registry source and returns
// a map of permission ID → resource type string.
func permissionResourceTypes(t *testing.T) map[string]string {
	t.Helper()
	repoRoot := findRepoRoot(t)
	registryFile := filepath.Join(repoRoot, "pkg/hub/permissions/registry.go")
	content, err := os.ReadFile(registryFile)
	if err != nil {
		t.Fatalf("cannot read permission registry: %v", err)
	}
	permResources := make(map[string]string)
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{ID:") {
			continue
		}
		id := extractQuoted(line, "ID:")
		resource := extractField(line, "Resource:")
		if id != "" && resource != "" {
			permResources[id] = resource
		}
	}
	return permResources
}

// findDomainResourceMismatches validates that each operation's base permission
// has a Resource type compatible with its domain per the compatibility map.
// Returns error messages for mismatches and unmapped domains. Used by
// TestCatalogBasePermissionSemanticsAssertive and TestProofPermissionSemanticMismatchDetected.
func findDomainResourceMismatches(catalog []OperationSpec, permResources map[string]string, compatMap map[string][]string) []string {
	var errors []string
	for _, spec := range catalog {
		if spec.BasePermission == "" {
			continue
		}
		resource, ok := permResources[spec.BasePermission]
		if !ok {
			continue // covered by TestCatalogBasePermissionsExist
		}

		domain := spec.Domain
		bestMatch := ""
		var bestResources []string
		for d, allowedResources := range compatMap {
			if domain == d || strings.HasPrefix(domain, d+".") {
				if len(d) > len(bestMatch) {
					bestMatch = d
					bestResources = allowedResources
				}
			}
		}
		if bestMatch != "" {
			compatible := false
			for _, ar := range bestResources {
				if resource == ar {
					compatible = true
					break
				}
			}
			if !compatible {
				errors = append(errors, "operation "+string(spec.ID)+" (domain="+domain+"): permission "+
					spec.BasePermission+" has resource "+resource+", expected one of "+strings.Join(bestResources, ", "))
			}
		} else {
			errors = append(errors, "operation "+string(spec.ID)+": domain "+domain+
				" has no explicit resource mapping (resource="+resource+") — add to domainResourceCompatibility")
		}
	}
	return errors
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func findRepoRoot(t *testing.T) string {
	t.Helper()
	// Walk up from the test file's directory to find go.mod
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find repository root (no go.mod)")
		}
		dir = parent
	}
}

func findSpec(id OperationID) *OperationSpec {
	for i := range Catalog {
		if Catalog[i].ID == id {
			return &Catalog[i]
		}
	}
	return nil
}

func extractQuoted(line, prefix string) string {
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(prefix):]
	start := strings.Index(rest, `"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(rest[start+1:], `"`)
	if end < 0 {
		return ""
	}
	return rest[start+1 : start+1+end]
}

func extractField(line, prefix string) string {
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(prefix):]
	rest = strings.TrimSpace(rest)
	// Handle both Resource"type" and ResourceType constant references
	if strings.HasPrefix(rest, "Resource") || strings.HasPrefix(rest, "Action") {
		end := strings.IndexAny(rest, ",}")
		if end < 0 {
			return rest
		}
		return strings.TrimSpace(rest[:end])
	}
	return extractQuoted(line, prefix)
}

func findEnclosingFunc(fset *token.FileSet, file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Body == nil {
			continue
		}
		if fn.Pos() <= pos && pos <= fn.End() {
			return fn.Name.Name
		}
	}
	return "?"
}

// ---------------------------------------------------------------------------
// Generated report staleness gate
// ---------------------------------------------------------------------------

// TestCatalogReportStaleness verifies the generated catalog report matches
// the checked-in version. If no report is checked in yet, this test creates
// the baseline.
func TestCatalogReportStaleness(t *testing.T) {
	repoRoot := findRepoRoot(t)
	reportPath := filepath.Join(repoRoot, ".design", "authorization-operation-catalog.md")

	generated := RenderMarkdown(Catalog)

	existing, err := os.ReadFile(reportPath)
	if err != nil {
		if os.IsNotExist(err) {
			// First generation — write the baseline
			if err := os.WriteFile(reportPath, []byte(generated), 0644); err != nil {
				t.Fatalf("cannot write catalog report: %v", err)
			}
			t.Logf("generated initial catalog report at %s", reportPath)
			return
		}
		t.Fatalf("cannot read catalog report: %v", err)
	}

	if string(existing) != generated {
		// Update the file so the diff is visible in git
		if err := os.WriteFile(reportPath, []byte(generated), 0644); err != nil {
			t.Logf("WARNING: could not update catalog report: %v", err)
		}
		t.Error("catalog report is stale — regenerated. Run 'go test ./pkg/hub/authzop/...' and commit the updated report.")
	}
}

// ---------------------------------------------------------------------------
// Mutation classification validation (R3)
// ---------------------------------------------------------------------------

// TestMutationClassificationsValid verifies structural validity of every
// MutationClassification entry: exactly one of OperationID or Exemption must
// be set, and OperationID (if set) must exist in the catalog.
func TestMutationClassificationsValid(t *testing.T) {
	if len(MutationClassifications) == 0 {
		t.Fatal("MutationClassifications is empty — no call sites classified")
	}

	catalogIDs := CatalogOperationIDs()

	for i, mc := range MutationClassifications {
		if mc.File == "" || mc.Function == "" || mc.Symbol == "" {
			t.Errorf("classification [%d]: missing file/function/symbol", i)
		}
		hasOp := mc.OperationID != ""
		hasEx := mc.Exemption != nil
		if hasOp == hasEx {
			t.Errorf("classification [%d] (%s:%s:%s): exactly one of OperationID or Exemption must be set (op=%v, ex=%v)",
				i, mc.File, mc.Function, mc.Symbol, hasOp, hasEx)
		}
		if hasOp && !catalogIDs[mc.OperationID] {
			t.Errorf("classification [%d] (%s:%s:%s): OperationID %q not found in catalog",
				i, mc.File, mc.Function, mc.Symbol, mc.OperationID)
		}
		if hasEx {
			if mc.Exemption.Kind == "" {
				t.Errorf("classification [%d] (%s:%s:%s): exemption has empty Kind",
					i, mc.File, mc.Function, mc.Symbol)
			} else if !validExemptionKinds[mc.Exemption.Kind] {
				t.Errorf("classification [%d] (%s:%s:%s): unknown exemption kind %q",
					i, mc.File, mc.Function, mc.Symbol, mc.Exemption.Kind)
			}
			if mc.Exemption.Reason == "" {
				t.Errorf("classification [%d] (%s:%s:%s): exemption has empty Reason",
					i, mc.File, mc.Function, mc.Symbol)
			}
		}
	}
	t.Logf("mutation classifications: %d entries validated", len(MutationClassifications))
}

// TestMutationClassificationBidirectional verifies bidirectional agreement
// between the scanner and the classification table:
//  1. Every scanner-discovered call site must have a classification entry.
//  2. Every classification entry must correspond to a scanner-discovered site.
func TestMutationClassificationBidirectional(t *testing.T) {
	repoRoot := findRepoRoot(t)

	// Run the same scanner logic as TestSecurityMutationScanProduction
	type callSite struct {
		file     string
		function string
		symbol   string
	}

	var discovered []callSite

	scanRoots := []string{
		filepath.Join(repoRoot, "pkg/hub"),
		filepath.Join(repoRoot, "pkg/store"),
	}

	fset := token.NewFileSet()
	for _, root := range scanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
				return nil
			}
			f, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return nil
			}
			relPath, _ := filepath.Rel(repoRoot, path)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var symbolName string
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					symbolName = fn.Sel.Name
				case *ast.Ident:
					symbolName = fn.Name
				}
				if symbolName == "" {
					return true
				}
				if _, isMutation := SecurityMutationSymbols[symbolName]; !isMutation {
					return true
				}
				enclosingFunc := findEnclosingFunc(fset, f, call.Pos())
				discovered = append(discovered, callSite{file: relPath, function: enclosingFunc, symbol: symbolName})
				return true
			})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("cannot walk %s: %v", root, err)
		}
	}

	// Build classification lookup
	classifiedCounts := mutationClassificationCounts(MutationClassifications)

	// Build discovered lookup
	discoveredCounts := make(map[string]int)
	for _, s := range discovered {
		key := s.file + ":" + s.function + ":" + s.symbol
		discoveredCounts[key]++
	}

	unclassified := findUnclassifiedMutations(discoveredCounts, classifiedCounts)
	if len(unclassified) > 0 {
		t.Errorf("unclassified mutation call sites (%d):\n  %s",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}

	stale := findStaleMutationClassifications(discoveredCounts, classifiedCounts)
	if len(stale) > 0 {
		t.Errorf("stale mutation classifications (%d):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}

	t.Logf("mutation classification bidirectional check: %d discovered, %d classified",
		len(discovered), len(MutationClassifications))
}

// ---------------------------------------------------------------------------
// Assertive permission semantic validation (R6)
// ---------------------------------------------------------------------------

// domainResourceCompatibility defines the expected mapping between catalog
// operation domains and permission resource types. Every operation's base
// permission must have a Resource type that is compatible with its domain.
var domainResourceCompatibility = map[string][]string{
	"project.membership": {"ResourceProject", "ResourceRoleBinding"},
	"role":               {"ResourceRole"},
	"role.binding":       {"ResourceRoleBinding"},
	"group":              {"ResourceGroup"},
	"access.constraint":  {"ResourceAccessConstraint"},
	"credential":         {"ResourceUser"},
	"gcp.identity":       {"ResourceGCPServiceAccount"},
	"agent":              {"ResourceAgent"},
	"project":            {"ResourceProject"},
	"agent.message":      {"ResourceAgent"},
	"user.admin":         {"ResourceUser"},
	"secret":             {"ResourceProject"},
	"hub":                {"ResourceHub"},
	"hub.admin":          {"ResourceHub", "ResourceProject"},
	"skill":              {"ResourceSkill"},
	"template":           {"ResourceTemplate"},
	"harnessconfig":      {"ResourceHarnessConfig"},
	"user":               {"ResourceUser"},
	"broker":             {"ResourceBroker"},
	"quota":              {"ResourceQuota"},
	"schedule":           {"ResourceScheduledEvent"},
	"chat":               {"ResourceProject"},
	"env":                {"ResourceProject"},
}

// TestCatalogBasePermissionSemanticsAssertive validates that each operation's
// base permission has a Resource type compatible with its domain per the
// explicit mapping table above. This replaces the log-only version (R6).
func TestCatalogBasePermissionSemanticsAssertive(t *testing.T) {
	permResources := permissionResourceTypes(t)
	mismatches := findDomainResourceMismatches(Catalog, permResources, domainResourceCompatibility)
	for _, m := range mismatches {
		t.Error(m)
	}
}

// ---------------------------------------------------------------------------
// Additional proof tests (R4)
// ---------------------------------------------------------------------------

// TestProofUnclassifiedEntryPointDetected proves that a route-metadata entry
// not covered by any catalog operation or exemption is detected by the
// production reconciliation helper. Injects a fake route into production-
// derived route keys and calls findUncoveredRoutes.
func TestProofUnclassifiedEntryPointDetected(t *testing.T) {
	routeKeys := routeMetadataKeys(t)
	covered := catalogAndExemptionPatterns()

	// Inject a fake route into production-derived data
	fakeRoute := "/api/v1/proof/unclassified-route"
	routeKeys[fakeRoute] = true

	// Call the same production helper used by TestEntryPointsCoverRouteMetadata
	uncovered := findUncoveredRoutes(routeKeys, covered)

	found := false
	for _, u := range uncovered {
		if u == fakeRoute {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected findUncoveredRoutes to detect injected unclassified route, but it did not")
	}
}

// TestProofUnclassifiedMutationDetected proves that a scanner-discovered
// mutation call site without a classification entry is detected by the
// production bidirectional helper. Injects a fake call site into production-
// derived counts and calls findUnclassifiedMutations.
func TestProofUnclassifiedMutationDetected(t *testing.T) {
	// Start from production classification counts
	classifiedCounts := mutationClassificationCounts(MutationClassifications)

	// Inject a fake discovered site not in any classification
	discoveredCounts := map[string]int{
		"pkg/hub/proof_injected.go:proofFunc:CreateRoleBinding": 1,
	}

	// Call the same production helper used by TestMutationClassificationBidirectional
	unclassified := findUnclassifiedMutations(discoveredCounts, classifiedCounts)
	if len(unclassified) == 0 {
		t.Error("expected findUnclassifiedMutations to detect injected call site, but it did not")
	}
}

// TestProofStaleMutationClassificationDetected proves that a classification
// entry for a non-existent call site is detected by the production
// bidirectional helper. Injects a fake classification into production-derived
// counts and calls findStaleMutationClassifications.
func TestProofStaleMutationClassificationDetected(t *testing.T) {
	// Start from production classification counts and inject a stale entry
	classifiedCounts := mutationClassificationCounts(MutationClassifications)
	classifiedCounts["pkg/hub/nonexistent.go:noFunc:DeleteRoleBinding"] = 1

	// Use empty discovered set — the stale entry won't match anything
	discoveredCounts := make(map[string]int)

	// Call the same production helper used by TestMutationClassificationBidirectional
	stale := findStaleMutationClassifications(discoveredCounts, classifiedCounts)

	found := false
	for _, s := range stale {
		if strings.Contains(s, "nonexistent.go") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected findStaleMutationClassifications to detect injected stale entry, but it did not")
	}
}

// TestProofStaleExemptionDetected proves that an exemption for a pattern not
// in routeMetadataTable is detected by the production stale-exemption helper.
// Injects a fake exemption into production-derived data and calls
// findStaleExemptions.
func TestProofStaleExemptionDetected(t *testing.T) {
	routeKeys := routeMetadataKeys(t)

	// Build exemptions list with an injected stale entry
	fakePattern := "/api/v1/proof/nonexistent-exempt"
	injected := append(
		append([]EntryPointExemption{}, EntryPointExemptions...),
		EntryPointExemption{Pattern: fakePattern, Kind: ExemptionInternalOnly, Reason: "proof", Owner: "proof"},
	)

	// Call the same production helper used by TestStaleExemptionDetection
	stale := findStaleExemptions(injected, routeKeys)

	found := false
	for _, s := range stale {
		if s == fakePattern {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected findStaleExemptions to detect injected stale exemption, but it did not")
	}
}

// TestProofNonexistentTestRefDetected proves that a nonexistent test
// reference is detected by the production test-ref helper. Injects a fake
// test ref into production-derived data and calls findMissingTestRefs.
func TestProofNonexistentTestRefDetected(t *testing.T) {
	repoRoot := findRepoRoot(t)

	// Start from production test refs and inject a fake nonexistent one
	refs := collectTestRefs(Catalog)
	refs["pkg/hub/authzop:TestZZZ_NoSuchFunction_12345"] = []OperationID{"proof.testref"}

	// Call the same production helper used by TestCatalogTestRefsExist
	missing := findMissingTestRefs(repoRoot, refs)
	if len(missing) == 0 {
		t.Error("expected findMissingTestRefs to detect injected nonexistent test ref, but it did not")
	}
	found := false
	for _, m := range missing {
		if strings.Contains(m, "TestZZZ_NoSuchFunction_12345") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("findMissingTestRefs returned %v but did not flag the injected ref", missing)
	}
}

// TestProofPermissionSemanticMismatchDetected proves that a permission with
// an incompatible resource type for its domain is detected by the production
// semantic validation helper. Injects a spec with a mismatched resource type
// and calls findDomainResourceMismatches.
func TestProofPermissionSemanticMismatchDetected(t *testing.T) {
	// Create a fake catalog with a known domain-resource mismatch
	fakeSpec := OperationSpec{
		ID:             "proof.mismatch",
		Domain:         "project.membership",
		BasePermission: "proof.mismatch.perm",
	}

	// Fake permission registry with an incompatible resource
	permResources := map[string]string{
		"proof.mismatch.perm": "ResourceAgent", // ResourceAgent is NOT compatible with project.membership
	}

	// Call the same production helper used by TestCatalogBasePermissionSemanticsAssertive
	mismatches := findDomainResourceMismatches([]OperationSpec{fakeSpec}, permResources, domainResourceCompatibility)
	if len(mismatches) == 0 {
		t.Error("expected findDomainResourceMismatches to detect injected mismatch, but it did not")
	}
}

// TestProofUnmappedDomainDetected proves that a catalog operation with a
// domain not in domainResourceCompatibility is detected by the production
// semantic validation helper (fail-closed behavior).
func TestProofUnmappedDomainDetected(t *testing.T) {
	fakeSpec := OperationSpec{
		ID:             "proof.unmapped",
		Domain:         "completely.unknown.domain",
		BasePermission: "proof.unmapped.perm",
	}

	permResources := map[string]string{
		"proof.unmapped.perm": "ResourceUnknown",
	}

	// Call the same production helper — unmapped domain must produce an error
	mismatches := findDomainResourceMismatches([]OperationSpec{fakeSpec}, permResources, domainResourceCompatibility)
	if len(mismatches) == 0 {
		t.Error("expected findDomainResourceMismatches to detect unmapped domain, but it did not")
	}
}

// ---------------------------------------------------------------------------
// CI gate coverage assertion
// ---------------------------------------------------------------------------

// TestCIGateCoversAllAF1Tests verifies that all required AF1 gate test
// functions exist in this package. The CI gate script runs ALL tests in the
// package (not a -run regex), so this test ensures the minimum required set
// of gate tests is present. If a required gate test is accidentally deleted
// or renamed, this test catches it.
func TestCIGateCoversAllAF1Tests(t *testing.T) {
	requiredGateTests := []string{
		// Structural validation
		"TestCatalogValidation",
		"TestCatalogNoDuplicateIDs",
		"TestCatalogNoDuplicateEntryPoints",
		// Exemption validation
		"TestEntryPointExemptionsAreValid",
		"TestNoExemptionOverlapWithCatalog",
		// Permission coverage
		"TestCatalogBasePermissionsExist",
		"TestCatalogBasePermissionSemantics",
		"TestCatalogBasePermissionSemanticsAssertive",
		"TestRegisteredPermissionsConsumed",
		// Entry-point reconciliation
		"TestEntryPointsCoverRouteMetadata",
		"TestStaleExemptionDetection",
		// Governance
		"TestProjectMembershipGovernance",
		// Test ref validation
		"TestCatalogTestRefsExist",
		// Mutation classification
		"TestMutationClassificationsValid",
		"TestMutationClassificationBidirectional",
		// Mutation scan
		"TestSecurityMutationSymbolsNotEmpty",
		"TestSecurityMutationScanProduction",
		"TestSecurityMutationSymbolsValid",
		// Report
		"TestGenerateCoverageReport",
		"TestCatalogReportStaleness",
		// Proof tests
		"TestProofDuplicateEntryPointRejected",
		"TestProofDuplicateOperationIDRejected",
		"TestProofMissingGovernanceRejected",
		"TestProofUnclassifiedEntryPointDetected",
		"TestProofUnclassifiedMutationDetected",
		"TestProofStaleMutationClassificationDetected",
		"TestProofStaleExemptionDetected",
		"TestProofNonexistentTestRefDetected",
		"TestProofPermissionSemanticMismatchDetected",
		"TestProofUnmappedDomainDetected",
	}

	repoRoot := findRepoRoot(t)
	pkgDir := filepath.Join(repoRoot, "pkg/hub/authzop")
	testFiles, err := filepath.Glob(filepath.Join(pkgDir, "*_test.go"))
	if err != nil || len(testFiles) == 0 {
		t.Fatal("cannot find test files in pkg/hub/authzop")
	}

	// Read all test file contents
	var allContent strings.Builder
	for _, tf := range testFiles {
		content, err := os.ReadFile(tf)
		if err != nil {
			t.Fatalf("cannot read %s: %v", tf, err)
		}
		allContent.Write(content)
		allContent.WriteByte('\n')
	}
	src := allContent.String()

	for _, name := range requiredGateTests {
		if !strings.Contains(src, "func "+name+"(") {
			t.Errorf("required AF1 gate test %q not found in package — was it renamed or deleted?", name)
		}
	}
	t.Logf("CI gate coverage: %d required gate tests verified present", len(requiredGateTests))
}

// Ensure SecurityMutationSymbols keys are valid identifiers (no spaces, etc).
func TestSecurityMutationSymbolsValid(t *testing.T) {
	for sym, effect := range SecurityMutationSymbols {
		if sym == "" {
			t.Error("empty symbol in SecurityMutationSymbols")
		}
		if strings.ContainsAny(sym, " \t\n") {
			t.Errorf("symbol %q contains whitespace", sym)
		}
		if effect == "" {
			t.Errorf("symbol %q has empty effect classification", sym)
		}
	}
}
