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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// E.2a (ptone/scion#2127, plan §3.1(5) and §3.4 item 4): logAuthzDenial
// enrichment (credential_kind, credential_id, request_id) and the
// token-management denial log line.
// ---------------------------------------------------------------------------

func TestLogAuthzDenial_IncludesCredentialAndRequestID(t *testing.T) {
	capture := &capturingHandler{}
	restore := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(restore) })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/x", nil)
	meta := &logging.RequestMeta{RequestID: "logdenial-corr-1"}
	ctx := logging.ContextWithRequestMeta(req.Context(), meta)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindUAT, ID: "logdenial-token-1"})
	req = req.WithContext(ctx)

	identity := NewAuthenticatedUser(tid("logdenial-user"), "u@test.com", "U", "member", "api")
	logAuthzDenial(req, identity, Resource{Type: "project", ID: "x"}, ActionRead, "test reason")

	rec, ok := findRecord(capture.all(), "authorization denied")
	require.True(t, ok)
	attrs := recordAttrs(rec)
	require.Equal(t, string(CredentialKindUAT), attrs["credential_kind"])
	require.Equal(t, "logdenial-token-1", attrs["credential_id"])
	require.Equal(t, "logdenial-corr-1", attrs["request_id"])
}

// TestDenyTokenManagement_LogsAndWrites403 proves a non-session credential
// (a UAT) attempting to manage access tokens is both logged (plan §3.4 item
// 4: "this is where a UAT attempting token management would show up") and
// rejected with 403.
func TestDenyTokenManagement_LogsAndWrites403(t *testing.T) {
	srv, s := testServer(t)
	capture := &capturingHandler{}
	restore := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(restore) })

	projectID, ownerID := setupUATProjectAndOwner(t, s, "denytok")
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/auth/tokens", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	_, ok := findRecord(capture.all(), "authorization denied")
	require.True(t, ok, "a UAT attempting token management must be logged as an authorization denial")
}
