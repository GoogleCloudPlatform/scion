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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/v2"
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

	// Counting table row 1: the file wins, even with an empty profile over a
	// hub value. DR2-C3: a corrupt file with no provenance counts "", as
	// start would, and is unreadable, not missing.
	nativePath := config.BrokerProjectPath(globalDir, "", "pr-native")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: tid("pr-count-emptyfile"), Slug: "emptyfile", Name: "emptyfile",
		ProjectID: native.ID, RuntimeBrokerID: broker.ID, AppliedConfig: &store.AgentAppliedConfig{Profile: "local"}}))
	writeAgentInfo(t, nativePath, "emptyfile", "")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: tid("pr-count-corrupt"), Slug: "corrupt", Name: "corrupt",
		ProjectID: native.ID, RuntimeBrokerID: broker.ID, AppliedConfig: &store.AgentAppliedConfig{Profile: "local"}}))
	writeAgentInfo(t, nativePath, "corrupt", "x")
	projectDir, err := config.GetResolvedProjectDir(nativePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config.GetAgentHomePath(projectDir, "corrupt"), "agent-info.json"), []byte("{not json"), 0o644))

	refs, err := srv.countProfileReferences(ctx, broker.ID, globalDir)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"local": 3, "remote": 1, "hubval": 2, "": 2}, refs.Agents)
	assert.Equal(t, 1, refs.AgentInfoMissing)
	assert.Equal(t, 1, refs.AgentInfoUnreadable)
}

// publishTarget publishes overlay and brokerID as the reference target
// without a count (worker tests).
func publishTarget(srv *Server, overlay *config.SettingsOverlay, brokerID, globalDir string) {
	srv.profileRefs.mu.Lock()
	srv.profileRefs.overlay, srv.profileRefs.brokerID, srv.profileRefs.globalDir = overlay, brokerID, globalDir
	srv.profileRefs.mu.Unlock()
}

// refFaultStore gates GetBrokerProjects behind its switch: blocking until
// release (or ctx ends), failing, or panicking, and counts calls and their
// concurrency.
type refFaultStore struct {
	store.Store
	fault   *storeFaultSwitch
	mode    string // "block", "fail" or "panic"
	started chan struct{}
	release chan struct{}

	mu        sync.Mutex
	calls     int
	inFlight  int
	maxFlight int
	ctxErr    error
}

func (r *refFaultStore) GetBrokerProjects(ctx context.Context, brokerID string) ([]store.ProjectProvider, error) {
	r.mu.Lock()
	r.calls++
	r.inFlight++
	if r.inFlight > r.maxFlight {
		r.maxFlight = r.inFlight
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}()
	r.mu.Lock()
	mode := r.mode
	r.mu.Unlock()
	if !r.fault.Active() || mode == "pass" {
		return r.Store.GetBrokerProjects(ctx, brokerID)
	}
	switch mode {
	case "fail":
		return nil, errors.New("store unavailable")
	case "panic":
		panic("boom")
	}
	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	select {
	case <-ctx.Done():
		r.mu.Lock()
		r.ctxErr = ctx.Err()
		r.mu.Unlock()
		return nil, ctx.Err()
	case <-r.release:
		return r.Store.GetBrokerProjects(ctx, brokerID)
	}
}

func (r *refFaultStore) snapshot() (calls, maxFlight int, ctxErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.maxFlight, r.ctxErr
}

func newRefFaultServer(t *testing.T, mode string) (*Server, store.Store, *refFaultStore, *storeFaultSwitch) {
	t.Helper()
	return testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *refFaultStore {
		return &refFaultStore{Store: inner, fault: f, mode: mode, started: make(chan struct{}, 1), release: make(chan struct{})}
	})
}

// startPropagation starts settings propagation (and so the reference
// worker) on srv with a channel publisher.
func startPropagation(t *testing.T, srv *Server, s store.Store, poll time.Duration) *OperationalSettings {
	t.Helper()
	ops := NewOperationalSettings(s.(store.HubSettingStore), koanf.New("."), koanf.New("."))
	ops.PollInterval = poll
	ops.SetEventPublisher(NewChannelEventPublisher())
	ops.StartPropagation(context.Background(), srv)
	return ops
}

// DR2-RQ1: the worker runs under the propagation lifecycle; StopPropagation
// returns while a count is blocked and cancels it; triggers never block,
// before the worker exists and after it stopped.
func TestProfileReferences_WorkerStopsWithPropagation(t *testing.T) {
	srv, s, fs, fault := newRefFaultServer(t, "block")
	for i := 0; i < 5; i++ {
		srv.triggerProfileReferences()
	}
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	publishTarget(srv, overlay, "b", t.TempDir())
	ops := startPropagation(t, srv, s, time.Hour)

	fault.Arm()
	srv.triggerProfileReferences()
	select {
	case <-fs.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not start a count")
	}
	stopped := make(chan struct{})
	go func() {
		ops.StopPropagation()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopPropagation did not return while a count was blocked")
	}
	_, _, ctxErr := fs.snapshot()
	assert.ErrorIs(t, ctxErr, context.Canceled, "the count in progress was cancelled")
	srv.triggerProfileReferences()
	srv.triggerProfileReferences()
}

