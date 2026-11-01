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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/portview"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// portViewTransports names the two port proxy transports a page view test
// runs over: the agent's conduit session, or the port-forward tunnel.
var portViewTransports = []string{"conduit", "tunnel"}

// startOver connects the fixture's agent over transport.
func (f *portViewFixture) startOver(t *testing.T, transport string) {
	t.Helper()
	switch transport {
	case "conduit":
		f.startAgent(t)
	case "tunnel":
		startLegacyTunnel(t, f.conduitProxyFixture, f.srv)
	default:
		t.Fatalf("unknown transport %q", transport)
	}
}

// requireServedOver asserts which transport carried the fixture's
// requests.
func (f *portViewFixture) requireServedOver(t *testing.T, transport string) {
	t.Helper()
	assert.Equal(t, transport == "tunnel", f.srv.portTunnels.has(f.launched.ID), "tunnel session")
	assert.Equal(t, transport == "conduit", f.sessions.Load() == 1, "conduit sessions")
}

func (f *portViewFixture) drainApp() {
	for len(f.app.got) > 0 {
		<-f.app.got
	}
}

// TestPortViewTransportAvailable_TunnelTerm: with no conduit routing on
// this node, only the tunnel term decides: a page link is created only when
// the port-forward tunnel is up and the legacy route is not refused for the
// request's identity. The check writes, logs and records nothing. (The
// conduit term is TestPortView_EligibilityExternalRelayEnforce.)
func TestPortViewTransportAvailable_TunnelTerm(t *testing.T) {
	srv, _ := testServer(t)
	rec := recordOpenRefusals(srv)
	ctx := context.Background()
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}
	exposed := &store.ExposedPort{Port: 3000, Host: "127.0.0.1"}
	user := NewScopedUserIdentity(NewAuthenticatedUser("user-1", "u@example.com", "U", store.UserRoleMember, string(ClientTypeWeb)),
		"project-1", []string{portViewScope})
	require.False(t, srv.conduitRoutable(), "the conduit term is off")

	assert.False(t, srv.portViewTransportAvailable(ctx, agent, exposed, user), "tunnel term: no tunnel")

	srv.portTunnels.mu.Lock()
	srv.portTunnels.sessions[agent.ID] = &PortTunnelSession{}
	srv.portTunnels.mu.Unlock()
	assert.True(t, srv.portViewTransportAvailable(ctx, agent, exposed, user), "tunnel term: tunnel up, legacy route allowed")

	srv.conduitExternal.Store(&conduitExternalRuntime{})
	enforceTargetDeadlines(srv)
	require.True(t, srv.conduitUpgradeGateOn())
	assert.False(t, srv.portViewTransportAvailable(ctx, agent, exposed, user), "tunnel term: tunnel up, legacy route refused for a user")
	assert.Empty(t, rec.list(), "the check records no refusal")
}

