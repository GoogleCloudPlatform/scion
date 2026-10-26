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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prefixRecorder is object storage that records DeletePrefix calls; any
// other call panics (the storage cleanup makes none).
type prefixRecorder struct {
	storage.Storage
	mu       sync.Mutex
	prefixes []string
}

func (p *prefixRecorder) DeletePrefix(_ context.Context, prefix string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prefixes = append(p.prefixes, prefix)
	return nil
}

func (p *prefixRecorder) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prefixes...)
}

// storageCleanupEnv is a hub with HOME in a temp dir, an nfs project
// storage block on a share at <home>/mnt/share1 (subpath root projects),
// object storage that records prefix deletions, and logs captured.
type storageCleanupEnv struct {
	srv     *Server
	s       store.Store
	home    string
	share   string
	subRoot string
	objects *prefixRecorder
	logs    *syncLogBuffer
}

func newStorageCleanupEnv(t *testing.T) *storageCleanupEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	srv, s := testServer(t)
	mountRoot := filepath.Join(home, "mnt")
	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot:   mountRoot,
			SubPathRoot: "projects",
			Shares:      []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/ws", PVName: "pv"}},
		},
	}
	share := filepath.Join(mountRoot, "share1")
	subRoot := filepath.Join(share, "projects")
	require.NoError(t, os.MkdirAll(subRoot, 0o755))
	objects := &prefixRecorder{}
	srv.SetStorage(objects)
	srv.SetHubID("hub-1")
	logs := &syncLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv.projectsLog = logger
	srv.agentLifecycleLog = logger
	return &storageCleanupEnv{srv: srv, s: s, home: home, share: share, subRoot: subRoot, objects: objects, logs: logs}
}

func (e *storageCleanupEnv) seedTree(t *testing.T, base, projectID string) string {
	t.Helper()
	dir := filepath.Join(base, projectID)
	for _, d := range []string{"workspace/.git", "shared-dirs/scratchpad", "provision", ".scion", "agents/dev/workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace", "README.md"), []byte("x"), 0o644))
	return dir
}

func (e *storageCleanupEnv) auditLines() []string {
	var out []string
	for _, l := range strings.Split(e.logs.String(), "\n") {
		if strings.Contains(l, `msg="`+projectfs.AuditMessage+`"`) {
			out = append(out, l)
		}
	}
	return out
}

// auditLine returns the one audit line naming tree (and containing every
// extra string), failing when there is not exactly one.
func (e *storageCleanupEnv) auditLine(t *testing.T, tree string, extra ...string) string {
	t.Helper()
	var match []string
	for _, l := range e.auditLines() {
		if !strings.Contains(l, "tree="+tree+" ") {
			continue
		}
		ok := true
		for _, x := range extra {
			ok = ok && strings.Contains(l, x)
		}
		if ok {
			match = append(match, l)
		}
	}
	require.Len(t, match, 1, "audit lines for %s %v:\n%s", tree, extra, strings.Join(e.auditLines(), "\n"))
	return match[0]
}

// On a shared backend a project delete removes the project root (and no
// other project's), and the project's object-storage prefix, exactly
// hubs/<hub>/workspaces/<id>/; each is one audit line naming the hub as
// the deleter.
func TestProjectStorageCleanup_RootAndObjectPrefix(t *testing.T) {
	e := newStorageCleanupEnv(t)
	project := &store.Project{ID: tid("psc-root"), Slug: "psc-root"}
	tree := e.seedTree(t, e.subRoot, project.ID)
	sibling := e.seedTree(t, e.subRoot, tid("psc-sibling"))

	e.srv.startProjectStorageCleanup(project)
	e.srv.storageCleanupWG.Wait()

	assertAbsent(t, tree, "the project root is removed")
	assert.DirExists(t, filepath.Join(sibling, "workspace", ".git"))
	assert.Equal(t, []string{"hubs/hub-1/workspaces/" + project.ID + "/"}, e.objects.got())
	l := e.auditLine(t, "project-root")
	for _, s := range []string{"deleter=hub", "outcome=deleted", "project_id=" + project.ID, "level=INFO"} {
		assert.Contains(t, l, s)
	}
	assert.Contains(t, e.auditLine(t, "object-prefix"), "outcome=deleted")
}

