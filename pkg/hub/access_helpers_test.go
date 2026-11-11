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

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func newLiveModeStore() *liveModeStore {
	return &liveModeStore{proxyAuthStore: newProxyAuthStoreWithRoles()}
}

// newLiveModeServer returns a Server whose access settings start in the given
// mode. It is used as the WebServer's AccessSettingsProvider, as in
// cmd/server_foreground.go.
func newLiveModeServer(st store.Store, mode string) *Server {
	srv := &Server{
		store:       st,
		auditLogger: &LogAuditLogger{},
		maintenance: NewMaintenanceState(false, ""),
	}
	srv.config.UserAccessMode = mode
	return srv
}

// oauthCallbackLogin drives one complete OAuth callback for email on a fresh
// session and returns the redirect location.
func oauthCallbackLogin(t *testing.T, ws *WebServer, email string) string {
	t.Helper()
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockOAuthTransport{
			tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
			userinfoJSON: `{"id":"id-` + email + `","email":"` + email + `","verified_email":true,"name":"Live Mode User"}`,
		},
	}

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	require.NoError(t, err)
	const oauthState = "live-mode-state"
	sess.Values[sessKeyOAuthState] = oauthState
	require.NoError(t, sess.Save(reqSetup, recSetup))
	cookies := recSetup.Result().Cookies()
	require.NotEmpty(t, cookies)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback/google?code=test-code&state="+oauthState, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	return resp.Header.Get("Location")
}

// liveModeStore extends proxyAuthStore with the invite lookup that
// checkUserAuthorized needs in invite_only mode.
type liveModeStore struct {
	*proxyAuthStore
}

func (s *liveModeStore) IsUserInvitedOrActive(_ context.Context, email string) (bool, error) {
	for _, u := range s.users {
		if strings.EqualFold(u.Email, email) &&
			(u.Status == store.UserStatusInvited || u.Status == store.UserStatusActive) {
			return true, nil
		}
	}
	return false, nil
}
