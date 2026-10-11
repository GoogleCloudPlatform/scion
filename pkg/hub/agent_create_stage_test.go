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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each create failure site records its own stage in the rollback audit
// record.
//
// Must not run in parallel: the managed case swaps the package-level
// managed-agent backend.
func TestCreateRollbackStageAtEachSite(t *testing.T) {
	for i, tc := range createRollbackSites() {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, tc.disp)
			if tc.setup != nil {
				tc.setup(t, srv)
			}
			req := tc.req
			req.Name = "stage-" + tidSlugSafe(tc.name)
			req.ProjectID = project.ID
			req.Task = "do something"

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
			require.GreaterOrEqual(t, rec.Code, 400, "case %d: %s", i, rec.Body.String())

			failed, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
			require.NoError(t, err)
			require.Len(t, failed, 1, "the rolled-back create is recorded once")
			var sum compensationSummary
			require.NoError(t, json.Unmarshal([]byte(failed[0].AfterSummary), &sum))
			assert.Equal(t, tc.wantStage, sum.Stage)
		})
	}
}

// When the rollback's compensation transaction fails, every create failure
// site answers 500 with the request ID as its correlation ID, whatever
// status the site's own failure carries, and logs one ERROR record with the
// same correlation ID. The extra delete_in_progress case checks that a 409
// from dispatch gives way to the 500 too.
//
// Must not run in parallel: the managed case swaps the package-level
// managed-agent backend.
func TestCreateCompensationFailureAtEachSite(t *testing.T) {
	sites := append(createRollbackSites(), createRollbackSite{
		name:      "dispatch delete in progress",
		disp:      &failingCreateDispatcher{createErr: fmt.Errorf("persist run id: %w", store.ErrDeleteInProgress)},
		wantStage: createStageDispatch,
	})
	for _, tc := range sites {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, tc.disp)
			if tc.setup != nil {
				tc.setup(t, srv)
			}
			srv.store = &createTxFaultStore{Store: srv.store, auditErrFor: mutationTypeAgentCreateDispatchFailed}

			req := tc.req
			req.Name = "comp-" + tidSlugSafe(tc.name)
			req.ProjectID = project.ID
			req.Task = "do something"
			body, err := json.Marshal(req)
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+testDevToken)
			requestID := "req-comp-" + tidSlugSafe(tc.name)

			rec, logs := serveWithRequestID(t, srv.Handler(), r, requestID)

			require.Equal(t, http.StatusInternalServerError, rec.Code,
				"the site's own status must not reach the caller: %s", rec.Body.String())
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, ErrCodeInternalError, resp.Error.Code)
			assert.Equal(t, requestID, resp.Error.Details["correlation_id"],
				"the 500 body carries the request ID as its correlation ID")
			assert.Empty(t, rec.Header().Get("Retry-After"), "no relayed retry hint")

			require.Len(t, logs, 1, "one ERROR record for the failed compensation")
			assert.Equal(t, requestID, logs[0].CorrelationID, "the ERROR record carries the same correlation ID")
			require.NotEmpty(t, logs[0].AgentID)
			assert.NotEmpty(t, logs[0].OpID)

			ctx := context.Background()
			_, err = s.GetAgent(ctx, logs[0].AgentID)
			assert.ErrorIs(t, err, store.ErrNotFound, "the fallback removes the logged agent's row")
			assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, logs[0].AgentID),
				"the failed compensation wrote no record")
		})
	}
}

// A delete that claims the row while the create is being dispatched gets the
// delete_in_progress answer, carrying the created agent's ID, at both
// dispatch sites, and the create is rolled back with that site's stage.
func TestCreateDispatchDeleteInProgressAnswer(t *testing.T) {
	cases := []struct {
		name      string
		gatherEnv bool
		wantStage string
	}{
		{name: "dispatch with env gather", gatherEnv: true, wantStage: createStageDispatchEnvGather},
		{name: "dispatch", gatherEnv: false, wantStage: createStageDispatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &failingCreateDispatcher{createErr: fmt.Errorf("persist run id: %w", store.ErrDeleteInProgress)}
			srv, s, project := setupCreateAgentServer(t, disp)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:      "claim-" + tidSlugSafe(tc.name),
				ProjectID: project.ID,
				Task:      "do something",
				GatherEnv: tc.gatherEnv,
			})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			require.NotNil(t, disp.capturedAgent, "the create reached dispatch")
			agentID := disp.capturedAgent.ID
			require.NotEmpty(t, agentID)

			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
			assert.Equal(t, agentID, body.Error.Details["agentId"], "details.agentId")

			ctx := context.Background()
			failed, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
			require.NoError(t, err)
			require.Len(t, failed, 1, "the rolled-back create is recorded once")
			assert.Equal(t, agentID, failed[0].TargetID)
			var sum compensationSummary
			require.NoError(t, json.Unmarshal([]byte(failed[0].AfterSummary), &sum))
			assert.Equal(t, tc.wantStage, sum.Stage)

			_, err = s.GetAgent(ctx, agentID)
			assert.ErrorIs(t, err, store.ErrNotFound, "the agent row is rolled back")
			assert.True(t, disp.deleteCalled, "the broker-side delete runs for the rolled-back create")
		})
	}
}
