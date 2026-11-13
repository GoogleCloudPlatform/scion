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

//go:build unix

package runtime

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloudRunHomeReset_StartClearsOnce(t *testing.T) {
	rt, cfg, leaf := crHomeRun(t, &fakeInstancesClient{getErr: notFoundErr()})
	require.NoError(t, os.MkdirAll(filepath.Join(leaf, ".old-harness"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, ".old-harness", "session"), []byte("old"), 0o600))
	outside := filepath.Join(t.TempDir(), "kept")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(filepath.Dir(outside), filepath.Join(leaf, "old-link")))
	cfg.HomeResetID = "reset-1"
	// A broker-home marker must never overwrite the runtime's marker.
	require.NoError(t, os.WriteFile(filepath.Join(cfg.HomeDir, ".scion-home-reset-id"), []byte("forged"), 0o600))
	_, err := rt.Run(context.Background(), cfg)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(leaf, ".old-harness", "session"))
	_, err = os.Lstat(filepath.Join(leaf, "old-link"))
	assert.True(t, os.IsNotExist(err))
	contents, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "outside", string(contents))
	marker, err := os.ReadFile(filepath.Join(leaf, ".scion-home-reset-id"))
	require.NoError(t, err)
	assert.Equal(t, cfg.HomeResetID, string(marker))
	assert.FileExists(t, filepath.Join(leaf, ".bashrc"))

	session := filepath.Join(leaf, "new-session")
	require.NoError(t, os.WriteFile(session, []byte("new"), 0o600))
	_, err = rt.Run(context.Background(), cfg)
	require.NoError(t, err)
	assert.FileExists(t, session, "matching reset ID keeps new-harness state")

	cfg.HomeResetID = "reset-2"
	_, err = rt.Run(context.Background(), cfg)
	require.NoError(t, err)
	assert.NoFileExists(t, session, "a later reset clears again")
}

func TestCloudRunHomeReset_NoRecordKeepsHome(t *testing.T) {
	rt, cfg, leaf := crHomeRun(t, &fakeInstancesClient{getErr: notFoundErr()})
	require.NoError(t, os.MkdirAll(leaf, 0o700))
	session := filepath.Join(leaf, "session")
	require.NoError(t, os.WriteFile(session, []byte("kept"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.HomeDir, ".scion-home-reset-id"), []byte("forged"), 0o600))
	_, err := rt.Run(context.Background(), cfg)
	require.NoError(t, err)
	assert.FileExists(t, session)
	assert.NoFileExists(t, filepath.Join(leaf, ".scion-home-reset-id"), "reserved marker is excluded even without a pending reset")
}

func TestCloudRunHomeReset_FailedSeedRetries(t *testing.T) {
	rt, cfg, leaf := crHomeRun(t, &fakeInstancesClient{getErr: notFoundErr()})
	require.NoError(t, os.MkdirAll(leaf, 0o700))
	// Keep the leaf across failed-start cleanup (it predates this start).
	cfg.HomeResetID = "reset-1"
	markerPath := filepath.Join(leaf, ".scion-home-reset-id")
	require.NoError(t, os.WriteFile(markerPath, []byte("previous"), 0o600))
	old := filepath.Join(leaf, "old-session")
	require.NoError(t, os.WriteFile(old, []byte("old"), 0o600))
	huge := filepath.Join(cfg.HomeDir, "huge")
	f, err := os.Create(huge)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(cloudRunHomeMaxBytes+1))
	require.NoError(t, f.Close())
	_, err = rt.Run(context.Background(), cfg)
	require.Error(t, err)
	assert.NoFileExists(t, markerPath, "a failed seed never commits the reset")
	assert.NoFileExists(t, old)
	require.NoError(t, os.Remove(huge))
	// State left by a partial seed must be cleared on retry too.
	partial := filepath.Join(leaf, "partial-session")
	require.NoError(t, os.WriteFile(partial, []byte("partial"), 0o600))
	_, err = rt.Run(context.Background(), cfg)
	require.NoError(t, err)
	assert.NoFileExists(t, partial)
	marker, err := os.ReadFile(markerPath)
	require.NoError(t, err)
	assert.Equal(t, cfg.HomeResetID, string(marker))
}

// The fake fails at the filesystem boundary after an earlier entry was
// actually removed. The retry uses the same leaf with real no-follow IO.
type interruptedCloudRunResetLeaf struct {
	*agenthome.Home
	failName string
}

func (l interruptedCloudRunResetLeaf) RemoveAll(rel string) error {
	if rel == l.failName {
		return fs.ErrPermission
	}
	return l.Home.RemoveAll(rel)
}

func TestCloudRunHomeReset_InterruptedClearRetries(t *testing.T) {
	dir := t.TempDir()
	leaf, err := agenthome.Open(dir)
	require.NoError(t, err)
	defer func() { _ = leaf.Close() }()
	for _, name := range []string{"a", "b", ".scion-home-reset-id"} {
		require.NoError(t, leaf.WriteFile(name, []byte("old"), 0o600))
	}
	_, err = clearCloudRunHomeForReset(interruptedCloudRunResetLeaf{leaf, "b"}, "reset-1")
	require.ErrorIs(t, err, fs.ErrPermission)
	_, err = leaf.ReadFile("a", 64)
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = leaf.ReadFile("b", 64)
	require.NoError(t, err)
	_, err = leaf.ReadFile(".scion-home-reset-id", 64)
	require.ErrorIs(t, err, fs.ErrNotExist)
	reset, err := clearCloudRunHomeForReset(leaf, "reset-1")
	require.NoError(t, err)
	assert.True(t, reset)
	entries, err := leaf.ReadDir(".")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// Editing or replacing agent-owned metadata may cause one extra reset,
// but a corrupt marker must not make the home permanently unstartable.
func TestCloudRunHomeReset_InvalidMarkerClearsWithoutFollowing(t *testing.T) {
	for _, kind := range []string{"link", "hardlink", "directory", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			leaf, err := agenthome.Open(dir)
			require.NoError(t, err)
			defer func() { _ = leaf.Close() }()
			outside := filepath.Join(t.TempDir(), "kept")
			require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
			marker := filepath.Join(dir, ".scion-home-reset-id")
			switch kind {
			case "link":
				require.NoError(t, os.Symlink(outside, marker))
			case "hardlink":
				require.NoError(t, os.Link(outside, marker))
			case "directory":
				require.NoError(t, os.Mkdir(marker, 0o700))
			case "oversize":
				require.NoError(t, os.WriteFile(marker, []byte(strings.Repeat("x", 65)), 0o600))
			}
			reset, err := clearCloudRunHomeForReset(leaf, "reset-1")
			require.NoError(t, err)
			assert.True(t, reset)
			entries, err := leaf.ReadDir(".")
			require.NoError(t, err)
			assert.Empty(t, entries)
			contents, err := os.ReadFile(outside)
			require.NoError(t, err)
			assert.Equal(t, "outside", string(contents))
		})
	}
}
