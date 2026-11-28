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

package remotefetch

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOpen_StreamingAndMidStreamMaxBytes(t *testing.T) {
	t.Run("streams within limit and forwards allowlisted headers", func(t *testing.T) {
		var gotAccept, gotINM, gotSecret string
		env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			gotAccept = r.Header.Get("Accept")
			gotINM = r.Header.Get("If-None-Match")
			gotSecret = r.Header.Get("X-Unlisted")
			w.Header().Set("ETag", `"abc"`)
			w.Header().Set("X-RateLimit-Remaining", "42")
			w.Header().Set("Set-Cookie", "session=secret")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("hello streaming world"))
		}, Config{MaxBytes: 1024})

		hdr := make(http.Header)
		hdr.Set("Accept", "application/vnd.github.sha")
		hdr.Set("If-None-Match", `"prev"`)
		hdr.Set("X-Unlisted", "must-not-be-sent")

		resp, err := env.fetcher.Open(context.Background(), Request{
			URL:    env.url("example.com", "/stream"),
			Header: hdr,
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(body) != "hello streaming world" {
			t.Fatalf("body = %q, want %q", string(body), "hello streaming world")
		}
		if gotAccept != "application/vnd.github.sha" {
			t.Errorf("Accept = %q", gotAccept)
		}
		if gotINM != `"prev"` {
			t.Errorf("If-None-Match = %q", gotINM)
		}
		if gotSecret != "" {
			t.Errorf("unlisted header forwarded: %q", gotSecret)
		}
		if resp.Header.Get("ETag") != `"abc"` {
			t.Errorf("ETag = %q", resp.Header.Get("ETag"))
		}
		if resp.Header.Get("X-RateLimit-Remaining") != "42" {
			t.Errorf("X-RateLimit-Remaining = %q", resp.Header.Get("X-RateLimit-Remaining"))
		}
		if resp.Header.Get("Set-Cookie") != "" {
			t.Errorf("non-allowlisted response header leaked: %q", resp.Header.Get("Set-Cookie"))
		}
	})

	t.Run("mid-stream MaxBytes enforced on chunked response", func(t *testing.T) {
		env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			flusher, _ := w.(http.Flusher)
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 10; i++ {
				_, _ = w.Write(bytes.Repeat([]byte("a"), 200))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}, Config{MaxBytes: 500})

		resp, err := env.fetcher.Open(context.Background(), Request{
			URL: env.url("example.com", "/chunked"),
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		_, err = io.ReadAll(resp.Body)
		if err == nil {
			t.Fatal("expected ReasonTooLarge mid-stream, got nil")
		}
		if got := reasonOf(t, err); got != ReasonTooLarge {
			t.Fatalf("reason = %s, want %s", got, ReasonTooLarge)
		}
	})

	t.Run("per-request MaxBytes overrides config", func(t *testing.T) {
		env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("x"), 200))
		}, Config{MaxBytes: 1024})

		resp, err := env.fetcher.Open(context.Background(), Request{
			URL:      env.url("example.com", "/small"),
			MaxBytes: 50,
		})
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("expected ReasonTooLarge")
		}
		if got := reasonOf(t, err); got != ReasonTooLarge {
			t.Fatalf("reason = %s, want %s", got, ReasonTooLarge)
		}
	})
}

func TestOpen_Non2xxStatusAndHeaders(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-GitHub-SSO", "required; url=https://github.com/orgs/acme/sso")
		w.Header().Set("X-Private-Header", "secret")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("You have exceeded a secondary rate limit"))
	}, Config{MaxBytes: 1024})

	resp, err := env.fetcher.Open(context.Background(), Request{
		URL: env.url("example.com", "/forbidden"),
	})
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("expected nil response on 403")
	}
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("expected *Error, got %v", err)
	}
	if fe.Reason != ReasonStatus || fe.Status != http.StatusForbidden {
		t.Fatalf("got reason=%s status=%d, want status 403", fe.Reason, fe.Status)
	}
	if fe.Header.Get("Retry-After") != "120" {
		t.Errorf("Retry-After = %q", fe.Header.Get("Retry-After"))
	}
	if fe.Header.Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("X-RateLimit-Remaining = %q", fe.Header.Get("X-RateLimit-Remaining"))
	}
	if fe.Header.Get("X-GitHub-SSO") == "" {
		t.Errorf("X-GitHub-SSO missing")
	}
	if fe.Header.Get("X-Private-Header") != "" {
		t.Errorf("non-allowlisted header leaked: %q", fe.Header.Get("X-Private-Header"))
	}
	if !strings.Contains(string(fe.Body), "secondary rate limit") {
		t.Errorf("Body snippet = %q", string(fe.Body))
	}
	if strings.Contains(fe.Error(), "secondary rate limit") {
		t.Errorf("Error() must not contain body: %q", fe.Error())
	}
}

