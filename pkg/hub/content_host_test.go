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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase A (ptone/scion#7107): the hub wiring of the content host
// (server.hub.content_url). The guard's own behaviour is tested in
// pkg/artifacts; these tests pin that the hub feeds it the value in effect,
// puts it outermost (before authentication and the web server's session
// bridge), strips the hub's credential headers, logs refused requests with
// the capability redacted, and builds shareUrl and the view CSP from
// configuration.

const (
	loTestOrigin    = "https://links.example.net"
	loTestHost      = "links.example.net"
	loTestHubOrigin = "https://hub.example.com"
	loTestHubHost   = "hub.example.com"
)

// contentHostServer is a hub with artifacts on, its public origin set to
// loTestHubOrigin and server.hub.content_url set to contentURL, as an
// apply from the endpoints section would set it.
func contentHostServer(t *testing.T, contentURL string) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	srv.config.HubEndpoints = &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin}
	srv.SetOperationalSettings(artifactsOps(t, `{}`))
	setContentURLForTest(srv, contentURL)
	return srv, s
}

// setContentURLForTest applies contentURL as ApplySnapshot does.
func setContentURLForTest(srv *Server, contentURL string) {
	srv.contentURLLive.Store(&contentURL)
}

// assertContentHostHeaders checks the content host's fixed response
// headers (C1-1, K44).
func assertContentHostHeaders(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	assert.Equal(t, []string{"no-referrer"}, rec.Header().Values("Referrer-Policy"), "%s: Referrer-Policy", what)
	assert.Equal(t, []string{"no-store"}, rec.Header().Values("Cache-Control"), "%s: Cache-Control", what)
	assert.Equal(t, []string{"*"}, rec.Header().Values("Access-Control-Allow-Origin"), "%s: Access-Control-Allow-Origin", what)
	assert.Empty(t, rec.Header().Values("Access-Control-Allow-Credentials"), "%s: Access-Control-Allow-Credentials", what)
}

// hostRequest serves a request on host through the full hub handler.
func hostRequest(srv *Server, method, host, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func bearerFor(t *testing.T, srv *Server, user *store.User) string {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	return "Bearer " + token
}

// contentHostFixture publishes an artifact as its owner and creates a share
// link on the hub Host.
func contentHostFixture(t *testing.T, srv *Server, s store.Store, slug string) (owner *store.User, id string, created artifacts.CreateLinkResponse) {
	t.Helper()
	ctx := context.Background()
	p := artifactProject(t, s, slug)
	createTestUserWithProjectRole(t, s, tid(slug+"-owner"), slug+"-owner@test.com", p.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, tid(slug+"-owner"))
	require.NoError(t, err)
	rec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=report.md&scope="+p.ID, []byte("# report\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id = decodeArtifactID(t, rec)
	rec = userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts/"+id+"/links", nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	return owner, id, created
}

// TestArtifactsContentHostOnRoutes: with content_url set, the create
// response carries shareUrl on it; on that Host the API, the web app root
// and port view answer 404 even for the owner, while the share link
// redirects and its view serves with the content-host CSP. The hub Host is
// unchanged. Conditions 1, 2 and 6.
func TestArtifactsContentHostOnRoutes(t *testing.T) {
	srv, s := contentHostServer(t, loTestOrigin)
	owner, id, created := contentHostFixture(t, srv, s, "ch-routes")
	auth := map[string]string{"Authorization": bearerFor(t, srv, owner)}

	assert.True(t, strings.HasPrefix(created.URL, artifacts.RouteShared), created.URL)
	assert.Equal(t, loTestOrigin+created.URL, created.ShareURL)

	for _, rq := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/artifacts/" + id},
		{http.MethodPost, "/api/v1/artifacts/" + id + "/links"},
		{http.MethodGet, "/api/v1/projects"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/api/v1/portview/abc/"},
		{http.MethodGet, "/api/v1/artifacts/shared/../projects"},
		{http.MethodGet, "/api/v1/artifacts/view/x/%2e%2e/y"},
		{http.MethodGet, "/api/v1/artifacts/shared%2Ftok"},
		{http.MethodPost, created.URL},
	} {
		rec := hostRequest(srv, rq.method, loTestHost, rq.target, nil, auth)
		assert.Equal(t, http.StatusNotFound, rec.Code, "content host %s %s", rq.method, rq.target)
		assertContentHostHeaders(t, rec, "404 "+rq.target)
	}
	rec := hostRequest(srv, http.MethodGet, loTestHubHost, "/api/v1/artifacts/"+id, nil, auth)
	assert.Equal(t, http.StatusOK, rec.Code, "hub Host artifact read")

	shared := hostRequest(srv, http.MethodGet, loTestHost, created.URL, nil, nil)
	require.Equal(t, http.StatusSeeOther, shared.Code, shared.Body.String())
	assertContentHostHeaders(t, shared, "share 303")
	loc := shared.Header().Get("Location")
	require.True(t, strings.HasPrefix(loc, artifacts.RouteView), loc)
	view := hostRequest(srv, http.MethodGet, loTestHost, loc, nil, nil)
	require.Equal(t, http.StatusOK, view.Code, view.Body.String())
	assert.Contains(t, view.Header().Get("Content-Security-Policy"), "script-src "+loTestHost+artifacts.RouteView)
	assertContentHostHeaders(t, view, "view 200")

	view = hostRequest(srv, http.MethodGet, "unrelated.example.org", loc, nil, nil)
	require.Equal(t, http.StatusOK, view.Code)
	csp := view.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "script-src "+loTestHubHost+artifacts.RouteView)
	assert.NotContains(t, csp, "unrelated.example.org")
}

