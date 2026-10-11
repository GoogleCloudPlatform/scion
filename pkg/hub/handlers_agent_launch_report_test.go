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

// This file covers the report endpoint's HTTP wiring around
// ApplyLaunchReport (routing, broker identity, and the wire
// status-code/body mapping from store.LaunchReportAnswer). ApplyLaunchReport
// itself is tested in pkg/store/entadapter.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postLaunchReport(t *testing.T, srv *Server, brokerID, agentID, identityBrokerID string, report AgentLaunchReport) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(report)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/agents/"+agentID+"/launch", bytes.NewReader(body))
	if identityBrokerID != "" {
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(identityBrokerID)))
	}
	rr := httptest.NewRecorder()
	srv.handleRuntimeBrokerRoutes(rr, req)
	return rr
}

// postLaunchReportWithIdentity is like postLaunchReport but carries a
// non-broker Identity (a user or agent token) instead of a broker identity,
// for H-1 cases where the caller authenticated as something other than a
// broker.
func postLaunchReportWithIdentity(t *testing.T, srv *Server, brokerID, agentID string, identity Identity, report AgentLaunchReport) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(report)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/agents/"+agentID+"/launch", bytes.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rr := httptest.NewRecorder()
	srv.handleRuntimeBrokerRoutes(rr, req)
	return rr
}

func TestAgentLaunchReport_RoutingAndBrokerIdentity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-project"), Slug: "lr-project", Name: "LR Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-agent"), Slug: "lr-agent", Name: "LR Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	t.Run("no broker identity is forbidden", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "", AgentLaunchReport{LaunchID: launchID, InstanceID: "i1", State: "progress"})
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("wrong broker identity is forbidden", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-2", AgentLaunchReport{LaunchID: launchID, InstanceID: "i1", State: "progress"})
		assert.Equal(t, http.StatusForbidden, rec.Code)
		// Pins the documented contract (hub-api.md §5.8): a 403 carries the
		// standard API error body, not the endpoint's own {code,reason}
		// shape or an empty body.
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
	})

	t.Run("a user identity is forbidden", func(t *testing.T) {
		user := NewAuthenticatedUser(tid("lr-user"), "user@example.com", "User", "member", "cli")
		rec := postLaunchReportWithIdentity(t, srv, "broker-1", agent.ID, user, AgentLaunchReport{LaunchID: launchID, InstanceID: "i1", State: "progress"})
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("an agent identity is forbidden", func(t *testing.T) {
		callerAgent := &store.Agent{ID: agent.ID, ProjectID: project.ID}
		rec := postLaunchReportWithIdentity(t, srv, "broker-1", agent.ID, newAgentIdentityFromStore(callerAgent), AgentLaunchReport{LaunchID: launchID, InstanceID: "i1", State: "progress"})
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("a broker identity matching the path but not the agent's own broker gets 403 without the launch-report body shape", func(t *testing.T) {
		// The broker identity ("broker-2") matches the path's brokerId, so it
		// passes the handler's own broker-identity check, but the agent row
		// names a different broker ("broker-reassigned"), which the store's
		// broker-ownership check in ApplyLaunchReport rejects.
		otherAgent := &store.Agent{
			ID: tid("lr-other-broker-agent"), Slug: "lr-other-broker-agent", Name: "LR Other Broker Agent",
			ProjectID: project.ID, Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-reassigned", StateVersion: 1,
			Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, otherAgent))
		otherLaunchID, err := s.BeginLaunch(ctx, otherAgent.ID, store.LaunchKindCreate, 5*time.Minute)
		require.NoError(t, err)

		rec := postLaunchReport(t, srv, "broker-2", otherAgent.ID, "broker-2", AgentLaunchReport{
			LaunchID: otherLaunchID, InstanceID: "i1", State: "progress",
		})
		assert.Equal(t, http.StatusForbidden, rec.Code)
		// A store-side 403 (ApplyLaunchReport's own broker-ownership check)
		// goes through the same Forbidden(w) helper as the handler-level
		// identity check above, and so carries the standard API error body
		// (ErrorResponse{Error: APIError{Code: "forbidden", ...}}), not the
		// endpoint's own {code,reason} agentLaunchReportErrorResponse shape
		// used for 404/409. Decoding into ErrorResponse and asserting the
		// code (rather than decoding into agentLaunchReportErrorResponse and
		// asserting it's empty) actually pins this: the latter would also
		// pass if the store-side 403 mistakenly used the endpoint's own
		// {code,reason} shape with an empty Code.
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
	})

	t.Run("progress report applies and returns 200", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Step: "cloning",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp agentLaunchReportAppliedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.LaunchReportResultApplied, resp.Result)
	})

	t.Run("superseded launch id gets 409 stale_launch", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
			LaunchID: "some-other-launch", InstanceID: "i1", State: "progress",
		})
		require.Equal(t, http.StatusConflict, rec.Code)
		var resp agentLaunchReportErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.LaunchReportCodeStaleLaunch, resp.Code)
		assert.Equal(t, store.LaunchReportReasonSuperseded, resp.Reason)
	})

	t.Run("unknown agent gets 404", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", "00000000-0000-0000-0000-000000000000", "broker-1", AgentLaunchReport{
			LaunchID: "L", InstanceID: "i1", State: "progress",
		})
		require.Equal(t, http.StatusNotFound, rec.Code)
		var resp agentLaunchReportErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.LaunchReportCodeUnknownLaunch, resp.Code)
	})

	t.Run("GET is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers/broker-1/agents/"+agent.ID+"/launch", nil)
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("broker-1")))
		rec := httptest.NewRecorder()
		srv.handleRuntimeBrokerRoutes(rec, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})
}