// portLinkNavigation sends a page navigation by the agent's owner to the
// port link of w's agent, without following redirects.
func portLinkNavigation(t *testing.T, w *separateRelayWorld) pvResult {
	t.Helper()
	tok, _, _, err := w.srv.userTokenService.GenerateTokenPair(
		w.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	f := &portViewFixture{conduitProxyFixture: w.conduitProxyFixture}
	return f.send(t, http.MethodGet, f.portLink("index.html"), withBearer(navHeader(), tok), nil)
}

// TestPortView_EligibilityExternalRelayEnforce: in external relay mode with
// enforced target deadlines, conduit is routable but a user request to an
// agent without a conduit session is refused rather than served on the
// tunnel. A port link navigation then creates no page link (no_transport),
// though the agent's tunnel is up; with a conduit session it does.
func TestPortView_EligibilityExternalRelayEnforce(t *testing.T) {
	t.Run("conduit term: no session, tunnel refused", func(t *testing.T) {
		w := newSeparateRelayWorldWith(t, separateRelayOptions{enforce: true, noConduitAgent: true})
		setPortViewExperiments(t, w.srv, true, true)
		points := portViewCounters(t, w.srv)
		rec := recordOpenRefusals(w.srv)
		startLegacyTunnel(t, w.conduitProxyFixture, w.srv)
		require.True(t, w.srv.conduitRoutable())
		require.True(t, w.srv.conduitUpgradeGateOn())
		require.Empty(t, w.agentSessions(t), "the agent has no conduit session")

		res := portLinkNavigation(t, w)
		assert.NotEqual(t, http.StatusFound, res.status, "no page link: %s", res.header.Get("Location"))
		assert.NotContains(t, res.header.Get("Location"), "portview/")
		got := points()
		assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.redirect",
			map[string]string{"outcome": portViewRedirectNoTransport}), "%+v", got)
		assert.Zero(t, portViewCount(got, "scion.hub.port_view.redirect",
			map[string]string{"outcome": portViewRedirectMinted}), "%+v", got)
		// The port link's own request is refused by the route (the
		// existing rule), and that refusal is the only one recorded: the
		// eligibility check records none.
		assert.Equal(t, []string{noSessionRefusal(conduitRouteProxy, ErrCodeUnavailable)}, rec.list())
	})
	t.Run("conduit term: session present", func(t *testing.T) {
		w := newSeparateRelayWorldWith(t, separateRelayOptions{enforce: true})
		setPortViewExperiments(t, w.srv, true, true)
		points := portViewCounters(t, w.srv)
		require.True(t, w.srv.conduitUpgradeGateOn())
		require.NotEmpty(t, w.agentSessions(t), "the agent has a conduit session")

		res := portLinkNavigation(t, w)
		require.Equal(t, http.StatusFound, res.status, res.body)
		assert.Contains(t, res.header.Get("Location"), "portview/")
		got := points()
		assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.redirect",
			map[string]string{"outcome": portViewRedirectMinted}), "%+v", got)
	})
}

// TestPortView_TunnelRedirectsAndServes: with the conduit route off, a
// page navigation to the port link of an agent on the port-forward tunnel
// is answered with a page link, which the tunnel serves.
func TestPortView_TunnelRedirectsAndServes(t *testing.T) {
	f := newPortViewFixture(t, nil)
	setPortViewExperiments(t, f.srv, false, true)
	startLegacyTunnel(t, f.conduitProxyFixture, f.srv)

	link := f.portLink("dir/index.html?a=1")
	res := f.send(t, http.MethodGet, link, withBearer(navHeader(), f.userToken), nil)
	require.Equal(t, http.StatusFound, res.status, res.body)
	view := resolveLocation(t, link, res)
	require.True(t, strings.HasPrefix(view.Path, portview.Route), "redirected to %s", view)

	served := f.send(t, http.MethodGet, view.String(), subHeader(), nil)
	require.Equal(t, http.StatusOK, served.status, served.body)
	assert.Equal(t, "hello /dir/index.html?a=1", served.body)
}

// TestPortView_NoLinkWithoutTransport: with the conduit route off and no
// port-forward tunnel, a page navigation to the port link creates no page
// link; the port link answers it. (The refused-tunnel outcome is covered
// by TestPortViewTransportAvailable.)
func TestPortView_NoLinkWithoutTransport(t *testing.T) {
	f := newPortViewFixture(t, nil)
	setPortViewExperiments(t, f.srv, false, true)

	res := f.send(t, http.MethodGet, f.portLink("index.html"), withBearer(navHeader(), f.userToken), nil)
	assert.NotEqual(t, http.StatusFound, res.status, "no page link: %s", res.header.Get("Location"))
	assert.NotContains(t, res.header.Get("Location"), "portview/")
}

