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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHarnessConfigService_GetAndList_DecodeCapabilities(t *testing.T) {
	const hcJSON = `{
		"id": "hc-codex-1",
		"name": "codex",
		"slug": "codex",
		"harness": "codex",
		"scope": "global",
		"status": "active",
		"config": {
			"harness": "codex",
			"image": "scion-codex:latest",
			"capabilities": {
				"harness": "codex",
				"limits": {
					"max_turns": {"support": "yes"},
					"max_model_calls": {"support": "no", "reason": "no model events"},
					"max_duration": {"support": "yes"}
				},
				"resume": {"support": "yes"},
				"agent_plugins": {"support": "partial", "reason": "Native install via local marketplace"}
			}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/harness-configs/hc-codex-1":
			_, _ = w.Write([]byte(hcJSON))
		case "/api/v1/harness-configs":
			_, _ = w.Write([]byte(`{"harnessConfigs":[` + hcJSON + `],"totalCount":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	got, err := c.HarnessConfigs().Get(context.Background(), "hc-codex-1")
	require.NoError(t, err)
	require.NotNil(t, got.Config)
	require.NotNil(t, got.Config.Capabilities)
	assert.Equal(t, "codex", got.Config.Capabilities.Harness)
	assert.Equal(t, api.SupportYes, got.Config.Capabilities.Limits.MaxTurns.Support)
	assert.Equal(t, api.CapabilityField{Support: api.SupportNo, Reason: "no model events"}, got.Config.Capabilities.Limits.MaxModelCalls)
	assert.Equal(t, api.SupportYes, got.Config.Capabilities.Resume.Support)
	assert.Equal(t, api.CapabilityField{
		Support: api.SupportPartial,
		Reason:  "Native install via local marketplace",
	}, got.Config.Capabilities.AgentPlugins)

	listResp, err := c.HarnessConfigs().List(context.Background(), &ListHarnessConfigsOptions{Status: "active"})
	require.NoError(t, err)
	require.Len(t, listResp.HarnessConfigs, 1)
	require.NotNil(t, listResp.HarnessConfigs[0].Config)
	require.NotNil(t, listResp.HarnessConfigs[0].Config.Capabilities)
	assert.Equal(t, got.Config.Capabilities, listResp.HarnessConfigs[0].Config.Capabilities)
}
