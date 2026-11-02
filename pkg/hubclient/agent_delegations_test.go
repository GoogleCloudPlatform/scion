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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Agent delegation grant routes (.design/agent-delegation.md §7, §8, §18.6).

const agentDelegationGrantJSON = `{"id":"g-1","agentId":"a-1","issuerUserId":"u-1","boundary":{"kind":"project","projectId":"p-1"},` +
	`"permissions":["agent.read"],"ceilingVersion":1,"name":"n","expiresAt":"2026-10-31T00:00:00Z","maxCredentialTtlSeconds":900,` +
	`"created":"2026-10-24T00:00:00Z","status":"active","userSuppliedFields":["name","purpose","labels"],"activeCredentialCount":1,` +
	`"activeCredentials":[{"id":"c-1","issuedAt":"2026-10-24T00:00:00Z","expiresAt":"2026-10-24T00:15:00Z"}]}`

const delegatedCredentialTestToken = "scion_adt_testonlytokenvalue"

type adRequest struct {
	method, path, rawPath string
	body                  map[string]interface{}
}

// agentDelegationTestHub records every request and answers each route with
// a canned body.
func agentDelegationTestHub(t *testing.T) (*httptest.Server, func() []adRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []adRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := adRequest{method: r.Method, path: r.URL.Path, rawPath: r.URL.RawPath}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.body); err != nil {
				t.Errorf("%s %s: body is not JSON: %v", r.Method, r.URL.Path, err)
			}
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exchange"):
			_, _ = w.Write([]byte(`{"token":"` + delegatedCredentialTestToken + `","expiresAt":"2026-10-24T00:15:00Z","grantId":"g-1","credentialId":"c-9","audience":"scion-hub:h","permissions":["agent.read"]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/revoke-all"):
			_, _ = w.Write([]byte(`{"revoked":["g-1"],"skipped":[{"grantId":"g-2","agentId":"a-1","reason":"forbidden"}]}`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(agentDelegationGrantJSON))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/delegations"):
			_, _ = w.Write([]byte(`{"delegations":[` + agentDelegationGrantJSON + `]}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(agentDelegationGrantJSON))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []adRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]adRequest(nil), reqs...)
	}
}

// TestAgentDelegations_RoutesAndBodies: every method sends its route's
// method and path (IDs path-escaped) and the documented body fields, and
// decodes the answer.
func TestAgentDelegations_RoutesAndBodies(t *testing.T) {
	srv, requests := agentDelegationTestHub(t)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ad := c.AgentDelegations()
	ctx := context.Background()
	exp := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	ttl := 600

	g, err := ad.Create(ctx, "a-1", &CreateAgentDelegationRequest{
		Boundary: AgentDelegationBoundary{Kind: "project", ProjectID: "p-1"}, Permissions: []string{"agent:read", "agent:attach"},
		ExpiresAt: &exp, Name: "n", Purpose: "why", Labels: map[string]string{"team": "x"}, MaxCredentialTTLSeconds: &ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "g-1" || g.Boundary.ProjectID != "p-1" || g.ActiveCredentialCount != 1 || len(g.ActiveCredentials) != 1 {
		t.Errorf("unexpected grant %+v", g)
	}
	list, err := ad.List(ctx, "a-1")
	if err != nil || len(list.Delegations) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	if _, err := ad.ListMine(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.Get(ctx, "a-1", "g/1"); err != nil {
		t.Fatal(err)
	}
	if err := ad.Revoke(ctx, "a-1", "g-1"); err != nil {
		t.Fatal(err)
	}
	if err := ad.RevokeCredential(ctx, "a-1", "g-1", "c-1"); err != nil {
		t.Fatal(err)
	}
	all, err := ad.RevokeAll(ctx, "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Revoked) != 1 || all.Revoked[0] != "g-1" || len(all.Skipped) != 1 || all.Skipped[0].Reason != "forbidden" {
		t.Errorf("unexpected revoke-all answer %+v", all)
	}
	if _, err := ad.RevokeAllMine(ctx); err != nil {
		t.Fatal(err)
	}
	cred, err := ad.Exchange(ctx, "a-1", "g-1", &ExchangeAgentDelegationRequest{Audience: "scion-hub:h", TTLSeconds: &ttl})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token != delegatedCredentialTestToken || cred.CredentialID != "c-9" {
		t.Errorf("unexpected credential (id %s)", cred.CredentialID)
	}

	want := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/agents/a-1/delegations"},
		{http.MethodGet, "/api/v1/agents/a-1/delegations"},
		{http.MethodGet, "/api/v1/users/me/delegations"},
		{http.MethodGet, "/api/v1/agents/a-1/delegations/g/1"},
		{http.MethodDelete, "/api/v1/agents/a-1/delegations/g-1"},
		{http.MethodDelete, "/api/v1/agents/a-1/delegations/g-1/credentials/c-1"},
		{http.MethodPost, "/api/v1/agents/a-1/delegations/revoke-all"},
		{http.MethodPost, "/api/v1/users/me/delegations/revoke-all"},
		{http.MethodPost, "/api/v1/agents/a-1/delegations/g-1/exchange"},
	}
	got := requests()
	if len(got) != len(want) {
		t.Fatalf("got %d requests, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].method != w.method || got[i].path != w.path {
			t.Errorf("request %d: got %s %s, want %s %s", i, got[i].method, got[i].path, w.method, w.path)
		}
	}
	if got[3].rawPath != "/api/v1/agents/a-1/delegations/g%2F1" {
		t.Errorf("grant ID not path-escaped: raw path %q", got[3].rawPath)
	}
	create := got[0].body
	for k, v := range map[string]interface{}{
		"boundary": map[string]interface{}{"kind": "project", "projectId": "p-1"}, "permissions": []interface{}{"agent:read", "agent:attach"},
		"expiresAt": "2026-10-31T00:00:00Z", "name": "n", "purpose": "why", "labels": map[string]interface{}{"team": "x"},
		"maxCredentialTtlSeconds": float64(600),
	} {
		gotJSON, _ := json.Marshal(create[k])
		wantJSON, _ := json.Marshal(v)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("create body %s = %s, want %s", k, gotJSON, wantJSON)
		}
	}
	if len(create) != 7 {
		t.Errorf("unexpected create body fields: %v", create)
	}
	if got[6].body != nil || got[7].body != nil {
		t.Errorf("revoke-all sends no body: %v %v", got[6].body, got[7].body)
	}
	exch := got[8].body
	if exch["audience"] != "scion-hub:h" || exch["ttlSeconds"] != float64(600) || len(exch) != 2 {
		t.Errorf("unexpected exchange body %v", exch)
	}
}

// TestAgentDelegations_MutationsSentOnce: issuance, revoke-all and exchange
// are sent once, even on a 500 with retries configured.
func TestAgentDelegations_MutationsSentOnce(t *testing.T) {
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
	ad := c.AgentDelegations()
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"create": func() error {
			_, err := ad.Create(ctx, "a-1", &CreateAgentDelegationRequest{Boundary: AgentDelegationBoundary{Kind: "hub"}, Permissions: []string{"agent:read"}, Name: "n"})
			return err
		},
		"revoke-all":      func() error { _, err := ad.RevokeAll(ctx, "a-1"); return err },
		"revoke-all mine": func() error { _, err := ad.RevokeAllMine(ctx); return err },
		"exchange": func() error {
			_, err := ad.Exchange(ctx, "a-1", "g-1", &ExchangeAgentDelegationRequest{Audience: "scion-hub"})
			return err
		},
	} {
		calls.Store(0)
		if err := call(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("%s sent %d times on a 500, want 1 (no retry)", name, n)
		}
	}
}

