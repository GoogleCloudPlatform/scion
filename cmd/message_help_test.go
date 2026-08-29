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

package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

// countReferenceKinds parses pkg/messaging/resolve.go and counts the number of
// ValueSpec entries in the const block that defines RefConversation (the
// ReferenceKind enum). This is a source-parsed drift guard: when a new member
// is added to the const block, the count rises and forces the test table to be
// updated in lockstep.
func countReferenceKinds(t *testing.T) int {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../pkg/messaging/resolve.go", nil, 0)
	require.NoError(t, err, "failed to parse resolve.go")

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// Identify the right const block by looking for RefConversation.
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if name.Name == "RefConversation" {
					return len(gen.Specs)
				}
			}
		}
	}
	t.Fatal("could not find ReferenceKind const block containing RefConversation in resolve.go")
	return 0
}

// TestMessageHelpCoversAllRefForms asserts that every conversation-reference
// form accepted by messaging.ParseReference is documented in messageCmd.Long.
// The table entries are validated against the live parser, so they cannot drift
// from the implementation.
func TestMessageHelpCoversAllRefForms(t *testing.T) {
	table := []struct {
		Kind           messaging.ReferenceKind
		CanonicalInput string
		PrefixInHelp   string
	}{
		{messaging.RefAgent, "@test-agent", "@<agent"},
		{messaging.RefEmail, "@user@example.com", "@<email"},
		{messaging.RefConversation, "conv:00000000-0000-0000-0000-000000000000", "conv:<"},
		{messaging.RefThread, "#my-thread", "#<thread"},
	}

	// Source-parsed drift guard: count the members of the ReferenceKind const
	// block in resolve.go and assert it matches the table length. When a new
	// enum member is added, this fails and forces the table (and messageCmd.Long)
	// to be updated.
	kindCount := countReferenceKinds(t)
	require.Equal(t, len(table), kindCount,
		"ReferenceKind const block in resolve.go has %d members but the table has %d — "+
			"add the new kind to the table AND to messageCmd.Long", kindCount, len(table))

	long := messageCmd.Long

	for _, tc := range table {
		t.Run(tc.CanonicalInput, func(t *testing.T) {
			// (a) Validate the canonical input against the live parser.
			ref, err := messaging.ParseReference(tc.CanonicalInput)
			require.NoError(t, err, "ParseReference(%q) should succeed", tc.CanonicalInput)
			assert.Equal(t, tc.Kind, ref.Kind,
				"ParseReference(%q) returned unexpected Kind", tc.CanonicalInput)

			// (b) Assert the help text documents this form.
			assert.True(t, strings.Contains(long, tc.PrefixInHelp),
				"messageCmd.Long must contain %q for reference form %q",
				tc.PrefixInHelp, tc.CanonicalInput)
		})
	}

	// Rule 10: prove the help-text check is load-bearing. A fabricated prefix
	// that does not appear in Long must NOT match.
	t.Run("catches_missing_form", func(t *testing.T) {
		assert.False(t, strings.Contains(long, "xyz:<fake>"),
			"expected Long to NOT contain fabricated prefix xyz:<fake> — assertion mechanism is broken")
	})

	// CHANGE 2: assert that deprecation warnings referencing conversation forms
	// point to forms that are actually documented in Long.
	t.Run("deprecation_warnings_reference_documented_forms", func(t *testing.T) {
		// Patterns that indicate a deprecation message references a
		// conversation-reference form.
		refPatterns := []string{"@<", "conv:", "#<"}
		checked := 0
		for _, d := range deprecationReplacements {
			for _, pat := range refPatterns {
				if strings.Contains(d.Message, pat) {
					checked++
					assert.True(t, strings.Contains(long, pat),
						"deprecation for --%s says %q which references %q, but messageCmd.Long does not contain it",
						d.Flag, d.Message, pat)
				}
			}
		}
		require.Greater(t, checked, 0,
			"expected at least one deprecation replacement to reference a conversation form")
	})
}
