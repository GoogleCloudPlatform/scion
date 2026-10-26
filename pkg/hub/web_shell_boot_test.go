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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shellBootManifest is a Vite manifest shaped like a real build: the entry
// imports the shoelace and lit chunks statically (shoelace imports lit
// too), a page chunk dynamically, and a few names that must never become a
// preload hint.
const shellBootManifest = `{
  "src/client/main.ts": {"file": "assets/main.js", "isEntry": true,
    "imports": ["_shoelace-CBLC-ROG.js", "_lit-BwvvpRRW.js", "_missing-AbCdEfGh.js", "_bad-1", "_bad-2", "_bad-3", "_shoelace-BZzytDYN.css"],
    "dynamicImports": ["src/components/pages/project-detail.ts"]},
  "_shoelace-CBLC-ROG.js": {"file": "assets/shoelace-CBLC-ROG.js", "imports": ["_lit-BwvvpRRW.js"], "css": ["assets/shoelace-BZzytDYN.css"]},
  "_lit-BwvvpRRW.js": {"file": "assets/lit-BwvvpRRW.js", "imports": ["src/client/main.ts"]},
  "_shoelace-BZzytDYN.css": {"file": "assets/shoelace-BZzytDYN.css"},
  "src/components/pages/project-detail.ts": {"file": "assets/project-detail-CeCkJL--.js", "isDynamicEntry": true, "imports": ["src/client/main.ts", "_lit-BwvvpRRW.js"]},
  "_bad-1": {"file": "../outside-AbCdEfGh.js"},
  "_bad-2": {"file": "https://cdn.example.com/x-AbCdEfGh.js"},
  "_bad-3": {"file": "assets/x\"><script>alert(1)</script>-AbCdEfGh.js"}
}`

// shellHead returns the part of a shell document up to and including
// </head>, failing the test if there is none.
func shellHead(t *testing.T, html string) string {
	t.Helper()
	end := strings.Index(html, "</head>")
	require.GreaterOrEqual(t, end, 0, "no </head> in the shell")
	return html[:end+len("</head>")]
}

// TestSPAShell_ModulePreloadFromManifest: the shell hints the chunks the
// entry imports statically, read from the Vite manifest at startup, and
// nothing else: not the entry, not dynamic imports, not CSS, not paths
// outside the build, not names with characters outside the URL-safe set.
func TestSPAShell_ModulePreloadFromManifest(t *testing.T) {
	dir := writeFingerprintFixture(t, shellBootManifest)
	ws := newTestWebServer(t, WebServerConfig{AssetsDir: dir})

	assert.Equal(t, []string{"/assets/shoelace-CBLC-ROG.js", "/assets/lit-BwvvpRRW.js"},
		ws.shellModulePreloads, "static imports, depth first, each once")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	ws.spaHandler()(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	head := shellHead(t, rec.Body.String())

	assert.Contains(t, head, `<link rel="modulepreload" href="/assets/shoelace-CBLC-ROG.js">`)
	assert.Contains(t, head, `<link rel="modulepreload" href="/assets/lit-BwvvpRRW.js">`)
	assert.Equal(t, 2, strings.Count(head, `rel="modulepreload"`))
	assert.NotContains(t, head, `modulepreload" href="/assets/main.js"`, "the entry is requested by its script, not preloaded")
	assert.NotContains(t, head, "project-detail", "dynamic imports load on demand")
	assert.NotContains(t, head, "outside-AbCdEfGh")
	assert.NotContains(t, head, "cdn.example.com")
	assert.NotContains(t, head, `rel="stylesheet" href="/assets/`, "chunk CSS is applied by the client, not linked")
	assert.NotContains(t, head, "alert(1)", "a manifest name with markup is skipped")
	assert.Contains(t, head, `<script type="module" src="/assets/main.js"></script>`, "the entry script is in the head")
	assert.Less(t, strings.Index(head, `rel="modulepreload"`), strings.Index(head, `src="/assets/main.js"`))

	t.Run("no manifest, no hints", func(t *testing.T) {
		ws := newTestWebServer(t, WebServerConfig{AssetsDir: writeFingerprintFixture(t, "")})
		assert.Nil(t, ws.shellModulePreloads)
		rec := httptest.NewRecorder()
		ws.spaHandler()(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		assert.NotContains(t, rec.Body.String(), "modulepreload")
		assert.Contains(t, shellHead(t, rec.Body.String()), `<script type="module" src="/assets/main.js"></script>`)
	})

	t.Run("no entry in the manifest, no hints", func(t *testing.T) {
		assert.Nil(t, shellModulePreloadsFromManifest(map[string]viteManifestChunk{
			"_lit-BwvvpRRW.js": {File: "assets/lit-BwvvpRRW.js"},
			"other.ts":         {File: "assets/other-AbCdEfGh.js", IsEntry: true, Imports: []string{"_lit-BwvvpRRW.js"}},
		}))
	})
}

// TestSPAShell_HeadFlushedBeforePrefetch: the client receives the whole
// head while the page data prefetch is still blocked, and the rest of the
// document, with the prefetched data, once it is released.
func TestSPAShell_HeadFlushedBeforePrefetch(t *testing.T) {
	dir := writeFingerprintFixture(t, shellBootManifest)
	ws := newDevAuthWebServer(t, func(cfg *WebServerConfig) { cfg.AssetsDir = dir })
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)

	entered := make(chan struct{})
	var enteredOnce sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHub := func() { releaseOnce.Do(func() { close(release) }) }
	agentID := tid("flush-agent")
	ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		// Block on the test only, not on the prefetch deadline, so the
		// head can only have arrived through the flush.
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": agentID, "name": "flush-agent"})
	}), func(ctx context.Context) error { return nil })

	srv := httptest.NewServer(devSignedIn(t, ws))
	defer srv.Close()
	// Runs before srv.Close (defers run last-in first-out), so a failing
	// test never leaves the handler blocked and Close waiting on it.
	defer releaseHub()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/agents/"+agentID, nil)
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	policy := resp.Header.Get("Content-Security-Policy")
	assert.Equal(t, webContentSecurityPolicy(policyNonce(t, policy)), policy)
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))

	// Read up to </head> while the prefetch is still blocked.
	br := bufio.NewReader(resp.Body)
	headCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		var sb strings.Builder
		for !strings.HasSuffix(sb.String(), "</head>") {
			b, err := br.ReadByte()
			if err != nil {
				errCh <- err
				return
			}
			sb.WriteByte(b)
		}
		headCh <- sb.String()
	}()
	var head string
	select {
	case head = <-headCh:
	case err := <-errCh:
		t.Fatalf("reading the head: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the head did not arrive while the prefetch was blocked")
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the prefetch never started")
	}
	assert.Contains(t, head, `<link rel="modulepreload" href="/assets/shoelace-CBLC-ROG.js">`)
	assert.Contains(t, head, `src="/assets/main.js"`)
	assert.NotContains(t, head, "__SCION_DATA__")
	assert.NotContains(t, head, "flush-agent")

	releaseHub()
	rest, err := io.ReadAll(br)
	require.NoError(t, err)
	assert.Contains(t, string(rest), `"flush-agent"`, "the prefetched data follows the head")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(string(rest)), "</html>"))
}

