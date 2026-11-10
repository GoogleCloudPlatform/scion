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
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// shellExperimentsFixture is a web server whose mounted Hub API is a real
// hub server, counting the requests that reach GET /api/v1/experiments.
type shellExperimentsFixture struct {
	srv     *Server
	store   store.Store
	ws      *WebServer
	expHits atomic.Int32
}

func newShellExperimentsFixture(t *testing.T) *shellExperimentsFixture {
	t.Helper()
	srv, s := testServerWithOps(t, nil)
	f := &shellExperimentsFixture{srv: srv, store: s}
	f.ws = newTestWebServer(t, WebServerConfig{Host: "127.0.0.1"})
	hub := srv.Handler()
	f.ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/experiments" {
			f.expHits.Add(1)
		}
		hub.ServeHTTP(w, r)
	}), func(ctx context.Context) error { return nil })
	return f
}

// userToken mints a web access token for user, as the OAuth login does.
func (f *shellExperimentsFixture) userToken(t *testing.T, user *store.User) string {
	t.Helper()
	tok, _, _, err := f.srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	return tok
}

// render renders the SPA shell for path. A non-nil user is set as the
// session user (as the session middleware does); a non-empty token is
// stored as the session's hub access token.
func (f *shellExperimentsFixture) render(t *testing.T, path string, user *store.User, token string) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		setupReq := httptest.NewRequest(http.MethodGet, "/", nil)
		setupRec := httptest.NewRecorder()
		sess, err := f.ws.sessionStore.Get(setupReq, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyHubAccessToken] = token
		require.NoError(t, sess.Save(setupReq, setupRec))
		cookies := setupRec.Result().Cookies()
		require.NotEmpty(t, cookies)
		for _, c := range cookies {
			req.AddCookie(c)
		}
	}
	if user != nil {
		req = req.WithContext(setWebSessionUser(req.Context(), &webSessionUser{
			UserID: user.ID, Email: user.Email, Name: user.DisplayName, Role: user.Role,
		}))
	}
	rec := httptest.NewRecorder()
	f.ws.spaHandler()(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return shellInitialData(t, rec.Body.String())
}

// endpointExperiments calls GET /api/v1/experiments on the hub with
// token, returning the status and the raw "experiments" field.
func (f *shellExperimentsFixture) endpointExperiments(t *testing.T, token string) (int, json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/experiments", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Code, body["experiments"]
}

// TestShellExperiments_SignedInEmbedsEndpointMap: a signed-in shell with a
// session hub token carries "experiments", equal to the "experiments" map
// GET /api/v1/experiments returns to the same user, on a page with and
// without prefetched page data.
func TestShellExperiments_SignedInEmbedsEndpointMap(t *testing.T) {
	f := newShellExperimentsFixture(t)
	admin, member := profilingTestUsers(t, f.store)

	for _, u := range []*store.User{member, admin} {
		tok := f.userToken(t, u)
		code, want := f.endpointExperiments(t, tok)
		require.Equal(t, http.StatusOK, code)
		var wantMap map[string]bool
		require.NoError(t, json.Unmarshal(want, &wantMap))
		require.NotEmpty(t, wantMap, "the registry resolves at least one experiment")

		for _, path := range []string{"/projects", "/agents/" + tid("no-such-agent"), "/login"} {
			fields := f.render(t, path, u, tok)
			got, ok := fields["experiments"]
			require.True(t, ok, "%s as %s: experiments embedded", path, u.ID)
			require.JSONEq(t, string(want), string(got), "%s as %s: the endpoint's map", path, u.ID)
		}
	}
}

// TestShellExperiments_AnonymousNeverEmbeds: a shell with no session user,
// or a session user without a hub token, carries no "experiments" and
// does not call the endpoint at all; a token without a session user (no
// signed-in shell) does not either.
func TestShellExperiments_AnonymousNeverEmbeds(t *testing.T) {
	f := newShellExperimentsFixture(t)
	_, member := profilingTestUsers(t, f.store)
	tok := f.userToken(t, member)

	cases := []struct {
		name  string
		user  *store.User
		token string
	}{
		{"no session", nil, ""},
		{"session user without hub token", member, ""},
		{"hub token without session user", nil, tok},
	}
	for _, tc := range cases {
		for _, path := range []string{"/login", "/invite", "/projects", "/"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				before := f.expHits.Load()
				fields := f.render(t, path, tc.user, tc.token)
				require.NotContains(t, fields, "experiments")
				require.Equal(t, before, f.expHits.Load(), "the endpoint is not called")
			})
		}
	}
}

// TestShellExperiments_SameAuthorizationAsEndpoint: for each session
// token, the shell carries "experiments" exactly when GET
// /api/v1/experiments answers that token 200, since the embed is that
// endpoint's own answer: a garbage token and a suspended user's token get
// no map.
func TestShellExperiments_SameAuthorizationAsEndpoint(t *testing.T) {
	f := newShellExperimentsFixture(t)
	_, member := profilingTestUsers(t, f.store)
	suspended := createTestUser(t, f.store, "exp-suspended", "exp-suspended@test.com")
	suspendedTok := f.userToken(t, suspended)
	setUserStatus(t, f.store, suspended.ID, "suspended")

	cases := []struct {
		name  string
		user  *store.User
		token string
	}{
		{"member", member, f.userToken(t, member)},
		{"garbage token", member, "not-a-token"},
		{"suspended user", suspended, suspendedTok},
	}
	sawEmbed, sawRefusal := false, false
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, want := f.endpointExperiments(t, tc.token)
			before := f.expHits.Load()
			fields := f.render(t, "/projects", tc.user, tc.token)
			require.Equal(t, before+1, f.expHits.Load(), "the shell asked the endpoint once")
			got, ok := fields["experiments"]
			if code == http.StatusOK {
				sawEmbed = true
				require.True(t, ok, "endpoint answered 200: embedded")
				require.JSONEq(t, string(want), string(got))
			} else {
				sawRefusal = true
				require.False(t, ok, "endpoint answered %d: not embedded", code)
			}
		})
	}
	require.True(t, sawEmbed, "at least one token is admitted")
	require.True(t, sawRefusal, "at least one token is refused")
}
