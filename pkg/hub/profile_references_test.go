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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storedGkeNFSProfiles is a stored profile set without the built-in local.
func storedGkeNFSProfiles() map[string]config.V1ProfileConfig {
	return map[string]config.V1ProfileConfig{
		"gke":       {Runtime: "gke-autopilot"},
		"local-nfs": {Runtime: "docker"},
	}
}

// fileLoadedSettings is what a settings load yields before the overlay:
// the built-in defaults plus a settings file defining gke and local-nfs.
func fileLoadedSettings(active string) *config.VersionedSettings {
	return &config.VersionedSettings{
		ActiveProfile: active,
		Runtimes: map[string]config.V1RuntimeConfig{
			"docker":        {Type: "docker"},
			"container":     {Type: "container"},
			"kubernetes":    {Type: "kubernetes"},
			"gke-autopilot": {Type: "kubernetes"},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"local":     {Runtime: "docker"},
			"remote":    {Runtime: "kubernetes"},
			"gke":       {Runtime: "gke-autopilot"},
			"local-nfs": {Runtime: "docker"},
		},
	}
}

// resolvesThrough reports whether profile resolves through overlay in a
// fresh settings load, as the co-located broker's dispatch would.
func resolvesThrough(overlay *config.SettingsOverlay, profile string) error {
	vs := fileLoadedSettings("gke")
	overlay.Apply(vs)
	_, _, err := vs.ResolveRuntime(profile)
	return err
}

// writeAgentInfo writes agent slug's agent-info.json with profile where
// provision puts it for the broker project path projectPath.
func writeAgentInfo(t *testing.T, projectPath, slug, profile string) {
	t.Helper()
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	require.NoError(t, err)
	home := config.GetAgentHomePath(projectDir, slug)
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(`{"profile":"`+profile+`"}`), 0o644))
}

// resolveAtDispatchDispatcher records, inside DispatchAgentCreate (at
// dispatch time), whether the built-in local resolves through the overlay.
type resolveAtDispatchDispatcher struct {
	createAgentDispatcher
	overlay *config.SettingsOverlay

	mu     sync.Mutex
	called bool
	err    error
}

func (d *resolveAtDispatchDispatcher) DispatchAgentCreateWithGather(ctx context.Context, a *store.Agent) (*CreateDispatchResult, error) {
	d.mu.Lock()
	d.called = true
	d.err = resolvesThrough(d.overlay, "local")
	d.mu.Unlock()
	return d.createAgentDispatcher.DispatchAgentCreateWithGather(ctx, a)
}

// The auto-link create path links the co-located broker and dispatches in
// the same request: the project's activeProfile built-in must already
// resolve at that dispatch (DR2-RQ2), not only after the next count.
func TestProfileReferences_AutoLinkCreateRetainsFirstTime(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	ctx := context.Background()
	if f.proj.Annotations == nil {
		f.proj.Annotations = map[string]string{}
	}
	f.proj.Annotations[projectSettingActiveProfile] = "local"
	require.NoError(t, f.store.UpdateProject(ctx, f.proj))

	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	f.srv.EnableProfileReferences(ctx, overlay, f.unlinked.ID, t.TempDir())
	require.Error(t, resolvesThrough(overlay, "local"), "precondition: local is not retained before the link")

	disp := &resolveAtDispatchDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}, overlay: overlay}
	f.srv.SetDispatcher(disp)
	rec := createAgentAsOwner(t, f.bypassAgentsFixture, CreateAgentRequest{
		Name:            "auto-link-local-agent",
		RuntimeBrokerID: f.unlinked.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	disp.mu.Lock()
	defer disp.mu.Unlock()
	require.True(t, disp.called, "the create must dispatch")
	assert.NoError(t, disp.err, "local must resolve at the first dispatch")
}

// A link to another broker adds no reference.
func TestProfileReferences_LinkToOtherBrokerAddsNothing(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("pr-other"), Name: "pr-other", Slug: "pr-other",
		Annotations: map[string]string{projectSettingActiveProfile: "local"}}
	require.NoError(t, s.CreateProject(ctx, project))
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	srv.EnableProfileReferences(ctx, overlay, "colocated-broker", t.TempDir())

	srv.onColocatedPlacement(ctx, project.ID, "some-other-broker", "local")
	assert.Error(t, resolvesThrough(overlay, "local"))

	srv.onColocatedPlacement(ctx, project.ID, "colocated-broker", "")
	assert.NoError(t, resolvesThrough(overlay, "local"))
}

