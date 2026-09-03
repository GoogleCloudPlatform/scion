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

// TestConsistencyCheckReturnConsumed is a structural regression guard for
// DEF-139. The entire defect was that CheckConversationConsistency's return
// value was discarded at every call site, making the independent consistency
// check invisible. This test fails if any call site in non-test Go source
// discards the return (bare statement or `_ =` assignment), and also fails
// if a new call site appears without being accounted for.
//
// The test scans source text rather than go/ast because the patterns it
// needs to detect — bare function calls and `_ =` assignments — are
// simple enough that regex is clearer and cheaper than an AST walk.
func TestConsistencyCheckReturnConsumed(t *testing.T) {
	// Patterns that indicate the return value is discarded.
	bareCall := regexp.MustCompile(
		`^\s*messaging\.CheckConversationConsistency\(`,
	)
	discardAssign := regexp.MustCompile(
		`^\s*_\s*[:=]+\s*messaging\.CheckConversationConsistency\(`,
	)

	// Pattern that matches any call to CheckConversationConsistency.
	anyCall := regexp.MustCompile(
		`messaging\.CheckConversationConsistency\(`,
	)

	hubDir := "."
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		t.Fatalf("reading hub dir: %v", err)
	}

	var violations []string
	totalCallSites := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(hubDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !anyCall.MatchString(line) {
				continue
			}
			// Skip comment lines.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
				continue
			}

			totalCallSites++

			if bareCall.MatchString(line) {
				violations = append(violations,
					formatViolation(name, i+1, line, "bare call (return value discarded)"))
			}
			if discardAssign.MatchString(line) {
				violations = append(violations,
					formatViolation(name, i+1, line, "assigned to _ (return value discarded)"))
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("DEF-139 regression: CheckConversationConsistency return value "+
			"must be consumed at every call site.\n\nViolations:\n%s",
			strings.Join(violations, "\n"))
	}

	// Assert the expected count of call sites so that a NEW unguarded site
	// also trips this test — the developer must update this count and
	// verify the new site consumes the return.
	const expectedCallSites = 7
	if totalCallSites != expectedCallSites {
		t.Errorf("expected %d CheckConversationConsistency call sites in non-test "+
			"pkg/hub sources, found %d. If you added or removed a call site, "+
			"update expectedCallSites in this test after verifying each site "+
			"consumes the return value.", expectedCallSites, totalCallSites)
	}
}

func formatViolation(file string, lineNum int, content, reason string) string {
	return fmt.Sprintf("  %s:%d: %s\n    %s", file, lineNum, reason, strings.TrimSpace(content))
}
