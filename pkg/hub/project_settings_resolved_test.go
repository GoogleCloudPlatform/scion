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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawDoc is a helper for building an agent_defaults section document.
func rawDoc(t *testing.T, jsonText string) map[string]json.RawMessage {
	t.Helper()
	doc := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal([]byte(jsonText), &doc))
	return doc
}

// TestResolvedSettings_HubDefaultPresenceSemantics pins the tri-state rules
// that were established by measurement rather than by reading the Go types.
//
// The per-field split is not guessable from opsettings.AgentDefaultsSettings:
// it comes from settings-v1.schema.json. default_max_turns and
// default_max_model_calls carry "minimum": 1, so an explicit 0 is rejected by
// validation and can never be persisted — absence therefore means unset. The
// string fields have no minLength, so "" validates and is then dropped by the
// `omitempty` marshal on the admin write path, making absence ambiguous.
func TestResolvedSettings_HubDefaultPresenceSemantics(t *testing.T) {
	tests := []struct {
		name string
		key  string
		doc  string
		want ResolvedHubDefault
	}{
		{
			name: "int present",
			key:  projectSettingDefaultMaxTurns,
			doc:  `{"default_max_turns": 120}`,
			want: ResolvedHubDefaultPresent,
		},
		{
			// Unambiguous: schema minimum:1 means 0 can never reach the doc,
			// so a missing key really does mean "never configured".
			name: "int missing is absent, because zero is unpersistable",
			key:  projectSettingDefaultMaxTurns,
			doc:  `{}`,
			want: ResolvedHubDefaultAbsent,
		},
		{
			name: "string present",
			key:  projectSettingDefaultTemplate,
			doc:  `{"default_template": "base"}`,
			want: ResolvedHubDefaultPresent,
		},
		{
			// Ambiguous: an explicitly-configured "" is dropped by omitempty
			// before it is stored, so this is indistinguishable from unset.
			// Reporting "absent" here would be a false statement.
			name: "string missing is unknown, not absent",
			key:  projectSettingDefaultTemplate,
			doc:  `{}`,
			want: ResolvedHubDefaultUnknown,
		},
		{
			name: "duration missing is unknown",
			key:  projectSettingDefaultMaxDuration,
			doc:  `{}`,
			want: ResolvedHubDefaultUnknown,
		},
		{
			name: "nested resource leaf present",
			key:  projectSettingDefaultResourcesCPUReq,
			doc:  `{"default_resources": {"requests": {"cpu": "2"}}}`,
			want: ResolvedHubDefaultPresent,
		},
		{
			// default_resources is *api.ResourceSpec — a pointer, which CAN
			// represent unset — so its absence is faithful and reports absent
			// even though the leaf itself is an ambiguous string.
			name: "nested root missing is absent, because the root is a pointer",
			key:  projectSettingDefaultResourcesCPUReq,
			doc:  `{}`,
			want: ResolvedHubDefaultAbsent,
		},
		{
			name: "nested leaf missing under present root is unknown",
			key:  projectSettingDefaultResourcesCPUReq,
			doc:  `{"default_resources": {"requests": {"memory": "1Gi"}}}`,
			want: ResolvedHubDefaultUnknown,
		},
		{
			name: "nested disk present",
			key:  projectSettingDefaultResourcesDisk,
			doc:  `{"default_resources": {"disk": "10Gi"}}`,
			want: ResolvedHubDefaultPresent,
		},
		{
			name: "explicit json null counts as missing",
			key:  projectSettingDefaultMaxTurns,
			doc:  `{"default_max_turns": null}`,
			want: ResolvedHubDefaultAbsent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			desc, ok := resolvedSettingDescriptors[tc.key]
			require.Truef(t, ok, "no descriptor for %q", tc.key)
			got := hubDefaultFromDoc(rawDoc(t, tc.doc), desc)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolvedSettings_UnreadableSourceIsUnknownNotAbsent is the case a bool
// could not have expressed. In file/SQLite mode there is no OperationalSettings
// and no agent_defaults document exists to consult, so every key backed by that
// section is UNKNOWN. Reporting "absent" would tell a consumer that the hub has
// no default when in fact we never looked.
func TestResolvedSettings_UnreadableSourceIsUnknownNotAbsent(t *testing.T) {
	srv := &Server{} // no OperationalSettings: the file/SQLite path
	doc, readable := srv.hubAgentDefaultsDoc()
	require.False(t, readable, "a zero Server must report agent_defaults as unreadable")
	require.Nil(t, doc)

	resp := srv.resolvedProjectSettings(&store.Project{ID: "p-1"})

	for key, desc := range resolvedSettingDescriptors {
		if desc.source != hubSourceAgentDefaults {
			continue
		}
		assert.Equalf(t, ResolvedHubDefaultUnknown, resp.Settings[key].HubDefault,
			"%q is backed by agent_defaults, which could not be read; it must be "+
				"unknown rather than absent", key)
	}
}

// TestResolvedSettings_NoHubCounterpartIsAbsent covers the opposite case:
// AgentDefaultsSettings has exactly six fields, enumerated in full, so "there
// is no hub default for defaultModel" is a measured structural fact and may be
// reported as absent rather than unknown.
func TestResolvedSettings_NoHubCounterpartIsAbsent(t *testing.T) {
	resp := (&Server{}).resolvedProjectSettings(&store.Project{ID: "p-1"})

	for _, key := range []string{
		projectSettingDefaultModel,
		projectSettingDefaultThinkingLevel,
		projectSettingActiveProfile,
		projectSettingDefaultGCPIdentityMode,
		projectSettingDefaultGCPIdentitySAID,
	} {
		assert.Equalf(t, ResolvedHubDefaultAbsent, resp.Settings[key].HubDefault,
			"%q has no agent_defaults counterpart; that is a measured structural "+
				"fact, so absent is the honest answer", key)
	}
}

// TestResolvedSettings_TelemetryUsesItsRealSource checks that telemetry is
// answered from Server.config.TelemetryDefault rather than from agent_defaults.
//
// This matters because agent_defaults has no telemetry field at all, so the
// tempting answer is "absent" — which would be false, since a hub telemetry
// default does exist, just somewhere else. TelemetryDefault is a *bool and is
// therefore presence-faithful, so the right source can answer truthfully.
func TestResolvedSettings_TelemetryUsesItsRealSource(t *testing.T) {
	project := &store.Project{ID: "p-1"}

	unset := (&Server{}).resolvedProjectSettings(project)
	assert.Equal(t, ResolvedHubDefaultAbsent,
		unset.Settings[projectSettingTelemetryEnabled].HubDefault,
		"a nil TelemetryDefault means no hub telemetry default is configured")

	for _, configured := range []bool{true, false} {
		srv := &Server{}
		v := configured
		srv.config.TelemetryDefault = &v
		got := srv.resolvedProjectSettings(project)
		assert.Equalf(t, ResolvedHubDefaultPresent,
			got.Settings[projectSettingTelemetryEnabled].HubDefault,
			"TelemetryDefault=%v is configured, so a hub default is present. "+
				"An explicit false is still a configured default — this is exactly the "+
				"distinction the *bool exists to preserve.", configured)
	}
}

// TestResolvedSettings_ProjectAnnotationReporting checks the project half of
// each entry, including that an annotation explicitly set to the empty string
// is reported as set. ProjectSet is about the annotation's existence, not about
// whether its content is useful.
func TestResolvedSettings_ProjectAnnotationReporting(t *testing.T) {
	project := &store.Project{
		ID: "p-1",
		Annotations: map[string]string{
			projectSettingDefaultModel:    "sonnet",
			projectSettingActiveProfile:   "",
			projectSettingDefaultMaxTurns: "42",
		},
	}

	resp := (&Server{}).resolvedProjectSettings(project)

	model := resp.Settings[projectSettingDefaultModel]
	assert.True(t, model.ProjectSet)
	require.NotNil(t, model.ProjectValue)
	assert.Equal(t, "sonnet", *model.ProjectValue)

	empty := resp.Settings[projectSettingActiveProfile]
	assert.True(t, empty.ProjectSet, "an annotation set to \"\" is still set")
	require.NotNil(t, empty.ProjectValue)
	assert.Equal(t, "", *empty.ProjectValue)

	// Raw string, not parsed: the annotation store holds strings.
	turns := resp.Settings[projectSettingDefaultMaxTurns]
	require.NotNil(t, turns.ProjectValue)
	assert.Equal(t, "42", *turns.ProjectValue)

	unset := resp.Settings[projectSettingDefaultHarnessConfig]
	assert.False(t, unset.ProjectSet)
	assert.Nil(t, unset.ProjectValue, "an unset annotation must be null, not \"\"")
}

// TestResolvedSettings_EndpointReturnsNoEffectiveValue asserts on the actual
// serialized wire payload, not on the Go structs. The shape guard reflects over
// types; this checks what a client really receives, so that a marshalling
// surprise cannot slip an extra key past both.
func TestResolvedSettings_EndpointReturnsNoEffectiveValue(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/settings/resolved", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Contains(t, body, "project")
	require.Contains(t, body, "settings")
	assert.Len(t, body, 2, "unexpected top-level keys in the resolved response")

	var settings map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body["settings"], &settings))

	for _, key := range projectSettingKeys {
		entry, ok := settings[key]
		require.Truef(t, ok, "registered setting %q missing from the wire payload", key)

		// The forbidden names are checked here as a belt-and-braces read of the
		// real payload. The authoritative check is the exact-set guard in
		// project_settings_resolved_guard_test.go — this loop is a denylist and
		// would not catch a name nobody thought of.
		for _, forbidden := range []string{"value", "effective", "effectiveValue", "source", "hubValue"} {
			assert.NotContainsf(t, entry, forbidden,
				"the resolved response must not carry %q: this endpoint does not "+
					"resolve precedence", forbidden)
		}

		require.Contains(t, entry, "hubDefault")
		var state string
		require.NoError(t, json.Unmarshal(entry["hubDefault"], &state))
		assert.Containsf(t, []string{"present", "absent", "unknown"}, state,
			"hubDefault for %q must be one of the tri-state values, got %q", key, state)
	}
}

// TestResolvedSettings_EndpointProjectSubObject checks that the "project" key
// carries the same payload as GET /settings, so a client needs one round trip
// rather than two.
func TestResolvedSettings_EndpointProjectSubObject(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	putBody := hubclient.ProjectSettings{
		DefaultTemplate:      "my-template",
		DefaultHarnessConfig: "claude-default",
	}
	putRec := doRequest(t, srv, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, putRec.Code, "body: %s", putRec.Body.String())

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/settings/resolved", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resolved hubclient.ResolvedProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))

	require.NotNil(t, resolved.Project)
	assert.Equal(t, "my-template", resolved.Project.DefaultTemplate)
	assert.Equal(t, "claude-default", resolved.Project.DefaultHarnessConfig)

	entry := resolved.Settings[projectSettingDefaultTemplate]
	assert.True(t, entry.ProjectSet)
	require.NotNil(t, entry.ProjectValue)
	assert.Equal(t, "my-template", *entry.ProjectValue)
}

// TestResolvedSettings_EndpointIsReadOnly — there is no PUT. The whole reason
// this is a separate sub-resource is that a writable resolved view would let a
// GET-modify-PUT round trip promote hub defaults into project annotations.
func TestResolvedSettings_EndpointIsReadOnly(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		rec := doRequest(t, srv, method,
			"/api/v1/projects/"+project.ID+"/settings/resolved", hubclient.ProjectSettings{})
		assert.Equalf(t, http.StatusMethodNotAllowed, rec.Code,
			"%s on the resolved endpoint must be rejected; it is read-only", method)
	}
}

// TestResolvedSettings_EndpointUnknownProject checks the not-found path, so a
// missing project does not surface as an empty but successful response.
func TestResolvedSettings_EndpointUnknownProject(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/projects/does-not-exist/settings/resolved", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
}
