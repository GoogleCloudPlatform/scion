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

func TestTriggerPresets_UnknownFieldsCappedOverHTTP(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setGitHubTriggersExperimentForRouting(t, srv, true)

	memberID := tid("presets-s1-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "presets-s1@example.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}))
	member := NewAuthenticatedUser(memberID, "presets-s1@example.com", "Member", store.UserRoleMember, "api")

	fields := map[string]any{"command": "review"}
	for i := 0; i < 25; i++ {
		fields[fmt.Sprintf("unknown_%02d", i)] = "sensitive_value_do_not_echo"
	}
	rawBody, err := json.Marshal(map[string]any{
		"preset": "github.command",
		"fields": fields,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/trigger-presets/compile", bytes.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithCredentialContext(
		contextWithIdentity(ctx, member),
		CredentialContext{Kind: CredentialKindInteractive},
	))
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "sensitive_value_do_not_echo")

	var errEnv ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errEnv))
	rawFields, ok := errEnv.Error.Details["fields"].([]any)
	require.True(t, ok, "expected error.details.fields array: %s", rr.Body.String())
	require.Len(t, rawFields, 11)
	last, ok := rawFields[10].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "fields", last["field"])
	assert.Equal(t, "too_many_unknown_fields", last["code"])
	assert.Equal(t, "15 additional unknown fields omitted", last["message"])
}

