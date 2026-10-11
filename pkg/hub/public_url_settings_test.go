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

//go:build !no_sqlite && (!hubshard || hubshard_5)

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func storedEndpointsPublicURL(t *testing.T, fake *fakeHubSettingStore) (string, bool) {
	t.Helper()
	row, err := fake.GetHubSetting(context.Background(), "endpoints")
	if err != nil {
		return "", false
	}
	return publicURLFromSectionDoc(row.Value), true
}

// TestPutServerConfigDB_PublicURLRejected checks each rejected form of a
// public URL saved in the settings database: 422 validation_error naming the
// key, the value not echoed, and nothing written (ptone/scion#3905).
func TestPutServerConfigDB_PublicURLRejected(t *testing.T) {
	for name, value := range map[string]string{
		"relative":          "hub.example.com",
		"no host":           "https://",
		"other scheme":      "ftp://hub.example.com",
		"http non-loopback": "http://hub.example.com",
		"credentials":       "https://u:p@hub.example.com",
		"fragment":          "https://hub.example.com/#x",
		"query":             "https://hub.example.com/?a=b",
		"path":              "https://hub.example.com/hub",
		"port out of range": "https://hub.example.com:99999",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			body, err := json.Marshal(map[string]any{"server": map[string]any{"hub": map[string]any{"public_url": value}}})
			require.NoError(t, err)
			rr := putServerConfigDB(t, srv, ops, string(body))
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())

			var env struct {
				Error struct {
					Code    string         `json:"code"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env), rr.Body.String())
			assert.Equal(t, ErrCodeValidationError, env.Error.Code)
			assert.Equal(t, config.PublicURLKey, env.Error.Details["field"])
			assert.NotContains(t, env.Error.Message, value, "the message does not echo the value")

			_, ok := storedEndpointsPublicURL(t, fake)
			assert.False(t, ok, "nothing is written")
		})
	}
}

// TestPutServerConfigDB_PublicURLNormalized checks that an accepted value is
// stored without its trailing slash, and that the save reports the key as
// pending a restart.
func TestPutServerConfigDB_PublicURLNormalized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"HTTPS://hub.example.com/"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, ok := storedEndpointsPublicURL(t, fake)
	require.True(t, ok)
	assert.Equal(t, "https://hub.example.com", got)
	assert.Contains(t, rr.Body.String(), `"pending_restart":["server.hub.public_url"]`)
}

// TestPutServerConfigDB_PublicURLEchoNotRechecked checks that sending back
// the value the GET view shows (here a settings.yaml value the stricter
// database rule would reject) does not fail a save of another field.
func TestPutServerConfigDB_PublicURLEchoNotRechecked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := newFakeHubSettingStore()
	fileK := newFileKoanf(t, map[string]any{config.PublicURLKey: "http://hub.internal:8080"})
	ops := NewOperationalSettings(fake, fileK, emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"http://hub.internal:8080","monitoring_dashboard_url":"https://dash.example.com/d"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}

// TestPublicURLWrites_Audited checks that a change to the stored public URL,
// by a save or by a section reset, is recorded through the hub settings
// audit path with the actor and the old and new values.
func TestPublicURLWrites_Audited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _, ops := newTestDBServer(t)
	st := newGCPIdentitySettingsStore(t)
	srv.store = st

	audits := func() []*store.MutationAuditRecord {
		recs, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: gcpIAMAuditMutation})
		require.NoError(t, err)
		var out []*store.MutationAuditRecord
		for _, r := range recs {
			if r.TargetID == config.PublicURLKey {
				out = append(out, r)
			}
		}
		return out
	}

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://one.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://two.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	// An unchanged value records nothing.
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://two.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	reset := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(reset,
		adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/endpoints", ""))
	require.Equal(t, http.StatusOK, reset.Code, reset.Body.String())

	recs := audits()
	require.Len(t, recs, 3, "two saves and a reset")
	pairs := map[string]string{}
	for _, r := range recs {
		assert.Equal(t, gcpIAMAuditTarget, r.TargetType)
		assert.NotEmpty(t, r.ActorPrincipalID, "the actor is recorded")
		pairs[r.BeforeSummary] = r.AfterSummary
	}
	assert.Equal(t, map[string]string{
		"":                        "https://one.example.com",
		"https://one.example.com": "https://two.example.com",
		"https://two.example.com": "",
	}, pairs)
}
