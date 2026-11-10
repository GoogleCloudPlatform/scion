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

// A start dispatch whose run could not be begun (beginRun) sent nothing to
// the broker: it is classified as a start that did not happen
// (errStartRunNotBegun), so its claim is released rather than kept
// unconfirmed, and a restart's start leg that fails this way records the
// agent stopped and releases its slot.

package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The start dispatch wraps each of beginRun's pre-send failures in
// errStartRunNotBegun. Both classifiers treat it as a start that did not
// act, except that a run-ID write a delete refused is not a start that did
// not happen (the delete owns the row) although its claim is still
// released. Without the wrap, the same errors are unknown outcomes.
func TestStartOutcomeOf_PreSendFailuresReleased(t *testing.T) {
	cases := []struct {
		name         string
		cause        error
		didNotHappen bool
	}{
		{"no token generator", errors.New("failed to sign the agent token for agent a: no token generator"), true},
		{"sign error", fmt.Errorf("failed to sign the agent token for agent a: %w", errors.New("key unavailable")), true},
		{"agent token record", fmt.Errorf("%w for agent a: %w", errAgentTokenRecord, store.ErrCredentialNotRecorded), true},
		{"run-ID record", fmt.Errorf("failed to record the run ID for agent a: %w", errors.New("disk full")), true},
		{"run-ID record refused by a delete", fmt.Errorf("failed to record the run ID for agent a: %w", store.ErrDeleteInProgress), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("%w: %w", errStartRunNotBegun, tc.cause)
			assert.Equal(t, startReleased, startOutcomeOf(wrapped), "a pre-send failure releases the slot")
			assert.Equal(t, tc.didNotHappen, startDidNotHappen(wrapped), "a pre-send failure did not happen")
			if !errors.Is(tc.cause, store.ErrDeleteInProgress) {
				assert.Equal(t, startUnconfirmed, startOutcomeOf(tc.cause), "setup: unwrapped, the cause alone is an unknown outcome")
			}
		})
	}
}

// signFailTokenGenerator authorizes tokens but fails to sign them.
type signFailTokenGenerator struct{ staticTokenGenerator }

func (signFailTokenGenerator) SignAgentToken(AgentTokenGrant, string) (string, *store.AgentCredential, error) {
	return "", nil, errors.New("key unavailable")
}

// runIDWriteFailStore fails every run-ID write: with a credential as a
// refused credential record (when credential is set), otherwise as a plain
// store error.
type runIDWriteFailStore struct {
	store.Store
	credential bool
}

func (s runIDWriteFailStore) SetAgentRunID(ctx context.Context, agentID, runID string, cred *store.AgentCredential) (string, error) {
	if s.credential && cred != nil {
		return "", fmt.Errorf("%w: %w", store.ErrCredentialNotRecorded, errors.New("insert refused"))
	}
	if !s.credential {
		return "", errors.New("disk full")
	}
	return s.Store.SetAgentRunID(ctx, agentID, runID, cred)
}

// A restart whose start leg fails before anything is sent (its run could
// not be begun) did not start anything: it records the agent stopped and
// releases its reservation, as for any start leg that did not act.
func TestRestartPreSendFailure_RecordsStoppedAndReleases(t *testing.T) {
	cases := []struct {
		name  string
		gen   AgentTokenGenerator
		store func(store.Store) store.Store
	}{
		{"sign error", signFailTokenGenerator{staticTokenGenerator{token: "test-token"}}, func(s store.Store) store.Store { return s }},
		{"agent token record", staticTokenGenerator{token: "test-token"}, func(s store.Store) store.Store { return runIDWriteFailStore{Store: s, credential: true} }},
		{"run-ID record", staticTokenGenerator{token: "test-token"}, func(s store.Store) store.Store { return runIDWriteFailStore{Store: s} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, agent := restartQuotaAgent(t, "presend-"+tc.name)
			mockClient := &mockRuntimeBrokerClient{}
			d := NewHTTPAgentDispatcherWithClient(tc.store(s), mockClient, false, slog.Default())
			d.SetTokenGenerator(tc.gen)
			srv.SetDispatcher(d)
			require.EqualValues(t, 1, brokerReservationCount(t, s, agent.RuntimeBrokerID), "setup: the running agent holds its slot")

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
			require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
			require.True(t, mockClient.stopCalled, "the stop leg was dispatched")
			assert.False(t, mockClient.startCalled, "nothing was sent for the start leg")

			got := mustGetAgent(t, s, agent.ID)
			assert.Equal(t, string(state.PhaseStopped), got.Phase, "a pre-send failure releases the slot")
			assert.EqualValues(t, 0, brokerReservationCount(t, s, agent.RuntimeBrokerID), "a pre-send failure releases the slot")
			assert.Empty(t, got.StartClaimID, "the restart claim is released")
		})
	}
}
