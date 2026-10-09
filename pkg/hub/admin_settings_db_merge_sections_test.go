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

// Tests for the telemetry and agent_defaults sections on
// mergeSectionOnCurrent (ptone/scion#3898, ptone/scion#3717,
// ptone/scion#3719).

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storedTelemetry has every nested object the Server Config page does not
// show: cloud.headers, cloud.tls, cloud.batch, filter and resource.
const storedTelemetry = `{
	"enabled": true,
	"cloud": {
		"enabled": true,
		"endpoint": "otel.example.com:4317",
		"protocol": "grpc",
		"provider": "generic",
		"headers": {"x-api-key": "k1"},
		"tls": {"enabled": true, "ca_file": "/etc/otel/ca.pem"},
		"batch": {"max_size": 512, "timeout": "5s"}
	},
	"hub": {"enabled": true, "report_interval": "30s"},
	"local": {"enabled": false},
	"filter": {
		"enabled": true,
		"events": {"include": ["agent.start", "agent.stop"]},
		"sampling": {"default": 0.5, "rates": {"agent.tool": 0.1}}
	},
	"resource": {"service.name": "scion-hub"}
}`

// pageTelemetryBody is the telemetry object the Server Config page sends
// (buildLayer1Payload): cloud without headers, tls or batch, and no filter
// or resource.
const pageTelemetryBody = `{"telemetry": {
	"enabled": true,
	"cloud": {"enabled": true, "endpoint": "otel.example.com:4318", "protocol": "http", "provider": "generic", "gcp_project_id": null, "cloud_logging": false},
	"hub": {"enabled": true, "report_interval": "60s"},
	"local": {"enabled": false, "file": "", "console": false}
}}`

func jsonMap(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(s), &m))
	return m
}

func sectionRowRaw(t *testing.T, f *fakeHubSettingStore, section string) map[string]interface{} {
	t.Helper()
	f.mu.Lock()
	row := f.settings[section]
	f.mu.Unlock()
	require.NotNil(t, row, "%s row missing", section)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(row.Value, &m))
	return m
}

// mergeTelemetry runs mergeSectionOnCurrent for telemetry the way the
// server-config PUT does, and returns the merged document as a map.
func mergeTelemetry(t *testing.T, ops *OperationalSettings, body string) (map[string]interface{}, int64) {
	t.Helper()
	var req ServerConfigUpdateDBRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	fp, err := parseFieldPresence([]byte(body))
	require.NoError(t, err)
	reqDoc, err := buildSingleSectionDoc(&req.ServerConfigUpdateRequest, "telemetry", fp)
	require.NoError(t, err)
	doc, rev, err := mergeSectionOnCurrent(context.Background(), ops, "telemetry", reqDoc, telemetryPresence([]byte(body)))
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(doc, &m))
	return m, rev
}

// mergeAgentDefaults runs mergeSectionOnCurrent for agent_defaults the way
// the server-config PUT does, and returns the merged document as a map.
func mergeAgentDefaults(t *testing.T, ops *OperationalSettings, body string) map[string]interface{} {
	t.Helper()
	var req ServerConfigUpdateDBRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	fp, err := parseFieldPresence([]byte(body))
	require.NoError(t, err)
	reqDoc, err := buildSingleSectionDoc(&req.ServerConfigUpdateRequest, "agent_defaults", fp)
	require.NoError(t, err)
	doc, _, err := mergeSectionOnCurrent(context.Background(), ops, "agent_defaults", reqDoc, agentDefaultsPresence(fp))
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(doc, &m))
	return m
}

// objAt returns the object at path in m, failing the test if it is
// missing.
func objAt(t *testing.T, m map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	cur := m
	for _, p := range path {
		next, ok := cur[p].(map[string]interface{})
		require.True(t, ok, "missing object %v", path)
		cur = next
	}
	return cur
}