// The volume backends (the P2 follow-up) remove the uuid tree too.
func TestProjectStorageCleanup_VolumeBackends(t *testing.T) {
	for _, backend := range []string{"cloudrun-volume", "gke-shared-volume"} {
		t.Run(backend, func(t *testing.T) {
			e := newStorageCleanupEnv(t)
			prev := volumeMountBase
			volumeMountBase = t.TempDir()
			t.Cleanup(func() { volumeMountBase = prev })
			ps := &config.V1ProjectStorageConfig{Backend: backend}
			if backend == "cloudrun-volume" {
				ps.CloudRunVolume = &config.V1CloudRunVolumeConfig{VolumeName: "vol"}
			} else {
				ps.GKESharedVolume = &config.V1GKESharedVolumeConfig{VolumeName: "vol", PVClaimName: "claim"}
			}
			e.srv.config.ProjectStorageConfig = ps
			project := &store.Project{ID: tid("psc-vol-" + backend), Slug: "psc-vol"}
			tree := e.seedTree(t, filepath.Join(volumeMountBase, "vol", "projects"), project.ID)

			e.srv.startProjectStorageCleanup(project)
			e.srv.storageCleanupWG.Wait()

			assertAbsent(t, tree, "the project root is removed on "+backend)
		})
	}
}

// The transitional split (shared dirs on an nfs export while workspaces are
// not) removes the project root from the shared-dir export.
func TestProjectStorageCleanup_LegacySplit(t *testing.T) {
	e := newStorageCleanupEnv(t)
	mountRoot := filepath.Join(e.home, "sd")
	ps, _, err := config.SynthesizeProjectStorage(nil, nil, &config.V1SharedDirStorageConfig{Backend: "nfs",
		NFS: &config.V1NFSConfig{MountRoot: mountRoot, Shares: []config.V1NFSShare{{ID: "scion-shared", PVName: "pv"}}}}, nil)
	require.NoError(t, err)
	e.srv.config.ProjectStorageConfig = ps
	project := &store.Project{ID: tid("psc-split"), Slug: "psc-split"}
	tree := e.seedTree(t, filepath.Join(mountRoot, "scion-shared", "projects"), project.ID)

	e.srv.startProjectStorageCleanup(project)
	e.srv.storageCleanupWG.Wait()

	assertAbsent(t, tree, "the shared-dir export's project root is removed")
}

// The local backend: no tree is removed by this cleanup (its slug-keyed
// trees keep their own deleters); the object-storage prefix still goes.
// Without object storage nothing at all happens.
func TestProjectStorageCleanup_LocalBackend(t *testing.T) {
	e := newStorageCleanupEnv(t)
	e.srv.config.ProjectStorageConfig = nil
	project := &store.Project{ID: tid("psc-local"), Slug: "psc-local"}
	local := filepath.Join(e.home, ".scion", "projects", "psc-local")
	require.NoError(t, os.MkdirAll(local, 0o755))
	nfsTree := e.seedTree(t, e.subRoot, project.ID)

	e.srv.startProjectStorageCleanup(project)
	e.srv.storageCleanupWG.Wait()
	assert.DirExists(t, local)
	assert.DirExists(t, nfsTree)
	assert.Equal(t, []string{"hubs/hub-1/workspaces/" + project.ID + "/"}, e.objects.got())

	e.srv.SetStorage(nil)
	before := len(e.auditLines())
	e.srv.startProjectStorageCleanup(project)
	e.srv.storageCleanupWG.Wait()
	assert.Len(t, e.auditLines(), before, "nothing to do, nothing audited")
}

