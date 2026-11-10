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
	"testing"
)

// The admin pages send expected_revisions 0 for a section that had no
// stored row when they loaded (ptone/scion#3922). 0 is create-only: a row
// created since then answers 409 with the expected and current revision,
// and with still no row the save creates it.
func TestPutServerConfigDB_ExpectedRevisionZeroIsCreateOnly(t *testing.T) {
	t.Run("a row created since load conflicts", func(t *testing.T) {
		srv, fakeStore, ops := newTestDBServer(t)
		fakeStore.seed("access", json.RawMessage(`{"admin_emails":["first@example.com"]}`))
		_, _ = ops.Refresh(context.Background())

		rr := putServerConfigDB(t, srv, ops,
			`{"server":{"hub":{"admin_emails":["second@example.com"]}},"expected_revisions":{"access":0}}`)
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
		if c["section"] != "access" || c["expected_revision"] != float64(0) || c["current_revision"] != float64(1) {
			t.Errorf("conflicted[0] = %v, want access expected 0 current 1", c)
		}
		access, _ := readAccessRow(t, fakeStore)
		if len(access.AdminEmails) != 1 || access.AdminEmails[0] != "first@example.com" {
			t.Errorf("the stored row must be unchanged, got %v", access.AdminEmails)
		}
	})

	t.Run("with still no row the save creates it", func(t *testing.T) {
		srv, fakeStore, ops := newTestDBServer(t)
		rr := putServerConfigDB(t, srv, ops,
			`{"server":{"hub":{"admin_emails":["new@example.com"]}},"expected_revisions":{"access":0}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
		}
		access, _ := readAccessRow(t, fakeStore)
		row := storedRow(fakeStore, "access")
		if len(access.AdminEmails) != 1 || access.AdminEmails[0] != "new@example.com" || row == nil || row.Revision != 1 {
			t.Errorf("row = %v (stored %+v), want [new@example.com] at revision 1", access.AdminEmails, row)
		}
	})
}
