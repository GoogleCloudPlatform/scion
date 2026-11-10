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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// pendingAuditRecorder records pending secret audit events and logs every
// event through the embedded LogAuditLogger.
type pendingAuditRecorder struct {
	*LogAuditLogger
	mu     sync.Mutex
	events []*PendingSecretEvent
}

func (r *pendingAuditRecorder) LogPendingSecretEvent(ctx context.Context, e *PendingSecretEvent) error {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return r.LogAuditLogger.LogPendingSecretEvent(ctx, e)
}

func (r *pendingAuditRecorder) ofType(eventType string) []*PendingSecretEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*PendingSecretEvent
	for _, e := range r.events {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

func recordPendingAudit(srv *Server) *pendingAuditRecorder {
	r := &pendingAuditRecorder{LogAuditLogger: NewLogAuditLogger("[Test]", false)}
	srv.auditLogger = r
	return r
}

// browserOwnerRequest is ownerRequest with request headers and a raw body
// of the given content type ("" sends no body). The Host is hub.example.
func browserOwnerRequest(t *testing.T, srv *Server, ident UserIdentity, method, path, contentType, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "http://hub.example"+path, reader)
	req.Host = "hub.example"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx := contextWithIdentity(req.Context(), ident)
	ctx = context.WithValue(ctx, userContextKey{}, ident)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive})
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func decodeReveal(t *testing.T, rec *httptest.ResponseRecorder) RevealPendingSecretResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp RevealPendingSecretResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// TestPendingSecretsAPI_Reveal: the owner's session gets the held value,
// base64-encoded, with Cache-Control: no-store; the reveal changes
// nothing and is audited without the value.
func TestPendingSecretsAPI_Reveal(t *testing.T) {
	f := newPendingFixture(t, "pend-reveal")
	audit := recordPendingAudit(f.Server)
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_V", userScopeWrite("test-value-reveal", false)))

	rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
		"/api/v1/pending-secrets/"+resp.PendingID+"/reveal", nil)
	got := decodeReveal(t, rec)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, resp.PendingID, got.ID)
	assert.Equal(t, "KEY_V", got.Key)
	assert.Equal(t, store.SecretTypeFile, got.Type)
	assert.Equal(t, "~/.config/tool/creds.json", got.Target)
	assert.Equal(t, "base64", got.Encoding)
	value, err := base64.StdEncoding.DecodeString(got.Value)
	require.NoError(t, err)
	assert.Equal(t, "test-value-reveal", string(value))

	// Nothing changed: the row is still pending and nothing is approved.
	f.pendingRow(t, resp.PendingID)
	_, err = f.Server.secretBackend.GetMeta(context.Background(), "KEY_V", store.ScopeUser, f.UserID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	reveals := audit.ofType(PendingSecretEventReveal)
	require.Len(t, reveals, 1)
	assert.Equal(t, resp.PendingID, reveals[0].PendingID)
	assert.Equal(t, PendingSecretActorUser, reveals[0].ActorKind)
	assert.Equal(t, f.UserID, reveals[0].ActorID)
	assert.Equal(t, f.AgentID, reveals[0].AgentID)

	// GET is not a reveal.
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet,
		"/api/v1/pending-secrets/"+resp.PendingID+"/reveal", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.NotContains(t, rec.Body.String(), "test-value")
}

// TestPendingSecretsAPI_RevealRefusals: agents, user access tokens,
// federated callers, other users, hub admins and the dev credential get no
// value; an expired pending secret is not found.
func TestPendingSecretsAPI_RevealRefusals(t *testing.T) {
	f := newPendingFixture(t, "pend-reveal-refuse")
	audit := recordPendingAudit(f.Server)
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_RV", userScopeWrite("test-value-1", false)))
	path := "/api/v1/pending-secrets/" + resp.PendingID + "/reveal"
	noValue := func(rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		assert.NotEqual(t, http.StatusOK, rec.Code, "%s: %s", what, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "test-value", what)
		assert.NotContains(t, rec.Body.String(), base64.StdEncoding.EncodeToString([]byte("test-value-1")), what)
	}

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, path, nil, f.Token)
	noValue(rec, "writing agent")
	rec = doRequestWithAgentToken(t, f.Server, http.MethodPost, path, nil, f.TokenB)
	noValue(rec, "other agent")

	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindUAT, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	noValue(rec, "user access token")

	fed := NewFederatedUserIdentity("https://issuer.example", f.UserID, f.UserID+"@test.com", "Fed", "member", nil)
	rec = ownerRequest(t, f.Server, fed, CredentialKindFederation, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	noValue(rec, "federated")

	for name, ident := range map[string]UserIdentity{
		"other user": sessionUser(tid("pend-reveal-other")),
		"hub admin":  NewAuthenticatedUser(tid("pend-reveal-admin"), "admin@test.com", "Admin", "admin", "web"),
	} {
		rec = ownerRequest(t, f.Server, ident, CredentialKindInteractive, http.MethodPost, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, name)
		noValue(rec, name)
	}
	rec = doRequest(t, f.Server, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "dev credential")
	noValue(rec, "dev credential")

	assert.Empty(t, audit.ofType(PendingSecretEventReveal), "a refused reveal is not a reveal")

	// Expired: not found, even for the owner.
	_, err := f.Store.CreateOrReplacePendingSecret(context.Background(), &store.PendingSecret{
		Key: "KEY_OLD", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: f.UserID,
		ValueRef: mustHold(t, f.Server, "test-value-old"), ValueLen: 14,
		AgentID: f.AgentID, AgentSlug: "a", ProjectID: f.ProjectID, ProjectName: f.ProjectName,
	}, time.Now().Add(-store.PendingSecretTTL-time.Minute))
	require.NoError(t, err)
	rows, err := f.Store.ListPendingSecretsByOwner(context.Background(), store.ScopeUser, f.UserID)
	require.NoError(t, err)
	for _, row := range rows {
		if row.Key != "KEY_OLD" {
			continue
		}
		rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost,
			"/api/v1/pending-secrets/"+row.ID+"/reveal", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.NotContains(t, rec.Body.String(), "test-value-old")
	}
}

