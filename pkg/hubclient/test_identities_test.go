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

// The token value below is a synthetic placeholder, not a credential.
const fakeTestIdentityToken = "synthetic.fixture.token"

func TestTestIdentities_IssueSendsBodyOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/test-identities" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("body: %v", err)
		}
		if req["role"] != "viewer" || req["purpose"] != "ci" || req["tokenTtlSeconds"] != float64(600) {
			t.Errorf("unexpected body %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"identity":{"id":"u1","email":"e@scion-fixture.invalid","role":"viewer","live":true},"accessToken":"` + fakeTestIdentityToken + `","tokenType":"Bearer","expiresIn":600}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.TestIdentities().Issue(context.Background(), &IssueTestIdentityRequest{Role: "viewer", Purpose: "ci", TokenTTLSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Identity.ID != "u1" || resp.AccessToken != fakeTestIdentityToken || resp.Identity.Role != "viewer" {
		t.Errorf("unexpected response %+v", resp.Identity)
	}
	if calls.Load() != 1 {
		t.Errorf("issue sent %d times, want 1", calls.Load())
	}
}

func TestTestIdentities_IssueNotRetriedOnServerError(t *testing.T) {
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
	if _, err := c.TestIdentities().Issue(context.Background(), nil); err == nil {
		t.Fatal("want error")
	}
	if _, err := c.TestIdentities().Token(context.Background(), "u1", nil); err == nil {
		t.Fatal("want error")
	}
	if calls.Load() != 2 {
		t.Errorf("sent %d requests, want 2 (no retries)", calls.Load())
	}
}

func TestTestIdentities_TokenListDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/test-identities/u1/token":
			_, _ = w.Write([]byte(`{"identity":{"id":"u1"},"accessToken":"` + fakeTestIdentityToken + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/test-identities":
			if got := r.URL.Query().Get("limit"); got != "7" {
				t.Errorf("limit = %q", got)
			}
			if got := r.URL.Query().Get("includeExpired"); got != "true" {
				t.Errorf("includeExpired = %q", got)
			}
			_, _ = w.Write([]byte(`{"items":[{"id":"u1"},{"id":"u2"}],"truncated":true}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/test-identities/u1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/test-identities/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"test identity not found"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/test-identities/busy":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"owns agents","details":{"agents":[{"id":"a1","slug":"s1","projectId":"p1"}]}}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/test-identities/owner":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"last_owner","message":"last owner","details":{"projects":[{"id":"p1","name":"proj"}]}}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc := c.TestIdentities()

	tok, err := svc.Token(ctx, "u1", &TestIdentityTokenRequest{TokenTTLSeconds: 60})
	if err != nil || tok.AccessToken != fakeTestIdentityToken {
		t.Fatalf("token: %v %+v", err, tok)
	}
	list, err := svc.List(ctx, &ListTestIdentitiesOptions{Limit: 7, IncludeExpired: true})
	if err != nil || len(list.Items) != 2 || !list.Truncated {
		t.Fatalf("list: %v %+v", err, list)
	}
	if err := svc.Delete(ctx, "u1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	err = svc.Delete(ctx, "gone")
	if !apiclient.IsNotFoundError(err) {
		t.Fatalf("delete gone: want 404, got %v", err)
	}
	if _, _, ok := TestIdentityDeleteConflict(err); ok {
		t.Error("a 404 is not a delete conflict")
	}

	agents, projects, ok := TestIdentityDeleteConflict(svc.Delete(ctx, "busy"))
	if !ok || len(agents) != 1 || agents[0] != (TestIdentityBlockingAgent{ID: "a1", Slug: "s1", ProjectID: "p1"}) || len(projects) != 0 {
		t.Errorf("busy: ok=%v agents=%+v projects=%+v", ok, agents, projects)
	}
	agents, projects, ok = TestIdentityDeleteConflict(svc.Delete(ctx, "owner"))
	if !ok || len(projects) != 1 || projects[0] != (TestIdentityBlockingProject{ID: "p1", Name: "proj"}) || len(agents) != 0 {
		t.Errorf("owner: ok=%v agents=%+v projects=%+v", ok, agents, projects)
	}

	if err := svc.Delete(ctx, ""); err == nil {
		t.Error("empty id: want error")
	}
}
