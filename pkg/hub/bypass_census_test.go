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

package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBypassCensus is a regression test that detects new admin bypass patterns
// in pkg/hub/. It maintains an allowlist of authorized bypass locations. As
// handlers are converted from inline admin checks to permission-based checks
// (PR-A2 through PR-A6), entries are removed from the allowlist. Any new
// bypass appearing outside the allowlist causes this test to fail.
//
// The final state (after all D4 conversion PRs) will have only the engine-
// internal keeps plus the deprecated requireAdmin helper and the routeGuard
// fallback.
func TestBypassCensus(t *testing.T) {
	// Patterns that indicate an admin bypass site.
	patterns := []*regexp.Regexp{
		// Category A: inline Role() != "admin" checks in handlers
		regexp.MustCompile(`\.Role\(\)\s*!=\s*"admin"`),
		// Category B: IsUnscopedLocalPlatformAdmin calls (outside authorized engine files)
		regexp.MustCompile(`IsUnscopedLocalPlatformAdmin\(`),
		// Category C: requireAdmin call sites
		regexp.MustCompile(`requireAdmin\(`),
	}

	// Authorized bypass locations. Each entry is "filename:lineContent" where
	// lineContent is a substring that must appear on the matching line. This
	// provides both file-level and line-level specificity.
	//
	// As handlers are converted to permission-based checks, entries are removed
	// from this allowlist. The test fails if a new bypass appears that is not
	// in this list.
	type allowEntry struct {
		file        string // base filename
		lineSubstr  string // substring that must appear on the matching line
		description string // why this is allowed
	}

	allowlist := []allowEntry{
		// ─── Engine-internal keeps (permanent) ───────────────────────────
		// These are part of the authorization engine, not handler-level bypasses.
		{file: "authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "checkAccessForUser admin bypass (KEEP)"},
		{file: "authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "AuthorizeReadBatch short-circuit (KEEP)"},
		{file: "authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "Decide explain trace admin check"},
		{file: "authz_candelegate.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "CanDelegate super-admin bypass (KEEP)"},
		{file: "authz_delegation_ceiling.go", lineSubstr: "IsSystemAdmin", description: "delegation ceiling system admin check (KEEP)"},

		// ─── Authorization infrastructure (permanent or deprecating) ─────
		{file: "authorize.go", lineSubstr: "func (s *Server) requireAdmin(", description: "requireAdmin helper definition (DEPRECATE later)"},
		{file: "authorize.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "requireAdmin implementation"},
		{file: "authorize.go", lineSubstr: "requireAdmin(w, r)", description: "requireAdminHandler wrapper"},
		{file: "route_metadata.go", lineSubstr: "requireAdmin(w, r)", description: "routeGuard fallback for unconverted routes (temporary)"},
		{file: "identity.go", lineSubstr: "IsUnscopedLocalPlatformAdmin", description: "IsUnscopedLocalPlatformAdmin definition"},
		{file: "identity.go", lineSubstr: `user.Role() != "admin"`, description: "IsUnscopedLocalPlatformAdmin implementation"},
		{file: "authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin", description: "comment reference"},

		// ─── Capabilities (will be updated in PR-A7) ─────────────────────
		{file: "capabilities.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "capability computation (PR-A7)"},
		{file: "capabilities.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "capability computation (PR-A7)"},
		{file: "capabilities.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "capability computation (PR-A7)"},

		// ─── Audit explain endpoint (will be updated in PR-A7) ───────────
		{file: "audit_authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "explain endpoint admin check (PR-A7)"},

		// ─── Handler-level bypasses (will be removed in PR-A2 through PR-A6) ──

		// Settings and config (PR-A2) — converted to permission-based checks

		// User management (PR-A3) — converted to permission-based checks

		// Operations (PR-A4) — 10 handler bypass sites converted to permission-based route guards.
		// The AdminModeMiddleware bypass at admin_mode.go:113 remains as infrastructure.
		{file: "admin_mode.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "AdminModeMiddleware admin bypass (infrastructure, KEEP)"},

		// Integrations and hooks (PR-A5)
		{file: "handlers_integrations.go", lineSubstr: `user.Role() != "admin"`, description: "update integration (PR-A5)"},
		{file: "handlers_integrations.go", lineSubstr: `user.Role() != "admin"`, description: "delete integration (PR-A5)"},
		{file: "handlers_lifecycle_hooks.go", lineSubstr: `user.Role() != "admin"`, description: "create lifecycle hook (PR-A5)"},
		{file: "handlers_lifecycle_hooks.go", lineSubstr: `user.Role() != "admin"`, description: "update lifecycle hook (PR-A5)"},
		{file: "handlers_teams_manifest.go", lineSubstr: `user.Role() != "admin"`, description: "teams manifest (PR-A5)"},
		{file: "passthrough_gate.go", lineSubstr: `userIdent.Role() != "admin"`, description: "passthrough gate (PR-A5)"},

		// Resource handlers with existing types (PR-A6)
		{file: "handlers_policies.go", lineSubstr: "requireAdmin(w, r)", description: "create policy (PR-A6)"},
		{file: "handlers_policies.go", lineSubstr: "requireAdmin(w, r)", description: "update policy (PR-A6)"},
		{file: "handlers_policies.go", lineSubstr: "requireAdmin(w, r)", description: "delete policy (PR-A6)"},
		{file: "handlers_policies.go", lineSubstr: "requireAdmin(w, r)", description: "bind policy (PR-A6)"},
		{file: "handlers_policies.go", lineSubstr: "requireAdmin(w, r)", description: "unbind policy (PR-A6)"},
		{file: "handlers_policies.go", lineSubstr: "requireAdmin(w, r)", description: "policy CRUD (PR-A6)"},
		{file: "handlers_policies.go", lineSubstr: `callerUser.Role() != "admin"`, description: "policy evaluate admin check (PR-A6)"},
		{file: "skill_registry_handlers.go", lineSubstr: "requireAdmin(w, r)", description: "skill registry list (PR-A6)"},
		{file: "skill_registry_handlers.go", lineSubstr: "requireAdmin(w, r)", description: "skill registry create (PR-A6)"},
		{file: "skill_registry_handlers.go", lineSubstr: "requireAdmin(w, r)", description: "skill registry update (PR-A6)"},
		{file: "skill_registry_handlers.go", lineSubstr: "requireAdmin(w, r)", description: "skill registry delete (PR-A6)"},
		{file: "skill_registry_handlers.go", lineSubstr: "requireAdmin(w, r)", description: "skill registry sync (PR-A6)"},
		{file: "skill_registry_handlers.go", lineSubstr: "requireAdmin(w, r)", description: "skill registry import (PR-A6)"},
		{file: "skill_registry_handlers.go", lineSubstr: "requireAdmin(w, r)", description: "skill registry CRUD (PR-A6)"},
		{file: "skill_registry_handlers.go", lineSubstr: "requireAdmin(w, r)", description: "skill registry CRUD (PR-A6)"},
		{file: "handlers_gcp_identity.go", lineSubstr: `user.Role() != "admin"`, description: "GCP identity admin check (PR-A6)"},
		{file: "handlers_gcp_identity_scoped.go", lineSubstr: `user.Role() != "admin"`, description: "GCP identity scoped admin check (PR-A6)"},
		{file: "project_clone.go", lineSubstr: `user.Role() != "admin"`, description: "project clone admin check (PR-A6)"},
		{file: "project_template_handlers.go", lineSubstr: `user.Role() != "admin"`, description: "project template admin check (PR-A6)"},
		{file: "handlers_agents_core.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "agent list admin filter (PR-A6)"},
		{file: "handlers_brokers.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "broker list admin filter (PR-A6)"},
		{file: "handlers_groups.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "group list admin filter (PR-A6)"},
		{file: "handlers_groups.go", lineSubstr: "isPlatformAdmin", description: "group membership admin check (PR-A6)"},
		{file: "handlers_projects_core.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "project list admin filter (PR-A6)"},
		{file: "harness_config_handlers.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "harness config list admin filter (PR-A6)"},
		{file: "template_handlers.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "template list admin filter (PR-A6)"},
		{file: "port_forward_handlers.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(userIdent)", description: "port forward admin check (PR-A6)"},
		{file: "handlers_agent_lifecycle.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(userIdent)", description: "agent lifecycle admin check (PR-A6)"},
		{file: "handlers_agent_lifecycle.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(userIdent)", description: "agent lifecycle admin check (PR-A6)"},
		{file: "handlers_skills_injection.go", lineSubstr: "requireAdmin(w, r)", description: "hub injected skills admin check (PR-A6)"},
		{file: "hub_pre_start_hook_handlers.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(identity)", description: "pre-start hook admin check (PR-A6)"},
		{file: "hub_pre_start_hook_handlers.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(identity)", description: "pre-start hook admin check (PR-A6)"},

		// ─── Auth/identity infrastructure (non-bypass references) ────────
		{file: "handlers_auth.go", lineSubstr: "IsUnscopedLocalPlatformAdmin", description: "admin reconciliation comment reference"},
		{file: "handlers_auth.go", lineSubstr: "IsUnscopedLocalPlatformAdmin", description: "admin reconciliation helper"},
		{file: "authz_candelegate.go", lineSubstr: "requireAdmin", description: "comment reference in CanDelegate"},
	}

	// Build the allow map: file -> list of allowed substrings with counts
	type allowKey struct {
		file       string
		lineSubstr string
	}
	allowCounts := map[allowKey]int{}
	for _, entry := range allowlist {
		key := allowKey{file: entry.file, lineSubstr: entry.lineSubstr}
		allowCounts[key]++
	}

	hubDir := "." // pkg/hub is the current package directory in test context
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		t.Fatalf("failed to read hub directory: %v", err)
	}

	var violations []string
	matchCounts := map[allowKey]int{}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(hubDir, name))
		if err != nil {
			t.Fatalf("failed to read %s: %v", name, err)
		}

		lines := strings.Split(string(data), "\n")
		for lineNum, line := range lines {
			for _, pattern := range patterns {
				if !pattern.MatchString(line) {
					continue
				}

				// Check if this match is in the allowlist
				allowed := false
				for _, entry := range allowlist {
					if entry.file == name && strings.Contains(line, entry.lineSubstr) {
						key := allowKey{file: entry.file, lineSubstr: entry.lineSubstr}
						if matchCounts[key] < allowCounts[key] {
							matchCounts[key]++
							allowed = true
							break
						}
					}
				}

				if !allowed {
					violations = append(violations, fmt.Sprintf(
						"%s:%d: %s\n  matched: %s",
						name, lineNum+1, strings.TrimSpace(line), pattern.String(),
					))
				}
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("bypass census: %d unauthorized admin bypass site(s) found.\n"+
			"Each site below uses an inline admin check instead of the permission-based\n"+
			"Decide pipeline. Either convert the handler to use route metadata permissions\n"+
			"(preferred) or add the site to the allowlist in this test with justification.\n\n%s",
			len(violations), strings.Join(violations, "\n\n"))
	}
}
