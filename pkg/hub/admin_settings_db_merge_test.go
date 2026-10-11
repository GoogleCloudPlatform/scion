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

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const storedGitHubApp = `{"app_id":42,"api_base_url":"https://ghe.example.com/api/v3","webhooks_enabled":true,"installation_url":"https://github.com/apps/x","private_key_path":"/etc/ghapp/key.pem"}`

// githubAppRowRaw returns the stored github_app row as a generic map, so
// tests see keys the section struct does not model.
func githubAppRowRaw(t *testing.T, f *fakeHubSettingStore) map[string]interface{} {
	t.Helper()
	f.mu.Lock()
	row := f.settings["github_app"]
	f.mu.Unlock()
	require.NotNil(t, row, "github_app row missing")
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(row.Value, &m))
	return m
}

// mergeGitHubApp runs mergeSectionOnCurrent for github_app the way the
// server-config PUT does, and returns the merged document as a map.
func mergeGitHubApp(t *testing.T, ops *OperationalSettings, body string) (map[string]interface{}, int64) {
	t.Helper()
	var req ServerConfigUpdateDBRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	fp, err := parseFieldPresence([]byte(body))
	require.NoError(t, err)
	reqDoc, err := buildSingleSectionDoc(&req.ServerConfigUpdateRequest, "github_app", fp)
	require.NoError(t, err)
	doc, rev, err := mergeSectionOnCurrent(context.Background(), ops, "github_app", reqDoc, githubAppPresence([]byte(body)))
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(doc, &m))
	return m, rev
}

func TestMergeSectionOnCurrent_GitHubApp(t *testing.T) {
	stored := func() map[string]interface{} {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(storedGitHubApp), &m)
		return m
	}
	for _, tc := range []struct {
		name string
		ga   string
		edit func(m map[string]interface{})
	}{
		{"omitted private_key_path is kept", `{"app_id": 43}`, func(m map[string]interface{}) { m["app_id"] = float64(43) }},
		{"empty private_key_path clears it", `{"private_key_path": ""}`, func(m map[string]interface{}) { delete(m, "private_key_path") }},
		{"null private_key_path clears it", `{"private_key_path": null}`, func(m map[string]interface{}) { delete(m, "private_key_path") }},
		{"new private_key_path replaces it", `{"private_key_path": "/k2.pem"}`, func(m map[string]interface{}) { m["private_key_path"] = "/k2.pem" }},
		{"case-insensitive key clears it", `{"Private_Key_Path": ""}`, func(m map[string]interface{}) { delete(m, "private_key_path") }},
		{"explicit false webhooks_enabled is stored", `{"webhooks_enabled": false}`, func(m map[string]interface{}) { m["webhooks_enabled"] = false }},
		{"case-insensitive false webhooks_enabled is stored", `{"Webhooks_Enabled": false}`, func(m map[string]interface{}) { m["webhooks_enabled"] = false }},
		{"null webhooks_enabled clears it", `{"webhooks_enabled": null}`, func(m map[string]interface{}) { delete(m, "webhooks_enabled") }},
		// encoding/json folds the long s (U+017F) to s; so does the merge.
		{"unicode-folded key replaces it", `{"inſtallation_url": "https://github.com/apps/y"}`, func(m map[string]interface{}) { m["installation_url"] = "https://github.com/apps/y" }},
		{"zero app_id clears it", `{"app_id": 0}`, func(m map[string]interface{}) { delete(m, "app_id") }},
		{"request-only secret members change nothing", `{"private_key": "", "webhook_secret": ""}`, func(map[string]interface{}) {}},
		{"empty object changes nothing", `{}`, func(map[string]interface{}) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fakeStore, ops := newTestDBServer(t)
			fakeStore.seedWithOrigin("github_app", json.RawMessage(storedGitHubApp), "managed")

			got, rev := mergeGitHubApp(t, ops, `{"server": {"github_app": `+tc.ga+`}}`)
			want := stored()
			tc.edit(want)
			assert.Equal(t, want, got)
			assert.Equal(t, int64(1), rev, "the base revision is the row revision")
		})
	}
}

// With no row the base is empty: the doc holds only the sent keys, and the
// CAS base is 0 (create-only).
func TestMergeSectionOnCurrent_NoRow(t *testing.T) {
	_, _, ops := newTestDBServer(t)
	got, rev := mergeGitHubApp(t, ops, `{"server": {"github_app": {"app_id": 7, "private_key_path": ""}}}`)
	assert.Equal(t, map[string]interface{}{"app_id": float64(7)}, got)
	assert.Equal(t, int64(0), rev)
}