// A share not mounted on the hub is a failure, audited at ERROR, and the
// mount path is never created; a symlinked project root is refused and not
// retried; its target is untouched.
func TestProjectStorageCleanup_FailuresAudited(t *testing.T) {
	t.Run("share not mounted", func(t *testing.T) {
		e := newStorageCleanupEnv(t)
		absent := filepath.Join(e.home, "absent")
		e.srv.config.ProjectStorageConfig.NFS.MountRoot = absent
		project := &store.Project{ID: tid("psc-unmounted"), Slug: "psc-unmounted"}

		e.srv.startProjectStorageCleanup(project)
		e.srv.storageCleanupWG.Wait()

		l := e.auditLine(t, "project-root")
		assert.Contains(t, l, "level=ERROR")
		assert.Contains(t, l, "outcome=failed")
		_, err := os.Stat(absent)
		assert.True(t, os.IsNotExist(err), "the mount path must not be created")
	})
	t.Run("symlinked root", func(t *testing.T) {
		e := newStorageCleanupEnv(t)
		project := &store.Project{ID: tid("psc-symlink"), Slug: "psc-symlink"}
		victim := e.seedTree(t, t.TempDir(), "victim")
		require.NoError(t, os.Symlink(victim, filepath.Join(e.subRoot, project.ID)))
		seen := setHubAttemptHook(t, nil)

		e.srv.startProjectStorageCleanup(project)
		e.srv.storageCleanupWG.Wait()

		assert.Equal(t, []int{1}, *seen, "a refusal is not retried")
		assert.Contains(t, e.auditLine(t, "project-root"), "outcome=failed")
		assert.FileExists(t, filepath.Join(victim, "workspace", "README.md"))
	})
	t.Run("layout unavailable", func(t *testing.T) {
		e := newStorageCleanupEnv(t)
		e.srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{Backend: "nfs"}
		project := &store.Project{ID: tid("psc-layout"), Slug: "psc-layout"}

		e.srv.startProjectStorageCleanup(project)
		e.srv.storageCleanupWG.Wait()

		assert.Contains(t, e.auditLine(t, "project-root"), "outcome=failed")
	})
}

// A project that exists again (a reused ID) keeps all of its storage.
func TestProjectStorageCleanup_ProjectExistsKeepsEverything(t *testing.T) {
	e := newStorageCleanupEnv(t)
	project := &store.Project{ID: tid("psc-exists"), Slug: "psc-exists", Name: "psc-exists"}
	require.NoError(t, e.s.CreateProject(context.Background(), project))
	tree := e.seedTree(t, e.subRoot, project.ID)

	e.srv.startProjectStorageCleanup(project)
	e.srv.storageCleanupWG.Wait()

	assert.DirExists(t, filepath.Join(tree, "workspace", ".git"))
	assert.Empty(t, e.objects.got())
	assert.Empty(t, e.auditLines())
}

// The share's retired hub-projects/<slug> tree goes when its marker names
// the project, and is kept (audited) when it names none.
func TestProjectStorageCleanup_LegacyHubProjectsTree(t *testing.T) {
	e := newStorageCleanupEnv(t)
	project := &store.Project{ID: tid("psc-legacy"), Slug: "psc-legacy"}
	marked := filepath.Join(e.share, "hub-projects", "psc-legacy")
	require.NoError(t, os.MkdirAll(filepath.Join(marked, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(marked, ".scion", "project-id"), []byte(project.ID), 0o644))
	other := &store.Project{ID: tid("psc-legacy-2"), Slug: "psc-legacy-2"}
	unmarked := filepath.Join(e.share, "hub-projects", "psc-legacy-2")
	require.NoError(t, os.MkdirAll(unmarked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unmarked, "README.md"), []byte("x"), 0o644))

	e.srv.startProjectStorageCleanup(project)
	e.srv.startProjectStorageCleanup(other)
	e.srv.storageCleanupWG.Wait()

	assertAbsent(t, marked, "the marked legacy tree is removed")
	assert.FileExists(t, filepath.Join(unmarked, "README.md"), "a name-only match is kept")
	assert.Contains(t, e.auditLine(t, "legacy-hub-projects", "project_id="+project.ID), "outcome=deleted")
	assert.Contains(t, e.auditLine(t, "legacy-hub-projects", "project_id="+other.ID), "outcome=kept")
}

