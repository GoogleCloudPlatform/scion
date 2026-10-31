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
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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

	e.srv.startProjectStorageCleanup(project, time.Time{})
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

			e.srv.startProjectStorageCleanup(project, time.Time{})
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

	e.srv.startProjectStorageCleanup(project, time.Time{})
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

	e.srv.startProjectStorageCleanup(project, time.Time{})
	e.srv.storageCleanupWG.Wait()
	assert.DirExists(t, local)
	assert.DirExists(t, nfsTree)
	assert.Equal(t, []string{"hubs/hub-1/workspaces/" + project.ID + "/"}, e.objects.got())

	e.srv.SetStorage(nil)
	before := len(e.auditLines())
	e.srv.startProjectStorageCleanup(project, time.Time{})
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

		e.srv.startProjectStorageCleanup(project, time.Time{})
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

		e.srv.startProjectStorageCleanup(project, time.Time{})
		e.srv.storageCleanupWG.Wait()

		assert.Equal(t, []int{1}, *seen, "a refusal is not retried")
		assert.Contains(t, e.auditLine(t, "project-root"), "outcome=failed")
		assert.FileExists(t, filepath.Join(victim, "workspace", "README.md"))
	})
	t.Run("layout unavailable", func(t *testing.T) {
		e := newStorageCleanupEnv(t)
		e.srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{Backend: "nfs"}
		project := &store.Project{ID: tid("psc-layout"), Slug: "psc-layout"}

		e.srv.startProjectStorageCleanup(project, time.Time{})
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

	e.srv.startProjectStorageCleanup(project, time.Time{})
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

	e.srv.startProjectStorageCleanup(project, time.Time{})
	e.srv.startProjectStorageCleanup(other, time.Time{})
	e.srv.storageCleanupWG.Wait()

	assertAbsent(t, marked, "the marked legacy tree is removed")
	assert.FileExists(t, filepath.Join(unmarked, "README.md"), "a name-only match is kept")
	assert.Contains(t, e.auditLine(t, "legacy-hub-projects", "project_id="+project.ID), "outcome=deleted")
	assert.Contains(t, e.auditLine(t, "legacy-hub-projects", "project_id="+other.ID), "outcome=kept")
}

// An agent's storage cleanup removes its own home and secrets, and then
// agents/<slug> once nothing else is in it (the broker already removed the
// agent's workspace), and exactly its object prefix, audited with the hub
// as the deleter.
func TestAgentStorageCleanup_AgentRootAndPrefix(t *testing.T) {
	e := newStorageCleanupEnv(t)
	projectID := tid("asc-project")
	tree := e.seedTree(t, e.subRoot, projectID)
	a := &store.Agent{ID: tid("asc-agent"), Slug: "dev", ProjectID: projectID}
	agentDir := filepath.Join(tree, "agents", "dev")
	require.NoError(t, os.RemoveAll(filepath.Join(agentDir, "workspace"))) // the broker's removal happened
	for _, d := range []string{"home-" + a.ID, "secrets-" + a.ID} {
		require.NoError(t, os.MkdirAll(filepath.Join(agentDir, d, "x"), 0o755))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "agents", "other", "workspace"), 0o755))

	e.srv.startAgentStorageCleanup(a, true)
	e.srv.storageCleanupWG.Wait()

	assertAbsent(t, agentDir, "the emptied agent root is removed")
	assert.DirExists(t, filepath.Join(tree, "agents", "other", "workspace"))
	assert.DirExists(t, filepath.Join(tree, "workspace", ".git"))
	assert.Equal(t, []string{"hubs/hub-1/workspaces/" + projectID + "/" + a.ID + "/"}, e.objects.got())
	for _, tree := range []string{"agent-home", "agent-secrets", "agent-root"} {
		l := e.auditLine(t, tree)
		for _, s := range []string{"deleter=hub", "outcome=deleted", "agent=dev"} {
			assert.Contains(t, l, s, tree)
		}
	}
}

