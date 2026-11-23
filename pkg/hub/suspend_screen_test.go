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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type suspendScreenDispatcher struct {
	quotaLifecycleDispatcher
	mu          sync.Mutex
	tmuxOutput  string
	tmuxExit    int
	tmuxErr     error
	tmuxBlock   bool
	wsBlock     bool
	stopErr     error
	tmuxCalls   int
	lastTmuxCmd []string
	// execNotFound causes live Exec calls (not pre-suspend tmux) to fail with
	// broker 404 so GET .../exec returns 409 agent_not_running.
	execNotFound bool
}

func (d *suspendScreenDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if d.stopErr != nil {
		return d.stopErr
	}
	return d.quotaLifecycleDispatcher.DispatchAgentStop(ctx, agent)
}

func (d *suspendScreenDispatcher) DispatchAgentExec(ctx context.Context, _ *store.Agent, command []string, _ int) (string, int, error) {
	d.mu.Lock()
	if len(command) > 0 && command[0] == "tmux" {
		if d.execNotFound {
			d.mu.Unlock()
			return "", 0, &brokerStatusError{StatusCode: http.StatusNotFound, Body: `{"error":{"code":"agent_not_found"}}`}
		}
		d.tmuxCalls++
		d.lastTmuxCmd = append([]string(nil), command...)
		out, exit, err, block := d.tmuxOutput, d.tmuxExit, d.tmuxErr, d.tmuxBlock
		d.mu.Unlock()
		if block {
			<-ctx.Done()
			return "", 0, ctx.Err()
		}
		return out, exit, err
	}
	wsBlock := d.wsBlock
	d.mu.Unlock()
	if wsBlock {
		<-ctx.Done()
		return "", 0, ctx.Err()
	}
	return "scion-workspace-check commits=0 files=0\n", 0, nil
}

func (d *suspendScreenDispatcher) setTmux(output string, exit int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tmuxOutput = output
	d.tmuxExit = exit
	d.tmuxErr = err
}

func setSuspendScreenExperiment(t *testing.T, srv *Server, value bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{experiments.SuspendScreen: value})
	require.Equal(t, value, srv.experimentEnabled(experiments.SuspendScreen))
}

func TestCaptureSuspendScreen_FailureModes(t *testing.T) {
	oldTimeout := suspendScreenTimeout
	suspendScreenTimeout = 150 * time.Millisecond
	t.Cleanup(func() { suspendScreenTimeout = oldTimeout })

	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "capfail")
	a := newWorkspaceAgent(t, s, broker, project, "cap-a", "docker", "", state.PhaseRunning)

	cases := []struct {
		name  string
		disp  AgentDispatcher
		agent *store.Agent
		off   bool
	}{
		{name: "nil dispatcher", disp: nil, agent: a},
		{name: "empty broker", disp: &suspendScreenDispatcher{tmuxOutput: "hi\n"}, agent: &store.Agent{ID: a.ID, Phase: string(state.PhaseRunning)}},
		{name: "not running", disp: &suspendScreenDispatcher{tmuxOutput: "hi\n"}, agent: &store.Agent{ID: a.ID, RuntimeBrokerID: broker.ID, Phase: string(state.PhaseStopped)}},
		{name: "exec error", disp: &suspendScreenDispatcher{tmuxErr: errors.New("cloudrun: exec connector not configured")}, agent: a},
		{name: "lifecycle deferred", disp: &suspendScreenDispatcher{tmuxErr: ErrLifecycleDeferred}, agent: a},
		{name: "non-zero exit", disp: &suspendScreenDispatcher{tmuxOutput: "can't find session: scion", tmuxExit: 1}, agent: a},
		{name: "empty output", disp: &suspendScreenDispatcher{tmuxOutput: "   \n\n"}, agent: a},
		{name: "timeout", disp: &suspendScreenDispatcher{tmuxBlock: true}, agent: a},
		{name: "experiment off", disp: &suspendScreenDispatcher{tmuxOutput: "hi\n"}, agent: a, off: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.off {
				setSuspendScreenExperiment(t, srv, false)
				t.Cleanup(func() { setSuspendScreenExperiment(t, srv, true) })
			}
			got := srv.captureSuspendScreen(context.Background(), tc.disp, tc.agent)
			assert.Nil(t, got)
		})
	}
}

