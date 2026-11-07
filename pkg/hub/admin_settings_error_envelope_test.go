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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// One error envelope for the settings write APIs (ptone/scion#4450): a
// rejected value is 422 validation_error in the writeError envelope, every
// details.errors entry names the koanf path of its field, all failures are
// reported at once, a stale revision is 409 revision_conflict naming the
// expected and current revision of each section, and an unreadable body
// stays 400 invalid_request.

func entryFields(entries []settingsFieldError) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Field)
	}
	sort.Strings(out)
	return out
}

func assertFields(t *testing.T, entries []settingsFieldError, want ...string) {
	t.Helper()
	got := entryFields(entries)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("entry fields = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry fields = %v, want %v", got, want)
		}
	}
}

func assertValidation422(t *testing.T, rr *httptest.ResponseRecorder) []settingsFieldError {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	if code := settingsErrorFields(t, rr.Body.Bytes())["error"]; code != ErrCodeValidationError {
		t.Fatalf("code = %v, want %s: %s", code, ErrCodeValidationError, rr.Body.String())
	}
	return settingsErrorEntries(t, rr)
}

func TestSettingsEnvelope_DBSchemaErrorsAcrossSections(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops,
		`{"server":{"auth":{"default_user_role":"superuser"},"hub":{"hub_name":"Bad.Name"}}}`)
	entries := assertValidation422(t, rr)
	assertFields(t, entries, "server.auth.default_user_role", "server.hub.hub_name")
	for _, e := range entries {
		if e.Section == "" {
			t.Errorf("schema entry without a section: %+v", e)
		}
	}
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if len(fakeStore.settings) != 0 {
		t.Errorf("a refused save wrote sections: %v", fakeStore.settings)
	}
}

func TestSettingsEnvelope_DBDurationsAllReported(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops,
		`{"server":{"hub":{"stalled_threshold":"1hour","soft_delete_retention":"x"}}}`)
	entries := assertValidation422(t, rr)
	assertFields(t, entries, "server.hub.stalled_threshold", "server.hub.soft_delete_retention")
}

func TestSettingsEnvelope_DBFederationAllFailuresReported(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"federation":{"enabled":true,"algorithms":["HS256"],"trusted_issuers":[`+
		`{"issuer_url":"ftp://a.example","issuer_type":"hub"},`+
		`{"issuer_url":"https://b.example","issuer_type":"nope"}]}}`)
	entries := assertValidation422(t, rr)
	assertFields(t, entries,
		"server.federation.algorithms",
		"server.federation.trusted_issuers[0].issuer_url",
		"server.federation.trusted_issuers[1].issuer_type")
	for _, e := range entries {
		if e.Section != "federation" {
			t.Errorf("federation entry section = %q: %+v", e.Section, e)
		}
	}
}

func TestSettingsEnvelope_DBGCPIAMAllKeysReported(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops,
		`{"server":{"hub":{"gcp_iam_check_mode":"bogus","gcp_iam_deny_unknown_policy":"nope"}}}`)
	entries := assertValidation422(t, rr)
	assertFields(t, entries, gcpIAMCheckModeKey, gcpIAMDenyUnknownKey)
}

func TestSettingsEnvelope_DBKeysRejectedNested(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"server":{"database":{"driver":"postgres"}}}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	fields := settingsErrorFields(t, rr.Body.Bytes())
	if fields["error"] != "layer0_rejected" {
		t.Errorf("code = %v, want layer0_rejected", fields["error"])
	}
	if keys, _ := fields["keys"].([]interface{}); len(keys) == 0 {
		t.Errorf("details.keys missing: %s", rr.Body.String())
	}
	settingsErrorEntries(t, rr)
}

func TestSettingsEnvelope_DBRevisionConflict(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["a@example.com"]}`))
	_, _ = ops.Refresh(context.Background())
	rr := putServerConfigDB(t, srv, ops,
		`{"server":{"hub":{"admin_emails":["b@example.com"]}},"expected_revisions":{"access":7}}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	fields := settingsErrorFields(t, rr.Body.Bytes())
	if fields["error"] != ErrCodeRevisionConflict {
		t.Errorf("code = %v", fields["error"])
	}
	conflicted, _ := fields["conflicted"].([]interface{})
	if len(conflicted) != 1 {
		t.Fatalf("conflicted = %v", fields["conflicted"])
	}
	c := conflicted[0].(map[string]interface{})
	if c["section"] != "access" || c["expected_revision"] != float64(7) || c["current_revision"] != float64(1) {
		t.Errorf("conflicted[0] = %v, want access expected 7 current 1", c)
	}
}

func TestSettingsEnvelope_DBMalformedBodyStays400(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"server":`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if code := settingsErrorFields(t, rr.Body.Bytes())["error"]; code != ErrCodeInvalidRequest {
		t.Errorf("code = %v, want %s", code, ErrCodeInvalidRequest)
	}
}

