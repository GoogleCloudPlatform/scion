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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

const adminResetAuthAllPath = "/api/v1/admin/agents/reset-auth-all"

// TestAdminResetAuthAll_TokenRefused: a caller without a user session is
// refused on the bulk agent auth-reset route, even a hub super-admin, with
// the session-only reason, before the handler runs.
func TestAdminResetAuthAll_TokenRefused(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin, err := s.GetUser(t.Context(), hubConfigTokenUser(t, s, "reset-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	key := mintHubConfigToken(t, srv, admin.ID, hubBoundary(), "hub_config:read", "hub_config:update")

	for _, body := range []any{nil, map[string]any{"reissue_scopes": true}} {
		rec := doRequestWithToken(t, srv, key, http.MethodPost, adminResetAuthAllPath, body)
		require.Equal(t, http.StatusForbidden, rec.Code, "body %v: %s", body, rec.Body.String())
		code, details := errorCodeAndDetails(t, rec)
		require.Equal(t, ErrCodeForbidden, code, rec.Body.String())
		require.Equal(t, string(authzop.ReasonSessionRecovery), details["reason"], rec.Body.String())
		require.Equal(t, sessionRequiredCredential, details["credential"], rec.Body.String())
	}
}

// TestAdminResetAuthAll_SessionPassesGuard: a hub super-admin session
// passes the session-only guard and reaches the handler.
func TestAdminResetAuthAll_SessionPassesGuard(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin, err := s.GetUser(t.Context(), hubConfigTokenUser(t, s, "reset-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	// A body the handler rejects proves the request got past the guard
	// without dispatching any reset.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, adminResetAuthAllPath, map[string]any{"dry_run": true})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "dry_run applies only with reissue_scopes")
}

// TestAdminResetAuthAll_DevCredentialPassesGuard: the local development
// credential, which the session-only guard admits alongside a session,
// passes the guard and reaches the handler.
func TestAdminResetAuthAll_DevCredentialPassesGuard(t *testing.T) {
	srv, _ := testServerWithOps(t, nil)

	rec := doRequest(t, srv, http.MethodPost, adminResetAuthAllPath, map[string]any{"dry_run": true})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "dry_run applies only with reissue_scopes")
}
