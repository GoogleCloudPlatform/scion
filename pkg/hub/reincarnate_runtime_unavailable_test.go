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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupHTTPReincarnateStartFailure wires a full HTTPAgentDispatcher (stop,
// reprovision, start, and best-effort re-render) with a minting token
// generator, so a reincarnation exercises both DispatchAgentStart's beginRun /
// settleFailedRun (shouldRevertRun) and runReincarnation's
// reincarnationStartLeftNoContainer / failAfterReprovision path end-to-end.
func setupHTTPReincarnateStartFailure(t *testing.T, startErr error) (*Server, store.Store, *store.Agent, *fakeMintingTokenGenerator, *atomic.Int32) {
	t.Helper()
	ctx := context.Background()

	srv, s, project, broker := setupReincarnateTestServer(t, nil)
	broker.Endpoint = "http://localhost:9800"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	reprovisions := &atomic.Int32{}
	mock := &mockRuntimeBrokerClient{
		failFirstStartWith: startErr,
	}
	mock.createWithGatherFunc = func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		reprovisions.Add(1)
		return &RemoteAgentResponse{
			Agent:         &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: string(state.PhaseCreated)},
			Created:       true,
			Reprovisioned: req.Reprovision,
		}, nil, nil
	}

	disp := NewHTTPAgentDispatcherWithClient(s, mock, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: s}
	disp.SetTokenGenerator(gen)
	srv.SetDispatcher(disp)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Profile = "gone"
		a.AppliedConfig.Task = "gen-1-task"
	})
	_, err := s.SetAgentRunID(ctx, agent.ID, "run-gen-1", nil)
	require.NoError(t, err)
	agent = mustGetAgent(t, s, agent.ID)
	return srv, s, agent, gen, reprovisions
}

// TestReincarnateAgent_ProfileUnresolved503_RestoresConfigAndRevertsRunID
// covers ptone/scion#3818: when the post-reprovision start fails with a
// broker 503 runtime_unavailable (such as an unresolvable saved runtime
// profile), no container was created, so the hub reverts the minted run ID,
// re-renders the outgoing generation's config, and restores the previous
// config on the agent row while leaving credential revocation unchanged.
func TestReincarnateAgent_ProfileUnresolved503_RestoresConfigAndRevertsRunID(t *testing.T) {
	startErr := &brokerStatusError{
		StatusCode: http.StatusServiceUnavailable,
		Body:       profileUnresolvedBody(`"gone"`),
		RetryAfter: "30",
	}
	srv, s, agent, gen, reprovisions := setupHTTPReincarnateStartFailure(t, startErr)
	self := agentIdentityFor(agent.ID, agent.ProjectID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{
		Handoff: "switch to v2",
	}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "start failed: runtime broker returned error 503")

	assert.Equal(t, int32(2), reprovisions.Load(), "the previous config must be re-rendered after a 503 runtime_unavailable start refusal")

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, final.Generation, "generation stays at 1")
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "previous config image is restored on the row")
	assert.Contains(t, final.AppliedConfig.Task, "[SCION RECOVERY HANDOFF]", "previous config is restored with the recovery handoff task")
	assert.NotContains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]", "new generation preamble is not kept on the row")
	assert.Equal(t, "run-gen-1", final.RunID, "the run ID minted by DispatchAgentStart must be reverted to the previous run")

	require.NotEmpty(t, gen.jtis)
	for _, jti := range gen.jtis {
		assert.Nil(t, getTestAgentCredential(t, s, jti).RevokedAt, "credential revocation behavior is unchanged")
	}
}

