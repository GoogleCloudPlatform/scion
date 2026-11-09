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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact"
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

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://links.example.org"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ := storedContentURL(t, fake)
	assert.Equal(t, "https://links.example.org", got)

	srv.config.HubEndpoints = &HubEndpoints{Operator: "http://localhost:8080", Public: "http://localhost:8080"}
	for _, v := range []string{"https://localhost:8080", "http://localhost:9000", "http://LOCALHOST:8080"} {
		rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+v+`"}}}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "%s: %s", v, rr.Body.String())
		got, _ := storedContentURL(t, fake)
		assert.Equal(t, "https://links.example.org", got, "%s: written", v)
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
	for _, v := range []string{"https://links.example.net/x", loTestHubOrigin, "https://hub.example.com:8443", "https://links.example.com"} {
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

// TestServerConfigContentURL_FileModeAuditRedactsStoredValue: a content_url
// stored in settings.yaml by hand, never validated, reaches neither the
// audit record nor the audit's log line raw when a save replaces it. A
// value that parses is recorded as its scheme and host only (no user info,
// path or query); a value that does not parse is recorded as urlredact.Mask.
func TestServerConfigContentURL_FileModeAuditRedactsStoredValue(t *testing.T) {
	const secret, token, pathToken = "s3cr3t-tok", "q-tok-123", "t-abc123"
	for _, tc := range []struct {
		name, stored, wantBefore string
	}{
		{"parses", "https://ops:" + secret + "@links.example.net/?token=" + token, "https://links.example.net"},
		{"token in path", "https://ops:" + secret + "@links.example.net/" + pathToken + "?token=" + token, "https://links.example.net"},
		{"does not parse", "https://ops:" + secret + "@links example.net/" + pathToken + "?token=" + token, urlredact.Mask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, settingsPath := fileModeGCPIdentityServer(t)
			srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
			require.NoError(t, os.WriteFile(settingsPath, []byte(
				"schema_version: \"1\"\ndefault_timezone: UTC\nserver:\n  hub:\n    port: 9810\n    content_url: \""+tc.stored+"\"\n"), 0o644))
			logs := captureSlogDefault(t)

			rr := httptest.NewRecorder()
			srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
				`{"server":{"hub":{"content_url":"https://other.example.net"}}}`))
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

			recs := contentURLAudits(t, st, gcpIAMAuditMutation)
			require.Len(t, recs, 1)
			assert.Equal(t, tc.wantBefore, recs[0].BeforeSummary)
			assert.Equal(t, "https://other.example.net", recs[0].AfterSummary)
			var auditLine string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "Hub endpoint setting changed") {
					auditLine = line
					break
				}
			}
			require.NotEmpty(t, auditLine, "the audit's log line")
			assert.Contains(t, auditLine, "old="+tc.wantBefore, "the audit's log line shows the summarized value")
			whole, err := json.Marshal(recs[0])
			require.NoError(t, err)
			for _, raw := range []string{secret, token, pathToken, tc.stored} {
				assert.NotContains(t, string(whole), raw, "the audit record holds the stored value raw")
				assert.NotContains(t, logs.String(), raw, "the log holds the stored value raw")
			}
		})
	}
}

// TestContentURLForAudit: the audit summary of a content URL value is its
// scheme, host and port for an http or https URL with a host, "" for an
// empty value, and urlredact.Mask for anything else; no user info, query,
// path or other raw text of the value survives.
func TestContentURLForAudit(t *testing.T) {
	const secret, token, pathToken = "s3cr3t-tok", "q-tok-123", "t-abc123"
	for _, tc := range []struct{ name, raw, want string }{
		{"empty", "", ""},
		{"origin", "https://links.example.net", "https://links.example.net"},
		{"port", "https://links.example.net:8443", "https://links.example.net:8443"},
		{"loopback http", "http://localhost:9810", "http://localhost:9810"},
		{"IPv6 literal", "https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"user info and query", "https://ops:" + secret + "@links.example.net/?token=" + token, "https://links.example.net"},
		{"token in path", "https://links.example.net/" + pathToken, "https://links.example.net"},
		{"scheme-less", "links.example.net/" + pathToken, urlredact.Mask},
		{"opaque", "https:links.example.net/" + pathToken, urlredact.Mask},
		{"word:rest", "token:" + secret, urlredact.Mask},
		{"upper-case scheme", "HTTPS://Host.Example:8443/p?q", "https://Host.Example:8443"},
		{"other scheme", "ftp://host.example", urlredact.Mask},
		{"other scheme with path", "ftp://links.example.net/" + pathToken, urlredact.Mask},
		{"empty host", "https:///" + pathToken, urlredact.Mask},
		{"protocol-relative", "//links.example.net/" + pathToken, urlredact.Mask},
		{"at sign after the path", "https://links.example.net/" + pathToken + "@" + secret, urlredact.Mask},
		{"does not parse", "https://ops:" + secret + "@links example.net/?token=" + token, urlredact.Mask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := contentURLForAudit(tc.raw)
			assert.Equal(t, tc.want, got)
			for _, raw := range []string{secret, token, pathToken} {
				assert.NotContains(t, got, raw)
			}
		})
	}
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

// TestServerConfigContentURL_SameSite: on save, a content URL on the hub's
// registrable domain is refused unless server.hub.content_url_allow_same_site
// is in effect after the save (sent with it, or already set). With the
// override it is stored, applied and logs the reworded warning, and the
// override's own change is audited. A value under a public suffix shared
// with the hub (a.github.io / b.github.io) is a different site, accepted
// with no override; a value on the hub's own host name stays refused with
// the override (ptone/scion#7107 phase B item 1, tests a-d on save).
func TestServerConfigContentURL_SameSite(t *testing.T) {
	const sameSite = "https://links.example.com"
	srv, fake, ops, st, _ := contentURLSettingsFixture(t)
	logs := captureContentHostLogs(t)

	// (a) Refused without the override, in DB mode and in file mode.
	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+sameSite+`"}}}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), config.ContentURLKey)
	assert.Contains(t, rr.Body.String(), config.ContentURLAllowSameSiteKey)
	_, ok := storedContentURL(t, fake)
	assert.False(t, ok, "a refused value was written")
	assert.Empty(t, srv.HubEndpoints().Content)

	// (d) The hub's own host name stays refused even with the override.
	for _, v := range []string{loTestHubOrigin, "https://hub.example.com:8443"} {
		rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+v+`","content_url_allow_same_site":true}}}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "%s: %s", v, rr.Body.String())
		_, ok = storedContentURL(t, fake)
		assert.False(t, ok, "%s: written", v)
	}

	// (b) Accepted with the override sent in the same save: stored,
	// applied, warned about, and the override change audited.
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+sameSite+`","content_url_allow_same_site":true}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ := storedContentURL(t, fake)
	assert.Equal(t, sameSite, got)
	assert.Equal(t, sameSite, srv.HubEndpoints().Content)
	out := logs()
	assert.Contains(t, out, "is not a separate security origin for cookies")
	assert.Contains(t, out, "(accepted because "+config.ContentURLAllowSameSiteKey+" is set)")

	recs, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: gcpIAMAuditMutation})
	require.NoError(t, err)
	var overrideChanges [][2]string
	for _, r := range recs {
		if r.TargetID == config.ContentURLAllowSameSiteKey {
			assert.NotEmpty(t, r.ActorPrincipalID, "the actor is recorded")
			overrideChanges = append(overrideChanges, [2]string{r.BeforeSummary, r.AfterSummary})
		}
	}
	assert.Equal(t, [][2]string{{"false", "true"}}, overrideChanges)

	// With the override already in effect, a later save of another
	// same-site value needs no override in the request.
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://share.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "https://share.example.com", srv.HubEndpoints().Content)

	// Turning the override off leaves the stored value ignored at runtime.
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url_allow_same_site":false}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Empty(t, srv.HubEndpoints().Content, "a same-site value was used without the override")

	// (c) Public suffix tenants are different sites: no override needed.
	srv.config.HubEndpoints = &HubEndpoints{Operator: "https://a.github.io", Public: "https://a.github.io"}
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://b.github.io"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "https://b.github.io", srv.HubEndpoints().Content)
}

