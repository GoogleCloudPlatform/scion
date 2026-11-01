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
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// settingsErrorFields decodes a settings write error body, which must use
// the writeError envelope {"error": {"code", "message", "details"}}
// (ptone/scion#4450), and returns it flattened: "error" is the code,
// "message" the message, and every details member at the top level. A
// flat body ({"error": "<code>", ...}) fails the test.
func settingsErrorFields(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error body: %v: %s", err, body)
	}
	var nested struct {
		Code    string                 `json:"code"`
		Message string                 `json:"message"`
		Details map[string]interface{} `json:"details"`
	}
	if err := json.Unmarshal(env.Error, &nested); err != nil || nested.Code == "" {
		t.Fatalf("error body is not the writeError envelope: %s", body)
	}
	out := map[string]interface{}{"error": nested.Code, "message": nested.Message}
	for k, v := range nested.Details {
		out[k] = v
	}
	return out
}

// settingsErrorDetailsJSON returns the raw "details" member of a settings
// error body in the writeError envelope, failing the test on a flat body.
func settingsErrorDetailsJSON(t *testing.T, body []byte) []byte {
	t.Helper()
	var env struct {
		Error struct {
			Code    string          `json:"code"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" || len(env.Error.Details) == 0 {
		t.Fatalf("error body is not the writeError envelope with details: %s", body)
	}
	return env.Error.Details
}

// settingsErrorEntries returns the details.errors entries of a settings
// validation error body, failing the test unless every entry names a field
// and a message.
func settingsErrorEntries(t *testing.T, rr *httptest.ResponseRecorder) []settingsFieldError {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Field  string               `json:"field"`
				Errors []settingsFieldError `json:"errors"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body: %v: %s", err, rr.Body.String())
	}
	entries := env.Error.Details.Errors
	if len(entries) == 0 {
		t.Fatalf("no details.errors entries: %s", rr.Body.String())
	}
	for _, e := range entries {
		if e.Field == "" || e.Message == "" {
			t.Errorf("entry without field or message: %+v (body %s)", e, rr.Body.String())
		}
	}
	if env.Error.Details.Field != entries[0].Field {
		t.Errorf("details.field = %q, want the first entry's field %q", env.Error.Details.Field, entries[0].Field)
	}
	return entries
}

func TestSettingsKoanfField(t *testing.T) {
	tests := []struct {
		section, pointer, want string
	}{
		// Mapped sections: the first segment is a section key.
		{"access", "/admin_emails", "server.hub.admin_emails"},
		{"access", "/admin_emails/0", "server.hub.admin_emails.0"},
		{"federation", "/trusted_issuers/1/issuer_url", "server.federation.trusted_issuers.1.issuer_url"},
		{"project_defaults", "/default_scratchpad", "project_defaults.default_scratchpad"},
		// Document keys that are koanf paths.
		{"agent_defaults", "/default_model", "default_model"},
		// Sections below one koanf root.
		{"telemetry", "/cloud/endpoint", "telemetry.cloud.endpoint"},
		{"runtimes", "/docker/shared_dir_size", "runtimes.docker.shared_dir_size"},
		{"profiles", "/p1/resources", "profiles.p1.resources"},
		// Dotted input works like a pointer.
		{"telemetry", "cloud.tls", "telemetry.cloud.tls"},
		// The section as a whole.
		{"federation", "", "server.federation"},
		{"runtimes", "", "runtimes"},
		{"telemetry", "", "telemetry"},
		{"maintenance", "", "maintenance"},
		// Unknown locations fall back below the section name.
		{"maintenance", "/message", "maintenance.message"},
		{"nosuch", "/a/b", "nosuch.a.b"},
		// Pointer escapes.
		{"profiles", "/a~1b/x", "profiles.a/b.x"},
	}
	for _, tt := range tests {
		if got := settingsKoanfField(tt.section, tt.pointer); got != tt.want {
			t.Errorf("settingsKoanfField(%q, %q) = %q, want %q", tt.section, tt.pointer, got, tt.want)
		}
	}
}

func TestSettingsValidationEnvelope(t *testing.T) {
	rr := httptest.NewRecorder()
	writeSettingsValidation(rr, "", sortedSectionValidationEntries(map[string][]config.ValidationError{
		"telemetry": {{Path: "/cloud/endpoint", Message: "bad endpoint"}},
		"access":    {{Path: "/default_user_role", Message: "bad role"}},
	}), nil)
	if rr.Code != 422 {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
	fields := settingsErrorFields(t, rr.Body.Bytes())
	if fields["error"] != ErrCodeValidationError {
		t.Errorf("code = %v, want %s", fields["error"], ErrCodeValidationError)
	}
	entries := settingsErrorEntries(t, rr)
	want := []settingsFieldError{
		{Section: "access", Field: "server.auth.default_user_role", Path: "/default_user_role", Message: "bad role"},
		{Section: "telemetry", Field: "telemetry.cloud.endpoint", Path: "/cloud/endpoint", Message: "bad endpoint"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
	if fields["message"] != "server.auth.default_user_role: bad role" {
		t.Errorf("message = %v", fields["message"])
	}
}

func TestSettingsCodedKeysEnvelope(t *testing.T) {
	rr := httptest.NewRecorder()
	writeSettingsCodedKeysError(rr, "layer0_rejected", "no", []string{"server.database.driver", "federation"})
	if rr.Code != 422 {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
	fields := settingsErrorFields(t, rr.Body.Bytes())
	if fields["error"] != "layer0_rejected" {
		t.Errorf("code = %v", fields["error"])
	}
	keys, _ := fields["keys"].([]interface{})
	if len(keys) != 2 || keys[1] != "federation" {
		t.Errorf("keys = %v, want the request keys as sent", fields["keys"])
	}
	entries := settingsErrorEntries(t, rr)
	if entries[1].Field != "server.federation" {
		t.Errorf("the federation key's field = %q, want server.federation", entries[1].Field)
	}
}

func TestSettingsRevisionConflictEnvelope(t *testing.T) {
	rr := httptest.NewRecorder()
	writeSettingsRevisionConflict(rr, "stale", []map[string]interface{}{settingsConflictEntry("access", 3, 4)},
		map[string]interface{}{"applied": map[string]int64{}})
	if rr.Code != 409 {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	fields := settingsErrorFields(t, rr.Body.Bytes())
	if fields["error"] != "revision_conflict" {
		t.Errorf("code = %v", fields["error"])
	}
	conflicted, _ := fields["conflicted"].([]interface{})
	if len(conflicted) != 1 {
		t.Fatalf("conflicted = %v", fields["conflicted"])
	}
	c := conflicted[0].(map[string]interface{})
	if c["section"] != "access" || c["expected_revision"] != float64(3) || c["current_revision"] != float64(4) {
		t.Errorf("conflicted[0] = %v", c)
	}
	if _, ok := fields["applied"]; !ok {
		t.Error("extra details member applied missing")
	}
}
