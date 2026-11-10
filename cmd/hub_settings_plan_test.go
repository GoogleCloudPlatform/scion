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

package cmd

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSettingsSchema mirrors the shape of the Hub's schema response for
// the sections the planning rules care about.
func testSettingsSchema() map[string]hubclient.ServerConfigSectionSchema {
	return map[string]hubclient.ServerConfigSectionSchema{
		"agent_defaults": {KoanfPaths: []string{"default_max_turns", "default_model", "default_resources", "default_max_agent_role", "default_agent_role"}},
		"endpoints":      {KoanfPaths: []string{"server.hub.public_url", "image_registry"}, RestartRequired: []string{"server.hub.public_url"}},
		"access":         {KoanfPaths: []string{"server.hub.admin_emails", "server.auth.default_user_role"}},
		"telemetry": {KoanfPaths: []string{
			"telemetry.enabled", "telemetry.cloud", "telemetry.cloud.tls", "telemetry.cloud.tls.enabled",
			"telemetry.cloud.tls.cert_file", "telemetry.cloud.tls.key_file", "telemetry.resource",
		}},
		"gcp_iam":          {KoanfPaths: []string{"server.hub.gcp_iam_check_mode", "server.hub.gcp_iam_deny_unknown_policy"}},
		"github_app":       {KoanfPaths: []string{"server.github_app", "server.github_app.app_id"}},
		"federation":       {KoanfPaths: []string{"server.federation.enabled", "server.federation.algorithms"}},
		"runtimes":         {KoanfPaths: []string{"runtimes"}},
		"profiles":         {KoanfPaths: []string{"profiles"}},
		"project_defaults": {KoanfPaths: []string{"project_defaults.default_scratchpad"}},
		"experiments":      {},
	}
}

// testServerConfig returns a GET response with two runtimes, one profile
// and section metadata (agent_defaults stored at revision 7; endpoints and
// runtimes stored; telemetry from the startup configuration).
func testServerConfig(t *testing.T) *hubclient.ServerConfig {
	t.Helper()
	var cfg hubclient.ServerConfig
	require.NoError(t, json.Unmarshal([]byte(`{
		"settings_tier": "db",
		"layer0_editable": false,
		"default_max_turns": 100,
		"server": {"hub": {"public_url": "https://hub.example"}},
		"federation": {"enabled": false},
		"telemetry": {"enabled": true, "resource": {"team": "a", "env": "prod"}},
		"runtimes": {"docker": {"type": "docker", "image": "old"}, "k8s": {"type": "kubernetes"}},
		"profiles": {"small": {"runtime": "docker", "env": {"A": "1"}}},
		"section_metadata": {
			"agent_defaults": {"source": "db", "revision": 7},
			"endpoints": {"source": "db", "revision": 3},
			"runtimes": {"source": "db", "revision": 9},
			"profiles": {"source": "db", "revision": 2},
			"telemetry": {"source": "file"},
			"federation": {"source": "default"},
			"access": {"source": "db", "revision": 1},
			"gcp_iam": {"source": "db", "revision": 1},
			"github_app": {"source": "db", "revision": 1}
		}
	}`), &cfg))
	return &cfg
}

func planJSON(t *testing.T, w *settingsWrite) string {
	t.Helper()
	b, err := json.Marshal(w.Body)
	require.NoError(t, err)
	return string(b)
}

