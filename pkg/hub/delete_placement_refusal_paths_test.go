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

// Tests that every agent delete path answers a placement refusal with the
// refusal's own 409 code, guidance and details (ptone/scion#5623).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requirePlacementAnswer asserts a placement-refusal delete answer for a:
// 409 with code, the guidance message, and the pin details with agentId.
// deletionCode is present (conflict) only when admin. It returns the
// details.
func requirePlacementAnswer(t *testing.T, rec *httptest.ResponseRecorder, a *store.Agent, code string, admin bool) map[string]interface{} {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	apiCode, msg, details := errorMessageAndDetails(t, rec)
	assert.Equal(t, code, apiCode)
	assert.Contains(t, msg, "saved placement is inconsistent")
	assert.Equal(t, a.ID, details["agentId"])
	assert.Equal(t, a.PinnedRuntimeBrokerID, details["pinnedRuntimeBrokerId"])
	assert.Equal(t, a.PinnedRuntimeTargetID, details["pinnedRuntimeTargetId"])
	assert.Equal(t, a.RuntimeBrokerID, details["runtimeBrokerId"])
	if admin {
		assert.Equal(t, store.DeletionCodeConflict, details["deletionCode"])
	} else {
		assert.NotContains(t, details, "deletionCode")
	}
	return details
}

// The dispatcher backstop's refusal after the claim answers with the
// refusal's code, not the plain conflict, under plain, force and
// best-effort deletes; the stored deletion code stays conflict.
func TestAgentDelete_BackstopRefusalAnswersSpecificCode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase state.Phase
		query string
	}{
		{"running, plain", state.PhaseRunning, ""},
		{"running, force", state.PhaseRunning, "?force=true"},
		{"created no launch (best-effort), plain", state.PhaseCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunMismatchFixture(t, "pr-del-"+uuid.NewString()[:8], tc.phase)
			refusal := placementRefusalFor(f.agent)
			f.client.answer = func(string) error { return refusal }

			r := f.del(t, tc.query)
			requirePlacementAnswer(t, r.rec, f.agent, ErrCodeRuntimeTargetPinStale, true)

			got := mustGetAgent(t, f.store, f.agent.ID)
			assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
			assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode, "the stored deletion code is unchanged")
			assert.Equal(t, string(tc.phase), got.Phase, "the prior phase is restored")
		})
	}
}

// A caller who is not a platform admin gets the same code, guidance and
// pin details (as for every other placement refusal), only without the
// deletion code.
func TestAgentDelete_BackstopRefusalNonAdminAnswer(t *testing.T) {
	f := newRunMismatchFixture(t, "pr-owner-"+uuid.NewString()[:8], state.PhaseRunning)
	refusal := placementRefusalFor(f.agent)
	f.client.answer = func(string) error { return refusal }

	req := ownerDeleteRequest(t, f.store, context.Background(), f.agent.ID)
	rec := httptest.NewRecorder()
	f.srv.performAgentDelete(rec, req, mustGetAgent(t, f.store, f.agent.ID))
	requirePlacementAnswer(t, rec, f.agent, ErrCodeRuntimeTargetPinStale, false)
	assert.NotContains(t, rec.Body.String(), genericDeleteFailedMessage)
}

// The engine's check of the claimed snapshot (an abandoned deleting row,
// which the pre-check skips) answers with the specific code too.
func TestAgentDelete_SnapshotCheckAnswersSpecificCode(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-snapshot-code", string(state.PhaseRunning))
	seedAbandonedDeletion(t, f.s, a.ID, store.DeletionStateDeleting)

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
	requirePlacementAnswer(t, rec, a, ErrCodeRuntimeTargetPinStale, true)
	assert.False(t, f.client.deleteCalled, "nothing is dispatched")
	assert.Equal(t, store.DeletionCodeConflict, mustGetAgent(t, f.s, a.ID).DeletionCode)
}

// A request that joins the failed delete on this hub process answers with
// the remembered refusal for that claim; for any other claim (or with no
// entry, as on another hub process) it answers the plain conflict.
func TestAgentDelete_JoinAnswersRememberedRefusalForItsClaim(t *testing.T) {
	f := newRunMismatchFixture(t, "pr-join-"+uuid.NewString()[:8], state.PhaseRunning)
	refusal := placementRefusalFor(f.agent)
	f.client.answer = func(string) error { return refusal }
	r := f.del(t, "")
	require.Equal(t, http.StatusConflict, r.rec.Code, r.rec.Body.String())
	claim := mustGetAgent(t, f.store, f.agent.ID).DeletionClaim
	t.Cleanup(func() { deletionPlacementRefusals.Delete(f.agent.ID) })

	rec := httptest.NewRecorder()
	require.True(t, f.srv.resolveJoinFromRow(rec, context.Background(), f.agent.ID, claim, true))
	requirePlacementAnswer(t, rec, f.agent, ErrCodeRuntimeTargetPinStale, true)

	// An entry for another claim is not this delete's outcome.
	deletionPlacementRefusals.Store(f.agent.ID, deletionPlacementRefusalEntry{claim: claim - 1, refusal: refusal})
	rec = httptest.NewRecorder()
	require.True(t, f.srv.resolveJoinFromRow(rec, context.Background(), f.agent.ID, claim, true))
	require.Equal(t, http.StatusConflict, rec.Code)
	code, details := errorBody(t, rec)
	assert.Equal(t, ErrCodeConflict, code)
	assert.NotContains(t, details, "pinnedRuntimeTargetId")

	// No entry: the plain conflict.
	deletionPlacementRefusals.Delete(f.agent.ID)
	rec = httptest.NewRecorder()
	require.True(t, f.srv.resolveJoinFromRow(rec, context.Background(), f.agent.ID, claim, true))
	code, _ = errorBody(t, rec)
	assert.Equal(t, ErrCodeConflict, code)
}

