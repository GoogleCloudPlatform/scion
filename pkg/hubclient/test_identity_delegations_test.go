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
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Workload issuer delegation routes (ptone/scion#4240, X2 API).

const delegationJSON = `{"id":"dlg-1","grantorId":"u-1","workloadSub":"123456789","scope":"test_identity:issue","memberCap":3,"label":"uat","created":"2026-10-12T08:00:00Z","expiresAt":"2026-10-12T16:00:00Z","live":true}`

func TestTestIdentityDelegations_CreateBodyAndSentOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/test-identity-delegations" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("body: %v", err)
		}
		// The field names of X2's CreateTestIdentityDelegationRequest.
		want := map[string]interface{}{"workloadSub": "123456789", "scopes": []interface{}{"test_identity:issue"}, "ttlSeconds": float64(28800), "memberCap": float64(3), "label": "uat"}
		for k, v := range want {
			got, _ := json.Marshal(req[k])
			exp, _ := json.Marshal(v)
			if string(got) != string(exp) {
				t.Errorf("body %s = %s, want %s", k, got, exp)
			}
		}
		if len(req) != len(want) {
			t.Errorf("unexpected body fields: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(delegationJSON))
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.TestIdentityDelegations().Create(context.Background(), &CreateTestIdentityDelegationRequest{
		WorkloadSub: "123456789", Scopes: []string{"test_identity:issue"}, TTLSeconds: 28800, MemberCap: 3, Label: "uat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "dlg-1" || d.MemberCap != 3 || !d.Live || d.Scope != "test_identity:issue" {
		t.Errorf("unexpected delegation %+v", d)
	}
	if calls.Load() != 1 {
		t.Errorf("create sent %d times, want 1", calls.Load())
	}
}

func TestTestIdentityDelegations_CreateNotRetriedOnServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"x"}}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.TestIdentityDelegations().Create(context.Background(), &CreateTestIdentityDelegationRequest{WorkloadSub: "1", Scopes: []string{"test_identity:issue"}}); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Errorf("create sent %d times on a 500, want 1 (no retry)", calls.Load())
	}
	if _, err := c.TestIdentityDelegations().Create(context.Background(), nil); err == nil {
		t.Error("a nil request must be refused")
	}
}

func TestTestIdentityDelegations_ListAndRevoke(t *testing.T) {
	var revokedPath, revokedRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/test-identity-delegations":
			_, _ = w.Write([]byte(`{"items":[` + delegationJSON + `],"truncated":true}`))
		case r.Method == http.MethodDelete:
			revokedPath, revokedRaw = r.URL.Path, r.URL.RawPath
			if r.URL.Path == "/api/v1/test-identity-delegations/missing" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"delegation not found"}}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("got %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.TestIdentityDelegations().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != "dlg-1" || !list.Truncated {
		t.Errorf("unexpected list %+v", list)
	}

	if err := c.TestIdentityDelegations().Revoke(context.Background(), "dlg-1"); err != nil {
		t.Fatal(err)
	}
	if revokedPath != "/api/v1/test-identity-delegations/dlg-1" {
		t.Errorf("revoke path %q", revokedPath)
	}
	// The id is path-escaped: it stays one path segment.
	if err := c.TestIdentityDelegations().Revoke(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
	if revokedRaw != "/api/v1/test-identity-delegations/a%2Fb" {
		t.Errorf("revoke raw path %q, want the id escaped", revokedRaw)
	}
	err = c.TestIdentityDelegations().Revoke(context.Background(), "missing")
	if !apiclient.IsNotFoundError(err) {
		t.Errorf("revoke of a missing delegation: %v, want a 404 API error", err)
	}
	if err := c.TestIdentityDelegations().Revoke(context.Background(), ""); err == nil {
		t.Error("an empty id must be refused")
	}
}
