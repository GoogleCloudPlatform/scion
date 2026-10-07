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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
)

var testPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...)

// fakeFetcher serves fixed bodies by URL and fails everything else with
// the given reasons, recording the calls and their deadlines.
type fakeFetcher struct {
	mu         sync.Mutex
	bodies     map[string][]byte
	reasons    map[string]remotefetch.Reason
	calls      []string
	noDeadline bool
	delay      time.Duration
}

func (f *fakeFetcher) Fetch(ctx context.Context, u string) (*remotefetch.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, u)
	if _, ok := ctx.Deadline(); !ok {
		f.noDeadline = true
	}
	body, ok := f.bodies[u]
	reason := f.reasons[u]
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if !ok {
		if reason == "" {
			reason = remotefetch.ReasonStatus
		}
		return nil, &remotefetch.Error{Reason: reason, Detail: "server-side detail: 10.0.0.5"}
	}
	sum := sha256.Sum256(body)
	return &remotefetch.Result{Body: body, ContentType: remotefetch.SniffImage(body), SHA256: hex.EncodeToString(sum[:])}, nil
}

func (f *fixture) useFetcher(ff *fakeFetcher) {
	f.svc.setImageFetcherFactory(func(RemoteImageLimits) ImageFetcher { return ff })
}

func TestPublishWarnsBeyondWindow(t *testing.T) {
	f := newFixture(t, false)
	f.useFetcher(&fakeFetcher{bodies: map[string][]byte{"https://img.example/a.png": testPNG}})
	md := "![a](https://img.example/a.png)\n" + strings.Repeat("text\n", imageScanWindow/5+1) + "![b](https://img.example/b.png)\n"
	resp := f.publish(agentA, "doc.md", []byte(md), "")
	if len(resp.Warnings) != 1 || resp.Warnings[0] != warnBeyondWindow {
		t.Fatalf("warnings %q", resp.Warnings)
	}
	if resp.Version.FileCount != 2 {
		t.Fatalf("files %d: the image past the window must have no row", resp.Version.FileCount)
	}
}

// TestPublishWarnsWhenBudgetReached: a markdown entry whose extraction
// reaches the allocation budget publishes with one warning saying some
// images were not fetched.

func TestRemoteExtractLimit(t *testing.T) {
	lim := RemoteImageLimits{MaxCount: 32}
	if got := remoteExtractLimit(lim); got != 128 {
		t.Fatalf("remoteExtractLimit = %d, want 128", got)
	}
	for _, n := range []int{127, 128, 129, 200} {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "![](https://img.example/%d.png)\n", i)
		}
		got := extractImageURLs(b.String(), remoteExtractLimit(lim)).urls
		if want := min(n, 128); len(got) != want {
			t.Errorf("%d distinct URLs: extracted %d, want %d", n, len(got), want)
		}
	}
}

