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

// Package hubprofiletest provides test fakes for target-hub profiles:
// the metadata emulator, IAM Credentials generateIdToken, and a target
// hub that verifies credentials like the real arms. Test use only.
package hubprofiletest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Test fakes: the metadata emulator (assigned account access token), IAM
// Credentials generateIdToken, and a target hub that verifies the
// credentials the way the real arms do (issuer token email and audience
// plus delegation header on the issuance routes, the IAP principal on every
// request, fixture tokens elsewhere).

const (
	IssuerSA       = "t1-issuer@proj.iam.gserviceaccount.com"
	UATSA          = "t1-uat@proj.iam.gserviceaccount.com"
	IAPAudience    = "1234.apps.googleusercontent.com"
	IssuerAudience = "scion-test-issuer:hub-t1"
	Delegation     = "dlg-member-1"
	AccessToken    = "ACCESS-TOKEN-ASSIGNED-SA"
)

func JWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func NewClock() *Clock { return &Clock{t: time.Now().Truncate(time.Second)} }

func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type GCP struct {
	Emulator *httptest.Server
	IAM      *httptest.Server
	IAMCalls atomic.Int32
	EmuCalls atomic.Int32
	IAMFail  atomic.Bool
	EmuFail  atomic.Bool
	// minted records every ID token the IAM fake returned.
	mu     sync.Mutex
	minted []string
	// emailOverride, when set, is the email put in tokens for that SA.
	EmailOverride map[string]string
}

func NewGCP(t testing.TB, clk *Clock) *GCP {
	t.Helper()
	g := &GCP{EmailOverride: map[string]string{}}
	g.Emulator = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.EmuCalls.Add(1)
		if g.EmuFail.Load() || r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "token generation failed", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": AccessToken, "expires_in": 3600})
	}))
	t.Cleanup(g.Emulator.Close)
	g.IAM = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.IAMCalls.Add(1)
		if g.IAMFail.Load() || r.Header.Get("Authorization") != "Bearer "+AccessToken {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"status":"PERMISSION_DENIED","message":"denied"}}`))
			return
		}
		sa := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/"), ":generateIdToken")
		var body struct {
			Audience string `json:"audience"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		email := sa
		g.mu.Lock()
		if o, ok := g.EmailOverride[sa]; ok {
			email = o
		}
		n := len(g.minted) + 1
		tok := JWT(map[string]any{"iss": "https://accounts.google.com", "sub": "sub-" + sa, "email": email, "aud": body.Audience, "exp": clk.Now().Add(time.Hour).Unix(), "iat": clk.Now().Unix(), "n": n})
		g.minted = append(g.minted, tok)
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
	}))
	t.Cleanup(g.IAM.Close)
	return g
}

func (g *GCP) MintedTokens() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.minted...)
}

// Env returns the environment of an assign-mode agent whose metadata
// emulator is g, plus every ambient hub credential, so a test can show none
// of them is used.
func (g *GCP) Env() map[string]string {
	return map[string]string{
		"SCION_METADATA_MODE": "assign",
		"GCE_METADATA_HOST":   strings.TrimPrefix(g.Emulator.URL, "http://"),
		"SCION_AUTH_TOKEN":    "AMBIENT-SCION-AUTH-TOKEN",
		"SCION_HUB_TOKEN":     "AMBIENT-SCION-HUB-TOKEN",
	}
}

type Identity struct {
	ID, Email, Role, Purpose, Delegation string
	ExpiresAt                            time.Time
	deleted                              bool
}

type Request struct {
	Method, Path string
	Header       http.Header
}

type Target struct {
	Srv       *httptest.Server
	clk       *Clock
	iap       bool
	mu        sync.Mutex
	requests  []Request
	ids       map[string]*Identity
	tokens    map[string]string // fixture token -> identity id
	nextID    int
	tokenTTL  time.Duration
	idLife    time.Duration
	tokenHits int
	// AudienceOverride, when set, replaces the issuer audience the
	// target publishes (set before use).
	AudienceOverride *string
}

func NewTarget(t testing.TB, clk *Clock, iap bool) *Target {
	t.Helper()
	f := &Target{clk: clk, iap: iap, ids: map[string]*Identity{}, tokens: map[string]string{}, tokenTTL: 30 * time.Minute, idLife: time.Hour}
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Srv.Close)
	return f
}