// An agent's storage cleanup removes agents/<slug> and exactly its object
// prefix, audited with the hub as the deleter.
func TestAgentStorageCleanup_AgentRootAndPrefix(t *testing.T) {
	e := newStorageCleanupEnv(t)
	projectID := tid("asc-project")
	tree := e.seedTree(t, e.subRoot, projectID)
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "agents", "other", "workspace"), 0o755))
	a := &store.Agent{ID: tid("asc-agent"), Slug: "dev", ProjectID: projectID}

	e.srv.startAgentStorageCleanup(a)
	e.srv.storageCleanupWG.Wait()

	assertAbsent(t, filepath.Join(tree, "agents", "dev"), "the agent root is removed")
	assert.DirExists(t, filepath.Join(tree, "agents", "other", "workspace"))
	assert.DirExists(t, filepath.Join(tree, "workspace", ".git"))
	assert.Equal(t, []string{"hubs/hub-1/workspaces/" + projectID + "/" + a.ID + "/"}, e.objects.got())
	l := e.auditLine(t, "agent-root")
	for _, s := range []string{"deleter=hub", "outcome=deleted", "agent=dev"} {
		assert.Contains(t, l, s)
	}
}

// While another agent record of the project with the same slug (live, or
// soft-deleted with retained files) may still use agents/<slug>, only this
// agent's ID-keyed home and secrets go.
func TestAgentStorageCleanup_SlugInUseRemovesOnlyOwnLeaves(t *testing.T) {
	e := newStorageCleanupEnv(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("asc-shared-slug"), Slug: "asc-shared-slug", Name: "asc-shared-slug"}
	require.NoError(t, e.s.CreateProject(ctx, project))
	other := &store.Agent{ID: tid("asc-other"), Slug: "dev", Name: "dev", ProjectID: project.ID}
	require.NoError(t, e.s.CreateAgent(ctx, other))
	a := &store.Agent{ID: tid("asc-self"), Slug: "dev", ProjectID: project.ID}
	agentDir := filepath.Join(e.seedTree(t, e.subRoot, project.ID), "agents", "dev")
	for _, d := range []string{"home-" + a.ID, "secrets-" + a.ID, "home-" + other.ID} {
		require.NoError(t, os.MkdirAll(filepath.Join(agentDir, d), 0o755))
	}

	e.srv.startAgentStorageCleanup(a)
	e.srv.storageCleanupWG.Wait()

	assertAbsent(t, filepath.Join(agentDir, "home-"+a.ID), "own home removed")
	assertAbsent(t, filepath.Join(agentDir, "secrets-"+a.ID), "own secrets removed")
	assert.DirExists(t, filepath.Join(agentDir, "home-"+other.ID))
	assert.DirExists(t, filepath.Join(agentDir, "workspace"))
	assert.Contains(t, e.auditLine(t, "agent-home"), "outcome=deleted")
}

// The agent's storage goes on a hard delete that deletes its files only;
// a soft delete, or one that keeps the files, keeps it.
func TestAgentStorageOnDelete(t *testing.T) {
	cases := []struct {
		req  store.DeletionRequestInfo
		want bool
	}{
		{store.DeletionRequestInfo{DeleteFiles: true}, true},
		{store.DeletionRequestInfo{DeleteFiles: true, Force: true}, true},
		{store.DeletionRequestInfo{DeleteFiles: false}, false},
		{store.DeletionRequestInfo{Soft: true, DeleteFiles: true}, false},
		{store.DeletionRequestInfo{Soft: true}, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, agentStorageOnDelete(tc.req), "%+v", tc.req)
	}
}
