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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The operator behaviour of the removed runtime.CleanupNFSProject, kept on
// the Hub's removal path: a refused layout (here the WHOLE subpath root is a
// symlink to another directory on the share holding the same project ID) is
// logged as a failed project-root deletion and left in place for manual
// removal. It is not retried, nothing behind the link is removed, and the
// link itself is kept.
func TestProjectStorageCleanup_RefusedLayoutLoggedAndLeft(t *testing.T) {
	e := newStorageCleanupEnv(t)
	project := &store.Project{ID: tid("psc-refused-layout"), Slug: "psc-refused-layout"}
	require.NoError(t, os.RemoveAll(e.subRoot))
	other := filepath.Join(e.share, "other")
	keep := filepath.Join(other, project.ID, "workspace", "keep.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(keep), 0o755))
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o644))
	require.NoError(t, os.Symlink(other, e.subRoot))
	seen := setHubAttemptHook(t, nil)

	e.srv.startProjectStorageCleanup(project, time.Time{})
	e.srv.storageCleanupWG.Wait()

	assert.Equal(t, []int{1}, *seen, "a refusal is not retried")
	l := e.auditLine(t, "project-root")
	assert.Contains(t, l, "outcome=failed")
	assert.Contains(t, l, "refused", "the audit line must say the removal was refused")
	assert.FileExists(t, keep, "the tree behind the symlink must be left for manual removal")
	fi, err := os.Lstat(e.subRoot)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the symlinked subpath root must be left in place")
}