// TestSPAShell_InitialDataAtEndOfBody: __SCION_DATA__ is the last element
// of the body, after the app element, and the head carries no per-request
// data (no user, no path, no prefetched data).
func TestSPAShell_InitialDataAtEndOfBody(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{AssetsDir: writeFingerprintFixture(t, shellBootManifest)})
	req := httptest.NewRequest(http.MethodGet, "/projects/p-marker", nil)
	req = req.WithContext(setWebSessionUser(req.Context(), &webSessionUser{
		UserID: "u-marker", Email: "marker@example.com", Name: "Marker Name",
	}))
	rec := httptest.NewRecorder()
	ws.spaHandler()(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	html := rec.Body.String()

	head := shellHead(t, html)
	for _, v := range []string{"__SCION_DATA__", "u-marker", "marker@example.com", "Marker Name", "p-marker"} {
		assert.NotContains(t, head, v)
	}

	const open = `<script id="__SCION_DATA__" type="application/json">`
	dataAt := strings.Index(html, open)
	require.Greater(t, dataAt, len(head))
	assert.Less(t, strings.Index(html, `<div id="app">`), dataAt, "the data follows the app element")
	afterData := html[dataAt+len(open):]
	end := strings.Index(afterData, "</script>")
	require.GreaterOrEqual(t, end, 0)
	assert.Equal(t, "</script>\n</body>\n</html>", afterData[end:], "the data script is the last element of the body")
	fields := shellInitialData(t, html)
	assert.JSONEq(t, `"/projects/p-marker"`, string(fields["path"]))
	assert.Contains(t, string(fields["user"]), "marker@example.com")

	// Executing the whole template (head then body) gives the same layout.
	whole := renderSPAShell(t)
	assert.Less(t, strings.Index(whole, "</head>"), strings.Index(whole, open))
}

