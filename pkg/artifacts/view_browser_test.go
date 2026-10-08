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

package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The browser probe tests run a real headless Chromium against the view
// route. They are skipped when no Chromium is found (set SCION_TEST_CHROMIUM
// to its path, or install it as chromium).

func chromiumPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("SCION_TEST_CHROMIUM"); p != "" {
		return p
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no Chromium found; set SCION_TEST_CHROMIUM to run the browser probe")
	return ""
}

// dumpDOM loads url in headless Chromium, waits (in real time) until the
// JavaScript expression ready is true, and returns the page's DOM. Site
// isolation is turned off only to match the earlier probe setup; the
// sandbox and CSP checks are made by the browser either way.
func dumpDOM(t *testing.T, chromium, url, ready string) string {
	t.Helper()
	// One overall limit per subtest, covering a retried browser start.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dom, err := browserDOM(ctx, t.Logf, func(ctx context.Context) (string, error) {
		return runBrowser(ctx, chromium, url, ready)
	})
	if err != nil {
		t.Fatalf("chromium: %v (DOM so far:\n%s)", err, dom)
	}
	return dom
}

// browserStartTimeout is the error chromedp returns when the browser does
// not report its DevTools address in time (chromedp does not export it).
const browserStartTimeout = "websocket url timeout reached"

// browserDOM runs run, and runs it once more only if the browser did not
// start in time, as happens on a loaded CI runner. Any other error, such
// as a page that never reports its result, is returned as it is.
func browserDOM(ctx context.Context, logf func(string, ...any), run func(context.Context) (string, error)) (string, error) {
	dom, err := run(ctx)
	if err != nil && strings.Contains(err.Error(), browserStartTimeout) {
		logf("browser did not start in time; starting it once more")
		dom, err = run(ctx)
	}
	return dom, err
}

// runBrowser starts a browser of its own, loads url and returns the DOM
// once ready is true.
func runBrowser(ctx context.Context, chromium, url, ready string) (string, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromium),
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-site-isolation-trials", true),
		chromedp.Flag("disable-features", "IsolateOrigins,site-per-process"),
		// CI runners can be slow to start a browser.
		chromedp.WSURLReadTimeout(60*time.Second),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()
	bctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	var dom string
	err := chromedp.Run(bctx,
		chromedp.Navigate(url),
		chromedp.Poll(ready, nil, chromedp.WithPollingInterval(100*time.Millisecond)),
		chromedp.OuterHTML("html", &dom, chromedp.ByQuery),
	)
	return dom, err
}

// TestBrowserDOMRetriesStartOnce: a browser that did not start in time is
// started once more; other errors and a second start timeout are returned.
func TestBrowserDOMRetriesStartOnce(t *testing.T) {
	startTimeout := errors.New(browserStartTimeout)
	cases := []struct {
		name    string
		results []error
		calls   int
		wantErr bool
	}{
		{"start timeout then success", []error{startTimeout, nil}, 2, false},
		{"start timeout twice", []error{startTimeout, startTimeout}, 2, true},
		{"other error is not retried", []error{errors.New("context deadline exceeded"), nil}, 1, true},
		{"success", []error{nil}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			dom, err := browserDOM(context.Background(), t.Logf, func(context.Context) (string, error) {
				e := tc.results[calls]
				calls++
				if e != nil {
					return "", e
				}
				return "<html></html>", nil
			})
			if calls != tc.calls {
				t.Errorf("run called %d times, want %d", calls, tc.calls)
			}
			if (err != nil) != tc.wantErr || (err == nil && dom != "<html></html>") {
				t.Errorf("got (%q, %v), want error %v", dom, err, tc.wantErr)
			}
		})
	}
}

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// probePage is an HTML bundle entry that tries everything the sandbox and
// the view CSP must allow or deny, and reports what happened to its parent
// (postMessage) and on its own root element (data-probe).
const probePage = `<!doctype html><html><head><meta charset="utf-8">
<script>
const r = { inlineScript: 'ran', origin: String(self.origin), violations: [] };
document.addEventListener('securitypolicyviolation', (e) => {
  r.violations.push(e.effectiveDirective);
  if (String(e.blockedURI).endsWith('/cookie-check.png')) r.outsideViolation = e.effectiveDirective;
});
</script>
<link rel="stylesheet" href="css/s.css">
<script src="https://remote.invalid/x.js"></script>
</head><body>
<img id="local" src="img/a.png">
<img id="remote" src="https://remote.invalid/x.png">
<img id="hubimg" src="/cookie-check.png">
<script src="js/app.js"></script>
<iframe id="nested" src="docs/page.htm"></iframe>
<object id="obj" data="docs/page.htm"></object>
<form id="form" action="/submit" method="post"><input name="a" value="b"></form>
<script>
try { r.parentDOM = parent === self ? 'top' : String(parent.document.title); } catch (e) { r.parentDOM = 'blocked'; }
try { r.cookie = 'read:' + document.cookie; } catch (e) { r.cookie = 'blocked'; }
try { localStorage.getItem('parent'); r.storage = 'read'; } catch (e) { r.storage = 'blocked'; }
try { r.popup = window.open('about:blank') ? 'opened' : 'blocked'; } catch (e) { r.popup = 'blocked'; }
try { if (parent !== self) { top.location.href = top.location.href + '#navigated'; } r.topNav = 'attempted'; } catch (e) { r.topNav = 'blocked'; }
try { document.getElementById('form').submit(); } catch (e) {}
fetch('../../../ARTIFACT_PATH').then(() => { r.fetch = 'allowed'; }, () => { r.fetch = 'blocked'; });
setTimeout(() => {
  r.local = document.getElementById('local').naturalWidth > 0 ? 'loaded' : 'failed';
  r.remote = document.getElementById('remote').naturalWidth > 0 ? 'loaded' : 'blocked';
  r.outsideView = document.getElementById('hubimg').naturalWidth > 0 ? 'loaded' : 'blocked';
  r.css = getComputedStyle(document.body).backgroundColor;
  r.violations = Array.from(new Set(r.violations)).sort();
  const s = JSON.stringify(r);
  document.documentElement.setAttribute('data-probe', s);
  if (parent !== self) parent.postMessage(s, '*');
}, 500);
</script></body></html>`

