// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The create rollback helpers refuse a nil agent with a wrapped error (or a
// 500 carrying a correlation ID) and write nothing to the store.
func TestCreateRollbackNilAgentGuards(t *testing.T) {
	f := newUATCreateFixture(t, "nil-guard")
	f.srv.SetDispatcher(nil)
	ctx := logging.ContextWithRequestMeta(context.Background(), &logging.RequestMeta{RequestID: "req-nil-guard"})

	agentCount := func() int {
		res, err := f.store.ListAgents(context.Background(), store.AgentFilter{ProjectID: f.proj.ID}, store.ListOptions{})
		require.NoError(t, err)
		return len(res.Items)
	}
	compensationAudits := func() int {
		recs, _, err := f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
		require.NoError(t, err)
		return len(recs)
	}

	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"compensateAgentCreate", func(t *testing.T) {
			err := f.srv.compensateAgentCreate(ctx, createCompensation{Stage: createStageDispatch, Cause: errors.New("dispatch failed")})
			assert.ErrorIs(t, err, errAgentCreateWriteInvalid)
		}},
		{"cleanupFailedCreate", func(t *testing.T) {
			deleteCalled := false
			corrID := f.srv.cleanupFailedCreate(ctx, createRollback{
				Stage:             createStageDispatch,
				Cause:             errors.New("dispatch failed"),
				RevokeCredentials: true,
				DeleteRuntime:     func(context.Context) error { deleteCalled = true; return nil },
			})
			assert.Equal(t, "req-nil-guard", corrID)
			assert.False(t, deleteCalled, "no runtime delete without an agent")

			rec := httptest.NewRecorder()
			originalWritten := false
			writeCreateFailure(rec, corrID, func() { originalWritten = true })
			assert.False(t, originalWritten, "only the 500 is written")
			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, ErrCodeInternalError, body.Error.Code)
			assert.Equal(t, "req-nil-guard", body.Error.Details["correlation_id"])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentsBefore, auditsBefore := agentCount(), compensationAudits()
			tc.run(t)
			assert.Equal(t, agentsBefore, agentCount(), "no agent row written or removed")
			assert.Equal(t, auditsBefore, compensationAudits(), "no compensation audit record")
		})
	}
}