func TestSettingsKeyIndex_Resolve(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	cases := []struct {
		key, schemaKey, section string
		ok                      bool
	}{
		{"default_max_turns", "default_max_turns", "agent_defaults", true},
		{"server.hub.public_url", "server.hub.public_url", "endpoints", true},
		{"runtimes", "runtimes", "runtimes", true},
		{"runtimes.docker.image", "runtimes", "runtimes", true},
		{"telemetry.cloud.tls.enabled", "telemetry.cloud.tls.enabled", "telemetry", true}, // longest prefix
		{"telemetry.cloud.endpoint", "telemetry.cloud", "telemetry", true},
		{"federation.enabled", "server.federation.enabled", "federation", true},
		{"server.federation.enabled", "server.federation.enabled", "federation", true},
		{"default_max_turnsx", "", "", false}, // a prefix match needs a dot
		{"nope", "", "", false},
	}
	for _, c := range cases {
		sk, sec, ok := ix.resolve(c.key)
		assert.Equal(t, c.ok, ok, c.key)
		assert.Equal(t, c.schemaKey, sk, c.key)
		assert.Equal(t, c.section, sec, c.key)
	}
	assert.Contains(t, ix.suggest("default_max_turn"), "default_max_turns")
	assert.Contains(t, ix.suggest("hub.public_url"), "server.hub.public_url", "same last segment")
	assert.Empty(t, ix.suggest("completely_unrelated_thing"))
}

func TestSettingsBodyPath(t *testing.T) {
	assert.Equal(t, []string{"federation", "enabled"}, settingsBodyPath("server.federation.enabled"))
	assert.Equal(t, []string{"federation"}, settingsBodyPath("server.federation"))
	assert.Equal(t, []string{"server", "hub", "public_url"}, settingsBodyPath("server.hub.public_url"))
}

func TestBuildSettingsWrite_SparseSetWithRevisions(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	w, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{
		{Key: "default_max_turns", Value: 200},
		{Key: "server.hub.public_url", Value: "https://new.example"},
		{Key: "telemetry.enabled", Value: false},
	}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"default_max_turns":200,"server":{"hub":{"public_url":"https://new.example"}},"telemetry":{"enabled":false}}`, planJSON(t, w))
	assert.Equal(t, []string{"agent_defaults", "endpoints", "telemetry"}, w.Sections)
	// telemetry has no stored row: revision 0, which the Hub reads as
	// create-only.
	assert.Equal(t, map[string]int64{"agent_defaults": 7, "endpoints": 3, "telemetry": 0}, w.Revisions)
}

func TestBuildSettingsWrite_FederationIsSentTopLevel(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	w, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "federation.enabled", Value: true}}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"federation":{"enabled":true}}`, planJSON(t, w))
	assert.Equal(t, map[string]int64{"federation": 0}, w.Revisions)
}

func TestBuildSettingsWrite_UnsetSendsNull(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	w, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{
		{Key: "default_max_turns", Unset: true},
		{Key: "runtimes", Unset: true},
	}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"default_max_turns":null,"runtimes":null}`, planJSON(t, w))
}

func TestBuildSettingsWrite_UnsetRefusedKeys(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	for _, k := range []string{"server.hub.gcp_iam_check_mode", "server.hub.gcp_iam_deny_unknown_policy", "default_max_agent_role", "default_agent_role", "server.github_app"} {
		_, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: k, Unset: true}}, settingsPlanOptions{})
		require.Error(t, err, k)
		assert.Contains(t, err.Error(), "cannot unset", k)
	}
	// Each github_app key can still be unset on its own.
	_, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "server.github_app.app_id", Unset: true}}, settingsPlanOptions{})
	require.NoError(t, err)
}

func TestBuildSettingsWrite_PairedUnset(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	_, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "telemetry.cloud.tls.cert_file", Unset: true}}, settingsPlanOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "telemetry.cloud.tls.key_file")

	w, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{
		{Key: "telemetry.cloud.tls.cert_file", Unset: true},
		{Key: "telemetry.cloud.tls.key_file", Unset: true},
	}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"telemetry":{"cloud":{"tls":{"cert_file":null,"key_file":null}}}}`, planJSON(t, w))
}