// probeHost is the page that frames the view the way the web UI does.
const probeHost = `<!doctype html><html><head><title>host-secret</title></head><body>
<pre id="result">pending</pre>
<script>
document.cookie = 'probe=parent-secret; path=/';
localStorage.setItem('parent', 'secret');
window.addEventListener('message', (e) => {
  document.getElementById('result').textContent = e.data;
  document.getElementById('result').setAttribute('data-location', location.href);
});
const f = document.createElement('iframe');
f.setAttribute('sandbox', SANDBOX);
f.setAttribute('referrerpolicy', 'no-referrer');
f.src = VIEW_URL;
document.body.appendChild(f);
</script></body></html>`

type probeResult struct {
	InlineScript string   `json:"inlineScript"`
	Origin       string   `json:"origin"`
	ParentDOM    string   `json:"parentDOM"`
	Cookie       string   `json:"cookie"`
	Storage      string   `json:"storage"`
	Popup        string   `json:"popup"`
	TopNav       string   `json:"topNav"`
	Fetch        string   `json:"fetch"`
	Local        string   `json:"local"`
	Remote       string   `json:"remote"`
	OutsideView  string   `json:"outsideView"`
	OutsideViol  string   `json:"outsideViolation"`
	BundleScript string   `json:"bundleScript"`
	CSS          string   `json:"css"`
	Violations   []string `json:"violations"`
}

