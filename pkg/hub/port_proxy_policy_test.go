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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/portforward"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertAgentPortHubPagePolicy asserts that a hub-generated response on an
// agent-port proxy route carries the exact single-valued sandbox page policy,
// Permissions-Policy, Referrer-Policy, and nosniff headers.
func assertAgentPortHubPagePolicy(t *testing.T, h http.Header) {
	t.Helper()
	assert.Equal(t, []string{untrustedContentSandboxCSP}, h.Values("Content-Security-Policy"), "Content-Security-Policy")
	assert.Equal(t, []string{untrustedContentPermissionsPolicy}, h.Values("Permissions-Policy"), "Permissions-Policy")
	assert.Equal(t, []string{"no-referrer"}, h.Values("Referrer-Policy"), "Referrer-Policy")
	assert.Equal(t, []string{"nosniff"}, h.Values("X-Content-Type-Options"), "X-Content-Type-Options")
}

// TestAgentPortHubResponses_PagePolicyOnLegacyPath (ptone/scion#5944) verifies
// that every hub-generated response on agent-port proxy routes carries the
// sandbox page policy on a hub-only listener (without WebServer's
// securityHeadersMiddleware), that upstream tunnel error text is replaced with
// fixed error text, and that behind securityHeadersMiddleware the headers are
// not duplicated.
func TestAgentPortHubResponses_PagePolicyOnLegacyPath(t *testing.T) {
	srv, s := testServer(t)
	srv.config.ConduitPolicy.DenyPorts = []int{3001}

	agent, token := createPortForwardAgent(t, srv, s)
	otherToken := srv.generateRecordedAgentToken(t, tid("pf-other-agent"), agent.ProjectID, []AgentTokenScope{ScopeAgentPortForward}, nil)

	for _, port := range []int{3000, 3001} {
		rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
			"port": port,
			"host": "127.0.0.1",
		}, token)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	base := "/api/v1/agents/" + agent.ID + "/ports/3000/proxy"

	serve := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("sign_in_required_browser", func(t *testing.T) {
		rec := serve(http.MethodGet, base+"/", map[string]string{"Accept": "text/html"})
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assertAgentPortHubPagePolicy(t, rec.Header())
	})

	t.Run("sign_in_required_subresource", func(t *testing.T) {
		rec := serve(http.MethodGet, base+"/app.js", map[string]string{"Origin": "null"})
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assertAgentPortHubPagePolicy(t, rec.Header())
	})

	t.Run("invalid_token", func(t *testing.T) {
		rec := serve(http.MethodGet, base+"/", map[string]string{"X-Scion-Agent-Token": "invalid"})
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assertAgentPortHubPagePolicy(t, rec.Header())
	})

	t.Run("non_numeric_port_not_found", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/notaport/proxy/", map[string]string{
			"X-Scion-Agent-Token": token,
		})
		require.Equal(t, http.StatusNotFound, rec.Code)
		assertAgentPortHubPagePolicy(t, rec.Header())
	})

	t.Run("agent_not_found", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v1/agents/does-not-exist/ports/3000/proxy/", map[string]string{
			"X-Scion-Agent-Token": token,
		})
		require.Equal(t, http.StatusNotFound, rec.Code)
		assertAgentPortHubPagePolicy(t, rec.Header())
	})

	t.Run("forbidden_other_agent", func(t *testing.T) {
		rec := serve(http.MethodGet, base+"/", map[string]string{"X-Scion-Agent-Token": otherToken})
		require.Equal(t, http.StatusForbidden, rec.Code)
		assertAgentPortHubPagePolicy(t, rec.Header())
	})

	t.Run("port_not_exposed_browser_and_api", func(t *testing.T) {
		for _, accept := range []string{"text/html", "application/json"} {
			rec := serve(http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/9999/proxy/", map[string]string{
				"X-Scion-Agent-Token": token,
				"Accept":              accept,
			})
			require.Equal(t, http.StatusNotFound, rec.Code)
			assertAgentPortHubPagePolicy(t, rec.Header())
		}
	})

	t.Run("port_denied_browser_and_api", func(t *testing.T) {
		for _, accept := range []string{"text/html", "application/json"} {
			rec := serve(http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/3001/proxy/", map[string]string{
				"X-Scion-Agent-Token": token,
				"Accept":              accept,
			})
			require.Equal(t, http.StatusForbidden, rec.Code)
			assertAgentPortHubPagePolicy(t, rec.Header())
		}
	})

	t.Run("bare_url_redirect", func(t *testing.T) {
		rec := serve(http.MethodGet, base, map[string]string{"X-Scion-Agent-Token": token})
		require.Equal(t, http.StatusPermanentRedirect, rec.Code)
		assertAgentPortHubPagePolicy(t, rec.Header())
	})

	t.Run("no_tunnel_offline_browser_and_api", func(t *testing.T) {
		for _, accept := range []string{"text/html", "application/json"} {
			rec := serve(http.MethodGet, base+"/", map[string]string{
				"X-Scion-Agent-Token": token,
				"Accept":              accept,
			})
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assertAgentPortHubPagePolicy(t, rec.Header())
		}
	})

	t.Run("websocket_not_supported_on_legacy_tunnel", func(t *testing.T) {
		rec := serve(http.MethodGet, base+"/", map[string]string{
			"X-Scion-Agent-Token": token,
			"Connection":          "Upgrade",
			"Upgrade":             "websocket",
		})
		require.Equal(t, http.StatusNotImplemented, rec.Code)
		assertAgentPortHubPagePolicy(t, rec.Header())
	})

	t.Run("upstream_tunnel_error_has_policy_and_fixed_text", func(t *testing.T) {
		hubHTTP := httptest.NewServer(srv.Handler())
		t.Cleanup(hubHTTP.Close)

		header := http.Header{}
		header.Set("X-Scion-Agent-Token", token)
		wsURL := "ws" + strings.TrimPrefix(hubHTTP.URL, "http") + "/api/v1/agents/" + agent.ID + "/ports/tunnel"
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
		require.NoError(t, err)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.Cleanup(func() { _ = conn.Close() })

		// Fake agent tunnel that replies with an upstream error containing
		// raw internal details.
		const secretUpstreamDetail = "dial tcp 127.0.0.1:3000: secret-upstream-detail"
		go func() {
			for {
				var msg portforward.Message
				if err := conn.ReadJSON(&msg); err != nil {
					return
				}
				if msg.Type == portforward.MessageTypeRequest && msg.Request != nil {
					_ = conn.WriteJSON(portforward.Message{
						Type: portforward.MessageTypeError,
						Response: &portforward.Response{
							StreamID: msg.Request.StreamID,
							Error:    secretUpstreamDetail,
						},
					})
				}
			}
		}()
		require.Eventually(t, func() bool {
			return srv.portTunnels.has(agent.ID)
		}, 5*time.Second, 25*time.Millisecond)

		rec := serve(http.MethodGet, base+"/boom", map[string]string{"X-Scion-Agent-Token": token})
		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		assertAgentPortHubPagePolicy(t, rec.Header())
		assert.NotContains(t, rec.Body.String(), secretUpstreamDetail)
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
		assert.Equal(t, portTunnelFailedMessage, errResp.Error.Message)
	})

	t.Run("behind_web_security_headers_no_duplicate_policy", func(t *testing.T) {
		wrapped := (&WebServer{}).securityHeadersMiddleware(srv.Handler())
		for _, tc := range []struct {
			name    string
			path    string
			headers map[string]string
			want    int
		}{
			{
				name:    "sign_in_required",
				path:    base + "/",
				headers: map[string]string{"Accept": "text/html"},
				want:    http.StatusUnauthorized,
			},
			{
				name:    "port_not_exposed",
				path:    "/api/v1/agents/" + agent.ID + "/ports/9999/proxy/",
				headers: map[string]string{"X-Scion-Agent-Token": token, "Accept": "text/html"},
				want:    http.StatusNotFound,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, tc.path, nil)
				for k, v := range tc.headers {
					req.Header.Set(k, v)
				}
				rec := httptest.NewRecorder()
				wrapped.ServeHTTP(rec, req)
				require.Equal(t, tc.want, rec.Code)
				assertAgentPortHubPagePolicy(t, rec.Header())
			})
		}
	})

	t.Run("non_proxy_route_does_not_get_sandbox_policy", func(t *testing.T) {
		rec := serve(http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports", map[string]string{
			"X-Scion-Agent-Token": token,
		})
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Empty(t, rec.Header().Get("Content-Security-Policy"))
	})
}

