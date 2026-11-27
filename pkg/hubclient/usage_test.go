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

package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageService_ScopedEndpointsAndRawPassthrough(t *testing.T) {
	var gotPath string
	var gotQuery string
	rawBody := `{"scope":{"kind":"project","id":"proj-1"},"window":{"since":"2026-11-20T00:00:00Z","until":"2026-11-27T23:59:59Z","granularity":"day"},"group_by":["model"],"calls":10,"error_calls":1,"events":10,"null_token_events":0,"tokens":{"input":100,"output":50,"cache_read":null,"cache_write":0,"cache_write_1h":null,"reasoning":null,"summable_tokens":150,"unknown_events":{"output":2}},"groups":[{"model":"claude-sonnet-4-6","calls":10,"error_calls":1,"events":10,"null_token_events":0,"tokens":{"input":100,"output":50,"cache_read":null,"cache_write":0,"cache_write_1h":null,"reasoning":null,"summable_tokens":150}}],"truncated":false,"measurement_status":"partial","unmeasured_agents":1,"stale_agents":2,"as_of":"2026-11-27T12:00:00Z","last_receipt_at":null,"future_field":"preserved"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rawBody))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	opts := &UsageQueryOptions{
		Since:   "7d",
		Until:   "2026-11-27",
		GroupBy: []string{"model", "day"},
		Limit:   25,
	}

	resp, err := c.Usage().GetProject(context.Background(), "proj-1", opts)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/projects/proj-1/usage", gotPath)
	assert.Contains(t, gotQuery, "since=7d")
	assert.Contains(t, gotQuery, "until=2026-11-27")
	assert.Contains(t, gotQuery, "groupBy=model%2Cday")
	assert.Contains(t, gotQuery, "limit=25")
	assert.Equal(t, "partial", resp.MeasurementStatus)
	assert.Equal(t, 1, resp.UnmeasuredAgents)
	assert.Equal(t, 2, resp.StaleAgents)
	require.NotNil(t, resp.Tokens.Input)
	assert.Equal(t, int64(100), *resp.Tokens.Input)
	assert.Nil(t, resp.Tokens.CacheRead)
	require.NotNil(t, resp.Tokens.CacheWrite)
	assert.Equal(t, int64(0), *resp.Tokens.CacheWrite)
	require.NotNil(t, resp.Tokens.UnknownEvents)
	assert.Equal(t, int64(2), resp.Tokens.UnknownEvents.Output)
	assert.JSONEq(t, rawBody, string(resp.Raw))

	_, err = c.Usage().GetMe(context.Background(), &UsageQueryOptions{Since: "7d"})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/me/usage", gotPath)

	_, err = c.Usage().GetUser(context.Background(), "user-42", nil)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/users/user-42/usage", gotPath)

	_, err = c.Usage().GetGlobal(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/usage", gotPath)
}

func TestUsageService_GetAgent(t *testing.T) {
	var gotPath string
	var gotQuery string
	rawBody := `{"agent_id":"ag-1","project_id":"proj-1","window":{"since":"2026-11-20T00:00:00Z","until":"2026-11-27T23:59:59Z"},"calls":5,"error_calls":0,"events":5,"null_token_events":0,"tokens":{"input":50,"output":25,"cache_read":null,"cache_write":null,"cache_write_1h":null,"reasoning":null,"summable_tokens":75},"by_model":[{"model":"claude-sonnet-4-6","harness":"claude","calls":5,"error_calls":0,"events":5,"null_token_events":0,"tokens":{"input":50,"output":25,"cache_read":null,"cache_write":null,"cache_write_1h":null,"reasoning":null,"summable_tokens":75}}],"measurement_status":"measured","as_of":"2026-11-27T12:00:00Z","last_receipt_at":null}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rawBody))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	resp, err := c.Usage().GetAgent(context.Background(), "ag-1", &UsageQueryOptions{Since: "7d"})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/agents/ag-1/usage", gotPath)
	assert.Equal(t, "since=7d", gotQuery)
	assert.Equal(t, "ag-1", resp.AgentID)
	assert.Equal(t, "measured", resp.MeasurementStatus)
	require.Len(t, resp.ByModel, 1)
	assert.Equal(t, "claude-sonnet-4-6", resp.ByModel[0].Model)
	assert.JSONEq(t, rawBody, string(resp.Raw))
}