func TestViewSandboxBrowserProbe(t *testing.T) {
	chromium := chromiumPath(t)
	f := newFixture(t, false)
	f.svc.SetViewKey(testViewKey)
	pngBytes := onePixelPNG(t)
	site := bundle{
		"index.html":    []byte(probePage),
		"img/a.png":     pngBytes,
		"css/s.css":     []byte("body { background-color: rgb(1, 2, 3); }"),
		"js/app.js":     []byte("r.bundleScript = 'ran';"),
		"docs/page.htm": []byte("<p>nested</p>"),
	}
	pub := f.publishBundle(agentA, "/api/v1/artifacts", site.manifest("index.html"), site)
	// The probe fetches the artifact's metadata route relative to the view.
	site["index.html"] = []byte(strings.Replace(probePage, "ARTIFACT_PATH", pub.Artifact.ID, 1))
	pub = f.publishBundle(agentA, "/api/v1/artifacts", site.manifest("index.html"), site)
	view, code := f.mintView(&agentA, pub.Artifact.ID, 1)
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/host", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		viewJSON, _ := json.Marshal(view.URL)
		sandboxJSON, _ := json.Marshal(r.URL.Query().Get("sandbox"))
		page := strings.Replace(probeHost, "VIEW_URL", string(viewJSON), 1)
		_, _ = w.Write([]byte(strings.Replace(page, "SANDBOX", string(sandboxJSON), 1)))
	})
	var cookieSeen atomicBool
	subResources := &requestLog{}
	mux.HandleFunc("/cookie-check.png", func(w http.ResponseWriter, r *http.Request) {
		if c := r.Header.Get("Cookie"); strings.Contains(c, "probe=") {
			cookieSeen.set()
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the sandboxed form was submitted")
	})
	mux.Handle("/api/v1/artifacts/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The hub's session would identify the parent's requests; the
		// probe's own requests must not be served as anyone.
		if strings.HasPrefix(r.URL.Path, RouteView) {
			if !strings.HasSuffix(r.URL.Path, "/index.html") {
				subResources.record(r)
			}
			f.svc.ServeHTTP(w, r)
			return
		}
		f.svc.ServeHTTP(w, withPrincipal(r, agentA))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	check := func(t *testing.T, r probeResult, framed bool) {
		t.Helper()
		if r.InlineScript != "ran" {
			t.Errorf("inline script did not run: %+v", r)
		}
		if r.Origin != "null" {
			t.Errorf("origin = %q, want an opaque origin", r.Origin)
		}
		want := map[string][2]string{
			"cookie":            {r.Cookie, "blocked"},
			"storage":           {r.Storage, "blocked"},
			"popup":             {r.Popup, "blocked"},
			"fetch":             {r.Fetch, "blocked"},
			"local":             {r.Local, "loaded"},
			"remote":            {r.Remote, "blocked"},
			"outside":           {r.OutsideView, "blocked"},
			"outside violation": {r.OutsideViol, "img-src"},
			"script":            {r.BundleScript, "ran"},
			"css":               {r.CSS, "rgb(1, 2, 3)"},
		}
		if framed {
			want["parentDOM"] = [2]string{r.ParentDOM, "blocked"}
		}
		for k, v := range want {
			if v[0] != v[1] {
				t.Errorf("%s = %q, want %q", k, v[0], v[1])
			}
		}
		got := strings.Join(r.Violations, ",")
		for _, d := range []string{"connect-src", "frame-src", "img-src", "object-src", "script-src-elem"} {
			if !strings.Contains(got, d) {
				t.Errorf("no %s violation reported (got %q)", d, got)
			}
		}
	}

	framed := func(t *testing.T, sandbox string) {
		t.Helper()
		dom := dumpDOM(t, chromium, srv.URL+"/host?sandbox="+url.QueryEscape(sandbox),
			`document.getElementById("result").textContent !== "pending"`)
		m := regexp.MustCompile(`<pre id="result"[^>]*>([^<]*)</pre>`).FindStringSubmatch(dom)
		if m == nil || m[1] == "pending" {
			t.Fatalf("no probe result in the host page:\n%s", dom)
		}
		if strings.Contains(dom, "#navigated") {
			t.Errorf("the framed document navigated its parent")
		}
		var r probeResult
		if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &r); err != nil {
			t.Fatalf("result %q: %v", m[1], err)
		}
		check(t, r, true)
		if cookieSeen.get() {
			t.Errorf("a request from the framed document carried the host page's cookie")
		}
		// The framed document's own sub-resource requests, which its policy
		// allows, reach the hub without the host page's session cookie.
		paths, withCookie := subResources.take()
		for _, want := range []string{"/img/a.png", "/css/s.css", "/js/app.js"} {
			found := false
			for _, p := range paths {
				found = found || strings.HasSuffix(p, want)
			}
			if !found {
				t.Errorf("no request for %s from the framed document (saw %v)", want, paths)
			}
		}
		if len(withCookie) > 0 {
			t.Errorf("framed sub-resource requests carried the host page's cookie: %v", withCookie)
		}
	}
	t.Run("framed as the web UI frames it", func(t *testing.T) { framed(t, "allow-scripts") })
	// The response's own sandbox keeps the document in an opaque origin
	// even if a frame were given allow-same-origin.
	t.Run("framed with allow-same-origin", func(t *testing.T) { framed(t, "allow-scripts allow-same-origin") })
	t.Run("opened directly", func(t *testing.T) {
		dom := dumpDOM(t, chromium, srv.URL+view.URL, `document.documentElement.hasAttribute("data-probe")`)
		m := regexp.MustCompile(`data-probe="([^"]*)"`).FindStringSubmatch(dom)
		if m == nil {
			t.Fatalf("no probe result:\n%s", dom)
		}
		var r probeResult
		if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &r); err != nil {
			t.Fatalf("result %q: %v", m[1], err)
		}
		check(t, r, false)
	})
}

type atomicBool struct{ v atomic.Bool }

func (b *atomicBool) set()      { b.v.Store(true) }
func (b *atomicBool) get() bool { return b.v.Load() }

// requestLog records the paths of requests and those that carried the
// probe cookie.
type requestLog struct {
	mu         sync.Mutex
	paths      []string
	withCookie []string
}

func (l *requestLog) record(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, r.URL.Path)
	if strings.Contains(r.Header.Get("Cookie"), "probe=") {
		l.withCookie = append(l.withCookie, r.URL.Path)
	}
}

func (l *requestLog) take() ([]string, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, c := l.paths, l.withCookie
	l.paths, l.withCookie = nil, nil
	return p, c
}