func TestPublishExtendsWriteDeadline(t *testing.T) {
	f := newFixture(t, false)
	f.useFetcher(&fakeFetcher{bodies: map[string][]byte{"https://img.example/a.png": testPNG}, delay: 600 * time.Millisecond})
	f.svc.SetLimits(func(context.Context) Limits {
		return Limits{MaxFileBytes: 1 << 20, RemoteImages: RemoteImageLimits{
			Enabled: true, MaxCount: 4, MaxBytes: 1 << 20, FetchTimeout: 2 * time.Second, TotalBudget: 2 * time.Second,
		}}
	})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.svc.ServeHTTP(w, withPrincipal(r, agentA))
	}))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/v1/artifacts?name=doc.md", "text/markdown", strings.NewReader("![a](https://img.example/a.png)"))
	if err != nil {
		t.Fatalf("publish: %v (the response was cut off)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestFetchFloor: refused and unresolvable image fetches complete no
// sooner than the fetch floor, once per publish however many there are;
// a publish whose fetches all succeed is not held.
func TestFetchFloor(t *testing.T) {
	const floor = 300 * time.Millisecond
	good := "https://img.example/good.png"
	publishWith := func(t *testing.T, md string, reasons map[string]remotefetch.Reason) time.Duration {
		t.Helper()
		f := newFixture(t, false)
		f.svc.fetchFloor = floor
		f.useFetcher(&fakeFetcher{bodies: map[string][]byte{good: testPNG}, reasons: reasons})
		start := time.Now()
		f.publish(agentA, "doc.md", []byte(md), "")
		return time.Since(start)
	}
	t.Run("refused", func(t *testing.T) {
		u := "https://refused.example/a.png"
		if d := publishWith(t, "![a]("+u+")", map[string]remotefetch.Reason{u: remotefetch.ReasonDeniedAddress}); d < floor {
			t.Fatalf("took %v, want at least %v", d, floor)
		}
	})
	t.Run("unresolvable", func(t *testing.T) {
		u := "https://unresolvable.example/a.png"
		if d := publishWith(t, "![a]("+u+")", map[string]remotefetch.Reason{u: remotefetch.ReasonResolve}); d < floor {
			t.Fatalf("took %v, want at least %v", d, floor)
		}
	})
	t.Run("many refused cost one floor", func(t *testing.T) {
		var md strings.Builder
		reasons := map[string]remotefetch.Reason{}
		for i := 0; i < 50; i++ {
			u := fmt.Sprintf("https://refused.example/%d.png", i)
			reasons[u] = remotefetch.ReasonDeniedAddress
			fmt.Fprintf(&md, "![a](%s)\n", u)
		}
		if d := publishWith(t, md.String(), reasons); d < floor || d > 3*floor {
			t.Fatalf("took %v, want about one floor (%v)", d, floor)
		}
	})
	t.Run("all fetched", func(t *testing.T) {
		if d := publishWith(t, "![a]("+good+")", nil); d >= floor {
			t.Fatalf("took %v, want well under the floor %v", d, floor)
		}
	})
	t.Run("floor stays within the fetch budget", func(t *testing.T) {
		u := "https://refused.example/a.png"
		f := newFixture(t, false)
		f.svc.fetchFloor = 5 * time.Second
		f.useFetcher(&fakeFetcher{reasons: map[string]remotefetch.Reason{u: remotefetch.ReasonDeniedAddress}})
		f.svc.SetLimits(func(context.Context) Limits {
			return Limits{MaxFileBytes: 1 << 20, RemoteImages: RemoteImageLimits{
				Enabled: true, MaxCount: 4, MaxBytes: 1 << 20, FetchTimeout: 200 * time.Millisecond, TotalBudget: 200 * time.Millisecond,
			}}
		})
		start := time.Now()
		f.publish(agentA, "doc.md", []byte("![a]("+u+")"), "")
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v, want the floor cut short by the 200ms budget", d)
		}
	})
	t.Run("refused by the production fetcher", func(t *testing.T) {
		// The real fetcher with its production address rules: an IP
		// literal in a denied range is refused before any connection (no
		// DNS involved), and the publish is held for the floor.
		f := newFixture(t, false)
		f.svc.fetchFloor = floor
		f.svc.setImageFetcherFactory(defaultFetcherFactory)
		start := time.Now()
		resp := f.publish(agentA, "doc.md", []byte("![a](https://169.254.169.254/latest/meta-data/)"), "")
		if d := time.Since(start); d < floor {
			t.Fatalf("took %v, want at least %v", d, floor)
		}
		if len(resp.Warnings) != 1 {
			t.Fatalf("warnings %q", resp.Warnings)
		}
	})
	t.Run("missing image is not held", func(t *testing.T) {
		u := "https://img.example/missing.png"
		if d := publishWith(t, "![a]("+u+")", map[string]remotefetch.Reason{u: remotefetch.ReasonStatus}); d >= floor {
			t.Fatalf("took %v; only refused and unresolvable fetches are held", d)
		}
	})
}

// TestRemoteImageLimitsFailClosed: limits a host supplies that are
// incomplete or invalid turn remote images off; the defaults apply only
// when the host gives no limits at all.
func TestRemoteImageLimitsFailClosed(t *testing.T) {
	ctx := context.Background()
	if got := (backend{}).remoteImageLimits(ctx); got != DefaultRemoteImageLimits() {
		t.Fatalf("no limits getter: %+v", got)
	}
	for name, l := range map[string]RemoteImageLimits{
		"zero":            {},
		"enabled, no cap": {Enabled: true, MaxBytes: 1, FetchTimeout: time.Second, TotalBudget: time.Second},
		"negative bytes":  {Enabled: true, MaxCount: 1, MaxBytes: -1, FetchTimeout: time.Second, TotalBudget: time.Second},
		"budget too long": {Enabled: true, MaxCount: 1, MaxBytes: 1, FetchTimeout: time.Second, TotalBudget: MaxRemoteFetchBudget + time.Second},
	} {
		b := backend{limits: func(context.Context) Limits { return Limits{RemoteImages: l} }}
		if got := b.remoteImageLimits(ctx); got.Enabled {
			t.Fatalf("%s: remote images enabled: %+v", name, got)
		}
	}

	f := newFixture(t, false)
	ff := &fakeFetcher{bodies: map[string][]byte{"https://img.example/a.png": testPNG}}
	f.useFetcher(ff)
	f.svc.SetLimits(func(context.Context) Limits { return Limits{MaxFileBytes: 1 << 20} })
	resp := f.publish(agentA, "doc.md", []byte("![a](https://img.example/a.png)"), "")
	if len(ff.calls) != 0 || resp.Version.FileCount != 1 {
		t.Fatalf("host limits without remote image limits must not fetch: calls %v files %d", ff.calls, resp.Version.FileCount)
	}
}

func TestRemotePath(t *testing.T) {
	sum := sha256.Sum256([]byte("https://img.example/a.png"))
	if got := RemotePath("https://img.example/a.png"); got != "_remote/"+hex.EncodeToString(sum[:]) {
		t.Fatalf("RemotePath = %q", got)
	}
}

// TestPublishMarkdownFetchesRemoteImages: one image fetches, one is refused
// and one is missing; the publish succeeds, the good image is a manifest
// file, the others are failed rows, and the warnings are identical and
// generic (a refused address and a missing image look the same).
func TestPublishMarkdownFetchesRemoteImages(t *testing.T) {
	f := newFixture(t, false)
	good, refused, missing := "https://img.example/good.png", "https://169.254.169.254/latest", "https://img.example/missing.png"
	ff := &fakeFetcher{bodies: map[string][]byte{good: testPNG}, reasons: map[string]remotefetch.Reason{refused: remotefetch.ReasonDeniedAddress, missing: remotefetch.ReasonStatus}}
	f.useFetcher(ff)
	md := "![g](" + good + ")\n![r](" + refused + ")\n![m](" + missing + ")\n"
	resp := f.publish(agentA, "doc.md", []byte(md), "")

	if len(ff.calls) != 3 || ff.noDeadline {
		t.Fatalf("fetch calls %v, every call must carry the budget deadline (missing: %v)", ff.calls, ff.noDeadline)
	}
	files := map[string]FileInfo{}
	for _, fi := range resp.Version.Files {
		files[fi.Path] = fi
	}
	if len(files) != 4 || resp.Version.FileCount != 4 {
		t.Fatalf("manifest %+v", resp.Version.Files)
	}
	g := files[RemotePath(good)]
	if g.Origin != FileOriginRemote || g.FetchStatus != FetchStatusOK || g.SourceURL != good || g.SHA256 != sha(testPNG) || g.MediaType != "image/png" {
		t.Fatalf("good row %+v", g)
	}
	if resp.Version.TotalBytes != int64(len(md)+len(testPNG)) {
		t.Fatalf("total bytes %d", resp.Version.TotalBytes)
	}
	for _, u := range []string{refused, missing} {
		r := files[RemotePath(u)]
		if r.FetchStatus != FetchStatusFailed || r.SHA256 != "" || r.SourceURL != u {
			t.Fatalf("failed row %+v", r)
		}
	}
	wantWarnings := []string{remoteFetchFailed + ": " + refused, remoteFetchFailed + ": " + missing}
	if strings.Join(resp.Warnings, "|") != strings.Join(wantWarnings, "|") {
		t.Fatalf("warnings %q, want %q", resp.Warnings, wantWarnings)
	}
	rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+resp.Artifact.ID, nil, nil)
	for _, internal := range []string{"denied", "10.0.0.5", "bad_status", "detail"} {
		if strings.Contains(rec.Body.String(), internal) {
			t.Fatalf("metadata shows server-side text %q: %s", internal, rec.Body.String())
		}
	}
	// The stored error text is the generic one for both failures.
	v, err := f.store.GetVersion(context.Background(), resp.Artifact.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{refused, missing} {
		row, err := f.store.GetFile(context.Background(), v.ID, RemotePath(u))
		if err != nil || row.FetchError != remoteFetchFailed {
			t.Fatalf("stored row for %s: %+v %v", u, row, err)
		}
	}
}

func TestServeRemoteImages(t *testing.T) {
	f := newFixture(t, true)
	good, bad := "https://img.example/good.png", "https://img.example/bad.png"
	f.useFetcher(&fakeFetcher{bodies: map[string][]byte{good: testPNG}})
	resp := f.publish(agentA, "doc.md", []byte("![g]("+good+") ![b]("+bad+")"), "")
	base := "/api/v1/artifacts/" + resp.Artifact.ID + "/versions/1/files/"

	rec := f.do(&agentA, http.MethodGet, base+RemotePath(good)+"?stream=1", nil, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), testPNG) {
		t.Fatalf("good image: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=31536000, immutable" {
		t.Fatalf("cache control %q", cc)
	}
	// On the current-version route the same image is not cached as
	// immutable: that route can point at another version later.
	current := "/api/v1/artifacts/" + resp.Artifact.ID + "/files/" + RemotePath(good) + "?stream=1"
	if rec := f.do(&agentA, http.MethodGet, current, nil, nil); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("current route: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get(HeaderRemoteStatus) != "" {
		t.Fatal("status header on a good image")
	}

	rec = f.do(&agentA, http.MethodGet, base+RemotePath(bad)+"?stream=1", nil, nil)
	if rec.Code != http.StatusNotFound || rec.Header().Get(HeaderRemoteStatus) != "failed" {
		t.Fatalf("failed image: %d %q", rec.Code, rec.Header().Get(HeaderRemoteStatus))
	}

	// A path nobody fetched is a plain 404, without the header.
	rec = f.do(&agentA, http.MethodGet, base+RemotePath("https://img.example/other.png")+"?stream=1", nil, nil)
	if rec.Code != http.StatusNotFound || rec.Header().Get(HeaderRemoteStatus) != "" {
		t.Fatalf("unknown remote path: %d %q", rec.Code, rec.Header().Get(HeaderRemoteStatus))
	}
}

// TestRemoteImageAuthorization: remote files go through the same chain as
// any other file, credential check first; an unreadable artifact answers a
// plain 404 even for a failed image, so the status header says nothing
// about an artifact the caller cannot read.
func TestRemoteImageAuthorization(t *testing.T) {
	f := newFixture(t, false)
	good, bad := "https://img.example/good.png", "https://img.example/bad.png"
	f.useFetcher(&fakeFetcher{bodies: map[string][]byte{good: testPNG}})
	resp := f.publish(agentA, "doc.md", []byte("![g]("+good+") ![b]("+bad+")"), "")
	base := "/api/v1/artifacts/" + resp.Artifact.ID + "/files/"

	for _, p := range []*principal{nil, &agentX, &outside} {
		for _, u := range []string{good, bad} {
			rec := f.do(p, http.MethodGet, base+RemotePath(u), nil, nil)
			if rec.Code != http.StatusNotFound || rec.Header().Get(HeaderRemoteStatus) != "" {
				t.Fatalf("%v %s: %d %q", p, u, rec.Code, rec.Header().Get(HeaderRemoteStatus))
			}
		}
	}
	f.host.deny(agentB, "project-1", PermissionRead)
	f.host.calls = nil
	rec := f.do(&agentB, http.MethodGet, base+RemotePath(good), nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("credential denial: %d", rec.Code)
	}
	if len(f.host.calls) == 0 || !strings.HasPrefix(f.host.calls[0], "permits ") {
		t.Fatalf("Permits not asked first: %v", f.host.calls)
	}
	if rec := f.do(&agentA, http.MethodGet, base+RemotePath(good), nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner: %d", rec.Code)
	}
}

func TestPublishRejectsReservedRemoteNames(t *testing.T) {
	f := newFixture(t, false)
	for _, name := range []string{"_remote"} {
		rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name="+name, []byte("x"), nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: %d", name, rec.Code)
		}
	}
	if !isReservedPath("_remote/abc") || !isReservedPath("_remote") || isReservedPath("_remotes") || isReservedPath("a/_remote/b") {
		t.Fatal("isReservedPath")
	}
}

func TestPublishRemoteImageLimits(t *testing.T) {
	f := newFixture(t, false)
	ff := &fakeFetcher{bodies: map[string][]byte{}}
	var md strings.Builder
	for i := 0; i < 5; i++ {
		u := "https://img.example/" + string(rune('a'+i)) + ".png"
		ff.bodies[u] = testPNG
		md.WriteString("![x](" + u + ")\n")
	}
	f.useFetcher(ff)
	f.svc.SetLimits(func(context.Context) Limits {
		return Limits{RemoteImages: RemoteImageLimits{Enabled: true, MaxCount: 2, MaxBytes: 1 << 20, FetchTimeout: time.Second, TotalBudget: time.Second}}
	})
	resp := f.publish(agentA, "doc.md", []byte(md.String()), "")
	if len(ff.calls) != 2 || resp.Version.FileCount != 3 {
		t.Fatalf("calls %v, files %d", ff.calls, resp.Version.FileCount)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "3 more remote images") {
		t.Fatalf("warnings %q", resp.Warnings)
	}

	// Disabled: nothing is fetched and no rows are written.
	ff.calls = nil
	f.svc.SetLimits(func(context.Context) Limits {
		return Limits{RemoteImages: RemoteImageLimits{Enabled: false, MaxCount: 2, MaxBytes: 1 << 20, FetchTimeout: time.Second, TotalBudget: time.Second}}
	})
	resp = f.publish(agentA, "doc2.md", []byte(md.String()), "")
	if len(ff.calls) != 0 || resp.Version.FileCount != 1 || len(resp.Warnings) != 0 {
		t.Fatalf("disabled: calls %v files %d warnings %q", ff.calls, resp.Version.FileCount, resp.Warnings)
	}
}

func TestPublishNonMarkdownFetchesNothing(t *testing.T) {
	f := newFixture(t, false)
	ff := &fakeFetcher{bodies: map[string][]byte{"https://img.example/a.png": testPNG}}
	f.useFetcher(ff)
	resp := f.publish(agentA, "notes.txt", []byte("![a](https://img.example/a.png)"), "")
	if len(ff.calls) != 0 || resp.Version.FileCount != 1 {
		t.Fatalf("calls %v files %d", ff.calls, resp.Version.FileCount)
	}
}

// TestFinalizeFetchesRemoteImages: a two-step version whose entry is
// markdown gets its remote images at finalize, as manifest rows next to the
// uploaded files; a non-markdown entry and a markdown file that is not the
// entry fetch nothing.
func TestFinalizeFetchesRemoteImages(t *testing.T) {
	f := newFixture(t, false)
	good, bad := "https://img.example/good.png", "https://img.example/bad.png"
	ff := &fakeFetcher{bodies: map[string][]byte{good: testPNG}}
	f.useFetcher(ff)
	files := bundle{
		"docs/index.md":  []byte("![g](" + good + ") ![b](" + bad + ") ![local](img/a.png)"),
		"docs/img/a.png": testPNG,
		"notes.md":       []byte("![n](https://img.example/notes.png)"),
	}
	resp := f.publishBundle(agentA, "/api/v1/artifacts", files.manifest("docs/index.md"), files)
	if len(ff.calls) != 2 {
		t.Fatalf("fetch calls %v, want the entry's two remote images", ff.calls)
	}
	byPath := map[string]FileInfo{}
	for _, fi := range resp.Version.Files {
		byPath[fi.Path] = fi
	}
	if g := byPath[RemotePath(good)]; g.FetchStatus != FetchStatusOK || g.Origin != FileOriginRemote || g.SourceURL != good {
		t.Errorf("good row %+v", g)
	}
	if b := byPath[RemotePath(bad)]; b.FetchStatus != FetchStatusFailed {
		t.Errorf("failed row %+v", b)
	}
	if resp.Version.FileCount != 5 || len(resp.Version.Files) != 5 {
		t.Errorf("manifest %+v", resp.Version.Files)
	}
	if len(resp.Warnings) != 1 || resp.Warnings[0] != remoteFetchFailed+": "+bad {
		t.Errorf("warnings %q", resp.Warnings)
	}
	rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+resp.Artifact.ID+"/versions/1/files/"+RemotePath(good), nil, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), testPNG) {
		t.Errorf("remote copy: %d", rec.Code)
	}

	// An HTML entry fetches nothing.
	ff.calls = nil
	html := bundle{"index.html": []byte(`<img src="` + good + `">`)}
	f.publishBundle(agentA, "/api/v1/artifacts", html.manifest("index.html"), html)
	if len(ff.calls) != 0 {
		t.Errorf("HTML entry fetched %v", ff.calls)
	}
}

