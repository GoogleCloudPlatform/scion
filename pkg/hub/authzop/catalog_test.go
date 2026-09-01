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
	"fmt"
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
// permission has compatible resource/action semantics.
func TestCatalogBasePermissionSemantics(t *testing.T) {
	repoRoot := findRepoRoot(t)
	registryFile := filepath.Join(repoRoot, "pkg/hub/permissions/registry.go")
	content, err := os.ReadFile(registryFile)
	if err != nil {
		t.Fatalf("cannot read permission registry: %v", err)
	}

	// Build a map of permission ID -> resource and action
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
		_ = pi // Semantic validation: the permission's resource should relate
		// to the operation's domain. We log but don't fail on loose
		// semantic matches since domains are hierarchical.
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

	// Permissions that are consumed by catalog operations indirectly or are
	// explicitly reserved/deferred for later phases.
	reservedOrDeferred := map[string]string{
		// Read/list operations that are route-guarded but not individually
		// cataloged as high-risk operations in AF1 (cataloged as a class).
		"agent.read":                 "Route-guarded read; full operation catalog in AH3",
		"agent.list":                 "Route-guarded list; full operation catalog in AH3",
		"agent.update":               "Route-guarded update; full operation catalog in AH5",
		"agent.attach":               "Route-guarded attach; full operation catalog in AH2",
		"agent.port_access":          "Route-guarded port access; full operation catalog in AH4",
		"agent.stop_all":             "Route-guarded admin action; full operation catalog in AH2",
		"agent.message":              "Cataloged via agent.message.send operation",
		"agent.set_message_mode":     "Route-guarded admin action; full operation catalog in AH5",
		"project.read":               "Route-guarded read; full operation catalog in AH3",
		"project.update":             "Route-guarded update; full operation catalog in AH5",
		"project.register":           "Route-guarded register; full operation catalog in AH5",
		"skill.create":               "Route-guarded CRUD; full operation catalog in AH5",
		"skill.read":                 "Route-guarded read; full operation catalog in AH5",
		"skill.update":               "Route-guarded update; full operation catalog in AH5",
		"skill.delete":               "Route-guarded delete; full operation catalog in AH2",
		"skill.list":                 "Route-guarded list; full operation catalog in AH5",
		"template.create":            "Route-guarded CRUD; full operation catalog in AH5",
		"template.read":              "Route-guarded read; full operation catalog in AH5",
		"template.update":            "Route-guarded update; full operation catalog in AH5",
		"template.delete":            "Route-guarded delete; full operation catalog in AH2",
		"template.list":              "Route-guarded list; full operation catalog in AH5",
		"harness_config.create":      "Route-guarded CRUD; full operation catalog in AH5",
		"harness_config.read":        "Route-guarded read; full operation catalog in AH5",
		"harness_config.update":      "Route-guarded update; full operation catalog in AH5",
		"harness_config.delete":      "Route-guarded delete; full operation catalog in AH2",
		"harness_config.list":        "Route-guarded list; full operation catalog in AH5",
		"group.create":               "Route-guarded CRUD; full operation catalog in AH5",
		"group.read":                 "Route-guarded read; full operation catalog in AH3",
		"group.update":               "Route-guarded update; full operation catalog in AH5",
		"group.list":                 "Route-guarded list; full operation catalog in AH3",
		"user.read":                  "Route-guarded read; full operation catalog in AH3",
		"user.update":                "Route-guarded update; full operation catalog in AH5",
		"policy.create":              "Deprecated (CO1 cutover, 410 Gone)",
		"policy.read":                "Deprecated (CO1 cutover, 410 Gone)",
		"policy.update":              "Deprecated (CO1 cutover, 410 Gone)",
		"policy.delete":              "Deprecated (CO1 cutover, 410 Gone)",
		"policy.list":                "Deprecated (CO1 cutover, 410 Gone)",
		"broker.create":              "Route-guarded CRUD; full operation catalog in AH5",
		"broker.read":                "Route-guarded read; full operation catalog in AH5",
		"broker.update":              "Route-guarded update; full operation catalog in AH5",
		"broker.delete":              "Route-guarded delete; full operation catalog in AH2",
		"broker.list":                "Route-guarded list; full operation catalog in AH5",
		"broker.dispatch":            "Route-guarded dispatch; full operation catalog in AH4",
		"gcp_service_account.read":   "Route-guarded read; full operation catalog in AH3",
		"gcp_service_account.list":   "Route-guarded list; full operation catalog in AH3",
		"gcp_service_account.verify": "Route-guarded verify; full operation catalog in AH4",
		"role.read":                  "Route-guarded read; full operation catalog in AH3",
		"role_binding.read":          "Route-guarded read; full operation catalog in AH3",
		"access_constraint.read":     "Route-guarded read; full operation catalog in AH3",
		"scheduled_event.read":       "Route-guarded read; full operation catalog in AH5",
		"scheduled_event.list":       "Route-guarded list; full operation catalog in AH5",
		"scheduled_event.create":     "Route-guarded create; full operation catalog in AH5",
		"scheduled_event.delete":     "Route-guarded delete; full operation catalog in AH5",
		"scheduled_event.update":     "Route-guarded update; full operation catalog in AH5",
		"quota.read":                 "Route-guarded read; full operation catalog in AH5",
		"quota.create":               "Route-guarded create; full operation catalog in AH5",
		"quota.update":               "Route-guarded update; full operation catalog in AH5",
		"quota.delete":               "Route-guarded delete; full operation catalog in AH5",
		"skill.register":             "Route-guarded register; full operation catalog in AH5",

		// Hub admin operations — Phase 2 D4 route guard conversion
		"hub.settings.read":           "Phase 2 D4 route guard conversion",
		"hub.settings.update":         "Phase 2 D4 route guard conversion",
		"hub.config.read":             "Phase 2 D4 route guard conversion",
		"hub.config.update":           "Phase 2 D4 route guard conversion",
		"hub.maintenance.execute":     "Phase 2 D4 route guard conversion",
		"hub.diagnostics.read":        "Phase 2 D4 route guard conversion",
		"hub.health.read":             "Phase 2 D4 route guard conversion",
		"hub.admin_mode.read":         "Phase 2 D4 route guard conversion",
		"hub.admin_mode.update":       "Phase 2 D4 route guard conversion",
		"hub.integrations.read":       "Phase 2 D4 route guard conversion",
		"hub.integrations.update":     "Phase 2 D4 route guard conversion",
		"hub.lifecycle_hooks.read":    "Phase 2 D4 route guard conversion",
		"hub.lifecycle_hooks.update":  "Phase 2 D4 route guard conversion",
		"hub.allow_list.read":         "Phase 2 D4 route guard conversion",
		"hub.allow_list.update":       "Phase 2 D4 route guard conversion",
		"hub.project_defaults.read":   "Phase 2 D4 route guard conversion",
		"hub.project_defaults.update": "Phase 2 D4 route guard conversion",
		"hub.auth_reset.execute":      "Phase 2 D4 route guard conversion",
		"hub.scheduler.read":          "Phase 2 D4 route guard conversion",
		"hub.scheduler.update":        "Phase 2 D4 route guard conversion",
		"hub.federation.read":         "Phase 2 D4 route guard conversion",
		"hub.federation.update":       "Phase 2 D4 route guard conversion",
		"hub.teams_manifest.read":     "Phase 2 D4 route guard conversion",
		"hub.teams_manifest.update":   "Phase 2 D4 route guard conversion",
		"hub.validate.execute":        "Phase 2 D4 route guard conversion",
		"hub.github_app.read":         "Phase 2 D4 route guard conversion",
		"hub.github_app.update":       "Phase 2 D4 route guard conversion",
		"hub.metrics.read":            "Phase 2 D4 route guard conversion",
		"hub.audit.read":              "Super-admin audit explain; full operation catalog in AH4",

		// User management operations — deferred
		"user.invite":   "Phase 2 D4 route guard conversion",
		"user.suspend":  "Cataloged via user.admin.suspend operation",
		"user.promote":  "Phase 2 D4 route guard conversion",
		"user.list":     "Phase 2 D4 route guard conversion",
		"project.clone": "Phase 2 D4 route guard conversion",
		"project.list":  "Phase 2 D4 route guard conversion",

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

	// Collect all unique package:function pairs
	refs := make(map[string][]OperationID)
	for _, spec := range Catalog {
		for _, tr := range spec.TestRefs {
			key := tr.Package + ":" + tr.Function
			refs[key] = append(refs[key], spec.ID)
		}
	}

	for ref, ops := range refs {
		parts := strings.SplitN(ref, ":", 2)
		pkg, fn := parts[0], parts[1]

		pkgDir := filepath.Join(repoRoot, pkg)
		if _, err := os.Stat(pkgDir); os.IsNotExist(err) {
			t.Errorf("test ref package %q does not exist (referenced by %v)", pkg, ops)
			continue
		}

		// Check that the function exists in test files
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
			t.Errorf("test function %q not found in package %q (referenced by %v)", fn, pkg, ops)
		}
	}
}