// TestAgentPortHubResponses_PagePolicyOnConduitPath (ptone/scion#5944) verifies
// that every hub-generated response on agent-port proxy routes carries the
// sandbox page policy on a hub-only listener when conduit is enabled, and that
// proxied responses still carry a single agentServedPageSandboxCSP.
func TestAgentPortHubResponses_PagePolicyOnConduitPath(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "<html>ok</html>")
		case "/drop":
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		default:
			<-r.Context().Done()
		}
	})

	f := newConduitProxyFixture(t, app)
	f.withHeaderTimeout(t, 500*time.Millisecond)
	base := "/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy"

	doHub := func(path string, headers map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, f.base+path, nil)
		require.NoError(t, err)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	t.Run("sign_in_required_browser_and_subresource", func(t *testing.T) {
		for _, hdrs := range []map[string]string{
			{"Accept": "text/html"},
			{"Origin": "null"},
		} {
			resp := doHub(base+"/ok", hdrs)
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assertAgentPortHubPagePolicy(t, resp.Header)
		}
	})

	t.Run("port_not_exposed_browser_and_api", func(t *testing.T) {
		for _, accept := range []string{"text/html", "application/json"} {
			resp := doHub("/api/v1/agents/"+f.launched.ID+"/ports/9999/proxy/", map[string]string{
				"Authorization": "Bearer " + f.userToken,
				"Accept":        accept,
			})
			require.Equal(t, http.StatusNotFound, resp.StatusCode)
			assertAgentPortHubPagePolicy(t, resp.Header)
		}
	})

	t.Run("agent_offline_browser_and_api", func(t *testing.T) {
		// Agent has not connected to conduit yet -> 503 agent_offline.
		for _, accept := range []string{"text/html", "application/json"} {
			resp := doHub(base+"/ok", map[string]string{
				"Authorization": "Bearer " + f.userToken,
				"Accept":        accept,
			})
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assertAgentPortHubPagePolicy(t, resp.Header)
		}
	})

	// Now connect the conduit agent and exercise upstream error, timeout, and
	// successful proxy responses.
	f.startAgent(t)

	t.Run("conduit_upstream_drop_502_fixed_text", func(t *testing.T) {
		resp := doHub(base+"/drop", map[string]string{
			"Authorization": "Bearer " + f.userToken,
		})
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadGateway, resp.StatusCode, string(body))
		assertAgentPortHubPagePolicy(t, resp.Header)
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(body, &errResp))
		assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
		assert.Equal(t, "Port proxy failed", errResp.Error.Message)
	})

	t.Run("conduit_upstream_timeout_504_browser_and_api", func(t *testing.T) {
		for _, accept := range []string{"text/html", "application/json"} {
			resp := doHub(base+"/slow", map[string]string{
				"Authorization": "Bearer " + f.userToken,
				"Accept":        accept,
			})
			require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
			assertAgentPortHubPagePolicy(t, resp.Header)
		}
	})

	t.Run("conduit_proxied_ok_keeps_single_app_sandbox_policy", func(t *testing.T) {
		resp := doHub(base+"/ok", map[string]string{
			"Authorization": "Bearer " + f.userToken,
		})
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		assert.Equal(t, []string{agentServedPageSandboxCSP}, resp.Header.Values("Content-Security-Policy"))
		assert.Equal(t, []string{untrustedContentPermissionsPolicy}, resp.Header.Values("Permissions-Policy"))
		assert.Equal(t, []string{"no-referrer"}, resp.Header.Values("Referrer-Policy"))
		assert.Equal(t, []string{"nosniff"}, resp.Header.Values("X-Content-Type-Options"))
	})
}
