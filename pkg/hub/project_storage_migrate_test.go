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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs/migrate"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storageMigrateFixture is a hub with a fresh HOME and one hub-managed
// project created directly in the store (no filesystem seeding).
type storageMigrateFixture struct {
	srv       *Server
	project   *store.Project
	home      string
	mountRoot string
}

func newStorageMigrateFixture(t *testing.T) storageMigrateFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("K_SERVICE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	srv, s := testServer(t)
	p := &store.Project{ID: uuid.NewString(), Name: "Migrate Me", Slug: "migrate-me"}
	require.NoError(t, s.CreateProject(context.Background(), p))
	return storageMigrateFixture{srv: srv, project: p, home: home, mountRoot: t.TempDir()}
}

func (f storageMigrateFixture) useNFS(t *testing.T) string {
	t.Helper()
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(f.mountRoot))
	share := filepath.Join(f.mountRoot, "share1")
	require.NoError(t, os.MkdirAll(share, 0o755))
	return share
}

func (f storageMigrateFixture) globalDir(t *testing.T) string {
	t.Helper()
	g, err := config.GetGlobalDir()
	require.NoError(t, err)
	return g
}

// legacyHubProjects is the retired hub-managed tree on an nfs share, as the
// migration computes it (the only place its name is spelled).
func legacyHubProjects(t *testing.T, share string) string {
	t.Helper()
	legacy, err := migrate.LegacyHubProjectsRoot(projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: share})
	require.NoError(t, err)
	return legacy.String()
}

// seedSettings is a hub seed settings.yaml naming projectID.
func seedSettings(projectID string) string { return "hub:\n  project_id: " + projectID + "\n" }

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

func runStorageMigrate(t *testing.T, srv *Server, dryRun bool) (string, error) {
	t.Helper()
	exec, err := srv.resolveMaintenanceExecutor(entadapter.ProjectStorageMigrateKey)
	require.NoError(t, err)
	params := map[string]string{}
	if dryRun {
		params["dryRun"] = "true"
	}
	var log bytes.Buffer
	err = exec.Run(context.Background(), &log, params)
	return log.String(), err
}

func TestProjectStorageMigrate_SeededRerunnable(t *testing.T) {
	f := newStorageMigrateFixture(t)
	op, err := f.srv.store.GetMaintenanceOperation(context.Background(), entadapter.ProjectStorageMigrateKey)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceCategoryMigration, op.Category)
	assert.Equal(t, store.MaintenanceStatusPending, op.Status)
	assert.True(t, rerunnableMigrations[entadapter.ProjectStorageMigrateKey], "a second run must be allowed and report moves=0")
	exec, err := f.srv.resolveMaintenanceExecutor(entadapter.ProjectStorageMigrateKey)
	require.NoError(t, err)
	assert.IsType(t, &ProjectStorageMigrateExecutor{}, exec)
}

