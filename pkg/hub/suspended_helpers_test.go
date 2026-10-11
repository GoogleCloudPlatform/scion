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

func loginSession(t *testing.T, ws *WebServer, userID, email, role string) []*http.Cookie {
	t.Helper()

	// Create a synthetic request and set up the session.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	session, err := ws.sessionStore.Get(req, webSessionName)
	require.NoError(t, err)

	session.Values[sessKeyUserID] = userID
	session.Values[sessKeyUserEmail] = email
	session.Values[sessKeyUserName] = "Test User"
	session.Values[sessKeyUserAvatar] = ""
	session.Values[sessKeyUserRole] = role

	require.NoError(t, session.Save(req, rec))

	return rec.Result().Cookies()
}