// TestServerConfigContentURL_SameSiteTokensRefused: a user access token
// cannot write server.hub.content_url_allow_same_site.
func TestServerConfigContentURL_SameSiteTokensRefused(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "cu-ss-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_config:read", "hub_config:update")
	for _, body := range []string{
		`{"server":{"hub":{"content_url_allow_same_site":true}}}`,
		`{"server":{"hub":{"content_url_allow_same_site":false}}}`,
	} {
		rec := doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/server-config", json.RawMessage(body))
		requireTokenRefusedKeys(t, rec, config.ContentURLAllowSameSiteKey)
	}
}

// TestServerConfigContentURL_AllowSameSiteEchoNotPinned: GET always shows
// content_url_allow_same_site (false while the stored row leaves it unset).
// A save that sends that value back is an echo, so the key is neither
// written into the row nor reported nor audited; an explicit change is
// still stored (ptone/scion#7107 PB1 F2).
func TestServerConfigContentURL_AllowSameSiteEchoNotPinned(t *testing.T) {
	srv, fake, ops, st, _ := contentURLSettingsFixture(t)
	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://links.example.net"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	get := getServerConfigDB(t, srv, ops)
	require.NotNil(t, get.Server.Hub.ContentURLAllowSameSite)
	require.False(t, *get.Server.Hub.ContentURLAllowSameSite)

	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"admin_emails":["new@example.com"],"content_url":"https://links.example.net","content_url_allow_same_site":false}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, _ := rowDoc(t, fake, "endpoints")
	assert.NotContains(t, doc, "content_url_allow_same_site", "the echoed override was pinned into the row")
	applied, _ := savedKeys(t, rr.Body.Bytes())
	assert.NotContains(t, applied, config.ContentURLAllowSameSiteKey)
	recs, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{})
	require.NoError(t, err)
	for _, r := range recs {
		assert.NotEqual(t, config.ContentURLAllowSameSiteKey, r.TargetID, "an echo was audited")
	}

	// An explicit change is stored.
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url_allow_same_site":true}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, _ = rowDoc(t, fake, "endpoints")
	assert.Equal(t, true, doc["content_url_allow_same_site"])
}