func TestAgentLaunchReport_TerminalPublishesStatus(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-pub-project"), Slug: "lr-pub-project", Name: "LR Pub Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-pub-agent"), Slug: "lr-pub-agent", Name: "LR Pub Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	events := NewChannelEventPublisher()
	defer events.Close()
	srv.events = events
	statusCh, unsub := events.Subscribe("agent." + agent.ID + ".status")
	defer unsub()

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "succeeded",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	select {
	case evt := <-statusCh:
		var data AgentStatusEvent
		require.NoError(t, json.Unmarshal(evt.Data, &data))
		assert.Equal(t, "running", data.Phase)
		if assert.NotNil(t, data.Launch, "the published event must carry the launch's terminal state") {
			assert.Equal(t, launchID, data.Launch.ID)
			assert.Equal(t, store.LaunchStateEnded, data.Launch.State)
			assert.False(t, data.Launch.Active)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for the terminal report to publish a status event")
	}
}

func TestAgentLaunchReport_KeepaliveDoesNotPublish(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-keepalive-project"), Slug: "lr-keepalive-project", Name: "LR Keepalive Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-keepalive-agent"), Slug: "lr-keepalive-agent", Name: "LR Keepalive Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	events := NewChannelEventPublisher()
	defer events.Close()
	srv.events = events
	statusCh, unsub := events.Subscribe("agent." + agent.ID + ".status")
	defer unsub()

	// Seq 1 with a step first, so seq 1 again below is a genuine duplicate.
	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Step: "cloning",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	select {
	case <-statusCh:
	case <-time.After(time.Second):
		t.Fatal("expected the first progress report to publish")
	}

	rec = postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Step: "cloning",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	select {
	case evt := <-statusCh:
		t.Fatalf("a duplicate report must not publish, got %s", evt.Data)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAgentLaunchReport_NewSeqKeepaliveDoesNotPublish covers the other half
// of design §3.7's keepalive rule: a report with a NEW seq (not a
// duplicate) that changes none of step, phase or message is still a
// keepalive — Result "applied", but Changed false, so it must not publish.
// This is distinct from TestAgentLaunchReport_KeepaliveDoesNotPublish, which
// covers the seq<=launch_seq duplicate path (Result "duplicate").
func TestAgentLaunchReport_NewSeqKeepaliveDoesNotPublish(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-newseq-project"), Slug: "lr-newseq-project", Name: "LR NewSeq Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-newseq-agent"), Slug: "lr-newseq-agent", Name: "LR NewSeq Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	events := NewChannelEventPublisher()
	defer events.Close()
	srv.events = events
	statusCh, unsub := events.Subscribe("agent." + agent.ID + ".status")
	defer unsub()

	// Seq 1 sets step "cloning" and publishes.
	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Step: "cloning",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	select {
	case <-statusCh:
	case <-time.After(time.Second):
		t.Fatal("expected the first progress report to publish")
	}

	// Seq 2 is a genuinely new sequence number (not a duplicate of seq 1),
	// but repeats the same step and carries no phase/message change — a
	// plain keepalive per §3.7.
	rec = postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 2, State: "progress", Step: "cloning",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp agentLaunchReportAppliedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, store.LaunchReportResultApplied, resp.Result, "a new-seq keepalive is still \"applied\", not \"duplicate\"")

	select {
	case evt := <-statusCh:
		t.Fatalf("a new-seq keepalive with no field change must not publish, got %s", evt.Data)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAgentLaunchReport_MessageIsTruncated verifies a broker-supplied
// Message is bounded to the same 512-byte limit message-failures uses,
// before it is stored and (elsewhere) republished to SSE subscribers.
func TestAgentLaunchReport_MessageIsTruncated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-msg-project"), Slug: "lr-msg-project", Name: "LR Msg Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-msg-agent"), Slug: "lr-msg-agent", Name: "LR Msg Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	longMessage := strings.Repeat("m", 10000)
	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Step: "cloning", Message: longMessage,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(after.Message), 512, "the stored message must be bounded to 512 bytes")
	assert.NotEqual(t, longMessage, after.Message)
}

// TestAgentLaunchReport_MessageStripsDisplayUnsafeRunes verifies the
// launch-report reuse of sanitizeFailureReason also removes format
// characters (here a bidi override and a zero-width space) and controls
// from the stored Message.
func TestAgentLaunchReport_MessageStripsDisplayUnsafeRunes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-cf-project"), Slug: "lr-cf-project", Name: "LR Cf Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-cf-agent"), Slug: "lr-cf-agent", Name: "LR Cf Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Step: "cloning",
		Message: "clone\u202edone\u200b \x1b[2Jok",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "clonedone [2Jok", after.Message)
}