func TestOpen_RedirectAuthAndHostRules(t *testing.T) {
	t.Run("strips Authorization on redirect to different allowed host", func(t *testing.T) {
		var hop0Auth, hop1Auth string
		var env *testEnv
		env = newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				hop0Auth = r.Header.Get("Authorization")
				http.Redirect(w, r, env.url("sub.example.com", "/final"), http.StatusFound)
				return
			}
			hop1Auth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}, Config{
			MaxBytes:     1024,
			AllowedHosts: []string{"example.com", "sub.example.com"},
		})
		env.resolver.answers["sub.example.com"] = env.resolver.answers["example.com"]

		resp, err := env.fetcher.Open(context.Background(), Request{
			URL:           env.url("example.com", "/start"),
			Authorization: "Bearer super-secret-token",
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		_ = resp.Body.Close()
		if hop0Auth != "Bearer super-secret-token" {
			t.Errorf("hop 0 auth = %q", hop0Auth)
		}
		if hop1Auth != "" {
			t.Errorf("hop 1 auth leaked: %q", hop1Auth)
		}
		if strings.Contains(env.logs.String(), "super-secret-token") {
			t.Errorf("token leaked in logs")
		}
	})

	t.Run("refuses redirect to non-allowlisted host", func(t *testing.T) {
		var env *testEnv
		env = newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, env.url("other.test", "/evil"), http.StatusFound)
		}, Config{
			MaxBytes:     1024,
			AllowedHosts: []string{"example.com"},
		})

		_, err := env.fetcher.Open(context.Background(), Request{
			URL:           env.url("example.com", "/start"),
			Authorization: "Bearer super-secret-token",
		})
		if got := reasonOf(t, err); got != ReasonHostNotAllowed {
			t.Fatalf("reason = %s, want %s", got, ReasonHostNotAllowed)
		}
	})

	t.Run("NetworkVetted still refuses denylisted address", func(t *testing.T) {
		env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("server must not be contacted")
		}, Config{MaxBytes: 1024})

		_, err := env.fetcher.Open(context.Background(), Request{
			URL: env.url("private.test", "/x"),
		})
		if got := reasonOf(t, err); got != ReasonDeniedAddress {
			t.Fatalf("reason = %s, want %s", got, ReasonDeniedAddress)
		}
		if env.connections() != 0 {
			t.Errorf("connections = %d, want 0", env.connections())
		}
	})
}