// TestConcurrentFinalizeFetchesOnce: of several concurrent finalize
// requests for one version, one completes it and the others answer 409
// without fetching; each remote image is fetched exactly once.
func TestConcurrentFinalizeFetchesOnce(t *testing.T) {
	f := newFixture(t, false)
	a, b := "https://img.example/a.png", "https://img.example/b.png"
	ff := &fakeFetcher{bodies: map[string][]byte{a: testPNG, b: testPNG}, delay: 200 * time.Millisecond}
	f.useFetcher(ff)
	files := bundle{"doc.md": []byte("![a](" + a + ") ![b](" + b + ")")}
	pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("doc.md"))
	if rec := f.put(agentA, pend.Artifact.ID, 1, "doc.md", files["doc.md"]); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", rec.Code)
	}
	const n = 4
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.finalize(agentA, pend.Artifact.ID, 1).Code
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Errorf("finalize codes %v, want one 200 and %d 409", codes, n-1)
	}
	seen := map[string]int{}
	for _, u := range ff.calls {
		seen[u]++
	}
	if len(ff.calls) != 2 || seen[a] != 1 || seen[b] != 1 {
		t.Errorf("fetch calls %v, want each image once", ff.calls)
	}
}

// TestRemoteImagesStayWithinVersionLimits: remote images count toward the
// version's file and size limits; images past either limit are not
// fetched, or not kept, and the publisher gets a warning.
func TestRemoteImagesStayWithinVersionLimits(t *testing.T) {
	big := append(append([]byte{}, testPNG...), bytes.Repeat([]byte{1}, 400)...)
	ff := &fakeFetcher{bodies: map[string][]byte{}}
	var md strings.Builder
	for i := 0; i < 4; i++ {
		u := fmt.Sprintf("https://img.example/%d.png", i)
		ff.bodies[u] = big
		md.WriteString("![x](" + u + ")\n")
	}
	limits := func(files int, bundle int64) func(context.Context) Limits {
		return func(context.Context) Limits {
			return Limits{MaxFileBytes: 1 << 20, MaxBundleBytes: bundle, MaxFiles: files, RemoteImages: RemoteImageLimits{
				Enabled: true, MaxCount: 10, MaxBytes: 1 << 20, FetchTimeout: time.Second, TotalBudget: time.Second}}
		}
	}
	t.Run("file limit", func(t *testing.T) {
		f := newFixture(t, false)
		f.useFetcher(ff)
		ff.calls = nil
		f.svc.SetLimits(limits(3, 1<<20)) // the entry plus two images
		resp := f.publish(agentA, "doc.md", []byte(md.String()), "")
		if resp.Version.FileCount != 3 || len(ff.calls) != 2 {
			t.Errorf("files %d, fetches %v; want 3 files and 2 fetches", resp.Version.FileCount, ff.calls)
		}
		if strings.Join(resp.Warnings, "|") != "2 more remote images were not fetched (the version's file limit)" {
			t.Errorf("warnings %q", resp.Warnings)
		}
	})
	t.Run("size limit", func(t *testing.T) {
		f := newFixture(t, false)
		f.useFetcher(ff)
		ff.calls = nil
		size := int64(md.Len() + 2*len(big)) // the entry plus two images
		f.svc.SetLimits(limits(200, size))
		resp := f.publish(agentA, "doc.md", []byte(md.String()), "")
		if resp.Version.FileCount != 3 || resp.Version.TotalBytes > size {
			t.Errorf("files %d, bytes %d (limit %d)", resp.Version.FileCount, resp.Version.TotalBytes, size)
		}
		if strings.Join(resp.Warnings, "|") != "2 more remote images were not fetched (the version's size limit)" {
			t.Errorf("warnings %q", resp.Warnings)
		}
	})
	t.Run("two-step version already at its file limit", func(t *testing.T) {
		f := newFixture(t, false)
		f.useFetcher(ff)
		ff.calls = nil
		f.svc.SetLimits(limits(2, 1<<20))
		files := bundle{"doc.md": []byte(md.String()), "a.txt": []byte("a")}
		resp := f.publishBundle(agentA, "/api/v1/artifacts", files.manifest("doc.md"), files)
		if resp.Version.FileCount != 2 || len(ff.calls) != 0 {
			t.Errorf("files %d, fetches %v", resp.Version.FileCount, ff.calls)
		}
		if strings.Join(resp.Warnings, "|") != "4 more remote images were not fetched (the version's file limit)" {
			t.Errorf("warnings %q", resp.Warnings)
		}
	})
}

