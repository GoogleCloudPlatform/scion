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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateAgent_HubDefaultMessageMode: server.hub.default_message_mode
// applies only when neither the request nor the template sets a mode; unset,
// the default stays "project".
func TestCreateAgent_HubDefaultMessageMode(t *testing.T) {
	cases := []struct {
		name, hubDefault, snapshot, requested string
		wantMode                              string
	}{
		{name: "unset hub default: project", wantMode: store.MessageModeProject},
		{name: "hub default applies", hubDefault: store.MessageModeLineage, wantMode: store.MessageModeLineage},
		{name: "template wins over hub default", hubDefault: store.MessageModeLineage, snapshot: store.MessageModeBranch, wantMode: store.MessageModeBranch},
		{name: "request wins over hub default", hubDefault: store.MessageModeNone, requested: store.MessageModeProject, wantMode: store.MessageModeProject},
		{name: "hub value never applies as a default", hubDefault: store.MessageModeHub, wantMode: store.MessageModeProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			srv.config.DefaultMessageMode = tc.hubDefault
			require.NoError(t, s.CreateTemplate(context.Background(), &store.Template{
				ID: tid("hdmm-template-" + t.Name()), Name: "hdmm-tmpl", Slug: "hdmm-tmpl", Harness: "claude",
				ContentHash: "d00dfeed", Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
				AgentConfig: &api.ScionConfig{MessageMode: tc.snapshot},
			}))
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "hdmm-agent", ProjectID: project.ID, Template: "hdmm-tmpl", Task: "x",
				MessageMode: tc.requested,
			})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantMode, resp.Agent.MessageMode)
		})
	}
}
