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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// startClaimRefusedBodies are start-claim saves both tiers refuse with a
// 422 naming the key: unparseable, below or above a range, and a create
// hold over the effective hold.
var startClaimRefusedBodies = map[string]string{
	`{"server": {"hub": {"start_claim_lease_ttl": "soon"}}}`:          "server.hub.start_claim_lease_ttl",
	`{"server": {"hub": {"start_claim_lease_ttl": "10s"}}}`:           "server.hub.start_claim_lease_ttl",
	`{"server": {"hub": {"start_claim_lease_ttl": "6m"}}}`:            "server.hub.start_claim_lease_ttl",
	`{"server": {"hub": {"start_max_duration": "5m"}}}`:               "server.hub.start_max_duration",
	`{"server": {"hub": {"start_create_unconfirmed_hold": "20m"}}}`:   "server.hub.start_create_unconfirmed_hold",
	`{"server": {"hub": {"start_unconfirmed_hold": "not-a-length"}}}`: "server.hub.start_unconfirmed_hold",
}

// startClaimAcceptedBodies are start-claim saves both tiers accept: in
// range, a hold an earlier release accepted (raised when applied), and a
// clear.
var startClaimAcceptedBodies = []string{
	`{"server": {"hub": {"start_max_duration": "15m"}}}`,
	`{"server": {"hub": {"start_claim_lease_ttl": "2m"}}}`,
	`{"server": {"hub": {"start_unconfirmed_hold": "13m"}}}`,
	`{"server": {"hub": {"start_create_unconfirmed_hold": "5m"}}}`,
	`{"server": {"hub": {"start_max_duration": ""}}}`,
}

// refusedField returns the field the 422 names, or "" when the response is
// not a 422 with exactly one field entry.
func refusedField(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		return ""
	}
	entries := settingsErrorEntries(t, rr)
	if len(entries) != 1 {
		return ""
	}
	return entries[0].Field
}

// TestPutServerConfigFile_StartClaimSettingsValidated: the settings.yaml PUT
// refuses an out-of-range or unparseable start-claim value with the same
// 422 envelope as the DB tier, and writes nothing.
func TestPutServerConfigFile_StartClaimSettingsValidated(t *testing.T) {
	for body, field := range startClaimRefusedBodies {
		srv := &Server{maintenance: NewMaintenanceState(false, "")}
		rr, settingsPath := fileModePutServerConfig(t, srv, body)
		if got := refusedField(t, rr); got != field {
			t.Errorf("%s: field %q, want %q (status %d: %s)", body, got, field, rr.Code, rr.Body.String())
		}
		if _, err := os.Stat(settingsPath); err == nil {
			t.Errorf("%s: settings.yaml written by a refused PUT", body)
		}
	}
}

// TestPutServerConfigFile_StartClaimSettingsAccepted: in-range values, a
// legacy hold and a clear save, and the value reaches settings.yaml.
func TestPutServerConfigFile_StartClaimSettingsAccepted(t *testing.T) {
	for _, body := range startClaimAcceptedBodies {
		srv := &Server{maintenance: NewMaintenanceState(false, "")}
		rr, settingsPath := fileModePutServerConfig(t, srv, body)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200: %s", body, rr.Code, rr.Body.String())
			continue
		}
		if strings.Contains(body, `"15m"`) {
			data, err := os.ReadFile(settingsPath)
			if err != nil || !strings.Contains(string(data), "start_max_duration: 15m") {
				t.Errorf("%s: start_max_duration not written: %s (%v)", body, data, err)
			}
		}
	}
}

// TestPutServerConfigFile_StartClaimValidatedAgainstStartupValues: as on the
// DB tier, the create hold is compared with the hold in effect (the startup
// value for an absent key).
func TestPutServerConfigFile_StartClaimValidatedAgainstStartupValues(t *testing.T) {
	body := `{"server": {"hub": {"start_create_unconfirmed_hold": "18m"}}}`
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	srv.config.StartClaim = StartClaimSettings{UnconfirmedHold: 20 * time.Minute}
	if rr, _ := fileModePutServerConfig(t, srv, body); rr.Code != http.StatusOK {
		t.Errorf("a create hold under the startup hold refused: %d %s", rr.Code, rr.Body.String())
	}
	srv = &Server{maintenance: NewMaintenanceState(false, "")}
	if rr, _ := fileModePutServerConfig(t, srv, body); refusedField(t, rr) != "server.hub.start_create_unconfirmed_hold" {
		t.Errorf("a create hold over the default hold accepted: %d %s", rr.Code, rr.Body.String())
	}
}

// TestPutServerConfigFile_UnrelatedSaveIgnoresStoredStartClaim: a save that
// sends no start-claim key is not checked against a stored out-of-range
// value (the hub already replaces it by its default when applied).
func TestPutServerConfigFile_UnrelatedSaveIgnoresStoredStartClaim(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  hub:\n    start_claim_lease_ttl: 1s\n")
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server": {"hub": {"stalled_threshold": "10m"}}}`))
	if rr.Code != http.StatusOK {
		t.Errorf("unrelated PUT refused: %d %s", rr.Code, rr.Body.String())
	}
	// Control: a save that sends a start-claim key is checked on the merged
	// values, the stored out-of-range lease included.
	rr = httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server": {"hub": {"start_max_duration": "15m"}}}`))
	if got := refusedField(t, rr); got != "server.hub.start_claim_lease_ttl" {
		t.Errorf("merged check: field %q, want server.hub.start_claim_lease_ttl (%d %s)", got, rr.Code, rr.Body.String())
	}
}

// TestPutServerConfig_StartClaimTierParity: both tiers refuse the same
// inputs with the same field, and accept the same inputs.
func TestPutServerConfig_StartClaimTierParity(t *testing.T) {
	for body, field := range startClaimRefusedBodies {
		dbSrv, _, ops := newTestDBServer(t)
		db := putStartClaimConfigDB(t, dbSrv, ops, body)
		file, _ := fileModePutServerConfig(t, &Server{maintenance: NewMaintenanceState(false, "")}, body)
		if dbF, fileF := refusedField(t, db), refusedField(t, file); dbF != field || fileF != field {
			t.Errorf("%s: db field %q, file field %q, want both %q", body, dbF, fileF, field)
		}
	}
	for _, body := range startClaimAcceptedBodies {
		dbSrv, _, ops := newTestDBServer(t)
		db := putStartClaimConfigDB(t, dbSrv, ops, body)
		file, _ := fileModePutServerConfig(t, &Server{maintenance: NewMaintenanceState(false, "")}, body)
		if db.Code != http.StatusOK || file.Code != http.StatusOK {
			t.Errorf("%s: db %d, file %d, want both 200", body, db.Code, file.Code)
		}
	}
}

// TestPutServerConfigFile_StoredNonStringStartClaimRefused: a start-claim
// value stored in settings.yaml as a YAML integer (no unit) is not skipped:
// a save that sends another start key is refused with a 422 naming it.
func TestPutServerConfigFile_StoredNonStringStartClaimRefused(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  hub:\n    start_max_duration: 90\n")
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server": {"hub": {"start_claim_lease_ttl": "2m"}}}`))
	if got := refusedField(t, rr); got != "server.hub.start_max_duration" {
		t.Errorf("field %q, want server.hub.start_max_duration (%d %s)", got, rr.Code, rr.Body.String())
	}
}
