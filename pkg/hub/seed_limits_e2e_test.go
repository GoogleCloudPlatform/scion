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

// End-to-end test for ptone/scion#2061 P1a: the admin API is now the
// supported path for changing the hub-wide max_agents_per_broker value
// (design.md §4.3/§4.7 P1a test 3). Deliberately goes through
// PUT /api/v1/admin/limits/{id} instead of the setBrokerAgentCeiling test
// helper (which writes the store directly) to prove the HTTP path itself
// enforces the new cap end to end.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func TestBrokerQuota_AdminPUTChangesEnforcedCap_EndToEnd(t *testing.T) {
	disp := &quotaLifecycleDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	ctx := context.Background()

	limitDef, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	require.True(t, limitDef.System)

	// Set the hub-wide cap to 2 via the admin API — the supported path for
	// ptone/scion#2063 item 1, not a direct store write.
	putRec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+limitDef.ID, updateLimitDefinitionRequest{
		Name:         limitDef.Name,
		ResourceType: limitDef.ResourceType,
		Unit:         limitDef.Unit,
		Description:  limitDef.Description,
		DefaultValue: 2,
	})
	require.Equal(t, http.StatusOK, putRec.Code, "body: %s", putRec.Body.String())

	// Two agents on the broker should be accepted.
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "e2e-cap-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "e2e-cap-2", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())

	// The third must be rejected with 429 quota_exceeded.
	rec3 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "e2e-cap-3", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusTooManyRequests, rec3.Code, rec3.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &errResp))
	require.Equal(t, ErrCodeQuotaExceeded, errResp.Error.Code)
}