func TestBuildSettingsWrite_BelowMapKeySendsWholeMap(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	w, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{
		{Key: "runtimes.docker.image", Value: "new"},
		{Key: "runtimes.extra.type", Value: "docker"},
	}, settingsPlanOptions{})
	require.NoError(t, err)
	// The k8s entry is kept: the whole map is sent.
	assert.JSONEq(t, `{"runtimes":{"docker":{"type":"docker","image":"new"},"k8s":{"type":"kubernetes"},"extra":{"type":"docker"}}}`, planJSON(t, w))
	assert.Equal(t, map[string]int64{"runtimes": 9}, w.Revisions)
}

func TestBuildSettingsWrite_UnsetBelowMapKeyRemovesOnlyThatPart(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	w, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{
		{Key: "runtimes.k8s", Unset: true},
		{Key: "runtimes.docker.image", Unset: true},
	}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"runtimes":{"docker":{"type":"docker"}}}`, planJSON(t, w))

	// A profile leaf: the rest of the entry and the map are kept.
	w, err = buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "profiles.small.env.A", Unset: true}}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"profiles":{"small":{"runtime":"docker","env":{}}}}`, planJSON(t, w))

	// The last entry: an empty map, which the Hub stores as empty.
	w, err = buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "profiles.small", Unset: true}}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"profiles":{}}`, planJSON(t, w))

	_, err = buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "runtimes.missing", Unset: true}}, settingsPlanOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not set")
}

func TestBuildSettingsWrite_BelowObjectKeyKeepsSiblings(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	w, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "telemetry.resource.team", Value: "b"}}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"telemetry":{"resource":{"team":"b","env":"prod"}}}`, planJSON(t, w))

	// Below an unset key: start from an empty object.
	w, err = buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "default_resources.requests.cpu", Value: "1"}}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"default_resources":{"requests":{"cpu":"1"}}}`, planJSON(t, w))

	// Below a scalar: refused.
	_, err = buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "default_max_turns.x", Value: 1}}, settingsPlanOptions{})
	require.Error(t, err)
}

func TestBuildSettingsWrite_Conflicts(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	cases := map[string][]settingsChange{
		"overlapping schema keys": {{Key: "telemetry.cloud", Value: map[string]interface{}{}}, {Key: "telemetry.cloud.tls.enabled", Value: true}},
		"whole and below":         {{Key: "runtimes", Value: map[string]interface{}{}}, {Key: "runtimes.docker.image", Value: "x"}},
		"given twice":             {{Key: "default_max_turns", Value: 1}, {Key: "default_max_turns", Value: 2}},
		"expected_revisions key":  {{Key: "expected_revisions", Value: 1}},
		"empty":                   {},
	}
	for name, changes := range cases {
		_, err := buildSettingsWrite(testServerConfig(t), ix, changes, settingsPlanOptions{})
		assert.Error(t, err, name)
	}
}

func TestBuildSettingsWrite_UnknownKeys(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	cfg := testServerConfig(t)
	_, err := buildSettingsWrite(cfg, ix, []settingsChange{{Key: "default_max_turn", Value: 1}}, settingsPlanOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean default_max_turns")

	w, err := buildSettingsWrite(cfg, ix, []settingsChange{{Key: "server.port", Value: 9}}, settingsPlanOptions{AllowUnknown: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"server":{"port":9}}`, planJSON(t, w))
	assert.Equal(t, []string{"server.port"}, w.Unlisted)
	assert.Empty(t, w.Revisions, "an unlisted key belongs to no section")

	cfg.Layer0Editable = true
	_, err = buildSettingsWrite(cfg, ix, []settingsChange{{Key: "server.port", Value: 9}}, settingsPlanOptions{})
	require.NoError(t, err, "a hub with editable file-backed keys accepts unlisted keys")
}

func TestBuildSettingsWrite_ProjectDefaultsRedirected(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	_, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "project_defaults.default_scratchpad", Value: false}}, settingsPlanOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scion hub project-defaults")
}

