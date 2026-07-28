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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Contract tests for the GCP service account service.
//
// This file did not exist until P5, and its absence is the whole reason
// TestGCPServiceAccounts_List_DecodesTheServersActualShape below describes a
// four-month-old break rather than a hypothetical one. Six methods, zero
// tests, so nothing pinned the client's decoding against the server's
// encoding and the two drifted apart in a commit that was itself a correct
// fix to the server.
//
// The bodies below are the shapes the hub really writes. When changing one,
// change it because pkg/hub changed, not to make a test pass.
//
// No live GCP: every response here is canned, which is the whole point — the
// service under test is an HTTP client and GCP is on the far side of the hub.

// saListBody is what both list handlers actually write:
// ListGCPServiceAccountsResponse, an OBJECT with an "items" array.
// pkg/hub/handlers_gcp_identity.go:301.
const saListBody = `{
  "items": [
    {"id":"sa-1","scope":"project","scopeId":"proj-1","email":"one@x.iam.gserviceaccount.com","verified":true,
     "_capabilities":{"actions":["read","assign"]}},
    {"id":"sa-2","scope":"hub","scopeId":"hub-instance","email":"two@x.iam.gserviceaccount.com","verified":false,
     "_capabilities":{"actions":["read"]}}
  ],
  "_capabilities":{"actions":["list"]}
}`

func saTestClient(t *testing.T, h http.HandlerFunc) (Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv.Close
}

// THE REGRESSION. Broken since d65dc095 (2026-03-17), which changed the server
// from writing a bare array to writing {"items":[...]} in order to return
// capabilities — a correct change — and did not update the only Go client that
// decodes it. `scion project service-accounts list`
// (cmd/project_service_accounts.go:223) has returned a decode error, not a
// short list, ever since.
//
// It survived because List hand-rolled its own json.NewDecoder instead of
// using apiclient.DecodeResponse like every sibling method. That private copy
// of the contract is what went stale; Get, Create, Verify and Mint all rode
// the shared helper and were unaffected by the same server change.
func TestGCPServiceAccounts_List_DecodesTheServersActualShape(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(saListBody))
	})
	defer done()

	sas, err := c.GCPServiceAccounts("proj-1").List(context.Background())
	if err != nil {
		t.Fatalf("List against the server's real response shape must succeed, got: %v", err)
	}
	if len(sas) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(sas))
	}
	if sas[0].ID != "sa-1" || sas[1].ID != "sa-2" {
		t.Errorf("unexpected ids: %q, %q", sas[0].ID, sas[1].ID)
	}
	// Scope must survive decoding: every consumer that distinguishes a
	// hub-scoped account from a project-scoped one reads this field.
	if sas[1].Scope != "hub" {
		t.Errorf("hub-scoped account lost its scope in decode: %q", sas[1].Scope)
	}
}

// Delete swallowed every HTTP error status: it returned the transport error
// only, and a transport error is nil for a 403. So a refused deletion of a
// credential binding reported success to the caller, and the CLI printed a
// confirmation for a thing that still exists.
//
// Checked across three statuses because the bug is in the absence of any
// status check at all — a fix that special-cases one code would pass a
// single-status test while leaving the others silent.
func TestGCPServiceAccounts_Delete_ReportsHTTPErrors(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("expected DELETE, got %s", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"nope"}}`))
		})

		err := c.GCPServiceAccounts("proj-1").Delete(context.Background(), "sa-1")
		if err == nil {
			t.Errorf("HTTP %d on delete must surface as an error; reporting success for a "+
				"credential binding that still exists is the dangerous direction to fail in", code)
		}
		done()
	}
}

func TestGCPServiceAccounts_Get(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		want := "/api/v1/projects/proj-1/gcp-service-accounts/sa-1"
		if r.URL.Path != want {
			t.Errorf("expected path %s, got %s", want, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sa-1","scope":"project","email":"one@x.iam.gserviceaccount.com"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts("proj-1").Get(context.Background(), "sa-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sa.ID != "sa-1" {
		t.Errorf("expected sa-1, got %q", sa.ID)
	}
}

func TestGCPServiceAccounts_Get_ReportsHTTPErrors(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no"}}`))
	})
	defer done()

	if _, err := c.GCPServiceAccounts("proj-1").Get(context.Background(), "sa-1"); err == nil {
		t.Error("a 404 must surface as an error rather than a zero-valued account")
	}
}

func TestGCPServiceAccounts_Create(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var got CreateGCPServiceAccountRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if got.Email != "new@x.iam.gserviceaccount.com" {
			t.Errorf("unexpected email on the wire: %q", got.Email)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sa-new","scope":"project","email":"new@x.iam.gserviceaccount.com"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts("proj-1").Create(context.Background(),
		&CreateGCPServiceAccountRequest{Email: "new@x.iam.gserviceaccount.com", ProjectID: "gcp-proj"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sa.ID != "sa-new" {
		t.Errorf("expected sa-new, got %q", sa.ID)
	}
}

func TestGCPServiceAccounts_Verify(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		want := "/api/v1/projects/proj-1/gcp-service-accounts/sa-1/verify"
		if r.URL.Path != want {
			t.Errorf("expected path %s, got %s", want, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sa-1","verified":true,"verificationStatus":"verified"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts("proj-1").Verify(context.Background(), "sa-1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !sa.Verified {
		t.Error("expected verified=true to survive decoding")
	}
}

func TestGCPServiceAccounts_Mint(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		want := "/api/v1/projects/proj-1/gcp-service-accounts/mint"
		if r.URL.Path != want {
			t.Errorf("expected path %s, got %s", want, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sa-minted","managed":true,"managedBy":"scion"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts("proj-1").Mint(context.Background(),
		&MintGCPServiceAccountRequest{AccountID: "minted"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !sa.Managed {
		t.Error("expected managed=true to survive decoding")
	}
}