// TestArtifactsContentHostForwardedHeaders: detection uses the Host the hub
// receives. A hub-Host request naming the content host in
// X-Forwarded-Host or Forwarded is served as a hub request (test A); a
// content-host request naming the hub there is still confined (test B).
// Condition 2 [r2], kill check K14.
func TestArtifactsContentHostForwardedHeaders(t *testing.T) {
	srv, s := contentHostServer(t, loTestOrigin)
	owner, id, created := contentHostFixture(t, srv, s, "ch-fwd")
	bearer := bearerFor(t, srv, owner)

	for _, fwd := range []map[string]string{
		{"X-Forwarded-Host": loTestHost},
		{"Forwarded": "host=" + loTestHost},
	} {
		fwd["Authorization"] = bearer
		rec := hostRequest(srv, http.MethodGet, loTestHubHost, "/api/v1/artifacts/"+id, nil, fwd)
		assert.Equal(t, http.StatusOK, rec.Code, "test A %v", fwd)
		shared := hostRequest(srv, http.MethodGet, loTestHubHost, created.URL, nil, fwd)
		require.Equal(t, http.StatusSeeOther, shared.Code)
		view := hostRequest(srv, http.MethodGet, loTestHubHost, shared.Header().Get("Location"), nil, fwd)
		require.Equal(t, http.StatusOK, view.Code)
		assert.Contains(t, view.Header().Get("Content-Security-Policy"), "script-src "+loTestHubHost+artifacts.RouteView, "test A %v", fwd)
	}

	fwd := map[string]string{"X-Forwarded-Host": loTestHubHost, "Authorization": bearer}
	rec := hostRequest(srv, http.MethodGet, loTestHost, "/api/v1/artifacts/"+id, nil, fwd)
	assert.Equal(t, http.StatusNotFound, rec.Code, "test B")
	shared := hostRequest(srv, http.MethodGet, loTestHost, created.URL, nil, fwd)
	require.Equal(t, http.StatusSeeOther, shared.Code)
	view := hostRequest(srv, http.MethodGet, loTestHost, shared.Header().Get("Location"), nil, fwd)
	require.Equal(t, http.StatusOK, view.Code)
	assert.Contains(t, view.Header().Get("Content-Security-Policy"), "script-src "+loTestHost+artifacts.RouteView, "test B")
}