// TestRemoteImagesWarnWhenTheScanFills: when the scan's candidates are
// used up by references that cannot be fetched, a later valid image is not
// read, and the publisher is told.
func TestRemoteImagesWarnWhenTheScanFills(t *testing.T) {
	f := newFixture(t, false)
	good := "https://img.example/late.png"
	ff := &fakeFetcher{bodies: map[string][]byte{good: testPNG}}
	f.useFetcher(ff)
	f.svc.SetLimits(func(context.Context) Limits {
		return Limits{MaxFileBytes: 1 << 20, RemoteImages: RemoteImageLimits{
			Enabled: true, MaxCount: 2, MaxBytes: 1 << 20, FetchTimeout: time.Second, TotalBudget: time.Second}}
	})
	var md strings.Builder
	for i := 0; i < remoteExtractLimit(RemoteImageLimits{MaxCount: 2}); i++ {
		fmt.Fprintf(&md, "![x](https://user@img.example/%d.png)\n", i) // userinfo: never fetched
	}
	md.WriteString("![late](" + good + ")\n")
	resp := f.publish(agentA, "doc.md", []byte(md.String()), "")
	if len(ff.calls) != 0 || resp.Version.FileCount != 1 {
		t.Errorf("fetches %v, files %d", ff.calls, resp.Version.FileCount)
	}
	if strings.Join(resp.Warnings, "|") != warnTooManyReferences {
		t.Errorf("warnings %q", resp.Warnings)
	}
}

