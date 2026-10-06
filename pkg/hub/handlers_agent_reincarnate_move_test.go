//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moveFixture is a clone-per-agent Kubernetes agent on src, with dst a
// second broker on the same NFS export. Both advertise AgentMove unless a
// test downgrades them.
type moveFixture struct {
	srv     *Server
	s       store.Store
	disp    *reincarnateTestDispatcher
	project *store.Project
	src     *store.RuntimeBroker
	dst     *store.RuntimeBroker
	agent   *store.Agent
}

func moveFixtureStorage() *api.BrokerWorkspaceStorage {
	return &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS: &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/scion-workspaces", SubPathRoot: "projects", Healthy: true,
			ExportID: moveTestExportID},
	}
}

func moveFixtureProfiles() []store.BrokerProfile {
	return []store.BrokerProfile{{Name: "k8s", Type: "kubernetes", Available: true}}
}

// setupMoveFixture builds the fixture. dstIsProvider links dst to the
// project; mutate adjusts dst before it is stored.
func setupMoveFixture(t *testing.T, dstIsProvider bool, mutate func(dst *store.RuntimeBroker)) *moveFixture {
	t.Helper()
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, src := setupReincarnateTestServer(t, disp)

	src.WorkspaceStorage = moveFixtureStorage()
	src.Capabilities = &store.BrokerCapabilities{Reprovision: true, AgentMove: true}
	src.Profiles = moveFixtureProfiles()
	src.DefaultProfile = "k8s"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, src))

	dst := &store.RuntimeBroker{
		ID:               tid("move-dst-" + t.Name()),
		Name:             "move-dst",
		Slug:             "move-dst-" + tidSlugSafe(t.Name()),
		Status:           store.BrokerStatusOnline,
		Capabilities:     &store.BrokerCapabilities{Reprovision: true, AgentMove: true},
		WorkspaceStorage: moveFixtureStorage(),
		Profiles:         moveFixtureProfiles(),
		DefaultProfile:   "k8s",
	}
	if mutate != nil {
		mutate(dst)
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, dst))
	if dstIsProvider {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: project.ID, BrokerID: dst.ID, BrokerName: dst.Name, Status: store.BrokerStatusOnline,
		}))
	}

	agent := newReincarnateTestAgent(t, s, project, src, func(a *store.Agent) {
		a.Runtime = "kubernetes"
	})
	require.Equal(t, src.ID, agent.RuntimeBrokerID, "fixture agent runs on src")
	// The agent's last start placed its workspace on the export.
	require.NoError(t, s.SetAgentWorkspacePlacement(ctx, agent.ID, api.WorkspacePlacementExport))
	agent.WorkspacePlacement = api.WorkspacePlacementExport
	return &moveFixture{srv: srv, s: s, disp: disp, project: project, src: src, dst: dst, agent: agent}
}

// moveCaller is the agent itself with agent-create scope (needed to
// dispatch to another broker).
func (f *moveFixture) moveCaller() AgentIdentity {
	return agentIdentityFor(f.agent.ID, f.project.ID, ScopeAgentCreate)
}

func (f *moveFixture) reincarnate(t *testing.T, body ReincarnateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	req := reincarnateRequest(t, f.agent.ID, f.moveCaller(), body)
	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, req, f.agent.ID)
	return rec
}

// assertNoMoveSideEffects checks that nothing a move would write was
// written: the agent row, its reincarnation history, provider links, the
// project's default broker, the dispatcher, and the agent count.
func (f *moveFixture) assertNoMoveSideEffects(t *testing.T, agentCount int) {
	t.Helper()
	ctx := context.Background()
	after, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, f.agent.StateVersion, after.StateVersion, "agent row must be untouched")
	assert.Equal(t, f.agent.RuntimeBrokerID, after.RuntimeBrokerID, "agent must stay on its broker")
	assert.Equal(t, 1, after.Generation)
	assert.Equal(t, "", after.ReincarnationState)

	list, err := f.s.ListAgentReincarnations(ctx, f.agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "no reincarnation record")

	project, err := f.s.GetProject(ctx, f.project.ID)
	require.NoError(t, err)
	assert.Equal(t, f.src.ID, project.DefaultRuntimeBrokerID, "project default broker unchanged")

	n, err := f.s.CountAgents(ctx, store.AgentFilter{})
	require.NoError(t, err)
	assert.Equal(t, agentCount, n, "no agent row created")

	assert.Zero(t, f.disp.stopCalls)
	assert.Zero(t, f.disp.reprovisionCalls)
	assert.Zero(t, f.disp.startCalls)
}

