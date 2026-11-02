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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedRequest is one request a test server saw.
type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]json.RawMessage
}

// settingsTestServer answers every request with status and body, and
// records the requests.
func settingsTestServer(t *testing.T, status int, body string) (AdminSettingsService, *[]recordedRequest) {
	t.Helper()
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			require.NoError(t, json.Unmarshal(b, &rec.Body), "request body %s", b)
		}
		got = append(got, rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	require.NoError(t, err)
	return c.AdminSettings(), &got
}

func TestUpdateServerConfig_SendsSparseBodyWithExpectedRevisions(t *testing.T) {
	svc, got := settingsTestServer(t, http.StatusOK,
		`{"status":"saved","reload":{"applied":["agent_defaults"],"requires_restart":[],"applied_keys":["default_max_turns"],"pending_restart":[],"skipped":[{"key":"k","reason":"r"}],"file_applied":[]},"unchanged":["endpoints"],"warnings":["w1"]}`)

	res, err := svc.UpdateServerConfig(context.Background(),
		map[string]interface{}{
			"default_max_turns": 200,
			"server":            map[string]interface{}{"hub": map[string]interface{}{"public_url": nil}},
		},
		map[string]int64{"agent_defaults": 7, "endpoints": 0})
	require.NoError(t, err)

	require.Len(t, *got, 1)
	req := (*got)[0]
	assert.Equal(t, http.MethodPut, req.Method)
	assert.Equal(t, "/api/v1/admin/server-config", req.Path)
	assert.JSONEq(t, `200`, string(req.Body["default_max_turns"]))
	assert.JSONEq(t, `{"hub":{"public_url":null}}`, string(req.Body["server"]), "an explicit null is sent, not dropped")
	assert.JSONEq(t, `{"agent_defaults":7,"endpoints":0}`, string(req.Body["expected_revisions"]))
	assert.Len(t, req.Body, 3, "only the sent keys and expected_revisions")

	assert.Equal(t, []string{"agent_defaults"}, res.Reload.Applied)
	assert.Equal(t, []string{"default_max_turns"}, res.Reload.AppliedKeys)
	assert.Equal(t, []SettingsSkippedKey{{Key: "k", Reason: "r"}}, res.Reload.Skipped)
	assert.Equal(t, []string{"endpoints"}, res.Unchanged)
	assert.Equal(t, []string{"w1"}, res.Warnings)
}

func TestUpdateServerConfig_NoRevisionsOmitsMember(t *testing.T) {
	svc, got := settingsTestServer(t, http.StatusOK, `{"status":"saved","reload":{}}`)
	_, err := svc.UpdateServerConfig(context.Background(), map[string]interface{}{"default_model": "m"}, nil)
	require.NoError(t, err)
	_, present := (*got)[0].Body["expected_revisions"]
	assert.False(t, present)
}

func TestUpdateServerConfig_RefusesExpectedRevisionsInBody(t *testing.T) {
	svc, got := settingsTestServer(t, http.StatusOK, `{}`)
	_, err := svc.UpdateServerConfig(context.Background(), map[string]interface{}{"expected_revisions": map[string]int64{"x": 1}}, nil)
	require.Error(t, err)
	assert.Empty(t, *got, "nothing is sent")
}

