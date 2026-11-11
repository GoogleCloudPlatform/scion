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

package shareddirs

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfineLeaf directly pins the confinement check.
func TestConfineLeaf(t *testing.T) {
	hostBase := "/srv/scion-shared"

	t.Run("path directly under the expected parent passes", func(t *testing.T) {
		hostPath := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs", "scratchpad")
		require.NoError(t, ConfineLeaf(hostPath, hostBase, "projects", "pid-1", "scratchpad"))
	})

	t.Run("wrong subpath_root component fails despite valid name/ID", func(t *testing.T) {
		// "scratchpad" and "pid-1" both pass their own validators; only the
		// resolved parent is wrong (as if Resolve used a different
		// subpath_root than the one ConfineLeaf was told to expect).
		hostPath := filepath.Join(hostBase, "other-root", "pid-1", "shared-dirs", "scratchpad")
		err := ConfineLeaf(hostPath, hostBase, "projects", "pid-1", "scratchpad")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolved outside its project subtree")
	})

	t.Run("wrong project ID component fails despite valid name/ID", func(t *testing.T) {
		hostPath := filepath.Join(hostBase, "projects", "someone-else", "shared-dirs", "scratchpad")
		err := ConfineLeaf(hostPath, hostBase, "projects", "pid-1", "scratchpad")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolved outside its project subtree")
	})

	t.Run("missing the shared-dirs segment fails", func(t *testing.T) {
		hostPath := filepath.Join(hostBase, "projects", "pid-1", "scratchpad")
		err := ConfineLeaf(hostPath, hostBase, "projects", "pid-1", "scratchpad")
		require.Error(t, err)
	})

	// The cases runtime.ValidateNotExportRoot refuses on the agent side.
	// The plugin resolver (runtime.ResolveSharedDirHostPath) does not call
	// it and relies on ConfineLeaf to refuse them.
	for _, tc := range []struct {
		name     string
		hostPath string
	}{
		{"leaf equal to the host base (empty server-relative path)", filepath.Join(hostBase, "")},
		{"leaf equal to the host base with a trailing slash", hostBase + "/"},
		{"leaf above the host base", filepath.Dir(hostBase)},
		{"leaf outside the host base", "/srv/other-export/projects/pid-1/shared-dirs/scratchpad"},
	} {
		t.Run(tc.name+" fails", func(t *testing.T) {
			err := ConfineLeaf(tc.hostPath, hostBase, "projects", "pid-1", "scratchpad")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "resolved outside its project subtree")
		})
	}
}

// TestValidProjectID_RejectsTraversal directly pins the format rules
// ValidProjectID enforces: the allow-list pattern
// `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, which rejects pure-dot names,
// control characters/spaces and unbounded length.
func TestValidProjectID_RejectsTraversal(t *testing.T) {
	tests := []struct {
		id    string
		valid bool
	}{
		{"", false},
		{".", false},
		{"..", false},
		{"...", false}, // pure-dot names
		{"../victim", false},
		{"victim/..", false},
		{"a/b", false},
		{`a\b`, false},
		{"/etc/passwd", false},
		{".hidden", false},                // must start alphanumeric
		{"-leading", false},               // must start alphanumeric
		{"a b", false},                    // space
		{"a\x00b", false},                 // NUL
		{"a\nb", false},                   // control character
		{strings.Repeat("a", 129), false}, // over the 128-char limit
		{"pid-1", true},
		{"550e8400-e29b-41d4-a716-446655440000", true},
		{"a", true},
		{"a.b_c-d", true},
		{strings.Repeat("a", 128), true}, // at the limit
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.id), func(t *testing.T) {
			assert.Equal(t, tc.valid, ValidProjectID(tc.id))
		})
	}
}