func mustHold(t *testing.T, srv *Server, value string) string {
	t.Helper()
	ref, err := srv.secretBackend.HoldPending(context.Background(), "", []byte(value))
	require.NoError(t, err)
	return ref
}

// TestPendingSecretsAPI_RevealCrossSiteRefused: a reveal from another
// site, or a browser reveal without a JSON body, is refused before the
// value is read, in every auth mode; a same-origin browser reveal and a
// reveal with no browser headers (the CLI) succeed.
func TestPendingSecretsAPI_RevealCrossSiteRefused(t *testing.T) {
	f := newPendingFixture(t, "pend-reveal-xsite")
	audit := recordPendingAudit(f.Server)
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_XS", userScopeWrite("test-value-xs", false)))
	path := "/api/v1/pending-secrets/" + resp.PendingID + "/reveal"
	encoded := base64.StdEncoding.EncodeToString([]byte("test-value-xs"))

	refused := []struct {
		name        string
		contentType string
		body        string
		headers     map[string]string
		code        int
	}{
		{"foreign origin", "", "", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"null origin", "", "", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"sec-fetch-site cross-site", "", "", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"sibling host", "", "", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://other.hub.example"}, http.StatusForbidden},
		{"sibling host without sec-fetch-site", "", "", map[string]string{"Origin": "https://other.hub.example"}, http.StatusForbidden},
		{"browser form body", "application/x-www-form-urlencoded", "a=1", map[string]string{"Origin": "http://hub.example"}, http.StatusUnsupportedMediaType},
		{"browser text body", "text/plain", "{}", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusUnsupportedMediaType},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			rec := browserOwnerRequest(t, f.Server, f.owner(), http.MethodPost, path, tc.contentType, tc.body, tc.headers)
			assert.Equal(t, tc.code, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "test-value")
			assert.NotContains(t, rec.Body.String(), encoded)
		})
	}
	assert.Empty(t, audit.ofType(PendingSecretEventReveal))
	f.pendingRow(t, resp.PendingID)

	allowed := []struct {
		name        string
		contentType string
		body        string
		headers     map[string]string
	}{
		{"same origin, json body", "application/json", "{}", map[string]string{"Origin": "http://hub.example", "Sec-Fetch-Site": "same-origin"}},
		{"same origin, no body", "", "", map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{"no browser headers", "", "", nil},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			rec := browserOwnerRequest(t, f.Server, f.owner(), http.MethodPost, path, tc.contentType, tc.body, tc.headers)
			got := decodeReveal(t, rec)
			assert.Equal(t, encoded, got.Value)
		})
	}
}

// TestPendingSecretsAPI_RevealNoCORSHeaders: through the full middleware
// chain, with the default CORS settings (any origin allowed), the reveal
// route answers a foreign Origin without Access-Control-Allow-* headers
// and without the value, and answers its own origin without them too.
func TestPendingSecretsAPI_RevealNoCORSHeaders(t *testing.T) {
	f := newPendingFixture(t, "pend-reveal-cors")
	require.True(t, f.Server.config.CORSEnabled, "the default config enables CORS")
	ref := mustHold(t, f.Server, "test-value-cors")
	res, err := f.Store.CreateOrReplacePendingSecret(context.Background(), &store.PendingSecret{
		Key: "KEY_CORS", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: DevUserID,
		ValueRef: ref, ValueLen: 15,
		AgentID: f.AgentID, AgentSlug: "a", ProjectID: f.ProjectID, ProjectName: f.ProjectName,
	}, time.Now())
	require.NoError(t, err)
	path := "/api/v1/pending-secrets/" + res.Secret.ID + "/reveal"

	send := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://hub.example"+path, bytes.NewReader([]byte("{}")))
		req.Host = "hub.example"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		f.Server.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := send(map[string]string{"Origin": "https://evil.example"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "test-value")
	for name := range rec.Header() {
		assert.False(t, strings.HasPrefix(name, "Access-Control-Allow-"), "unexpected header %s", name)
	}

	rec = send(map[string]string{"Origin": "http://hub.example", "Sec-Fetch-Site": "same-origin"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	for name := range rec.Header() {
		assert.False(t, strings.HasPrefix(name, "Access-Control-Allow-"), "unexpected header %s", name)
	}
}

// TestPendingSecretsAPI_RevealProxyModeCrossSite: behind an authenticating
// front proxy, the hub-wide write guard refuses a cross-site reveal before
// authentication, through the full middleware chain.
func TestPendingSecretsAPI_RevealProxyModeCrossSite(t *testing.T) {
	f := newPendingFixture(t, "pend-reveal-proxy")
	resp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_PX", userScopeWrite("test-value-px", false)))
	f.Server.authConfig.AuthMode = proxyAuthMode

	req := httptest.NewRequest(http.MethodPost, "http://hub.example/api/v1/pending-secrets/"+resp.PendingID+"/reveal",
		bytes.NewReader([]byte("{}")))
	req.Host = "hub.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	f.Server.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "test-value")
	f.pendingRow(t, resp.PendingID)
}