// TestAgentLaunchReport_StepAndErrorCodeAreTruncated verifies the same
// 512-byte cap applies to Step and ErrorCode, not just Message: a `failed`
// report's Step becomes the stored LaunchStep-derived Message
// (formatFailureMessage), and its ErrorCode becomes the stored LaunchError.
func TestAgentLaunchReport_StepAndErrorCodeAreTruncated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-step-err-project"), Slug: "lr-step-err-project", Name: "LR Step Err Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-step-err-agent"), Slug: "lr-step-err-agent", Name: "LR Step Err Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	longStep := strings.Repeat("s", 10000)
	longErrorCode := strings.Repeat("e", 10000)
	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "failed", Step: longStep, ErrorCode: longErrorCode,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(after.Message), 512, "the stored message (from Step) must be bounded to 512 bytes")
	assert.NotEqual(t, longStep, after.Message)
	assert.LessOrEqual(t, len(after.LaunchError), 512, "the stored launch error (from ErrorCode) must be bounded to 512 bytes")
	assert.NotEqual(t, longErrorCode, after.LaunchError)
}

// TestAgentLaunchReport_OversizedInstanceIDRejected and
// TestAgentLaunchReport_OversizedPhaseRejected cover the identifier fields:
// InstanceID and Phase are matched exactly downstream, so an oversized or
// control-character value must be rejected with 400, not silently
// normalized the way Step/Message/ErrorCode are.
func TestAgentLaunchReport_OversizedInstanceIDRejected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-badiid-project"), Slug: "lr-badiid-project", Name: "LR BadIID Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-badiid-agent"), Slug: "lr-badiid-agent", Name: "LR BadIID Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	t.Run("too long", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: strings.Repeat("i", 257), Seq: 1, State: "progress",
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("at the boundary is accepted", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: strings.Repeat("i", 256), Seq: 1, State: "progress",
		})
		assert.NotEqual(t, http.StatusBadRequest, rec.Code, "a 256-byte instanceId must not be rejected: %s", rec.Body.String())
	})

	t.Run("control character", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1\x00", Seq: 1, State: "progress",
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

func TestAgentLaunchReport_OversizedPhaseRejected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-badphase-project"), Slug: "lr-badphase-project", Name: "LR BadPhase Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-badphase-agent"), Slug: "lr-badphase-agent", Name: "LR BadPhase Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	t.Run("too long", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Phase: strings.Repeat("p", 65),
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("at the boundary is accepted", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Phase: strings.Repeat("p", 64),
		})
		assert.NotEqual(t, http.StatusBadRequest, rec.Code, "a 64-byte phase must not be rejected: %s", rec.Body.String())
	})

	t.Run("control character", func(t *testing.T) {
		rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Phase: "provisioning\n",
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// TestAgentLaunchReport_ValidPhaseMovesAgent verifies a legitimate phase
// value goes through the HTTP layer and actually moves the agent, not just
// past the instanceId/phase validation: a cap tight enough to reject a real
// phase like "provisioning" would still pass every other test in this file,
// since none of them send a phase that also needs to succeed.
func TestAgentLaunchReport_ValidPhaseMovesAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-validphase-project"), Slug: "lr-validphase-project", Name: "LR ValidPhase Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-validphase-agent"), Slug: "lr-validphase-agent", Name: "LR ValidPhase Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: "progress", Phase: "provisioning",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "provisioning", after.Phase)
}

// TestAgentLaunchReport_OversizedBodyRejected verifies the http.MaxBytesReader
// wrap: a body larger than the 64 KiB limit must not be fully buffered and
// decoded, and must fail cleanly rather than succeed with a truncated
// message.
func TestAgentLaunchReport_OversizedBodyRejected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-oversized-project"), Slug: "lr-oversized-project", Name: "LR Oversized Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-oversized-agent"), Slug: "lr-oversized-agent", Name: "LR Oversized Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "progress", Message: strings.Repeat("x", 100*1024),
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestAgentLaunchReport_AgentIDWithSlashIsNotFound verifies an agent id
// containing an extra path segment must be rejected at routing time with
// 404, matching the sibling path-segment routing convention, rather than
// reaching the handler and failing some other validation (e.g. a 400 for an
// invalid UUID).
func TestAgentLaunchReport_AgentIDWithSlashIsNotFound(t *testing.T) {
	srv, _ := testServer(t)
	rec := postLaunchReport(t, srv, "broker-1", "x/y", "broker-1", AgentLaunchReport{
		LaunchID: "L", InstanceID: "i1", State: "progress",
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