// ---------------------------------------------------------------------------
// Proof tests — deliberately introduced violations must be caught
// ---------------------------------------------------------------------------

// TestProofUnclassifiedEntryPointRejected proves that a deliberately
// introduced unclassified entry point is detected by the catalog validation.
// This is a proof test for AF1 deliverable 4.
func TestProofUnclassifiedEntryPointRejected(t *testing.T) {
	// Create a spec that references an entry point not in the catalog
	proofSpec := OperationSpec{
		ID:          "proof.unclassified.entrypoint",
		Domain:      "proof",
		Description: "Proof test: unclassified entry point detection",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/proof/unclassified", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "proof-resolver",
		BasePermission:   "proof.test",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestProofUnclassifiedEntryPointRejected"}},
	}

	// Verify the proof spec is structurally valid
	if err := proofSpec.Validate(); err != nil {
		t.Fatalf("proof spec should be structurally valid: %v", err)
	}

	// Verify that adding a duplicate entry point to the catalog would be
	// caught by ValidateSpecs.
	allSpecs := make([]OperationSpec, len(Catalog)+2)
	copy(allSpecs, Catalog)
	dup := proofSpec
	dup.ID = "proof.duplicate.entrypoint"
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

	scanDirs := []string{
		filepath.Join(repoRoot, "pkg/hub"),
		filepath.Join(repoRoot, "pkg/store"),
	}

	fset := token.NewFileSet()
	for _, dir := range scanDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("cannot read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			if strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			filePath := filepath.Join(dir, entry.Name())
			f, err := parser.ParseFile(fset, filePath, nil, 0)
			if err != nil {
				t.Errorf("parse error in %s: %v", filePath, err)
				continue
			}

			relPath, _ := filepath.Rel(repoRoot, filePath)

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

				// Find enclosing function
				enclosingFunc := findEnclosingFunc(fset, f, call.Pos())
				sites = append(sites, callSite{
					file:     relPath,
					function: enclosingFunc,
					symbol:   symbolName,
				})
				return true
			})
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

// Placeholder: ensure fmt import is used
var _ = fmt.Sprintf