// TestArtifactsContentHostOutermost: the guard runs before authentication,
// so a handler behind the hub's whole middleware chain sees no identity
// and no credential header on the content host, whatever the request
// carried; on the hub Host the same credentials authenticate. Condition 3,
// kill checks K5, K6 and K7.
func TestArtifactsContentHostOutermost(t *testing.T) {
	srv, s := contentHostServer(t, loTestOrigin)
	ctx := context.Background()
	p := artifactProject(t, s, "ch-outer")
	createTestUserWithProjectRole(t, s, tid("ch-outer-user"), "ch-outer-user@test.com", p.ID, store.ProjectRoleMember)
	user, err := s.GetUser(ctx, tid("ch-outer-user"))
	require.NoError(t, err)
	_, agentTok := artifactAgent(t, srv, s, p.ID, "ch-outer-agent", AgentRoleBaseline)

	var mu sync.Mutex
	var sawIdentity Identity
	var sawHeaders http.Header
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawIdentity = GetIdentityFromContext(r.Context())
		sawHeaders = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	h := srv.applyMiddleware(probe)
	serve := func(host string, hdr map[string]string) (Identity, http.Header) {
		r := httptest.NewRequest(http.MethodGet, artifacts.RouteView+"cap/index.html", nil)
		r.Host = host
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		mu.Lock()
		sawIdentity, sawHeaders = nil, nil
		mu.Unlock()
		h.ServeHTTP(httptest.NewRecorder(), r)
		mu.Lock()
		defer mu.Unlock()
		return sawIdentity, sawHeaders
	}

	for name, hdr := range map[string]map[string]string{
		"bearer":      {"Authorization": bearerFor(t, srv, user)},
		"agent token": {"X-Scion-Agent-Token": agentTok},
	} {
		id, _ := serve(loTestHubHost, hdr)
		assert.NotNil(t, id, "%s on the hub Host: no identity", name)

		id, headers := serve(loTestHost, hdr)
		assert.Nil(t, id, "%s on the content host: the handler saw an identity", name)
		for k := range hdr {
			assert.Empty(t, headers.Values(k), "%s on the content host: %s reached the handler", name, k)
		}
	}

	// Every hub credential header is removed on the content host; other
	// headers pass.
	creds := map[string]string{
		"Cookie":                    "scion_sess=x",
		"X-Scion-Federation-Token":  "f",
		"X-Scion-Broker-ID":         "b",
		"X-Scion-Signature":         "s",
		"X-Scion-On-Behalf-Of":      "o",
		"X-Scion-Issuer-Delegation": "d",
		"X-Forwarded-User-Email":    "e@test.com",
		IAPAssertionHeader:          "a",
		DefaultJWTHeader:            "j",
	}
	sent := map[string]string{"Accept": "text/html"}
	for k, v := range creds {
		sent[k] = v
	}
	_, headers := serve(loTestHost, sent)
	for k := range creds {
		assert.Empty(t, headers.Values(k), "credential header %s reached the handler", k)
	}
	assert.Equal(t, "text/html", headers.Get("Accept"))
}

// TestArtifactsContentHostSessionCookie: through the web server, a signed-in
// session cookie sent to the content host never reaches the hub as a
// credential, and the web app is not served there; on the hub Host the
// session reaches the hub as before. Condition 3, kill check K6.
func TestArtifactsContentHostSessionCookie(t *testing.T) {
	ws := newDevAuthWebServer(t)
	var mu sync.Mutex
	var calls int
	var sawAuth, sawCookie string
	ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		sawAuth, sawCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}), func(context.Context) error { return nil })
	ws.SetContentHostGuard(artifacts.ContentHostGuard(func() string { return loTestOrigin }, servesOnContentHost, nil, nil))
	cookies := devSignIn(t, ws)
	require.NotEmpty(t, cookies)
	h := ws.Handler()
	serve := func(host, target string) int {
		r := withCookies(httptest.NewRequest(http.MethodGet, target, nil), cookies...)
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	assert.Equal(t, http.StatusNotFound, serve(loTestHost, "/"), "web app on the content host")
	assert.Equal(t, http.StatusNotFound, serve(loTestHost, "/auth/me"), "auth route on the content host")

	require.Equal(t, http.StatusOK, serve(loTestHubHost, artifacts.RouteShared+"tok"))
	mu.Lock()
	hubAuth, hubCookie := sawAuth, sawCookie
	mu.Unlock()
	assert.True(t, hubAuth != "" || hubCookie != "", "hub Host: the session did not reach the hub")

	require.Equal(t, http.StatusOK, serve(loTestHost, artifacts.RouteShared+"tok"))
	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, sawAuth, "content host: the session became a bearer token")
	assert.Empty(t, sawCookie, "content host: the session cookie reached the hub")
}