// Local backend: on the local backend (workstation server) the migration is a no-op.
func TestProjectStorageMigrate_LocalBackendNoOp(t *testing.T) {
	f := newStorageMigrateFixture(t)
	g := f.globalDir(t)
	writeFiles(t, g, map[string]string{"projects/migrate-me/a.txt": "A"})
	for _, dry := range []bool{true, false} {
		log, err := runStorageMigrate(t, f.srv, dry)
		require.NoError(t, err)
		assert.Contains(t, log, "skipped:")
		assert.Contains(t, log, "moves=0")
	}
	got, err := os.ReadFile(filepath.Join(g, "projects/migrate-me/a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "A", string(got))
}

// Shared backend: dry run changes nothing; the run moves the legacy
// hub-managed tree, the seed and a hub-local shared dir into the uuid
// tree; a second run reports 0 moves.
func TestProjectStorageMigrate_NFSMovesAndIsIdempotent(t *testing.T) {
	f := newStorageMigrateFixture(t)
	share := f.useNFS(t)
	g := f.globalDir(t)
	writeFiles(t, legacyHubProjects(t, share), map[string]string{
		"migrate-me/README.md":            "readme",
		"migrate-me/.scion/settings.yaml": seedSettings(f.project.ID),
	})
	writeFiles(t, g, map[string]string{
		filepath.Join("project-configs", projectfs.LocalConfigDirName(f.project.Slug, f.project.ID), "shared-dirs/scratchpad/f.txt"): "scratch",
	})

	log, err := runStorageMigrate(t, f.srv, true)
	require.NoError(t, err, log)
	// The legacy tree holds a .scion, which never lands in the workspace, so
	// the plan is a copy of the rest (merge), not a whole-tree rename; the
	// configuration entries are copied to the project root first.
	assert.Contains(t, log, "step 4-scion-seed would-copy")
	assert.Contains(t, log, "step 1-legacy-hub-workspace would-merge")
	assert.NotContains(t, log, "would-rename")
	_, statErr := os.Stat(filepath.Join(share, "projects", f.project.ID))
	assert.True(t, os.IsNotExist(statErr), "a dry run must not create the project tree")

	log, err = runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	root := filepath.Join(share, "projects", f.project.ID)
	for p, want := range map[string]string{
		"workspace/README.md":          "readme",
		".scion/settings.yaml":         seedSettings(f.project.ID),
		"shared-dirs/scratchpad/f.txt": "scratch",
	} {
		got, err := os.ReadFile(filepath.Join(root, p))
		require.NoError(t, err, "%s\n%s", p, log)
		assert.Equal(t, want, string(got))
	}
	_, statErr = os.Stat(filepath.Join(root, "workspace/.scion"))
	assert.True(t, os.IsNotExist(statErr), "the seed must not land in the workspace")
	// The source is kept beside itself, its .scion included.
	kept, err := filepath.Glob(filepath.Join(legacyHubProjects(t, share), "migrate-me.migrated-*", ".scion", "settings.yaml"))
	require.NoError(t, err)
	assert.Len(t, kept, 1, "the legacy tree must be kept as .migrated-<ts> with its .scion\n%s", log)
	// The moves ran under the project's provisioning lock (the real
	// provision.WithWorkspaceProvisionLocks): provision/ exists with the broker's
	// leaf contract, and the lock was released.
	fi, err := os.Stat(filepath.Join(root, "provision"))
	require.NoError(t, err, log)
	assert.Equal(t, os.FileMode(0o775), fi.Mode().Perm(), "provision/ mode")
	assert.NotZero(t, fi.Mode()&os.ModeSetgid, "provision/ must be setgid")
	// Both provisioning locks (the project root's, taken by the hub-side
	// Cloud Run provisioner, and the state dir's, taken by the Kubernetes
	// init container) were taken and released.
	for _, dir := range provision.WorkspaceProvisionLockDirs(filepath.Join(root, "workspace")) {
		_, statErr = os.Stat(filepath.Join(dir, ".scion-provision.lock"))
		assert.True(t, os.IsNotExist(statErr), "the provisioning lock in %s must be released", dir)
	}
	assert.Contains(t, log, "agent starts for this project waited on its migration")

	log, err = runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	assert.Contains(t, log, "moves=0")
}

// the object-storage cleanup through the hub's object storage: the project's legacy copy goes once
// the migration succeeded; per-agent objects stay.
func TestProjectStorageMigrate_M3DeletesOnlyLegacyCopy(t *testing.T) {
	f := newStorageMigrateFixture(t)
	share := f.useNFS(t)
	writeFiles(t, legacyHubProjects(t, share), map[string]string{"migrate-me/a.txt": "A", "migrate-me/.scion/project-id": f.project.ID})
	blobs, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "hub", LocalPath: t.TempDir()})
	require.NoError(t, err)
	f.srv.SetStorage(blobs)
	ctx := context.Background()
	legacyObj := migrate.LegacyObjectPrefix(f.srv.HubID(), f.project.ID) + "files/a.txt"
	agentObj := "hubs/" + f.srv.HubID() + "/workspaces/" + f.project.ID + "/agent-1/a.txt"
	if f.srv.HubID() == "" {
		agentObj = "workspaces/" + f.project.ID + "/agent-1/a.txt"
	}
	for _, o := range []string{legacyObj, agentObj} {
		_, err := blobs.Upload(ctx, o, strings.NewReader("x"), storage.UploadOptions{})
		require.NoError(t, err)
	}
	log, err := runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	exists, err := blobs.Exists(ctx, legacyObj)
	require.NoError(t, err)
	assert.False(t, exists, "legacy copy must be deleted\n%s", log)
	exists, err = blobs.Exists(ctx, agentObj)
	require.NoError(t, err)
	assert.True(t, exists, "per-agent object must stay")
}

// Fail closed: a volume backend without a volume name is an error, never a
// fallback to local paths.
func TestProjectStorageMigrate_VolumeWithoutNameFailsClosed(t *testing.T) {
	f := newStorageMigrateFixture(t)
	f.srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &config.V1CloudRunVolumeConfig{}}
	_, err := runStorageMigrate(t, f.srv, true)
	require.Error(t, err)
}

