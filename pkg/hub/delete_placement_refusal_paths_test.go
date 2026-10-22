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
	"context"
	"net/http"
	"net/http/httptest"
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