// TestArtifactsContentHostFallbacks: an empty or invalid content_url, or
// one on the hub's own host name at any scheme or port, leaves links on the
// hub origin (no shareUrl), gives HubEndpoints().Content "" and confines
// nothing. A same-site value is used, with a warning. Condition 5, K21.
func TestArtifactsContentHostFallbacks(t *testing.T) {
	for _, v := range []string{
		"",
		"https://links.example.net/path",
		"http://links.example.net",
		loTestHubOrigin,
		"https://hub.example.com:8443",
		"https://HUB.example.com",
		"http://hub.example.com",
	} {
		t.Run(v, func(t *testing.T) {
			srv, s := contentHostServer(t, v)
			owner, id, created := contentHostFixture(t, srv, s, "ch-fb")
			assert.Empty(t, created.ShareURL)
			assert.True(t, strings.HasPrefix(created.URL, artifacts.RouteShared))
			assert.Empty(t, srv.HubEndpoints().Content)
			rec := hostRequest(srv, http.MethodGet, loTestHost, "/api/v1/artifacts/"+id, nil,
				map[string]string{"Authorization": bearerFor(t, srv, owner)})
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
	// A same-site value is used (with a warning).
	srv, _ := contentHostServer(t, "https://links.example.com")
	assert.Equal(t, "https://links.example.com", srv.HubEndpoints().Content)
}

// TestArtifactsContentHostStartupValue: before any settings apply, the
// value configured at startup (ServerConfig.ContentURL, from settings.yaml
// or SCION_SERVER_HUB_CONTENTURL) is in effect, normalized; an apply then
// replaces it, and an applied "" clears it.
func TestArtifactsContentHostStartupValue(t *testing.T) {
	srv := &Server{config: ServerConfig{
		ContentURL:   "HTTPS://Links.Example.NET:443/",
		HubEndpoints: &HubEndpoints{Operator: loTestHubOrigin, Public: loTestHubOrigin},
	}}
	assert.Equal(t, loTestOrigin, srv.HubEndpoints().Content)
	setContentURLForTest(srv, "https://other.example.net")
	assert.Equal(t, "https://other.example.net", srv.HubEndpoints().Content)
	setContentURLForTest(srv, "")
	assert.Empty(t, srv.HubEndpoints().Content)
}

// TestArtifactsContentHostWebServerFromProvider: a combined server whose
// access settings provider is the hub applies the hub's guard without an
// explicit SetContentHostGuard, so the web app is not served on the link
// origin. Review O1.
func TestArtifactsContentHostWebServerFromProvider(t *testing.T) {
	srv, _ := contentHostServer(t, loTestOrigin)
	ws := newDevAuthWebServer(t)
	ws.SetAccessSettingsProvider(srv)
	ws.MountHubAPI(srv.Handler(), func(context.Context) error { return nil })
	h := ws.Handler()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = loTestHost
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	assert.Equal(t, http.StatusNotFound, rec.Code, "GET / on the content host")
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = loTestHubHost
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	assert.NotEqual(t, http.StatusNotFound, rec.Code, "GET / on the hub host")
}

// TestArtifactsContentHostWebServerStartFailsClosed: a web server that
// mounts the hub API with no content host guard (no hub provider,
// no SetContentHostGuard) refuses to start, before it binds a listener.
func TestArtifactsContentHostWebServerStartFailsClosed(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{Host: "127.0.0.1", Port: 0})
	ws.MountHubAPI(http.NotFoundHandler(), func(context.Context) error { return nil })
	// A cancelled context: should Start ever serve here, it shuts down and
	// returns at once, so the assertions below fail instead of hanging.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Cleanup(func() {
		if ws.httpServer != nil {
			_ = ws.httpServer.Close()
		}
	})
	err := ws.Start(ctx)
	require.ErrorIs(t, err, errContentHostGuardMissing)
	assert.Nil(t, ws.httpServer, "the server was built despite the missing guard")
}

// TestContentHostRouteFamilies: the content host serves exactly the
// artifact share and view families; every other family, the port view
// among them, is not in the table (condition 2; the port family belongs to
// the proxy lane's phase).
func TestContentHostRouteFamilies(t *testing.T) {
	var names []string
	for _, f := range contentHostRouteFamilies {
		names = append(names, f.name)
	}
	assert.Equal(t, []string{"artifact-share", "artifact-view"}, names)
	for _, rq := range []struct {
		method, target string
		want           bool
	}{
		{http.MethodGet, artifacts.RouteShared + "tok", true},
		{http.MethodGet, artifacts.RouteView + "cap/index.html", true},
		{http.MethodGet, "/api/v1/portview/cap/", false},
		{http.MethodGet, "/api/v1/agents/a/ports/8080/proxy/", false},
		{http.MethodGet, "/api/v1/artifacts", false},
		{http.MethodPost, artifacts.RouteShared + "tok", false},
	} {
		r := httptest.NewRequest(rq.method, rq.target, nil)
		assert.Equal(t, rq.want, servesOnContentHost(r), "%s %s", rq.method, rq.target)
	}
}