// portProxyPolicyResponseHeaders are the response headers the parity
// tests compare across transports.
var portProxyPolicyResponseHeaders = []string{
	"Content-Security-Policy", "Permissions-Policy", "X-Content-Type-Options", "Referrer-Policy",
	"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Expose-Headers",
	"Set-Cookie", "Clear-Site-Data", "Cache-Control", "X-App",
}

// policyApp answers with headers the hub's response policy replaces or
// removes; /cached also sets Cache-Control.
func policyApp() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Set-Cookie", "app=1; Path=/")
		h.Set("Content-Security-Policy", "default-src *")
		h.Set("Permissions-Policy", "camera=*")
		h.Set("X-Content-Type-Options", "off")
		h.Set("Clear-Site-Data", `"cookies"`)
		h.Set("Access-Control-Allow-Origin", "https://app.example")
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Expose-Headers", "X-App")
		h.Set("Referrer-Policy", "unsafe-url")
		h.Set("X-App", "ok")
		if strings.HasSuffix(r.URL.Path, "/cached") {
			h.Set("Cache-Control", "max-age=60")
		}
		_, _ = io.WriteString(w, "body")
	})
}

// TestPortProxyResponsePolicyParity: the conduit and the tunnel
// transports answer a proxied request with the same policy headers, on
// the page view route and on the port link, with and without the app's
// Cache-Control. The sandbox policy, Permissions-Policy and nosniff are
// sent once with the hub's values; the page view rules apply only on the
// page view route.
func TestPortProxyResponsePolicyParity(t *testing.T) {
	type answer map[string][]string
	got := map[string]map[string]answer{}
	for _, transport := range portViewTransports {
		f := newPortViewFixture(t, policyApp())
		f.startOver(t, transport)
		got[transport] = map[string]answer{}
		for _, sub := range []string{"page", "cached"} {
			for surface, target := range map[string]string{
				"view": f.viewURL(f.ownerCap(), sub),
				"link": f.portLink(sub),
			} {
				h := subHeader()
				if surface == "link" {
					// An API client on the port link: not a page navigation.
					h = withBearer(http.Header{"Accept": {"*/*"}}, f.userToken)
				}
				res := f.send(t, http.MethodGet, target, h, nil)
				require.Equal(t, http.StatusOK, res.status, "%s %s %s: %s", transport, surface, sub, res.body)
				require.Equal(t, "body", res.body)
				a := answer{}
				for _, k := range portProxyPolicyResponseHeaders {
					a[k] = res.header.Values(k)
				}
				got[transport][surface+" "+sub] = a
			}
		}
		f.requireServedOver(t, transport)
	}

	for key, a := range got["conduit"] {
		assert.Equal(t, a, got["tunnel"][key], "the transports disagree on %s", key)
		assert.Equal(t, []string{agentServedPageSandboxCSP}, a["Content-Security-Policy"], key)
		assert.Equal(t, []string{untrustedContentPermissionsPolicy}, a["Permissions-Policy"], key)
		assert.Equal(t, []string{"nosniff"}, a["X-Content-Type-Options"], key)
		assert.Empty(t, a["Set-Cookie"], key)
		assert.Empty(t, a["Clear-Site-Data"], key)
		assert.Empty(t, a["Access-Control-Allow-Credentials"], key)
		assert.Empty(t, a["Access-Control-Expose-Headers"], key)
		assert.Equal(t, []string{"ok"}, a["X-App"], key)
	}
	view := got["conduit"]["view page"]
	assert.Equal(t, []string{"no-referrer"}, view["Referrer-Policy"])
	assert.Equal(t, []string{"*"}, view["Access-Control-Allow-Origin"])
	assert.Equal(t, []string{"private"}, view["Cache-Control"])
	assert.Equal(t, []string{"max-age=60"}, got["conduit"]["view cached"]["Cache-Control"])
	for _, sub := range []string{"page", "cached"} {
		link := got["conduit"]["link "+sub]
		assert.NotContains(t, link["Referrer-Policy"], "unsafe-url", "the app's Referrer-Policy is removed on the port link")
		assert.NotContains(t, link["Access-Control-Allow-Origin"], "https://app.example", "the app's CORS headers are removed on the port link")
		assert.NotContains(t, link["Cache-Control"], "private", "the page view default does not apply to the port link")
	}
}