func TestOpen_NetworkSystem(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("system-ok"))
	}))
	t.Cleanup(srv.Close)

	f := New(Config{
		Network:      NetworkSystem,
		MaxBytes:     1024,
		AllowedHosts: []string{"127.0.0.1"},
	})
	f.rootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	// Loopback succeeds in NetworkSystem when host is in AllowedHosts.
	resp, err := f.Open(context.Background(), Request{URL: srv.URL + "/ok"})
	if err != nil {
		t.Fatalf("NetworkSystem Open: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "system-ok" {
		t.Fatalf("body = %q, want system-ok", string(body))
	}

	// Still enforces HTTPS only.
	_, err = f.Open(context.Background(), Request{URL: "http://127.0.0.1/nope"})
	if got := reasonOf(t, err); got != ReasonScheme {
		t.Fatalf("http scheme reason = %s, want %s", got, ReasonScheme)
	}

	// Still enforces no userinfo.
	_, err = f.Open(context.Background(), Request{URL: "https://user:pass@127.0.0.1/nope"})
	if got := reasonOf(t, err); got != ReasonUserinfo {
		t.Fatalf("userinfo reason = %s, want %s", got, ReasonUserinfo)
	}

	// Still enforces AllowedHosts.
	_, err = f.Open(context.Background(), Request{URL: "https://evil.example/nope"})
	if got := reasonOf(t, err); got != ReasonHostNotAllowed {
		t.Fatalf("disallowed host reason = %s, want %s", got, ReasonHostNotAllowed)
	}

	// Cross-host redirect in NetworkSystem strips hop-0 Authorization and
	// enforces MaxRedirects.
	t.Run("cross-host redirect strips auth and enforces redirect cap", func(t *testing.T) {
		var hop0Auth, hop1Auth string
		var targetURL string

		targetLn, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		targetSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hop1Auth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("redirected-ok"))
		}))
		targetSrv.Listener = targetLn
		targetSrv.StartTLS()
		t.Cleanup(targetSrv.Close)
		targetURL = targetSrv.URL + "/final"

		startSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/start":
				hop0Auth = r.Header.Get("Authorization")
				http.Redirect(w, r, targetURL, http.StatusFound)
			case "/loop":
				http.Redirect(w, r, "/loop", http.StatusFound)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(startSrv.Close)

		pool := x509.NewCertPool()
		pool.AddCert(startSrv.Certificate())
		pool.AddCert(targetSrv.Certificate())

		sf := New(Config{
			Network:      NetworkSystem,
			MaxBytes:     1024,
			MaxRedirects: 2,
			AllowedHosts: []string{"127.0.0.1", "::1"},
		})
		sf.rootCAs = pool

		resp, err := sf.Open(context.Background(), Request{
			URL:           startSrv.URL + "/start",
			Authorization: "Bearer sys-secret-token",
		})
		if err != nil {
			t.Fatalf("NetworkSystem cross-host redirect Open: %v", err)
		}
		gotBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(gotBody) != "redirected-ok" {
			t.Fatalf("body = %q, want redirected-ok", string(gotBody))
		}
		if hop0Auth != "Bearer sys-secret-token" {
			t.Errorf("hop 0 auth = %q, want Bearer sys-secret-token", hop0Auth)
		}
		if hop1Auth != "" {
			t.Errorf("hop 1 auth leaked across host redirect: %q", hop1Auth)
		}

		// Exceeding MaxRedirects fails with ReasonRedirects.
		_, err = sf.Open(context.Background(), Request{URL: startSrv.URL + "/loop"})
		if got := reasonOf(t, err); got != ReasonRedirects {
			t.Fatalf("redirect loop reason = %s, want %s", got, ReasonRedirects)
		}
	})

	// L1: same-hostname redirect to a different port in NetworkSystem must strip
	// hop-0 Authorization, while canonicalOrigin normalizes default port 443.
	t.Run("same-host different-port redirect strips Authorization", func(t *testing.T) {
		var hop0Auth, hop1Auth string

		otherPortSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hop1Auth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("other-port-ok"))
		}))
		t.Cleanup(otherPortSrv.Close)

		startSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hop0Auth = r.Header.Get("Authorization")
			http.Redirect(w, r, otherPortSrv.URL+"/final", http.StatusFound)
		}))
		t.Cleanup(startSrv.Close)

		pool := x509.NewCertPool()
		pool.AddCert(startSrv.Certificate())
		pool.AddCert(otherPortSrv.Certificate())

		sf := New(Config{
			Network:      NetworkSystem,
			MaxBytes:     1024,
			MaxRedirects: 2,
			AllowedHosts: []string{"127.0.0.1"},
		})
		sf.rootCAs = pool

		resp, err := sf.Open(context.Background(), Request{
			URL:           startSrv.URL + "/start",
			Authorization: "Bearer port-secret-token",
		})
		if err != nil {
			t.Fatalf("NetworkSystem same-host port-change redirect Open: %v", err)
		}
		gotBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(gotBody) != "other-port-ok" {
			t.Fatalf("body = %q, want other-port-ok", string(gotBody))
		}
		if hop0Auth != "Bearer port-secret-token" {
			t.Errorf("hop 0 auth = %q, want Bearer port-secret-token", hop0Auth)
		}
		if hop1Auth != "" {
			t.Errorf("hop 1 auth leaked across port change on same host: %q", hop1Auth)
		}
	})

	t.Run("canonicalOrigin normalizes default port 443 in both modes", func(t *testing.T) {
		vetted := New(Config{Network: NetworkVetted})
		sys := New(Config{Network: NetworkSystem})

		mustParse := func(raw string) *url.URL {
			t.Helper()
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse %q: %v", raw, err)
			}
			return u
		}
		for name, f := range map[string]*Fetcher{"vetted": vetted, "system": sys} {
			if got, want := f.canonicalOrigin(mustParse("https://api.github.com/a")), f.canonicalOrigin(mustParse("https://API.GITHUB.COM.:443/b")); got != want {
				t.Errorf("%s: default port 443 mismatch: %q vs %q", name, got, want)
			}
			if a, b := f.canonicalOrigin(mustParse("https://api.github.com/a")), f.canonicalOrigin(mustParse("https://api.github.com:8443/b")); a == b {
				t.Errorf("%s: different ports must not share origin: both %q", name, a)
			}
		}
	})
}
