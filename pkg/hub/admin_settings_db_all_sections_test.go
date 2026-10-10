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

// Tests for the Server Config sections merged on their current row
// (ptone/scion#3899, ptone/scion#3720), the Layer-1 round trip over the
// opsettings registry (ptone/scion#3721), and telemetry.cloud.headers
// masking in the admin server-config GET.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
)

// layer1RoundTripSamples holds a valid value (JSON) for every Layer-1
// koanf key of the opsettings registry that the round trip covers. A key
// added to the registry must be added here (or to layer1RoundTripExempt
// with a reason), or TestPutServerConfigDB_Layer1RoundTrip fails.
var layer1RoundTripSamples = map[string]string{
	// access
	"server.hub.admin_emails":        `["admin@example.com"]`,
	"server.auth.user_access_mode":   `"invite"`,
	"server.auth.default_user_role":  `"viewer"`,
	"server.auth.authorized_domains": `["example.com"]`,
	// lifecycle
	"server.hub.auto_suspend_stalled":          `true`,
	"server.hub.stalled_threshold":             `"10m"`,
	"server.hub.soft_delete_retention":         `"720h"`,
	"server.hub.soft_delete_retain_files":      `true`,
	"server.hub.start_claim_lease_ttl":         `"60s"`,
	"server.hub.start_max_duration":            `"15m"`,
	"server.hub.start_unconfirmed_hold":        `"14m"`,
	"server.hub.start_create_unconfirmed_hold": `"4m"`,
	// telemetry (deep-merged)
	"telemetry.enabled":                        `true`,
	"telemetry.cloud":                          `{"endpoint":"otel.example.com:4317"}`,
	"telemetry.cloud.enabled":                  `true`,
	"telemetry.cloud.endpoint":                 `"otel.example.com:4317"`,
	"telemetry.cloud.protocol":                 `"grpc"`,
	"telemetry.cloud.headers":                  `{"x-api-key":"k1"}`,
	"telemetry.cloud.provider":                 `"gcp"`,
	"telemetry.cloud.tls":                      `{"ca_file":"/etc/ca.pem"}`,
	"telemetry.cloud.tls.enabled":              `true`,
	"telemetry.cloud.tls.insecure_skip_verify": `false`,
	"telemetry.cloud.tls.ca_file":              `"/etc/ca.pem"`,
	"telemetry.cloud.batch":                    `{"max_size":256}`,
	"telemetry.cloud.batch.max_size":           `256`,
	"telemetry.cloud.batch.timeout":            `"10s"`,
	"telemetry.hub":                            `{"report_interval":"60s"}`,
	"telemetry.hub.enabled":                    `true`,
	"telemetry.hub.report_interval":            `"60s"`,
	"telemetry.local":                          `{"file":"/tmp/t.jsonl"}`,
	"telemetry.local.enabled":                  `true`,
	"telemetry.local.file":                     `"/tmp/t.jsonl"`,
	"telemetry.local.console":                  `true`,
	"telemetry.filter":                         `{"respect_debug_mode":false}`,
	"telemetry.filter.enabled":                 `true`,
	"telemetry.filter.respect_debug_mode":      `false`,
	"telemetry.filter.events":                  `{"include":["agent.start"]}`,
	"telemetry.filter.events.include":          `["agent.start"]`,
	"telemetry.filter.events.exclude":          `["agent.debug"]`,
	"telemetry.filter.attributes":              `{"redact":["user.email"]}`,
	"telemetry.filter.attributes.redact":       `["user.email"]`,
	"telemetry.filter.attributes.hash":         `["user.id"]`,
	"telemetry.filter.sampling":                `{"default":0.5}`,
	"telemetry.filter.sampling.default":        `0.5`,
	"telemetry.filter.sampling.rates":          `{"agent.start":0.25}`,
	"telemetry.resource":                       `{"service.name":"scion-hub"}`,
	// auto_expose_ports, quotas, agent_secrets
	"auto_expose_ports.enabled":     `true`,
	"quotas.enforce_broker_quotas":  `false`,
	"agent_secrets.user_scope_only": `true`,
	// agent_defaults
	"default_template":                        `"tmpl-a"`,
	"default_harness_config":                  `"hc-a"`,
	"default_max_turns":                       `50`,
	"default_max_model_calls":                 `100`,
	"default_max_duration":                    `"2h"`,
	"default_resources":                       `{"requests":{"cpu":"1"}}`,
	"default_model":                           `"model-a"`,
	"default_thinking_level":                  `50`,
	"default_runtime_broker":                  `"broker-a"`,
	"default_timezone":                        `"America/Los_Angeles"`,
	"default_gcp_identity_mode":               `"block"`,
	"default_gcp_identity_service_account_id": `"sa-1"`,
	// endpoints
	"server.hub.public_url": `"https://hub.example.com"`,
	"server.hub.hub_name":   `"hub-a"`,
	"image_registry":        `"registry.example.com/scion"`,
	// github_app
	"server.github_app.app_id":           `12345`,
	"server.github_app.api_base_url":     `"https://api.github.com"`,
	"server.github_app.webhooks_enabled": `true`,
	"server.github_app.installation_url": `"https://github.com/apps/scion/installations/new"`,
	"server.github_app.private_key_path": `"/etc/scion/gh.pem"`,
	// notifications
	"server.notification_channels": `[{"type":"slack","params":{"channel":"#ops"}}]`,
	// project_defaults (PUT /api/v1/admin/project-defaults)
	"project_defaults.default_scratchpad": `false`,
	// federation
	"server.federation.enabled":           `false`,
	"server.federation.trusted_issuers":   `[{"issuer_url":"https://issuer.example.com"}]`,
	"server.federation.algorithms":        `["RS256"]`,
	"server.federation.refresh_interval":  `"2h"`,
	"server.federation.debounce_interval": `"10s"`,
	// map sections: the whole map is the field
	"runtimes":        `{"rt-a":{"type":"docker"}}`,
	"profiles":        `{"p-a":{"runtime":"rt-a"}}`,
	"harness_configs": `{"hc-a":{"harness":"claude"}}`,
}