func TestSettingsEnvelope_FileTierAllEntries(t *testing.T) {
	rr, _ := fileModePutServerConfig(t, &Server{},
		`{"runtimes":{"gke":{"type":"kubernetes","shared_dir_size":"1TB"}},"profiles":{"big":{"runtime":"gke","shared_dir_size":"lots"}}}`)
	entries := assertValidation422(t, rr)
	assertFields(t, entries, "runtimes.gke.shared_dir_size", "profiles.big.shared_dir_size")
}

func TestSettingsEnvelope_FileTierFieldEntry(t *testing.T) {
	rr, _ := fileModePutServerConfig(t, &Server{}, `{"server":{"hub":{"agent_endpoint":"not a url"}}}`)
	assertFields(t, assertValidation422(t, rr), "server.hub.agent_endpoint")

	rr, _ = fileModePutServerConfig(t, &Server{}, `{"default_timezone":"Not/AZone"}`)
	assertFields(t, assertValidation422(t, rr), "default_timezone")
}

func TestSettingsEnvelope_FileTierMalformedBodyStays400(t *testing.T) {
	rr, _ := fileModePutServerConfig(t, &Server{}, `{"server":`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

func TestSettingsEnvelope_NullFederation(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"federation":null}`)
	entries := assertValidation422(t, rr)
	assertFields(t, entries, "server.federation")
	if keys, _ := settingsErrorFields(t, rr.Body.Bytes())["keys"].([]interface{}); len(keys) != 1 || keys[0] != "federation" {
		t.Errorf("details.keys = %v, want [federation]", keys)
	}
}

// conflictingSettingStore answers every write with a revision conflict.
type conflictingSettingStore struct {
	*fakeHubSettingStore
}

func (c conflictingSettingStore) UpsertHubSetting(context.Context, string, json.RawMessage, string, int64, string) (*store.HubSetting, error) {
	return nil, store.ErrRevisionConflict
}

func TestSettingsEnvelope_ProjectDefaultsRevisionConflict(t *testing.T) {
	fake := newFakeHubSettingStore()
	fake.seed("project_defaults", json.RawMessage(`{"default_scratchpad":true}`))
	srv := &Server{}
	ops := NewOperationalSettings(conflictingSettingStore{fake}, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv.operationalSettings.Store(ops)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/project-defaults",
		bytes.NewBufferString(`{"default_scratchpad":false}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAdminProjectDefaults(rr, adminContext(req))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	fields := settingsErrorFields(t, rr.Body.Bytes())
	if fields["error"] != ErrCodeRevisionConflict {
		t.Errorf("code = %v, want %s", fields["error"], ErrCodeRevisionConflict)
	}
	conflicted, _ := fields["conflicted"].([]interface{})
	if len(conflicted) != 1 {
		t.Fatalf("conflicted = %v", fields["conflicted"])
	}
	c := conflicted[0].(map[string]interface{})
	if c["section"] != "project_defaults" || c["expected_revision"] != float64(1) || c["current_revision"] == nil {
		t.Errorf("conflicted[0] = %v, want project_defaults expected 1 with a current revision", c)
	}
}
