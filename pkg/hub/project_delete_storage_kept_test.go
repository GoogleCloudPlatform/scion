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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireProjectDeleteResponse checks a successful project delete: either
// 204 with no body, or (the Hub cannot see the shared project storage) 200
// whose body is exactly the one fixed storageKept item. Tests whose subject
// is not storage visibility use it; the visibility tests below pin which.
func requireProjectDeleteResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	switch rec.Code {
	case http.StatusNoContent:
		assert.Empty(t, rec.Body.String())
	case http.StatusOK:
		requireSharedStorageKeptBody(t, rec)
	default:
		require.Failf(t, "unexpected project delete status", "status %d, body: %s", rec.Code, rec.Body.String())
	}
}

// requireSharedStorageKeptBody checks a 200 project delete body: exactly
// one storageKept item, with the fixed code and the fixed message.
func requireSharedStorageKeptBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got projectDeleteResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "body: %s", rec.Body.String())
	require.Len(t, got.StorageKept, 1, "body: %s", rec.Body.String())
	assert.Equal(t, "shared-project-storage", got.StorageKept[0].Code)
	assert.Equal(t, "The Hub cannot see the shared project storage, so the deleted project's shared tree was kept. Hub health reports the shared project storage as not ready on nfs and as degraded on the other shared backends.", got.StorageKept[0].Message)
}

// When the Hub cannot see the shared project storage (the share is not
// mounted on the Hub), the delete still commits, but it does not answer an
// unqualified 204: it answers 200 with the fixed, path-free storageKept
// item.
func TestDeleteProject_SharedStorageNotVisible_ReportsKept(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "kept-unmounted")
	absent := filepath.Join(e.home, "absent")
	e.srv.config.ProjectStorageConfig.NFS.MountRoot = absent
	logs := captureProjectsLog(t, e.srv)

	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+e.project.ID, nil)

	requireSharedStorageKeptBody(t, rec)
	body := rec.Body.String()
	for _, leak := range []string{absent, e.home, "share1", "10.0.0.2", "/ws", "projects/", e.project.ID, e.project.Slug} {
		assert.NotContains(t, body, leak, "the response must not carry %q", leak)
	}
	e.srv.storageCleanupWG.Wait()
	_, err := e.s.GetProject(context.Background(), e.project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the delete itself commits")

	// The kept tree is one ERROR audit line on the storage-deletion channel,
	// naming the project ID and no path.
	var keptLines []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, `msg="project storage deletion"`) && strings.Contains(l, "outcome=kept") && strings.Contains(l, "Hub cannot see") {
			keptLines = append(keptLines, l)
		}
	}
	require.Len(t, keptLines, 1, "logs:\n%s", logs.String())
	assert.Contains(t, keptLines[0], "level=ERROR")
	assert.Contains(t, keptLines[0], "project_id="+e.project.ID)
	assert.Contains(t, keptLines[0], "deleter=hub")
	assert.NotContains(t, keptLines[0], "path=")
	assert.NotContains(t, keptLines[0], absent)
}

// With the shared project storage visible, the delete answers 204 with no
// body, as before.
func TestDeleteProject_SharedStorageVisible_NoContent(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "kept-mounted")
	e.seedTree(t, e.project.ID)

	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+e.project.ID, nil)

	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	assert.Empty(t, rec.Body.String())
	e.srv.storageCleanupWG.Wait()
}

// A hung visibility probe (a stale or hung mount) must not block the delete
// handler: after the short deadline it counts as not visible, and the delete
// answers 200 with the storageKept item.
func TestDeleteProject_SharedStorageProbeHangs_ReportsKept(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "kept-hung")
	release := make(chan struct{})
	prevProbe, prevTimeout := projectStorageTreeProbe, projectDeleteStorageCheckTimeout
	projectStorageTreeProbe = func(projectfs.Layout) error { <-release; return nil }
	projectDeleteStorageCheckTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		projectStorageTreeProbe, projectDeleteStorageCheckTimeout = prevProbe, prevTimeout
	})

	start := time.Now()
	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+e.project.ID, nil)
	elapsed := time.Since(start)

	requireSharedStorageKeptBody(t, rec)
	assert.Less(t, elapsed, 10*time.Second, "the delete must not wait for the hung probe")
	e.srv.storageCleanupWG.Wait()
}