// layer1RoundTripExempt lists the Layer-1 koanf keys the round trip does
// not cover, with the reason.
var layer1RoundTripExempt = map[string]string{
	"default_max_agent_role": "read-only through server-config: accepted only as an unchanged echo of GET (dbUnwrittenLayer1Paths)",
	"default_agent_role":     "read-only through server-config: accepted only as an unchanged echo of GET (dbUnwrittenLayer1Paths)",
	"server.github_app":      "the whole github_app object; each of its fields is covered on its own, and a null object is not a clear (as for telemetry)",
	"server.hub.gcp_iam_check_mode": "gcp_iam is built by buildGCPIAMDoc: a sent key must carry a recognised value (validateGCPIAMRequest), so it has no explicit clear, " +
		"and changes are audited; covered by gcp_iam_settings_test.go",
	"server.hub.gcp_iam_deny_unknown_policy": "gcp_iam is built by buildGCPIAMDoc: a sent key must carry a recognised value (validateGCPIAMRequest), so it has no explicit clear, " +
		"and changes are audited; covered by gcp_iam_settings_test.go",
}

// layer1RoundTripNoCompanion lists sample keys that are never sent as the
// unrelated key of a round trip, because sending them needs state the
// test server does not have.
var layer1RoundTripNoCompanion = map[string]bool{
	"default_gcp_identity_service_account_id": true, // must name a stored, verified service account
	"default_gcp_identity_mode":               true, // checked together with a stored service account
	"server.hub.hub_name":                     true, // a sent value equal to the effective name is an echo
}

// mapSections are the opsettings sections whose document is one map.
var mapSections = map[string]bool{"runtimes": true, "profiles": true, "harness_configs": true}

// layer1Field locates one Layer-1 koanf key for the round trip.
type layer1Field struct {
	koanf    string
	section  string
	docPath  []string // path inside the section document; nil for a map section
	bodyPath []string // path in the PUT body
}

// layer1FieldFor resolves a registry koanf key of section.
func layer1FieldFor(t *testing.T, section, koanf string) layer1Field {
	t.Helper()
	f := layer1Field{koanf: koanf, section: section}
	switch {
	case mapSections[section]:
		f.bodyPath = []string{section}
		return f
	case section == "project_defaults":
		f.docPath = []string{strings.TrimPrefix(koanf, "project_defaults.")}
		f.bodyPath = f.docPath
		return f
	case section == "telemetry":
		f.docPath = strings.Split(strings.TrimPrefix(koanf, "telemetry."), ".")
	default:
		for _, key := range sectionJSONKeys(section) {
			if sectionKeyKoanfPath(section, key) == koanf {
				f.docPath = []string{key}
			}
		}
		if f.docPath == nil {
			t.Fatalf("no section key of %s maps to %s", section, koanf)
		}
	}
	f.bodyPath = bodyPathForKoanf(koanf)
	return f
}

// nestJSON returns the JSON document holding value at path.
func nestJSON(t *testing.T, path []string, value string) string {
	t.Helper()
	b := json.RawMessage(value)
	for i := len(path) - 1; i >= 0; i-- {
		var err error
		b, err = json.Marshal(map[string]json.RawMessage{path[i]: b})
		require.NoError(t, err)
	}
	return string(b)
}

// decodeJSONValue decodes s as a generic JSON value.
func decodeJSONValue(t *testing.T, s []byte) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(s, &v), "%s", s)
	return v
}

// valueAtPath returns the value at path in a decoded JSON document.
func valueAtPath(doc any, path []string) (any, bool) {
	cur := doc
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// storedRow returns the stored row of section, or nil when there is none.
func storedRow(fake *fakeHubSettingStore, section string) *store.HubSetting {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	row, ok := fake.settings[section]
	if !ok {
		return nil
	}
	cp := *row
	return &cp
}

// putLayer1 sends body to the PUT endpoint that writes section.
func putLayer1(t *testing.T, srv *Server, ops *OperationalSettings, section, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	if section == "project_defaults" {
		srv.handleAdminProjectDefaults(rr, adminRequest(http.MethodPut, "/api/v1/admin/project-defaults", body))
		return rr
	}
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	return rr
}

// related reports whether one of two koanf keys is the other or contains it.
func related(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+".") || strings.HasPrefix(b, a+".")
}

// omitBody returns a PUT body that writes f's section without sending f:
// another sample key of the section when there is one, else the section
// object without f (for a single-key section), else a key of another
// section.
func omitBody(t *testing.T, f layer1Field, sectionKeys []string) string {
	t.Helper()
	for _, other := range sectionKeys {
		if related(other, f.koanf) || layer1RoundTripNoCompanion[other] {
			continue
		}
		sample, ok := layer1RoundTripSamples[other]
		if !ok {
			continue
		}
		o := layer1FieldFor(t, f.section, other)
		return nestJSON(t, o.bodyPath, sample)
	}
	if f.section == "project_defaults" {
		return `{}`
	}
	if len(f.bodyPath) > 1 {
		return nestJSON(t, f.bodyPath[:len(f.bodyPath)-1], `{}`)
	}
	return `{"default_model":"model-unrelated"}`
}

