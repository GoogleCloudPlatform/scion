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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerPresets_Routes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("presets-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "presets-member@example.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}))
	member := NewAuthenticatedUser(memberID, "presets-member@example.com", "Member", store.UserRoleMember, "api")

	doAsMember := func(method, path string, rawBody []byte) *httptest.ResponseRecorder {
		t.Helper()
		var bodyReader *bytes.Reader
		if rawBody != nil {
			bodyReader = bytes.NewReader(rawBody)
		} else {
			bodyReader = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, bodyReader)
		if rawBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req = req.WithContext(contextWithCredentialContext(
			contextWithIdentity(ctx, member),
			CredentialContext{Kind: CredentialKindInteractive},
		))
		rr := httptest.NewRecorder()
		srv.mux.ServeHTTP(rr, req)
		return rr
	}

	// 1. Unauthenticated callers get 401 before experiment gate.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/trigger-presets?source=github"},
		{http.MethodPost, "/api/v1/trigger-presets/compile"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rr := httptest.NewRecorder()
		srv.mux.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusUnauthorized, rr.Code, "%s %s without auth", tc.method, tc.path)
	}

	// 2. Experiment off -> 404 for authenticated users on both GET and POST.
	rr := doAsMember(http.MethodGet, "/api/v1/trigger-presets?source=github", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())

	validBody, err := json.Marshal(map[string]any{
		"preset": "github.command",
		"fields": map[string]any{"command": "review"},
	})
	require.NoError(t, err)
	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets/compile", validBody)
	assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())

	// Enable hub.github_triggers.
	setGitHubTriggersExperimentForRouting(t, srv, true)

	// 3. Unsupported HTTP methods -> 405 Method Not Allowed with Allow header.
	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets", validBody)
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
	rr = doAsMember(http.MethodHead, "/api/v1/trigger-presets", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
	assert.Equal(t, http.MethodGet, rr.Header().Get("Allow"))
	rr = doAsMember(http.MethodGet, "/api/v1/trigger-presets/compile", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)

	// 4. GET /api/v1/trigger-presets?source=github returns the six preset schemas.
	rr = doAsMember(http.MethodGet, "/api/v1/trigger-presets?source=github", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var schemas []hubclient.TriggerPreset
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &schemas))
	require.Len(t, schemas, 6)
	assert.Equal(t, "github.command", schemas[0].ID)

	// Unfiltered GET also returns all registered presets; unknown valid source returns [].
	rr = doAsMember(http.MethodGet, "/api/v1/trigger-presets", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	rr = doAsMember(http.MethodGet, "/api/v1/trigger-presets?source=webhook", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.JSONEq(t, "[]", rr.Body.String())

	// Malformed source query parameter -> 400 content-free validation_error.
	rr = doAsMember(http.MethodGet, "/api/v1/trigger-presets?source=bad%2Fsource_evil_token", nil)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	assert.NotContains(t, rr.Body.String(), "evil_token")

	// 5. POST /api/v1/trigger-presets/compile succeeds and emits ordered match.rules + githubToken="installation".
	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets/compile", validBody)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var compiled hubclient.CompiledTriggerPreset
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &compiled))
	require.Len(t, compiled.Match.Rules, 1)
	assert.Equal(t, "cel", compiled.Match.Rules[0].Dialect)
	require.NotNil(t, compiled.SuggestedAction)
	assert.Equal(t, "installation", compiled.SuggestedAction.GitHubToken)
	assert.False(t, compiled.SuggestedAction.Ephemeral)

	// 6. POST /api/v1/trigger-presets/compile returns 400 with field names and content-free details.
	badBody, err := json.Marshal(map[string]any{
		"preset": "github.command",
		"fields": map[string]any{
			"command":              "review' || true || 'evil_marker",
			"allowed_associations": []string{},
			"bogus_field":          "evil_marker",
		},
	})
	require.NoError(t, err)
	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets/compile", badBody)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "evil_marker")

	var errEnv ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errEnv))
	assert.Equal(t, ErrCodeValidationError, errEnv.Error.Code)
	rawFields, ok := errEnv.Error.Details["fields"].([]any)
	require.True(t, ok, "expected error.details.fields array: %s", rr.Body.String())
	var errFieldNames []string
	for _, elem := range rawFields {
		m, isMap := elem.(map[string]any)
		require.True(t, isMap)
		name, _ := m["field"].(string)
		errFieldNames = append(errFieldNames, name)
	}
	assert.ElementsMatch(t, []string{"bogus_field", "command", "allowed_associations"}, errFieldNames)

	// Oversized compiled match expression (>4096 bytes) -> 400 naming the driving list field.
	twentyBranches := make([]string, 20)
	for i := range twentyBranches {
		prefix := fmt.Sprintf("release/evil_branch_marker_%02d_", i)
		twentyBranches[i] = prefix + strings.Repeat("b", 193-len(prefix))
	}
	oversizeExprBody, err := json.Marshal(map[string]any{
		"preset": "github.pr_opened",
		"fields": map[string]any{"base_branches": twentyBranches},
	})
	require.NoError(t, err)
	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets/compile", oversizeExprBody)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "evil_branch_marker")
	var oversizeEnv ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &oversizeEnv))
	assert.Equal(t, ErrCodeValidationError, oversizeEnv.Error.Code)
	oversizeFields, ok := oversizeEnv.Error.Details["fields"].([]any)
	require.True(t, ok, "expected error.details.fields array: %s", rr.Body.String())
	require.Len(t, oversizeFields, 1)
	oversizeFieldMap, ok := oversizeFields[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "base_branches", oversizeFieldMap["field"])
	assert.Equal(t, "too_long", oversizeFieldMap["code"])

	// Malformed JSON, unknown top-level JSON field, trailing tokens, and oversized body.
	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets/compile", []byte(`{"preset":`))
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets/compile", []byte(`{"preset":"github.pr_opened","secret_extra_key":1}`))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.NotContains(t, rr.Body.String(), "secret_extra_key")

	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets/compile", []byte(`{"preset":"github.pr_opened"}{"extra":true}`))
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	oversized := []byte(`{"preset":"github.pr_opened","fields":{"pad":"` + strings.Repeat("a", 70*1024) + `"}}`)
	rr = doAsMember(http.MethodPost, "/api/v1/trigger-presets/compile", oversized)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)

	// 7. End-to-end hubclient round-trip against live Server handler.
	tokUser := hubConfigTokenUser(t, s, "presets-tok-member", store.SystemRoleHubMember)
	tok := mintHubConfigToken(t, srv, tokUser, hubBoundary(), "user:read")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	hc, err := hubclient.New(ts.URL, hubclient.WithBearerToken(tok))
	require.NoError(t, err)

	listed, err := hc.TriggerPresets().List(ctx, "github")
	require.NoError(t, err)
	require.Len(t, listed, 6)

	compOut, err := hc.TriggerPresets().Compile(ctx, &hubclient.CompileTriggerPresetRequest{
		Preset: "github.pr_updated",
		Fields: map[string]any{"include_forks": true},
	})
	require.NoError(t, err)
	assert.Equal(t, "reuse", compOut.Subject.Mode)
	assert.Equal(t, "30s", compOut.Subject.CoalesceWindow)
	require.NotNil(t, compOut.SuggestedAction)
	assert.Empty(t, compOut.SuggestedAction.Branch)
	assert.Equal(t, "installation", compOut.SuggestedAction.GitHubToken)

	_, err = hc.TriggerPresets().Compile(ctx, &hubclient.CompileTriggerPresetRequest{
		Preset: "github.command",
		Fields: map[string]any{"command": "BAD"},
	})
	require.Error(t, err)
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)

	setGitHubTriggersExperimentForRouting(t, srv, false)
	_, err = hc.TriggerPresets().List(ctx, "github")
	require.Error(t, err)
	assert.True(t, apiclient.IsNotFoundError(err))
}

// TestTriggerPresets_AnyTokenReadsAndCompiles pins the admit_self bearer
// disposition and agent-token access on GET /api/v1/trigger-presets and
// POST /api/v1/trigger-presets/compile.
func TestTriggerPresets_AnyTokenReadsAndCompiles(t *testing.T) {
	srv, s, user, project := setupCredentialTestServer(t)
	setGitHubTriggersExperimentForRouting(t, srv, true)

	compileBody := map[string]any{
		"preset": "github.pr_opened",
	}

	for _, key := range authenticatedReadTokens(t, srv, s, "ar-trigger-presets") {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/trigger-presets?source=github", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/trigger-presets/compile", compileBody)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	// Agent token principal also reads and compiles presets.
	agentID := tid("ar-presets-agent")
	createCredTestAgent(t, s, agentID, project.ID, user.ID)
	agentTok, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleBaseline, nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/trigger-presets?source=github", nil)
	req.Header.Set("X-Scion-Agent-Token", agentTok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rawBody, err := json.Marshal(compileBody)
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/trigger-presets/compile", bytes.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", agentTok)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