func TestMergeSectionOnCurrent_TelemetryDeepMerge(t *testing.T) {
	for _, tc := range []struct {
		name string
		tel  string
		edit func(t *testing.T, m map[string]interface{})
	}{
		{"omitted nested objects are kept", `{"cloud": {"endpoint": "e2"}}`, func(t *testing.T, m map[string]interface{}) {
			objAt(t, m, "cloud")["endpoint"] = "e2"
		}},
		{"nested null clears that object", `{"cloud": {"tls": null}}`, func(t *testing.T, m map[string]interface{}) {
			delete(objAt(t, m, "cloud"), "tls")
		}},
		{"merge reaches every depth", `{"cloud": {"tls": {"ca_file": ""}}}`, func(t *testing.T, m map[string]interface{}) {
			delete(objAt(t, m, "cloud", "tls"), "ca_file")
		}},
		{"explicit false is stored", `{"cloud": {"tls": {"enabled": false}}}`, func(t *testing.T, m map[string]interface{}) {
			objAt(t, m, "cloud", "tls")["enabled"] = false
		}},
		{"a map is replaced whole", `{"resource": {"deployment.environment": "prod"}}`, func(t *testing.T, m map[string]interface{}) {
			m["resource"] = map[string]interface{}{"deployment.environment": "prod"}
		}},
		{"an empty map clears it", `{"cloud": {"headers": {}}}`, func(t *testing.T, m map[string]interface{}) {
			delete(objAt(t, m, "cloud"), "headers")
		}},
		{"an array is replaced whole", `{"filter": {"events": {"include": ["agent.error"]}}}`, func(t *testing.T, m map[string]interface{}) {
			objAt(t, m, "filter", "events")["include"] = []interface{}{"agent.error"}
		}},
		{"an empty object for a struct changes nothing", `{"filter": {}}`, func(*testing.T, map[string]interface{}) {}},
		{"top-level null clears the object", `{"filter": null}`, func(t *testing.T, m map[string]interface{}) { delete(m, "filter") }},
		{"clearing the last nested key removes the object", `{"local": {"enabled": null}}`, func(t *testing.T, m map[string]interface{}) {
			delete(m, "local")
		}},
		{"case-insensitive keys merge", `{"Cloud": {"Endpoint": "e3"}}`, func(t *testing.T, m map[string]interface{}) {
			objAt(t, m, "cloud")["endpoint"] = "e3"
		}},
		{"empty telemetry object changes nothing", `{}`, func(*testing.T, map[string]interface{}) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fakeStore, ops := newTestDBServer(t)
			fakeStore.seedWithOrigin("telemetry", json.RawMessage(storedTelemetry), "managed")

			got, rev := mergeTelemetry(t, ops, `{"telemetry": `+tc.tel+`}`)
			want := jsonMap(t, storedTelemetry)
			tc.edit(t, want)
			assert.Equal(t, want, got)
			assert.Equal(t, int64(1), rev, "the base revision is the row revision")
		})
	}
}

// With no row the save creates the row from the sent keys only.
func TestMergeSectionOnCurrent_TelemetryNoRow(t *testing.T) {
	_, _, ops := newTestDBServer(t)
	got, rev := mergeTelemetry(t, ops, `{"telemetry": {"cloud": {"endpoint": "e1", "tls": {"enabled": true}}, "filter": null}}`)
	assert.Equal(t, jsonMap(t, `{"cloud": {"endpoint": "e1", "tls": {"enabled": true}}}`), got)
	assert.Equal(t, int64(0), rev)
}

// An env var that pins a nested telemetry key drops only that key from a
// seeded row; the object holding it keeps its other keys. A managed row
// is carried as is.
func TestMergeSectionOnCurrent_TelemetryEnvPinnedNestedKey(t *testing.T) {
	_, fakeStore, ops := newTestDBServer(t)
	ops.envOverrides = map[string]bool{"telemetry.cloud.enabled": true}
	fakeStore.seedWithOrigin("telemetry", json.RawMessage(storedTelemetry), "seeded")

	got, _ := mergeTelemetry(t, ops, `{"telemetry": {"enabled": true}}`)
	want := jsonMap(t, storedTelemetry)
	delete(objAt(t, want, "cloud"), "enabled")
	assert.Equal(t, want, got)

	fakeStore.seedWithOrigin("telemetry", json.RawMessage(storedTelemetry), "managed")
	got, _ = mergeTelemetry(t, ops, `{"telemetry": {"enabled": true}}`)
	assert.Equal(t, jsonMap(t, storedTelemetry), got)
}

// The env var name maps to the nested koanf key the merge looks for.
func TestLoadEnvKoanf_TelemetryCloudEnabledKey(t *testing.T) {
	t.Setenv("SCION_SERVER_TELEMETRY_CLOUD_ENABLED", "true")
	assert.Contains(t, config.LoadEnvKoanf().Keys(), "telemetry.cloud.enabled")
}