// Every Layer-1 field of the opsettings registry round-trips through the
// DB-backed save: with the field stored, a PUT that omits it keeps it, and
// a PUT that sends an explicit null clears it (ptone/scion#3721). A
// registry key without a sample or an exemption fails the test.
func TestPutServerConfigDB_Layer1RoundTrip(t *testing.T) {
	covered := map[string]bool{}
	for _, sec := range opsettings.Registry {
		var keys []string
		keys = append(keys, sec.KoanfPaths...)
		sort.Strings(keys)
		for _, koanf := range keys {
			covered[koanf] = true
			if reason, ok := layer1RoundTripExempt[koanf]; ok {
				require.NotEmpty(t, reason, "exemption of %s needs a reason", koanf)
				continue
			}
			sample, ok := layer1RoundTripSamples[koanf]
			if !ok {
				t.Errorf("Layer-1 key %s (section %s) has no round-trip sample: add one to layer1RoundTripSamples, or an exemption with a reason to layer1RoundTripExempt", koanf, sec.Name)
				continue
			}
			t.Run(koanf, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				f := layer1FieldFor(t, sec.Name, koanf)
				srv, fake, ops := newTestDBServer(t)

				seed := sample
				if f.docPath != nil {
					seed = nestJSON(t, f.docPath, sample)
				}
				fake.seedWithOrigin(sec.Name, json.RawMessage(seed), "managed")
				_, err := ops.Refresh(context.Background())
				require.NoError(t, err)
				want := decodeJSONValue(t, []byte(sample))

				// Omitted: kept.
				body := omitBody(t, f, keys)
				rr := putLayer1(t, srv, ops, sec.Name, body)
				require.Equal(t, http.StatusOK, rr.Code, "omit body %s: %s", body, rr.Body.String())
				row := storedRow(fake, sec.Name)
				require.NotNil(t, row)
				got, ok := valueAtPath(decodeJSONValue(t, row.Value), f.docPath)
				require.True(t, ok, "omitting %s must keep it; row %s after %s", koanf, row.Value, body)
				assert.Equal(t, want, got, "omitting %s must keep its value", koanf)

				// Explicit null: cleared.
				body = nestJSON(t, f.bodyPath, `null`)
				rr = putLayer1(t, srv, ops, sec.Name, body)
				require.Equal(t, http.StatusOK, rr.Code, "clear body %s: %s", body, rr.Body.String())
				row = storedRow(fake, sec.Name)
				require.NotNil(t, row, "a clear keeps the row")
				doc := decodeJSONValue(t, row.Value)
				if f.docPath == nil {
					assert.Equal(t, map[string]any{}, doc, "null must clear the %s map", sec.Name)
					return
				}
				_, ok = valueAtPath(doc, f.docPath)
				assert.False(t, ok, "an explicit null must clear %s; row %s", koanf, row.Value)
			})
		}
	}
	for k := range layer1RoundTripSamples {
		assert.True(t, covered[k], "layer1RoundTripSamples has %s, which is not a registry key", k)
	}
	for k := range layer1RoundTripExempt {
		assert.True(t, covered[k], "layer1RoundTripExempt has %s, which is not a registry key", k)
	}
}

// bodyMergedSections lists every section with keys that the server-config
// PUT writes, except those merged with presence functions of their own,
// gcp_iam and the map sections.
func TestBodyMergedSections_CoverWritableSections(t *testing.T) {
	own := map[string]bool{"github_app": true, "telemetry": true, "agent_defaults": true, gcpIAMSection: true, "project_defaults": true}
	listed := map[string]bool{}
	for _, s := range bodyMergedSections {
		listed[s] = true
	}
	for _, sec := range opsettings.Registry {
		if len(sec.KoanfPaths) == 0 || own[sec.Name] || mapSections[sec.Name] {
			continue
		}
		assert.True(t, listed[sec.Name], "section %s is written by server-config but not merged on its row", sec.Name)
	}
}