// A seeded (non-managed) row does not carry an env-overridden key forward.
func TestMergeSectionOnCurrent_SeededRowDropsEnvOverriddenKeys(t *testing.T) {
	_, fakeStore, ops := newTestDBServer(t)
	ops.envOverrides = map[string]bool{"server.github_app.private_key_path": true}
	fakeStore.seedWithOrigin("github_app", json.RawMessage(storedGitHubApp), "seeded")

	got, _ := mergeGitHubApp(t, ops, `{"server": {"github_app": {"app_id": 42}}}`)
	assert.NotContains(t, got, "private_key_path")
	assert.Equal(t, "https://ghe.example.com/api/v3", got["api_base_url"])

	// A managed row is carried as is.
	fakeStore.seedWithOrigin("github_app", json.RawMessage(storedGitHubApp), "managed")
	got, _ = mergeGitHubApp(t, ops, `{"server": {"github_app": {"app_id": 42}}}`)
	assert.Equal(t, "/etc/ghapp/key.pem", got["private_key_path"])
}

// Raw-JSON test: a stored key the section struct does not model survives
// the merge when the section schema allows extra keys. No section wired to
// the helper allows them today (github_app sets additionalProperties to
// false), so the allow path is covered here end to end only through
// runtimes, where nothing the body sends applies, and at the unit level in
// TestApplySectionPatch_SentKeyChangesUnmodelledKeySurvives.
func TestMergeSectionOnCurrent_KeepsUnmodelledKeyWhenSchemaAllows(t *testing.T) {
	_, fakeStore, ops := newTestDBServer(t)
	// runtimes allows any top-level key (a map of runtime entries) and
	// has no section-level key mapping, so nothing the body sends applies.
	fakeStore.seedWithOrigin("runtimes", json.RawMessage(`{"local":{"type":"docker"},"future_runtime":{"type":"x","new_leaf":1}}`), "managed")
	fp, err := parseFieldPresence([]byte(`{"local": null}`))
	require.NoError(t, err)

	doc, rev, err := mergeSectionOnCurrent(context.Background(), ops, "runtimes", json.RawMessage(`{}`), fp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"local":{"type":"docker"},"future_runtime":{"type":"x","new_leaf":1}}`, string(doc))
	assert.Equal(t, int64(1), rev)

	// The schema rule itself: extra keys survive unless the object schema
	// sets additionalProperties to false.
	doc2 := map[string]json.RawMessage{"known": json.RawMessage(`1`), "extra": json.RawMessage(`2`)}
	props := map[string]interface{}{"known": map[string]interface{}{"type": "integer"}}
	assert.Empty(t, dropKeysForbiddenBySchema("s", map[string]interface{}{"properties": props}, doc2))
	assert.Empty(t, dropKeysForbiddenBySchema("s", map[string]interface{}{"properties": props, "additionalProperties": true}, doc2))
	assert.Len(t, doc2, 2)
	assert.Equal(t, []string{"s.extra"},
		dropKeysForbiddenBySchema("s", map[string]interface{}{"properties": props, "additionalProperties": false}, doc2))
	assert.Equal(t, map[string]json.RawMessage{"known": json.RawMessage(`1`)}, doc2)
}

// A sent, modelled key changes while a stored key the section does not
// model survives, under a schema that allows extra keys; under the real
// github_app schema (additionalProperties: false) the extra key is dropped.
func TestApplySectionPatch_SentKeyChangesUnmodelledKeySurvives(t *testing.T) {
	newBase := func() map[string]json.RawMessage {
		return map[string]json.RawMessage{
			"app_id":           json.RawMessage(`42`),
			"private_key_path": json.RawMessage(`"/etc/ghapp/key.pem"`),
			"future_leaf":      json.RawMessage(`{"x":1}`),
		}
	}
	fp, err := parseFieldPresence([]byte(`{"private_key_path": "/k2.pem", "app_id": null}`))
	require.NoError(t, err)
	next := map[string]json.RawMessage{"private_key_path": json.RawMessage(`"/k2.pem"`)}

	base := newBase()
	sent := applySectionPatch("github_app", base, next, fp)
	assert.Equal(t, map[string]bool{"private_key_path": true, "app_id": true}, sent)
	permissive := map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"app_id": map[string]interface{}{}, "private_key_path": map[string]interface{}{},
	}}
	assert.Empty(t, dropKeysForbiddenBySchema("github_app", permissive, base))
	assert.Equal(t, map[string]json.RawMessage{
		"private_key_path": json.RawMessage(`"/k2.pem"`),
		"future_leaf":      json.RawMessage(`{"x":1}`),
	}, base)

	base = newBase()
	applySectionPatch("github_app", base, next, fp)
	strict, _ := opsettings.SchemaInfo()["github_app"].Schema.(map[string]interface{})
	require.NotNil(t, strict)
	assert.Equal(t, []string{"github_app.future_leaf"}, dropKeysForbiddenBySchema("github_app", strict, base))
	assert.Equal(t, map[string]json.RawMessage{"private_key_path": json.RawMessage(`"/k2.pem"`)}, base)
}

// A carried-forward (unsent) stored value that fails the section schema is
// dropped; a sent key is never dropped, and a valid doc is left alone.
func TestDropInvalidCarriedKeys(t *testing.T) {
	validate := func(doc json.RawMessage) bool { return len(opsettings.Validate("github_app", doc)) == 0 }
	schema, _ := opsettings.SchemaInfo()["github_app"].Schema.(map[string]interface{})

	doc := map[string]json.RawMessage{
		"app_id":           json.RawMessage(`"42"`),
		"private_key_path": json.RawMessage(`"/etc/ghapp/key.pem"`),
		"installation_url": json.RawMessage(`"https://github.com/apps/y"`),
	}
	dropped := dropInvalidCarriedKeys("github_app", schema, doc, map[string]bool{"installation_url": true}, validate)
	assert.Equal(t, []string{"github_app.app_id"}, dropped)
	assert.NotContains(t, doc, "app_id")
	assert.Len(t, doc, 2)

	// The same invalid value, when sent, stays for the write to reject.
	doc = map[string]json.RawMessage{"app_id": json.RawMessage(`"42"`)}
	assert.Empty(t, dropInvalidCarriedKeys("github_app", schema, doc, map[string]bool{"app_id": true}, validate))
	assert.Contains(t, doc, "app_id")

	// A valid doc: nothing dropped.
	doc = map[string]json.RawMessage{"app_id": json.RawMessage(`42`)}
	assert.Empty(t, dropInvalidCarriedKeys("github_app", schema, doc, nil, validate))

	// A schema with top-level required keys is not checked key by key.
	doc = map[string]json.RawMessage{"app_id": json.RawMessage(`"42"`)}
	withRequired := map[string]interface{}{"required": []interface{}{"app_id"}}
	assert.Empty(t, dropInvalidCarriedKeys("github_app", withRequired, doc, nil, validate))
}

// AC1/AC2 at the handler: a server-config PUT that omits
// server.github_app.private_key_path keeps the stored value; an explicit ""
// clears it.
func TestPutServerConfigDB_GitHubAppMergesOnCurrentRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("github_app", json.RawMessage(storedGitHubApp), "managed")

	rr := putServerConfigDB(t, srv, ops, `{"server":{"github_app":{"app_id":42,"webhooks_enabled":false}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row := githubAppRowRaw(t, fakeStore)
	assert.Equal(t, "/etc/ghapp/key.pem", row["private_key_path"])
	assert.Equal(t, "https://ghe.example.com/api/v3", row["api_base_url"])
	assert.Equal(t, "https://github.com/apps/x", row["installation_url"])
	assert.Equal(t, false, row["webhooks_enabled"])

	rr = putServerConfigDB(t, srv, ops, `{"server":{"github_app":{"private_key_path":""}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row = githubAppRowRaw(t, fakeStore)
	assert.NotContains(t, row, "private_key_path")
	assert.Equal(t, float64(42), row["app_id"])
	assert.Empty(t, ops.Snapshot().GitHubPrivateKeyPath)
}

// Round trip, the way the settings page saves: GET the server config, drop
// private_key_path from the GitHub App block (the page's cached copy has
// none), PUT the whole body back. The stored path is unchanged, and GET
// still reports it.
func TestPutServerConfigDB_GitHubAppRoundTripKeepsPrivateKeyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("github_app", json.RawMessage(storedGitHubApp), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	before := getServerConfigDB(t, srv, ops)
	require.NotNil(t, before.Server)
	require.NotNil(t, before.Server.GitHubApp)
	require.Equal(t, "/etc/ghapp/key.pem", before.Server.GitHubApp.PrivateKeyPath)

	b, err := json.Marshal(before.ServerConfigResponse)
	require.NoError(t, err)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &body))
	ga := body["server"].(map[string]interface{})["github_app"].(map[string]interface{})
	delete(ga, "private_key_path")
	ga["installation_url"] = "https://github.com/apps/y"
	b, err = json.Marshal(body)
	require.NoError(t, err)

	rr := putServerConfigDB(t, srv, ops, string(b))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	row := githubAppRowRaw(t, fakeStore)
	assert.Equal(t, "/etc/ghapp/key.pem", row["private_key_path"])
	assert.Equal(t, "https://github.com/apps/y", row["installation_url"])
	after := getServerConfigDB(t, srv, ops)
	assert.Equal(t, "/etc/ghapp/key.pem", after.Server.GitHubApp.PrivateKeyPath)
}

// githubAppRaceStore simulates another replica writing the github_app row
// between the PUT's read of it and its write. With noRow, the first read
// reports the row as missing and the other replica creates it.
type githubAppRaceStore struct {
	*fakeHubSettingStore
	noRow bool
	once  sync.Once
}

func (c *githubAppRaceStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if section != "github_app" {
		return c.fakeHubSettingStore.GetHubSetting(ctx, section)
	}
	row, err := c.fakeHubSettingStore.GetHubSetting(ctx, section)
	var snapshot *store.HubSetting
	if err == nil {
		cp := *row
		snapshot = &cp
	}
	raced := false
	c.once.Do(func() {
		raced = true
		_, _ = c.UpsertHubSetting(ctx, "github_app",
			json.RawMessage(`{"app_id":99,"private_key_path":"/other.pem"}`), "other-replica", -1, "managed")
	})
	if raced && c.noRow {
		return nil, store.ErrNotFound
	}
	if snapshot != nil {
		return snapshot, nil
	}
	return row, err
}

// AC3: the merged github_app write is CAS-guarded on the revision it read,
// so a concurrent write yields 409 and the other writer's row stands.
func TestPutServerConfigDB_GitHubAppMerge_ConcurrentWrite409(t *testing.T) {
	for _, noRow := range []bool{false, true} {
		name := "existing row"
		if noRow {
			name = "no row"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fake := newFakeHubSettingStore()
			if !noRow {
				fake.seedWithOrigin("github_app", json.RawMessage(storedGitHubApp), "managed")
			}
			raceStore := &githubAppRaceStore{fakeHubSettingStore: fake, noRow: noRow}
			ops := NewOperationalSettings(raceStore, emptyKoanf(), emptyKoanf())
			srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
			srv.SetOperationalSettings(ops)

			rr := putServerConfigDB(t, srv, ops, `{"server":{"github_app":{"installation_url":"https://github.com/apps/y"}}}`)
			require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
			row := githubAppRowRaw(t, fake)
			assert.Equal(t, map[string]interface{}{"app_id": float64(99), "private_key_path": "/other.pem"}, row)
		})
	}
}

// Masked secrets echoed back from GET (the settings page sends the GitHub
// App block as GET showed it) neither overwrite nor remove anything in the
// stored github_app row.
func TestPutServerConfigDB_GitHubAppMaskedEchoLeavesRowUnchanged(t *testing.T) {
	settingsPath := setTempScionHome(t)
	fileSettings := "schema_version: \"1\"\nserver:\n  github_app:\n    app_id: 42\n    webhook_secret: " + rtWebhookSec + "\n"
	require.NoError(t, os.WriteFile(settingsPath, []byte(fileSettings), 0600))
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seedWithOrigin("github_app", json.RawMessage(storedGitHubApp), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	server := getServerSection(t, rr.Body.Bytes())
	ga := server["github_app"].(map[string]interface{})
	require.Equal(t, maskedValue, ga["webhook_secret"])

	payload := map[string]interface{}{"server": map[string]interface{}{
		"hub":        map[string]interface{}{"admin_emails": []string{"b@example.com"}},
		"github_app": ga,
	}}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	rr = putServerConfigDB(t, srv, ops, string(body))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var want map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(storedGitHubApp), &want))
	assert.Equal(t, want, githubAppRowRaw(t, fakeStore))
}
