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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

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