// failOneKeyAuditStore fails the change record of one key and passes every
// other audit write, so a write that records two changes fails on the
// second.
type failOneKeyAuditStore struct {
	store.Store
	key string
}

func (f *failOneKeyAuditStore) CreateMutationAudit(ctx context.Context, rec *store.MutationAuditRecord) error {
	if rec.TargetID == f.key && rec.MutationType == gcpIAMAuditMutation {
		return errors.New("injected audit failure")
	}
	return f.Store.CreateMutationAudit(ctx, rec)
}

// TestServerConfigContentURL_SecondAuditFailureMarksFirstNotApplied: when
// one write changes content_url and its same-site override and the
// override's record cannot be written, nothing is written and the
// content_url change already recorded gets a not-applied record, on a DB
// save, a section reset and a file-mode save (ptone/scion#7107 sec S1).
func TestServerConfigContentURL_SecondAuditFailureMarksFirstNotApplied(t *testing.T) {
	const sameSite = "https://links.example.com"
	failOverride := func(t *testing.T, srv *Server) {
		t.Helper()
		installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *failOneKeyAuditStore {
			return &failOneKeyAuditStore{Store: inner, key: config.ContentURLAllowSameSiteKey}
		})
	}
	notApplied := func(t *testing.T, st store.Store) [][2]string {
		t.Helper()
		var out [][2]string
		for _, r := range contentURLAudits(t, st, gcpIAMNotAppliedMutation) {
			out = append(out, [2]string{r.BeforeSummary, r.AfterSummary})
		}
		return out
	}

	t.Run("db save", func(t *testing.T) {
		srv, fake, ops, st, _ := contentURLSettingsFixture(t)
		failOverride(t, srv)
		rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+sameSite+`","content_url_allow_same_site":true}}}`)
		require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
		_, ok := storedContentURL(t, fake)
		assert.False(t, ok, "written without the override's record")
		assert.Equal(t, [][2]string{{"", sameSite}}, notApplied(t, st))
	})

	t.Run("section reset", func(t *testing.T) {
		srv, fake, ops, st, _ := contentURLSettingsFixture(t)
		rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+sameSite+`","content_url_allow_same_site":true}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		failOverride(t, srv)
		rr = resetEndpoints(t, srv)
		require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
		got, _ := storedContentURL(t, fake)
		assert.Equal(t, sameSite, got, "reset without the override's record")
		assert.Equal(t, [][2]string{{sameSite, ""}}, notApplied(t, st))
	})

	t.Run("file mode save", func(t *testing.T) {
		srv, st, settingsPath := fileModeGCPIdentityServer(t)
		srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
		before, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		failOverride(t, srv)
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
			`{"server":{"hub":{"content_url":"`+sameSite+`","content_url_allow_same_site":true}}}`))
		require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
		after, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "settings.yaml written without the override's record")
		assert.Equal(t, [][2]string{{"", sameSite}}, notApplied(t, st))
	})
}

// TestServerConfigContentURL_AgentAndBrokerRefused: an agent or broker
// credential cannot write server.hub.content_url or its same-site override,
// by a DB save, a reset of the endpoints section or a file-mode save; the
// request is refused naming the key (or the section), nothing is written
// and nothing is audited (ptone/scion#7107 PB1 F5a).
func TestServerConfigContentURL_AgentAndBrokerRefused(t *testing.T) {
	creds := []struct {
		name     string
		identity Identity
		kind     CredentialKind
	}{
		{"agent", &agentIdentityWrapper{&AgentTokenClaims{ProjectID: "p1"}}, CredentialKindAgentJWT},
		{"broker", NewBrokerIdentity(tid("b1")), CredentialKindBroker},
	}
	bodies := []struct{ key, body string }{
		{config.ContentURLKey, `{"server":{"hub":{"content_url":"https://links.example.net"}}}`},
		{config.ContentURLAllowSameSiteKey, `{"server":{"hub":{"content_url_allow_same_site":true}}}`},
	}
	for _, c := range creds {
		ctx := contextWithCredentialContext(contextWithIdentity(context.Background(), c.identity), CredentialContext{Kind: c.kind})
		for _, b := range bodies {
			t.Run(c.name+" db save "+b.key, func(t *testing.T) {
				srv, fake, ops, st, _ := contentURLSettingsFixture(t)
				rr := httptest.NewRecorder()
				srv.handlePutServerConfigDB(rr, httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", strings.NewReader(b.body)).WithContext(ctx), ops)
				requireTokenRefusedKeys(t, rr, b.key)
				_, err := fake.GetHubSetting(context.Background(), "endpoints")
				assert.ErrorIs(t, err, store.ErrNotFound, "the endpoints section was written")
				recs, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{})
				require.NoError(t, err)
				assert.Empty(t, recs, "a refused save was audited")
			})
			t.Run(c.name+" file mode "+b.key, func(t *testing.T) {
				srv, st, settingsPath := fileModeGCPIdentityServer(t)
				srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
				before, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				rr := httptest.NewRecorder()
				srv.handlePutServerConfig(rr, httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", strings.NewReader(b.body)).WithContext(ctx))
				requireTokenRefusedKeys(t, rr, b.key)
				after, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "settings.yaml was written")
				recs, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{})
				require.NoError(t, err)
				assert.Empty(t, recs, "a refused save was audited")
			})
		}
		t.Run(c.name+" section reset", func(t *testing.T) {
			srv, fake, ops, st, _ := contentURLSettingsFixture(t)
			rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://links.example.com","content_url_allow_same_site":true}}}`)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			auditsBefore, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{})
			require.NoError(t, err)
			rr = httptest.NewRecorder()
			srv.handleAdminServerConfigSectionReset(rr, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/endpoints", nil).WithContext(ctx))
			requireTokenRefusedKeys(t, rr, "endpoints")
			doc, _ := rowDoc(t, fake, "endpoints")
			assert.Equal(t, "https://links.example.com", doc["content_url"], "the reset was made")
			assert.Equal(t, true, doc["content_url_allow_same_site"], "the reset was made")
			auditsAfter, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{})
			require.NoError(t, err)
			assert.Len(t, auditsAfter, len(auditsBefore), "a refused reset was audited")
		})
	}
}

