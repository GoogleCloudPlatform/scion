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

package runtimebroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/transfertest"
)

// fakeSignedHub serves workspace download pages (fetch) for a set of
// objects, and the objects themselves from an httptest server at
// V4-shaped signed URLs it records as issued.
type fakeSignedHub struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	objects  map[string][]byte
	sizes    map[string]int64 // listed size override
	pageSize int
	// expires is the expiry of each page; nil means 15 minutes ahead.
	expires func(call int) time.Time
	// pageErr, when set, may fail a page request (call is 1-based).
	pageErr func(call int) error
	// objectStatus, when set, may answer an object GET with a status
	// (attempt is 1-based per path); 0 serves the object.
	objectStatus func(path string, attempt int) int
	// extraFiles, when set, is appended to the page for its call.
	extraFiles func(call int) []api.WorkspaceDownloadFile
	pageCalls  int
	gets       map[string]int
	issued     []string
}

func newFakeSignedHub(t *testing.T, objects map[string]string) *fakeSignedHub {
	t.Helper()
	h := &fakeSignedHub{t: t, objects: map[string][]byte{}, sizes: map[string]int64{}, pageSize: 500, gets: map[string]int{}}
	for p, c := range objects {
		h.objects[p] = []byte(c)
	}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serveObject))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeSignedHub) serveObject(w http.ResponseWriter, r *http.Request) {
	p, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/o/"))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	h.gets[p]++
	attempt := h.gets[p]
	content, ok := h.objects[p]
	statusFn := h.objectStatus
	h.mu.Unlock()
	if statusFn != nil {
		if st := statusFn(p, attempt); st != 0 {
			// The error body carries the URL, as GCS error bodies can: it
			// must never reach any error text.
			http.Error(w, "denied "+r.URL.String(), st)
			return
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(content)
}

func (h *fakeSignedHub) fetch(_ context.Context, _ CreateAgentRequest, cursor string) (*api.WorkspaceDownloadResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pageCalls++
	call := h.pageCalls
	if h.pageErr != nil {
		if err := h.pageErr(call); err != nil {
			return nil, err
		}
	}
	var names []string
	for p := range h.objects {
		if p >= cursor {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	resp := &api.WorkspaceDownloadResponse{Expires: time.Now().Add(15 * time.Minute)}
	if h.expires != nil {
		resp.Expires = h.expires(call)
	}
	resp.Files = []api.WorkspaceDownloadFile{}
	for i, p := range names {
		if i >= h.pageSize {
			resp.NextCursor = p
			break
		}
		size := int64(len(h.objects[p]))
		if s, ok := h.sizes[p]; ok {
			size = s
		}
		u := h.srv.URL + "/o/" + url.PathEscape(p) + "?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential=hub%40example&X-Goog-Signature=sig" + fmt.Sprint(len(h.issued))
		h.issued = append(h.issued, u)
		resp.Files = append(resp.Files, api.WorkspaceDownloadFile{Path: p, URL: u, Size: size})
	}
	if h.extraFiles != nil {
		resp.Files = append(resp.Files, h.extraFiles(call)...)
	}
	return resp, nil
}

func (h *fakeSignedHub) getCount(p string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gets[p]
}

func (h *fakeSignedHub) issuedURLs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.issued...)
}

// noSleep makes retries immediate in tests.
func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// runSignedDownload runs the downloader into a fresh directory with h as
// the page source.
func runSignedDownload(t *testing.T, h *fakeSignedHub, tweak func(*signedDownloadSettings)) (string, []string, error) {
	t.Helper()
	dir := t.TempDir()
	srv := &Server{}
	srv.signedDownload = signedDownloadSettings{fetchPage: h.fetch, sleep: noSleep}
	if tweak != nil {
		tweak(&srv.signedDownload)
	}
	written, err := srv.signedWorkspaceDownload(context.Background(), CreateAgentRequest{ID: "agent-1"}, dir)
	return dir, written, err
}

func requireSignedClass(t *testing.T, err error, class string) *signedDownloadError {
	t.Helper()
	var de *signedDownloadError
	if !errors.As(err, &de) {
		t.Fatalf("error %v (%T) is not a signedDownloadError", err, err)
	}
	if de.Class != class {
		t.Fatalf("class = %q, want %q (error %v)", de.Class, class, err)
	}
	return de
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSignedDownload_Success(t *testing.T) {
	h := newFakeSignedHub(t, map[string]string{"README.md": "hello", "src/main.go": "package main", "a/.scion/x": "nested"})
	dir, written, err := runSignedDownload(t, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, dir); len(got) != 3 || got["src/main.go"] != "package main" || got["a/.scion/x"] != "nested" {
		t.Fatalf("tree = %v", got)
	}
	if len(written) != 3 {
		t.Fatalf("written = %v", written)
	}
}

// TestSignedDownload_ContinuesPastEmptyPage: an empty page with a cursor is
// followed, never taken as the end.
func TestSignedDownload_ContinuesPastEmptyPage(t *testing.T) {
	h := newFakeSignedHub(t, map[string]string{"a": "1", "b": "2"})
	h.pageSize = 1
	var calls int
	inner := h.fetch
	fetch := func(ctx context.Context, req CreateAgentRequest, cursor string) (*api.WorkspaceDownloadResponse, error) {
		calls++
		if calls == 1 {
			return &api.WorkspaceDownloadResponse{Files: []api.WorkspaceDownloadFile{}, NextCursor: "a", Expires: time.Now().Add(time.Hour)}, nil
		}
		return inner(ctx, req, cursor)
	}
	dir, _, err := runSignedDownload(t, h, func(st *signedDownloadSettings) { st.fetchPage = fetch })
	if err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, dir); len(got) != 2 {
		t.Fatalf("tree = %v", got)
	}
}

// TestSignedDownload_RefusesInvalidEntries: a root identity entry, a
// traversal or a directory object in a page fails closed and writes nothing
// outside the stage.
func TestSignedDownload_RefusesInvalidEntries(t *testing.T) {
	for _, name := range []string{".scion", ".scion/x", "../escape", "/abs", "./.scion", "d/"} {
		t.Run(name, func(t *testing.T) {
			h := newFakeSignedHub(t, map[string]string{"ok.txt": "1"})
			h.extraFiles = func(int) []api.WorkspaceDownloadFile {
				return []api.WorkspaceDownloadFile{{Path: name, URL: h.srv.URL + "/o/x", Size: 1}}
			}
			_, _, err := runSignedDownload(t, h, nil)
			requireSignedClass(t, err, signedClassInvalidEntry)
		})
	}
}

// TestSignedDownload_FilterMatchesSharedTable is the broker side of the
// shared exclusion table: a page entry is accepted exactly when the table
// keeps its name.
func TestSignedDownload_FilterMatchesSharedTable(t *testing.T) {
	cases := transfertest.WorkspaceObjectCases()
	if len(cases) == 0 {
		t.Fatal("empty shared table")
	}
	for _, c := range cases {
		h := newFakeSignedHub(t, nil)
		h.extraFiles = func(int) []api.WorkspaceDownloadFile {
			return []api.WorkspaceDownloadFile{{Path: c.Name, URL: h.srv.URL + "/o/entry", Size: 1}}
		}
		_, _, err := runSignedDownload(t, h, func(st *signedDownloadSettings) {
			st.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("x")), Header: http.Header{}}, nil
			})}
		})
		accepted := err == nil
		if want := c.Excluded == ""; accepted != want {
			t.Errorf("name %q: accepted %v (err %v), table keeps it: %v", c.Name, accepted, err, want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSignedDownload_SizeMismatchRefused(t *testing.T) {
	h := newFakeSignedHub(t, map[string]string{"a.txt": "12345"})
	h.sizes["a.txt"] = 3
	dir, _, err := runSignedDownload(t, h, nil)
	requireSignedClass(t, err, signedClassSizeMismatch)
	if got := readTree(t, dir); len(got) != 0 {
		t.Fatalf("a refused file must leave nothing behind: %v", got)
	}
}

// TestSignedDownload_ObjectNotFoundFailsWithoutRefresh: a 404 for a listed
// object fails closed at once (the listing is stale), with no page refresh
// and no retry.
func TestSignedDownload_ObjectNotFoundFailsWithoutRefresh(t *testing.T) {
	h := newFakeSignedHub(t, map[string]string{"a.txt": "1"})
	h.objectStatus = func(string, int) int { return http.StatusNotFound }
	_, _, err := runSignedDownload(t, h, nil)
	de := requireSignedClass(t, err, "http_404")
	if de.Status != http.StatusNotFound || h.pageCalls != 1 || h.getCount("a.txt") != 1 {
		t.Fatalf("status %d, page calls %d, gets %d; want one page and one GET", de.Status, h.pageCalls, h.getCount("a.txt"))
	}
}

// TestSignedDownload_ExpiredURLRefreshesPage: a 403 from the object store
// fetches the page again with the same cursor, and files already written
// are not fetched again.
func TestSignedDownload_ExpiredURLRefreshesPage(t *testing.T) {
	h := newFakeSignedHub(t, map[string]string{"a.txt": "1", "b.txt": "2"})
	h.objectStatus = func(p string, attempt int) int {
		if p == "b.txt" && attempt == 1 {
			return http.StatusForbidden
		}
		return 0
	}
	dir, _, err := runSignedDownload(t, h, func(st *signedDownloadSettings) { st.concurrency = 1 })
	if err != nil {
		t.Fatal(err)
	}
	if h.pageCalls != 2 {
		t.Fatalf("page calls = %d, want 2 (one refresh)", h.pageCalls)
	}
	if h.getCount("a.txt") != 1 {
		t.Fatalf("a completed file was fetched %d times, want once", h.getCount("a.txt"))
	}
	if got := readTree(t, dir); len(got) != 2 {
		t.Fatalf("tree = %v", got)
	}
}

// TestSignedDownload_PageNearExpiryRefreshes: a page expiring within the
// margin is fetched again before its files are started.
func TestSignedDownload_PageNearExpiryRefreshes(t *testing.T) {
	h := newFakeSignedHub(t, map[string]string{"a.txt": "1"})
	h.expires = func(call int) time.Time {
		if call == 1 {
			return time.Now().Add(10 * time.Second)
		}
		return time.Now().Add(15 * time.Minute)
	}
	if _, _, err := runSignedDownload(t, h, nil); err != nil {
		t.Fatal(err)
	}
	if h.pageCalls != 2 || h.getCount("a.txt") != 1 {
		t.Fatalf("page calls %d, gets %d", h.pageCalls, h.getCount("a.txt"))
	}
}

// TestSignedDownload_IdleRefreshBudget: refreshes with no file completed
// fail after two in a row, while a page that keeps completing files
// survives more than two refreshes.
func TestSignedDownload_IdleRefreshBudget(t *testing.T) {
	t.Run("no progress fails", func(t *testing.T) {
		h := newFakeSignedHub(t, map[string]string{"a.txt": "1"})
		h.objectStatus = func(string, int) int { return http.StatusForbidden }
		_, _, err := runSignedDownload(t, h, nil)
		requireSignedClass(t, err, signedClassExpired)
		if h.pageCalls != 1+signedDownloadMaxIdleRefreshes {
			t.Fatalf("page calls = %d, want %d", h.pageCalls, 1+signedDownloadMaxIdleRefreshes)
		}
		if op := signedBootstrapOp(err); failedOpText(op) != "Failed to download workspace (signed URLs expired or refused)" {
			t.Fatalf("client text = %q", failedOpText(op))
		}
	})
	t.Run("progress survives", func(t *testing.T) {
		objs := map[string]string{}
		for i := 0; i < 6; i++ {
			objs[fmt.Sprintf("f%d", i)] = "x"
		}
		h := newFakeSignedHub(t, objs)
		// Every round completes exactly one file and refuses the rest.
		var mu sync.Mutex
		served := map[int]bool{}
		h.objectStatus = func(p string, attempt int) int {
			h.mu.Lock()
			round := h.pageCalls
			h.mu.Unlock()
			mu.Lock()
			defer mu.Unlock()
			if served[round] {
				return http.StatusForbidden
			}
			served[round] = true
			return 0
		}
		dir, _, err := runSignedDownload(t, h, func(st *signedDownloadSettings) { st.concurrency = 1 })
		if err != nil {
			t.Fatalf("a progressing page must survive: %v", err)
		}
		if h.pageCalls <= 1+signedDownloadMaxIdleRefreshes {
			t.Fatalf("page calls = %d; the test must exercise more than %d refreshes", h.pageCalls, signedDownloadMaxIdleRefreshes)
		}
		if got := readTree(t, dir); len(got) != 6 {
			t.Fatalf("tree = %v", got)
		}
	})
}

// TestSignedDownload_RefreshListingChangedFailsClosed: a refreshed page that
// lists other paths or sizes fails closed.
func TestSignedDownload_RefreshListingChangedFailsClosed(t *testing.T) {
	h := newFakeSignedHub(t, map[string]string{"a.txt": "1"})
	h.objectStatus = func(string, int) int { return http.StatusForbidden }
	h.extraFiles = func(call int) []api.WorkspaceDownloadFile {
		if call == 2 {
			return []api.WorkspaceDownloadFile{{Path: "new.txt", URL: h.srv.URL + "/o/new.txt", Size: 1}}
		}
		return nil
	}
	_, _, err := runSignedDownload(t, h, nil)
	requireSignedClass(t, err, signedClassListingChanged)
}

// TestSignedDownload_EndpointRetriesThenFailsClosed: a page request that
// keeps failing with a 5xx or a transport error is retried after each
// backoff, then fails closed; a 404 or 409 fails at once.
func TestSignedDownload_EndpointRetriesThenFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantClass string
		wantCalls int
	}{
		{"5xx", &apiclient.APIError{StatusCode: http.StatusBadGateway}, "endpoint_502", 1 + len(defaultSignedPageBackoff)},
		{"transport", &url.Error{Op: "Post", URL: "https://hub.example/x?X-Goog-Signature=nope", Err: errors.New("connection refused")}, signedClassTransport, 1 + len(defaultSignedPageBackoff)},
		{"404", &apiclient.APIError{StatusCode: http.StatusNotFound}, "endpoint_404", 1},
		{"409", &apiclient.APIError{StatusCode: http.StatusConflict}, "endpoint_409", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeSignedHub(t, map[string]string{"a": "1"})
			h.pageErr = func(int) error { return tc.err }
			var waits []time.Duration
			_, _, err := runSignedDownload(t, h, func(st *signedDownloadSettings) {
				st.sleep = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return nil }
			})
			requireSignedClass(t, err, tc.wantClass)
			if h.pageCalls != tc.wantCalls {
				t.Fatalf("page calls = %d, want %d", h.pageCalls, tc.wantCalls)
			}
			if tc.wantCalls > 1 && fmt.Sprint(waits) != fmt.Sprint(defaultSignedPageBackoff) {
				t.Fatalf("waits = %v, want %v", waits, defaultSignedPageBackoff)
			}
			if strings.Contains(err.Error(), "X-Goog-") || strings.Contains(err.Error(), "hub.example") {
				t.Fatalf("error text leaks the URL: %v", err)
			}
		})
	}
	t.Run("recovers after a 5xx", func(t *testing.T) {
		h := newFakeSignedHub(t, map[string]string{"a": "1"})
		h.pageErr = func(call int) error {
			if call == 1 {
				return &apiclient.APIError{StatusCode: http.StatusServiceUnavailable}
			}
			return nil
		}
		if _, _, err := runSignedDownload(t, h, nil); err != nil {
			t.Fatal(err)
		}
	})
}