func TestTriggerPresets_MatchFormRoundTripAndPatchClear(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setGitHubTriggersExperimentForRouting(t, srv, true)

	adminID := tid("preset-form-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "preset-form-admin@example.com", DisplayName: "Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now().UTC(),
	}))
	hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               tid("preset-form-admin-rb"),
		RoleDefinitionID: hubAdminRole.ID,
		PrincipalType:    "user",
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
	})
	require.NoError(t, err)
	adminAuth := NewAuthenticatedUser(adminID, "preset-form-admin@example.com", "Admin", store.UserRoleAdmin, "api")

	const instID int64 = 778899
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "octo-org",
		AccountType:    "Organization",
		AppID:          12345,
		Status:         store.GitHubInstallationStatusActive,
		Repositories:   []string{"octo-org/octo-repo"},
	}))

	instCopy := instID
	bindingRaw, err := json.Marshal(githubInstallationBinding{
		InstallationID: instID,
		Remote:         "github.com/octo-org/octo-repo",
		BoundBy:        adminID,
		BoundAt:        time.Now().UTC(),
	})
	require.NoError(t, err)

	projID := tid("preset-form-proj")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                   projID,
		Name:                 "preset-form-proj",
		Slug:                 "preset-form-proj",
		GitRemote:            "https://github.com/octo-org/octo-repo.git",
		GitHubInstallationID: &instCopy,
		Annotations: map[string]string{
			githubInstallationBindingAnnotation: string(bindingRaw),
		},
		CreatedBy: adminID,
	}))
	srv.invalidateGitHubAssociation(projID)

	targetAgentID := tid("preset-form-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        targetAgentID,
		Slug:      "lead",
		Name:      "lead",
		ProjectID: projID,
		Phase:     "running",
		OwnerID:   adminID,
	}))

	doJSON := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var bodyReader *bytes.Reader
		if body != nil {
			raw, mErr := json.Marshal(body)
			require.NoError(t, mErr)
			bodyReader = bytes.NewReader(raw)
		} else {
			bodyReader = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, bodyReader)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req = req.WithContext(contextWithCredentialContext(
			contextWithIdentity(ctx, adminAuth),
			CredentialContext{Kind: CredentialKindInteractive},
		))
		rr := httptest.NewRecorder()
		srv.mux.ServeHTTP(rr, req)
		return rr
	}
	triggersBase := "/api/v1/projects/" + projID + "/triggers"

	// 1. Compile github.command via /api/v1/trigger-presets/compile.
	cRec := doJSON(http.MethodPost, "/api/v1/trigger-presets/compile", map[string]any{
		"preset": "github.command",
		"fields": map[string]any{
			"command": "review",
			"on":      "pull_request",
		},
	})
	require.Equal(t, http.StatusOK, cRec.Code, cRec.Body.String())

	var compiled hubclient.CompiledTriggerPreset
	require.NoError(t, json.Unmarshal(cRec.Body.Bytes(), &compiled))
	formBytes, err := json.Marshal(compiled.Form)
	require.NoError(t, err)

	compiledRules := make([]TriggerMatchRuleSpec, len(compiled.Match.Rules))
	for i, r := range compiled.Match.Rules {
		compiledRules[i] = TriggerMatchRuleSpec{Dialect: r.Dialect, Expr: r.Expr}
	}

	// 2. Create trigger with compiled types, rules, subject, and form.
	createRec := doJSON(http.MethodPost, triggersBase, CreateTriggerRequest{
		Name:   "preset-review",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match: &TriggerMatchSpec{
			Types: compiled.Match.Types,
			Rules: compiledRules,
			Form:  json.RawMessage(formBytes),
		},
		Subject: &TriggerSubjectSpec{
			Mode: compiled.Subject.Mode,
			Expr: compiled.Subject.Expr,
		},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: targetAgentID,
			Message: "Handle /review",
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	require.NotEmpty(t, created.Match.Form)
	assert.JSONEq(t, string(formBytes), string(created.Match.Form))
	assert.Equal(t, compiledRules, created.Match.Rules)

	// 3. GET returns the stored Match.Form and Match.Rules.
	getRec := doJSON(http.MethodGet, triggersBase+"/"+created.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var fetched TriggerResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &fetched))
	assert.JSONEq(t, string(formBytes), string(fetched.Match.Form))
	assert.Equal(t, compiledRules, fetched.Match.Rules)

	// 4. PATCH of name/description preserves Match.Form.
	newDesc := "updated description"
	patchDescRec := doJSON(http.MethodPatch, triggersBase+"/"+created.ID, UpdateTriggerRequest{
		Description: &newDesc,
	})
	require.Equal(t, http.StatusOK, patchDescRec.Code, patchDescRec.Body.String())
	var afterDesc TriggerResponse
	require.NoError(t, json.Unmarshal(patchDescRec.Body.Bytes(), &afterDesc))
	assert.JSONEq(t, string(formBytes), string(afterDesc.Match.Form))

	// 5. Hand-editing match.rules via PATCH clears Match.Form.
	editedRules := []TriggerMatchRuleSpec{{Dialect: "cel", Expr: "event.attributes.command == 'review'"}}
	patchRulesRec := doJSON(http.MethodPatch, triggersBase+"/"+created.ID, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{
			Rules: &editedRules,
		},
	})
	require.Equal(t, http.StatusOK, patchRulesRec.Code, patchRulesRec.Body.String())
	var afterRules TriggerResponse
	require.NoError(t, json.Unmarshal(patchRulesRec.Body.Bytes(), &afterRules))
	assert.Empty(t, afterRules.Match.Form, "PATCH of match.rules must clear match.form")

	getAfterRulesRec := doJSON(http.MethodGet, triggersBase+"/"+created.ID, nil)
	require.Equal(t, http.StatusOK, getAfterRulesRec.Code)
	var fetchedAfterRules TriggerResponse
	require.NoError(t, json.Unmarshal(getAfterRulesRec.Body.Bytes(), &fetchedAfterRules))
	assert.Empty(t, fetchedAfterRules.Match.Form)

	// 6. PATCH of match.rules WITH an explicit new match.form stores the new form,
	//    and scalar match.expr PATCH clears it again.
	newFormRaw := json.RawMessage(`{"preset":"github.command","version":1,"fields":{"command":"fix"}}`)
	patchReapplyRec := doJSON(http.MethodPatch, triggersBase+"/"+created.ID, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{
			Rules: &compiledRules,
			Form:  TriggerMatchFormPatch{Set: true, Raw: newFormRaw},
		},
	})
	require.Equal(t, http.StatusOK, patchReapplyRec.Code, patchReapplyRec.Body.String())
	var afterReapply TriggerResponse
	require.NoError(t, json.Unmarshal(patchReapplyRec.Body.Bytes(), &afterReapply))
	assert.JSONEq(t, string(newFormRaw), string(afterReapply.Match.Form))

	editedExpr := "event.attributes.command == 'fix'"
	patchExprRec := doJSON(http.MethodPatch, triggersBase+"/"+created.ID, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{
			Expr: &editedExpr,
		},
	})
	require.Equal(t, http.StatusOK, patchExprRec.Code, patchExprRec.Body.String())
	var afterExpr TriggerResponse
	require.NoError(t, json.Unmarshal(patchExprRec.Body.Bytes(), &afterExpr))
	assert.Empty(t, afterExpr.Match.Form, "PATCH of scalar match.expr must clear match.form")

	// 7. Re-apply form, then explicit PATCH {"match":{"form":null}} clears it (O1).
	patchSetFormRec := doJSON(http.MethodPatch, triggersBase+"/"+created.ID, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{
			Form: TriggerMatchFormPatch{Set: true, Raw: newFormRaw},
		},
	})
	require.Equal(t, http.StatusOK, patchSetFormRec.Code, patchSetFormRec.Body.String())
	var afterSetForm TriggerResponse
	require.NoError(t, json.Unmarshal(patchSetFormRec.Body.Bytes(), &afterSetForm))
	assert.JSONEq(t, string(newFormRaw), string(afterSetForm.Match.Form))

	nullFormReq := httptest.NewRequest(http.MethodPatch, triggersBase+"/"+created.ID, bytes.NewReader([]byte(`{"match":{"form":null}}`)))
	nullFormReq.Header.Set("Content-Type", "application/json")
	nullFormReq = nullFormReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(ctx, adminAuth),
		CredentialContext{Kind: CredentialKindInteractive},
	))
	nullFormRec := httptest.NewRecorder()
	srv.mux.ServeHTTP(nullFormRec, nullFormReq)
	require.Equal(t, http.StatusOK, nullFormRec.Code, nullFormRec.Body.String())
	var afterNullForm TriggerResponse
	require.NoError(t, json.Unmarshal(nullFormRec.Body.Bytes(), &afterNullForm))
	assert.Empty(t, afterNullForm.Match.Form, "explicit PATCH match.form: null must clear match.form")

	getAfterNullRec := doJSON(http.MethodGet, triggersBase+"/"+created.ID, nil)
	require.Equal(t, http.StatusOK, getAfterNullRec.Code)
	var fetchedAfterNull TriggerResponse
	require.NoError(t, json.Unmarshal(getAfterNullRec.Body.Bytes(), &fetchedAfterNull))
	assert.Empty(t, fetchedAfterNull.Match.Form)

	// 8. Re-apply form, then PATCH changing only match.types clears match.form (O3).
	patchSetForm2Rec := doJSON(http.MethodPatch, triggersBase+"/"+created.ID, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{
			Form: TriggerMatchFormPatch{Set: true, Raw: newFormRaw},
		},
	})
	require.Equal(t, http.StatusOK, patchSetForm2Rec.Code, patchSetForm2Rec.Body.String())

	editedTypes := []string{"github.issue_comment.created"}
	patchTypesRec := doJSON(http.MethodPatch, triggersBase+"/"+created.ID, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{
			Types: &editedTypes,
		},
	})
	require.Equal(t, http.StatusOK, patchTypesRec.Code, patchTypesRec.Body.String())
	var afterTypes TriggerResponse
	require.NoError(t, json.Unmarshal(patchTypesRec.Body.Bytes(), &afterTypes))
	assert.Empty(t, afterTypes.Match.Form, "PATCH changing only match.types must clear match.form")

	// 9. 16 KiB maxTriggerMatchFormBytes boundary: exact limit accepted, limit+1 refused (O3).
	const formPrefix = `{"p":"`
	const formSuffix = `"}`
	exactForm := json.RawMessage(formPrefix + strings.Repeat("a", maxTriggerMatchFormBytes-len(formPrefix)-len(formSuffix)) + formSuffix)
	require.Len(t, exactForm, maxTriggerMatchFormBytes)
	atLimitRec := doJSON(http.MethodPost, triggersBase, CreateTriggerRequest{
		Name:   "preset-form-at-limit",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match: &TriggerMatchSpec{
			Types: compiled.Match.Types,
			Rules: compiledRules,
			Form:  exactForm,
		},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: targetAgentID,
			Message: "hi",
		},
	})
	require.Equal(t, http.StatusCreated, atLimitRec.Code, atLimitRec.Body.String())
	var atLimitResp TriggerResponse
	require.NoError(t, json.Unmarshal(atLimitRec.Body.Bytes(), &atLimitResp))
	assert.JSONEq(t, string(exactForm), string(atLimitResp.Match.Form))

	overLimitForm := json.RawMessage(formPrefix + strings.Repeat("a", maxTriggerMatchFormBytes-len(formPrefix)-len(formSuffix)+1) + formSuffix)
	require.Len(t, overLimitForm, maxTriggerMatchFormBytes+1)
	overLimitRec := doJSON(http.MethodPost, triggersBase, CreateTriggerRequest{
		Name:   "preset-form-over-limit",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match: &TriggerMatchSpec{
			Types: compiled.Match.Types,
			Rules: compiledRules,
			Form:  overLimitForm,
		},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: targetAgentID,
			Message: "hi",
		},
	})
	assert.Equal(t, http.StatusBadRequest, overLimitRec.Code, overLimitRec.Body.String())

	// 10. Invalid match.form (non-object array and non-UTF-8 bytes) returns 400 (O3).
	badRec := doJSON(http.MethodPost, triggersBase, CreateTriggerRequest{
		Name:   "preset-bad-form",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match: &TriggerMatchSpec{
			Types: compiled.Match.Types,
			Rules: compiledRules,
			Form:  json.RawMessage(`["not","an","object"]`),
		},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: targetAgentID,
			Message: "hi",
		},
	})
	assert.Equal(t, http.StatusBadRequest, badRec.Code, badRec.Body.String())

	_, utf8Err := validateTriggerMatchForm(json.RawMessage("{\"k\":\"\xff\"}"))
	require.Error(t, utf8Err)
	assert.Contains(t, utf8Err.Error(), "UTF-8")

	nonUTF8Body := append([]byte(`{"name":"preset-non-utf8","source":{"kind":"github","id":""},"match":{"types":["github.issue_comment.created"],"form":{"k":"`), 0xff)
	nonUTF8Body = append(nonUTF8Body, []byte(`"}},"action":{"type":"message","agentId":"`+targetAgentID+`","message":"hi"}}`)...)
	nonUTF8Req := httptest.NewRequest(http.MethodPost, triggersBase, bytes.NewReader(nonUTF8Body))
	nonUTF8Req.Header.Set("Content-Type", "application/json")
	nonUTF8Req = nonUTF8Req.WithContext(contextWithCredentialContext(
		contextWithIdentity(ctx, adminAuth),
		CredentialContext{Kind: CredentialKindInteractive},
	))
	nonUTF8Rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(nonUTF8Rec, nonUTF8Req)
	assert.Equal(t, http.StatusBadRequest, nonUTF8Rec.Code, nonUTF8Rec.Body.String())
}