// sectionBodyPresence finds each section key at the body path of its
// koanf key, matching members the way the typed decode does.
func TestSectionBodyPresence(t *testing.T) {
	body := []byte(`{
		"server": {"Hub": {"admin_emails": [], "public_url": null, "stalled_threshold": "5m"},
		           "auth": {"user_access_mode": "open"},
		           "notification_channels": null},
		"image_registry": "",
		"federation": {"enabled": false, "Algorithms": ["RS256"]},
		"quotas": {}
	}`)
	keys := func(section string) []string {
		var out []string
		for k := range sectionBodyPresence(section, body).sentKeys() {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	assert.Equal(t, []string{"admin_emails", "user_access_mode"}, keys("access"))
	assert.Equal(t, []string{"image_registry", "public_url"}, keys("endpoints"))
	assert.Equal(t, []string{"stalled_threshold"}, keys("lifecycle"))
	assert.Equal(t, []string{"notification_channels"}, keys("notifications"))
	assert.Equal(t, []string{"algorithms", "enabled"}, keys("federation"))
	assert.Equal(t, "server.notification_channels", sectionKeyKoanfPath("notifications", "notification_channels"))
	assert.Empty(t, keys("quotas"))
	assert.Empty(t, keys("agent_secrets"))
}

// raceSectionStore simulates another replica writing a section row
// between the PUT's read of it and its write.
type raceSectionStore struct {
	*fakeHubSettingStore
	section string
	other   json.RawMessage
	once    sync.Once
}

func (c *raceSectionStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
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

// Each newly merged section writes with a CAS on the revision it read, so
// a concurrent write yields 409 and the other writer's row stands.
func TestPutServerConfigDB_MergedSections_ConcurrentWrite409(t *testing.T) {
	for _, tc := range []struct {
		section, stored, other, body string
	}{
		{"federation", `{"enabled":false,"algorithms":["RS256"]}`, `{"algorithms":["ES256"]}`, `{"federation": {"refresh_interval": "2h"}}`},
		{"auto_expose_ports", `{"enabled":true}`, `{"enabled":false}`, `{"auto_expose_ports": {"enabled": null}}`},
		{"quotas", `{"enforce_broker_quotas":true}`, `{"enforce_broker_quotas":false}`, `{"quotas": {"enforce_broker_quotas": null}}`},
		{"agent_secrets", `{"user_scope_only":true}`, `{"user_scope_only":false}`, `{"agent_secrets": {"user_scope_only": null}}`},
		{"notifications", `{"notification_channels":[{"type":"slack","params":{"channel":"#a"}}]}`, `{"notification_channels":[{"type":"slack","params":{"channel":"#b"}}]}`, `{"server": {"notification_channels": []}}`},
		{"project_defaults", `{"default_scratchpad":true}`, `{"default_scratchpad":false}`, `{"default_scratchpad": null}`},
	} {
		t.Run(tc.section, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fake := newFakeHubSettingStore()
			fake.seedWithOrigin(tc.section, json.RawMessage(tc.stored), "managed")
			race := &raceSectionStore{fakeHubSettingStore: fake, section: tc.section, other: json.RawMessage(tc.other)}
			ops := NewOperationalSettings(race, emptyKoanf(), emptyKoanf())
			srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
			srv.SetOperationalSettings(ops)

			rr := putLayer1(t, srv, ops, tc.section, tc.body)
			require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
			row := storedRow(fake, tc.section)
			require.NotNil(t, row)
			assert.Equal(t, decodeJSONValue(t, []byte(tc.other)), decodeJSONValue(t, row.Value), "the concurrent writer's row must stand")
		})
	}
}

// Each newly merged section stores explicit values, keeps a key the body
// omits and clears a key sent as null or as an empty value.
func TestPutServerConfigDB_MergedSections_SendKeepClear(t *testing.T) {
	for _, tc := range []struct {
		name, section, stored, body, want string
	}{
		{"federation set keeps others", "federation", `{"enabled":false,"algorithms":["RS256"],"refresh_interval":"1h"}`,
			`{"federation": {"refresh_interval": "2h"}}`, `{"enabled":false,"algorithms":["RS256"],"refresh_interval":"2h"}`},
		{"federation empty array clears", "federation", `{"enabled":false,"algorithms":["RS256"],"refresh_interval":"1h"}`,
			`{"federation": {"algorithms": []}}`, `{"enabled":false,"refresh_interval":"1h"}`},
		{"federation empty string clears", "federation", `{"enabled":false,"refresh_interval":"1h","debounce_interval":"5s"}`,
			`{"federation": {"refresh_interval": ""}}`, `{"enabled":false,"debounce_interval":"5s"}`},
		{"auto_expose_ports false stored", "auto_expose_ports", `{"enabled":true}`,
			`{"auto_expose_ports": {"enabled": false}}`, `{"enabled":false}`},
		{"auto_expose_ports omitted kept", "auto_expose_ports", `{"enabled":true}`,
			`{"auto_expose_ports": {}}`, `{"enabled":true}`},
		{"notifications empty array clears", "notifications", `{"notification_channels":[{"type":"slack","params":{"channel":"#a"}}]}`,
			`{"server": {"notification_channels": []}}`, `{}`},
		{"notifications omitted kept", "notifications", `{"notification_channels":[{"type":"slack","params":{"channel":"#a"}}]}`,
			`{"server": {"hub": {"stalled_threshold": "5m"}}}`, `{"notification_channels":[{"type":"slack","params":{"channel":"#a"}}]}`},
		{"project_defaults omitted kept", "project_defaults", `{"default_scratchpad":false}`,
			`{}`, `{"default_scratchpad":false}`},
		{"project_defaults null clears", "project_defaults", `{"default_scratchpad":false}`,
			`{"default_scratchpad": null}`, `{}`},
		{"lifecycle start-claim empty string clears", "lifecycle", `{"start_claim_lease_ttl":"60s","stalled_threshold":"10m"}`,
			`{"server": {"hub": {"start_claim_lease_ttl": ""}}}`, `{"stalled_threshold":"10m"}`},
		{"endpoints image_registry null clears", "endpoints", `{"public_url":"https://hub.example.com","image_registry":"r.example.com"}`,
			`{"image_registry": null}`, `{"public_url":"https://hub.example.com"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			fake.seedWithOrigin(tc.section, json.RawMessage(tc.stored), "managed")
			_, err := ops.Refresh(context.Background())
			require.NoError(t, err)

			rr := putLayer1(t, srv, ops, tc.section, tc.body)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			row := storedRow(fake, tc.section)
			require.NotNil(t, row)
			assert.Equal(t, decodeJSONValue(t, []byte(tc.want)), decodeJSONValue(t, row.Value))
		})
	}
}

// pageSaveRows are stored rows for the sections a Server Config page save
// writes, in a key order and spacing a re-encode would change.
var pageSaveRows = map[string]string{
	"access":            `{"user_access_mode": "invite", "admin_emails": ["admin@example.com"], "default_user_role": "viewer"}`,
	"lifecycle":         `{"stalled_threshold": "10m", "auto_suspend_stalled": true, "soft_delete_retain_files": false, "soft_delete_retention": "720h"}`,
	"endpoints":         `{"public_url": "https://hub.example.com", "image_registry": "registry.example.com"}`,
	"agent_defaults":    `{"default_template": "tmpl-a", "default_model": "model-a", "default_max_turns": 50}`,
	"telemetry":         `{"enabled": true, "cloud": {"protocol": "grpc", "enabled": true, "endpoint": "otel.example.com:4317", "headers": {"x-api-key": "k1"}}}`,
	"auto_expose_ports": `{"enabled": true}`,
	"quotas":            `{"enforce_broker_quotas": false}`,
	"agent_secrets":     `{"user_scope_only": true}`,
	"runtimes":          `{"rt-a": {"type": "docker"}}`,
	"profiles":          `{"p-a": {"runtime": "rt-a"}}`,
	"harness_configs":   `{"hc-a": {"harness": "claude"}}`,
}

// A Server Config page save (the payload buildLayer1Payload builds) that
// changes one field leaves the rows of every other section it sends
// byte-identical, with their revisions (ptone/scion#3899).
func TestPutServerConfigDB_PageSaveLeavesUntouchedRowsUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	for sec, doc := range pageSaveRows {
		fake.seedWithOrigin(sec, json.RawMessage(doc), "managed")
	}
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	// The page sends every field it shows; only stalled_threshold changed.
	page := `{
		"default_template": "tmpl-a",
		"image_registry": "registry.example.com",
		"default_max_turns": 50,
		"default_model": "model-a",
		"server": {
			"hub": {"public_url": "https://hub.example.com", "admin_emails": ["admin@example.com"],
			        "soft_delete_retention": "720h", "soft_delete_retain_files": false,
			        "auto_suspend_stalled": true, "stalled_threshold": "20m"},
			"auth": {"user_access_mode": "invite", "default_user_role": "viewer"}
		},
		"telemetry": {"enabled": true, "cloud": {"enabled": true, "endpoint": "otel.example.com:4317", "protocol": "grpc"}},
		"auto_expose_ports": {"enabled": true},
		"quotas": {"enforce_broker_quotas": false},
		"agent_secrets": {"user_scope_only": true},
		"runtimes": {"rt-a": {"type": "docker"}},
		"profiles": {"p-a": {"runtime": "rt-a"}},
		"harness_configs": {"hc-a": {"harness": "claude"}}
	}`
	rr := putServerConfigDB(t, srv, ops, page)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// The response names the written section in reload.applied and the
	// skipped ones in unchanged.
	var resp struct {
		Reload struct {
			Applied []string `json:"applied"`
		} `json:"reload"`
		Unchanged []string `json:"unchanged"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, []string{"lifecycle"}, resp.Reload.Applied)
	var wantUnchanged []string
	for sec := range pageSaveRows {
		if sec != "lifecycle" {
			wantUnchanged = append(wantUnchanged, sec)
		}
	}
	sort.Strings(wantUnchanged)
	assert.Equal(t, wantUnchanged, resp.Unchanged)

	for sec, doc := range pageSaveRows {
		row := storedRow(fake, sec)
		require.NotNil(t, row, sec)
		if sec == "lifecycle" {
			assert.Equal(t, int64(2), row.Revision, "the changed section is written")
			got, _ := valueAtPath(decodeJSONValue(t, row.Value), []string{"stalled_threshold"})
			assert.Equal(t, "20m", got)
			continue
		}
		assert.Equal(t, doc, string(row.Value), "%s row must be byte-identical", sec)
		assert.Equal(t, int64(1), row.Revision, "%s row must not be rewritten", sec)
	}
}

// A seeded row is still written by a save that sends its values unchanged
// (the save adopts it as managed); only managed rows are left as they are.
func TestPutServerConfigDB_UnchangedSeededRowIsAdopted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	fake.seedWithOrigin("quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "seeded")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	rr := putServerConfigDB(t, srv, ops, `{"quotas": {"enforce_broker_quotas": false}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row := storedRow(fake, "quotas")
	require.NotNil(t, row)
	assert.Equal(t, "managed", row.Origin)
	assert.Equal(t, int64(2), row.Revision)
}

// An access save on a hub with no access row starts from the effective
// snapshot values, and endpoints from the effective public_url and
// image_registry, as their builders did before the move to the shared
// helper.
func TestPutServerConfigDB_AccessNoRowBaseFromSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := newFakeHubSettingStore()
	fileK := newFileKoanf(t, map[string]interface{}{
		"server.auth.default_user_role": "viewer",
		"image_registry":                "file.example.com",
	})
	ops := NewOperationalSettings(fake, fileK, emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"user_access_mode":"invite"},"hub":{"public_url":"https://hub.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, decodeJSONValue(t, []byte(`{"default_user_role":"viewer","user_access_mode":"invite"}`)),
		decodeJSONValue(t, storedRow(fake, "access").Value))
	assert.Equal(t, decodeJSONValue(t, []byte(`{"image_registry":"file.example.com","public_url":"https://hub.example.com"}`)),
		decodeJSONValue(t, storedRow(fake, "endpoints").Value))
}

// --- telemetry.cloud.headers masking ---

const storedTelemetryWithHeaders = `{"enabled":true,"cloud":{"enabled":true,"endpoint":"otel.example.com:4317","protocol":"grpc","headers":{"x-api-key":"real-key","x-tenant":"t1"}}}`

// getTelemetryHeaders returns telemetry.cloud.headers of a GET body.
func getTelemetryHeaders(t *testing.T, body []byte) map[string]any {
	t.Helper()
	v, ok := valueAtPath(decodeJSONValue(t, body), []string{"telemetry", "cloud", "headers"})
	require.True(t, ok, "GET body has no telemetry.cloud.headers: %s", body)
	m, ok := v.(map[string]any)
	require.True(t, ok)
	return m
}

func TestGetServerConfigDB_MasksTelemetryCloudHeaders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	fake.seedWithOrigin("telemetry", json.RawMessage(storedTelemetryWithHeaders), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, map[string]any{"x-api-key": maskedValue, "x-tenant": maskedValue}, getTelemetryHeaders(t, rr.Body.Bytes()))
	assert.NotContains(t, rr.Body.String(), "real-key")

	// The snapshot itself is not masked.
	assert.Equal(t, "real-key", ops.Snapshot().TelemetryConfig.Cloud.Headers["x-api-key"])
}

func TestGetServerConfig_FileMode_MasksTelemetryCloudHeaders(t *testing.T) {
	settingsPath := setTempScionHome(t)
	require.NoError(t, os.WriteFile(settingsPath, []byte(`schema_version: "1"
telemetry:
  cloud:
    endpoint: otel.example.com:4317
    headers:
      x-api-key: real-key
      x-empty: ""
`), 0600))
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, map[string]any{"x-api-key": maskedValue, "x-empty": ""}, getTelemetryHeaders(t, rr.Body.Bytes()))
}

