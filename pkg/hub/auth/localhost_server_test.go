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

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLocalhostAuthServer_StartAndShutdown(t *testing.T) {
	server := NewLocalhostAuthServer()

	ctx := context.Background()
	callbackURL, state, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if callbackURL == "" {
		t.Error("callbackURL is empty")
	}
	if state == "" {
		t.Error("state is empty")
	}

	// Verify callback URL format
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		t.Fatalf("failed to parse callbackURL: %v", err)
	}
	if parsed.Host != "127.0.0.1:18271" {
		t.Errorf("unexpected host: %s", parsed.Host)
	}
	if parsed.Path != "/callback" {
		t.Errorf("unexpected path: %s", parsed.Path)
	}

	// Shutdown
	if err := server.Shutdown(); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestLocalhostAuthServer_SuccessfulAuth(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18272) // Use different port to avoid conflicts

	ctx := context.Background()
	callbackURL, state, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = server.Shutdown() }()

	// Simulate OAuth callback in a goroutine
	expectedCode := "test-auth-code-12345"
	go func() {
		time.Sleep(100 * time.Millisecond) // Small delay to ensure server is ready

		// Make callback request with valid state and code
		callbackWithParams := callbackURL + "?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(expectedCode)
		resp, err := http.Get(callbackWithParams)
		if err != nil {
			t.Errorf("callback request failed: %v", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("callback returned status %d, want %d", resp.StatusCode, http.StatusOK)
		}
	}()

	// Wait for code
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	code, err := server.WaitForCode(ctx)
	if err != nil {
		t.Fatalf("WaitForCode failed: %v", err)
	}

	if code != expectedCode {
		t.Errorf("code mismatch: got %q, want %q", code, expectedCode)
	}
}