// TestDispatchAgentStart_RuntimeUnavailable503_RevertsRunWithoutRevokingCredential
// pins the narrow predicate split for ptone/scion#3818: a 503
// runtime_unavailable reverts the minted run ID and counts as not started for
// reincarnationStartLeftNoContainer / startDidNotHappen, while leaving
// isConfirmedBrokerRejection, isConfirmedStartNotActedOnError, and
// DispatchAgentStart's start_failed credential revocation unchanged.
func TestDispatchAgentStart_RuntimeUnavailable503_RevertsRunWithoutRevokingCredential(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"profile_unresolved", profileUnresolvedBody(`"gone"`)},
		{"generic_runtime_unavailable", brokerRuntimeUnavailableBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			se := &brokerStatusError{
				StatusCode: http.StatusServiceUnavailable,
				Body:       tc.body,
				RetryAfter: "30",
			}

			assert.False(t, isConfirmedBrokerRejection(se), "credential-revoke broker-rejection rule stays unchanged")
			assert.False(t, isConfirmedStartNotActedOnError(se), "credential-revoke start classifier stays unchanged")
			assert.True(t, shouldRevertRun(se), "503 runtime_unavailable reverts the minted run ID")
			assert.True(t, reincarnationStartLeftNoContainer(se), "503 runtime_unavailable is definitive for reincarnate")
			assert.True(t, startDidNotHappen(se), "503 runtime_unavailable counts as a start that did not happen")

			memStore := createTestStore(t)
			broker := newTestBrokerForRevoke(t, memStore, "ru-revert-"+tc.name)
			project := &store.Project{
				ID:   tid("ru-project-" + tc.name),
				Name: "ru-" + tc.name,
				Slug: "ru-" + tidSlugSafe(tc.name),
			}
			require.NoError(t, memStore.CreateProject(ctx, project))
			agent := &store.Agent{
				ID:              tid("ru-agent-" + tc.name),
				Name:            "ru-agent",
				Slug:            "ru-agent",
				ProjectID:       project.ID,
				RuntimeBrokerID: broker.ID,
				Phase:           string(state.PhaseStopped),
			}
			require.NoError(t, memStore.CreateAgent(ctx, agent))
			_, err := memStore.SetAgentRunID(ctx, agent.ID, "run-prev", nil)
			require.NoError(t, err)
			agent = mustGetAgent(t, memStore, agent.ID)

			client := &mockRuntimeBrokerClient{returnErr: se}
			d := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())
			gen := &fakeMintingTokenGenerator{store: memStore}
			d.SetTokenGenerator(gen)

			err = d.DispatchAgentStart(ctx, agent, "", false)
			require.Error(t, err)
			assert.Equal(t, "run-prev", mustGetAgent(t, memStore, agent.ID).RunID, "minted run ID must be reverted")
			require.Len(t, gen.jtis, 1)
			assert.Nil(t, getTestAgentCredential(t, memStore, gen.lastJTI()).RevokedAt, "start_failed credential revocation must stay disarmed for 503")
		})
	}

	// Defensive guard: if a 503 runtime_unavailable ever carried the
	// start-attempted marker, neither run revert nor reincarnate treats it as
	// pre-create.
	marked := brokerEnvelope(t, http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable, startAttempted("run-1"))
	assert.False(t, shouldRevertRun(marked))
	assert.False(t, reincarnationStartLeftNoContainer(marked))
}

// TestReincarnateAgent_Uncoded503AndTimeout_StayAmbiguous verifies that
// uncoded 503s, 503s with other error codes, and transport timeouts remain
// ambiguous: neither the run ID nor the reincarnate config is reverted.
func TestReincarnateAgent_Uncoded503AndTimeout_StayAmbiguous(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"uncoded_503_plain_text", &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: "service unavailable"}},
		{"other_coded_503", &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: `{"error":{"code":"unavailable","message":"upstream unavailable"}}`}},
		{"timeout", fmt.Errorf("failed to send request: %w", context.DeadlineExceeded)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, shouldRevertRun(tc.err))
			assert.False(t, reincarnationStartLeftNoContainer(tc.err))

			srv, s, agent, _, reprovisions := setupHTTPReincarnateStartFailure(t, tc.err)
			self := agentIdentityFor(agent.ID, agent.ProjectID)

			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{
				Handoff: "switch to v2",
			}), agent.ID)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

			r := waitForReincarnationSettled(t, s, agent.ID)
			require.Equal(t, store.AgentReincarnationStateFailed, r.State)
			assert.Equal(t, int32(1), reprovisions.Load(), "ambiguous start failure must not re-render previous config")

			final, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Empty(t, final.AppliedConfig.Image, "row stays on gen N+1 config when start outcome is ambiguous")
			assert.Contains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]")
			assert.NotEqual(t, "run-gen-1", final.RunID, "ambiguous start failure keeps the minted run ID")
		})
	}
}