func waitForNotice(t *testing.T, srv *Server, p *store.Project) *WorkspaceMigrationNotice {
	t.Helper()
	var n *WorkspaceMigrationNotice
	require.Eventually(t, func() bool {
		n = srv.workspaceMigrationNotice(context.Background(), p)
		return n != nil
	}, 10*time.Second, 20*time.Millisecond)
	return n
}

func TestWorkspaceMigrationNotice(t *testing.T) {
	f := newStorageMigrateFixture(t)
	ctx := context.Background()

	// Local backend: never a notice.
	assert.Nil(t, f.srv.workspaceMigrationNotice(ctx, f.project))

	share := f.useNFS(t)
	writeFiles(t, legacyHubProjects(t, share), map[string]string{"migrate-me/a.txt": "A", "migrate-me/.scion/project-id": f.project.ID})
	other := &store.Project{ID: uuid.NewString(), Name: "Clean", Slug: "clean"}
	require.NoError(t, f.srv.store.CreateProject(ctx, other))

	n := waitForNotice(t, f.srv, f.project)
	assert.Equal(t, entadapter.ProjectStorageMigrateKey, n.OperationKey)
	assert.NotContains(t, n.Message, share, "the notice names no path")
	assert.Nil(t, f.srv.workspaceMigrationNotice(ctx, other), "a project without legacy content gets no notice")

	// The files list carries the notice.
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/workspace/files", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.MigrationNotice)
	assert.Equal(t, entadapter.ProjectStorageMigrateKey, resp.MigrationNotice.OperationKey)

	// A real run invalidates the plan; once the migration is completed the
	// notice is gone.
	log, err := runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	op, err := f.srv.store.GetMaintenanceOperation(ctx, entadapter.ProjectStorageMigrateKey)
	require.NoError(t, err)
	op.Status = store.MaintenanceStatusCompleted
	require.NoError(t, f.srv.store.UpdateMaintenanceOperation(ctx, op))
	assert.Nil(t, f.srv.workspaceMigrationNotice(ctx, f.project))
}

// After a run that leaves nothing behind (op still pending, e.g. the run
// was done but the record reset), the recomputed plan shows no notice.
func TestWorkspaceMigrationNotice_RecomputedAfterRun(t *testing.T) {
	f := newStorageMigrateFixture(t)
	share := f.useNFS(t)
	writeFiles(t, legacyHubProjects(t, share), map[string]string{"migrate-me/a.txt": "A", "migrate-me/.scion/project-id": f.project.ID})
	waitForNotice(t, f.srv, f.project)
	_, err := runStorageMigrate(t, f.srv, false)
	require.NoError(t, err)
	assert.Never(t, func() bool {
		return f.srv.workspaceMigrationNotice(context.Background(), f.project) != nil
	}, 300*time.Millisecond, 20*time.Millisecond)
}

// A hub-local project tree with broker state is only copied from.
func TestProjectStorageMigrate_LocalBrokerTreeUntouched(t *testing.T) {
	f := newStorageMigrateFixture(t)
	share := f.useNFS(t)
	g := f.globalDir(t)
	local := filepath.Join(g, "projects", f.project.Slug)
	writeFiles(t, local, map[string]string{
		".scion/settings.yaml":    seedSettings(f.project.ID),
		".scion/agents/a1/home/x": "agent",
		"notes.txt":               "notes",
	})
	log, err := runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	got, err := os.ReadFile(filepath.Join(share, "projects", f.project.ID, "workspace/notes.txt"))
	require.NoError(t, err, log)
	assert.Equal(t, "notes", string(got))
	for _, p := range []string{".scion/settings.yaml", ".scion/agents/a1/home/x", "notes.txt"} {
		_, err := os.Stat(filepath.Join(local, p))
		assert.NoError(t, err, "the broker's %s must stay", p)
	}
	_, err = os.Stat(filepath.Join(share, "projects", f.project.ID, ".scion/agents"))
	assert.True(t, os.IsNotExist(err), "broker agent state must not be copied")
	log, err = runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	assert.Contains(t, log, "moves=0")
}