func (f *moveFixture) agentCount(t *testing.T) int {
	t.Helper()
	n, err := f.s.CountAgents(context.Background(), store.AgentFilter{})
	require.NoError(t, err)
	return n
}

// decodeMoveRefusal decodes an error response and its verdict.
func decodeMoveRefusal(t *testing.T, rec *httptest.ResponseRecorder) (code, message string, v MoveVerdict) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Verdict *MoveVerdict `json:"verdict"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.NotNil(t, body.Error.Details.Verdict, "refusal must carry the verdict: %s", rec.Body.String())
	return body.Error.Code, body.Error.Message, *body.Error.Details.Verdict
}

// assertVerdictFailedAt checks every check before failed passed, failed
// failed, and every later one was not evaluated.
func assertVerdictFailedAt(t *testing.T, v MoveVerdict, failed string) {
	t.Helper()
	require.Len(t, v.Checks, len(moveCheckOrder))
	seen := false
	for i, c := range v.Checks {
		require.Equal(t, moveCheckOrder[i], c.Name)
		switch {
		case c.Name == failed:
			seen = true
			assert.Equal(t, MoveCheckFailed, c.Result, c.Name)
		case seen:
			assert.Equal(t, MoveCheckNotEvaluated, c.Result, c.Name)
		default:
			assert.Equal(t, MoveCheckPassed, c.Result, c.Name)
		}
	}
	require.True(t, seen, "check %s not in verdict", failed)
	assert.False(t, v.Eligible)
}

func TestReincarnateMove_UnknownTarget_Returns404(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: "no-such-broker"})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeRuntimeBrokerNotFound, body.Error.Code)
	assert.Equal(t, "no-such-broker", body.Error.Details["requestedBroker"])
	assert.NotEmpty(t, body.Error.Details["availableBrokers"])

	// Unknown target is also 404 (not 501) without --dry-run.
	rec = f.reincarnate(t, ReincarnateAgentRequest{TargetBroker: "no-such-broker"})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	f.assertNoMoveSideEffects(t, count)
}

func TestReincarnateMove_TargetResolvesByIDNameAndSlug(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	for _, target := range []string{f.dst.ID, f.dst.Name, f.dst.Slug} {
		rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: target})
		require.Equal(t, http.StatusOK, rec.Code, "target %q: %s", target, rec.Body.String())
		var resp ReincarnateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, f.dst.ID, resp.TargetBrokerID, "target %q", target)
	}
}

func TestReincarnateMove_SameBrokerTarget_IsPlainReincarnate(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.src.Slug})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "planned", resp.State)
	assert.Nil(t, resp.MoveVerdict, "same broker is not a move")
	assert.Equal(t, f.src.ID, resp.SourceBrokerID)
	assert.Equal(t, f.src.ID, resp.TargetBrokerID)
}

// The phase-1 live positive case: real brokers advertise AgentMove=false,
// so a dry run on an otherwise eligible pair stops at the capability check
// with every earlier check passed.
func TestReincarnateMove_DryRun_NoAgentMove_Returns412AtCapability(t *testing.T) {
	f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
		dst.Capabilities = &store.BrokerCapabilities{Reprovision: true, AgentMove: false}
	})
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.Name})
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	code, msg, v := decodeMoveRefusal(t, rec)
	assert.Equal(t, ErrCodeUnsupportedCapability, code)
	assert.Contains(t, msg, "does not support agent move")
	assertVerdictFailedAt(t, v, moveCheckCapability)
	assert.Equal(t, f.src.ID, v.SourceBroker.ID)
	assert.Equal(t, f.dst.ID, v.TargetBroker.ID)
	assert.Equal(t, "k8s", v.Profile)
	f.assertNoMoveSideEffects(t, count)
}

func TestReincarnateMove_DryRun_FullPass_Returns200WithVerdict(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "planned", resp.State)
	assert.Equal(t, 2, resp.Generation)
	assert.Equal(t, f.src.ID, resp.SourceBrokerID)
	assert.Equal(t, f.dst.ID, resp.TargetBrokerID)
	require.NotNil(t, resp.MoveVerdict)
	assert.True(t, resp.MoveVerdict.Eligible)
	assert.Equal(t, "kubernetes", resp.MoveVerdict.RuntimeType)
	require.Len(t, resp.MoveVerdict.Checks, len(moveCheckOrder))
	for _, c := range resp.MoveVerdict.Checks {
		assert.Equal(t, MoveCheckPassed, c.Result, c.Name)
	}
	f.assertNoMoveSideEffects(t, count)
}

// A dry run against a broker that is not yet a provider for the project
// must not link it, even when every check passes.
func TestReincarnateMove_DryRun_DoesNotLinkTargetAsProvider(t *testing.T) {
	f := setupMoveFixture(t, false, func(dst *store.RuntimeBroker) {
		dst.AutoProvide = true
	})
	ctx := context.Background()
	count := f.agentCount(t)

	// The dev user may link brokers to the project, so the access check
	// passes without the link existing.
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/reincarnate",
		ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.MoveVerdict)
	assert.True(t, resp.MoveVerdict.Eligible)

	_, err := f.s.GetProjectProvider(ctx, f.project.ID, f.dst.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "dry run must not link the target broker to the project")
	f.assertNoMoveSideEffects(t, count)

	// An agent moving itself may only move to a broker that already serves
	// its project, so the same request is refused at the access check with
	// 409, again without a link.
	rec = f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	_, msg, v := decodeMoveRefusal(t, rec)
	assert.Contains(t, msg, "target broker move-dst does not serve this project")
	assertVerdictFailedAt(t, v, moveCheckAccess)
	_, err = f.s.GetProjectProvider(ctx, f.project.ID, f.dst.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	f.assertNoMoveSideEffects(t, count)
}

// An agent not assigned to any broker cannot be moved: --broker is refused
// with a 400 before the target is resolved, with or without dry run.
func TestReincarnateMove_UnassignedAgent_Returns400(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("dryRun=%v", dryRun), func(t *testing.T) {
			f := setupMoveFixture(t, true, nil)
			ctx := context.Background()
			f.agent.RuntimeBrokerID = ""
			require.NoError(t, f.s.UpdateAgent(ctx, f.agent))
			agent, err := f.s.GetAgent(ctx, f.agent.ID)
			require.NoError(t, err)
			require.Empty(t, agent.RuntimeBrokerID)
			f.agent = agent
			count := f.agentCount(t)

			for _, target := range []string{f.dst.ID, "no-such-broker"} {
				rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: dryRun, Handoff: "h", TargetBroker: target})
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				var body ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, ErrCodeValidationError, body.Error.Code)
				assert.Equal(t, "cannot move an agent that is not currently assigned to a broker", body.Error.Message)
			}
			f.assertNoMoveSideEffects(t, count)
		})
	}
}

// An old target broker reports no workspace storage descriptor: a 412 at
// the descriptor check, never a crash.
func TestReincarnateMove_OldBrokerWithoutDescriptor_Returns412(t *testing.T) {
	f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
		dst.WorkspaceStorage = nil
		dst.Capabilities = &store.BrokerCapabilities{Reprovision: true}
	})
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	code, msg, v := decodeMoveRefusal(t, rec)
	assert.Equal(t, ErrCodeUnsupportedCapability, code)
	assert.Contains(t, msg, "does not advertise workspace storage")
	assertVerdictFailedAt(t, v, moveCheckWorkspaceStorage)
	f.assertNoMoveSideEffects(t, count)
}

func TestReincarnateMove_DifferentExport_Returns409(t *testing.T) {
	f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
		dst.WorkspaceStorage.NFS.SubPathRoot = "other-projects"
	})
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	code, _, v := decodeMoveRefusal(t, rec)
	assert.Equal(t, ErrCodeConflict, code)
	assertVerdictFailedAt(t, v, moveCheckSameExport)
}

func TestReincarnateMove_WorktreeProject_RefusedAtWorkspaceMode(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	f.project.GitRemote = "https://example.com/repo.git"
	f.project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent}
	require.NoError(t, f.s.UpdateProject(ctx, f.project))
	require.True(t, f.project.IsWorktreePerAgent(), "fixture check")

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg, v := decodeMoveRefusal(t, rec)
	assert.Equal(t, ErrCodeValidationError, code)
	assert.Contains(t, msg, "worktree-per-agent")
	assertVerdictFailedAt(t, v, moveCheckWorkspaceMode)
}

func TestReincarnateMove_LinkedProject_RefusedAtWorkspaceMode(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.project.ID,
		BrokerID:   f.src.ID,
		BrokerName: f.src.Name,
		LocalPath:  "/home/broker/projects/linked",
		Status:     store.BrokerStatusOnline,
	}))

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	_, msg, v := decodeMoveRefusal(t, rec)
	assert.Contains(t, msg, "linked projects")
	assertVerdictFailedAt(t, v, moveCheckWorkspaceMode)
}

// assertSameAsUnknownTarget checks that a request naming target returns a
// 404 whose body is byte-identical to one naming an unknown broker, apart
// from the echoed target string.
func assertSameAsUnknownTarget(t *testing.T, do func(body ReincarnateAgentRequest) *httptest.ResponseRecorder, target string) {
	t.Helper()
	const unknown = "no-such-broker-xyz"
	for _, dryRun := range []bool{true, false} {
		got := do(ReincarnateAgentRequest{DryRun: dryRun, TargetBroker: target})
		want := do(ReincarnateAgentRequest{DryRun: dryRun, TargetBroker: unknown})
		require.Equal(t, http.StatusNotFound, got.Code, "dryRun=%v: %s", dryRun, got.Body.String())
		require.Equal(t, http.StatusNotFound, want.Code, "dryRun=%v: %s", dryRun, want.Body.String())
		assert.Equal(t, want.Body.String(), strings.ReplaceAll(got.Body.String(), target, unknown),
			"dryRun=%v: a not-visible target must be indistinguishable from an unknown one", dryRun)
	}
}

// An agent caller that cannot dispatch to the target and whose project the
// target does not serve cannot learn that the target exists.
func TestReincarnateMove_TargetNotVisibleToAgent_Returns404LikeUnknown(t *testing.T) {
	f := setupMoveFixture(t, false, nil)
	count := f.agentCount(t)
	do := func(body ReincarnateAgentRequest) *httptest.ResponseRecorder { return f.reincarnate(t, body) }
	for _, target := range []string{f.dst.ID, f.dst.Name, f.dst.Slug} {
		assertSameAsUnknownTarget(t, do, target)
	}
	f.assertNoMoveSideEffects(t, count)
}

// A user who may manage the agent but cannot read the target broker gets
// the unknown-broker 404; once the user can read it the request reaches
// the eligibility checks.
func TestReincarnateMove_TargetNotReadableByUser_Returns404LikeUnknown(t *testing.T) {
	f := setupMoveFixture(t, false, nil)
	user, do := f.unprivilegedUser(t)
	count := f.agentCount(t)

	// The user may reincarnate the agent in place.
	rec := do(ReincarnateAgentRequest{DryRun: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assertSameAsUnknownTarget(t, do, f.dst.ID)
	f.assertNoMoveSideEffects(t, count)

	// Hub members can read brokers, so the target resolves.
	ensureHubMembership(context.Background(), f.s, user.ID)
	rec = do(ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	assert.NotEqual(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// unprivilegedUser makes the agent's owner a user with no broker rights and
// only the project member role, and returns a request func acting as that
// user.
func (f *moveFixture) unprivilegedUser(t *testing.T) (*store.User, func(ReincarnateAgentRequest) *httptest.ResponseRecorder) {
	t.Helper()
	ctx := context.Background()
	user := &store.User{
		ID: tid("move-user-" + t.Name()), Email: "move-user@example.com", DisplayName: "Move User",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.s.CreateUser(ctx, user))
	// Reincarnating the agent records the user as its delegator, which
	// needs agent.create in the project. The project member role grants
	// it and no broker read.
	createTestUserWithProjectRole(t, f.s, user.ID, user.Email, f.project.ID, store.ProjectRoleMember)
	f.agent.OwnerID = user.ID
	f.agent.CreatedBy = user.ID
	require.NoError(t, f.s.UpdateAgent(ctx, f.agent))
	agent, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	f.agent = agent

	caller := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeWeb))
	return user, func(body ReincarnateAgentRequest) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.agent.ID, caller, body), f.agent.ID)
		return rec
	}
}

// A user who cannot read the agent's current broker may still name it: that
// is a plain reincarnate, and the broker is already in the agent record.
func TestReincarnateMove_CurrentBrokerVisibleToUserWithoutRead(t *testing.T) {
	f := setupMoveFixture(t, false, nil)
	// Unlink the source so only the current-broker exemption makes it
	// visible (a project provider is visible on its own).
	require.NoError(t, f.s.RemoveProjectProvider(context.Background(), f.project.ID, f.src.ID))
	_, do := f.unprivilegedUser(t)
	rec := do(ReincarnateAgentRequest{DryRun: true, TargetBroker: f.src.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp.MoveVerdict, "same broker is not a move")
	assert.Equal(t, f.src.ID, resp.TargetBrokerID)
}

// The current-broker exemption also applies when the broker is named, not
// identified: a user without broker read who names the agent's current
// broker gets a plain reincarnate, even when a hidden broker shares that
// name (case-insensitively).
func TestReincarnateMove_CurrentBrokerByNameVisibleWithHiddenTwin(t *testing.T) {
	f := setupMoveFixture(t, false, nil)
	require.NoError(t, f.s.RemoveProjectProvider(context.Background(), f.project.ID, f.src.ID))
	f.addMoveBroker(t, "twin-of-src-", strings.ToUpper(f.src.Name), "twin-of-src", false)
	_, do := f.unprivilegedUser(t)
	rec := do(ReincarnateAgentRequest{DryRun: true, TargetBroker: f.src.Name})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp.MoveVerdict, "same broker is not a move")
	assert.Equal(t, f.src.ID, resp.TargetBrokerID)
}

// An auto-providing broker is visible to a caller with no other rights:
// the request reaches the checks (and stops at access, since the user may
// not link brokers to the project).
func TestReincarnateMove_AutoProvideTargetVisibleWithoutRights(t *testing.T) {
	f := setupMoveFixture(t, false, func(dst *store.RuntimeBroker) { dst.AutoProvide = true })
	_, do := f.unprivilegedUser(t)
	rec := do(ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, _, v := decodeMoveRefusal(t, rec)
	assertVerdictFailedAt(t, v, moveCheckAccess)
}

// A broker that serves the agent's project (and so is listed in the
// not-found response) is visible to a user without broker read.
func TestReincarnateMove_ProjectProviderVisibleToUserWithoutRead(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	_, do := f.unprivilegedUser(t)
	rec := do(ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.NotEqual(t, http.StatusNotFound, rec.Code, rec.Body.String())
	_, _, v := decodeMoveRefusal(t, rec)
	assert.Equal(t, f.dst.ID, v.TargetBroker.ID)
}

// An agent without agent-create scope still sees a broker that serves its
// project. Moving itself there needs only its reincarnate rights (A8), so
// the dry run is eligible. Another agent moving it with lifecycle scope but
// no agent-create scope would become its recorded delegator, so the
// delegation authority check refuses it with 403 before any move check
// runs: the refusal carries no verdict and nothing is written.
func TestReincarnateMove_AgentServesProjectWithoutScope(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	count := f.agentCount(t)
	do := func(caller AgentIdentity) *httptest.ResponseRecorder {
		req := reincarnateRequest(t, f.agent.ID, caller, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
		rec := httptest.NewRecorder()
		f.srv.handleReincarnateAgent(rec, req, f.agent.ID)
		return rec
	}

	rec := do(agentIdentityFor(f.agent.ID, f.project.ID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.MoveVerdict)
	assert.True(t, resp.MoveVerdict.Eligible)

	rec = do(agentIdentityFor(tid("coordinator"), f.project.ID, ScopeAgentLifecycle))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Cannot delegate agent authority you do not hold")
	assert.NotContains(t, rec.Body.String(), `"verdict"`, "refused before the move checks")
	f.assertNoMoveSideEffects(t, count)
}

// A passthrough agent cannot move itself, even to a broker serving its
// project: 403 with the "ask a user" message, and nothing written.
func TestReincarnateMove_SelfMovePassthroughRefused(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough}
	require.NoError(t, f.s.UpdateAgent(ctx, a))
	f.agent = a
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, msg, v := decodeMoveRefusal(t, rec)
	assert.Equal(t, "this agent cannot move itself; ask a user to move you", msg)
	assertVerdictFailedAt(t, v, moveCheckAccess)
	f.assertNoMoveSideEffects(t, count)
}

// The verdict reads the placement recorded on the agent row: unknown
// (never reported) and local are refused at workspace_on_export with the
// re-provision hint.
func TestReincarnateMove_PlacementNotExportRefused(t *testing.T) {
	for _, tc := range []struct{ placement, want string }{
		{"", "workspace placement is unknown"},
		{api.WorkspacePlacementLocal, `placement "local"`},
	} {
		t.Run("placement="+tc.placement, func(t *testing.T) {
			f := setupMoveFixture(t, true, nil)
			require.NoError(t, f.s.SetAgentWorkspacePlacement(context.Background(), f.agent.ID, tc.placement))
			count := f.agentCount(t)

			rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			_, msg, v := decodeMoveRefusal(t, rec)
			assert.Contains(t, msg, tc.want)
			assert.Contains(t, msg, "re-provision the agent once")
			assertVerdictFailedAt(t, v, moveCheckWorkspaceOnExport)
			f.assertNoMoveSideEffects(t, count)
		})
	}
}

// Brokers with the same configured export but different export identity
// markers do not share a directory; a broker with no marker (an older
// broker, or an unreadable mount) cannot prove it does.
func TestReincarnateMove_ExportIdentity(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		status int
		want   string
	}{
		{"different marker", "0a0a0a0a-0000-4000-8000-000000000000", http.StatusConflict, "see different export identity markers"},
		{"no marker", "", http.StatusPreconditionFailed, "has not reported the export identity marker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
				dst.WorkspaceStorage.NFS.ExportID = tc.id
			})
			count := f.agentCount(t)
			rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			_, msg, v := decodeMoveRefusal(t, rec)
			assert.Contains(t, msg, tc.want)
			assertVerdictFailedAt(t, v, moveCheckSameExport)
			f.assertNoMoveSideEffects(t, count)
		})
	}
}

// Plugin records are not runtime brokers: a user who can read every broker
// gets the unknown-broker 404 for one, by ID or name.
func TestReincarnateMove_PluginTargetIs404ForUser(t *testing.T) {
	f := setupMoveFixture(t, false, func(dst *store.RuntimeBroker) {
		dst.Labels = map[string]string{"scion.io/plugin": "discord"}
	})
	do := func(body ReincarnateAgentRequest) *httptest.ResponseRecorder {
		return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/reincarnate", body)
	}
	for _, target := range []string{f.dst.ID, f.dst.Name} {
		assertSameAsUnknownTarget(t, do, target)
	}
}

// addMoveBroker stores another move-eligible broker, linked to the project
// (and so visible to the agent caller) when provider is true.
func (f *moveFixture) addMoveBroker(t *testing.T, id, name, slug string, provider bool) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID: tid(id + t.Name()), Name: name, Slug: slug,
		Status: store.BrokerStatusOnline, WorkspaceStorage: moveFixtureStorage(),
		Capabilities: &store.BrokerCapabilities{Reprovision: true, AgentMove: true},
		Profiles:     moveFixtureProfiles(), DefaultProfile: "k8s",
	}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, b))
	if provider {
		require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: f.project.ID, BrokerID: b.ID, BrokerName: b.Name, Status: store.BrokerStatusOnline,
		}))
	}
	return b
}

func (f *moveFixture) assertTargetResolvesTo(t *testing.T, target string, want *store.RuntimeBroker) {
	t.Helper()
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: target})
	require.Equal(t, http.StatusOK, rec.Code, "target %q: %s", target, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, want.ID, resp.TargetBrokerID, "target %q", target)
}

// A hidden broker never shadows a visible one: hidden brokers are filtered
// out before the name or slug is matched. Each case runs with the hidden
// broker created both before and after the visible one, so it is listed
// first in one of the two runs.
func TestReincarnateMove_HiddenBrokerDoesNotShadowVisible(t *testing.T) {
	type spec struct{ id, name, slug string }
	cases := []struct {
		name            string
		hidden, visible spec
		target          string
	}{
		{"same name, different case", spec{"hidden-", "shadow-name", "hidden-slug-x"}, spec{"visible-", "SHADOW-NAME", "visible-slug-x"}, "shadow-name"},
		{"hidden name equals visible slug", spec{"hidden2-", "visible2-slug", "hidden2-slug"}, spec{"visible2-", "Visible Two", "visible2-slug"}, "visible2-slug"},
	}
	for _, tc := range cases {
		for _, hiddenFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/hiddenFirst=%v", tc.name, hiddenFirst), func(t *testing.T) {
				f := setupMoveFixture(t, true, nil)
				var vis *store.RuntimeBroker
				if hiddenFirst {
					f.addMoveBroker(t, tc.hidden.id, tc.hidden.name, tc.hidden.slug, false)
					vis = f.addMoveBroker(t, tc.visible.id, tc.visible.name, tc.visible.slug, true)
				} else {
					vis = f.addMoveBroker(t, tc.visible.id, tc.visible.name, tc.visible.slug, true)
					f.addMoveBroker(t, tc.hidden.id, tc.hidden.name, tc.hidden.slug, false)
				}
				f.assertTargetResolvesTo(t, tc.target, vis)
			})
		}
	}
}

// An exact ID match wins over a broker whose name is that ID.
func TestReincarnateMove_ExactIDWinsOverName(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	f.addMoveBroker(t, "named-like-id-", f.dst.ID, "named-like-id", true)
	f.assertTargetResolvesTo(t, f.dst.ID, f.dst)
}

// An exact ID match on a hidden broker is not a match: resolution falls
// through to names and slugs, so a visible broker whose name is that ID
// string resolves (a direct 404 would reveal the hidden ID exists).
func TestReincarnateMove_HiddenIDFallsThroughToVisibleName(t *testing.T) {
	f := setupMoveFixture(t, false, nil)
	vis := f.addMoveBroker(t, "named-like-hidden-id-", f.dst.ID, "named-like-hidden-id", true)
	f.assertTargetResolvesTo(t, f.dst.ID, vis)
}

// A name or slug matching more than one visible broker is refused with a
// 409 listing only the visible candidates; the caller must use the ID.
func TestReincarnateMove_AmbiguousTarget_Returns409(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	count := f.agentCount(t)
	a := f.addMoveBroker(t, "twin-a-", "Twin", "twin-a", true)
	b := f.addMoveBroker(t, "twin-b-", "twin", "twin-b", true)
	f.addMoveBroker(t, "twin-hidden-", "TWIN", "twin-hidden", false)

	for _, dryRun := range []bool{true, false} {
		rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: dryRun, TargetBroker: "twin"})
		require.Equal(t, http.StatusConflict, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Details struct {
					RequestedBroker string                 `json:"requestedBroker"`
					Candidates      []RuntimeBrokerSummary `json:"candidates"`
				} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, ErrCodeRuntimeBrokerAmbiguous, body.Error.Code)
		assert.Contains(t, body.Error.Message, "use the broker ID")
		assert.Equal(t, "twin", body.Error.Details.RequestedBroker)
		ids := []string{}
		for _, c := range body.Error.Details.Candidates {
			ids = append(ids, c.ID)
		}
		want := []string{a.ID, b.ID}
		sort.Strings(want)
		assert.Equal(t, want, ids, "only visible candidates, sorted by ID")
	}
	f.assertTargetResolvesTo(t, a.ID, a)
	f.assertNoMoveSideEffects(t, count)
}

// A target that is not reachable fails the target health check.
func TestReincarnateMove_TargetOffline_Returns503(t *testing.T) {
	f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
		dst.Status = store.BrokerStatusOffline
	})
	count := f.agentCount(t)
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	code, msg, v := decodeMoveRefusal(t, rec)
	assert.Equal(t, ErrCodeRuntimeBrokerUnavail, code)
	assert.Contains(t, msg, "is unavailable")
	assertVerdictFailedAt(t, v, moveCheckTargetHealth)
	f.assertNoMoveSideEffects(t, count)
}

// A source that is not reachable cannot clean up, so the move is refused
// at the capability check.
func TestReincarnateMove_SourceOffline_Returns412(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	f.src.Status = store.BrokerStatusOffline
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, f.src))
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	code, msg, v := decodeMoveRefusal(t, rec)
	assert.Equal(t, ErrCodeRuntimeBrokerUnavail, code)
	assert.Contains(t, msg, "source broker")
	assertVerdictFailedAt(t, v, moveCheckCapability)
	f.assertNoMoveSideEffects(t, count)
}

// The target's agent limit: at the limit the move is refused at the
// capacity check; one below it passes.
func TestReincarnateMove_TargetCapacity(t *testing.T) {
	const limit = 2
	t.Run("at limit returns 429", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		setBrokerAgentCeiling(t, f.s, limit)
		for i := 0; i < limit; i++ {
			reserveBrokerSlot(t, f.s, f.dst, tid(fmt.Sprintf("occupant-%d", i)))
		}
		count := f.agentCount(t)
		rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		code, msg, v := decodeMoveRefusal(t, rec)
		assert.Equal(t, ErrCodeQuotaExceeded, code)
		assert.Contains(t, msg, "at its agent limit (2 of 2)")
		assertVerdictFailedAt(t, v, moveCheckCapacity)
		assert.EqualValues(t, limit, brokerReservationCount(t, f.s, f.dst.ID), "dry run reserves nothing")
		f.assertNoMoveSideEffects(t, count)
	})
	t.Run("one below limit passes", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		setBrokerAgentCeiling(t, f.s, limit)
		for i := 0; i < limit-1; i++ {
			reserveBrokerSlot(t, f.s, f.dst, tid(fmt.Sprintf("occupant-%d", i)))
		}
		rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ReincarnateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.MoveVerdict)
		assert.True(t, resp.MoveVerdict.Eligible)
		assert.EqualValues(t, limit-1, brokerReservationCount(t, f.s, f.dst.ID), "dry run reserves nothing")
	})
}
