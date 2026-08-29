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
		// These are engine-internal keeps — permanent or deprecating-with-replacement.

		// ─── Engine-internal keeps (permanent) ───────────────────────────
		// These are part of the authorization engine, not handler-level bypasses.
		{file: "authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "checkAccessForUser admin bypass (KEEP)"},
		{file: "authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "AuthorizeReadBatch short-circuit (KEEP)"},
		{file: "authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "Decide explain trace admin check"},
		{file: "authz_candelegate.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "CanDelegate super-admin bypass (KEEP)"},
		{file: "capabilities.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "checkAccessPrecomputed admin bypass (KEEP — mirrors checkAccessForUser step 1)"},
		{file: "authz_delegation_ceiling.go", lineSubstr: "IsSystemAdmin", description: "delegation ceiling system admin check (KEEP)"},

		// ─── Authorization infrastructure (permanent or deprecating) ─────
		{file: "authorize.go", lineSubstr: "func (s *Server) requireAdmin(", description: "requireAdmin helper definition (DEPRECATED — fallback only)"},
		{file: "authorize.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "requireAdmin implementation"},
		{file: "authorize.go", lineSubstr: "requireAdmin(w, r)", description: "requireAdminHandler wrapper"},
		{file: "route_metadata.go", lineSubstr: "requireAdmin(w, r)", description: "routeGuard fallback for unconverted routes (temporary)"},
		{file: "identity.go", lineSubstr: "IsUnscopedLocalPlatformAdmin", description: "IsUnscopedLocalPlatformAdmin definition"},
		{file: "identity.go", lineSubstr: `user.Role() != "admin"`, description: "IsUnscopedLocalPlatformAdmin implementation"},
		{file: "authz.go", lineSubstr: "IsUnscopedLocalPlatformAdmin", description: "comment reference"},

		// ─── AdminModeMiddleware bypass (infrastructure, KEEP) ───────────
		{file: "admin_mode.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "AdminModeMiddleware admin bypass (infrastructure, KEEP)"},

		// ─── Messaging authorization engine (permanent, D6) ─────────────
		{file: "authorize_message.go", lineSubstr: "IsUnscopedLocalPlatformAdmin(user)", description: "authorizeAgentMessage super-admin bypass (D6, KEEP)"},

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
