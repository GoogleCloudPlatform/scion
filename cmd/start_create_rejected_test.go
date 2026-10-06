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

package cmd

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3430: a create the Hub rejects (non-2xx, no agent record) must
// end `scion start` at once with exit code 1 and the Hub's message. It must
// not enter the launch wait or send any request after the create.
func TestStartAgentViaHub_RejectedCreateExitsImmediately(t *testing.T) {
	const projectID, agentName = "proj-rejected", "rejected-agent"
	for _, tc := range []struct {
		name    string
		status  int
		code    string
		message string
	}{
		{"validation 400", http.StatusBadRequest, "validation_error", "GCP service account not available in this project"},
		{"forbidden 403", http.StatusForbidden, "forbidden", "not allowed to create agents in this project"},
		{"unprocessable 422", http.StatusUnprocessableEntity, "validation_error", "template not found"},
		{"server error 500", http.StatusInternalServerError, "internal_error", "database unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			origSA, origNoWait, origWaitTimeout := serviceAccountFlag, startNoWait, startWaitTimeout
			t.Cleanup(func() {
				serviceAccountFlag, startNoWait, startWaitTimeout = origSA, origNoWait, origWaitTimeout
			})
			serviceAccountFlag, startNoWait, startWaitTimeout = "sa-unavailable", false, 0

			stub := newHubStartStub(t, projectID, agentName, "")
			stub.createStatus, stub.createErrCode, stub.createErrMsg = tc.status, tc.code, tc.message

			var startErr error
			stdout, stderr := captureStdIO(t, func() {
				startErr = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "do it", false, nil)
			})

			require.Error(t, startErr)
			assert.Contains(t, startErr.Error(), tc.message, "the Hub's error message must be surfaced")
			assert.Equal(t, 1, exitCodeFor(startErr), "a rejected create is a plain Hub failure: exit 1")
			assert.True(t, isHubFailure(startErr), "a rejected create is a Hub failure, not a usage error")

			assert.Equal(t, 1, stub.createCalls, "the create must be sent exactly once")
			assert.Equal(t, map[string]interface{}{"metadata_mode": "assign", "service_account_id": "sa-unavailable"},
				stub.createBody["gcp_identity"], "the reporter's --service-account must reach the create")
			assert.Empty(t, stub.afterCreate, "no request (wait/poll) may follow a rejected create")
			assert.NotContains(t, stdout+stderr, "Waiting for agent", "a rejected create must not enter the launch wait")
		})
	}
}