// TestContentHostLogsRedacted: requests on the content host are logged with
// the capability redacted, a refused one (answered by the guard, outside
// the request logger) included. C1-2, K45.
func TestContentHostLogsRedacted(t *testing.T) {
	srv, s := contentHostServer(t, loTestOrigin)
	_, _, created := contentHostFixture(t, srv, s, "ch-log")
	token := strings.TrimPrefix(created.URL, artifacts.RouteShared)
	require.NotEmpty(t, token)

	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&contentHostLockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	shared := hostRequest(srv, http.MethodGet, loTestHost, created.URL, nil, nil)
	require.Equal(t, http.StatusSeeOther, shared.Code)
	viewPath := shared.Header().Get("Location")
	capability := strings.SplitN(strings.TrimPrefix(viewPath, artifacts.RouteView), "/", 2)[0]
	require.NotEmpty(t, capability)
	refused := hostRequest(srv, http.MethodPost, loTestHost, viewPath, nil, nil)
	require.Equal(t, http.StatusNotFound, refused.Code)
	refused = hostRequest(srv, http.MethodPost, loTestHost, created.URL, nil, nil)
	require.Equal(t, http.StatusNotFound, refused.Code)

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	assert.Contains(t, out, "Content host: request refused", "the refused request left no log line")
	assert.NotContains(t, out, token, "a share token reached the log")
	assert.NotContains(t, out, capability, "a view capability reached the log")
}

// contentHostLockedWriter serializes writes to w.
type contentHostLockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *contentHostLockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestHubSessionCookiesHaveNoDomain: every cookie the web server sets at
// sign-in is host-only (no Domain attribute), and the session store's
// options set none, so a content host under a parent domain never receives
// the hub session. No setting widens the cookie domain, so the startup
// refuse-or-warn check of condition CK has nothing to check. CK, K46.
func TestHubSessionCookiesHaveNoDomain(t *testing.T) {
	ws := newDevAuthWebServer(t)
	cookies := devSignIn(t, ws)
	require.NotEmpty(t, cookies)
	for _, c := range cookies {
		assert.Empty(t, c.Domain, "cookie %s carries Domain", c.Name)
	}
	require.NotNil(t, ws.sessionStore)
	require.NotNil(t, ws.sessionStore.Options)
	assert.Empty(t, ws.sessionStore.Options.Domain, "session store options set Domain")
}

// TestContentHostCORS: with the hub's CORS on and every origin allowed, a
// view on the content host answers Access-Control-Allow-Origin: * and never
// echoes the requester's Origin or admits credentials; the hub Host keeps
// the hub's CORS answer. CORS, K32.
func TestContentHostCORS(t *testing.T) {
	srv, s := contentHostServer(t, loTestOrigin)
	srv.config.CORSEnabled = true
	srv.config.CORSAllowedOrigins = []string{"*"}
	_, _, created := contentHostFixture(t, srv, s, "ch-cors")
	origin := map[string]string{"Origin": "https://evil.example.org"}
	shared := hostRequest(srv, http.MethodGet, loTestHost, created.URL, nil, origin)
	require.Equal(t, http.StatusSeeOther, shared.Code)
	view := hostRequest(srv, http.MethodGet, loTestHost, shared.Header().Get("Location"), nil, origin)
	require.Equal(t, http.StatusOK, view.Code, view.Body.String())
	for _, rec := range []*httptest.ResponseRecorder{shared, view} {
		assert.Equal(t, []string{"*"}, rec.Header().Values("Access-Control-Allow-Origin"))
		assert.Empty(t, rec.Header().Values("Access-Control-Allow-Credentials"))
	}
	hub := hostRequest(srv, http.MethodGet, loTestHubHost, shared.Header().Get("Location"), nil, origin)
	require.Equal(t, http.StatusOK, hub.Code)
	assert.Equal(t, "https://evil.example.org", hub.Header().Get("Access-Control-Allow-Origin"), "hub Host: the hub's CORS answer")
}

// TestArtifactsContentHostAgentEndpointConflict: a content URL on the host
// name of server.hub.agent_endpoint, whatever the scheme or port, is
// ignored at runtime (the guard never confines the agent endpoint), also
// when the agent endpoint changes after the value was set.
func TestArtifactsContentHostAgentEndpointConflict(t *testing.T) {
	srv, _ := contentHostServer(t, "https://agents.example.net")
	assert.Equal(t, "https://agents.example.net", srv.HubEndpoints().Content)
	srv.config.AgentEndpoint = "http://agents.example.net:9810"
	assert.Empty(t, srv.HubEndpoints().Content, "a content URL on the agent endpoint host is used")
	rec := hostRequest(srv, http.MethodGet, "agents.example.net:9810", "/api/v1/agents", nil, nil)
	assert.NotEqual(t, http.StatusNotFound, rec.Code, "the agent endpoint host is confined")
}