func (f *Target) Recorded() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func bearerOf(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (f *Target) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
	f.mu.Unlock()

	if f.iap {
		c, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(r.Header.Get("Proxy-Authorization"), "Bearer "))
		if err != nil || c.Email != UATSA || !c.HasAudience(IAPAudience) {
			http.Error(w, "IAP: forbidden", http.StatusUnauthorized)
			return
		}
	}
	if r.URL.Path == "/api/v1/test-infra/status" {
		aud := IssuerAudience
		f.mu.Lock()
		if f.AudienceOverride != nil {
			aud = *f.AudienceOverride
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"testIdentities": true, "testHubAdmin": false, "testSuperAdmin": false, "testIssuerAudience": aud})
		return
	}
	for _, h := range []string{"X-Scion-Agent-Token", "X-Scion-Broker-ID", "X-Scion-Federation-Token"} {
		if r.Header.Get(h) != "" {
			apiError(w, http.StatusUnauthorized, "unauthorized", "another credential")
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/test-identities") {
		f.serveIssuance(w, r)
		return
	}
	// Every other route: a fixture token.
	f.mu.Lock()
	id, ok := f.tokens[bearerOf(r)]
	var u *Identity
	if ok {
		u = f.ids[id]
	}
	f.mu.Unlock()
	if r.Header.Get("X-Scion-Issuer-Delegation") != "" || !ok || u == nil || u.deleted {
		apiError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}
	switch r.URL.Path {
	case "/api/v1/auth/me":
		writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "email": u.Email, "role": u.Role})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (f *Target) serveIssuance(w http.ResponseWriter, r *http.Request) {
	c, err := transportauth.DecodeIDTokenClaims(bearerOf(r))
	if err != nil || c.Email != IssuerSA || !c.HasAudience(IssuerAudience) || r.Header.Get("X-Scion-Issuer-Delegation") != Delegation {
		apiError(w, http.StatusUnauthorized, "unauthorized", "workload delegation refused")
		return
	}
	now := f.clk.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/test-identities")
	switch {
	case rest == "" && r.Method == http.MethodPost:
		var req struct {
			Role    string `json:"role"`
			Purpose string `json:"purpose"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.nextID++
		u := &Identity{ID: fmt.Sprintf("fx-%d", f.nextID), Email: fmt.Sprintf("test-identity-%d@scion-fixture.invalid", f.nextID), Role: req.Role, Purpose: req.Purpose, Delegation: Delegation, ExpiresAt: now.Add(f.idLife)}
		f.ids[u.ID] = u
		writeJSON(w, http.StatusCreated, f.tokenResponseLocked(u, now))
	case rest == "" && r.Method == http.MethodGet:
		items := []map[string]any{}
		for _, u := range f.ids {
			if !u.deleted && u.Delegation == Delegation && (now.Before(u.ExpiresAt) || r.URL.Query().Get("includeExpired") == "true") {
				items = append(items, identityView(u, now))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": false})
	case strings.HasSuffix(rest, "/token") && r.Method == http.MethodPost:
		u := f.ids[strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/token")]
		if u == nil || u.deleted {
			apiError(w, http.StatusNotFound, "not_found", "test identity not found")
			return
		}
		f.tokenHits++
		writeJSON(w, http.StatusOK, f.tokenResponseLocked(u, now))
	case r.Method == http.MethodDelete:
		u := f.ids[strings.TrimPrefix(rest, "/")]
		if u == nil || u.deleted {
			apiError(w, http.StatusNotFound, "not_found", "test identity not found")
			return
		}
		u.deleted = true
		if r.URL.Query().Get("purge") == "true" {
			writeJSON(w, http.StatusOK, map[string]any{"identityId": u.ID, "complete": true, "identityDeleted": true, "deleted": map[string]int{}})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		apiError(w, http.StatusNotFound, "not_found", "no route")
	}
}

func identityView(u *Identity, now time.Time) map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "role": u.Role, "status": "active", "purpose": u.Purpose, "issuedBy": "grantor-1", "issuerDelegationId": u.Delegation, "expiresAt": u.ExpiresAt, "created": now, "live": now.Before(u.ExpiresAt)}
}

func (f *Target) tokenResponseLocked(u *Identity, now time.Time) map[string]any {
	exp := now.Add(f.tokenTTL)
	if exp.After(u.ExpiresAt) {
		exp = u.ExpiresAt
	}
	tok := JWT(map[string]any{"uid": u.ID, "email": u.Email, "role": u.Role, "type": "access", "exp": exp.Unix(), "n": len(f.tokens) + 1, "secret": "FIXTURE-TOKEN"})
	f.tokens[tok] = u.ID
	return map[string]any{"identity": identityView(u, now), "accessToken": tok, "tokenType": "Bearer", "expiresIn": int64(exp.Sub(now).Seconds()), "tokenExpiresAt": exp}
}

// Expire moves identity id's expiry into the past (it stays on the hub,
// as before the reaper runs).
func (f *Target) Expire(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.ids[id]; u != nil {
		u.ExpiresAt = f.clk.Now().Add(-time.Minute)
	}
}

// TokenHits returns how many token re-issues the target served.
func (f *Target) TokenHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenHits
}

// Purpose returns the recorded purpose of identity id.
func (f *Target) Purpose(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.ids[id]; u != nil {
		return u.Purpose
	}
	return ""
}

// FixtureTokens returns every fixture token the target issued.
func (f *Target) FixtureTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for tok := range f.tokens {
		out = append(out, tok)
	}
	return out
}

// LiveIdentityIDs returns the ids of the identities not deleted.
func (f *Target) LiveIdentityIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, u := range f.ids {
		if !u.deleted {
			out = append(out, id)
		}
	}
	return out
}
