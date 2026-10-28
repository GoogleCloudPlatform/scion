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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createProjectOverHTTP creates a project through POST /api/v1/projects (the
// hub is already running, so the hub-start identity pass has passed) and
// returns the stored project.
func createProjectOverHTTP(t *testing.T, srv *Server, body CreateProjectRequest) store.Project {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	return project
}

func readIdentityFile(t *testing.T, cfgDir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cfgDir, name))
	require.NoError(t, err, name)
	return string(data)
}

// TestCreateProject_PerAgentGit_SharedBackend_SeedsProjectIdentity: a git
// project created AFTER hub start whose create prepares no workspace
// (clone-per-agent, the git default with no workspace mode, and
// worktree-per-agent) gets <ProjectRoot>/.scion/project-id and project-slug
// on a shared backend, so a broker's shared project configuration check finds
// them without waiting for the next hub start. Only the identity files are
// written: no workspace and no settings are seeded for these shapes.
func TestCreateProject_PerAgentGit_SharedBackend_SeedsProjectIdentity(t *testing.T) {
	for name, mode := range map[string]string{
		"clone-per-agent":    "per-agent",
		"git default":        "",
		"worktree-per-agent": "worktree-per-agent",
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkspaceLayoutFixture(t)
			f.useNFS()
			project := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
				Name:          "Per Agent " + name,
				GitRemote:     "github.com/test/per-agent-seed",
				WorkspaceMode: mode,
			})
			require.False(t, project.IsSharedWorkspace(), "fixture check: not a shared workspace")

			projectRoot := filepath.Join(f.shareBase, "projects", project.ID)
			cfgDir := filepath.Join(projectRoot, ".scion")
			assert.Equal(t, project.ID+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile))
			assert.Equal(t, project.Slug+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectSlugFile))
			for _, n := range []string{projectkeys.ProjectIDFile, projectkeys.ProjectSlugFile} {
				info, err := os.Lstat(filepath.Join(cfgDir, n))
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), n)
			}
			info, err := os.Lstat(cfgDir)
			require.NoError(t, err)
			assert.True(t, info.IsDir())
			assert.Zero(t, info.Mode().Perm()&0o022, "the configuration directory is not group/other-writable")

			_, err = os.Stat(filepath.Join(projectRoot, "workspace"))
			assert.True(t, os.IsNotExist(err), "no workspace is prepared for a per-agent git project at create")
			_, err = os.Stat(filepath.Join(cfgDir, "settings.yaml"))
			assert.True(t, os.IsNotExist(err), "only the identity files are seeded for this shape")
			_, err = os.Stat(filepath.Join(f.home, ".scion", "projects", project.Slug))
			assert.True(t, os.IsNotExist(err), "nothing is seeded on the hub's local disk")
		})
	}
}

// TestCreateProject_PerAgentGit_LocalBackend_NoIdentityFiles: on the local
// backend (no shared workspace storage) a per-agent git create seeds nothing.
func TestCreateProject_PerAgentGit_LocalBackend_NoIdentityFiles(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
		Name:          "Local Per Agent",
		GitRemote:     "github.com/test/local-per-agent",
		WorkspaceMode: "per-agent",
	})
	_, err := os.Stat(filepath.Join(f.home, ".scion", "projects", project.Slug))
	assert.True(t, os.IsNotExist(err), "no project directory is created on the local backend")
	_, err = os.Stat(filepath.Join(f.shareBase, "projects"))
	assert.True(t, os.IsNotExist(err), "nothing is written to the (unconfigured) share")
}

// TestCreateProject_SharedBackend_IdentitySeedIdempotent: the per-agent
// seeding helper and the hub-native path agree. A hub-native project seeded
// at create (initHubManagedProject) is unchanged by the per-agent helper and
// by the hub-start pass; a per-agent git project seeded at create is
// unchanged by the hub-start pass.
func TestCreateProject_SharedBackend_IdentitySeedIdempotent(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	native, _ := createTestHubManagedProject(t, f.srv, "Native Seeded")
	perAgent := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
		Name:          "Per Agent Seeded",
		GitRemote:     "github.com/test/per-agent-idem",
		WorkspaceMode: "per-agent",
	})

	nativeDir := filepath.Join(f.shareBase, "projects", native.ID, ".scion")
	perAgentDir := filepath.Join(f.shareBase, "projects", perAgent.ID, ".scion")
	before := map[string]string{}
	for _, d := range []string{nativeDir, perAgentDir} {
		for _, n := range []string{projectkeys.ProjectIDFile, projectkeys.ProjectSlugFile} {
			before[filepath.Join(d, n)] = readIdentityFile(t, d, n)
		}
	}

	f.srv.ensureSharedProjectIdentityBestEffort(native)
	counts := f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Equal(t, 2, counts.ensured)
	assert.Zero(t, counts.mismatched+counts.failed+counts.notReached)
	for path, want := range before {
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want, string(got), path)
	}
	assert.Equal(t, native.ID+"\n", before[filepath.Join(nativeDir, projectkeys.ProjectIDFile)])
	assert.Equal(t, perAgent.ID+"\n", before[filepath.Join(perAgentDir, projectkeys.ProjectIDFile)])
}

// TestCreateProject_PerAgentGit_SharedBackend_MismatchLeftAndCreateSucceeds:
// the per-agent seed keeps the existing mismatch handling. A pre-existing
// project-id naming another project is not overwritten, the mismatch is
// reported by the health check, and the create still succeeds (best-effort).
func TestCreateProject_PerAgentGit_SharedBackend_MismatchLeftAndCreateSucceeds(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	const id = "aaaaaaaa-0000-4000-8000-0000000000e1"
	const other = "bbbbbbbb-0000-4000-8000-0000000000e2"
	cfgDir := filepath.Join(f.shareBase, "projects", id, ".scion")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, projectkeys.ProjectIDFile), []byte(other+"\n"), 0o644))

	project := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
		ID:            id,
		Name:          "Per Agent Mismatch",
		GitRemote:     "github.com/test/per-agent-mismatch",
		WorkspaceMode: "per-agent",
	})
	require.Equal(t, id, project.ID, "fixture check: the client-supplied ID is used")
	assert.Equal(t, other+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile), "a differing project-id is never overwritten")
	_, err := os.Stat(filepath.Join(cfgDir, projectkeys.ProjectSlugFile))
	assert.True(t, os.IsNotExist(err), "project-slug is not written next to another project's project-id")

	checks := map[string]string{}
	f.srv.checkProjectConfigIdentityHealth(checks)
	assert.Contains(t, checks[projectConfigIdentityHealthKey], HealthStatusDegraded)
}
