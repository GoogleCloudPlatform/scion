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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawAppliedConfigView decodes just enough of an agent response body to
// check whether AppliedConfig.Env and AppliedConfig.InlineConfig.Env are
// present on the wire, without depending on the full response DTO shape.
type rawAppliedConfigView struct {
	AppliedConfig *struct {
		Env          map[string]string `json:"env"`
		InlineConfig *struct {
			Env map[string]string `json:"env"`
		} `json:"inlineConfig"`
	} `json:"appliedConfig"`
}

func assertEnvHidden(t *testing.T, view rawAppliedConfigView, context string) {
	t.Helper()
	if view.AppliedConfig == nil {
		return // appliedConfig itself absent is the strongest form of hidden
	}
	assert.Nil(t, view.AppliedConfig.Env, "%s: appliedConfig.env must be absent", context)
	if view.AppliedConfig.InlineConfig != nil {
		assert.Nil(t, view.AppliedConfig.InlineConfig.Env, "%s: appliedConfig.inlineConfig.env must be absent", context)
	}
}

func assertEnvVisibleMinusGitHubToken(t *testing.T, view rawAppliedConfigView, context string) {
	t.Helper()
	require.NotNil(t, view.AppliedConfig, "%s: appliedConfig must be present", context)
	require.NotNil(t, view.AppliedConfig.Env, "%s: appliedConfig.env must be present", context)
	assert.Equal(t, "plain-value", view.AppliedConfig.Env["PLAIN_VAR"], "%s: appliedConfig.env.PLAIN_VAR", context)
	_, hasToken := view.AppliedConfig.Env["GITHUB_TOKEN"]
	assert.False(t, hasToken, "%s: appliedConfig.env.GITHUB_TOKEN must never be present", context)

	require.NotNil(t, view.AppliedConfig.InlineConfig, "%s: appliedConfig.inlineConfig must be present", context)
	require.NotNil(t, view.AppliedConfig.InlineConfig.Env, "%s: appliedConfig.inlineConfig.env must be present", context)
	assert.Equal(t, "inline-plain-value", view.AppliedConfig.InlineConfig.Env["INLINE_PLAIN_VAR"], "%s: appliedConfig.inlineConfig.env.INLINE_PLAIN_VAR", context)
	_, hasInlineToken := view.AppliedConfig.InlineConfig.Env["GITHUB_TOKEN"]
	assert.False(t, hasInlineToken, "%s: appliedConfig.inlineConfig.env.GITHUB_TOKEN must never be present", context)
}

// TestListProjectAgentsResponseBody_EnvHiding pins env visibility for both
// AppliedConfig.Env and AppliedConfig.InlineConfig.Env in the
// listProjectAgents response body: hidden for a project member with no
// attach capability (plainMember), visible (minus GITHUB_TOKEN) for the
// project owner. This must fail if either field's hiding is removed.
func TestListProjectAgentsResponseBody_EnvHiding(t *testing.T) {
	listPath := func(f *projectAgentAuthzFixture) string {
		return "/api/v1/projects/" + f.project.ID + "/agents"
	}

	findTarget := func(t *testing.T, body []byte, targetID string) rawAppliedConfigView {
		t.Helper()
		var resp struct {
			Agents []json.RawMessage `json:"agents"`
		}
		require.NoError(t, json.Unmarshal(body, &resp))
		for _, raw := range resp.Agents {
			var idOnly struct {
				ID string `json:"id"`
			}
			require.NoError(t, json.Unmarshal(raw, &idOnly))
			if idOnly.ID != targetID {
				continue
			}
			var view rawAppliedConfigView
			require.NoError(t, json.Unmarshal(raw, &view))
			return view
		}
		t.Fatalf("target agent %s not found in response: %s", targetID, body)
		return rawAppliedConfigView{}
	}

	t.Run("plain project member (no attach) does not see env", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.plainMember, http.MethodGet, listPath(f), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		view := findTarget(t, rec.Body.Bytes(), f.target.ID)
		assertEnvHidden(t, view, "plainMember listProjectAgents")
	})

	t.Run("project owner sees env minus GITHUB_TOKEN", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, listPath(f), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		view := findTarget(t, rec.Body.Bytes(), f.target.ID)
		assertEnvVisibleMinusGitHubToken(t, view, "owner listProjectAgents")
	})
}

// TestGetProjectAgentSelfRead_EnvHiding covers m4's item 7: after the
// default-closed InlineConfig.Env treatment, an agent's own self-read of its
// project-scoped record must not return inlineConfig.env (or env) either --
// self-read only exempts the *authorization* check (isSelf in
// getProjectAgent), not the response-view env gate, which is keyed on
// attach capability and an agent reading itself has none. Peers and
// non-attach users must see the same thing.
func TestGetProjectAgentSelfRead_EnvHiding(t *testing.T) {
	getPath := func(f *projectAgentAuthzFixture) string {
		return f.targetPath()
	}

	t.Run("agent self-read does not see its own env", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, getPath(f), nil, f.selfToken(t))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var view rawAppliedConfigView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
		assertEnvHidden(t, view, "agent self-read getProjectAgent")
	})

	t.Run("a peer agent in the same project cannot read it at all", func(t *testing.T) {
		// getProjectAgent only exempts an agent reading its *own* record
		// (isSelf) from the ActionRead check; a peer agent goes through the
		// same authorize() call a user would and is denied outright here --
		// a stronger guarantee than "can read but env is hidden."
		f := projectAgentAuthzSetup(t)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, getPath(f), nil, f.callerToken(t))
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("a non-attach project member does not see it either", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.plainMember, http.MethodGet, getPath(f), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var view rawAppliedConfigView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
		assertEnvHidden(t, view, "plainMember getProjectAgent")
	})

	t.Run("the project owner sees env minus GITHUB_TOKEN", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, getPath(f), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var view rawAppliedConfigView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
		assertEnvVisibleMinusGitHubToken(t, view, "owner getProjectAgent")
	})
}