func TestBuildSettingsWrite_RevisionPins(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	w, err := buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "default_max_turns", Value: 1}},
		settingsPlanOptions{Revisions: map[string]int64{"agent_defaults": 42}})
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"agent_defaults": 42}, w.Revisions)

	_, err = buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "default_max_turns", Value: 1}},
		settingsPlanOptions{Revisions: map[string]int64{"endpoints": 1}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not touch")
}

func TestBuildSettingsWrite_PinOnReadModifyWriteSection(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	below := []settingsChange{{Key: "runtimes.docker.image", Value: "new"}}

	// A pin other than the revision just read is refused: the value sent is
	// built from that read.
	for _, pin := range []int64{10, 8} {
		_, err := buildSettingsWrite(testServerConfig(t), ix, below, settingsPlanOptions{Revisions: map[string]int64{"runtimes": pin}})
		require.Error(t, err, pin)
		assert.Contains(t, err.Error(), "runtimes is at revision 9")
	}
	// The revision just read is accepted.
	w, err := buildSettingsWrite(testServerConfig(t), ix, below, settingsPlanOptions{Revisions: map[string]int64{"runtimes": 9}})
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"runtimes": 9}, w.Revisions)
	// A whole-key change keeps any pin.
	w, err = buildSettingsWrite(testServerConfig(t), ix, []settingsChange{{Key: "runtimes", Value: map[string]interface{}{}}}, settingsPlanOptions{Revisions: map[string]int64{"runtimes": 3}})
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"runtimes": 3}, w.Revisions)
}

