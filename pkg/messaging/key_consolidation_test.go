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

package messaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAC_DEF15_1_KeyDerivationConsolidation reads the Go source files in
// pkg/messaging/ and asserts that the legacy key-construction patterns are
// confined to their expected files.
//
// This test prevents regressions where a new call site constructs a thread:
// or DM key directly instead of going through DeriveConversationKey.
//
// Prior art: cmd/doc_syntax_test.go reads source files and validates content.
func TestAC_DEF15_1_KeyDerivationConsolidation(t *testing.T) {
	// Read all non-test .go files in this package directory.
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var threadKeyFiles []string
	var legacyDMRefFiles []string

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		// Skip test files — we only care about production code.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(".", name))
		require.NoError(t, err)

		content := string(data)

		// Check for fmt.Sprintf("thread:%s:%s" — the thread key construction pattern.
		if strings.Contains(content, `fmt.Sprintf("thread:%s:%s"`) {
			threadKeyFiles = append(threadKeyFiles, name)
		}

		// Check for directMessageExternalRef (now unexported).
		// Count production definitions (not just references in comments).
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			// Skip comments.
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "directMessageExternalRef") {
				legacyDMRefFiles = append(legacyDMRefFiles, name)
				break // one match per file is enough
			}
		}
	}

	// AC-DEF15-1: fmt.Sprintf("thread:%s:%s" must appear in exactly ONE
	// non-test .go file (derive_key.go).
	assert.Equal(t, []string{"derive_key.go"}, threadKeyFiles,
		"AC-DEF15-1: thread key construction must be confined to derive_key.go")

	// AC-DEF15-1: directMessageExternalRef (now unexported) must appear in
	// exactly ONE non-test .go file (divergence.go).
	assert.Equal(t, []string{"divergence.go"}, legacyDMRefFiles,
		"AC-DEF15-1: directMessageExternalRef must be confined to divergence.go")
}
