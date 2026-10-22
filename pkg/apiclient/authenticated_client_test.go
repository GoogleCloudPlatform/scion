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
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// These tests are sequential because the fallback cases replace DefaultClient.
func authenticatedClientForTest(t *testing.T, base *http.Client, useDefault bool) *http.Client {
	t.Helper()
	configured := base
	if useDefault {
		previous := http.DefaultClient
		http.DefaultClient = base
		t.Cleanup(func() { http.DefaultClient = previous })
		configured = nil
	}
	transport := NewTransport("https://hub.invalid",
		WithHTTPClient(configured),
		WithAuth(&BearerAuth{Token: "test-token"}),
		WithUserAgent("test-client"),
	)
	return transport.AuthenticatedHTTPClient()
}

func TestAuthenticatedHTTPClient_PreservesRedirectPolicy(t *testing.T) {
	for _, source := range []string{"configured", "default"} {
		for _, policy := range []string{"stop", "reject", "allow", "nil"} {
			t.Run(source+"/"+policy, func(t *testing.T) {
				var targetCalls, policyCalls atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer test-token" || r.UserAgent() != "test-client" {
						t.Error("authentication or user agent missing from request")
					}
					if r.URL.Path == "/start" {
						http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
						return
					}
					targetCalls.Add(1)
					if policy == "allow" && r.Header.Get("X-Test-Redirect") != "allowed" {
						t.Error("custom redirect policy did not modify the target request")
					}
					_, _ = io.WriteString(w, "target content")
				}))
				defer server.Close()
				base := server.Client()
				base.Timeout = 5 * time.Second
				originalTransport := base.Transport
				rejected := errors.New("redirect rejected by caller")
				if policy != "nil" {
					base.CheckRedirect = func(req *http.Request, via []*http.Request) error {
						policyCalls.Add(1)
						if len(via) != 1 || via[0].URL.Path != "/start" {
							t.Error("unexpected redirect history")
						}
						switch policy {
						case "stop":
							return http.ErrUseLastResponse
						case "reject":
							return rejected
						default:
							req.Header.Set("X-Test-Redirect", "allowed")
							return nil
						}
					}
				}
				client := authenticatedClientForTest(t, base, source == "default")
				if client == base || base.Transport != originalTransport || client.Timeout != base.Timeout {
					t.Fatal("wrapping must preserve the timeout without mutating the base client")
				}
				resp, err := client.Get(server.URL + "/start")
				if resp != nil {
					defer func() { _ = resp.Body.Close() }()
				}
				if policy == "reject" {
					if !errors.Is(err, rejected) {
						t.Errorf("redirect error = %v, want caller's rejection", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					wantStatus := http.StatusOK
					if policy == "stop" {
						wantStatus = http.StatusTemporaryRedirect
					}
					if resp.StatusCode != wantStatus {
						t.Errorf("status = %d, want %d", resp.StatusCode, wantStatus)
					}
					body, readErr := io.ReadAll(resp.Body)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if policy != "stop" && string(body) != "target content" {
						t.Errorf("downloaded content = %q", body)
					}
				}
				wantTarget := int32(1)
				if policy == "stop" || policy == "reject" {
					wantTarget = 0
				}
				if got := targetCalls.Load(); got != wantTarget {
					t.Errorf("target requests = %d, want %d", got, wantTarget)
				}
				wantPolicy := int32(1)
				if policy == "nil" {
					wantPolicy = 0
				}
				if got := policyCalls.Load(); got != wantPolicy {
					t.Errorf("policy calls = %d, want %d", got, wantPolicy)
				}
			})
		}
	}
}

func TestAuthenticatedHTTPClient_PreservesCookieJar(t *testing.T) {
	for _, source := range []string{"configured", "default"} {
		t.Run(source, func(t *testing.T) {
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "initial"
				if r.URL.Path == "/target" {
					want = "updated"
				}
				cookie, err := r.Cookie("session")
				if err != nil || cookie.Value != want {
					t.Errorf("cookie = %v, error = %v; want session=%s", cookie, err, want)
				}
				if r.URL.Path == "/start" {
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "updated", Path: "/"})
					http.Redirect(w, r, "/target", http.StatusFound)
				}
			}))
			defer server.Close()
			serverURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(serverURL, []*http.Cookie{{Name: "session", Value: "initial", Path: "/"}})
			base := &http.Client{Jar: jar, Timeout: 5 * time.Second}
			client := authenticatedClientForTest(t, base, source == "default")
			resp, err := client.Get(server.URL + "/start")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			cookies := jar.Cookies(serverURL)
			if len(cookies) != 1 || cookies[0].Value != "updated" {
				t.Errorf("shared jar = %v, want updated session cookie", cookies)
			}
			if base.Jar != jar || client.Jar != jar {
				t.Error("authenticated client must share the configured cookie jar")
			}
		})
	}
}