func TestMaskSensitiveSettings(t *testing.T) {
	in := map[string]interface{}{
		"server": map[string]interface{}{
			"github_app":            map[string]interface{}{"private_key_path": "/k.pem", "app_id": 7},
			"oauth":                 map[string]interface{}{"web": map[string]interface{}{"google": map[string]interface{}{"client_id": "id", "client_secret": "cs"}}},
			"notification_channels": []interface{}{map[string]interface{}{"type": "slack", "params": map[string]interface{}{"url": "https://hooks"}}},
			"auth":                  map[string]interface{}{"dev_token": "dt", "default_user_role": "member"},
		},
		"telemetry":     map[string]interface{}{"cloud": map[string]interface{}{"headers": map[string]interface{}{"X": "h"}, "tls": map[string]interface{}{"key_file": "/tls.key"}}},
		"api_keys":      []interface{}{"a", "b", ""},
		"unset_this":    nil,
		"agent_secrets": map[string]interface{}{"user_scope_only": true},
		"dev_token":     float64(123456),
	}
	in["server"].(map[string]interface{})["database"] = map[string]interface{}{"driver": "postgres", "url": "postgres://u:pw@db/scion"}
	got := maskSensitiveSettings(in)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"server": {
			"github_app": {"private_key_path": "********", "app_id": 7},
			"oauth": {"web": {"google": {"client_id": "id", "client_secret": "********"}}},
			"notification_channels": [{"type": "slack", "params": {"url": "********"}}],
			"auth": {"dev_token": "********", "default_user_role": "member"},
			"database": {"driver": "postgres", "url": "********"}
		},
		"telemetry": {"cloud": {"headers": {"X": "********"}, "tls": {"key_file": "/tls.key"}}},
		"api_keys": ["********", "********", ""],
		"unset_this": null,
		"agent_secrets": {"user_scope_only": true},
		"dev_token": "********"
	}`, string(b), "strings and numbers under secret keys and hub-masked paths are masked; booleans, nulls, empty strings and other keys are kept")
	// The input is not changed.
	assert.Equal(t, "cs", in["server"].(map[string]interface{})["oauth"].(map[string]interface{})["web"].(map[string]interface{})["google"].(map[string]interface{})["client_secret"])
}

func TestBuildSettingsWrite_NoMetadataSendsNoRevision(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	cfg := testServerConfig(t)
	cfg.SectionMetadata = nil // a file-backed hub
	w, err := buildSettingsWrite(cfg, ix, []settingsChange{{Key: "default_max_turns", Value: 1}}, settingsPlanOptions{})
	require.NoError(t, err)
	assert.Empty(t, w.Revisions)
}

func TestParseSettingsValue(t *testing.T) {
	cases := []struct {
		in       string
		asString bool
		want     interface{}
	}{
		{"true", false, true},
		{"42", false, 42},
		{"1.5", false, 1.5},
		{"text", false, "text"},
		{`"007"`, false, "007"},
		{"007", true, "007"},
		{"true", true, "true"},
		{"", false, ""},
		{"[a, b]", false, []interface{}{"a", "b"}},
		{"{k: v, n: {m: 1}}", false, map[string]interface{}{"k": "v", "n": map[string]interface{}{"m": 1}}},
	}
	for _, c := range cases {
		got, err := parseSettingsValue(c.in, c.asString)
		require.NoError(t, err, c.in)
		assert.Equal(t, c.want, got, c.in)
	}
	for _, bad := range []string{"null", "~", "[unclosed"} {
		_, err := parseSettingsValue(bad, false)
		require.Error(t, err, bad)
		if bad != "null" { // the word itself names the refusal
			assert.NotContains(t, err.Error(), bad, "a parse error does not echo the value")
		}
	}
}

func TestSettingsChangesFromDocument(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	doc := map[string]interface{}{
		"default_max_turns": 5,
		"server":            map[string]interface{}{"hub": map[string]interface{}{"public_url": nil}},
		"federation":        map[string]interface{}{"enabled": true},
		"runtimes":          map[string]interface{}{"docker": map[string]interface{}{"type": "docker"}},
		"telemetry":         map[string]interface{}{"cloud": map[string]interface{}{"tls": map[string]interface{}{"enabled": true}}},
		"unknown":           map[string]interface{}{"x": 1},
	}
	got := settingsChangesFromDocument(doc, ix)
	byKey := map[string]settingsChange{}
	for _, c := range got {
		byKey[c.Key] = c
	}
	assert.Len(t, got, 6)
	assert.Equal(t, 5, byKey["default_max_turns"].Value)
	assert.True(t, byKey["server.hub.public_url"].Unset, "a null is an unset")
	assert.Equal(t, true, byKey["federation.enabled"].Value)
	assert.Equal(t, map[string]interface{}{"docker": map[string]interface{}{"type": "docker"}}, byKey["runtimes"].Value, "a map key takes its whole value")
	assert.Equal(t, map[string]interface{}{"tls": map[string]interface{}{"enabled": true}}, byKey["telemetry.cloud"].Value, "stops at the first schema key")
	assert.Contains(t, byKey, "unknown")
}

func TestParseRevisionPins(t *testing.T) {
	got, err := parseRevisionPins([]string{"agent_defaults=7", " endpoints = 0 "})
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"agent_defaults": 7, "endpoints": 0}, got)
	for _, bad := range [][]string{{"x"}, {"=1"}, {"x=-1"}, {"x=a"}, {"x=1", "x=2"}} {
		_, err := parseRevisionPins(bad)
		assert.Error(t, err, bad)
	}
}

// TestMaskSensitiveSettings_CoversHubMaskedFields keeps the dry-run mask in
// step with the fields the Hub masks on GET. The Hub's list
// (maskedServerFields in pkg/hub/admin_settings_mask.go) is unexported, so
// this reads it from source: every path listed there must be masked by
// maskSensitiveSettings, by its name or by settingsHubMaskedPaths. A field
// added to the Hub list with a name the CLI rule misses fails here.
func TestMaskSensitiveSettings_CoversHubMaskedFields(t *testing.T) {
	src, err := os.ReadFile("../pkg/hub/admin_settings_mask.go")
	require.NoError(t, err)
	text := string(src)
	start := strings.Index(text, "var maskedServerFields = []maskedServerField{")
	require.GreaterOrEqual(t, start, 0, "maskedServerFields not found; update this guard")
	end := strings.Index(text[start:], "\n}\n")
	require.Greater(t, end, 0)
	block := text[start : start+end]

	var paths []string
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`oauthSecretField\("([^"]+)"`),
		regexp.MustCompile(`name:\s*"([^"]+)"`),
	} {
		for _, m := range re.FindAllStringSubmatch(block, -1) {
			paths = append(paths, m[1])
		}
	}
	// Count control: the list held 12 paths when this guard was written; a
	// parse that finds far fewer means the guard no longer reads it.
	require.GreaterOrEqual(t, len(paths), 12, "parsed %d paths from maskedServerFields: %v", len(paths), paths)
	// Shape control: every top-level entry of the list (a line indented by
	// exactly one tab that is not a comment or a closing brace) must be one
	// the parse above understood, so an entry in a new helper shape fails
	// here instead of going unchecked.
	topLevelEntry := regexp.MustCompile(`^\t[^\t}/]`)
	entries := 0
	for _, line := range strings.Split(block, "\n")[1:] {
		if topLevelEntry.MatchString(line) {
			entries++
		}
	}
	// Expected: the paths the guard parsed; actual: the entries in the Hub
	// list. An entry in a shape the parse misses makes them differ.
	require.Equal(t, len(paths), entries, "maskedServerFields has %d entries but %d parsed paths; teach this guard the new entry shape", entries, len(paths))

	maskedAt := func(path string) bool {
		body := map[string]interface{}{}
		require.NoError(t, putAtPath(body, settingsBodyPath(path), "secret-value"))
		b, err := json.Marshal(maskSensitiveSettings(body))
		require.NoError(t, err)
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(b, &raw))
		v, ok := hubclient.LookupDotted(raw, strings.Join(settingsBodyPath(path), "."))
		require.True(t, ok, path)
		return string(v) == `"`+settingsMaskedValue+`"`
	}
	for _, p := range paths {
		assert.True(t, maskedAt(p), "the Hub masks %s on GET; the dry-run mask must too", p)
	}
	// Negative control: a field the Hub does not mask is shown, so the check
	// above can fail.
	assert.False(t, maskedAt("server.hub.public_url"))

	// The positional entries mirror the Hub's non-scalar masks.
	assert.Contains(t, text, "NotificationChannels[i].Params", "the Hub masks channel params; settingsHubMaskedPaths lists them")
	assert.Contains(t, text, "maskedTelemetry", "the Hub masks telemetry headers; settingsHubMaskedPaths lists them")
}