// TestServerConfigContentURL_TokenResetKeepsSection: a user access token
// cannot reset the endpoints section, so a stored content_url and override
// stay stored and in effect, and no reset is audited (ptone/scion#7107 PB1
// F5b).
func TestServerConfigContentURL_TokenResetKeepsSection(t *testing.T) {
	const sameSite = "https://links.example.com"
	srv, fake, ops, st, _ := contentURLSettingsFixture(t)
	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"`+sameSite+`","content_url_allow_same_site":true}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", store.UserRoleAdmin, "cli")
	ctx := contextWithCredentialContext(contextWithIdentity(context.Background(), admin), CredentialContext{Kind: CredentialKindUAT})
	rr = httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/endpoints", nil).WithContext(ctx))
	requireTokenRefusedKeys(t, rr, "endpoints")

	doc, _ := rowDoc(t, fake, "endpoints")
	assert.Equal(t, sameSite, doc["content_url"])
	assert.Equal(t, true, doc["content_url_allow_same_site"])
	assert.Equal(t, sameSite, srv.HubEndpoints().Content, "the content host in effect changed")
	recs := contentURLAudits(t, st, gcpIAMAuditMutation)
	require.Len(t, recs, 1, "a refused reset was audited")
	assert.Equal(t, gcpIAMSurfaceSave, recs[0].ExecutorID, "a refused reset was audited")
}