func TestLocalhostAuthServer_StateMismatch(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18273) // Use different port

	ctx := context.Background()
	callbackURL, _, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = server.Shutdown() }()

	// Simulate OAuth callback with wrong state
	go func() {
		time.Sleep(100 * time.Millisecond)

		// Make callback request with wrong state
		callbackWithParams := callbackURL + "?state=wrong-state&code=test-code"
		resp, err := http.Get(callbackWithParams)
		if err != nil {
			t.Errorf("callback request failed: %v", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("callback returned status %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	}()

	// Wait for code - should get an error
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err = server.WaitForCode(ctx)
	if err == nil {
		t.Error("WaitForCode should have failed for state mismatch")
	}
}

func TestLocalhostAuthServer_ErrorResponse(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18274) // Use different port

	ctx := context.Background()
	callbackURL, state, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = server.Shutdown() }()

	// Simulate OAuth error callback
	go func() {
		time.Sleep(100 * time.Millisecond)

		// Make callback request with error
		callbackWithParams := callbackURL + "?state=" + url.QueryEscape(state) + "&error=access_denied&error_description=User+denied+access"
		resp, err := http.Get(callbackWithParams)
		if err != nil {
			t.Errorf("callback request failed: %v", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("callback returned status %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	}()

	// Wait for code - should get an error
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err = server.WaitForCode(ctx)
	if err == nil {
		t.Error("WaitForCode should have failed for error response")
	}
}

func TestLocalhostAuthServer_ErrorPageEscapesDescription(t *testing.T) {
	tests := []struct {
		name    string
		desc    string
		want    string
		notWant string
	}{
		{
			name:    "html special characters",
			desc:    `<b>"a" & 'b'</b>`,
			want:    "<p>&lt;b&gt;&#34;a&#34; &amp; &#39;b&#39;&lt;/b&gt;</p>",
			notWant: `<b>"a" & 'b'</b>`,
		},
		{
			name: "ordinary description",
			desc: "User denied access",
			want: "<p>User denied access</p>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewLocalhostAuthServer()
			server.state = "test-state"

			q := url.Values{}
			q.Set("state", "test-state")
			q.Set("error", "access_denied")
			q.Set("error_description", tt.desc)
			req := httptest.NewRequest(http.MethodGet, CallbackPath+"?"+q.Encode(), nil)
			rec := httptest.NewRecorder()

			server.handleCallback(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.want) {
				t.Errorf("body does not contain %q:\n%s", tt.want, body)
			}
			if tt.notWant != "" && strings.Contains(body, tt.notWant) {
				t.Errorf("body contains unescaped %q:\n%s", tt.notWant, body)
			}
		})
	}
}

func TestLocalhostAuthServer_Timeout(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18275) // Use different port

	ctx := context.Background()
	_, _, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = server.Shutdown() }()

	// Wait for code with short timeout - should timeout
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	_, err = server.WaitForCode(ctx)
	if err == nil {
		t.Error("WaitForCode should have timed out")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestLocalhostAuthServer_DoubleStart(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18276)

	ctx := context.Background()
	_, _, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("First Start failed: %v", err)
	}
	defer func() { _ = server.Shutdown() }()

	// Second start should fail
	_, _, err = server.Start(ctx)
	if err == nil {
		t.Error("Second Start should have failed")
	}
}

func TestGenerateRandomState(t *testing.T) {
	state1, err := generateRandomState()
	if err != nil {
		t.Fatalf("generateRandomState failed: %v", err)
	}

	state2, err := generateRandomState()
	if err != nil {
		t.Fatalf("generateRandomState failed: %v", err)
	}

	if state1 == "" || state2 == "" {
		t.Error("state should not be empty")
	}

	if state1 == state2 {
		t.Error("states should be unique")
	}
}

func TestLocalhostAuthServer_CallbackBrandingAndSecurity(t *testing.T) {
	t.Run("default unbranded uses theme variables without legacy gradients", func(t *testing.T) {
		srv := NewLocalhostAuthServer()
		srv.state = "st"
		req := httptest.NewRequest(http.MethodGet, CallbackPath+"?state=st&code=abc", nil)
		rec := httptest.NewRecorder()
		srv.handleCallback(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("Content-Type = %q", ct)
		}
		if nosniff := rec.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q", nosniff)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q", cc)
		}
		body := rec.Body.String()
		for _, legacy := range []string{"#667eea", "#764ba2", "#e74c3c", "#c0392b"} {
			if strings.Contains(body, legacy) {
				t.Errorf("callback HTML still contains legacy gradient color %s", legacy)
			}
		}
		if !strings.Contains(body, "@media (prefers-color-scheme: dark)") {
			t.Errorf("expected dark mode media query in callback HTML")
		}
		if !strings.Contains(body, `<span class="badge">scion</span>`) {
			t.Errorf("expected stock scion badge:\n%s", body)
		}
		if strings.Contains(body, "Powered by Scion") {
			t.Errorf("unbranded default must not show Powered by Scion")
		}
	})

	t.Run("branded with attribution shown and light/dark logos", func(t *testing.T) {
		srv := NewLocalhostAuthServer().WithBranding(&CallbackBranding{
			Name:        "Acme Cloud",
			Attribution: "shown",
			LogoLight:   "https://hub.example.com/brand/logo-light?v=12345678",
			LogoDark:    "https://hub.example.com/brand/logo-dark?v=87654321",
			Light: CallbackColors{
				Accent: "#0f766e",
			},
			Dark: CallbackColors{
				Bg:     "#0c0a09",
				Accent: "#2dd4bf",
			},
		})
		srv.state = "st"
		req := httptest.NewRequest(http.MethodGet, CallbackPath+"?state=st&code=abc", nil)
		rec := httptest.NewRecorder()
		srv.handleCallback(rec, req)

		body := rec.Body.String()
		for _, want := range []string{
			"<title>Acme Cloud - Authentication Successful</title>",
			"--accent: #0f766e;",
			"--bg: #0c0a09;",
			"--accent: #2dd4bf;",
			`<source media="(prefers-color-scheme: dark)" srcset="https://hub.example.com/brand/logo-dark?v=87654321">`,
			`<img class="brand-logo" src="https://hub.example.com/brand/logo-light?v=12345678" alt="Acme Cloud">`,
			`<span class="badge">Acme Cloud · Powered by Scion</span>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("branded with attribution hidden omits Powered by Scion", func(t *testing.T) {
		srv := NewLocalhostAuthServer().WithBranding(&CallbackBranding{
			Name:        "Acme Cloud",
			Attribution: "hidden",
		})
		srv.state = "st"
		req := httptest.NewRequest(http.MethodGet, CallbackPath+"?state=st&code=abc", nil)
		rec := httptest.NewRecorder()
		srv.handleCallback(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, `<span class="badge">Acme Cloud</span>`) {
			t.Errorf("expected Acme Cloud badge without attribution:\n%s", body)
		}
		if strings.Contains(body, "Powered by Scion") {
			t.Errorf("hidden attribution must omit Powered by Scion")
		}
	})

	t.Run("sanitizes malicious colors, logo URLs, and HTML in brand name", func(t *testing.T) {
		srv := NewLocalhostAuthServer().WithBranding(&CallbackBranding{
			Name:      `<script>alert(1)</script>`,
			LogoLight: `javascript:alert(1)`,
			LogoDark:  `http://evil.example.com/logo.svg`,
			Light: CallbackColors{
				Bg:     `#fff; } body { background: url(https://evil.example/x); }`,
				Accent: `red`,
			},
		})
		srv.state = "st"
		req := httptest.NewRequest(http.MethodGet, CallbackPath+"?state=st&code=abc", nil)
		rec := httptest.NewRecorder()
		srv.handleCallback(rec, req)

		body := rec.Body.String()
		for _, forbidden := range []string{"<script>", "javascript:", "evil.example", `<img class="brand-logo"`} {
			if strings.Contains(body, forbidden) {
				t.Errorf("body must not contain unsanitized %q:\n%s", forbidden, body)
			}
		}
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("expected HTML-escaped brand name in body")
		}
		if !strings.Contains(body, "--bg: #f8fafc;") || !strings.Contains(body, "--accent: #3b82f6;") {
			t.Errorf("invalid hex colors must fall back to stock defaults:\n%s", body)
		}
	})
}