// TestPortView_RequestStrippingParity: on the page view route, the
// conduit and the tunnel transports both keep the hub's credential and
// identity headers from the app, one header per subtest, and forward
// Origin "null" and an ordinary header.
func TestPortView_RequestStrippingParity(t *testing.T) {
	for _, transport := range portViewTransports {
		t.Run(transport, func(t *testing.T) {
			f := newPortViewFixture(t, nil)
			f.startOver(t, transport)
			cases := map[string]string{
				"Cookie":                          "scion_sess=abc; other=1",
				"Authorization":                   "Bearer " + f.userToken,
				"X-Scion-Agent-Token":             f.agentToken(t, f.launched),
				"X-Scion-Broker-HMAC":             "abc",
				"X-Forwarded-User-Id":             "u1",
				"X-Forwarded-User-Email":          "u1@example.com",
				"X-Forwarded-User-Name":           "U",
				"X-Forwarded-User-Role":           "admin",
				IAPAssertionHeader:                "assertion",
				"X-Goog-Authenticated-User-Email": "accounts.google.com:u1@example.com",
				"X-Goog-Authenticated-User-Id":    "accounts.google.com:1",
			}
			for name, value := range cases {
				f.drainApp()
				h := subHeader()
				h.Set(name, value)
				h.Set("X-Keep", "kept")
				res := f.send(t, http.MethodGet, f.viewURL(f.ownerCap(), "probe"), h, nil)
				require.Equal(t, http.StatusOK, res.status, "%s: %s", name, res.body)
				select {
				case r := <-f.app.got:
					assert.Empty(t, r.Header.Values(name), "%s reached the app", name)
					assert.Equal(t, "null", r.Header.Get("Origin"), name)
					assert.Equal(t, "kept", r.Header.Get("X-Keep"), name)
				case <-time.After(10 * time.Second):
					t.Fatalf("%s: the request did not reach the app", name)
				}
			}
			f.requireServedOver(t, transport)
		})
	}
}

// TestPortView_TunnelWebSocketNotSupported: a WebSocket on a page link
// served by the port-forward tunnel gets the tunnel's generic 501, whose
// body does not carry the page link.
func TestPortView_TunnelWebSocketNotSupported(t *testing.T) {
	f := newPortViewFixture(t, echoApp())
	startLegacyTunnel(t, f.conduitProxyFixture, f.srv)
	capability := f.ownerCap()
	wsURL := "ws" + strings.TrimPrefix(f.viewURL(capability, "ws"), "http")
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if c != nil {
		_ = c.Close()
	}
	require.Error(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode, string(body))
	assert.NotContains(t, string(body), capability)
	assert.NotContains(t, string(body), "portview")
}