// A project activeProfile save adds the reference only when the co-located
// broker provides the project (SR4-R1).
func TestProfileReferences_ProjectSaveGatedOnProvider(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "pr-save-broker")
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	srv.EnableProfileReferences(ctx, overlay, broker.ID, t.TempDir())

	notProvided := &store.Project{ID: tid("pr-save-np"), Name: "np", Slug: "pr-save-np",
		Annotations: map[string]string{projectSettingActiveProfile: "remote"}}
	require.NoError(t, s.CreateProject(ctx, notProvided))
	srv.onProjectActiveProfileSaved(ctx, notProvided)
	assert.Error(t, resolvesThrough(overlay, "remote"), "a project the co-located broker does not provide adds nothing")

	provided := &store.Project{ID: tid("pr-save-p"), Name: "p", Slug: "pr-save-p",
		Annotations: map[string]string{projectSettingActiveProfile: "local"}}
	require.NoError(t, s.CreateProject(ctx, provided))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: provided.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
	srv.onProjectActiveProfileSaved(ctx, provided)
	assert.NoError(t, resolvesThrough(overlay, "local"))
}

// The count reads agent-info.json at the path dispatch sends and the broker
// resolves, for linked, hub-native and global projects and a project with no
// provider row; it counts the hub value for missing files and unplaced
// agents, and skips agents on other brokers (SR2-R1, settings N1).
func TestProfileReferences_CountReadsAgentInfoWhereDispatchPoints(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	globalDir := t.TempDir()
	broker := newLocalPathTestBroker(t, s, "pr-count-broker")
	other := newLocalPathTestBroker(t, s, "pr-count-other")
	d := NewHTTPAgentDispatcherWithClient(s, nil, false, slog.Default())

	linkedPath := filepath.Join(t.TempDir(), "linked", config.DotScion)
	require.NoError(t, os.MkdirAll(linkedPath, 0o755))
	type proj struct {
		slug, localPath string
		provider        bool
		savedProfile    string
	}
	projects := []proj{
		{slug: "pr-linked", localPath: linkedPath, provider: true, savedProfile: "local"},
		{slug: "pr-native", provider: true, savedProfile: "local"},
		{slug: globalProjectSlug, localPath: globalDir, provider: true, savedProfile: "remote"},
		{slug: "pr-noprov", provider: false, savedProfile: "local"},
	}
	for _, p := range projects {
		project := &store.Project{ID: tid("pr-count-" + p.slug), Name: p.slug, Slug: p.slug}
		require.NoError(t, s.CreateProject(ctx, project))
		if p.provider {
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: p.localPath,
				Status: store.BrokerStatusOnline,
			}))
		}
		a := &store.Agent{ID: tid("pr-count-agent-" + p.slug), Slug: "agent-" + p.slug, Name: "agent-" + p.slug,
			ProjectID: project.ID, RuntimeBrokerID: broker.ID, Phase: string(state.PhaseStopped)}
		require.NoError(t, s.CreateAgent(ctx, a))

		// Parity: the count's location is exactly what dispatch sends.
		info, err := d.resolveDispatchProjectInfo(ctx, a)
		require.NoError(t, err)
		wantPath, wantSlug := config.ProjectDispatchLocation(p.localPath, p.slug, p.slug == globalProjectSlug)
		assert.Equal(t, wantPath, info.projectPath, p.slug)
		assert.Equal(t, wantSlug, info.projectSlug, p.slug)
		if p.slug == globalProjectSlug {
			assert.Equal(t, globalProjectSlug, info.projectSlug, "the global project's constant slug travels with its path")
		}
		writeAgentInfo(t, config.BrokerProjectPath(globalDir, info.projectPath, info.projectSlug), a.Slug, p.savedProfile)
	}

	// A placed agent whose file is missing counts its hub value and raises
	// agent_info_missing; an unplaced agent counts its hub value without it;
	// an agent on another broker counts nowhere.
	native, err := s.GetProjectBySlug(ctx, "pr-native")
	require.NoError(t, err)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: tid("pr-count-missing"), Slug: "missing", Name: "missing",
		ProjectID: native.ID, RuntimeBrokerID: broker.ID, AppliedConfig: &store.AgentAppliedConfig{Profile: "hubval"}}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: tid("pr-count-unplaced"), Slug: "unplaced", Name: "unplaced",
		ProjectID: native.ID, AppliedConfig: &store.AgentAppliedConfig{Profile: "hubval"}}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: tid("pr-count-otherb"), Slug: "otherb", Name: "otherb",
		ProjectID: native.ID, RuntimeBrokerID: other.ID, AppliedConfig: &store.AgentAppliedConfig{Profile: "remote"}}))

	refs, err := srv.countProfileReferences(ctx, broker.ID, globalDir)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"local": 3, "remote": 1, "hubval": 2}, refs.Agents)
	assert.Equal(t, 1, refs.AgentInfoMissing)
	assert.Equal(t, 0, refs.AgentInfoUnreadable)
}