// A GET body echoed back on save keeps the stored telemetry headers; a
// real new value still saves.
func TestPutServerConfigDB_TelemetryMaskedHeadersEchoKeeps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	fake.seedWithOrigin("telemetry", json.RawMessage(storedTelemetryWithHeaders), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	tel, _ := valueAtPath(decodeJSONValue(t, rr.Body.Bytes()), []string{"telemetry"})
	echo, err := json.Marshal(map[string]any{"telemetry": tel})
	require.NoError(t, err)

	rr = putServerConfigDB(t, srv, ops, string(echo))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	headers, _ := valueAtPath(decodeJSONValue(t, storedRow(fake, "telemetry").Value), []string{"cloud", "headers"})
	assert.Equal(t, map[string]any{"x-api-key": "real-key", "x-tenant": "t1"}, headers)

	// One header echoed masked, one replaced with a real value.
	rr = putServerConfigDB(t, srv, ops, `{"telemetry":{"cloud":{"endpoint":"otel.example.com:4317","headers":{"x-api-key":"********","x-tenant":"t2"}}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	headers, _ = valueAtPath(decodeJSONValue(t, storedRow(fake, "telemetry").Value), []string{"cloud", "headers"})
	assert.Equal(t, map[string]any{"x-api-key": "real-key", "x-tenant": "t2"}, headers)
	assert.NotContains(t, string(storedRow(fake, "telemetry").Value), maskedValue)
}

// A masked header is restored only into an unchanged telemetry.cloud
// block, and only when a value is stored.
func TestPutServerConfigDB_TelemetryMaskedHeadersRejected(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"changed endpoint", `{"telemetry":{"cloud":{"endpoint":"other.example.com:4317","headers":{"x-api-key":"********"}}}}`},
		{"no stored value", `{"telemetry":{"cloud":{"headers":{"x-new":"********"}}}}`},
		{"cleared endpoint", `{"telemetry":{"cloud":{"endpoint":"","headers":{"x-api-key":"********"}}}}`},
		{"null protocol", `{"telemetry":{"cloud":{"protocol":null,"headers":{"x-api-key":"********"}}}}`},
		{"case-variant endpoint member", `{"telemetry":{"cloud":{"Endpoint":"other.example.com:4317","headers":{"x-api-key":"********"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			fake.seedWithOrigin("telemetry", json.RawMessage(storedTelemetryWithHeaders), "managed")
			_, err := ops.Refresh(context.Background())
			require.NoError(t, err)

			rr := putServerConfigDB(t, srv, ops, tc.body)
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Equal(t, int64(1), storedRow(fake, "telemetry").Revision, "nothing is written")
		})
	}
}

// A masked header sent with the stored endpoint, protocol and enabled
// sent explicitly, and with a null for a member GET did not show, keeps the
// stored header.
func TestPutServerConfigDB_TelemetryMaskedHeaderSameMembersKeeps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	fake.seedWithOrigin("telemetry", json.RawMessage(storedTelemetryWithHeaders), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	rr := putServerConfigDB(t, srv, ops, `{"telemetry":{"cloud":{"enabled":true,"endpoint":"otel.example.com:4317","protocol":"grpc","tls":null,"headers":{"x-api-key":"********","x-tenant":"t9"}}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	headers, _ := valueAtPath(decodeJSONValue(t, storedRow(fake, "telemetry").Value), []string{"cloud", "headers"})
	assert.Equal(t, map[string]any{"x-api-key": "real-key", "x-tenant": "t9"}, headers)
}

// storedTelemetryWithTLS has a nested tls object beside the headers.
const storedTelemetryWithTLS = `{"cloud":{"endpoint":"otel.example.com:4317","tls":{"enabled":true,"ca_file":"/etc/ca.pem"},"headers":{"x-api-key":"real-key"}}}`

// A masked header next to sent members that change nothing relative to
// GET keeps the stored header; next to a real change it is rejected.
func TestPutServerConfigDB_TelemetryMaskedHeaderNoOpMembers(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantCode   int
	}{
		// Page-shaped echo of the GET block with empty values for the
		// members GET did not show.
		{"empty values for members GET omitted", `{"telemetry":{"cloud":{"endpoint":"otel.example.com:4317","protocol":"","provider":"","gcp_project_id":null,"batch":{},"tls":{"enabled":true,"ca_file":"/etc/ca.pem"},"headers":{"x-api-key":"********"}}}}`, http.StatusOK},
		{"empty nested object for an absent member", `{"telemetry":{"cloud":{"batch":{"max_size":null,"timeout":""},"headers":{"x-api-key":"********"}}}}`, http.StatusOK},
		{"partial nested object, no-op", `{"telemetry":{"cloud":{"tls":{"enabled":true},"headers":{"x-api-key":"********"}}}}`, http.StatusOK},
		{"partial nested object, changed", `{"telemetry":{"cloud":{"tls":{"enabled":false},"headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
		{"partial nested object, cleared member", `{"telemetry":{"cloud":{"tls":{"ca_file":""},"headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
		{"empty nested object over a stored one", `{"telemetry":{"cloud":{"tls":{},"headers":{"x-api-key":"********"}}}}`, http.StatusOK},
		{"null nested object over a stored one", `{"telemetry":{"cloud":{"tls":null,"headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
		{"empty string over a shown endpoint", `{"telemetry":{"cloud":{"endpoint":"","headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
		{"false for a member GET omitted", `{"telemetry":{"cloud":{"cloud_logging":false,"headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
		{"case-variant nested clear", `{"telemetry":{"cloud":{"tls":{"CA_FILE":""},"headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
		{"case-variant nested null", `{"telemetry":{"cloud":{"tls":{"Enabled":null},"headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
		{"case-variant nested no-op", `{"telemetry":{"cloud":{"TLS":{"Ca_File":"/etc/ca.pem"},"headers":{"x-api-key":"********"}}}}`, http.StatusOK},
		{"changed endpoint", `{"telemetry":{"cloud":{"endpoint":"other.example.com:4317","headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
		{"empty string for a nested object", `{"telemetry":{"cloud":{"tls":"","headers":{"x-api-key":"********"}}}}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			fake.seedWithOrigin("telemetry", json.RawMessage(storedTelemetryWithTLS), "managed")
			_, err := ops.Refresh(context.Background())
			require.NoError(t, err)

			rr := putServerConfigDB(t, srv, ops, tc.body)
			require.Equal(t, tc.wantCode, rr.Code, rr.Body.String())
			row := storedRow(fake, "telemetry")
			headers, _ := valueAtPath(decodeJSONValue(t, row.Value), []string{"cloud", "headers"})
			assert.Equal(t, map[string]any{"x-api-key": "real-key"}, headers, "the stored header is never replaced by the placeholder")
			assert.NotContains(t, string(row.Value), maskedValue)
		})
	}
}

// File mode replaces the telemetry object whole, so a masked header is
// kept only when the cloud object written, apart from headers, equals the
// stored one: an exact echo keeps; a partial or empty tls, a case-variant
// clear and a changed or cleared endpoint are rejected; "" for a member
// GET omitted keeps.
func TestPutServerConfig_FileMode_TelemetryMaskedHeaderCloudRule(t *testing.T) {
	const stored = `schema_version: "1"
telemetry:
  cloud:
    endpoint: otel.example.com:4317
    tls:
      enabled: true
      ca_file: /etc/ca.pem
    headers:
      x-api-key: real-key
`
	for _, tc := range []struct {
		name, cloud string
		wantCode    int
	}{
		{"exact echo", `"endpoint":"otel.example.com:4317","tls":{"enabled":true,"ca_file":"/etc/ca.pem"}`, http.StatusOK},
		{"empty string for a member GET omitted", `"endpoint":"otel.example.com:4317","protocol":"","tls":{"enabled":true,"ca_file":"/etc/ca.pem"}`, http.StatusOK},
		{"case-variant nested clear", `"endpoint":"otel.example.com:4317","tls":{"enabled":true,"CA_FILE":""}`, http.StatusBadRequest},
		{"partial tls", `"endpoint":"otel.example.com:4317","tls":{"enabled":true}`, http.StatusBadRequest},
		{"empty tls", `"endpoint":"otel.example.com:4317","tls":{}`, http.StatusBadRequest},
		{"omitted tls", `"endpoint":"otel.example.com:4317"`, http.StatusBadRequest},
		{"changed endpoint", `"endpoint":"other.example.com:4317","tls":{"enabled":true,"ca_file":"/etc/ca.pem"}`, http.StatusBadRequest},
		{"cleared endpoint", `"endpoint":"","tls":{"enabled":true,"ca_file":"/etc/ca.pem"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settingsPath := setTempScionHome(t)
			require.NoError(t, os.WriteFile(settingsPath, []byte(stored), 0600))
			srv := &Server{}
			rr := httptest.NewRecorder()
			srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
				`{"telemetry":{"cloud":{`+tc.cloud+`,"headers":{"x-api-key":"********"}}}}`))
			require.Equal(t, tc.wantCode, rr.Code, rr.Body.String())
			data, err := os.ReadFile(settingsPath)
			require.NoError(t, err)
			if tc.wantCode != http.StatusOK {
				assert.Equal(t, stored, string(data), "nothing is written")
				return
			}
			var vs config.VersionedSettings
			require.NoError(t, yamlv3.Unmarshal(data, &vs))
			require.NotNil(t, vs.Telemetry)
			require.NotNil(t, vs.Telemetry.Cloud)
			assert.Equal(t, map[string]string{"x-api-key": "real-key"}, vs.Telemetry.Cloud.Headers)
			assert.Equal(t, "/etc/ca.pem", vs.Telemetry.Cloud.TLS.CAFile)
		})
	}
}

func TestTelemetryCloudUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, next, cur string
		want            bool
	}{
		{"equal apart from headers", `{"cloud":{"endpoint":"e","headers":{"a":"1"}}}`, `{"cloud":{"endpoint":"e","headers":{"a":"2"}}}`, true},
		{"no cloud on either side", `{}`, `{"enabled":true}`, true},
		{"empty values pruned", `{"cloud":{"endpoint":"e","protocol":"","tls":{"ca_file":""},"batch":{}}}`, `{"cloud":{"endpoint":"e"}}`, true},
		{"changed member", `{"cloud":{"endpoint":"x"}}`, `{"cloud":{"endpoint":"e"}}`, false},
		{"dropped nested member", `{"cloud":{"tls":{"enabled":true}}}`, `{"cloud":{"tls":{"enabled":true,"ca_file":"/c"}}}`, false},
		{"false is a value", `{"cloud":{"cloud_logging":false}}`, `{"cloud":{}}`, false},
		{"scalar over object", `{"cloud":{"tls":"x"}}`, `{"cloud":{"tls":{"enabled":true}}}`, false},
		{"undecodable", `{`, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, telemetryCloudUnchanged(json.RawMessage(tc.next), json.RawMessage(tc.cur)))
		})
	}
}

// File mode: a masked header sent next to a cleared endpoint is rejected
// and settings.yaml is left as it is.
func TestPutServerConfig_FileMode_TelemetryMaskedHeaderClearedEndpointRejected(t *testing.T) {
	settingsPath := setTempScionHome(t)
	original := `schema_version: "1"
telemetry:
  cloud:
    endpoint: otel.example.com:4317
    headers:
      x-api-key: real-key
`
	require.NoError(t, os.WriteFile(settingsPath, []byte(original), 0600))
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"telemetry":{"cloud":{"endpoint":"","headers":{"x-api-key":"********"}}}}`))
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
}

// File mode: the same masked echo keeps the stored header in settings.yaml,
// and a real value replaces it.
func TestPutServerConfig_FileMode_TelemetryMaskedHeadersEchoKeeps(t *testing.T) {
	settingsPath := setTempScionHome(t)
	require.NoError(t, os.WriteFile(settingsPath, []byte(`schema_version: "1"
telemetry:
  cloud:
    endpoint: otel.example.com:4317
    headers:
      x-api-key: real-key
`), 0600))
	srv := &Server{}
	readHeaders := func() map[string]string {
		data, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		var vs config.VersionedSettings
		require.NoError(t, yamlv3.Unmarshal(data, &vs))
		require.NotNil(t, vs.Telemetry)
		require.NotNil(t, vs.Telemetry.Cloud)
		return vs.Telemetry.Cloud.Headers
	}

	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"telemetry":{"cloud":{"endpoint":"otel.example.com:4317","headers":{"x-api-key":"********"}}}}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, map[string]string{"x-api-key": "real-key"}, readHeaders())

	rr = httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"telemetry":{"cloud":{"endpoint":"otel.example.com:4317","headers":{"x-api-key":"new-key"}}}}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, map[string]string{"x-api-key": "new-key"}, readHeaders())
}

func TestMaskedTelemetry_DoesNotModifyInput(t *testing.T) {
	in := &config.V1TelemetryConfig{Cloud: &config.V1TelemetryCloudConfig{Headers: map[string]string{"a": "secret"}}}
	out := maskedTelemetry(in)
	assert.Equal(t, maskedValue, out.Cloud.Headers["a"])
	assert.Equal(t, "secret", in.Cloud.Headers["a"])
	none := &config.V1TelemetryConfig{Cloud: &config.V1TelemetryCloudConfig{Endpoint: "e"}}
	assert.Same(t, none, maskedTelemetry(none))
	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(out))
	assert.NotContains(t, buf.String(), "secret")
}

// The system registry PUT skips a write that would leave a managed
// endpoints row as it is, and writes a real change.
func TestSystemRegistry_UnchangedManagedRowNotRewritten(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	const stored = `{"public_url": "https://hub.example.com", "image_registry": "registry.example.com"}`
	fake.seedWithOrigin("endpoints", json.RawMessage(stored), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	put := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := adminRequest(http.MethodPut, "/api/v1/system/registry", body)
		req.RemoteAddr = "127.0.0.1:1234"
		srv.handleSystemRegistry(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	}
	put(`{"image_registry":"registry.example.com"}`)
	row := storedRow(fake, "endpoints")
	assert.Equal(t, stored, string(row.Value), "an unchanged registry must not rewrite the row")
	assert.Equal(t, int64(1), row.Revision)

	put(`{"image_registry":"other.example.com"}`)
	row = storedRow(fake, "endpoints")
	assert.Equal(t, int64(2), row.Revision)
	got, _ := valueAtPath(decodeJSONValue(t, row.Value), []string{"image_registry"})
	assert.Equal(t, "other.example.com", got)
}