// TestSignedDownload_ObjectRetries: an object GET answered 5xx is retried
// up to three times in all.
func TestSignedDownload_ObjectRetries(t *testing.T) {
	h := newFakeSignedHub(t, map[string]string{"a": "1"})
	h.objectStatus = func(string, int) int { return http.StatusInternalServerError }
	_, _, err := runSignedDownload(t, h, nil)
	requireSignedClass(t, err, "http_500")
	if n := h.getCount("a"); n != 1+len(defaultSignedFileBackoff) {
		t.Fatalf("GETs = %d, want %d", n, 1+len(defaultSignedFileBackoff))
	}
}

// TestSignedDownload_StreamsLargeFile: a file much larger than the allowed
// heap growth is streamed to disk, not buffered.
func TestSignedDownload_StreamsLargeFile(t *testing.T) {
	const size = 64 << 20
	obj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte("z"), 1<<20)
		for i := 0; i < size/len(chunk); i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer obj.Close()
	fetch := func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error) {
		return &api.WorkspaceDownloadResponse{Files: []api.WorkspaceDownloadFile{{Path: "big.bin", URL: obj.URL + "/big", Size: size}}, Expires: time.Now().Add(time.Hour)}, nil
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	dir := t.TempDir()
	srv := &Server{signedDownload: signedDownloadSettings{fetchPage: fetch, sleep: noSleep}}
	if _, err := srv.signedWorkspaceDownload(context.Background(), CreateAgentRequest{}, dir); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if grew := int64(after.TotalAlloc) - int64(before.TotalAlloc); grew > size/2 {
		t.Fatalf("allocated %d bytes for a %d byte file; it must stream", grew, size)
	}
	if fi, err := os.Stat(filepath.Join(dir, "big.bin")); err != nil || fi.Size() != size {
		t.Fatalf("big.bin: %v %v", fi, err)
	}
}

// TestSignedDownloadError_TextIsFixed: the error text carries the stage,
// page, file and class only.
func TestSignedDownloadError_TextIsFixed(t *testing.T) {
	e := &signedDownloadError{Stage: "file", Page: 2, File: 7, Status: 403, Class: "http_403"}
	if got := e.Error(); got != "signed workspace download: page 2 file 7: http_403" {
		t.Fatalf("Error() = %q", got)
	}
	if got := (&signedDownloadError{Stage: "page", Page: 1, File: -1, Class: "endpoint_502"}).Error(); got != "signed workspace download: page 1 request: endpoint_502" {
		t.Fatalf("Error() = %q", got)
	}
}

// TestTransportClass covers the classes of the transport failures the
// redaction test injects, without keeping any text.
func TestTransportClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, signedClassCanceled},
		{context.DeadlineExceeded, signedClassTimeout},
		{&url.Error{Op: "Get", URL: "u", Err: context.DeadlineExceeded}, signedClassTimeout},
		{io.ErrUnexpectedEOF, signedClassReset},
		{errors.New("other"), signedClassTransport},
	} {
		if got := transportClass(tc.err); got != tc.want {
			t.Errorf("transportClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