func TestGetServerConfig_KeepsRawMembersAndLookup(t *testing.T) {
	svc, _ := settingsTestServer(t, http.StatusOK,
		`{"settings_tier":"db","layer0_editable":false,"default_max_turns":50,"server":{"hub":{"public_url":"https://hub.example"}},"federation":{"enabled":true},"section_metadata":{"agent_defaults":{"source":"db","revision":4}}}`)
	cfg, err := svc.GetServerConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "db", cfg.SettingsTier)
	assert.Equal(t, int64(4), cfg.SectionMetadata["agent_defaults"].Revision)

	v, ok := cfg.Lookup("server.hub.public_url")
	require.True(t, ok)
	assert.JSONEq(t, `"https://hub.example"`, string(v))
	v, ok = cfg.Lookup("default_max_turns")
	require.True(t, ok)
	assert.JSONEq(t, `50`, string(v))
	_, ok = cfg.Lookup("server.hub.missing")
	assert.False(t, ok)
	_, ok = cfg.Lookup("default_max_turns.below_a_scalar")
	assert.False(t, ok)

	b, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"federation"`, "marshal returns the response as received")
}

func TestResetServerConfigSection_PathAndError(t *testing.T) {
	svc, got := settingsTestServer(t, http.StatusNoContent, ``)
	require.NoError(t, svc.ResetServerConfigSection(context.Background(), "telemetry"))
	assert.Equal(t, http.MethodDelete, (*got)[0].Method)
	assert.Equal(t, "/api/v1/admin/server-config/sections/telemetry", (*got)[0].Path)

	svc, _ = settingsTestServer(t, http.StatusBadRequest, `{"error":{"code":"validation_failed","message":"use the experiments endpoint"}}`)
	err := svc.ResetServerConfigSection(context.Background(), "experiments")
	var se *SettingsError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "validation_failed", se.API.Code)
	assert.Equal(t, "use the experiments endpoint", se.API.Message)

	require.Error(t, svc.ResetServerConfigSection(context.Background(), ""))
}

func TestGetServerConfigSchema(t *testing.T) {
	svc, got := settingsTestServer(t, http.StatusOK,
		`{"sections":{"endpoints":{"schema":{"type":"object"},"koanf_paths":["server.hub.public_url"],"restart_required":["server.hub.public_url"]}}}`)
	schema, err := svc.GetServerConfigSchema(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/admin/server-config/schema", (*got)[0].Path)
	assert.Equal(t, []string{"server.hub.public_url"}, schema["endpoints"].KoanfPaths)
	assert.Equal(t, []string{"server.hub.public_url"}, schema["endpoints"].RestartRequired)
	assert.JSONEq(t, `{"type":"object"}`, string(schema["endpoints"].Schema))
}

func TestProjectDefaults_GetAndUpdate(t *testing.T) {
	svc, got := settingsTestServer(t, http.StatusOK, `{"default_scratchpad":true}`)
	cur, err := svc.GetProjectDefaults(context.Background())
	require.NoError(t, err)
	assert.JSONEq(t, `true`, string(cur["default_scratchpad"]))

	_, err = svc.UpdateProjectDefaults(context.Background(), map[string]interface{}{"default_scratchpad": nil})
	require.NoError(t, err)
	req := (*got)[1]
	assert.Equal(t, http.MethodPut, req.Method)
	assert.Equal(t, "/api/v1/admin/project-defaults", req.Path)
	assert.JSONEq(t, `null`, string(req.Body["default_scratchpad"]))
}

func TestExperiments_UpdateAndReset(t *testing.T) {
	body := `{"revision":3,"malformed":false,"experiments":[{"name":"x","default":false,"override":true,"enabled":true}],"unknown_overrides":{}}`
	svc, got := settingsTestServer(t, http.StatusOK, body)

	on := true
	res, err := svc.UpdateExperiments(context.Background(), map[string]*bool{"x": &on, "y": nil}, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(3), res.Revision)
	req := (*got)[0]
	assert.Equal(t, http.MethodPut, req.Method)
	assert.Equal(t, "/api/v1/admin/experiments", req.Path)
	assert.JSONEq(t, `{"x":true,"y":null}`, string(req.Body["overrides"]))
	assert.JSONEq(t, `3`, string(req.Body["expected_revision"]))

	_, err = svc.ResetExperiments(context.Background(), 3, false)
	require.NoError(t, err)
	req = (*got)[1]
	assert.Equal(t, http.MethodDelete, req.Method)
	assert.JSONEq(t, `3`, string(req.Body["expected_revision"]))
	_, present := req.Body["confirm_reset_malformed"]
	assert.False(t, present)

	_, err = svc.ResetExperiments(context.Background(), 0, true)
	require.NoError(t, err)
	assert.JSONEq(t, `true`, string((*got)[2].Body["confirm_reset_malformed"]))
}

// --- error envelope -------------------------------------------------------

func settingsErrorFrom(t *testing.T, status int, body string) *SettingsError {
	t.Helper()
	svc, _ := settingsTestServer(t, status, body)
	_, err := svc.UpdateServerConfig(context.Background(), map[string]interface{}{"k": 1}, nil)
	require.Error(t, err)
	var se *SettingsError
	require.ErrorAs(t, err, &se)
	return se
}

func TestSettingsError_Structured422(t *testing.T) {
	se := settingsErrorFrom(t, http.StatusUnprocessableEntity,
		`{"error":{"code":"validation_error","message":"server.auth.default_user_role: must be one of","details":{"field":"server.auth.default_user_role","errors":[{"section":"access","field":"server.auth.default_user_role","path":"/default_user_role","message":"must be one of"},{"field":"default_max_turns","message":"must be >= 0"}]}}}`)
	assert.Equal(t, http.StatusUnprocessableEntity, se.StatusCode())
	assert.Equal(t, "validation_error", se.API.Code)
	require.Len(t, se.Errors, 2)
	assert.Equal(t, SettingsFieldError{Section: "access", Field: "server.auth.default_user_role", Path: "/default_user_role", Message: "must be one of"}, se.Errors[0])
	assert.Equal(t, "default_max_turns", se.Errors[1].Location())
	assert.Equal(t, "must be >= 0", se.Errors[1].Message)
}

func TestSettingsError_StructuredKeyRefusal(t *testing.T) {
	se := settingsErrorFrom(t, http.StatusUnprocessableEntity,
		`{"error":{"code":"layer0_rejected","message":"keys are not editable","details":{"keys":["server.port","server.db.url"],"errors":[{"field":"server.port","message":"startup only"},{"field":"server.db.url","message":"startup only"}]}}}`)
	assert.Equal(t, "layer0_rejected", se.API.Code)
	assert.Equal(t, []string{"server.port", "server.db.url"}, se.Keys)
	require.Len(t, se.Errors, 2)
	assert.Equal(t, "server.db.url", se.Errors[1].Field)
}

func TestSettingsError_StructuredLoneField(t *testing.T) {
	se := settingsErrorFrom(t, http.StatusUnprocessableEntity,
		`{"error":{"code":"validation_error","message":"invalid duration","details":{"field":"server.hub.stalled_threshold"}}}`)
	require.Len(t, se.Errors, 1)
	assert.Equal(t, SettingsFieldError{Field: "server.hub.stalled_threshold", Message: "invalid duration"}, se.Errors[0])
}

func TestSettingsError_Structured409(t *testing.T) {
	se := settingsErrorFrom(t, http.StatusConflict,
		`{"error":{"code":"revision_conflict","message":"modified","details":{"conflicted":[{"section":"lifecycle","expected_revision":1,"current_revision":2}],"applied":{"access":2},"unchanged_revisions":{"endpoints":5},"applied_keys":["server.hub.admin_emails"],"pending_restart":["server.hub.public_url"],"skipped":[{"key":"a","reason":"b"}]}}}`)
	assert.True(t, se.IsRevisionConflict())
	assert.True(t, apiclient.IsConflictError(se), "unwraps to the API error")
	assert.Equal(t, []SettingsRevisionConflict{{Section: "lifecycle", ExpectedRevision: 1, CurrentRevision: 2}}, se.Conflicts)
	assert.Equal(t, map[string]int64{"access": 2}, se.Applied)
	assert.Equal(t, map[string]int64{"endpoints": 5}, se.UnchangedRevisions)
	assert.Equal(t, []string{"server.hub.admin_emails"}, se.AppliedKeys)
	assert.Equal(t, []string{"server.hub.public_url"}, se.PendingRestart)
	assert.Equal(t, []SettingsSkippedKey{{Key: "a", Reason: "b"}}, se.Skipped)
}

func TestSettingsError_Flat400ValidationBySection(t *testing.T) {
	// The earlier form: a string error code and errors grouped by section,
	// each entry with the untagged Path and Message members.
	se := settingsErrorFrom(t, http.StatusBadRequest,
		`{"error":"validation_failed","errors":{"lifecycle":[{"Path":"/stalled_threshold","Message":"bad"}],"access":[{"Path":"/default_user_role","Message":"must be one of"}]}}`)
	assert.Equal(t, "validation_failed", se.API.Code)
	require.Len(t, se.Errors, 2)
	assert.Equal(t, SettingsFieldError{Section: "access", Path: "/default_user_role", Message: "must be one of"}, se.Errors[0], "sorted by section")
	assert.Equal(t, "access:default_user_role", se.Errors[0].Location())
	assert.Equal(t, "lifecycle", se.Errors[1].Section)
}

func TestSettingsError_Flat409(t *testing.T) {
	se := settingsErrorFrom(t, http.StatusConflict,
		`{"error":"revision_conflict","message":"One or more sections have been modified since the expected revision.","applied":{"access":2},"unchanged_revisions":{},"conflicted":[{"section":"lifecycle","expected_revision":1,"current_revision":3}],"applied_keys":[],"pending_restart":[],"skipped":[]}`)
	assert.Equal(t, "revision_conflict", se.API.Code)
	assert.Equal(t, "One or more sections have been modified since the expected revision.", se.API.Message)
	assert.Equal(t, []SettingsRevisionConflict{{Section: "lifecycle", ExpectedRevision: 1, CurrentRevision: 3}}, se.Conflicts)
	assert.Equal(t, map[string]int64{"access": 2}, se.Applied)
}

func TestSettingsError_ListOfFlatEntries(t *testing.T) {
	// The experiments endpoint lists untagged entries under details.errors.
	se := settingsErrorFrom(t, http.StatusBadRequest,
		`{"error":{"code":"validation_failed","message":"Invalid experiment settings","details":{"errors":[{"Path":"/overrides/x","Message":"bad"}]}}}`)
	require.Len(t, se.Errors, 1)
	assert.Equal(t, "overrides/x", se.Errors[0].Location())
}

func TestSettingsError_PlainAndForbidden(t *testing.T) {
	se := settingsErrorFrom(t, http.StatusForbidden, `{"error":{"code":"forbidden","message":"Forbidden"}}`)
	assert.True(t, apiclient.IsForbiddenError(se))
	assert.Empty(t, se.Errors)

	se = settingsErrorFrom(t, http.StatusBadGateway, `upstream down`)
	assert.Equal(t, "upstream down", se.API.Message)
	assert.Equal(t, apiclient.ErrCodeInternalError, se.API.Code)

	se = settingsErrorFrom(t, http.StatusConflict, ``)
	assert.Equal(t, apiclient.ErrCodeConflict, se.API.Code)
	assert.True(t, errors.Is(se, se.API))
}

func TestSettingsError_Non409RevisionConflictsAreNotRevisionConflicts(t *testing.T) {
	// Other 409 refusals the server-config write returns: a refused GCP
	// permission-check change and a settings file that needs migrating.
	// They are conflicts (any 409) but not revision conflicts.
	cases := map[string]string{
		"gcp_iam_change_refused": `{"error":{"code":"gcp_iam_change_refused","message":"relaxing the GCP permission check is refused"}}`,
		"legacy_server_yaml":     `{"error":{"code":"legacy_server_yaml","message":"The server configuration is still read from the deprecated server.yaml."}}`,
		"flat legacy form":       `{"error":"legacy_server_yaml","message":"The server configuration is still read from the deprecated server.yaml."}`,
	}
	for name, body := range cases {
		se := settingsErrorFrom(t, http.StatusConflict, body)
		assert.False(t, se.IsRevisionConflict(), name)
		assert.True(t, apiclient.IsConflictError(se), "%s: still a 409 conflict", name)
		assert.NotEmpty(t, se.API.Message, name)
	}

	// Positive controls: the code alone, or a conflicted list alone, is a
	// revision conflict (the experiments endpoint sends the code with no
	// list).
	se := settingsErrorFrom(t, http.StatusConflict, `{"error":{"code":"revision_conflict","message":"modified concurrently"}}`)
	assert.True(t, se.IsRevisionConflict())
	se = settingsErrorFrom(t, http.StatusConflict, `{"error":{"code":"conflict","message":"m","details":{"conflicted":[{"section":"access","expected_revision":1,"current_revision":2}]}}}`)
	assert.True(t, se.IsRevisionConflict())
	// A revision_conflict code on another status is not one.
	se = settingsErrorFrom(t, http.StatusBadRequest, `{"error":{"code":"revision_conflict","message":"m"}}`)
	assert.False(t, se.IsRevisionConflict())
}
