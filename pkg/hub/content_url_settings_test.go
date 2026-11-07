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
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// server.hub.content_url on the server-config API (ptone/scion#7107 phase
// A, condition 4): admins set it in the same modes as public_url, user
// access tokens cannot write it or reset its section, a token body that
// mixes it with an allowed key is refused whole, every change of the value
// in effect is audited (save and reset), and a save is validated (shape and
// the host-name rule). Kill checks K8', K9, K15', K16', K17' and K18'.

// contentURLSettingsFixture is a DB-mode server whose public origin is
// loTestHubOrigin, with a SQLite store for audit records behind a switch
// that makes audit writes fail. A save or reset applies to the server as
// it does in production (selfApply needs ops.server), so the tests can
// check the value in effect through HubEndpoints().Content.
func contentURLSettingsFixture(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings, store.Store, *storeFaultSwitch) {
	t.Helper()
	srv, fake, ops, st, fault := publicURLAuditFixture(t)
	srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
	ops.server = srv
	return srv, fake, ops, st, fault
}

// storedContentURL returns content_url from the stored endpoints row, and
// whether the row sets it.
func storedContentURL(t *testing.T, fake *fakeHubSettingStore) (string, bool) {
	t.Helper()
	row, err := fake.GetHubSetting(context.Background(), "endpoints")
	if err != nil {
		return "", false
	}
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(row.Value, &m))
	v, ok := m["content_url"].(string)
	return v, ok
}

// contentURLAudits returns the audit records of mutationType for the
// content URL key.
func contentURLAudits(t *testing.T, st store.Store, mutationType string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType})
	require.NoError(t, err)
	var out []*store.MutationAuditRecord
	for _, r := range recs {
		if r.TargetID == config.ContentURLKey {
			out = append(out, r)
		}
	}
	return out
}

// TestServerConfigContentURL_TokensRefused: a user access token cannot write
// server.hub.content_url, set or cleared; a body mixing an allowed key with
// it is refused whole and the allowed key is not stored; the endpoints
// section cannot be reset by a token. K8', K16', K17'.
func TestServerConfigContentURL_TokensRefused(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "cu-set-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_config:read", "hub_config:update")

	for _, body := range []string{
		`{"server":{"hub":{"content_url":"https://links.example.net"}}}`,
		`{"server":{"hub":{"content_url":""}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodPost} {
				rec := doRequestWithToken(t, srv, key, method, "/api/v1/admin/server-config", json.RawMessage(body))
				requireTokenRefusedKeys(t, rec, config.ContentURLKey)
			}
		})
	}

	t.Run("mixed body refused whole", func(t *testing.T) {
		before, beforeErr := s.GetHubSetting(context.Background(), "agent_defaults")
		rec := doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/server-config",
			json.RawMessage(`{"default_max_turns":9,"server":{"hub":{"content_url":"https://links.example.net"}}}`))
		requireTokenRefusedKeys(t, rec, config.ContentURLKey)
		after, afterErr := s.GetHubSetting(context.Background(), "agent_defaults")
		assert.Equal(t, beforeErr, afterErr, "the allowed section changed")
		if before != nil && after != nil {
			assert.JSONEq(t, string(before.Value), string(after.Value), "the allowed key was applied")
			assert.Equal(t, before.Revision, after.Revision)
		}
		_, err := s.GetHubSetting(context.Background(), "endpoints")
		assert.ErrorIs(t, err, store.ErrNotFound, "the endpoints section was written")
		// The same allowed key alone is still a token's to write.
		rec = doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/server-config", json.RawMessage(`{"default_max_turns":9}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("section reset refused", func(t *testing.T) {
		rec := doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/admin/server-config/sections/endpoints", nil)
		requireTokenRefusedKeys(t, rec, "endpoints")
	})

	assert.Equal(t, settingsTokenRefused, serverConfigKeyTokenClass(config.ContentURLKey))
	assert.False(t, serverConfigSectionTokenResettable("endpoints"))
}

// TestServerConfigContentURL_FileModeTokensRefused: on a hub with no
// settings database a user access token cannot write the key either. K8'.
func TestServerConfigContentURL_FileModeTokensRefused(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "cu-file-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_config:read", "hub_config:update")
	rec := doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/server-config",
		json.RawMessage(`{"server":{"hub":{"content_url":"https://links.example.net"}}}`))
	requireTokenRefusedKeys(t, rec, config.ContentURLKey)
}