// DR-R3: triggers during a blocked count coalesce into exactly one more
// count, and counts never overlap.
func TestProfileReferences_TriggersCoalesce(t *testing.T) {
	srv, _, fs, fault := newRefFaultServer(t, "block")
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	publishTarget(srv, overlay, "b", t.TempDir())
	fault.Arm()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.runProfileReferenceWorker(ctx)

	srv.triggerProfileReferences()
	select {
	case <-fs.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not start a count")
	}
	for i := 0; i < 3; i++ {
		srv.triggerProfileReferences()
	}
	fs.release <- struct{}{} // finish the first count
	select {
	case <-fs.started: // the one coalesced count
	case <-time.After(10 * time.Second):
		t.Fatal("no coalesced count followed")
	}
	fs.release <- struct{}{}
	time.Sleep(200 * time.Millisecond)
	calls, maxFlight, _ := fs.snapshot()
	assert.Equal(t, 2, calls, "three triggers during a count give exactly one more count")
	assert.Equal(t, 1, maxFlight, "counts never overlap")
}

// The poll tick triggers a count even when no settings section changed.
func TestProfileReferences_PollTickTriggersCount(t *testing.T) {
	srv, s, fs, _ := newRefFaultServer(t, "block") // disarmed: counts pass through
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	publishTarget(srv, overlay, "b", t.TempDir())
	ops := startPropagation(t, srv, s, 50*time.Millisecond)
	t.Cleanup(ops.StopPropagation)
	require.Eventually(t, func() bool {
		calls, _, _ := fs.snapshot()
		return calls >= 2
	}, 10*time.Second, 20*time.Millisecond, "poll ticks must trigger counts")
}

// R5: a repeated count failure is logged once; a success logs recovery.
func TestProfileReferences_ErrorLoggedOncePerText(t *testing.T) {
	srv, _, fs, fault := newRefFaultServer(t, "fail")
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	srv.EnableProfileReferences(context.Background(), overlay, "b", t.TempDir())
	fault.Arm()
	logs := captureHubLogs(t, func() {
		srv.runProfileReferenceCount(context.Background())
		srv.runProfileReferenceCount(context.Background())
		srv.runProfileReferenceCount(context.Background())
	})
	assert.Equal(t, 1, strings.Count(logs, "could not be counted"), logs)
	assert.NotContains(t, logs, "counted again")

	fs.mu.Lock()
	fs.mode = "pass"
	fs.mu.Unlock()
	logs = captureHubLogs(t, func() { srv.runProfileReferenceCount(context.Background()) })
	assert.Contains(t, logs, "Built-in profile references counted again", logs)
}

// A panicking count is recovered and the next count runs again.
func TestProfileReferences_PanicRecovered(t *testing.T) {
	srv, _, fs, fault := newRefFaultServer(t, "panic")
	publishTarget(srv, config.NewSettingsOverlay(), "b", t.TempDir())
	fault.Arm()
	srv.runProfileReferenceCount(context.Background())
	srv.runProfileReferenceCount(context.Background())
	calls, _, _ := fs.snapshot()
	assert.Equal(t, 2, calls)
}

// DR2-C2 (final ruling): a failed install count enables conservative
// retention: local resolves, remote and its runtime do not; the ERROR and
// the conservative WARN are logged; a later successful zero-reference
// count keeps local.
func TestProfileReferences_InstallFailureIsConservative(t *testing.T) {
	srv, _, fs, fault := newRefFaultServer(t, "fail")
	fault.Arm()
	overlay := config.NewSettingsOverlay()
	overlay.Update(map[string]config.V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}}, storedGkeNFSProfiles(), nil, "")
	var localErr, remoteErr error
	logs := captureHubLogs(t, func() {
		srv.EnableProfileReferences(context.Background(), overlay, "b", t.TempDir())
		localErr = resolvesThrough(overlay, "local")
		remoteErr = resolvesThrough(overlay, "remote")
	})
	require.True(t, overlay.RetentionEnabled())
	assert.NoError(t, localErr)
	assert.Error(t, remoteErr)
	assert.Contains(t, logs, "could not be counted at start")
	assert.Contains(t, logs, "References could not be counted; retaining every built-in default profile")

	// Same server, store healthy again: a later count never removes local.
	fs.mu.Lock()
	fs.mode = "pass"
	fs.mu.Unlock()
	srv.runProfileReferenceCount(context.Background())
	assert.NoError(t, resolvesThrough(overlay, "local"), "a later count never removes a retained name")
}

