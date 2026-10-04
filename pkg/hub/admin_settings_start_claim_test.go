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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

func putStartClaimConfigDB(t *testing.T, srv *Server, ops *OperationalSettings, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	return rr
}

// A lifecycle PUT that leaves the start-claim keys out (as the admin form
// does) keeps their stored values.
func TestPutServerConfigDB_LifecycleKeepsStartClaimSettings(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	rr := putStartClaimConfigDB(t, srv, ops, `{"server": {"hub": {"start_claim_lease_ttl": "60s", "start_unconfirmed_hold": "14m"}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("first PUT: %d %s", rr.Code, rr.Body.String())
	}
	rr = putStartClaimConfigDB(t, srv, ops, `{"server": {"hub": {"stalled_threshold": "10m"}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second PUT: %d %s", rr.Code, rr.Body.String())
	}

	fakeStore.mu.Lock()
	row := fakeStore.settings["lifecycle"]
	fakeStore.mu.Unlock()
	var lc opsettings.LifecycleSettings
	if err := json.Unmarshal(row.Value, &lc); err != nil {
		t.Fatal(err)
	}
	if lc.StalledThreshold != "10m" {
		t.Errorf("stalled_threshold = %q, want 10m", lc.StalledThreshold)
	}
	if lc.StartClaimLeaseTTL != "60s" || lc.StartUnconfirmedHold != "14m" {
		t.Errorf("start-claim settings wiped by a lifecycle PUT: %+v", lc)
	}
}

func TestPutServerConfigDB_StartClaimSettingsValidated(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	for _, body := range []string{
		`{"server": {"hub": {"start_claim_lease_ttl": "soon"}}}`,
		`{"server": {"hub": {"start_claim_lease_ttl": "10s"}}}`,
		`{"server": {"hub": {"start_max_duration": "5m"}}}`,
		`{"server": {"hub": {"start_unconfirmed_hold": "10m"}}}`,
		`{"server": {"hub": {"start_create_unconfirmed_hold": "20m"}}}`,
	} {
		if rr := putStartClaimConfigDB(t, srv, ops, body); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422: %s", body, rr.Code, rr.Body.String())
		}
	}
	if rr := putStartClaimConfigDB(t, srv, ops, `{"server": {"hub": {"start_max_duration": "15m"}}}`); rr.Code != http.StatusOK {
		t.Errorf("valid value rejected: %d %s", rr.Code, rr.Body.String())
	}
}
