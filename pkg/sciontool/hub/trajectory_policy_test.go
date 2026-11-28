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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestClient_GetTrajectoryCapturePolicy(t *testing.T) {
	t.Run("decodes trajectory_capture when present", func(t *testing.T) {
		var gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("X-Scion-Agent-Token")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "agent-123",
				"trajectory_capture": map[string]any{
					"policy":          "full",
					"enforced_by_hub": true,
				},
			})
		}))
		defer srv.Close()

		c := &Client{
			hubURL:  srv.URL,
			agentID: "agent-123",
			token:   "tok-abc",
			client:  srv.Client(),
		}
		status, err := c.GetTrajectoryCapturePolicy(context.Background())
		require.NoError(t, err)
		require.NotNil(t, status)
		assert.Equal(t, "/api/v1/agents/agent-123", gotPath)
		assert.Equal(t, "tok-abc", gotAuth)
		assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyFull, status.Policy)
		assert.True(t, status.EnforcedByHub)
	})

	t.Run("returns nil when omitted (experiment off)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"agent-123"}`))
		}))
		defer srv.Close()

		c := &Client{
			hubURL:  srv.URL,
			agentID: "agent-123",
			token:   "tok-abc",
			client:  srv.Client(),
		}
		status, err := c.GetTrajectoryCapturePolicy(context.Background())
		require.NoError(t, err)
		assert.Nil(t, status)
	})

	t.Run("returns error on non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := &Client{
			hubURL:  srv.URL,
			agentID: "agent-123",
			token:   "tok-abc",
			client:  srv.Client(),
		}
		status, err := c.GetTrajectoryCapturePolicy(context.Background())
		require.Error(t, err)
		assert.Nil(t, status)
	})
}