// TestServerConfigContentURL_AdminSessionWrites: an interactive admin session
// writes content_url (stored normalized), which applies without a restart;
// a save that leaves it out keeps it; "" clears it. GET shows the value.
func TestServerConfigContentURL_AdminSessionWrites(t *testing.T) {
	srv, fake, ops, _, _ := contentURLSettingsFixture(t)

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"HTTPS://Links.Example.NET:443/"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, ok := storedContentURL(t, fake)
	require.True(t, ok)
	assert.Equal(t, loTestOrigin, got)
	assert.Equal(t, loTestOrigin, srv.HubEndpoints().Content, "applied without a restart")
	assert.Equal(t, loTestOrigin, ops.Snapshot().ContentURL)

	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"monitoring_dashboard_url":"https://dash.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ = storedContentURL(t, fake)
	assert.Equal(t, loTestOrigin, got, "an omitted key keeps its value")

	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":""}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	_, ok = storedContentURL(t, fake)
	assert.False(t, ok, "content_url not cleared")
	assert.Empty(t, srv.HubEndpoints().Content)
}

// TestServerConfigContentURL_Validation: an invalid value, or one on the
// hub's own host name at any scheme or port, is refused with 422 naming the
// key and nothing written or audited; a same-site value is saved. On a
// workstation hub on http://localhost:8080 the same host name under another
// scheme or port is refused and another loopback name is accepted.
// Condition 5, K10, K11, K18'.
func TestServerConfigContentURL_Validation(t *testing.T) {
	srv, fake, ops, st, _ := contentURLSettingsFixture(t)

	for _, v := range []string{
		"https://links.example.net/share",
		"http://links.example.net",
		"https://u@links.example.net",
		"https://links.example.net?x=1",
		loTestHubOrigin,
		"https://HUB.example.com:443",
		"https://hub.example.com:8443",
		"https://HUB.EXAMPLE.COM",
		"http://hub.example.com",
	} {
		rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+v+`"}}}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "%s: %s", v, rr.Body.String())
		assert.Contains(t, rr.Body.String(), config.ContentURLKey, v)
		_, ok := storedContentURL(t, fake)
		assert.False(t, ok, "%s: written", v)
	}
	assert.Empty(t, contentURLAudits(t, st, gcpIAMAuditMutation), "a refused save was audited")

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://links.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ := storedContentURL(t, fake)
	assert.Equal(t, "https://links.example.com", got)

	srv.config.HubEndpoints = &HubEndpoints{Operator: "http://localhost:8080", Public: "http://localhost:8080"}
	for _, v := range []string{"https://localhost:8080", "http://localhost:9000", "http://LOCALHOST:8080"} {
		rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+v+`"}}}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "%s: %s", v, rr.Body.String())
		got, _ := storedContentURL(t, fake)
		assert.Equal(t, "https://links.example.com", got, "%s: written", v)
	}
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"http://127.0.0.1:9000"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ = storedContentURL(t, fake)
	assert.Equal(t, "http://127.0.0.1:9000", got)

	// The agent endpoint's host name is refused too, at any scheme or port.
	srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
	srv.config.AgentEndpoint = "http://agents.example.net:9810"
	for _, v := range []string{"https://agents.example.net", "https://Agents.Example.NET:8443"} {
		rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+v+`"}}}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "%s: %s", v, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "agent_endpoint", v)
		got, _ := storedContentURL(t, fake)
		assert.Equal(t, "http://127.0.0.1:9000", got, "%s: written", v)
	}
}

// TestServerConfigContentURL_FileModeValidated: file mode validates the key
// the same way. K18'.
func TestServerConfigContentURL_FileModeValidated(t *testing.T) {
	srv, _, _ := fileModeGCPIdentityServer(t)
	srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
	for _, v := range []string{"https://links.example.net/x", loTestHubOrigin, "https://hub.example.com:8443"} {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"server":{"hub":{"content_url":"`+v+`"}}}`))
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "%s: %s", v, rr.Body.String())
		assert.Contains(t, rr.Body.String(), config.ContentURLKey, v)
	}
}