// TestPortView_NavigationEdgeCases covers the page view route's navigation
// identity check beyond TestPortView_NavigationMatrix: the development
// user counts as a signed-in user; an agent identity counts as no session;
// a page link whose user cannot use the agent now sends a navigation to
// the port link, which does not create a new page link for that user, so
// the two links never redirect to each other in a loop; that holds when
// the user's port access is removed or the account is suspended
// mid-session.
func TestPortView_NavigationEdgeCases(t *testing.T) {
	f := newPortViewFixture(t, nil)
	f.startAgent(t)
	const sub = "dir/page.html?q=1"
	direct := func(t *testing.T, capability string, identity Identity, header http.Header) pvResult {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, portview.Route+capability+"/"+sub, nil)
		for k, v := range header {
			req.Header[k] = v
		}
		if identity != nil {
			req = req.WithContext(contextWithIdentity(req.Context(), identity))
		}
		rec := httptest.NewRecorder()
		f.srv.handlePortView(rec, req)
		return pvResult{status: rec.Code, header: rec.Header(), body: rec.Body.String()}
	}
	toPortLink := func(t *testing.T, capability string, res pvResult) {
		t.Helper()
		require.Equal(t, http.StatusFound, res.status, res.body)
		got := resolveLocation(t, f.viewURL(capability, sub), res)
		want, err := url.Parse(f.portLink(sub))
		require.NoError(t, err)
		assert.Equal(t, want.String(), got.String())
	}

	t.Run("development user, other user's link", func(t *testing.T) {
		dev := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"})
		require.NotEqual(t, f.launched.OwnerID, dev.ID())
		toPortLink(t, f.ownerCap(), direct(t, f.ownerCap(), dev, navHeader()))
	})
	t.Run("agent identity counts as no session", func(t *testing.T) {
		agent := cpmAgentIdentity(f.launched.ID, f.launched.ProjectID, nil)
		res := direct(t, f.ownerCap(), agent, navHeader())
		require.Equal(t, http.StatusOK, res.status, res.body)
		assert.Equal(t, "hello /dir/page.html?q=1", res.body)
	})
	t.Run("link the user cannot use: no loop", func(t *testing.T) {
		foreign := f.capFor(f.stranger.ID(), f.app.port, time.Now().Add(time.Hour))
		first := f.send(t, http.MethodGet, f.viewURL(foreign, sub), withBearer(navHeader(), f.strangerToken), nil)
		toPortLink(t, foreign, first)
		next := f.send(t, http.MethodGet, f.portLink(sub), withBearer(navHeader(), f.strangerToken), nil)
		assert.NotEqual(t, http.StatusFound, next.status, "the port link created a page link: %s", next.header.Get("Location"))
		assert.NotContains(t, next.header.Get("Location"), "portview/")
	})
	t.Run("same user's port access removed mid-session", func(t *testing.T) {
		// A project admin may open the owner's agent's ports; the account
		// stays active while the role is removed.
		ctx := context.Background()
		adminID := tid("port-view-project-admin")
		createTestUserWithProjectRole(t, f.store, adminID, "admin@conduit.test", f.launched.ProjectID, store.ProjectRoleAdmin)
		ensureHubMembership(ctx, f.store, adminID)
		adminToken, _, _, err := f.srv.userTokenService.GenerateTokenPair(
			adminID, "admin@conduit.test", "Admin", store.UserRoleMember, ClientTypeWeb)
		require.NoError(t, err)
		capability := f.capFor(adminID, f.app.port, time.Now().Add(time.Hour))
		require.Equal(t, http.StatusOK, f.send(t, http.MethodGet, f.viewURL(capability, sub), subHeader(), nil).status,
			"served while the admin has port access")

		bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, adminID)
		require.NoError(t, err)
		require.NotEmpty(t, bindings)
		for _, b := range bindings {
			if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.launched.ProjectID {
				require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
			}
		}
		user, err := f.store.GetUser(ctx, adminID)
		require.NoError(t, err)
		require.Equal(t, "active", user.Status, "the account stays active")

		toPortLink(t, capability, f.send(t, http.MethodGet, f.viewURL(capability, sub), withBearer(navHeader(), adminToken), nil))
		requirePortViewInvalid(t, f.send(t, http.MethodGet, f.viewURL(capability, sub), subHeader(), nil))
		next := f.send(t, http.MethodGet, f.portLink(sub), withBearer(navHeader(), adminToken), nil)
		assert.NotEqual(t, http.StatusFound, next.status, "the port link created a page link: %s", next.header.Get("Location"))
		assert.NotContains(t, next.header.Get("Location"), "portview/")
	})
	t.Run("same user suspended mid-session", func(t *testing.T) {
		capability := f.ownerCap()
		require.Equal(t, http.StatusOK, f.send(t, http.MethodGet, f.viewURL(capability, sub), subHeader(), nil).status)
		rec := doRequest(t, f.srv, http.MethodPatch, "/api/v1/users/"+f.launched.OwnerID, map[string]string{"status": store.UserStatusSuspended})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		toPortLink(t, capability, f.send(t, http.MethodGet, f.viewURL(capability, sub), navHeader(), nil))
		requirePortViewInvalid(t, f.send(t, http.MethodGet, f.viewURL(capability, sub), subHeader(), nil))
	})
}