// A legacy tree without an identity marker is not migrated by default;
// the notice names the reason and the parameter; adopting the project's
// slug migrates it.
func TestProjectStorageMigrate_UnverifiedTreeAndAdoption(t *testing.T) {
	f := newStorageMigrateFixture(t)
	share := f.useNFS(t)
	writeFiles(t, legacyHubProjects(t, share), map[string]string{"migrate-me/a.txt": "A"})

	n := waitForNotice(t, f.srv, f.project)
	assert.Contains(t, n.Message, "adoptUnverified")

	log, err := runStorageMigrate(t, f.srv, false)
	require.Error(t, err)
	assert.Contains(t, log, "unverified")
	_, statErr := os.Stat(filepath.Join(share, "projects", f.project.ID, "workspace"))
	assert.True(t, os.IsNotExist(statErr), "an unverified tree must not be migrated by default")

	exec, err := f.srv.resolveMaintenanceExecutor(entadapter.ProjectStorageMigrateKey)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, exec.Run(context.Background(), &buf, map[string]string{"adoptUnverified": "other-slug, " + f.project.Slug}), buf.String())
	got, err := os.ReadFile(filepath.Join(share, "projects", f.project.ID, "workspace/a.txt"))
	require.NoError(t, err, buf.String())
	assert.Equal(t, "A", string(got))
}

func TestAdoptUnverifiedParam(t *testing.T) {
	assert.Nil(t, adoptUnverified(""))
	assert.Nil(t, adoptUnverified("false"))
	all := adoptUnverified("true")
	require.NotNil(t, all)
	assert.True(t, all("anything"))
	some := adoptUnverified(" a , b ")
	require.NotNil(t, some)
	assert.True(t, some("a"))
	assert.True(t, some("b"))
	assert.False(t, some("c"))
}

// Step 5 (ptone/scion#5825) through the hub's maintenance operation: a
// stopped agent's legacy Cloud Run home and secrets move to
// agents/<slug>/{home-,secrets-}<agent ID>; a running agent's are deferred
// (the project stays pending) and move once it is stopped.
func TestProjectStorageMigrate_CloudRunHomes(t *testing.T) {
	f := newStorageMigrateFixture(t)
	share := f.useNFS(t)
	ctx := context.Background()
	stopped := &store.Agent{ID: uuid.NewString(), Slug: "dev", Name: "dev", ProjectID: f.project.ID, Phase: "stopped"}
	running := &store.Agent{ID: uuid.NewString(), Slug: "ops", Name: "ops", ProjectID: f.project.ID, Phase: "running"}
	for _, a := range []*store.Agent{stopped, running} {
		require.NoError(t, f.srv.store.CreateAgent(ctx, a))
	}
	root := filepath.Join(share, "projects", f.project.ID)
	for _, a := range []*store.Agent{stopped, running} {
		writeFiles(t, root, map[string]string{
			filepath.Join("agents", a.ID, "home", ".bashrc"):  "home " + a.Slug,
			filepath.Join("agents", a.ID, "secrets", "token"): "secret " + a.Slug,
		})
	}

	log, err := runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	for p, want := range map[string]string{
		filepath.Join("agents", "dev", "home-"+stopped.ID, ".bashrc"):  "home dev",
		filepath.Join("agents", "dev", "secrets-"+stopped.ID, "token"): "secret dev",
		filepath.Join("agents", running.ID, "home", ".bashrc"):         "home ops",
	} {
		got, err := os.ReadFile(filepath.Join(root, p))
		require.NoError(t, err, "%s\n%s", p, log)
		assert.Equal(t, want, string(got))
	}
	assert.Contains(t, log, "step 5-cloudrun-homes deferred")
	_, statErr := os.Stat(filepath.Join(root, "agents", "ops", "home-"+running.ID))
	assert.True(t, os.IsNotExist(statErr), "nothing is created for a running agent")

	running.Phase = "stopped"
	require.NoError(t, f.srv.store.UpdateAgent(ctx, running))
	log, err = runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	got, err := os.ReadFile(filepath.Join(root, "agents", "ops", "home-"+running.ID, ".bashrc"))
	require.NoError(t, err, log)
	assert.Equal(t, "home ops", string(got))

	log, err = runStorageMigrate(t, f.srv, false)
	require.NoError(t, err, log)
	assert.Contains(t, log, "moves=0")
}

// Step 5 moves only agents whose instance is gone: an agent in phase error
// may still have one, and is deferred like a running agent.
func TestStorageMigrateAgentStopped(t *testing.T) {
	for phase, want := range map[string]bool{"stopped": true, "halted": true, "error": false, "running": false, "starting": false, "suspended": false, "": false} {
		assert.Equal(t, want, storageMigrateAgentStopped(phase), phase)
	}
}
