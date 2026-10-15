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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// handshakeRefusal decodes a refused handshake's error code and message.
func handshakeRefusal(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code, body.Error.Message
}

// TestConduitHandshake_UpgradeRequired: an admitted agent whose request is
// not a WebSocket upgrade gets 400 before any session exists.
func TestConduitHandshake_UpgradeRequired(t *testing.T) {
	f := newRelayFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	req.Header.Set("X-Scion-Agent-Token", f.agentToken(t, f.launched))
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := handshakeRefusal(t, rec)
	assert.Equal(t, ErrCodeInvalidRequest, code)
	assert.Equal(t, "WebSocket upgrade required", msg)
}

// TestConduitHandshake_AgentGone: a valid agent token for an agent whose
// row is gone gets 401.
func TestConduitHandshake_AgentGone(t *testing.T) {
	f := newRelayFixture(t, nil)
	tok := f.agentToken(t, f.launched)
	require.NoError(t, f.store.DeleteAgent(context.Background(), f.launched.ID))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	req.Header.Set("X-Scion-Agent-Token", tok)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	code, msg := handshakeRefusal(t, rec)
	assert.Equal(t, ErrCodeUnauthorized, code)
	assert.Equal(t, "Agent no longer exists", msg)
}

// userHandshakeRequest is an upgrade request authenticated as identity with
// authType, as the auth middleware leaves it.
func userHandshakeRequest(identity Identity, authType string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = context.WithValue(ctx, logging.AuthTypeKey{}, authType)
	return req.WithContext(ctx)
}

// TestConduitHandshake_CredentialNotAdmitted: a user whose credential kind
// has no expiry rule gets 403 before any upgrade.
func TestConduitHandshake_CredentialNotAdmitted(t *testing.T) {
	f := newRelayFixture(t, nil)
	user := NewAuthenticatedUser(f.owner.ID(), "owner@conduit.test", "Owner", store.UserRoleMember, "api")
	rec := httptest.NewRecorder()
	f.srv.handleConduit(rec, userHandshakeRequest(user, AuthTypeWorkloadDelegation))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, msg := handshakeRefusal(t, rec)
	assert.Equal(t, ErrCodeForbidden, code)
	assert.Equal(t, "This credential cannot open conduit sessions", msg)
}

// TestConduitHandshake_ExpiryReadFailure: a user credential whose expiry
// cannot be read gets 503 before any upgrade (an access token identity
// without its credential id).
func TestConduitHandshake_ExpiryReadFailure(t *testing.T) {
	f := newRelayFixture(t, nil)
	base := NewAuthenticatedUser(f.owner.ID(), "owner@conduit.test", "Owner", store.UserRoleMember, "api")
	scoped := NewScopedUserIdentity(base, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess})
	require.Empty(t, scoped.CredentialID())
	rec := httptest.NewRecorder()
	f.srv.handleConduit(rec, userHandshakeRequest(scoped, AuthTypeUAT))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	code, msg := handshakeRefusal(t, rec)
	assert.Equal(t, ErrCodeUnavailable, code)
	assert.Equal(t, "Unable to read the credential's expiry", msg)
}

// TestConduitServeAdmitted_UnknownKindRefused: an admission of a principal
// kind conduit does not serve is refused, never served as an agent.
func TestConduitServeAdmitted_UnknownKindRefused(t *testing.T) {
	f := newRelayFixture(t, nil)
	rt := f.srv.conduit.Load()
	require.NotNil(t, rt)
	for _, kind := range []string{"relay-peer", "", "service"} {
		t.Run(kind, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			rec := httptest.NewRecorder()
			f.srv.serveConduitAdmitted(rec, req, conduitAdmission{rt: rt, principal: relay.Principal{Kind: kind, ID: f.launched.ID}})
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
}
