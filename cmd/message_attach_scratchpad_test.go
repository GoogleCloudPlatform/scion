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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withScratchpadTargets points the scratchpad probe at targets for one test.
func withScratchpadTargets(t *testing.T, targets ...string) {
	t.Helper()
	prev := scratchpadTargets
	scratchpadTargets = func() []string { return targets }
	t.Cleanup(func() { scratchpadTargets = prev })
}

// TestScratchpadTargets_FromProjectfs: the CLI probes exactly the container
// targets the layout authority computes, /scion-volumes form first
// (ptone/scion#5830: no container-path literal in the CLI).
func TestScratchpadTargets_FromProjectfs(t *testing.T) {
	assert.Equal(t, projectfs.SharedDirTargets(projectfs.ScratchpadName, ""), scratchpadTargets())
	assert.Equal(t, []string{"/scion-volumes/scratchpad", "/workspace/.scion-volumes/scratchpad"}, scratchpadTargets())
}

// TestFindScratchpad_ProbesInOrder: the first target that is a directory
// wins; a missing first target falls through to the in-workspace form, and a
// regular file is not a scratchpad.
func TestFindScratchpad_ProbesInOrder(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "scion-volumes", "scratchpad")
	second := filepath.Join(base, "workspace", ".scion-volumes", "scratchpad")

	withScratchpadTargets(t, first, second)
	assert.Equal(t, "", findScratchpad(), "no target exists")

	require.NoError(t, os.MkdirAll(filepath.Dir(first), 0o755))
	require.NoError(t, os.WriteFile(first, []byte("not a dir"), 0o644))
	require.NoError(t, os.MkdirAll(second, 0o755))
	assert.Equal(t, second, findScratchpad(), "a file at the first target falls through to the in-workspace form")

	require.NoError(t, os.Remove(first))
	require.NoError(t, os.MkdirAll(first, 0o755))
	assert.Equal(t, first, findScratchpad(), "the /scion-volumes form is preferred")
}

// TestStageAttachments_NoScratchpadNamesTargets: with no scratchpad, the
// error names the probed targets.
func TestStageAttachments_NoScratchpadNamesTargets(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "a")
	second := filepath.Join(base, "b")
	withScratchpadTargets(t, first, second)

	_, err := stageAttachments([]string{"/workspace/some/file.go"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scratchpad volume not available at "+first+" or "+second)
}

// TestStageAttachments_StageDirFromProjectfs: the per-message staging dir is
// <scratchpad>/.attachments/<agent slug>/<message id>
// (projectfs.AttachmentRelDir), "_user" for a sender that is not an agent,
// and an agent name that is not one path component is refused.
func TestStageAttachments_StageDirFromProjectfs(t *testing.T) {
	scratchpad := t.TempDir()
	withScratchpadTargets(t, scratchpad)

	for _, tc := range []struct{ env, channel string }{
		{"test-agent", "test-agent"},
		{"", projectfs.AttachmentChannelUser},
	} {
		t.Setenv("SCION_AGENT_NAME", tc.env)
		// A path outside the allowed roots is skipped, so nothing is copied,
		// but the staging dir is still created where the authority says.
		staged, err := stageAttachments([]string{"/etc/hostname"})
		require.NoError(t, err)
		assert.Empty(t, staged)

		entries, err := os.ReadDir(filepath.Join(scratchpad, projectfs.AttachmentsDir, tc.channel))
		require.NoError(t, err)
		require.Len(t, entries, 1, "one per-message dir under channel %q", tc.channel)
		assert.True(t, entries[0].IsDir())
	}

	t.Setenv("SCION_AGENT_NAME", "../escape")
	_, err := stageAttachments([]string{"/etc/hostname"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "attachment staging directory"), err.Error())
	_, statErr := os.Stat(filepath.Join(scratchpad, "escape"))
	assert.True(t, os.IsNotExist(statErr))
}
