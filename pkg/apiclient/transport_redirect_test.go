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

package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// redirectHeaderNames are the request headers the Do redirect tests record.
var redirectHeaderNames = []string{
	"Authorization",
	"Proxy-Authorization",
	"X-Serverless-Authorization",
	"X-Scion-Agent-Token",
	"X-Scion-Run-Id",
	"X-Scion-Broker-Token",
	"X-Scion-Broker-ID",
	"X-Scion-Timestamp",
	"X-Scion-Nonce",
	"X-Scion-Signature",
	"X-Custom",
}

// hopRecorder records, per host and path, which of redirectHeaderNames
// each request carried and with what value.
type hopRecorder struct {
	mu   sync.Mutex
	hops map[string]map[string]string
}

func (r *hopRecorder) handler(redirects map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		got := map[string]string{}
		for _, name := range redirectHeaderNames {
			if v := req.Header.Get(name); v != "" {
				got[name] = v
			}
		}
		r.mu.Lock()
		if r.hops == nil {
			r.hops = map[string]map[string]string{}
		}
		r.hops[req.Host+req.URL.Path] = got
		r.mu.Unlock()
		if loc, ok := redirects[req.URL.Path]; ok {
			http.Redirect(w, req, loc, http.StatusFound)
			return
		}
		okHandler(w, req)
	}
}

// headers returns the headers recorded at hop, and whether the hop was
// reached at all.
func (r *hopRecorder) headers(hop string) (map[string]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hops[hop]
	return h, ok
}

func headerNames(h map[string]string) string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// hopLabel names a redirect target for subtest names.
func hopLabel(loc string) string {
	switch loc {
	case "/landed":
		return "same_origin"
	case "http://hub.test:80/landed":
		return "same_origin_explicit_port"
	case "http://hub.test:8080/landed":
		return "other_port"
	case "http://storage.test/landed":
		return "other_host"
	}
	return "unknown"
}

// doGet sends GET path through tr.Get and requires a 200.
func doGet(t *testing.T, tr *Transport, path string, headers http.Header) {
	t.Helper()
	resp, err := tr.Get(context.Background(), path, headers)
	if err != nil {
		t.Fatalf("Get %s: %v", path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Get %s: status %d, want 200", path, resp.StatusCode)
	}
}

// redirectNet builds an originTestNet with hub.test:80 redirecting
// /start to loc, and hub.test:8080 and storage.test:80 recording too.
func redirectNet(t *testing.T, rec *hopRecorder, loc string) *originTestNet {
	t.Helper()
	h := rec.handler(map[string]string{"/start": loc})
	return newOriginTestNet(t, map[string]http.HandlerFunc{
		"hub.test:80":     h,
		"hub.test:8080":   h,
		"storage.test:80": h,
	})
}

func newDoTransport(n *originTestNet, auth Authenticator) *Transport {
	return NewTransport("http://hub.test",
		WithHTTPClient(&http.Client{Transport: n.transport(), Timeout: 10 * time.Second}),
		WithAuth(auth))
}

func TestDo_RedirectToOtherPortOmitsAuthorization(t *testing.T) {
	rec := &hopRecorder{}
	n := redirectNet(t, rec, "http://hub.test:8080/landed")
	tr := newDoTransport(n, &BearerAuth{Token: "hub-token"})
	doGet(t, tr, "/start", nil)

	start, _ := rec.headers("hub.test/start")
	if got := start["Authorization"]; got != hubToken {
		t.Errorf("hub request Authorization = %q, want %q", got, hubToken)
	}
	landed, ok := rec.headers("hub.test:8080/landed")
	if !ok {
		t.Fatal("redirect to hub.test:8080 was not followed")
	}
	if got := landed["Authorization"]; got != "" {
		t.Errorf("hop to hub.test:8080 carried Authorization %q, want none", got)
	}
}

func TestDo_CrossOriginRedirectOmitsClientHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth Authenticator
		hub  string // header names expected on the hub request
	}{
		{"agent_token", &AgentTokenAuth{Token: "agent-token", RunID: "run-1"},
			"X-Custom,X-Scion-Agent-Token,X-Scion-Run-Id"},
		{"broker_token", &BrokerTokenAuth{Token: "broker-token"},
			"X-Custom,X-Scion-Broker-Token"},
		{"hmac", &HMACAuth{BrokerID: "broker-1", SecretKey: []byte("secret")},
			"X-Custom,X-Scion-Broker-ID,X-Scion-Nonce,X-Scion-Signature,X-Scion-Timestamp"},
	} {
		for _, loc := range []string{"http://storage.test/landed", "http://hub.test:8080/landed"} {
			t.Run(tc.name+"-"+hopLabel(loc), func(t *testing.T) {
				rec := &hopRecorder{}
				n := redirectNet(t, rec, loc)
				tr := newDoTransport(n, tc.auth)
				doGet(t, tr, "/start", http.Header{"X-Custom": {"kept"}})

				start, _ := rec.headers("hub.test/start")
				if got := headerNames(start); got != tc.hub {
					t.Errorf("hub request headers = %s, want %s", got, tc.hub)
				}
				landed, ok := rec.headers(strings.TrimPrefix(loc, "http://"))
				if !ok {
					t.Fatalf("redirect to %s was not followed", loc)
				}
				if got := headerNames(landed); got != "X-Custom" {
					t.Errorf("hop to %s carried headers %s, want only X-Custom", loc, got)
				}
			})
		}
	}
}

func TestDo_SameOriginRedirectKeepsHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth Authenticator
		want string
	}{
		{"bearer", &BearerAuth{Token: "hub-token"}, "Authorization,X-Custom"},
		{"agent_token", &AgentTokenAuth{Token: "agent-token", RunID: "run-1"},
			"X-Custom,X-Scion-Agent-Token,X-Scion-Run-Id"},
	} {
		// Both spellings of the hub origin: implicit and explicit port 80.
		for _, loc := range []string{"/landed", "http://hub.test:80/landed"} {
			t.Run(tc.name+"-"+hopLabel(loc), func(t *testing.T) {
				rec := &hopRecorder{}
				n := redirectNet(t, rec, loc)
				tr := newDoTransport(n, tc.auth)
				doGet(t, tr, "/start", http.Header{"X-Custom": {"kept"}})

				start, _ := rec.headers("hub.test/start")
				landed, ok := rec.headers("hub.test/landed")
				if !ok {
					landed, ok = rec.headers("hub.test:80/landed")
				}
				if !ok {
					t.Fatal("same-origin redirect was not followed")
				}
				if got := headerNames(landed); got != tc.want {
					t.Errorf("same-origin hop headers = %s, want %s", got, tc.want)
				}
				for k, v := range start {
					if landed[k] != v {
						t.Errorf("same-origin hop %s = %q, want %q", k, landed[k], v)
					}
				}
			})
		}
	}
}

func TestDo_CrossOriginRedirectOmitsTransportToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   transportauth.HeaderMode
		header string
	}{
		{"authorization", transportauth.HeaderAuthorization, "Authorization"},
		{"proxy_authorization", transportauth.HeaderProxyAuthorization, "Proxy-Authorization"},
		{"serverless_authorization", transportauth.HeaderServerlessAuthorization, "X-Serverless-Authorization"},
	} {
		for _, loc := range []string{"http://storage.test/landed", "http://hub.test:8080/landed", "/landed"} {
			t.Run(tc.name+"-"+hopLabel(loc), func(t *testing.T) {
				rec := &hopRecorder{}
				n := redirectNet(t, rec, loc)
				hc := &http.Client{
					Transport: transportauth.Wrap(n.transport(), stubTokenSource{tok: "transport-token"}, tc.mode),
					Timeout:   10 * time.Second,
				}
				tr := NewTransport("http://hub.test", WithHTTPClient(hc))
				doGet(t, tr, "/start", nil)

				const token = "Bearer transport-token"
				start, _ := rec.headers("hub.test/start")
				if got := headerNames(start); got != tc.header || start[tc.header] != token {
					t.Errorf("hub request headers = %v, want %s: %s", start, tc.header, token)
				}
				hop := strings.TrimPrefix(loc, "http://")
				want := ""
				if loc == "/landed" {
					hop, want = "hub.test/landed", tc.header
				}
				landed, ok := rec.headers(hop)
				if !ok {
					t.Fatalf("redirect to %s was not followed", loc)
				}
				if got := headerNames(landed); got != want {
					t.Errorf("hop to %s carried headers %s, want %q", loc, got, want)
				}
			})
		}
	}
}

