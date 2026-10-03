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
	"net/http"
	"net/http/httptest"
	"testing"

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
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/scion-workspaces", SubPathRoot: "projects", Healthy: true},
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
	assert.Equal(t, f.src.ID, after.RuntimeBrokerID, "agent must stay on its broker")
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

	// An agent caller cannot link brokers, so the same request is refused
	// at the access check, again without a link.
	rec = f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, _, v := decodeMoveRefusal(t, rec)
	assertVerdictFailedAt(t, v, moveCheckAccess)
	_, err = f.s.GetProjectProvider(ctx, f.project.ID, f.dst.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestReincarnateMove_NonDryRun_Returns501AndAgentUntouched(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h", TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeNotImplemented, body.Error.Code)
	f.assertNoMoveSideEffects(t, count)
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