// TestSPAShell_SpecialCharactersStayInsideInitialData: special characters in
// the request path, the session user and the prefetched API response
// cannot end the data script, open a comment or another element, or break
// the JSON; each comes back unchanged when the JSON is parsed.
func TestSPAShell_SpecialCharactersStayInsideInitialData(t *testing.T) {
	const special = "</script><script>alert(1)</script><!--<script \u2028\u2029<>&'\"`${x}"
	ws := newTestWebServer(t, WebServerConfig{})
	ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "p-1", "name": special, "description": special})
	}), func(ctx context.Context) error { return nil })

	benignRec := httptest.NewRecorder()
	ws.spaHandler()(benignRec, httptest.NewRequest(http.MethodGet, "/projects/p-1", nil))
	benign := benignRec.Body.String()

	for _, path := range []string{"/projects/p-1", "/x%3C%2Fscript%3E%3Cscript%3Ealert(1)%3C%2Fscript%3E%3C!--%E2%80%A8"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(setWebSessionUser(req.Context(), &webSessionUser{
				UserID: "u-1", Email: special + "@example.com", Name: special, AvatarURL: "https://example.com/" + special, Role: special,
			}))
			rec := httptest.NewRecorder()
			ws.spaHandler()(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
			html := rec.Body.String()

			// The markup around the data is exactly the benign shell's.
			assert.Equal(t, strings.Count(benign, "<script"), strings.Count(html, "<script"), "no extra script element")
			assert.Equal(t, strings.Count(benign, "</script>"), strings.Count(html, "</script>"), "no extra script end")
			assert.Equal(t, strings.Count(benign, "<!--"), strings.Count(html, "<!--"), "no extra comment")
			assert.NotContains(t, html, "\u2028")
			assert.NotContains(t, html, "\u2029")
			const open = `<script id="__SCION_DATA__" type="application/json">`
			data := html[strings.Index(html, open)+len(open):]
			data = data[:strings.Index(data, "</script>")]
			assert.NotContains(t, data, "<")
			assert.NotContains(t, data, ">")
			assert.NotContains(t, data, "&")

			fields := shellInitialData(t, html)
			var user map[string]string
			require.NoError(t, json.Unmarshal(fields["user"], &user))
			assert.Equal(t, special, user["name"])
			assert.Equal(t, special+"@example.com", user["email"])
			assert.Equal(t, special, user["role"])
			var gotPath string
			require.NoError(t, json.Unmarshal(fields["path"], &gotPath))
			assert.Equal(t, req.URL.Path, gotPath)
			if path == "/projects/p-1" {
				var apiData map[string]string
				require.NoError(t, json.Unmarshal(fields["data"], &apiData))
				assert.Equal(t, special, apiData["name"])
			}
		})
	}
}

// failAfterFlushWriter records what is written until Flush is called, then
// fails every write, standing in for a connection that breaks after the
// head is sent.
type failAfterFlushWriter struct {
	header  http.Header
	written strings.Builder
	flushed bool
}

func (w *failAfterFlushWriter) Header() http.Header { return w.header }
func (w *failAfterFlushWriter) WriteHeader(int)     {}
func (w *failAfterFlushWriter) Flush()              { w.flushed = true }
func (w *failAfterFlushWriter) Write(p []byte) (int, error) {
	if w.flushed {
		return 0, errors.New("connection broken")
	}
	return w.written.Write(p)
}

// TestSPAShell_FailureAfterFlush: once the head is sent, neither a
// prefetch that panics nor a write error puts anything but the shell into
// the body. A panicking prefetch degrades to no page data and a complete
// document; a broken connection leaves only the head written.
func TestSPAShell_FailureAfterFlush(t *testing.T) {
	const secret = "PREFETCH-PANIC-DETAIL-7f3a"

	t.Run("prefetch panics", func(t *testing.T) {
		ws := newTestWebServer(t, WebServerConfig{})
		ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":"p-1","name":"partial`))
			panic(secret)
		}), func(ctx context.Context) error { return nil })
		req := httptest.NewRequest(http.MethodGet, "/projects/p-1", nil)
		req = req.WithContext(setWebSessionUser(req.Context(), &webSessionUser{UserID: "u-1", Email: "u@example.com"}))
		rec := httptest.NewRecorder()
		require.NotPanics(t, func() { ws.spaHandler()(rec, req) })
		html := rec.Body.String()
		assert.True(t, rec.Flushed, "the head was flushed")
		assert.NotContains(t, html, secret)
		assert.NotContains(t, html, "partial")
		assert.True(t, strings.HasSuffix(html, "</body>\n</html>"), "a complete document")
		fields := shellInitialData(t, html)
		assert.NotContains(t, fields, "data", "the panic degrades to no page data")
		assert.Contains(t, fields, "user")
	})

	t.Run("write fails after the flush", func(t *testing.T) {
		ws := newTestWebServer(t, WebServerConfig{})
		w := &failAfterFlushWriter{header: http.Header{}}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(setWebSessionUser(req.Context(), &webSessionUser{UserID: "u-1", Email: "u@example.com"}))
		require.NotPanics(t, func() { ws.spaHandler()(w, req) })
		assert.True(t, w.flushed)
		assert.True(t, strings.HasSuffix(w.written.String(), "</head>\n"), "only the head was written")
		assert.NotContains(t, w.written.String(), "u@example.com")
		assert.NotContains(t, w.written.String(), "__SCION_DATA__")
		assert.Equal(t, "no-store", w.header.Get("Cache-Control"), "headers are set before the first write")
	})
}