// TestServerConfigContentURL_FileModeAcceptsWithOverride: in file mode a
// same-site content_url sent with the override is written to settings.yaml
// and both changes are audited (ptone/scion#7107 PB1 F5c).
func TestServerConfigContentURL_FileModeAcceptsWithOverride(t *testing.T) {
	const sameSite = "https://links.example.com"
	srv, st, settingsPath := fileModeGCPIdentityServer(t)
	srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
	rr := httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"hub":{"content_url":"`+sameSite+`","content_url_allow_same_site":true}}}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	raw := readSettingsYAML(t, settingsPath)
	hub, _ := raw["server"].(map[string]interface{})["hub"].(map[string]interface{})
	assert.Equal(t, sameSite, hub["content_url"])
	assert.Equal(t, true, hub["content_url_allow_same_site"])

	recs, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: gcpIAMAuditMutation})
	require.NoError(t, err)
	got := map[string][2]string{}
	for _, r := range recs {
		assert.Equal(t, gcpIAMSurfaceSave, r.ExecutorID)
		assert.NotEmpty(t, r.ActorPrincipalID, "the actor is recorded")
		got[r.TargetID] = [2]string{r.BeforeSummary, r.AfterSummary}
	}
	assert.Equal(t, map[string][2]string{
		config.ContentURLKey:              {"", sameSite},
		config.ContentURLAllowSameSiteKey: {"false", "true"},
	}, got)
	assert.Empty(t, contentURLAudits(t, st, gcpIAMNotAppliedMutation))
}

// TestServerConfigContentURL_Status: GET reports content_url_status for the
// value as applied, naming the override key when a same-site value waits
// for it; a PUT body that sends the status back has it dropped, so an
// echoed (or stale) status neither fails the save nor is stored
// (ptone/scion#7107 PB1 F1).
func TestServerConfigContentURL_Status(t *testing.T) {
	srv, fake, ops, _, _ := contentURLSettingsFixture(t)
	status := func() ContentURLStatus {
		t.Helper()
		resp := getServerConfigDB(t, srv, ops)
		require.NotNil(t, resp.ContentURLStatus)
		return *resp.ContentURLStatus
	}
	assert.Equal(t, ContentURLStatus{State: ContentURLStateUnset}, status())

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://links.example.net"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, ContentURLStatus{State: ContentURLStateInEffect, Effective: "https://links.example.net"}, status())

	// A stale status sent back is dropped: the save succeeds, the status is
	// not stored, and GET still reports the state as applied.
	rr = putServerConfigDB(t, srv, ops, `{"content_url_status":{"state":"unset"},"server":{"hub":{"monitoring_dashboard_url":"https://dash.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, _ := rowDoc(t, fake, "endpoints")
	assert.NotContains(t, doc, "content_url_status")
	assert.Equal(t, ContentURLStateInEffect, status().State)

	// A same-site value with the override, then without it.
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url":"https://links.example.com","content_url_allow_same_site":true}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, ContentURLStatus{State: ContentURLStateInEffect, Effective: "https://links.example.com"}, status())
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"content_url_allow_same_site":false}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, ContentURLStatus{State: ContentURLStateSameSite, OverrideKey: config.ContentURLAllowSameSiteKey}, status())

	// The other states, as applied (saves refuse these values).
	for value, want := range map[string]string{
		"https://hub.example.com:8443": ContentURLStateSameHost,
		"ftp://links.example.net":      ContentURLStateInvalid,
		"https://agents.example.net":   ContentURLStateAgentEndpointHost,
	} {
		srv.config.AgentEndpoint = "https://agents.example.net"
		setContentURLForTest(srv, value)
		assert.Equal(t, ContentURLStatus{State: want}, status(), value)
	}
}

// TestWarnContentURLNotInEffect: at startup, a stored same-site value
// without the override logs the one ERROR line naming the override key, and
// a value in effect logs nothing (ptone/scion#7107 PB1 F1).
func TestWarnContentURLNotInEffect(t *testing.T) {
	t.Run("same site without override", func(t *testing.T) {
		logs := captureContentHostLogs(t)
		srv := &Server{maintenance: NewMaintenanceState(false, "")}
		srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
		ApplySnapshot(srv, Layer1Snapshot{ContentURL: "https://links.example.com"})
		srv.WarnContentURLNotInEffect()
		var lines []string
		for _, l := range strings.Split(logs(), "\n") {
			if strings.Contains(l, config.ContentURLKey+" is ignored") {
				lines = append(lines, l)
			}
		}
		require.Len(t, lines, 1, "the warning is one line, logged once")
		assert.Contains(t, lines[0], "level=ERROR")
		assert.Contains(t, lines[0], config.ContentURLAllowSameSiteKey)
	})
	t.Run("in effect", func(t *testing.T) {
		logs := captureContentHostLogs(t)
		srv := &Server{maintenance: NewMaintenanceState(false, "")}
		srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
		ApplySnapshot(srv, Layer1Snapshot{ContentURL: "https://links.example.net"})
		srv.WarnContentURLNotInEffect()
		assert.NotContains(t, logs(), config.ContentURLKey)
	})
}
