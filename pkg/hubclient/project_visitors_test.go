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
	"time"
)

// Project visitor grants (ptone/scion#4603).

const projectVisitorJSON = `{"id":"v-1","bindingId":"rb-1","email":"alice@corp.example","issuer":"https://hub-a.example.com","status":"pending","issuerAccepted":true,"grantedBy":"u-1","expiresAt":"2026-11-01T00:00:00Z","createdAt":"2026-10-13T00:00:00Z"}`

// visitorProjectID contains a space, so the tests also check that the
// project ID is percent-encoded in the path.
const visitorProjectID = "proj a"

const visitorBasePath = "/api/v1/projects/proj%20a/visitors"

func TestProjectVisitors_ListAndRemove(t *testing.T) {
	var removedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == visitorBasePath:
			_, _ = w.Write([]byte(`{"visitors":[` + projectVisitorJSON + `]}`))
		case r.Method == http.MethodDelete:
			removedPath = r.URL.EscapedPath()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("got %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	visitors := c.ProjectVisitors(visitorProjectID)

	list, err := visitors.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d visitors, want 1", len(list))
	}
	v := list[0]
	if v.ID != "v-1" || v.BindingID != "rb-1" || v.Email != "alice@corp.example" || v.Status != "pending" || !v.IssuerAccepted || v.ExpiresAt == nil {
		t.Errorf("unexpected visitor %+v", v)
	}

	if err := visitors.Remove(context.Background(), "v/1"); err != nil {
		t.Fatalf("remove answered 204, want no error: %v", err)
	}
	if want := visitorBasePath + "/v%2F1"; removedPath != want {
		t.Errorf("remove path = %s, want %s", removedPath, want)
	}
}

func TestProjectVisitors_AddSentOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.EscapedPath() != visitorBasePath {
			t.Errorf("got %s %s", r.Method, r.URL.EscapedPath())
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("body: %v", err)
		}
		if req["issuer"] != "https://hub-a.example.com" || req["email"] != "alice@corp.example" {
			t.Errorf("unexpected body %s", body)
		}
		if _, ok := req["notBefore"]; ok {
			t.Errorf("an unset notBefore is omitted: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"x"}}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ProjectVisitors(visitorProjectID).Add(context.Background(), &AddProjectVisitorRequest{
		Issuer: "https://hub-a.example.com", Email: "alice@corp.example",
	})
	if err == nil {
		t.Fatal("expected an error for a 500")
	}
	if calls.Load() != 1 {
		t.Errorf("add sent %d times on a 500, want 1 (no retry)", calls.Load())
	}
}

func TestProjectVisitors_UpdateSendsExplicitNullBounds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.EscapedPath() != visitorBasePath+"/v-1" {
			t.Errorf("got %s %s", r.Method, r.URL.EscapedPath())
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("body: %v", err)
		}
		// A nil bound is sent as an explicit null: PATCH replaces the window.
		if v, ok := req["notBefore"]; !ok || v != nil {
			t.Errorf("notBefore must be an explicit null: %s", body)
		}
		if v, ok := req["expiresAt"]; !ok || v != "2026-11-01T00:00:00Z" {
			t.Errorf("unexpected expiresAt: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(projectVisitorJSON))
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	v, err := c.ProjectVisitors(visitorProjectID).Update(context.Background(), "v-1", &UpdateProjectVisitorRequest{ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "v-1" {
		t.Errorf("unexpected visitor %+v", v)
	}
}

const projectVisitorRuleJSON = `{"id":"r-1","issuer":"https://hub-a.example.com","domain":"corp.example","issuerAccepted":true,"createdBy":"u-1","createdAt":"2026-10-13T00:00:00Z"}`

// The visitor rule calls address /visitors/rules and /visitors/rules/{id}.
func TestProjectVisitors_Rules(t *testing.T) {
	var seen []string
	var addCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == visitorBasePath+"/rules":
			_, _ = w.Write([]byte(`{"rules":[` + projectVisitorRuleJSON + `]}`))
		case r.Method == http.MethodPost && r.URL.EscapedPath() == visitorBasePath+"/rules":
			addCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			if err := json.Unmarshal(body, &req); err != nil || req["issuer"] != "https://hub-a.example.com" || req["domain"] != "corp.example" {
				t.Errorf("unexpected body %s (%v)", body, err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(projectVisitorRuleJSON))
		case r.Method == http.MethodPatch && r.URL.EscapedPath() == visitorBasePath+"/rules/r%2F1":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("body: %v", err)
			}
			if v, ok := req["expiresAt"]; !ok || v != nil {
				t.Errorf("expiresAt must be an explicit null: %s", body)
			}
			_, _ = w.Write([]byte(projectVisitorRuleJSON))
		case r.Method == http.MethodDelete && r.URL.EscapedPath() == visitorBasePath+"/rules/r%2F1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("got %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	visitors := c.ProjectVisitors(visitorProjectID)
	ctx := context.Background()

	rules, err := visitors.ListRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ID != "r-1" || rules[0].Domain != "corp.example" || !rules[0].IssuerAccepted {
		t.Errorf("unexpected rules %+v", rules)
	}
	rule, err := visitors.AddRule(ctx, &AddProjectVisitorRuleRequest{Issuer: "https://hub-a.example.com", Domain: "corp.example"})
	if err != nil {
		t.Fatal(err)
	}
	if rule.ID != "r-1" || addCalls.Load() != 1 {
		t.Errorf("unexpected rule %+v after %d calls", rule, addCalls.Load())
	}
	if _, err := visitors.UpdateRule(ctx, "r/1", &UpdateProjectVisitorRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := visitors.RemoveRule(ctx, "r/1"); err != nil {
		t.Fatalf("remove answered 204, want no error: %v", err)
	}
	if len(seen) != 4 {
		t.Errorf("requests = %v", seen)
	}
}

// A listed visitor carries its source.
func TestProjectVisitors_ListCarriesSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"visitors":[{"id":"v-2","email":"bob@corp.example","issuer":"https://hub-a.example.com","status":"active","source":"rule","sourceRuleId":"r-1","createdAt":"2026-10-13T00:00:00Z"}]}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.ProjectVisitors(visitorProjectID).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Source != "rule" || list[0].SourceRuleID != "r-1" {
		t.Errorf("unexpected visitors %+v", list)
	}
}