// DR2-C5 / R10: a flat instance ID known to the hub is never the
// co-located ID; with no legacy ID, nothing is enabled.
func TestProfileReferences_FlatIDIsNotColocated(t *testing.T) {
	srv, _ := testServer(t)
	srv.mu.Lock()
	srv.recordEmbeddedFlatActivatedLocked("flat-key", "flat-broker-id")
	srv.mu.Unlock()
	require.Equal(t, "flat-broker-id", srv.GetEmbeddedBrokerID(), "precondition: the hub knows the flat instance")
	assert.Equal(t, "", srv.LegacyColocatedBrokerID())

	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	srv.EnableProfileReferences(context.Background(), overlay, srv.LegacyColocatedBrokerID(), t.TempDir())
	assert.False(t, overlay.RetentionEnabled())

	srv.SetEmbeddedBrokerID("legacy-id")
	assert.Equal(t, "legacy-id", srv.LegacyColocatedBrokerID())
}

// R7: a reincarnate move onto the co-located broker records the project's
// activeProfile and the moved agent's profile, on both branches (target
// already serves the project, and the target is linked by the move).
func TestProfileReferences_MoveOntoColocatedBroker(t *testing.T) {
	for _, linked := range []bool{true, false} {
		t.Run(map[bool]string{true: "target serves the project", false: "move links the target"}[linked], func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			broker := newLocalPathTestBroker(t, s, "pr-move-broker")
			project := &store.Project{ID: tid("pr-move"), Name: "pr-move", Slug: "pr-move",
				Annotations: map[string]string{projectSettingActiveProfile: "local"}}
			require.NoError(t, s.CreateProject(ctx, project))
			if linked {
				require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
					ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
				}))
			}
			overlay := config.NewSettingsOverlay()
			overlay.Update(map[string]config.V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}}, storedGkeNFSProfiles(), nil, "")
			srv.EnableProfileReferences(ctx, overlay, broker.ID, t.TempDir())
			require.Error(t, resolvesThrough(overlay, "local"))
			require.Error(t, resolvesThrough(overlay, "remote"))

			require.NoError(t, srv.linkMoveTargetProvider(ctx, &reincarnationMove{
				SourceBrokerID: "elsewhere", TargetBrokerID: broker.ID, ProjectID: project.ID, Profile: "remote",
			}))
			assert.NoError(t, resolvesThrough(overlay, "local"), "the project's activeProfile")
			assert.NoError(t, resolvesThrough(overlay, "remote"), "the moved agent's profile, with its runtime")
		})
	}
}

// R7 (structural): every pkg/hub provider link goes through
// linkProjectProvider, so none can skip the placement hook.
func TestProfileReferences_EveryProviderLinkIsHooked(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "profile_references.go" {
			continue
		}
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.NotContains(t, string(data), ".store.AddProjectProvider(",
			"%s links a provider without linkProjectProvider (built-in profile retention would miss it)", name)
	}
}

// R8: the project settings PUT of an activeProfile on a project the
// co-located broker provides retains it at once.
func TestProfileReferences_ProjectSettingsPutRetains(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID))
	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, storedGkeNFSProfiles(), nil, "")
	f.srv.EnableProfileReferences(ctx, overlay, f.broker.ID, t.TempDir())
	require.Error(t, resolvesThrough(overlay, "local"))

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPut, "/api/v1/projects/"+f.proj.ID+"/settings",
		map[string]any{"activeProfile": "local"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NoError(t, resolvesThrough(overlay, "local"))
}

// R11(c): the DB-tier GET shows the retained built-ins read-only, while
// profiles and runtimes stay the stored sets, and no request field can
// carry them.
func TestProfileReferences_ServerConfigGetShowsRetained(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _, ops := newHubNameDBServer(t, "")
	overlay := config.NewSettingsOverlay()
	overlay.Update(map[string]config.V1RuntimeConfig{"gke-autopilot": {Type: "kubernetes"}},
		map[string]config.V1ProfileConfig{"gke": {Runtime: "gke-autopilot"}, "local-nfs": {Runtime: "docker"}}, nil, "")
	overlay.SetProfileReferences(config.ProfileReferences{Agents: map[string]int{"local": 2}})
	require.NoError(t, resolvesThrough(overlay, "local"))
	publishTarget(srv, overlay, "b", t.TempDir())

	resp := getServerConfigDB(t, srv, ops)
	require.Len(t, resp.RetainedBuiltinProfiles, 1)
	assert.Equal(t, "local", resp.RetainedBuiltinProfiles[0].Name)
	assert.Equal(t, 2, resp.RetainedBuiltinProfiles[0].Agents)
	require.NotEmpty(t, resp.RetainedBuiltinRuntimes)
	assert.NotContains(t, resp.Profiles, "local", "profiles stay the stored set")

	reqType := reflect.TypeOf(ServerConfigUpdateRequest{})
	for i := 0; i < reqType.NumField(); i++ {
		assert.NotContains(t, reqType.Field(i).Tag.Get("json"), "retained", "no request field may carry retained built-ins")
	}
}

// captureHubLogs runs fn with the default slog logger writing to a buffer.
func captureHubLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}