// TestServerConfigContentURL_Audited: each change of the value in effect
// writes one audit record with the old and new value and the actor; a save
// that changes nothing writes none; a reset records the change back to the
// bootstrap value. K9, K15'.
func TestServerConfigContentURL_Audited(t *testing.T) {
	srv, _, ops, st, _ := contentURLSettingsFixture(t)

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://links.example.net"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://other.example.net"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	// Unchanged, and a save of another endpoints key: no record.
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://other.example.net"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"monitoring_dashboard_url":"https://dash.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	type change struct{ surface, before, after string }
	collect := func() []change {
		var got []change
		for _, r := range contentURLAudits(t, st, gcpIAMAuditMutation) {
			assert.Equal(t, gcpIAMAuditTarget, r.TargetType)
			assert.Equal(t, gcpIAMSurfaceKind, r.ExecutorKind)
			assert.NotEmpty(t, r.ActorPrincipalID, "the actor is recorded")
			got = append(got, change{r.ExecutorID, r.BeforeSummary, r.AfterSummary})
		}
		return got
	}
	assert.ElementsMatch(t, []change{
		{gcpIAMSurfaceSave, "", "https://links.example.net"},
		{gcpIAMSurfaceSave, "https://links.example.net", "https://other.example.net"},
	}, collect())

	require.Equal(t, "https://other.example.net", srv.HubEndpoints().Content, "the save applies before the reset")
	require.Equal(t, http.StatusOK, resetEndpoints(t, srv).Code)
	assert.ElementsMatch(t, []change{
		{gcpIAMSurfaceSave, "", "https://links.example.net"},
		{gcpIAMSurfaceSave, "https://links.example.net", "https://other.example.net"},
		{gcpIAMSurfaceReset, "https://other.example.net", ""},
	}, collect())
	assert.Empty(t, contentURLAudits(t, st, gcpIAMNotAppliedMutation))
	assert.Empty(t, srv.HubEndpoints().Content, "the reset applies")
}

// TestServerConfigContentURL_AuditFailureBlocksChange: when the audit record
// cannot be written, the change is not made, on save and on reset.
func TestServerConfigContentURL_AuditFailureBlocksChange(t *testing.T) {
	srv, fake, ops, _, fault := contentURLSettingsFixture(t)
	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://links.example.net"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	fault.Arm()

	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://other.example.net"}}}`)
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	got, _ := storedContentURL(t, fake)
	assert.Equal(t, loTestOrigin, got, "the save was made without its audit record")

	rr = resetEndpoints(t, srv)
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	got, ok := storedContentURL(t, fake)
	assert.True(t, ok, "the reset was made without its audit record")
	assert.Equal(t, loTestOrigin, got)
	assert.Equal(t, loTestOrigin, srv.HubEndpoints().Content)
}

// TestServerConfigContentURL_FileModeAudited: a settings-file save that
// changes the content host in effect is recorded before the write, with
// the previous and new value and the actor. A clear through an explicit
// null or a "hub": null is recorded too, and a null when the value is
// already "" records nothing. SEC-A1, SEC-A2, K61, K63.
func TestServerConfigContentURL_FileModeAudited(t *testing.T) {
	srv, st, settingsPath := fileModeGCPIdentityServer(t)
	srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
	put := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		require.Equal(t, http.StatusOK, rr.Code, "%s: %s", body, rr.Body.String())
	}
	type change struct{ before, after string }
	records := func() []change {
		var out []change
		for _, r := range contentURLAudits(t, st, gcpIAMAuditMutation) {
			assert.Equal(t, gcpIAMSurfaceSave, r.ExecutorID)
			assert.NotEmpty(t, r.ActorPrincipalID, "the actor is recorded")
			out = append(out, change{r.BeforeSummary, r.AfterSummary})
		}
		return out
	}

	// A no-op null: nothing set, nothing recorded.
	put(`{"server":{"hub":{"content_url":null}}}`)
	assert.Empty(t, records(), "a null with no value set was recorded")

	put(`{"server":{"hub":{"content_url":"HTTPS://Links.Example.NET:443/"}}}`)
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), loTestOrigin, "the save was written")
	assert.Equal(t, []change{{"", loTestOrigin}}, records())

	t.Run("null clear", func(t *testing.T) {
		setContentURLForTest(srv, loTestOrigin) // as the reload after the save applies it
		put(`{"server":{"hub":{"content_url":null}}}`)
		assert.Contains(t, records(), change{loTestOrigin, ""}, "a null clear was not recorded")
	})
	t.Run("hub null clear", func(t *testing.T) {
		put(`{"server":{"hub":{"content_url":"https://other.example.net"}}}`)
		setContentURLForTest(srv, "https://other.example.net")
		put(`{"server":{"hub":null}}`)
		assert.Contains(t, records(), change{"https://other.example.net", ""}, "a hub null clear was not recorded")
	})
}