// With no published target the worker does nothing, triggers never block,
// and the worker stops with its context, cancelling a count in progress.
func TestProfileReferences_WorkerLifetime(t *testing.T) {
	srv, s := testServer(t)
	for i := 0; i < 5; i++ {
		srv.triggerProfileReferences() // no worker yet: must not block
	}

	blocking := &blockingBrokerProjectsStore{Store: s, started: make(chan struct{}, 1)}
	srv.store = blocking
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	srv.profileRefs.mu.Lock()
	srv.profileRefs.overlay, srv.profileRefs.brokerID, srv.profileRefs.globalDir = overlay, "b", t.TempDir()
	srv.profileRefs.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.runProfileReferenceWorker(ctx)
		close(done)
	}()
	srv.triggerProfileReferences()
	select {
	case <-blocking.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not start a count")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop when its context ended")
	}
	assert.ErrorIs(t, blocking.ctxErr(), context.Canceled, "the count in progress was cancelled")
	srv.triggerProfileReferences() // after stop: must not block
	srv.triggerProfileReferences()
}

// A panicking count is recovered and the next trigger runs again.
func TestProfileReferences_PanicRecovered(t *testing.T) {
	srv, s := testServer(t)
	p := &panickingBrokerProjectsStore{Store: s}
	srv.store = p
	overlay := config.NewSettingsOverlay()
	srv.profileRefs.mu.Lock()
	srv.profileRefs.overlay, srv.profileRefs.brokerID = overlay, "b"
	srv.profileRefs.mu.Unlock()

	srv.runProfileReferenceCount(context.Background())
	srv.runProfileReferenceCount(context.Background())
	assert.Equal(t, 2, p.calls)
}

// A failed install count enables conservative retention: local resolves,
// remote does not; a later successful zero-reference count keeps local.
func TestProfileReferences_InstallFailureIsConservative(t *testing.T) {
	srv, s := testServer(t)
	srv.store = &failingBrokerProjectsStore{Store: s}
	overlay := config.NewSettingsOverlay()
	overlay.Update(map[string]config.V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}}, storedGkeNFSProfiles(), nil, "")
	srv.EnableProfileReferences(context.Background(), overlay, "b", t.TempDir())

	require.True(t, overlay.RetentionEnabled())
	assert.NoError(t, resolvesThrough(overlay, "local"))
	assert.Error(t, resolvesThrough(overlay, "remote"))

	srv.store = s
	srv.runProfileReferenceCount(context.Background())
	assert.NoError(t, resolvesThrough(overlay, "local"), "a later count never removes a retained name")
}

// LegacyColocatedBrokerID never returns a flat instance ID, and no ID means
// no retention.
func TestProfileReferences_NoLegacyBrokerNoRetention(t *testing.T) {
	srv, _ := testServer(t)
	assert.Equal(t, "", srv.LegacyColocatedBrokerID())
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	srv.EnableProfileReferences(context.Background(), overlay, srv.LegacyColocatedBrokerID(), t.TempDir())
	assert.False(t, overlay.RetentionEnabled())

	srv.SetEmbeddedBrokerID("legacy-id")
	assert.Equal(t, "legacy-id", srv.LegacyColocatedBrokerID())
}

type blockingBrokerProjectsStore struct {
	store.Store
	started chan struct{}
	mu      sync.Mutex
	err     error
}

func (b *blockingBrokerProjectsStore) GetBrokerProjects(ctx context.Context, brokerID string) ([]store.ProjectProvider, error) {
	b.started <- struct{}{}
	<-ctx.Done()
	b.mu.Lock()
	b.err = ctx.Err()
	b.mu.Unlock()
	return nil, ctx.Err()
}

func (b *blockingBrokerProjectsStore) ctxErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

type panickingBrokerProjectsStore struct {
	store.Store
	calls int
}

func (p *panickingBrokerProjectsStore) GetBrokerProjects(context.Context, string) ([]store.ProjectProvider, error) {
	p.calls++
	panic("boom")
}

type failingBrokerProjectsStore struct{ store.Store }

func (failingBrokerProjectsStore) GetBrokerProjects(context.Context, string) ([]store.ProjectProvider, error) {
	return nil, errors.New("store unavailable")
}
