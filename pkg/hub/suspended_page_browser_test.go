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
	// Agent sandboxes cap virtual memory (ulimit -v) to contain runaway test
	// binaries. Chromium cannot launch under any such cap, so the test would
	// fail with "chrome failed to start" for a reason unrelated to the page.
	// CI runners set no cap, so the test still runs there.
	if limit, ok := addressSpaceLimit(); ok {
		t.Skipf("headless Chromium cannot start under an address-space limit (RLIMIT_AS = %s, e.g. from ulimit -v)", limit)
	}

	// Set up suspended user.
	st := newProxyAuthStore()
	_ = st.CreateUser(context.Background(), &store.User{
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

	// Start the headless browser. startHeadlessBrowser bounds and retries
	// the Chrome launch and logs Chrome's own output on a failed attempt.
	ctx := startHeadlessBrowser(t)

	// Bound the page interaction separately from the launch.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
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

// Launch bounds for startHeadlessBrowser. Chrome normally prints its
// DevTools URL within a second; several CPU-bound processes per core slow
// that to a few seconds. CI has seen rare launches that never print it within
// 60 s (ptone/scion#3412). The cause is unknown: Chrome's output was not
// captured then. So each launch attempt is bounded and retried, and each
// failed attempt logs Chrome's output for diagnosis.
//
// browserLaunchTimeout bounds the wait for the DevTools URL. The whole
// attempt (starting Chrome, reading the URL, connecting to the browser and
// attaching to its first tab) is bounded by browserLaunchTimeout plus
// browserAttachGrace.
const (
	browserLaunchAttempts = 3
	browserLaunchTimeout  = 30 * time.Second
	browserAttachGrace    = 10 * time.Second
)

// startHeadlessBrowser launches headless Chrome and returns a chromedp context
// bound to it. Each attempt runs a fresh browser process with a fresh
// profile and is bounded as a whole (see browserAttachGrace). An attempt that
// fails or runs out of time is killed, its combined Chrome output is logged,
// and the launch is retried up to browserLaunchAttempts times. Only the
// launch is retried; navigation and assertions run once on the returned
// context, under the caller's own bound. The browser is shut down via
// t.Cleanup.
func startHeadlessBrowser(t *testing.T) context.Context {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= browserLaunchAttempts; attempt++ {
		output := &syncBuffer{}
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("disable-extensions", true),
			chromedp.Flag("disable-background-networking", true),
			chromedp.WSURLReadTimeout(browserLaunchTimeout),
			chromedp.CombinedOutput(output),
		)
		allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
		ctx, cancel := chromedp.NewContext(allocCtx)

		// Bound the whole attempt with a timer that cancels ctx, not with
		// context.WithTimeout: chromedp ties the browser's lifetime to the
		// context of the first Run, so a deadline there would also end the
		// browser handed back to the caller.
		timer := time.AfterFunc(browserLaunchTimeout+browserAttachGrace, cancel)

		// A Run with no actions only launches the browser and attaches to
		// its first tab.
		start := time.Now()
		err := chromedp.Run(ctx)
		if !timer.Stop() && err == nil {
			// The timer fired as Run returned, so ctx is already canceled.
			err = fmt.Errorf("launch did not finish within %s", browserLaunchTimeout+browserAttachGrace)
		}
		if err == nil {
			t.Logf("headless browser started in %s (attempt %d)", time.Since(start).Round(time.Millisecond), attempt)
			t.Cleanup(func() {
				cancel()
				allocCancel()
			})
			return ctx
		}
		lastErr = err
		cancel()
		allocCancel() // kills the Chrome process and waits for it to exit
		t.Logf("headless browser launch attempt %d/%d failed after %s: %v\nChrome output:\n%s",
			attempt, browserLaunchAttempts, time.Since(start).Round(time.Millisecond), err, output.String())
	}
	t.Fatalf("headless browser failed to start after %d attempts: %v", browserLaunchAttempts, lastErr)
	return nil
}

// TestSuspendedPage_NetworkLevel_RealHTTP is a network-level acceptance test
// that validates the suspended page serves correctly over the real HTTP stack
// (not just httptest.ResponseRecorder). It uses Go's http.Client to make real
// TCP requests and verify headers, status, and content.
func TestSuspendedPage_NetworkLevel_RealHTTP(t *testing.T) {
	st := newProxyAuthStore()
	_ = st.CreateUser(context.Background(), &store.User{
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
			defer func() { _ = resp.Body.Close() }()

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
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusForbidden, resp.StatusCode,
				"protected path %s must return 403 for suspended user", path)
		})
	}
}
