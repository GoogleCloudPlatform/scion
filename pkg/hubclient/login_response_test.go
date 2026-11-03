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

package hubclient

import (
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// signInPage stands in for an HTML sign-in page. The marker words must
// never appear in an error message.
const signInPage = `<!DOCTYPE html><html><head><title>Sign in MARKER-PAGE-TITLE</title></head>` +
	`<body>MARKER-PAGE-BODY</body></html>`

// signInQuery stands in for the query string of a sign-in redirect.
const signInQuery = "client_id=MARKER-QUERY-CLIENT&state=MARKER-QUERY-STATE"

// assertNoEcho fails when err's message carries any part of the HTML page
// or of the redirect query, or a JSON decode error.
func assertNoEcho(t *testing.T, err error) {
	t.Helper()
	msg := err.Error()
	for _, s := range []string{"MARKER-", "<html", "<!DOCTYPE", "invalid character", "decode"} {
		if strings.Contains(msg, s) {
			t.Errorf("error message contains %q: %s", s, msg)
		}
	}
}

// googleSignInRoundTripper serves requests to the Google sign-in host with
// signInPage and sends every other request through base.
// When sawCredential is set, it records whether a request to the sign-in
// host carried Proxy-Authorization or Authorization.
type googleSignInRoundTripper struct {
	base          http.RoundTripper
	sawCredential *atomic.Bool
}

func (rt googleSignInRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() != googleSignInHost {
		return rt.base.RoundTrip(req)
	}
	if rt.sawCredential != nil && (req.Header.Get("Proxy-Authorization") != "" || req.Header.Get("Authorization") != "") {
		rt.sawCredential.Store(true)
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/html; charset=utf-8")
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.WriteString(signInPage)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func newLoginTestClient(t *testing.T, handler http.HandlerFunc, opts ...Option) (Client, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, append([]Option{WithRetry(0, 0)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv.URL
}

func TestGetAuthProviders_HTMLPage(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(signInPage))
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if !errors.Is(err, ErrHubHTMLResponse) {
		t.Fatalf("err = %v, want ErrHubHTMLResponse", err)
	}
	assertNoEcho(t, err)
}

func TestGetAuthProviders_RedirectToGoogleSignIn(t *testing.T) {
	c, hubURL := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+googleSignInHost+"/o/oauth2/v2/auth?"+signInQuery, http.StatusFound)
	}, WithHTTPClient(&http.Client{Transport: googleSignInRoundTripper{base: http.DefaultTransport}}))

	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if !errors.Is(err, ErrHubBehindIAP) {
		t.Fatalf("err = %v, want ErrHubBehindIAP", err)
	}
	assertNoEcho(t, err)
	msg := err.Error()
	for _, want := range []string{hubURL, "SCION_TRANSPORT_MODE=iap", "SCION_TRANSPORT_TOKEN_FILE", "SCION_HUB_TOKEN", iapGuideURL} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, googleSignInHost) {
		t.Errorf("error message names the redirect target: %s", msg)
	}
}

func TestGetAuthProviders_IAPGeneratedUnauthorized(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-IAP-Generated-Response", "true")
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Invalid IAP credentials: MARKER-IAP-BODY"))
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if !errors.Is(err, ErrHubBehindIAP) {
		t.Fatalf("err = %v, want ErrHubBehindIAP", err)
	}
	assertNoEcho(t, err)
}

func TestGetAuthProviders_IAPRejectsTransportToken(t *testing.T) {
	src := transportauth.NewInjectedSource()
	src.SetToken(makeTestJWT(time.Now().Add(time.Hour)), time.Now().Add(time.Hour))
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-IAP-Generated-Response", "true")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Invalid IAP credentials: MARKER-IAP-BODY"))
	}, WithTransportAuth(src, transportauth.HeaderProxyAuthorization))

	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	var iapErr *IAPResponseError
	if !errors.As(err, &iapErr) || !iapErr.TransportCredentialSent {
		t.Fatalf("err = %v, want IAPResponseError with TransportCredentialSent", err)
	}
	if !strings.Contains(err.Error(), "did not accept the transport token") {
		t.Errorf("error message = %s", err)
	}
	assertNoEcho(t, err)
}

func TestGetAuthProviders_NonIAPHTMLError(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>MARKER-GATEWAY-PAGE</body></html>"))
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if !errors.Is(err, ErrHubHTMLResponse) || errors.Is(err, ErrHubBehindIAP) {
		t.Fatalf("err = %v, want only ErrHubHTMLResponse", err)
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("error message lacks the status: %s", err)
	}
	assertNoEcho(t, err)
}

func TestGetAuthProviders_HTMLWithoutContentType(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		_, _ = w.Write([]byte("\n  " + signInPage))
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if !errors.Is(err, ErrHubHTMLResponse) {
		t.Fatalf("err = %v, want ErrHubHTMLResponse", err)
	}
}

func TestGetAuthProviders_JSONStillDecodes(t *testing.T) {
	long := `{"clientType":"cli","providers":["google"],"pad":"` + strings.Repeat("x", 2*loginResponsePeekBytes) + `"}`
	cases := []struct {
		name        string
		contentType string
		body        string
		gzip        bool
	}{
		{name: "short body", contentType: "application/json", body: `{"providers":["google"]}`},
		{name: "longer than peek", contentType: "application/json", body: long},
		{name: "leading whitespace and charset", contentType: "application/json; charset=utf-8", body: "\n\t  " + `{"providers":["google"]}`},
		{name: "gzip encoded", contentType: "application/json", body: long, gzip: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.gzip {
					w.Header().Set("Content-Encoding", "gzip")
					gz := gzip.NewWriter(w)
					_, _ = gz.Write([]byte(tc.body))
					_ = gz.Close()
					return
				}
				_, _ = w.Write([]byte(tc.body))
			})
			resp, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
			if err != nil {
				t.Fatalf("GetAuthProviders: %v", err)
			}
			if len(resp.Providers) != 1 || resp.Providers[0] != "google" {
				t.Errorf("providers = %v", resp.Providers)
			}
		})
	}
}

