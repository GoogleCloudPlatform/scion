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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// rawCreateInputsView decodes just the createInputs inline env of an agent
// response body.
type rawCreateInputsView struct {
	AppliedConfig *struct {
		CreateInputs *struct {
			HarnessConfig string `json:"harnessConfig"`
			InlineConfig  *struct {
				Env map[string]string `json:"env"`
			} `json:"inlineConfig"`
		} `json:"createInputs"`
	} `json:"appliedConfig"`
}

// withCreateInputs stores create inputs carrying explicit inline env on the
// fixture's target agent.
func withCreateInputs(t *testing.T, f *projectAgentAuthzFixture) {
	t.Helper()
	ctx := context.Background()
	agent, err := f.srv.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	agent.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
		HarnessConfig: "claude",
		InlineConfig: &api.ScionConfig{
			Env: map[string]string{"CREATE_PLAIN_VAR": "create-plain-value", "GITHUB_TOKEN": "ghp_create_input_value"},
		},
	}
	require.NoError(t, f.srv.store.UpdateAgent(ctx, agent))
}

// TestGetProjectAgentResponseBody_CreateInputsEnvHiding pins create-input env
// visibility in the agent GET response body: hidden for a project member with
// no attach capability, visible minus GITHUB_TOKEN for the project owner.
func TestGetProjectAgentResponseBody_CreateInputsEnvHiding(t *testing.T) {
	get := func(t *testing.T, f *projectAgentAuthzFixture, user *store.User) rawCreateInputsView {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, user, http.MethodGet, f.targetPath(), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var view rawCreateInputsView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
		require.NotNil(t, view.AppliedConfig, "appliedConfig must be present: %s", rec.Body.String())
		require.NotNil(t, view.AppliedConfig.CreateInputs, "createInputs must be present: %s", rec.Body.String())
		assert.Equal(t, "claude", view.AppliedConfig.CreateInputs.HarnessConfig)
		return view
	}

	t.Run("a non-attach project member does not see create-input env", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		withCreateInputs(t, f)
		view := get(t, f, f.plainMember)
		if ic := view.AppliedConfig.CreateInputs.InlineConfig; ic != nil {
			assert.Nil(t, ic.Env, "appliedConfig.createInputs.inlineConfig.env must be absent")
		}
	})

	t.Run("the project owner sees create-input env minus GITHUB_TOKEN", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		withCreateInputs(t, f)
		view := get(t, f, f.member)
		ic := view.AppliedConfig.CreateInputs.InlineConfig
		require.NotNil(t, ic)
		assert.Equal(t, "create-plain-value", ic.Env["CREATE_PLAIN_VAR"])
		assert.NotContains(t, ic.Env, "GITHUB_TOKEN")
	})
}