// TestSettingsKeyIndex_SuggestSkipsFarKeys pins the suggestion bounds: the
// suggestions are those within edit distance 3 or with the same last segment,
// and a very long input returns at once (only last-segment matches apply).
func TestSettingsKeyIndex_SuggestSkipsFarKeys(t *testing.T) {
	ix := newSettingsKeyIndex(testSettingsSchema())
	assert.Equal(t, []string{"default_max_turns"}, ix.suggest("default_max_turn"))
	assert.Contains(t, ix.suggest("hub.public_url"), "server.hub.public_url", "same last segment, far in length")
	assert.Empty(t, ix.suggest("default_max_turns_and_more"), "length differs by more than 3")
	assert.Equal(t, 1, levenshtein("default_max_turn", "default_max_turns"))

	long := strings.Repeat("x", 1<<20)
	assert.Empty(t, ix.suggest(long))
	assert.Equal(t, []string{"server.hub.public_url"}, ix.suggest(long+".public_url"), "a long input still matches by its last segment")
	// suggest computes an edit distance only where suggestInRange holds: a
	// long input is compared against no schema key.
	for _, k := range ix.keys {
		assert.False(t, suggestInRange(long, k), "a long input must not be compared with %s", k)
	}
	assert.True(t, suggestInRange("default_max_turn", "default_max_turns"), "control: a close key is compared")
	assert.False(t, suggestInRange("default_max_turn", "default_max_turns_and_more"), "a key longer by more than 3 is skipped")
}