func TestPreSuspendProbes_ConcurrentBound(t *testing.T) {
	oldWS := workspaceCheckTimeout
	oldScreen := suspendScreenTimeout
	workspaceCheckTimeout = 400 * time.Millisecond
	suspendScreenTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		workspaceCheckTimeout = oldWS
		suspendScreenTimeout = oldScreen
	})

	disp := &suspendScreenDispatcher{tmuxBlock: true, wsBlock: true}
	srv, s := testServer(t)
	srv.SetDispatcher(disp)
	broker, project := newQuotaTestBrokerAndProject(t, s, "probe-bound")
	a := newWorkspaceAgent(t, s, broker, project, "probe-a", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	start := time.Now()
	cap := srv.preSuspendProbes(context.Background(), disp, a, false)
	elapsed := time.Since(start)
	assert.Nil(t, cap)
	// Running sequentially would take ~700ms; concurrently it finishes within
	// workspaceCheckTimeout + scheduling slack.
	assert.Less(t, elapsed, 600*time.Millisecond)
}

func TestSuspendScreen_ManualSuspendAndRBACAndLook(t *testing.T) {
	ctx := context.Background()
	disp := &suspendScreenDispatcher{tmuxOutput: "\x1b[32mready>\x1b[0m working on task\n\n\n"}
	srv, s := testServer(t)
	srv.SetDispatcher(disp)
	broker, project := newQuotaTestBrokerAndProject(t, s, "screen-rbac")

	// Seed users: agent owner, project admin (non-owner), and project viewer (agent.read only).
	mkUser := func(id, email string) *store.User {
		u := &store.User{
			ID:          tid(id),
			Email:       email,
			DisplayName: id,
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
		return u
	}
	bindProjectRole := func(u *store.User, roleName string) {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          project.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	owner := mkUser("screen-owner", "screen-owner@test.com")
	projAdmin := mkUser("screen-padmin", "screen-padmin@test.com")
	projViewer := mkUser("screen-pviewer", "screen-pviewer@test.com")
	bindProjectRole(owner, store.ProjectRoleMember)
	bindProjectRole(projAdmin, store.ProjectRoleAdmin)
	bindProjectRole(projViewer, store.ProjectRoleViewer)

	a := &store.Agent{
		ID:              tid("agent-screen-1"),
		Slug:            "screen-1",
		Name:            "screen-1",
		ProjectID:       project.ID,
		OwnerID:         owner.ID,
		CreatedBy:       owner.ID,
		RuntimeBrokerID: broker.ID,
		Runtime:         "docker",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, a))

	// Before suspend: GET /screen returns 404 for owner.
	rec := doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/agents/"+a.ID+"/screen", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// Manual suspend captures and persists the screen.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, suspendScreenCommand(), disp.lastTmuxCmd)

	// Both routes return 200 with Cache-Control: no-store for the owner.
	for _, path := range []string{
		"/api/v1/agents/" + a.ID + "/screen",
		"/api/v1/projects/" + project.ID + "/agents/" + a.Slug + "/screen",
	} {
		rec = doRequestAsUser(t, srv, owner, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, path+": "+rec.Body.String())
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		var resp agentScreenResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, a.ID, resp.AgentID)
		assert.Equal(t, "\x1b[32mready>\x1b[0m working on task\n", resp.Content)
		assert.False(t, resp.Truncated)
		assert.Equal(t, string(state.PhaseSuspended), resp.Phase)
		assert.False(t, resp.CapturedAt.IsZero())
	}

	// RBAC: project viewer (agent.read only) and project admin on another
	// member's agent are both denied 403 on both routes.
	for _, u := range []*store.User{projViewer, projAdmin} {
		for _, path := range []string{
			"/api/v1/agents/" + a.ID + "/screen",
			"/api/v1/projects/" + project.ID + "/agents/" + a.Slug + "/screen",
		} {
			rec = doRequestAsUser(t, srv, u, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s on %s: %s", u.Email, path, rec.Body.String())
		}
	}

	// Unauthenticated request gets 401.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+a.ID+"/screen", nil)
	recUnauth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recUnauth, req)
	assert.Equal(t, http.StatusUnauthorized, recUnauth.Code)

	// Wrong HTTP method gets 405.
	recMethod := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+a.ID+"/screen", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, recMethod.Code)

	// End-to-end hubclient: Exec returns 409 agent_not_running, GetScreen returns 200.
	disp.mu.Lock()
	disp.execNotFound = true
	disp.mu.Unlock()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ownerToken, _, _, err := srv.userTokenService.GenerateTokenPair(owner.ID, owner.Email, owner.DisplayName, owner.Role, ClientTypeCLI)
	require.NoError(t, err)
	hc, err := hubclient.New(ts.URL, hubclient.WithBearerToken(ownerToken))
	require.NoError(t, err)

	_, execErr := hc.ProjectAgents(project.ID).Exec(ctx, a.Slug, []string{"tmux", "capture-pane", "-pe", "-t", "scion"}, 10)
	var apiErr *apiclient.APIError
	require.ErrorAs(t, execErr, &apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.StatusCode)
	assert.Equal(t, ErrCodeAgentNotRunning, apiErr.Code)

	gotScreen, err := hc.ProjectAgents(project.ID).GetScreen(ctx, a.Slug)
	require.NoError(t, err)
	assert.Equal(t, "\x1b[32mready>\x1b[0m working on task\n", gotScreen.Content)
	assert.Equal(t, string(state.PhaseSuspended), gotScreen.Phase)

	disp.mu.Lock()
	disp.execNotFound = false
	disp.mu.Unlock()

	// Resume does not clear the stored row.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	gotAfterStart, err := s.GetAgentSuspendScreen(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "\x1b[32mready>\x1b[0m working on task\n", gotAfterStart.Content)

	// Re-suspend replaces the row.
	disp.setTmux("second run screen\n", 0, nil)
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	gotSecond, err := s.GetAgentSuspendScreen(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "second run screen\n", gotSecond.Content)

	// Experiment off: GET /screen returns 404 and existing row is left alone.
	setSuspendScreenExperiment(t, srv, false)
	rec = doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/agents/"+a.ID+"/screen", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	stillThere, err := s.GetAgentSuspendScreen(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "second run screen\n", stillThere.Content)
	setSuspendScreenExperiment(t, srv, true)

	// Re-suspend where capture fails deletes any older row so stale content
	// is never shown as the latest suspend's screen.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	disp.setTmux("", 1, nil)
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err = s.GetAgentSuspendScreen(ctx, a.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Managed agent returns 404.
	managed := &store.Agent{
		ID:        tid("agent-screen-managed"),
		Slug:      "screen-managed",
		Name:      "screen-managed",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Runtime:   "managed",
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, managed))
	rec = doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/agents/"+managed.ID+"/screen", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSuspendScreen_AutoSuspendPaths(t *testing.T) {
	ctx := context.Background()

	seedRunningAgent := func(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name, activity string, actSince time.Time) *store.Agent {
		t.Helper()
		a := newWorkspaceAgent(t, s, broker, project, name, "docker", "", state.PhaseRunning)
		_, err := s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
		require.NoError(t, err)
		db := s.(*entadapter.CompositeStore).DB()
		_, err = db.ExecContext(ctx,
			"UPDATE agents SET run_id = ?, activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
			"run-"+name, activity, actSince.UTC(), time.Now().UTC(), a.ID,
		)
		require.NoError(t, err)
		loaded, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		return loaded
	}

	enableIdle := func(t *testing.T, srv *Server) {
		t.Helper()
		setExperimentOverrides(t, srv, map[string]bool{
			experiments.SuspendScreen: true,
			experiments.IdleSuspend:   true,
		})
		srv.mu.Lock()
		srv.config.AutoSuspendIdle = true
		srv.config.IdleSuspendAfter = MinIdleSuspendAfter
		srv.mu.Unlock()
	}

	idleSince := time.Now().Add(-2 * DefaultIdleSuspendAfter)

	t.Run("stalled auto-suspend captures and persists screen", func(t *testing.T) {
		disp := &suspendScreenDispatcher{tmuxOutput: "stalled output\n"}
		srv, s := testServer(t)
		srv.SetDispatcher(disp)
		broker, project := newQuotaTestBrokerAndProject(t, s, "as-stalled")
		a := seedRunningAgent(t, s, broker, project, "stalled-1", string(state.ActivityStalled), time.Now().Add(-10*time.Minute))

		ok := srv.autoSuspendAgent(ctx, disp, a, state.SuspendReasonStalled)
		require.True(t, ok)

		got, err := s.GetAgentSuspendScreen(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "stalled output\n", got.Content)
	})

	t.Run("idle auto-suspend captures and persists screen and conversion preserves it", func(t *testing.T) {
		disp := &suspendScreenDispatcher{tmuxOutput: "idle output\n"}
		srv, s := testServer(t)
		srv.SetDispatcher(disp)
		enableIdle(t, srv)
		broker, project := newQuotaTestBrokerAndProject(t, s, "as-idle")
		a := seedRunningAgent(t, s, broker, project, "idle-1", string(state.ActivityIdle), idleSince)

		ok := srv.autoSuspendAgent(ctx, disp, a, state.SuspendReasonIdle)
		require.True(t, ok)

		got, err := s.GetAgentSuspendScreen(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "idle output\n", got.Content)

		// Hub->user conversion ("Keep suspended") preserves the existing row
		// without issuing another capture.
		callsBefore := disp.tmuxCalls
		disp.setTmux("should-not-replace\n", 0, nil)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, callsBefore, disp.tmuxCalls)

		still, err := s.GetAgentSuspendScreen(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "idle output\n", still.Content)
	})

	t.Run("idle auto-suspend refused stop leaves existing row untouched", func(t *testing.T) {
		disp := &suspendScreenDispatcher{
			tmuxOutput: "new capture\n",
			stopErr:    &brokerStatusError{StatusCode: http.StatusBadRequest, Body: `{"error":{"code":"bad_request"}}`},
		}
		srv, s := testServer(t)
		srv.SetDispatcher(disp)
		enableIdle(t, srv)
		broker, project := newQuotaTestBrokerAndProject(t, s, "as-refused")
		a := seedRunningAgent(t, s, broker, project, "refused-1", string(state.ActivityIdle), idleSince)

		// Seed an older screen row.
		require.NoError(t, s.PutAgentSuspendScreen(ctx, &store.AgentSuspendScreen{
			AgentID:    a.ID,
			Content:    "prior screen\n",
			CapturedAt: time.Now().UTC().Add(-time.Hour),
		}))

		ok := srv.autoSuspendAgent(ctx, disp, a, state.SuspendReasonIdle)
		require.False(t, ok)
		assert.Equal(t, 1, disp.tmuxCalls, "probe ran before refused stop")

		got, err := s.GetAgentSuspendScreen(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "prior screen\n", got.Content)
	})

	t.Run("idle auto-suspend unknown outcome persists captured screen now", func(t *testing.T) {
		disp := &suspendScreenDispatcher{
			tmuxOutput: "unknown-outcome capture\n",
			stopErr:    errors.New("broker connection reset"),
		}
		srv, s := testServer(t)
		srv.SetDispatcher(disp)
		enableIdle(t, srv)
		broker, project := newQuotaTestBrokerAndProject(t, s, "as-unknown")
		a := seedRunningAgent(t, s, broker, project, "unknown-1", string(state.ActivityIdle), idleSince)

		ok := srv.autoSuspendAgent(ctx, disp, a, state.SuspendReasonIdle)
		require.False(t, ok)

		got, err := s.GetAgentSuspendScreen(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "unknown-outcome capture\n", got.Content)
	})

	t.Run("membership-loss hold suspend does not capture screen", func(t *testing.T) {
		disp := &suspendScreenDispatcher{tmuxOutput: "held secret output\n"}
		srv, s := testServer(t)
		srv.SetDispatcher(disp)
		broker, project := newQuotaTestBrokerAndProject(t, s, "as-held")
		a := seedRunningAgent(t, s, broker, project, "held-1", string(state.ActivityIdle), time.Now().Add(-time.Minute))

		err := srv.stopHeldAgent(ctx, a.ID, 1)
		require.NoError(t, err)
		assert.Zero(t, disp.tmuxCalls)

		_, err = s.GetAgentSuspendScreen(ctx, a.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestSuspendScreen_ShellAgentNeverCapturedOrServed(t *testing.T) {
	ctx := context.Background()
	disp := &suspendScreenDispatcher{tmuxOutput: "ghp_secret_shell_token\n"}
	srv, s := testServer(t)
	srv.SetDispatcher(disp)
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.SuspendScreen: true,
		experiments.IdleSuspend:   true,
	})
	broker, project := newQuotaTestBrokerAndProject(t, s, "screen-shell")

	owner := &store.User{
		ID:          tid("screen-shell-owner"),
		Email:       "screen-shell-owner@test.com",
		DisplayName: "screen-shell-owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	shellAgent := &store.Agent{
		ID:              tid("agent-screen-shell-1"),
		Slug:            "screen-shell-1",
		Name:            "screen-shell-1",
		ProjectID:       project.ID,
		OwnerID:         owner.ID,
		CreatedBy:       owner.ID,
		RuntimeBrokerID: broker.ID,
		Runtime:         "docker",
		Phase:           string(state.PhaseRunning),
		HarnessConfig:   config.ShellHarnessConfigName,
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: config.ShellHarnessConfigName,
		},
	}
	require.NoError(t, s.CreateAgent(ctx, shellAgent))

	// Direct capture must return nil without executing tmux capture-pane.
	assert.Nil(t, srv.captureSuspendScreen(ctx, disp, shellAgent))
	assert.Zero(t, disp.tmuxCalls, "captureSuspendScreen must not exec into an execution container")

	// Even if a pre-existing screen row is present in the store, neither read
	// route may serve it for an execution container.
	require.NoError(t, s.PutAgentSuspendScreen(ctx, &store.AgentSuspendScreen{
		AgentID:    shellAgent.ID,
		Content:    "stale secret output\n",
		CapturedAt: time.Now().UTC(),
	}))
	for _, path := range []string{
		"/api/v1/agents/" + shellAgent.ID + "/screen",
		"/api/v1/projects/" + project.ID + "/agents/" + shellAgent.Slug + "/screen",
	} {
		rec := doRequestAsUser(t, srv, owner, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "stored screen must not be served for execution container on %s: %s", path, rec.Body.String())
	}

	// Manual suspend must not capture the pane and must clear any stale row.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+shellAgent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Zero(t, disp.tmuxCalls, "manual suspend must not exec tmux capture-pane for execution container")
	_, err = s.GetAgentSuspendScreen(ctx, shellAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "manual suspend should clear any stale screen row")

	// Idle auto-suspend must also skip capture for execution containers.
	srv.mu.Lock()
	srv.config.AutoSuspendIdle = true
	srv.config.IdleSuspendAfter = MinIdleSuspendAfter
	srv.mu.Unlock()

	idleShell := &store.Agent{
		ID:              tid("agent-screen-shell-2"),
		Slug:            "screen-shell-2",
		Name:            "screen-shell-2",
		ProjectID:       project.ID,
		OwnerID:         owner.ID,
		CreatedBy:       owner.ID,
		RuntimeBrokerID: broker.ID,
		Runtime:         "docker",
		Phase:           string(state.PhaseRunning),
		HarnessConfig:   config.ShellHarnessConfigName,
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: config.ShellHarnessConfigName,
		},
	}
	require.NoError(t, s.CreateAgent(ctx, idleShell))
	_, err = s.SetRunIntent(ctx, idleShell.ID, store.RunIntentRunning)
	require.NoError(t, err)
	db := s.(*entadapter.CompositeStore).DB()
	_, err = db.ExecContext(ctx,
		"UPDATE agents SET run_id = ?, activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
		"run-screen-shell-2", string(state.ActivityIdle), time.Now().Add(-2*DefaultIdleSuspendAfter).UTC(), time.Now().UTC(), idleShell.ID,
	)
	require.NoError(t, err)
	idleShell, err = s.GetAgent(ctx, idleShell.ID)
	require.NoError(t, err)

	ok := srv.autoSuspendAgent(ctx, disp, idleShell, state.SuspendReasonIdle)
	require.True(t, ok)
	assert.Zero(t, disp.tmuxCalls, "idle auto-suspend must not exec tmux capture-pane for execution container")
	_, err = s.GetAgentSuspendScreen(ctx, idleShell.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}