// TestServerConfigContentURL_FileModeAuditFailureBlocksWrite: when the audit
// record of a file-mode change cannot be written, settings.yaml and the
// value in effect stay as they were. SEC-A1 (round-2 P3).
func TestServerConfigContentURL_FileModeAuditFailureBlocksWrite(t *testing.T) {
	srv, _, settingsPath := fileModeGCPIdentityServer(t)
	srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *publicURLFailingAuditStore {
		return &publicURLFailingAuditStore{Store: inner, fault: f}
	})
	before, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	fault.Arm()
	rr := httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"server":{"hub":{"content_url":"https://links.example.net"}}}`))
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	after, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "settings.yaml was written without its audit record")
	assert.Empty(t, srv.HubEndpoints().Content, "the value in effect changed")
}

// TestServerConfigContentURL_SameSaveAgentEndpoint: a content_url on the
// host name of the agent endpoint a save sends, or of the running agent
// endpoint while the save sends another one, is refused in file mode and on
// the database path (a workstation writes agent_endpoint to settings.yaml).
// Round-2 P2 and P4, sec S2; K62, K64, K68.
func TestServerConfigContentURL_SameSaveAgentEndpoint(t *testing.T) {
	const refusal = "must not be on the host name of server.hub.agent_endpoint"
	sameHost := `{"server":{"hub":{"agent_endpoint":"http://agents.example.net:9810","content_url":"https://agents.example.net"}}}`
	runningHost := `{"server":{"hub":{"agent_endpoint":"http://other-agents.example.net:9810","content_url":"https://agents.example.net"}}}`
	fileMode := func(t *testing.T, running, body string) {
		t.Helper()
		srv, _, _ := fileModeGCPIdentityServer(t)
		srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
		srv.config.AgentEndpoint = running
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), refusal)
	}
	database := func(t *testing.T, running, body string) {
		t.Helper()
		srv, fake, ops, _, _ := contentURLSettingsFixture(t)
		srv.config.AgentEndpoint = running
		rr := putServerConfigDB(t, srv, ops, body)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), refusal)
		_, ok := storedContentURL(t, fake)
		assert.False(t, ok, "written")
	}
	t.Run("sent agent endpoint, file mode", func(t *testing.T) { fileMode(t, "", sameHost) })
	t.Run("sent agent endpoint, database", func(t *testing.T) { database(t, "", sameHost) })
	t.Run("running agent endpoint, file mode", func(t *testing.T) { fileMode(t, "http://agents.example.net:9810", runningHost) })
	t.Run("running agent endpoint, database", func(t *testing.T) { database(t, "http://agents.example.net:9810", runningHost) })
}

// TestServerConfigContentURL_FileModeEnvValueNotAudited: on a file-mode hub
// whose content_url comes only from the environment (not the file), a save
// of an unrelated key writes no content_url record and succeeds even with
// the audit store faulted (sec S1; K67).
func TestServerConfigContentURL_FileModeEnvValueNotAudited(t *testing.T) {
	srv, st, _ := fileModeGCPIdentityServer(t)
	srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
	srv.config.ContentURL = "https://env.example.net" // as SCION_SERVER_HUB_CONTENTURL sets it
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *publicURLFailingAuditStore {
		return &publicURLFailingAuditStore{Store: inner, fault: f}
	})
	fault.Arm()
	rr := httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"default_timezone":"Europe/Paris"}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Empty(t, contentURLAudits(t, st, gcpIAMAuditMutation), "an unrelated save wrote a content_url record")
}