// The hub never removes the agent's own workspace (or anything it does not
// own): while the workspace is still there (the broker's removal has not
// happened), agents/<slug> is kept, audited as pending broker removal, and
// only the agent's own home and secrets go. Another agent's home with the
// same slug stays too.
func TestAgentStorageCleanup_WorkspacePendingKeepsAgentRoot(t *testing.T) {
	e := newStorageCleanupEnv(t)
	projectID := tid("asc-pending")
	agentDir := filepath.Join(e.seedTree(t, e.subRoot, projectID), "agents", "dev")
	a := &store.Agent{ID: tid("asc-self"), Slug: "dev", ProjectID: projectID}
	other := tid("asc-other")
	for _, d := range []string{"home-" + a.ID, "secrets-" + a.ID, "home-" + other} {
		require.NoError(t, os.MkdirAll(filepath.Join(agentDir, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "workspace", "unpushed.txt"), []byte("work"), 0o644))

	e.srv.startAgentStorageCleanup(a, false)
	e.srv.storageCleanupWG.Wait()

	assertAbsent(t, filepath.Join(agentDir, "home-"+a.ID), "own home removed")
	assertAbsent(t, filepath.Join(agentDir, "secrets-"+a.ID), "own secrets removed")
	assert.DirExists(t, filepath.Join(agentDir, "home-"+other))
	assert.FileExists(t, filepath.Join(agentDir, "workspace", "unpushed.txt"))
	l := e.auditLine(t, "agent-root")
	assert.Contains(t, l, "outcome=kept")
	assert.Contains(t, l, "workspace pending broker removal")
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

// storageCleanupCall is one observed startAgentStorageCleanup call.
type storageCleanupCall struct {
	agentID           string
	teardownConfirmed bool
}

// observeAgentStorageCleanup records the agent storage cleanups started
// during a test.
func observeAgentStorageCleanup(t *testing.T) func() []storageCleanupCall {
	t.Helper()
	var mu sync.Mutex
	var calls []storageCleanupCall
	agentStorageCleanupObserver = func(a *store.Agent, confirmed bool) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, storageCleanupCall{a.ID, confirmed})
	}
	t.Cleanup(func() { agentStorageCleanupObserver = nil })
	return func() []storageCleanupCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]storageCleanupCall(nil), calls...)
	}
}

// The delete engine starts the agent storage cleanup on a hard delete that
// deletes the agent's files (teardown confirmed when the broker answered),
// and on a forced delete after a broker failure (teardown not confirmed),
// never on a soft delete or a delete that keeps the files.
func TestAgentStorageCleanup_EngineWiring(t *testing.T) {
	cases := []struct {
		name          string
		retention     time.Duration
		query         string
		brokerFails   bool
		wantCall      bool
		wantConfirmed bool
	}{
		{"hard delete", 0, "", false, true, true},
		{"soft delete", time.Hour, "", false, false, false},
		{"hard delete keeping files", 0, "?deleteFiles=false", false, false, false},
		{"forced delete after a broker failure", time.Hour, "?force=true", true, true, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := observeAgentStorageCleanup(t)
			srv, s, _, disp := engineTestServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			if tc.brokerFails {
				disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker unreachable") })
			}
			agent := setupBrokerAgentInPhase(t, s, "wire-"+string(rune('a'+i)), state.PhaseStopped)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+tc.query, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			srv.storageCleanupWG.Wait()

			got := calls()
			if !tc.wantCall {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, agent.ID, got[0].agentID)
			assert.Equal(t, tc.wantConfirmed, got[0].teardownConfirmed)
		})
	}
}

// The retained-files purge starts the agent storage cleanup once it has
// purged the record.
func TestAgentStorageCleanup_PurgeWiring(t *testing.T) {
	calls := observeAgentStorageCleanup(t)
	srv, s, disp, a := retainedPurgeFixture(t, "wire", 2*time.Hour)
	disp.setFn(func(context.Context, *store.Agent) error { return nil })

	srv.purgeHandler()(context.Background())
	srv.storageCleanupWG.Wait()

	require.True(t, agentGone(t, s, a.ID))
	got := calls()
	require.Len(t, got, 1)
	assert.Equal(t, a.ID, got[0].agentID)
}

// forcedDeleteProject creates project p with one agent record when
// withAgent, then deletes it with force=true and waits for the storage
// cleanup.
func (e *storageCleanupEnv) forcedDeleteProject(t *testing.T, p *store.Project, withAgent bool) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, e.s.CreateProject(ctx, p))
	if withAgent {
		require.NoError(t, e.s.CreateAgent(ctx, &store.Agent{ID: tid(p.Slug + "-agent"), Slug: "dev", Name: "dev", ProjectID: p.ID}))
	}
	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+p.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	e.srv.storageCleanupWG.Wait()
}

// AC-F1: a forced delete that finds agent records deletes nothing of the
// project tree: it writes the tombstone with now + grace (at least 24 h)
// and audits the deferral; the object-storage prefix stays.
func TestForcedProjectDelete_WithAgentsDefers(t *testing.T) {
	e := newStorageCleanupEnv(t)
	p := &store.Project{ID: tid("fpd-agents"), Slug: "fpd-agents", Name: "fpd-agents"}
	tree := e.seedTree(t, e.subRoot, p.ID)
	before := time.Now()

	e.forcedDeleteProject(t, p, true)

	assert.FileExists(t, filepath.Join(tree, "workspace", "README.md"), "nothing is deleted")
	data, err := os.ReadFile(filepath.Join(tree, "provision", projectfs.DeleteAfterFile))
	require.NoError(t, err)
	after, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	require.NoError(t, err)
	assert.False(t, after.Before(before.Add(24*time.Hour).Add(-time.Second)), "grace of at least 24h: %s", after)
	assert.True(t, after.Before(time.Now().Add(24*time.Hour).Add(time.Minute)))
	assert.Empty(t, e.objects.got(), "the object prefix is deleted with the tree, later")
	l := e.auditLine(t, "project-root")
	assert.Contains(t, l, "outcome=kept")
	assert.Contains(t, l, "deferred until")
}