func TestGetAuthProviders_EmptyBodyIsNotHTML(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if err == nil || errors.Is(err, ErrHubHTMLResponse) || errors.Is(err, ErrHubBehindIAP) {
		t.Fatalf("err = %v, want the existing decode error", err)
	}
}

func TestGetAuthProviders_JSONErrorUnchanged(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"not allowed"}}`))
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err = %v, want the hub's JSON error", err)
	}
}

func TestGetAuthURL_RedirectToGoogleSignIn(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+googleSignInHost+"/o/oauth2/v2/auth?"+signInQuery, http.StatusFound)
	}, WithHTTPClient(&http.Client{Transport: googleSignInRoundTripper{base: http.DefaultTransport}}))

	_, err := c.Auth().GetAuthURL(context.Background(), "http://127.0.0.1:1/callback", "state", OAuthProviderGoogle)
	if !errors.Is(err, ErrHubBehindIAP) {
		t.Fatalf("err = %v, want ErrHubBehindIAP", err)
	}
	assertNoEcho(t, err)
}

func TestRequestDeviceCode_HTMLPage(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(signInPage))
	})
	_, err := c.Auth().RequestDeviceCode(context.Background(), OAuthProviderGoogle)
	if !errors.Is(err, ErrHubHTMLResponse) {
		t.Fatalf("err = %v, want ErrHubHTMLResponse", err)
	}
	assertNoEcho(t, err)
}

func TestHubOrigin_DropsPathQueryAndUserInfo(t *testing.T) {
	u := &url.URL{Scheme: "https", User: url.UserPassword("u", "MARKER-PASS"), Host: "hub.example.com", Path: "/p", RawQuery: "MARKER-Q=1"}
	if got := hubOrigin(u.String()); got != "https://hub.example.com" {
		t.Errorf("hubOrigin = %q", got)
	}
}

func TestGetAuthProviders_XHTMLMediaType(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xhtml+xml; charset=utf-8")
		_, _ = w.Write([]byte("  MARKER-NOT-A-TAG-FIRST"))
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if !errors.Is(err, ErrHubHTMLResponse) {
		t.Fatalf("err = %v, want ErrHubHTMLResponse", err)
	}
	assertNoEcho(t, err)
}

func TestGetAuthProviders_NoCredentialTextNamesClientIDAudience(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-IAP-Generated-Response", "true")
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if !errors.Is(err, ErrHubBehindIAP) {
		t.Fatalf("err = %v, want ErrHubBehindIAP", err)
	}
	for _, want := range []string{"IAP OAuth client ID (the audience)", "email claim"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message lacks %q: %s", want, err)
		}
	}
}

func TestGetAuthProviders_RedirectToGoogleSignInWithTransportCredential(t *testing.T) {
	src := transportauth.NewInjectedSource()
	src.SetToken(makeTestJWT(time.Now().Add(time.Hour)), time.Now().Add(time.Hour))
	var sawCredential atomic.Bool
	var hubSawProxyAuth atomic.Bool
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			hubSawProxyAuth.Store(true)
		}
		http.Redirect(w, r, "https://"+googleSignInHost+"/o/oauth2/v2/auth?"+signInQuery, http.StatusFound)
	},
		WithHTTPClient(&http.Client{Transport: googleSignInRoundTripper{base: http.DefaultTransport, sawCredential: &sawCredential}}),
		WithTransportAuth(src, transportauth.HeaderProxyAuthorization),
	)

	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	var iapErr *IAPResponseError
	if !errors.As(err, &iapErr) || !iapErr.TransportCredentialSent {
		t.Fatalf("err = %v, want IAPResponseError with TransportCredentialSent", err)
	}
	if !hubSawProxyAuth.Load() {
		t.Error("the hub request carried no Proxy-Authorization; the test does not exercise the transport credential")
	}
	if sawCredential.Load() {
		t.Error("the request to the sign-in host carried a credential header")
	}
	assertNoEcho(t, err)
}

func TestGetAuthProviders_IAPPlainTextUnauthorizedWithoutHeader(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("Invalid IAP credentials: MARKER-IAP-REASON"))
		})
		_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
		if !errors.Is(err, ErrHubBehindIAP) {
			t.Fatalf("status %d: err = %v, want ErrHubBehindIAP", status, err)
		}
		assertNoEcho(t, err)
	}
}

func TestGetAuthProviders_OtherPlainTextUnauthorizedUnchanged(t *testing.T) {
	c, _ := newLoginTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("token expired"))
	})
	_, err := c.Auth().GetAuthProviders(context.Background(), string(OAuthClientTypeCLI))
	if err == nil || errors.Is(err, ErrHubBehindIAP) || errors.Is(err, ErrHubHTMLResponse) {
		t.Fatalf("err = %v, want the unchanged error path", err)
	}
	if !strings.Contains(err.Error(), "token expired") {
		t.Errorf("err = %v, want the hub's text", err)
	}
}

func TestHubOrigin_FallbackWithoutHost(t *testing.T) {
	for _, in := range []string{"", "hub.example.com", "%zz"} {
		if got := hubOrigin(in); got != "the configured URL" {
			t.Errorf("hubOrigin(%q) = %q, want the fallback", in, got)
		}
	}
}