// TestPortProxyPolicyHeadersMatchPolicy: the headers both transports clear
// on the response writer before merging the app's (portProxyPolicyHeaders)
// are exactly the ones applyPortProxyResponsePolicy always sets, so a
// header added to the policy is also cleared and sent once.
func TestPortProxyPolicyHeadersMatchPolicy(t *testing.T) {
	want := make([]string, 0, len(portProxyPolicyHeaders))
	for _, k := range portProxyPolicyHeaders {
		want = append(want, http.CanonicalHeaderKey(k))
	}
	sort.Strings(want)
	h := http.Header{}
	applyPortProxyResponsePolicy(h, portProxyPolicy{})
	got := make([]string, 0, len(h))
	for k := range h {
		got = append(got, k)
	}
	sort.Strings(got)
	assert.Equal(t, want, got)

	// The page view rules add only Cache-Control (a default the app may
	// set itself), which the writer never carries.
	pv := http.Header{}
	applyPortProxyResponsePolicy(pv, portProxyPolicy{portView: true})
	for _, k := range want {
		assert.Len(t, pv.Values(k), 1, k)
	}
	assert.Equal(t, []string{"private"}, pv.Values("Cache-Control"))
	assert.Len(t, pv, len(want)+1)
}

// TestPortView_EligibilityPinnedPlacement: the conduit term also models
// the open's placement guard. An agent on a conduit session gets a page
// link; once it is pinned to a saved placement that is inconsistent, the
// port link's own request is refused (the route's placement refusal), and
// a navigation creates no page link (no_transport) instead of one the
// route would refuse.
func TestPortView_EligibilityPinnedPlacement(t *testing.T) {
	f := newPortViewFixture(t, nil)
	points := portViewCounters(t, f.srv)
	f.startAgent(t)

	link := f.portLink("index.html")
	before := f.send(t, http.MethodGet, link, withBearer(navHeader(), f.userToken), nil)
	require.Equal(t, http.StatusFound, before.status, "positive control, unpinned: %s", before.body)

	// Pin the agent to a target that is not its Runtime Broker's flat
	// placement: an inconsistent saved placement.
	_, err := f.store.SetAgentPinnedRuntimeTarget(context.Background(), f.launched.ID,
		store.PinnedPlacement{RuntimeBrokerID: f.launched.RuntimeBrokerID},
		store.PinnedPlacement{RuntimeBrokerID: f.launched.RuntimeBrokerID, RuntimeTargetID: "target-of-a-non-flat-row", RuntimeTargetType: "docker"})
	require.NoError(t, err)

	// The port link's own request (not a navigation) is refused by the
	// route's placement guard (409 placement conflict).
	plain := f.send(t, http.MethodGet, link, withBearer(http.Header{"Accept": {"*/*"}}, f.userToken), nil)
	require.Equal(t, http.StatusConflict, plain.status, plain.body)

	after := f.send(t, http.MethodGet, link, withBearer(navHeader(), f.userToken), nil)
	assert.NotEqual(t, http.StatusFound, after.status, "no page link: %s", after.header.Get("Location"))
	assert.NotContains(t, after.header.Get("Location"), "portview/")
	got := points()
	assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.redirect",
		map[string]string{"outcome": portViewRedirectMinted}), "%+v", got)
	assert.Equal(t, int64(1), portViewCount(got, "scion.hub.port_view.redirect",
		map[string]string{"outcome": portViewRedirectNoTransport}), "%+v", got)
}