// The grace is the soft-delete retention when it is longer than 24 h.
func TestForcedDeleteGrace(t *testing.T) {
	e := newStorageCleanupEnv(t)
	e.srv.config.SoftDeleteRetention = time.Hour
	assert.Equal(t, 24*time.Hour, e.srv.forcedDeleteGrace())
	e.srv.config.SoftDeleteRetention = 72 * time.Hour
	assert.Equal(t, 72*time.Hour, e.srv.forcedDeleteGrace())
}

// AC-F2: a forced delete with no agent records deletes the tree and the
// prefix at once, as a normal delete.
func TestForcedProjectDelete_NoAgentsDeletesNow(t *testing.T) {
	e := newStorageCleanupEnv(t)
	p := &store.Project{ID: tid("fpd-none"), Slug: "fpd-none", Name: "fpd-none"}
	tree := e.seedTree(t, e.subRoot, p.ID)

	e.forcedDeleteProject(t, p, false)

	assertAbsent(t, tree, "the tree is removed at once")
	assert.Equal(t, []string{"hubs/hub-1/workspaces/" + p.ID + "/"}, e.objects.got())
}

// AC-F3: the maintenance pass removes a tombstoned tree and its prefix
// after its time, and not before.
func TestDeferredProjectDeletion_SweepAfterTime(t *testing.T) {
	e := newStorageCleanupEnv(t)
	layout, err := e.srv.projectStorageLayout()
	require.NoError(t, err)
	due := tid("dpd-due")
	notYet := tid("dpd-not-yet")
	dueTree := e.seedTree(t, e.subRoot, due)
	notYetTree := e.seedTree(t, e.subRoot, notYet)
	_, err = layout.MarkDeleteAfter(due, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	_, err = layout.MarkDeleteAfter(notYet, time.Now().Add(time.Hour))
	require.NoError(t, err)

	e.srv.sweepDeferredProjectDeletions(context.Background())

	assertAbsent(t, dueTree, "a due tree is removed")
	assert.FileExists(t, filepath.Join(notYetTree, "provision", projectfs.DeleteAfterFile), "a tree not yet due stays")
	assert.Equal(t, []string{"hubs/hub-1/workspaces/" + due + "/"}, e.objects.got())
	l := e.auditLine(t, "project-root", "project_id="+due)
	assert.Contains(t, l, "outcome=deleted")
	assert.Contains(t, l, deferredDeletionReason)
}

// AC-F4: a tombstoned tree whose project exists again is kept and its
// tombstone removed.
func TestDeferredProjectDeletion_RecreatedProjectKeepsTree(t *testing.T) {
	e := newStorageCleanupEnv(t)
	layout, err := e.srv.projectStorageLayout()
	require.NoError(t, err)
	p := &store.Project{ID: tid("dpd-again"), Slug: "dpd-again", Name: "dpd-again"}
	require.NoError(t, e.s.CreateProject(context.Background(), p))
	tree := e.seedTree(t, e.subRoot, p.ID)
	_, err = layout.MarkDeleteAfter(p.ID, time.Now().Add(-time.Minute))
	require.NoError(t, err)

	e.srv.sweepDeferredProjectDeletions(context.Background())

	assert.FileExists(t, filepath.Join(tree, "workspace", "README.md"))
	assertAbsent(t, filepath.Join(tree, "provision", projectfs.DeleteAfterFile), "the tombstone is removed")
	assert.Empty(t, e.objects.got())
	assert.Contains(t, e.auditLine(t, "project-root"), "deferred deletion cancelled")
}

// AC-F6: with the share not mounted the pass deletes nothing and writes one
// ERROR audit line per pass.
func TestDeferredProjectDeletion_ShareNotMounted(t *testing.T) {
	e := newStorageCleanupEnv(t)
	require.NoError(t, os.RemoveAll(e.subRoot)) // the mount point stays, its tree is gone

	e.srv.sweepDeferredProjectDeletions(context.Background())
	assert.Empty(t, e.auditLines(), "no project yet: a mount point without the tree is expected")

	require.NoError(t, e.s.CreateProject(context.Background(), &store.Project{ID: tid("dpd-unmounted"), Slug: "dpd-unmounted", Name: "dpd-unmounted"}))
	e.srv.sweepDeferredProjectDeletions(context.Background())

	lines := e.auditLines()
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "level=ERROR")
	assert.Contains(t, lines[0], "outcome=failed")
	assert.Empty(t, e.objects.got())
}

