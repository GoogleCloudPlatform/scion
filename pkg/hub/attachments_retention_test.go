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

//go:build !no_sqlite

package hub

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// TestSweepProjectionsIn_AgesSecondLevelEntries pins how the projection
// sweep ages entries: entries are aged by the second-level directory
// (.attachments/<channel>/<entry>); nested per-conversation layouts are
// aged as one entry, so a file added to such an entry keeps every older
// file in it.
func TestSweepProjectionsIn_AgesSecondLevelEntries(t *testing.T) {
	old := 10 * 24 * time.Hour
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	t.Run("nested entry refreshed by a new file is kept", func(t *testing.T) {
		sp := t.TempDir()
		space := filepath.Join(sp, ".attachments", "_gchat", "spaces_X")
		require.NoError(t, os.MkdirAll(space, 0o755))
		a := filepath.Join(space, "A")
		require.NoError(t, os.WriteFile(a, []byte("a"), 0o644))
		ageDir(t, a, old)
		ageDir(t, space, old)
		// A new file refreshes the entry's modification time.
		require.NoError(t, os.WriteFile(filepath.Join(space, "B"), []byte("b"), 0o644))

		n, err := sweepProjectionsIn(projectfs.Resolved{Backend: projectfs.BackendLocal, HostPath: sp}, cutoff)
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.FileExists(t, a)
	})

	t.Run("nested entry with only old content is removed", func(t *testing.T) {
		sp := t.TempDir()
		space := filepath.Join(sp, ".attachments", "_gchat", "spaces_X")
		require.NoError(t, os.MkdirAll(space, 0o755))
		a := filepath.Join(space, "A")
		require.NoError(t, os.WriteFile(a, []byte("a"), 0o644))
		ageDir(t, a, old)
		ageDir(t, space, old)

		n, err := sweepProjectionsIn(projectfs.Resolved{Backend: projectfs.BackendLocal, HostPath: sp}, cutoff)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.NoDirExists(t, space)
	})

	t.Run("one directory per message ages per message", func(t *testing.T) {
		sp := t.TempDir()
		ch := filepath.Join(sp, ".attachments", "_telegram")
		oldMsg := filepath.Join(ch, "tg_1")
		newMsg := filepath.Join(ch, "tg_2")
		for _, d := range []string{oldMsg, newMsg} {
			require.NoError(t, os.MkdirAll(d, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(d, "photo.png"), []byte("p"), 0o644))
		}
		ageDir(t, oldMsg, old)

		n, err := sweepProjectionsIn(projectfs.Resolved{Backend: projectfs.BackendLocal, HostPath: sp}, cutoff)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.NoDirExists(t, oldMsg)
		assert.DirExists(t, newMsg)
	})
}

// TestSweepLegacyAttachmentBlobs_LocalArtifactsKeepReferencedBlobs: on a hub
// whose attachment artifacts use local blob storage (on by default there),
// a legacy blob that a sent message references is kept however old, and
// still downloads from history; an old orphan upload is swept as before.
func TestSweepLegacyAttachmentBlobs_LocalArtifactsKeepReferencedBlobs(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	f.srv.SetHubID("local-hub")
	installLocalArtifactService(t, f.srv)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)

	// Uploaded and sent while attachment artifacts were off.
	storeAttachmentArtifactsOverride(t, f.srv, false)
	sent := f.uploadAs(t, f.ua, "", "history.txt", "history bytes")
	f.sendWith(t, f.ua, dm, sent)
	orphan := f.uploadAs(t, f.ua, "", "orphan.txt", "never sent")
	for _, id := range []string{sent, orphan} {
		require.DirExists(t, filepath.Join(f.as.baseDir, id), "stored in the legacy store")
		ageDir(t, filepath.Join(f.as.baseDir, id), 400*time.Hour)
	}

	// Then the hub resolves attachment artifacts on by default.
	f.srv.SetOperationalSettings(artifactsOps(t, ""))
	require.True(t, f.srv.experimentEnabled(experiments.AttachmentArtifacts))
	require.False(t, f.srv.objectStorageConfigured())

	n := f.srv.sweepLegacyAttachmentBlobs(ctx, f.srv.legacyBlobRetentionCutoff(time.Now()))
	assert.Equal(t, 1, n, "only the orphan is swept")
	assert.NoDirExists(t, filepath.Join(f.as.baseDir, orphan))
	assert.DirExists(t, filepath.Join(f.as.baseDir, sent))
	got := f.download(t, f.uc, sent)
	require.Equal(t, 200, got.status, got.body)
	assert.Equal(t, "history bytes", got.body)
}
