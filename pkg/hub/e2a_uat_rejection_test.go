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
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// E.2a (ptone/scion#2127, plan §3.1(3), rulings Q3 and plan correction (b)):
// rejected-UAT logging. A rejection may identify a server-verified matched
// credential record, but must mark it as a rejection, not an authenticated
// principal — and an unrecognized bearer value must never yield an asserted
// identity, not even a rejected one. The presented token string is never
// logged.
// ---------------------------------------------------------------------------

// installRejectionLogging wires both the auth-rejection logger and the
// request logger to the same capturingHandler, so a "credential rejected"
// test can assert on request_id (which only exists once a request logger
// installs *logging.RequestMeta) alongside the rejection reason.
func installRejectionLogging(srv *Server) *capturingHandler {
	capture := &capturingHandler{}
	logger := slog.New(capture)
	srv.authConfig.Logger = logger
	srv.SetRequestLogger(logger)
	return capture
}

func TestUATRejection_UnknownToken_NoIdentityAsserted(t *testing.T) {
	srv, _ := testServer(t)
	capture := installRejectionLogging(srv)

	presented := "scion_pat_" + "unknown-value-not-in-store"
	rr := doRequestWithBearer(srv, presented)
	require.Equal(t, http.StatusUnauthorized, rr.Code)

	rec, ok := findRecord(capture.all(), "credential rejected")
	require.True(t, ok, "expected a 'credential rejected' log line")
	attrs := recordAttrs(rec)
	require.Equal(t, "invalid", attrs["reason"])
	_, hasID := attrs["credential.id"]
	require.False(t, hasID, "an unrecognized bearer value must not surface a credential.id: %v", attrs)
	_, hasUserID := attrs["user_id"]
	require.False(t, hasUserID, "an unrecognized bearer value must not surface a user_id: %v", attrs)
	_, hasPrincipalKind := attrs["principal_kind"]
	require.False(t, hasPrincipalKind, "an unrecognized bearer value must not surface a principal_kind: %v", attrs)

	requestID, hasRequestID := attrs["request_id"]
	require.True(t, hasRequestID, "the rejection line must carry request_id: %v", attrs)
	require.Equal(t, rr.Header().Get("X-Request-ID"), requestID, "request_id must match the X-Request-ID response header")

	// The response line itself (emitted by RequestLogMiddleware) must also
	// carry no identity for this request.
	last := recordAttrs(capture.all()[len(capture.all())-1])
	_, hasUserIDOnResponse := last["user_id"]
	require.False(t, hasUserIDOnResponse, "the response line must not surface a user_id for a rejected request: %v", last)
	_, hasPrincipalKindOnResponse := last["principal_kind"]
	require.False(t, hasPrincipalKindOnResponse, "the response line must not surface a principal_kind for a rejected request: %v", last)
}

func TestUATRejection_RevokedToken_IdentifiesMatchedRecordAsRejection(t *testing.T) {
	srv, s := testServer(t)
	capture := installRejectionLogging(srv)

	projectID, ownerID := setupUATProjectAndOwner(t, s, "rej-revoked")
	key, token, err := srv.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "revoke-me", projectID, []string{"project:read"}, nil)
	require.NoError(t, err)
	require.NoError(t, srv.uatService.RevokeToken(rs4MintContext(ownerID), ownerID, token.ID))

	rr := doRequestWithBearer(srv, key)
	require.Equal(t, http.StatusUnauthorized, rr.Code)

	rec, ok := findRecord(capture.all(), "credential rejected")
	require.True(t, ok)
	attrs := recordAttrs(rec)
	require.Equal(t, "revoked", attrs["reason"])
	require.Equal(t, token.ID, attrs["credential.id"], "a revoked token's rejection may identify the matched record")
	require.Equal(t, rr.Header().Get("X-Request-ID"), attrs["request_id"])
}

func TestUATRejection_ExpiredToken(t *testing.T) {
	srv, s := testServer(t)
	capture := installRejectionLogging(srv)

	projectID, ownerID := setupUATProjectAndOwner(t, s, "rej-expired")
	past := time.Now().Add(-time.Hour)
	// Mint through the store directly since CreateToken rejects a past expiry.
	presented := "scion_pat_expired-key-body"
	hash := sha256.Sum256([]byte(presented))
	tok := &store.UserAccessToken{
		ID: tid("rej-expired-token"), UserID: ownerID, Name: "expired",
		Prefix: "scion_pat_exp1", KeyHash: hex.EncodeToString(hash[:]),
		ProjectID: projectID, Scopes: []string{"project:read"}, ExpiresAt: &past, Created: time.Now(),
	}
	require.NoError(t, s.CreateUserAccessToken(context.Background(), tok))

	rr := doRequestWithBearer(srv, "scion_pat_expired-key-body")
	require.Equal(t, http.StatusUnauthorized, rr.Code)

	rec, ok := findRecord(capture.all(), "credential rejected")
	require.True(t, ok)
	attrs := recordAttrs(rec)
	require.Equal(t, "expired", attrs["reason"])
	require.Equal(t, tok.ID, attrs["credential.id"])
	require.Equal(t, rr.Header().Get("X-Request-ID"), attrs["request_id"])
}

func TestUATRejection_SuspendedUser(t *testing.T) {
	srv, s := testServer(t)
	capture := installRejectionLogging(srv)

	projectID, ownerID := setupUATProjectAndOwner(t, s, "rej-suspended")
	key, token, err := srv.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "will-suspend", projectID, []string{"project:read"}, nil)
	require.NoError(t, err)

	owner, err := s.GetUser(context.Background(), ownerID)
	require.NoError(t, err)
	owner.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(context.Background(), owner))

	rr := doRequestWithBearer(srv, key)
	require.Equal(t, http.StatusForbidden, rr.Code)

	rec, ok := findRecord(capture.all(), "credential rejected")
	require.True(t, ok)
	attrs := recordAttrs(rec)
	require.Equal(t, "user_suspended", attrs["reason"])
	require.Equal(t, token.ID, attrs["credential.id"])
	require.Equal(t, rr.Header().Get("X-Request-ID"), attrs["request_id"])
}

func TestUATRejection_NeverLogsPresentedToken(t *testing.T) {
	srv, _ := testServer(t)
	capture := &capturingHandler{}
	srv.authConfig.Logger = slog.New(capture)

	presented := "scion_pat_super-secret-presented-value"
	rr := doRequestWithBearer(srv, presented)
	require.Equal(t, http.StatusUnauthorized, rr.Code)

	for _, r := range capture.all() {
		for _, v := range recordAttrs(r) {
			if s, ok := v.(string); ok {
				require.NotContains(t, s, "super-secret-presented-value")
			}
		}
	}
}