// TestAgentDelegations_ErrorsAndRefusals: a hub error decodes to an
// APIError with its code; missing IDs, a nil request and an exchange
// without an audience are refused before any request is sent.
func TestAgentDelegations_ErrorsAndRefusals(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"grant_not_found","message":"grant not found"}}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ad := c.AgentDelegations()
	ctx := context.Background()

	_, err = ad.Get(ctx, "a-1", "g-1")
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "grant_not_found" {
		t.Fatalf("got %v, want a 404 grant_not_found APIError", err)
	}
	if err := ad.Revoke(ctx, "a-1", "g-1"); !errors.As(err, &apiErr) || apiErr.Code != "grant_not_found" {
		t.Errorf("revoke: got %v", err)
	}
	calls.Store(0)
	refused := map[string]error{}
	_, refused["create nil"] = ad.Create(ctx, "a-1", nil)
	_, refused["create no agent"] = ad.Create(ctx, "", &CreateAgentDelegationRequest{})
	_, refused["list no agent"] = ad.List(ctx, "")
	_, refused["get no grant"] = ad.Get(ctx, "a-1", "")
	refused["revoke no grant"] = ad.Revoke(ctx, "a-1", "")
	refused["credential revoke no credential"] = ad.RevokeCredential(ctx, "a-1", "g-1", "")
	_, refused["revoke-all no agent"] = ad.RevokeAll(ctx, "")
	_, refused["exchange nil"] = ad.Exchange(ctx, "a-1", "g-1", nil)
	_, refused["exchange no audience"] = ad.Exchange(ctx, "a-1", "g-1", &ExchangeAgentDelegationRequest{})
	for name, err := range refused {
		if err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("refused calls sent %d requests, want 0", n)
	}
}

// TestDelegatedAgentCredential_FormattingOmitsToken: formatting the
// exchange result never prints the credential.
func TestDelegatedAgentCredential_FormattingOmitsToken(t *testing.T) {
	cred := DelegatedAgentCredential{Token: delegatedCredentialTestToken, CredentialID: "c-9", GrantID: "g-1", ExpiresAt: time.Unix(0, 0)}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, cred)
		if strings.Contains(out, "scion_adt_") || strings.Contains(out, "testonlytokenvalue") {
			t.Errorf("%s printed the token: %s", verb, out)
		}
		if !strings.Contains(out, "c-9") {
			t.Errorf("%s does not name the credential: %s", verb, out)
		}
	}
	out := fmt.Sprintf("%v", &cred)
	if strings.Contains(out, "testonlytokenvalue") {
		t.Errorf("pointer formatting printed the token: %s", out)
	}
}