// #3717 repro at the handler: a save from the Server Config page keeps the
// telemetry fields the page does not show.
func TestPutServerConfigDB_TelemetrySaveKeepsUnshownFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("telemetry", json.RawMessage(storedTelemetry), "seeded")

	rr := putServerConfigDB(t, srv, ops, pageTelemetryBody)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	row := sectionRowRaw(t, fakeStore, "telemetry")
	cloud := objAt(t, row, "cloud")
	assert.Equal(t, "otel.example.com:4318", cloud["endpoint"])
	assert.Equal(t, "http", cloud["protocol"])
	assert.Equal(t, false, cloud["cloud_logging"])
	assert.Equal(t, map[string]interface{}{"x-api-key": "k1"}, cloud["headers"])
	assert.Equal(t, map[string]interface{}{"enabled": true, "ca_file": "/etc/otel/ca.pem"}, cloud["tls"])
	assert.Equal(t, map[string]interface{}{"max_size": float64(512), "timeout": "5s"}, cloud["batch"])
	assert.Equal(t, objAt(t, jsonMap(t, storedTelemetry), "filter"), row["filter"])
	assert.Equal(t, map[string]interface{}{"service.name": "scion-hub"}, row["resource"])
	assert.Equal(t, "60s", objAt(t, row, "hub")["report_interval"])

	// An explicit clear still works.
	rr = putServerConfigDB(t, srv, ops, `{"telemetry": {"cloud": {"headers": null}, "resource": {}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row = sectionRowRaw(t, fakeStore, "telemetry")
	assert.NotContains(t, objAt(t, row, "cloud"), "headers")
	assert.NotContains(t, row, "resource")
	assert.Contains(t, objAt(t, row, "cloud"), "tls")
}

// On a workstation hub with the cloud telemetry export pinned by env, the
// page leaves cloud out of the save; the stored cloud object is kept.
func TestPutServerConfigDB_WorkstationTelemetryCloudEnvPinKeepsCloud(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_TELEMETRY_CLOUD_ENABLED", "true")
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), config.LoadEnvKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, ""), workstation: true}
	srv.SetOperationalSettings(ops)
	fakeStore.seedWithOrigin("telemetry", json.RawMessage(storedTelemetry), "managed")

	body := `{"telemetry": {"enabled": true, "hub": {"enabled": false, "report_interval": "30s"}, "local": {"enabled": false, "file": "", "console": false}}}`
	rr := putServerConfigDB(t, srv, ops, body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	row := sectionRowRaw(t, fakeStore, "telemetry")
	assert.Equal(t, objAt(t, jsonMap(t, storedTelemetry), "cloud"), row["cloud"])
	assert.Equal(t, false, objAt(t, row, "hub")["enabled"])
}

const storedAgentDefaults = `{
	"default_template": "claude",
	"default_harness_config": "claude-default",
	"default_model": "m0",
	"default_max_turns": 50,
	"default_max_duration": "2h",
	"default_resources": {"requests": {"cpu": "1"}},
	"default_thinking_level": 40,
	"default_max_agent_role": "baseline",
	"default_agent_role": "readonly",
	"default_timezone": "UTC",
	"default_gcp_identity_mode": "block"
}`

// #3719 repro at the handler: with default_template pinned by env, the
// page leaves it out of the save, and the other agent_defaults fields
// survive. On a seeded row only the env-pinned key is dropped, so this
// node's env value is not written into the shared row.
func TestPutServerConfigDB_AgentDefaultsEnvPinnedSaveKeepsOtherFields(t *testing.T) {
	for _, origin := range []string{"managed", "seeded"} {
		t.Run(origin, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SCION_SERVER_DEFAULTTEMPLATE", "env-template")
			envK := config.LoadEnvKoanf()
			require.Contains(t, envK.Keys(), "default_template")
			fakeStore := newFakeHubSettingStore()
			ops := NewOperationalSettings(fakeStore, emptyKoanf(), envK)
			srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
			srv.SetOperationalSettings(ops)
			fakeStore.seedWithOrigin("agent_defaults", json.RawMessage(storedAgentDefaults), origin)

			rr := putServerConfigDB(t, srv, ops, `{"default_model": "m1", "default_max_turns": 60}`)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

			want := jsonMap(t, storedAgentDefaults)
			want["default_model"] = "m1"
			want["default_max_turns"] = float64(60)
			if origin == "seeded" {
				delete(want, "default_template")
			}
			assert.Equal(t, want, sectionRowRaw(t, fakeStore, "agent_defaults"))
		})
	}
}

// Explicit clears: null, "" and 0 clear their keys; a lone null builds the
// section doc. Keys the body omits are kept.
func TestPutServerConfigDB_AgentDefaultsExplicitClears(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("agent_defaults", json.RawMessage(storedAgentDefaults), "managed")

	rr := putServerConfigDB(t, srv, ops, `{"default_model": null}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	want := jsonMap(t, storedAgentDefaults)
	delete(want, "default_model")
	assert.Equal(t, want, sectionRowRaw(t, fakeStore, "agent_defaults"))

	rr = putServerConfigDB(t, srv, ops, `{"default_max_turns": 0, "default_timezone": "", "default_resources": null}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	delete(want, "default_max_turns")
	delete(want, "default_timezone")
	delete(want, "default_resources")
	assert.Equal(t, want, sectionRowRaw(t, fakeStore, "agent_defaults"))
}

// Thinking level: null clears it, a value in range stores it, and 0 (or
// any value outside 1-100) is rejected rather than clearing it.
func TestPutServerConfigDB_AgentDefaultsThinkingLevel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("agent_defaults", json.RawMessage(storedAgentDefaults), "managed")

	for _, bad := range []string{"0", "-1", "101"} {
		rr := putServerConfigDB(t, srv, ops, `{"default_thinking_level": `+bad+`}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "level %s: %s", bad, rr.Body.String())
		assert.Equal(t, float64(40), sectionRowRaw(t, fakeStore, "agent_defaults")["default_thinking_level"])
	}

	rr := putServerConfigDB(t, srv, ops, `{"default_thinking_level": 7}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, float64(7), sectionRowRaw(t, fakeStore, "agent_defaults")["default_thinking_level"])

	rr = putServerConfigDB(t, srv, ops, `{"default_thinking_level": null}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row := sectionRowRaw(t, fakeStore, "agent_defaults")
	assert.NotContains(t, row, "default_thinking_level")
	assert.Equal(t, "m0", row["default_model"])
}

