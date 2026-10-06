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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

func brokerMismatchErr(status int, brokerID string) *brokerStatusError {
	return &brokerStatusError{StatusCode: status, Body: fmt.Sprintf(
		`{"error":{"code":%q,"message":"refused by the Runtime Broker","details":{"runtimeBrokerId":%q,"expectedRuntimeTargetId":"x","actualRuntimeTargetId":"y","startAttempted":true}}}`,
		ErrCodeRuntimeTargetMismatch, brokerID)}
}

// TestRelayRuntimeTargetError_RequiresMatchingStatus: a Runtime Broker answer
// is relayed as a flat refusal only with the code's own status; the same
// code under another status is left to the caller's generic handling.
func TestRelayRuntimeTargetError_RequiresMatchingStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	require.True(t, relayRuntimeTargetError(rec, brokerMismatchErr(http.StatusConflict, "b")))
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	requireNoStartMarkers(t, d)
	assert.Equal(t, "b", d["runtimeBrokerId"])

	assert.False(t, relayRuntimeTargetError(httptest.NewRecorder(), brokerMismatchErr(http.StatusInternalServerError, "b")),
		"a mismatch code under a 500 is not a flat refusal")
	assert.Nil(t, runtimeTargetDMErrorIfAny(brokerMismatchErr(http.StatusInternalServerError, "b")))
}

// TestFlatCreateOnExisting_RefusalSettlesMessage: a Runtime Broker refusal on
// the create-on-existing start is relayed with its own status and recorded
// as the agent message (definite start failure).
func TestFlatCreateOnExisting_RefusalSettlesMessage(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.pinnedAgent(t, "settle-resume", string(state.PhaseStopped))
	f.client.returnErr = brokerMismatchErr(http.StatusConflict, f.flat.ID)
	rec := f.create(t, map[string]interface{}{"name": "settle-resume", "task": "t", "resume": true})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	requireNoStartMarkers(t, d)
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, "refused by the Runtime Broker", got.Message)
}

// TestFlatWake_RefusalSettlesMessage: the same on the wake-on-DM path.
func TestFlatWake_RefusalSettlesMessage(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.pinnedAgent(t, "settle-wake", string(state.PhaseSuspended))
	f.client.returnErr = brokerMismatchErr(http.StatusConflict, f.flat.ID)
	_, dmErr := f.srv.wakeAgentForDM(context.Background(), a)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Equal(t, ErrCodeRuntimeTargetMismatch, dmErr.Code)
	assert.NotContains(t, dmErr.Details, "startAttempted")
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, "refused by the Runtime Broker", got.Message)
}