func TestDo_BaseCheckRedirectStillApplies(t *testing.T) {
	rec := &hopRecorder{}
	n := redirectNet(t, rec, "http://storage.test/landed")
	stop := errors.New("base policy stop")
	hc := &http.Client{
		Transport: n.transport(),
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return stop
		},
	}
	tr := NewTransport("http://hub.test", WithHTTPClient(hc))
	resp, err := tr.Get(context.Background(), "/start", nil)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Get followed the redirect, want the base CheckRedirect error")
	}
	if !errors.Is(err, stop) {
		t.Errorf("Get error = %v, want it to wrap the base CheckRedirect error", err)
	}
	if _, ok := rec.headers("storage.test/landed"); ok {
		t.Error("redirect target was reached despite the base CheckRedirect error")
	}
}

func TestDoNoRetry_CrossOriginRedirectNotFollowed(t *testing.T) {
	for _, loc := range []string{"http://storage.test/landed", "http://hub.test:8080/landed"} {
		t.Run(hopLabel(loc), func(t *testing.T) {
			rec := &hopRecorder{}
			n := redirectNet(t, rec, loc)
			tr := newDoTransport(n, &BearerAuth{Token: "hub-token"})
			resp, err := tr.PostNoRetry(context.Background(), "/start", map[string]string{"k": "v"},
				http.Header{"X-Custom": {"kept"}})
			if err != nil {
				t.Fatalf("PostNoRetry: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusFound {
				t.Errorf("PostNoRetry status = %d, want the 302 returned unchanged", resp.StatusCode)
			}
			if _, ok := rec.headers(strings.TrimPrefix(loc, "http://")); ok {
				t.Errorf("PostNoRetry followed the redirect to %s", loc)
			}
		})
	}
}

func TestAuthenticatedHTTPClient_CrossOriginRedirectDropsCallerScionHeaders(t *testing.T) {
	for _, loc := range []string{"http://storage.test/landed", "http://hub.test:8080/landed", "/landed"} {
		t.Run(hopLabel(loc), func(t *testing.T) {
			rec := &hopRecorder{}
			n := redirectNet(t, rec, loc)
			c := n.client("http://hub.test", &BearerAuth{Token: "hub-token"})
			req := newGet(t, "http://hub.test/start")
			req.Header.Set("X-Scion-Run-Id", "caller-run")
			req.Header.Set("X-Custom", "kept")
			mustGet(t, c, req)

			start, _ := rec.headers("hub.test/start")
			if got := headerNames(start); got != "Authorization,X-Custom,X-Scion-Run-Id" {
				t.Errorf("hub request headers = %s, want Authorization,X-Custom,X-Scion-Run-Id", got)
			}
			hop, want := strings.TrimPrefix(loc, "http://"), "X-Custom"
			if loc == "/landed" {
				hop, want = "hub.test/landed", "Authorization,X-Custom,X-Scion-Run-Id"
			}
			landed, ok := rec.headers(hop)
			if !ok {
				t.Fatalf("redirect to %s was not followed", loc)
			}
			if got := headerNames(landed); got != want {
				t.Errorf("hop to %s carried headers %s, want %s", loc, got, want)
			}
		})
	}
}