// A non-forced removal that still fails part-way after its retry writes
// the tombstone with the current time, so the maintenance pass retries it.
func TestProjectStorageCleanup_FailedRemovalRetriedByPass(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newStorageCleanupEnv(t)
	prev := projectStorageCleanupRetryDelay
	projectStorageCleanupRetryDelay = time.Millisecond
	t.Cleanup(func() { projectStorageCleanupRetryDelay = prev })
	project := &store.Project{ID: tid("psc-retry-later"), Slug: "psc-retry-later"}
	tree := e.seedTree(t, e.subRoot, project.ID)
	ws := filepath.Join(tree, "workspace")
	require.NoError(t, os.Chmod(ws, 0o555))
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })

	e.srv.startProjectStorageCleanup(project, time.Time{})
	e.srv.storageCleanupWG.Wait()
	assert.FileExists(t, filepath.Join(tree, "provision", projectfs.DeleteAfterFile))

	require.NoError(t, os.Chmod(ws, 0o755))
	e.srv.sweepDeferredProjectDeletions(context.Background())
	assertAbsent(t, tree, "the pass removes it")
}

// R1: on nfs the project tree is part of the critical workspace_storage
// check (handlers_health_storage_test.go). checkProjectStorageTree reports
// no project_storage_tree for nfs whatever the tree's state (present,
// missing on a fresh hub, missing with projects), and nothing on the local
// backend.
func TestHealth_ProjectStorageTree(t *testing.T) {
	e := newStorageCleanupEnv(t)
	steps := []func(){
		func() {},
		func() { require.NoError(t, os.RemoveAll(e.subRoot)) },
		func() { e.addProject(t, "health-p") },
	}
	for i, step := range steps {
		step()
		checks := map[string]string{}
		e.srv.checkProjectStorageTree(checks)
		_, has := checks[projectStorageTreeCheck]
		assert.False(t, has, "step %d: no project_storage_tree on nfs: %v", i, checks)
	}

	e.srv.config.ProjectStorageConfig = nil
	checks := map[string]string{}
	e.srv.checkProjectStorageTree(checks)
	_, has := checks[projectStorageTreeCheck]
	assert.False(t, has, "no check on the local backend")
}

// A sweep removal that fails part-way keeps the tombstone (writing it anew
// if the removal took it), so the next pass retries; that pass removes the
// tree.
func TestDeferredProjectDeletion_PartialFailureKeepsTombstone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newStorageCleanupEnv(t)
	prev := projectStorageCleanupRetryDelay
	projectStorageCleanupRetryDelay = time.Millisecond
	t.Cleanup(func() { projectStorageCleanupRetryDelay = prev })
	layout, err := e.srv.projectStorageLayout()
	require.NoError(t, err)
	id := tid("dpd-partial")
	tree := e.seedTree(t, e.subRoot, id)
	_, err = layout.MarkDeleteAfter(id, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	ws := filepath.Join(tree, "workspace")
	require.NoError(t, os.Chmod(ws, 0o555))
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })

	e.srv.sweepDeferredProjectDeletions(context.Background())
	assert.FileExists(t, filepath.Join(tree, "provision", projectfs.DeleteAfterFile), "the tombstone stays for the next pass")
	assert.Contains(t, e.auditLine(t, "project-root", "outcome=kept"), "maintenance pass retries it")

	require.NoError(t, os.Chmod(ws, 0o755))
	e.srv.sweepDeferredProjectDeletions(context.Background())
	assertAbsent(t, tree, "the next pass removes it")
}

// AC-F1b: on a non-shared backend a forced delete with agent records has no
// project tree on a share to defer; the object prefix (transfer staging) is
// deleted at once, audited with that outcome.
func TestForcedProjectDelete_NonSharedDeletesPrefixNow(t *testing.T) {
	e := newStorageCleanupEnv(t)
	e.srv.config.ProjectStorageConfig = nil // local backend
	p := &store.Project{ID: tid("fpd-local"), Slug: "fpd-local", Name: "fpd-local", GitRemote: "github.com/test/fpd-local"}
	nfsTree := e.seedTree(t, e.subRoot, p.ID)

	e.forcedDeleteProject(t, p, true)

	assert.Equal(t, []string{"hubs/hub-1/workspaces/" + p.ID + "/"}, e.objects.got())
	l := e.auditLine(t, "object-prefix")
	assert.Contains(t, l, "outcome=deleted")
	assert.Contains(t, l, "no broker workspace touched")
	assert.DirExists(t, filepath.Join(nfsTree, "workspace", ".git"), "no tree is touched on a non-shared backend")
}