// TestRemoteImagesCountSaysOrMoreWhenTheScanFills: when the scan stopped
// full, the count of images not fetched is a lower bound.
func TestRemoteImagesCountSaysOrMoreWhenTheScanFills(t *testing.T) {
	f := newFixture(t, false)
	ff := &fakeFetcher{bodies: map[string][]byte{}}
	f.useFetcher(ff)
	f.svc.SetLimits(func(context.Context) Limits {
		return Limits{MaxFileBytes: 1 << 20, RemoteImages: RemoteImageLimits{
			Enabled: true, MaxCount: 2, MaxBytes: 1 << 20, FetchTimeout: time.Second, TotalBudget: time.Second}}
	})
	var md strings.Builder
	// Six valid images, then refused ones until the scan fills (limit 8),
	// then more valid ones it never reads.
	for i := 0; i < 6; i++ {
		u := fmt.Sprintf("https://img.example/%d.png", i)
		ff.bodies[u] = testPNG
		fmt.Fprintf(&md, "![x](%s)\n", u)
	}
	for i := 0; i < 2; i++ {
		fmt.Fprintf(&md, "![x](https://user@img.example/r%d.png)\n", i)
	}
	md.WriteString("![x](https://img.example/late.png)\n")
	resp := f.publish(agentA, "doc.md", []byte(md.String()), "")
	if strings.Join(resp.Warnings, "|") != "at least 4 more remote images were not fetched (at most 2 per version)" {
		t.Errorf("warnings %q", resp.Warnings)
	}
}
