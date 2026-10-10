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
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inheritedGolden is testdata/agent-create-inherited-golden.json. The web's
// create form reads the same file: its inherited placeholders must equal
// dispatched for each case, so the two sides cannot drift.
type inheritedGolden struct {
	Cases []struct {
		Tier    string `json:"tier"`
		Sources struct {
			Template struct {
				Config *store.TemplateConfig `json:"config"`
			} `json:"template"`
			ProjectSettings json.RawMessage `json:"projectSettings"`
			HubSettings     struct {
				DefaultModel           string `json:"defaultModel"`
				AutoExposePortsEnabled *bool  `json:"autoExposePortsEnabled"`
			} `json:"hubSettings"`
		} `json:"sources"`
		Dispatched map[string]string `json:"dispatched"`
	} `json:"cases"`
}

// TestCreateAgent_InheritedValuesGolden creates an agent with no config keys
// from each case's sources (a template, project settings saved through the
// settings API, and hub defaults) and checks what the hub dispatches against
// the golden's dispatched values. It also checks that the settings and
// public-settings APIs return the sources as the golden states them, since
// that is what the web reads.
//
// Scope: the hub-side dispatch only. Values the broker resolves on its own
// (a template's image, the --model argument) are not covered here.
func TestCreateAgent_InheritedValuesGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "agent-create-inherited-golden.json"))
	require.NoError(t, err)
	var golden inheritedGolden
	require.NoError(t, json.Unmarshal(raw, &golden))
	require.Len(t, golden.Cases, 3, "one case per tier: template, project, hub")

	for _, tc := range golden.Cases {
		t.Run(tc.Tier, func(t *testing.T) {
			srv, s, project, mockClient := setupThinkingLevelDispatch(t)
			ctx := context.Background()
			// Wire the hub's auto-expose default as the server's own
			// dispatcher does, so the hub tier reaches the request.
			disp := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
			disp.SetAutoExposePortsDefaultProvider(srv.autoExposePortsDefault)
			srv.SetDispatcher(disp)

			// Hub tier.
			srv.mu.Lock()
			srv.config.AgentDefaults.DefaultModel = tc.Sources.HubSettings.DefaultModel
			srv.config.AutoExposePortsDefault = tc.Sources.HubSettings.AutoExposePortsEnabled
			srv.mu.Unlock()

			pub := doRequest(t, srv, http.MethodGet, "/api/v1/settings/public", nil)
			require.Equal(t, http.StatusOK, pub.Code, pub.Body.String())
			var pubBody map[string]interface{}
			require.NoError(t, json.Unmarshal(pub.Body.Bytes(), &pubBody))
			if tc.Sources.HubSettings.DefaultModel != "" {
				assert.Equal(t, tc.Sources.HubSettings.DefaultModel, pubBody["defaultModel"])
			}
			if tc.Sources.HubSettings.AutoExposePortsEnabled != nil {
				assert.Equal(t, *tc.Sources.HubSettings.AutoExposePortsEnabled, pubBody["autoExposePortsEnabled"])
			}

			// Project tier, through the settings API the web reads.
			put := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
				tc.Sources.ProjectSettings)
			require.Equal(t, http.StatusOK, put.Code, put.Body.String())
			get := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
			require.Equal(t, http.StatusOK, get.Code, get.Body.String())
			var want, got map[string]interface{}
			require.NoError(t, json.Unmarshal(tc.Sources.ProjectSettings, &want))
			require.NoError(t, json.Unmarshal(get.Body.Bytes(), &got))
			for k, v := range want {
				assert.Equal(t, v, got[k], "GET project settings must return %s as the golden states it", k)
			}

			// Template tier.
			tmpl := &store.Template{
				ID:          tid("template-inherited-" + tc.Tier),
				Name:        "inherited-" + tc.Tier,
				Slug:        "inherited-" + tc.Tier,
				Harness:     "claude",
				ContentHash: "d00dfeed",
				Scope:       store.TemplateScopeGlobal,
				Status:      store.TemplateStatusActive,
				Config:      tc.Sources.Template.Config,
			}
			require.NoError(t, s.CreateTemplate(ctx, tmpl))

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:      "inherited-" + tc.Tier,
				ProjectID: project.ID,
				Template:  tmpl.ID,
				Task:      "do something",
			})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.Agent)

			require.True(t, mockClient.createCalled, "agent should have been dispatched to the broker")
			req := mockClient.lastCreateReq
			require.NotNil(t, req)
			require.NotNil(t, req.Config)

			dispatched := map[string]string{}
			if v := req.ResolvedEnv["SCION_MODEL"]; v != "" {
				dispatched["config.model"] = v
			}
			if v := req.ResolvedEnv["SCION_THINKING_LEVEL"]; v != "" {
				dispatched["config.thinking_level"] = v
			}
			if v := req.ResolvedEnv["SCION_AUTO_EXPOSE_PORTS"]; v != "" {
				dispatched["autoExpose"] = v
			} else if d := req.Config.HubAgentDefaults; d != nil && d.AutoExposePorts != nil {
				dispatched["autoExpose"] = strconv.FormatBool(*d.AutoExposePorts)
			}
			if ic := req.InlineConfig; ic != nil {
				if ic.MaxTurns > 0 {
					dispatched["config.max_turns"] = strconv.Itoa(ic.MaxTurns)
				}
				if ic.MaxModelCalls > 0 {
					dispatched["config.max_model_calls"] = strconv.Itoa(ic.MaxModelCalls)
				}
				if ic.MaxDuration != "" {
					dispatched["config.max_duration"] = ic.MaxDuration
				}
				if r := ic.Resources; r != nil {
					if r.Requests.CPU != "" {
						dispatched["config.resources.requests.cpu"] = r.Requests.CPU
					}
					if r.Disk != "" {
						dispatched["config.resources.disk"] = r.Disk
					}
				}
				if ic.Telemetry != nil && ic.Telemetry.Enabled != nil {
					dispatched["config.telemetry"] = strconv.FormatBool(*ic.Telemetry.Enabled)
				}
			}
			if tc.Sources.Template.Config != nil && tc.Sources.Template.Config.MessageMode != "" {
				dispatched["messageMode"] = resp.Agent.MessageMode
			}

			assert.Equal(t, tc.Dispatched, dispatched)
		})
	}
}