// The read-only role keys are never changed or cleared by a save, even
// when the body carries them, and a key the request type has no field for
// is never touched.
func TestMergeSectionOnCurrent_AgentDefaultsReadOnlyKeysKept(t *testing.T) {
	_, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("agent_defaults", json.RawMessage(storedAgentDefaults), "managed")

	got := mergeAgentDefaults(t, ops, `{"default_max_agent_role": "", "Default_Agent_Role": null, "default_harness_auth": "", "default_model": "m1"}`)
	want := jsonMap(t, storedAgentDefaults)
	want["default_model"] = "m1"
	assert.Equal(t, want, got)

	fp, err := parseFieldPresence([]byte(`{"default_max_agent_role": "", "default_agent_role": "", "default_harness_auth": "", "Default_Model": "m1", "telemetry": {}}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"Default_Model"}, mapKeysOf(agentDefaultsPresence(fp).raw))
}

func mapKeysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A carried default GCP identity is not re-checked on an unrelated save:
// the stored assign mode names an account the hub does not have, and a
// save that sends neither GCP key still succeeds.
func TestPutServerConfigDB_AgentDefaultsUnsentGCPIdentityNotRechecked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("agent_defaults",
		json.RawMessage(`{"default_model":"m0","default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":"sa-missing"}`), "managed")

	rr := putServerConfigDB(t, srv, ops, `{"default_model": "m1"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row := sectionRowRaw(t, fakeStore, "agent_defaults")
	assert.Equal(t, "m1", row["default_model"])
	assert.Equal(t, "sa-missing", row["default_gcp_identity_service_account_id"])
}

func TestSectionKeyKoanfPath(t *testing.T) {
	for _, tc := range []struct{ section, key, want string }{
		{"telemetry", "cloud", "telemetry.cloud"},
		{"telemetry", "resource", "telemetry.resource"},
		{"telemetry", "bogus", ""},
		{"agent_defaults", "default_template", "default_template"},
		{"agent_defaults", "default_harness_auth", ""},
		{"github_app", "app_id", "server.github_app.app_id"},
		{"runtimes", "local", ""},
	} {
		assert.Equal(t, tc.want, sectionKeyKoanfPath(tc.section, tc.key), "%s.%s", tc.section, tc.key)
	}
}

// mergeSectionsRaceStore simulates another replica writing a section row
// between the PUT's read of it and its write.
type mergeSectionsRaceStore struct {
	*fakeHubSettingStore
	section string
	other   json.RawMessage
	once    sync.Once
}

func (c *mergeSectionsRaceStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	row, err := c.fakeHubSettingStore.GetHubSetting(ctx, section)
	if section != c.section || err != nil {
		return row, err
	}
	cp := *row
	c.once.Do(func() {
		_, _ = c.UpsertHubSetting(ctx, section, c.other, "other-replica", -1, "managed")
	})
	return &cp, nil
}

// The merged telemetry and agent_defaults writes are CAS-guarded on the
// revision they read, so a concurrent write yields 409 and the other
// writer's row stands.
func TestPutServerConfigDB_TelemetryAndAgentDefaultsMerge_ConcurrentWrite409(t *testing.T) {
	for _, tc := range []struct {
		section, stored, other, body string
	}{
		{"telemetry", storedTelemetry, `{"enabled":false}`, `{"telemetry": {"cloud": {"endpoint": "e2"}}}`},
		{"agent_defaults", storedAgentDefaults, `{"default_model":"other"}`, `{"default_model": "m1"}`},
	} {
		t.Run(tc.section, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fake := newFakeHubSettingStore()
			fake.seedWithOrigin(tc.section, json.RawMessage(tc.stored), "managed")
			race := &mergeSectionsRaceStore{fakeHubSettingStore: fake, section: tc.section, other: json.RawMessage(tc.other)}
			ops := NewOperationalSettings(race, emptyKoanf(), emptyKoanf())
			srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
			srv.SetOperationalSettings(ops)

			rr := putServerConfigDB(t, srv, ops, tc.body)
			require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
			assert.Equal(t, jsonMap(t, tc.other), sectionRowRaw(t, fake, tc.section))
		})
	}
}

// agentDefaultsReadOnlyKeys are the agent_defaults keys a server-config PUT
// never writes (see agentDefaultsRequestKeys).
var agentDefaultsReadOnlyKeys = map[string]bool{
	"default_max_agent_role": true,
	"default_agent_role":     true,
	"default_harness_auth":   true,
}

// Every agent_defaults key is either written by the PUT
// (agentDefaultsRequestKeys) or listed as read-only, and every written key
// has a request field that buildSingleSectionDoc copies into the section
// doc. A field added to the section or the request without updating
// agentDefaultsRequestKeys fails here instead of being ignored by the
// merge on every save.
func TestAgentDefaultsRequestKeys_Parity(t *testing.T) {
	sectionKeys := map[string]bool{}
	for name := range structJSONNames(reflect.TypeOf(opsettings.AgentDefaultsSettings{})) {
		sectionKeys[name] = true
		assert.True(t, agentDefaultsRequestKeys[name] || agentDefaultsReadOnlyKeys[name],
			"agent_defaults key %q is neither in agentDefaultsRequestKeys nor read-only", name)
		assert.False(t, agentDefaultsRequestKeys[name] && agentDefaultsReadOnlyKeys[name],
			"agent_defaults key %q is both written and read-only", name)
	}
	reqType := reflect.TypeOf(ServerConfigUpdateRequest{})
	for key := range agentDefaultsRequestKeys {
		require.True(t, sectionKeys[key], "agentDefaultsRequestKeys has %q, which the section does not model", key)
		f, ok := structFieldByJSONName(reqType, key)
		require.True(t, ok, "no ServerConfigUpdateRequest field for %q", key)
		require.Equal(t, key, jsonFieldName(f))

		var sample string
		switch ft := f.Type.Elem(); {
		case ft.Kind() == reflect.String:
			sample = `"x"`
		case ft.Kind() == reflect.Int:
			sample = `5`
		default:
			sample = `{"requests": {"cpu": "1"}}`
		}
		body := `{"` + key + `": ` + sample + `}`
		var req ServerConfigUpdateRequest
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		fp, err := parseFieldPresence([]byte(body))
		require.NoError(t, err)
		doc, err := buildSingleSectionDoc(&req, "agent_defaults", fp)
		require.NoError(t, err)
		assert.Contains(t, jsonMap(t, string(doc)), key, "buildSingleSectionDoc does not write %q", key)
	}
}

// A stored default_thinking_level of 0 (a legacy or hand-edited value) is
// invalid under the section schema, so a save that leaves it out drops it
// instead of failing.
func TestPutServerConfigDB_StoredZeroThinkingLevelDoesNotBlockSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_model":"m0","default_thinking_level":0}`), "managed")

	assert.NotEmpty(t, opsettings.Validate("agent_defaults", json.RawMessage(`{"default_thinking_level":0}`)))
	rr := putServerConfigDB(t, srv, ops, `{"default_model": "m1"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, map[string]interface{}{"default_model": "m1"}, sectionRowRaw(t, fakeStore, "agent_defaults"))
}

// The telemetry section type exposes the fields of the config struct it
// embeds, so deep merge and case-insensitive matching find them.
func TestSectionModelType_TelemetryExposesEmbeddedFields(t *testing.T) {
	model := sectionModelType("telemetry")
	require.NotNil(t, model)
	for _, key := range []string{"cloud", "Cloud"} {
		f, ok := structFieldByJSONName(model, key)
		require.True(t, ok, key)
		assert.Equal(t, reflect.TypeOf(&config.V1TelemetryCloudConfig{}), f.Type)
	}
	key, ok := modelledSectionKey("telemetry", "FILTER")
	assert.True(t, ok)
	assert.Equal(t, "filter", key)
}

// A stored default_timezone that validateDefaultTimezone rejects ("Local"
// passes the schema) does not block a save that leaves it out; sending it
// is still rejected (ptone/scion#2720).
func TestPutServerConfigDB_AgentDefaultsUnsentTimezoneNotRechecked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("agent_defaults", json.RawMessage(`{"default_model":"m0","default_timezone":"Local"}`), "managed")

	rr := putServerConfigDB(t, srv, ops, `{"default_model": "m1"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row := sectionRowRaw(t, fakeStore, "agent_defaults")
	assert.Equal(t, "m1", row["default_model"])
	assert.Equal(t, "Local", row["default_timezone"])

	rr = putServerConfigDB(t, srv, ops, `{"default_timezone": "Local"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
}

// An env pin on an entry of a free-form map drops only that entry from a
// seeded row; the map's other entries are kept.
func TestMergeSectionOnCurrent_TelemetryEnvPinnedMapEntry(t *testing.T) {
	_, fakeStore, ops := newTestDBServer(t)
	ops.envOverrides = map[string]bool{"telemetry.cloud.headers.authorization": true}
	fakeStore.seedWithOrigin("telemetry",
		json.RawMessage(`{"cloud":{"endpoint":"e1","headers":{"authorization":"a","x-tenant":"t"}}}`), "seeded")

	got, _ := mergeTelemetry(t, ops, `{"telemetry": {"enabled": true}}`)
	assert.Equal(t, jsonMap(t, `{"enabled":true,"cloud":{"endpoint":"e1","headers":{"x-tenant":"t"}}}`), got)
}

// Two spellings of one key ("cloud" and "Cloud") both count as sent: the
// keys sent under either are recorded, so neither is treated as carried.
func TestPatchSection_CaseVariantKeysUnionSentKeys(t *testing.T) {
	body := `{"cloud": {"endpoint": "e2"}, "Cloud": {"protocol": "grpc"}}`
	var tel config.V1TelemetryConfig
	require.NoError(t, json.Unmarshal([]byte(body), &tel))
	reqDoc, err := json.Marshal(tel)
	require.NoError(t, err)
	var next map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(reqDoc, &next))
	fp, err := parseFieldPresence([]byte(body))
	require.NoError(t, err)

	base := map[string]json.RawMessage{"cloud": json.RawMessage(`{"endpoint":"e1","protocol":"http","provider":"p"}`)}
	tree := patchSection("telemetry", base, next, fp)
	assert.Equal(t, sentTree{"cloud": sentTree{"endpoint": nil, "protocol": nil}}, tree)
	assert.JSONEq(t, `{"endpoint":"e2","protocol":"grpc","provider":"p"}`, string(base["cloud"]))

	assert.Nil(t, unionSentTrees(sentTree{"a": nil}, nil), "a value applied whole stays whole")
}
