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

// Tests for the admin-role test identity client (ptone/scion#4240, admin
// tier A4). Tokens below are synthetic placeholders, not credentials.

const fakeTestAdminToken = "synthetic.admin.fixture.token"

// IssueAdmin sends the body once (never retried, even with a retrying
// client) and decodes the identity, its kind and issuance ID.
func TestTestAdminIdentities_IssueAdminSendsBodyOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/test-identities/admin" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("body: %v", err)
		}
		want := map[string]interface{}{"adminRole": "hub-admin", "purpose": "admin journey", "tokenTtlSeconds": float64(600), "lifetimeSeconds": float64(1800)}
		if len(req) != len(want) {
			t.Errorf("unexpected body %s", body)
		}
		for k, v := range want {
			if req[k] != v {
				t.Errorf("body[%s] = %v, want %v", k, req[k], v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"identity":{"id":"ta-1","email":"test-admin-x@scion-test-hub-admin.invalid","role":"member","live":true,` +
			`"kind":"test_hub_admin","adminRole":"hub-admin","issuanceId":"iss-1"},"accessToken":"` + fakeTestAdminToken + `","tokenType":"Bearer","expiresIn":600}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.TestIdentities().IssueAdmin(context.Background(), &IssueTestAdminIdentityRequest{
		AdminRole: "hub-admin", Purpose: "admin journey", TokenTTLSeconds: 600, LifetimeSeconds: 1800,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Identity.ID != "ta-1" || resp.Identity.Kind != "test_hub_admin" || resp.Identity.AdminRole != "hub-admin" ||
		resp.Identity.IssuanceID != "iss-1" || resp.AccessToken != fakeTestAdminToken {
		t.Errorf("unexpected response %+v", resp.Identity)
	}
	if calls.Load() != 1 {
		t.Errorf("issue sent %d times, want 1", calls.Load())
	}
}

// IssueAdmin is not retried on a server error, refuses an empty admin
// role before sending, and refuses a response without a token.
func TestTestAdminIdentities_IssueAdminFailures(t *testing.T) {
	var calls atomic.Int32
	status := http.StatusInternalServerError
	body := `{"error":{"code":"internal_error","message":"x"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.TestIdentities().IssueAdmin(ctx, &IssueTestAdminIdentityRequest{AdminRole: "hub-admin", Purpose: "admin journey"}); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Errorf("issue sent %d times, want 1", calls.Load())
	}
	if _, err := c.TestIdentities().IssueAdmin(ctx, &IssueTestAdminIdentityRequest{Purpose: "admin journey"}); err == nil {
		t.Error("expected an error for an empty admin role")
	}
	if _, err := c.TestIdentities().IssueAdmin(ctx, nil); err == nil {
		t.Error("expected an error for a nil request")
	}
	if calls.Load() != 1 {
		t.Errorf("a refused request was sent (%d calls)", calls.Load())
	}
	status, body = http.StatusCreated, `{"identity":{"id":"ta-1"},"accessToken":""}`
	if _, err := c.TestIdentities().IssueAdmin(ctx, &IssueTestAdminIdentityRequest{AdminRole: "hub-admin", Purpose: "admin journey"}); err == nil {
		t.Error("expected an error for a response without a token")
	}
}

// RevokeAllAdmin posts once with no body and decodes each outcome; a 403
// stays a forbidden API error.
func TestTestAdminIdentities_RevokeAllAdmin(t *testing.T) {
	var calls atomic.Int32
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/test-identities/admin/revoke-all" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if b, _ := io.ReadAll(r.Body); len(b) != 0 {
			t.Errorf("revoke-all sent a body: %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"revoked":[{"id":"ta-1","kind":"test_hub_admin","outcome":"deleted"},{"id":"ta-2","kind":"test_super_admin","outcome":"suspended"}],"count":2}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"revoke-all requires a human super-admin session"}}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.TestIdentities().RevokeAllAdmin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Count != 2 || len(resp.Revoked) != 2 || resp.Revoked[1].Outcome != "suspended" || resp.Revoked[1].Kind != "test_super_admin" {
		t.Errorf("unexpected response %+v", resp)
	}
	status = http.StatusForbidden
	_, err = c.TestIdentities().RevokeAllAdmin(context.Background())
	if !apiclient.IsForbiddenError(err) {
		t.Errorf("err = %v, want a forbidden API error", err)
	}
	if calls.Load() != 2 {
		t.Errorf("revoke-all sent %d times, want 2 (one per call)", calls.Load())
	}
}

// AdminResidue escapes the ID into the path, decodes the report and
// refuses an empty ID before sending.
func TestTestAdminIdentities_AdminResidue(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v1/test-identities/admin/a%2Fb/residue" {
			t.Errorf("got %s %s", r.Method, r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"identityId":"a/b","issuanceId":"iss-1",` +
			`"stampedGrants":[{"kind":"role_binding","id":"rb-1","roleDefinitionId":"project-admin","principalType":"user","principalId":"u-2","scopeType":"project","scopeId":"p-1"}],` +
			`"stampedResources":{"agents":["ag-1"],"schedules":[],"groups":[],"projects":["p-1"]},` +
			`"unstampedChanges":[{"mutationType":"user_suspend","targetType":"user","targetId":"u-3","actorKind":"user","actorId":"a/b","timestamp":"2026-10-15T10:00:00Z"}],` +
			`"keptRoles":[],"truncated":true,"limit":"complete only as far as the mutation audit"}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := c.TestIdentities().AdminResidue(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if rep.IdentityID != "a/b" || rep.IssuanceID != "iss-1" || len(rep.StampedGrants) != 1 || rep.StampedGrants[0].ScopeID != "p-1" ||
		len(rep.StampedResources.Agents) != 1 || len(rep.UnstampedChanges) != 1 || rep.UnstampedChanges[0].TargetID != "u-3" || !rep.Truncated {
		t.Errorf("unexpected report %+v", rep)
	}
	if _, err := c.TestIdentities().AdminResidue(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty id")
	}
	if calls.Load() != 1 {
		t.Errorf("residue sent %d times, want 1", calls.Load())
	}
}