// The claim's pre-check answer carries the refusal's details, as the
// engine's answer for the same refusal does.
func TestAgentDelete_PreCheckAnswerCarriesDetails(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-precheck-details", string(state.PhaseRunning))

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
	details := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
	assert.Equal(t, a.ID, details["agentId"])
	assert.Equal(t, a.PinnedRuntimeBrokerID, details["pinnedRuntimeBrokerId"])
	assert.Equal(t, a.PinnedRuntimeTargetID, details["pinnedRuntimeTargetId"])
	assert.Equal(t, a.RuntimeBrokerID, details["runtimeBrokerId"])
	assert.False(t, f.client.deleteCalled)
}

// --- failed-create cleanup (ptone/scion#4962) --------------------------------

// A failed create whose cleanup delete is refused for an inconsistent
// saved placement keeps its row, as for a run mismatch: no compensation,
// the row in phase error with the cleanup prefix and the placement
// guidance (no advice to delete it, which is refused too), its quotas held.
// The phase-error write is guarded on the run the refused delete named,
// when the refusal carries it.
func TestCleanupFailedCreate_PlacementRefusalKeepsRow(t *testing.T) {
	type outcome int
	const (
		markedError outcome = iota
		untouched
	)
	for _, tc := range []struct {
		name    string
		wrapRun bool // the refusal carries the run the delete named
		moveTo  string
		want    outcome
	}{
		{name: "refused, run carried", wrapRun: true, want: markedError},
		{name: "refused, no run carried", want: markedError},
		{name: "refused, row moved to another run", wrapRun: true, moveTo: "run-other", want: untouched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			var logs bytes.Buffer
			srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logs, nil))
			setBrokerAgentCeiling(t, s, 5)
			agent := setupBrokerAgentInPhase(t, s, "pr-cleanup-"+uuid.NewString()[:8], state.PhaseProvisioning)
			ctx := context.Background()
			broker, err := s.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
			require.NoError(t, err)
			reserveBrokerSlot(t, s, broker, agent.ID)
			_, err = s.SetAgentRunID(ctx, agent.ID, "run-mine", nil)
			require.NoError(t, err)
			agent = mustGetAgent(t, s, agent.ID)
			before := agent.Phase

			var refusalErr error = placementConflict(stalePinRefusal(agent, "test: refused by the backstop"))
			if tc.wrapRun {
				refusalErr = &createCleanupDeleteError{runID: "run-mine", err: refusalErr}
			}
			corrID := srv.cleanupFailedCreate(ctx, createRollback{
				Agent:           agent,
				RuntimeBrokerID: agent.RuntimeBrokerID,
				Stage:           createStageDispatch,
				Cause:           errors.New("dispatch failed"),
				DeleteRuntime: func(dctx context.Context) error {
					if tc.moveTo != "" {
						if _, err := s.SetAgentRunID(dctx, agent.ID, tc.moveTo, nil); err != nil {
							return err
						}
					}
					return refusalErr
				},
			})
			assert.Empty(t, corrID)

			got, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err, "the row was removed after the placement refusal")
			assert.EqualValues(t, 1, brokerReservationCount(t, s, agent.RuntimeBrokerID), "the reservation is held")
			assert.Contains(t, logs.String(), "agent placement is inconsistent; nothing was dispatched, keeping the agent row")
			switch tc.want {
			case markedError:
				assert.Equal(t, string(state.PhaseError), got.Phase)
				assert.True(t, strings.HasPrefix(got.Message, createCleanupRefusedPrefix), got.Message)
				assert.Contains(t, got.Message, "saved placement is inconsistent")
				assert.NotContains(t, got.Message, "Delete the agent to retry")
				assert.True(t, isCreateCleanupRefusedRow(got))
			case untouched:
				assert.Equal(t, tc.moveTo, got.RunID)
				assert.Equal(t, before, got.Phase, "the live run was marked failed")
				assert.NotContains(t, got.Message, "cleanup was refused")
				assert.Contains(t, logs.String(), "not marking it failed")
			}
		})
	}
}

// The failed-create cleanup's broker delete returns the dispatcher's
// placement refusal with the run it named, and sends nothing.
func TestDispatchDeleteFailedCreate_PlacementRefusalCarriesRun(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-create-cleanup", string(state.PhaseProvisioning))
	_, err := f.s.SetAgentRunID(ctx, a.ID, "run-created", nil)
	require.NoError(t, err)

	err = dispatchDeleteFailedCreate(f.s, f.srv.GetDispatcher(), a)(ctx)
	refusal, ok := placementRefusalOf(err)
	require.True(t, ok, "a placement refusal: %v", err)
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, refusal.Code)
	assert.Equal(t, "run-created", createCleanupTargetRun(err))
	assert.False(t, f.client.deleteCalled, "nothing is sent")

	assert.Equal(t, "", createCleanupTargetRun(errors.New("other")))
}
