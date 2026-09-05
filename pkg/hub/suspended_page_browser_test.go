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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSuspendedPage_HeadlessBrowser_ZeroFanOut is the required headless-browser
// acceptance test. It starts a real HTTP test server with the full middleware
// chain, navigates to it with a headless Chromium browser, and verifies:
//
//  1. The dedicated suspended page renders (page title, visible text).
//  2. ZERO protected API requests, SSE connections, SPA module loads, or
//     route prefetches are initiated by the browser.
//
// The test uses Chrome DevTools Protocol (via chromedp) to observe all network
// requests the browser makes during the initial page load.
func TestSuspendedPage_HeadlessBrowser_ZeroFanOut(t *testing.T) {
	// Skip if chromium is not available.
	if _, err := exec.LookPath("chromium"); err != nil {
		if _, err2 := exec.LookPath("google-chrome"); err2 != nil {
			t.Skip("headless browser test requires chromium or google-chrome")
		}
	}

	// Set up suspended user.
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "suspended@example.com",
		Role:   "member",
		Status: "suspended",
	})

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)

	// Create session cookies.
	cookies := loginSession(t, ws, "user-1", "suspended@example.com", "member")

	// Start a real HTTP test server.
	handler := ws.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Track all network requests made by the browser.
	var (
		mu       sync.Mutex
		requests []string // URLs of all requests
	)

	// Set up chromedp with headless browser.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	// Set a timeout for the entire browser operation.
	ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Listen for network request events to capture all URLs.
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if req, ok := ev.(*network.EventRequestWillBeSent); ok {
			mu.Lock()
			requests = append(requests, req.Request.URL)
			mu.Unlock()
		}
	})

	// Navigate and wait for the page to be stable.
	var pageTitle string
	var pageText string
	var pageHTML string

	err := chromedp.Run(ctx,
		// Enable network domain to receive events.
		network.Enable(),
		// Set session cookies on the test server domain.
		chromedp.ActionFunc(func(ctx context.Context) error {
			for _, c := range cookies {
				err := network.SetCookie(c.Name, c.Value).
					WithURL(ts.URL).
					WithPath("/").
					Do(ctx)
				if err != nil {
					return fmt.Errorf("set cookie %s: %w", c.Name, err)
				}
			}
			return nil
		}),
		// Navigate to a protected page.
		chromedp.Navigate(ts.URL+"/projects"),
		// Wait for the page to finish loading.
		chromedp.WaitReady("body"),
		// Give time for any async requests (scripts, SSE, etc.) to fire.
		chromedp.Sleep(1*time.Second),
		// Extract page content.
		chromedp.Title(&pageTitle),
		chromedp.Text("body", &pageText),
		chromedp.OuterHTML("html", &pageHTML),
	)
	require.NoError(t, err, "headless browser navigation should succeed")

	// Assertion 1: The dedicated suspended page rendered.
	assert.Contains(t, pageTitle, "Account Suspended",
		"page title should indicate account suspension")
	assert.Contains(t, pageText, "Account Suspended",
		"page should show the suspended message")
	assert.Contains(t, pageText, "suspended@example.com",
		"page should show the user's email")
	assert.Contains(t, pageHTML, "/auth/logout",
		"page should contain sign-out link")

	// Assertion 2: No SPA bootstrap artifacts in the rendered HTML.
	assert.NotContains(t, pageHTML, "__SCION_DATA__",
		"suspended page must not contain prefetch data")
	assert.NotContains(t, pageHTML, `<script type="module"`,
		"suspended page must not contain module script tags")
	assert.NotContains(t, pageHTML, "main.js",
		"suspended page must not reference SPA entry point")
	assert.NotContains(t, pageHTML, "<scion-app",
		"suspended page must not contain SPA root element")

	// Assertion 3: ZERO protected requests were initiated.
	mu.Lock()
	defer mu.Unlock()

	protectedPrefixes := []string{"/api/v1/", "/events"}
	spaAssets := []string{"main.js", "chunk-"}

	for _, reqURL := range requests {
		// Skip requests to external domains (CDN, etc.) — we only care
		// about requests to our test server.
		if !strings.HasPrefix(reqURL, ts.URL) {
			continue
		}
		localPath := strings.TrimPrefix(reqURL, ts.URL)

		// Check for protected API/SSE requests.
		for _, prefix := range protectedPrefixes {
			if strings.HasPrefix(localPath, prefix) {
				t.Errorf("VIOLATION: browser initiated protected request: %s", localPath)
			}
		}
		// Check for SPA module/chunk loads.
		for _, asset := range spaAssets {
			if strings.Contains(localPath, asset) {
				t.Errorf("VIOLATION: browser loaded SPA asset: %s", localPath)
			}
		}
	}

	t.Logf("Browser made %d total network requests", len(requests))
	for i, u := range requests {
		t.Logf("  [%d] %s", i, u)
	}
}

// TestSuspendedPage_NetworkLevel_RealHTTP is a network-level acceptance test
// that validates the suspended page serves correctly over the real HTTP stack
// (not just httptest.ResponseRecorder). It uses Go's http.Client to make real
// TCP requests and verify headers, status, and content.
func TestSuspendedPage_NetworkLevel_RealHTTP(t *testing.T) {
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "suspended@example.com",
		Role:   "member",
		Status: "suspended",
	})

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)
	handler := ws.Handler()

	// Start a real HTTP test server.
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Create session cookies.
	cookies := loginSession(t, ws, "user-1", "suspended@example.com", "member")

	// Make requests with various Accept headers and verify responses.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects.
		},
	}

	tests := []struct {
		name     string
		accept   string
		path     string
		wantJSON bool
	}{
		{"Browser navigation", "text/html", "/projects", false},
		{"SSE connection", "text/event-stream", "/events?sub=project.123.>", true},
		{"API fetch", "application/json", "/projects", true},
		{"Bare fetch", "*/*", "/projects", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", ts.URL+tc.path, nil)
			require.NoError(t, err)
			req.Header.Set("Accept", tc.accept)
			for _, c := range cookies {
				req.AddCookie(c)
			}

			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusForbidden, resp.StatusCode,
				"suspended user should get 403")

			if tc.wantJSON {
				assert.Contains(t, resp.Header.Get("Content-Type"), "application/json",
					"non-browser request should get JSON")
			} else {
				assert.Contains(t, resp.Header.Get("Content-Type"), "text/html",
					"browser request should get HTML")
				assert.Equal(t, "no-cache, no-store, must-revalidate",
					resp.Header.Get("Cache-Control"),
					"suspended page must have no-store cache headers")
			}
		})
	}

	// Verify that ALL protected paths on the real server return 403.
	protectedPaths := []string{"/", "/projects", "/agents", "/skills"}
	for _, path := range protectedPaths {
		t.Run(fmt.Sprintf("Protected path %s", path), func(t *testing.T) {
			req, err := http.NewRequest("GET", ts.URL+path, nil)
			require.NoError(t, err)
			req.Header.Set("Accept", "text/html")
			for _, c := range cookies {
				req.AddCookie(c)
			}

			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusForbidden, resp.StatusCode,
				"protected path %s must return 403 for suspended user", path)
		})
	}
}
