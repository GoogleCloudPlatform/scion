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
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// --- helpers ---

func archivePath(id string, seq int) string {
	return fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/archive", id, seq)
}

func archiveOnAlways(context.Context) bool { return true }

// newArchiveFixture is a fixture with the archive route on.
func newArchiveFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, false)
	f.svc.SetArchiveEnabled(archiveOnAlways)
	return f
}

// archive serves an archive request, reporting whether the handler aborted
// the response (http.ErrAbortHandler), as the hub's recovery middleware
// would let it through.
func (f *fixture) archive(p *principal, method, target string, hdr map[string]string) (rec *httptest.ResponseRecorder, aborted bool) {
	f.t.Helper()
	rec = httptest.NewRecorder()
	return rec, f.serveArchive(rec, p, method, target, hdr)
}

// serveArchive serves an archive request into w and reports whether the
// handler aborted the response.
func (f *fixture) serveArchive(w http.ResponseWriter, p *principal, method, target string, hdr map[string]string) (aborted bool) {
	f.t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if p != nil {
		r = withPrincipal(r, *p)
	}
	defer func() {
		if v := recover(); v != nil {
			if v != http.ErrAbortHandler {
				panic(v)
			}
			aborted = true
		}
	}()
	f.svc.ServeHTTP(w, r)
	return false
}

// mustArchive serves a GET that must stream a whole archive.
func (f *fixture) mustArchive(p principal, id string, seq int) *httptest.ResponseRecorder {
	f.t.Helper()
	rec, aborted := f.archive(&p, http.MethodGet, archivePath(id, seq), nil)
	if aborted || rec.Code != http.StatusOK {
		f.t.Fatalf("archive: aborted=%v status %d: %s", aborted, rec.Code, rec.Body.String())
	}
	return rec
}

type archiveEntry struct {
	name   string
	method uint16
	mode   uint32
	body   []byte
}

// readArchive parses a zip body into its entries, in archive order.
func readArchive(t *testing.T, body []byte) []archiveEntry {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	var out []archiveEntry
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("entry %s: %v", zf.Name, err)
		}
		out = append(out, archiveEntry{name: zf.Name, method: zf.Method, mode: uint32(zf.Mode().Perm()), body: b})
	}
	return out
}

// getVersion reads GET /{id}/versions/{seq} as p, decoded.
func (f *fixture) getVersion(p principal, id string, seq int) (ArtifactResponse, map[string]json.RawMessage) {
	f.t.Helper()
	rec := f.do(&p, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", id, seq), nil, nil)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("get version: %d %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		f.t.Fatal(err)
	}
	return decodeInto[ArtifactResponse](f.t, rec), raw
}

// countingBlobs counts blob downloads.
type countingBlobs struct {
	storage.Storage
	downloads atomic.Int32
}

func (c *countingBlobs) Download(ctx context.Context, p string) (io.ReadCloser, *storage.Object, error) {
	c.downloads.Add(1)
	return c.Storage.Download(ctx, p)
}

func (f *fixture) countDownloads() *countingBlobs {
	c := &countingBlobs{Storage: f.blobs}
	f.svc.SetBlobStorage(c, "hub-1")
	return c
}

// overwriteBlob replaces the stored bytes of digest.
func (f *fixture) overwriteBlob(digest string, body []byte) {
	f.t.Helper()
	if _, err := f.blobs.Upload(context.Background(), BlobPath("hub-1", digest), bytes.NewReader(body), storage.UploadOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

// captureLogs sends the default logger to a buffer at level until the test
// ends.
func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// nested is a bundle with nested directories and a mix of stored and
// compressed media types.
var nested = bundle{
	"index.html":                []byte(`<html><img src="img/a.png"><a href="docs/guide/intro.md">intro</a></html>`),
	"img/a.png":                 append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1, 2, 3}, 300)...),
	"css/s.css":                 []byte(strings.Repeat("body{margin:0}\n", 200)),
	"docs/guide/intro.md":       []byte("# Intro\n\nWords.\n"),
	"docs/guide/deep/notes.dat": bytes.Repeat([]byte{0, 255, 7}, 1000),
}

func (f *fixture) publishNested(title string) ArtifactResponse {
	f.t.Helper()
	req := nested.manifest("index.html")
	req.Title = title
	return f.publishBundle(agentA, "/api/v1/artifacts", req, nested)
}

// --- round trips ---

// TestArchiveRoundTripBundle: a multi-file version downloads as a zip
// whose entries are exactly the manifest paths, sorted, byte for byte,
// with already-compressed media stored and the rest deflated, mode 0644,
// and no directory entries. The response carries the file headers and no
// Content-Length.
func TestArchiveRoundTripBundle(t *testing.T) {
	f := newArchiveFixture(t)
	resp := f.publishNested("Design Notes")
	id := resp.Artifact.ID

	rec := f.mustArchive(agentA, id, 1)
	entries := readArchive(t, rec.Body.Bytes())

	var want []string
	for p := range nested {
		want = append(want, p)
	}
	sort.Strings(want)
	if len(entries) != len(want) {
		t.Fatalf("entries %d, want %d", len(entries), len(want))
	}
	media := map[string]string{}
	for _, fi := range resp.Version.Files {
		media[fi.Path] = fi.MediaType
	}
	for i, e := range entries {
		if e.name != want[i] {
			t.Errorf("entry %d = %q, want %q", i, e.name, want[i])
		}
		if !bytes.Equal(e.body, nested[e.name]) {
			t.Errorf("entry %s: bytes differ", e.name)
		}
		wantMethod := zip.Deflate
		if archiveStored(media[e.name]) {
			wantMethod = zip.Store
		}
		if e.method != wantMethod {
			t.Errorf("entry %s (%s): method %d, want %d", e.name, media[e.name], e.method, wantMethod)
		}
		if e.mode != 0o644 {
			t.Errorf("entry %s: mode %o", e.name, e.mode)
		}
		if strings.HasSuffix(e.name, "/") {
			t.Errorf("directory entry %q", e.name)
		}
	}
	for _, e := range entries {
		if e.name == "img/a.png" && e.method != zip.Store {
			t.Errorf("a PNG must be stored, got method %d", e.method)
		}
		if e.name == "css/s.css" && e.method != zip.Deflate {
			t.Errorf("a stylesheet must be deflated, got method %d", e.method)
		}
	}

	h := rec.Header()
	for k, v := range map[string]string{
		"Content-Type":            "application/zip",
		"Content-Disposition":     "attachment; filename=design-notes-v1.zip",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "private, no-cache",
		"Content-Security-Policy": fileCSP,
	} {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if h.Get("Content-Length") != "" {
		t.Errorf("Content-Length %q set on a streamed archive", h.Get("Content-Length"))
	}
	etag := h.Get("ETag")
	if !strings.HasPrefix(etag, `"archive-v1:`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag %q", etag)
	}
	if again := f.mustArchive(agentA, id, 1); again.Header().Get("ETag") != etag || !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) {
		t.Error("a second download of the same version differs")
	}
}

// TestArchiveRoundTripSingleFile: a single-file version is a one-entry zip
// under its manifest path; another reader of the project downloads it too.
func TestArchiveRoundTripSingleFile(t *testing.T) {
	f := newArchiveFixture(t)
	body := []byte("# One file\n\nOnly this.\n")
	id := f.publish(agentA, "report.md", body, "").Artifact.ID
	for _, p := range []principal{agentA, agentB} {
		entries := readArchive(t, f.mustArchive(p, id, 1).Body.Bytes())
		if len(entries) != 1 || entries[0].name != "report.md" || !bytes.Equal(entries[0].body, body) {
			t.Fatalf("%s: entries %+v", p.ref, entries)
		}
	}
}

// TestArchiveVersionSpecific: each version's archive holds that version's
// files only.
func TestArchiveVersionSpecific(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publish(agentA, "a.md", []byte("v1"), "").Artifact.ID
	two := bundle{"a.md": []byte("v2"), "b.md": []byte("added")}
	f.publishBundle(agentA, "/api/v1/artifacts/"+id+"/versions", two.manifest("a.md"), two)
	e1 := readArchive(t, f.mustArchive(agentA, id, 1).Body.Bytes())
	e2 := readArchive(t, f.mustArchive(agentA, id, 2).Body.Bytes())
	if len(e1) != 1 || string(e1[0].body) != "v1" {
		t.Errorf("version 1: %+v", e1)
	}
	if len(e2) != 2 || e2[0].name != "a.md" || string(e2[0].body) != "v2" || e2[1].name != "b.md" {
		t.Errorf("version 2: %+v", e2)
	}
}

// TestArchiveLeavesOutRemoteImages: images the hub fetched at publish time
// are not in the archive, as on the Files tab.
func TestArchiveLeavesOutRemoteImages(t *testing.T) {
	f := newArchiveFixture(t)
	f.useFetcher(&fakeFetcher{bodies: map[string][]byte{"https://img.example/a.png": testPNG}})
	md := []byte("![a](https://img.example/a.png)\n")
	resp := f.publish(agentA, "doc.md", md, "")
	if resp.Version.FileCount != 2 {
		t.Fatalf("files %d: want the entry and one remote image", resp.Version.FileCount)
	}
	entries := readArchive(t, f.mustArchive(agentA, resp.Artifact.ID, 1).Body.Bytes())
	if len(entries) != 1 || entries[0].name != "doc.md" || !bytes.Equal(entries[0].body, md) {
		t.Fatalf("entries %+v", entries)
	}
}

// --- gate and access ---

// TestArchiveGateOff: while the host has not turned the route on, or turns
// it off, it answers the standard 404 and responses carry no archive field.
func TestArchiveGateOff(t *testing.T) {
	f := newFixture(t, false)
	id := f.publish(agentA, "report.md", []byte("# r"), "").Artifact.ID
	missing := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/00000000-0000-4000-8000-000000000000", nil, nil)
	for name, fn := range map[string]func(context.Context) bool{
		"unset": nil,
		"off":   func(context.Context) bool { return false },
	} {
		f.svc.SetArchiveEnabled(fn)
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec, aborted := f.archive(&agentA, method, archivePath(id, 1), nil)
			if aborted || rec.Code != http.StatusNotFound || (method == http.MethodGet && rec.Body.String() != missing.Body.String()) {
				t.Errorf("%s %s: %d %q", name, method, rec.Code, rec.Body.String())
			}
		}
		_, raw := f.getVersion(agentA, id, 1)
		if _, ok := raw["archive"]; ok {
			t.Errorf("%s: version response has an archive field", name)
		}
		rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
		if strings.Contains(rec.Body.String(), `"archive"`) {
			t.Errorf("%s: artifact response has an archive field", name)
		}
	}
	f.svc.SetArchiveEnabled(archiveOnAlways)
	resp, _ := f.getVersion(agentA, id, 1)
	if resp.Archive == nil || !resp.Archive.Available || resp.Archive.Reason != "" {
		t.Errorf("on: archive %+v", resp.Archive)
	}
	rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
	if a := decodeInto[ArtifactResponse](t, rec).Archive; a == nil || !a.Available {
		t.Errorf("on: artifact response archive %+v", a)
	}
}

// TestArchiveAccessParity: every caller who may not read a version of a
// file gets, from the archive route, exactly the answer a file read gives
// (the same 404); a caller who may read it downloads the archive.
func TestArchiveAccessParity(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publish(agentA, "report.md", []byte("# r"), "").Artifact.ID
	pend := f.createPending(agentA, "/api/v1/artifacts/"+id+"/versions", bundle{"report.md": []byte("# r2")}.manifest("report.md"))
	expired := f.publish(agentA, "old.md", []byte("# old"), "").Artifact.ID
	f.execSQL(`UPDATE artifact SET expires_at = ? WHERE id = ?`, linkPast(), expired)
	f.host.deny(agentB, "project-1", PermissionRead)

	cases := []struct {
		name string
		p    *principal
		id   string
		seq  int
		file string
	}{
		{"no principal", nil, id, 1, "report.md"},
		{"user without access", &outside, id, 1, "report.md"},
		{"agent of another project", &agentX, id, 1, "report.md"},
		{"credential does not permit read", &agentB, id, 1, "report.md"},
		{"pending version, owner", &agentA, id, pend.Version.Seq, "report.md"},
		{"missing version, owner", &agentA, id, 9, "report.md"},
		{"expired artifact, owner", &agentA, expired, 1, "old.md"},
		{"missing artifact", &agentA, "00000000-0000-4000-8000-000000000000", 1, "report.md"},
	}
	for _, c := range cases {
		file := f.do(c.p, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/%s", c.id, c.seq, c.file), nil, nil)
		if file.Code != http.StatusNotFound {
			t.Fatalf("%s: file read %d, want 404", c.name, file.Code)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec, aborted := f.archive(c.p, method, archivePath(c.id, c.seq), nil)
			if aborted {
				t.Fatalf("%s %s: aborted", c.name, method)
			}
			if method == http.MethodGet && response(rec) != response(file) {
				t.Errorf("%s: archive answer\n%s\nfile answer\n%s", c.name, response(rec), response(file))
			}
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", c.name, method, rec.Code)
			}
		}
	}

	// A principal grant opens the archive as it opens the files.
	f.grantPrincipal(id, outside)
	f.mustArchive(outside, id, 1)
}

// TestArchiveAccessParityAutomaticReviewGrant: the delegating user's
// automatic review grant (ptone/scion#7134) does not open the archive, or
// the version's files, without a role that reads in the home project. The
// archive follows the same role check as file reads: both routes ask the
// same readableArtifact, so the user gets the archive exactly while the
// role lets them read the files.
func TestArchiveAccessParityAutomaticReviewGrant(t *testing.T) {
	f, _ := newSponsorReviewFixture(t)
	f.svc.SetArchiveEnabled(archiveOnAlways)
	id := f.publish(agentA, "a.md", []byte("# a"), "").Artifact.ID
	checkSponsorGrant(t, f, id)
	check := func(stage string, want int) {
		t.Helper()
		file := f.do(&srSponsor, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/1/files/a.md", id), nil, nil)
		if file.Code != want {
			t.Fatalf("%s: file read %d, want %d", stage, file.Code, want)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec, aborted := f.archive(&srSponsor, method, archivePath(id, 1), nil)
			if aborted {
				t.Fatalf("%s %s: aborted", stage, method)
			}
			if rec.Code != want {
				t.Errorf("%s %s: archive %d, want %d", stage, method, rec.Code, want)
			}
			if want != http.StatusOK && method == http.MethodGet && response(rec) != response(file) {
				t.Errorf("%s: archive answer\n%s\nfile answer\n%s", stage, response(rec), response(file))
			}
		}
	}

	check("with a role in the home project", http.StatusOK)
	f.host.mu.Lock()
	delete(f.host.perms[srSponsorID], "project-1")
	f.host.mu.Unlock()
	check("after leaving the home project", http.StatusNotFound)
	f.host.allow(srSponsor, "project-1", PermissionRead)
	check("after being re-added", http.StatusOK)
}

// TestArchiveNotReachableByShareLink: a share-link token never yields an
// archive: the share-link route has no archive form, and the archive route
// itself needs a caller (a link holder has none).
func TestArchiveNotReachableByShareLink(t *testing.T) {
	f, id := newLinkFixture(t)
	f.svc.SetArchiveEnabled(archiveOnAlways)
	token, _ := f.mustMintLink(userU, id, `{"ttlHours":1}`)
	for _, target := range []string{
		RouteShared + token + "/archive",
		RouteShared + token + "/versions/1/archive",
		archivePath(id, 1) + "?token=" + token,
	} {
		rec := f.shared(target, "")
		if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") == "application/zip" {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	// The link still works for what it is for.
	if rec := f.shared(RouteShared+token, ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("link read: %d", rec.Code)
	}
}

func TestArchiveMethods(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publish(agentA, "a.md", []byte("a"), "").Artifact.ID
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec, _ := f.archive(&agentA, method, archivePath(id, 1), nil)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d Allow %q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

// --- limits ---

// TestArchiveTooLarge: a version over the current file count or bundle
// size limits answers 413 before any archive byte (no zip headers, no blob
// read), on GET and HEAD, and the version response says why the archive is
// not available.
func TestArchiveTooLarge(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publishNested("Big").Artifact.ID
	count := f.countDownloads()
	var total int64
	for _, b := range nested {
		total += int64(len(b))
	}
	for name, lim := range map[string]Limits{
		"files": {MaxFiles: len(nested) - 1},
		"bytes": {MaxBundleBytes: total - 1},
	} {
		f.svc.SetLimits(func(context.Context) Limits { return lim })
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec, aborted := f.archive(&agentA, method, archivePath(id, 1), nil)
			if aborted || rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s %s: aborted=%v %d", name, method, aborted, rec.Code)
			}
			if method == http.MethodGet && errCode(t, rec) != CodeArchiveTooLarge {
				t.Errorf("%s: code %s", name, errCode(t, rec))
			}
			if rec.Header().Get("Content-Disposition") != "" || rec.Header().Get("Content-Type") == "application/zip" ||
				rec.Header().Get("ETag") != "" || rec.Header().Get("Content-Security-Policy") != "" {
				t.Errorf("%s %s: archive headers on a refusal", name, method)
			}
		}
		resp, _ := f.getVersion(agentA, id, 1)
		if resp.Archive == nil || resp.Archive.Available || resp.Archive.Reason != ArchiveReasonTooLarge {
			t.Errorf("%s: archive %+v", name, resp.Archive)
		}
	}
	if n := count.downloads.Load(); n != 0 {
		t.Errorf("%d blob reads for refused archives", n)
	}
	// At the limits exactly, the archive is served.
	f.svc.SetLimits(func(context.Context) Limits { return Limits{MaxFiles: len(nested), MaxBundleBytes: total} })
	f.mustArchive(agentA, id, 1)
}

// TestArchiveBusy: over the per-caller cap or the service-wide cap the
// route answers 429 with Retry-After before any archive byte; a released
// slot is usable again.
func TestArchiveBusy(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publish(agentA, "a.md", []byte("a"), "").Artifact.ID
	slots := f.svc.archiveSlotsFor()
	busy := func(p principal) {
		t.Helper()
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec, aborted := f.archive(&p, method, archivePath(id, 1), nil)
			if aborted || rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "10" {
				t.Fatalf("%s %s: aborted=%v %d Retry-After %q", p.ref, method, aborted, rec.Code, rec.Header().Get("Retry-After"))
			}
			if method == http.MethodGet && errCode(t, rec) != CodeArchiveBusy {
				t.Errorf("code %s", errCode(t, rec))
			}
			for _, k := range []string{"Content-Disposition", "ETag", "Content-Security-Policy", "Referrer-Policy"} {
				if rec.Header().Get(k) != "" {
					t.Errorf("%s: archive header %s on a busy answer", method, k)
				}
			}
		}
	}

	// agentA holds its two: it is refused, agentB is not.
	var releases []func()
	for range ArchiveMaxConcurrentPerPrincipal {
		rel, ok := slots.acquire(PrincipalRef(agentA.kind, agentA.ref))
		if !ok {
			t.Fatal("acquire")
		}
		releases = append(releases, rel)
	}
	busy(agentA)
	f.mustArchive(agentB, id, 1)

	// Others fill the service: agentB is refused too.
	for i := ArchiveMaxConcurrentPerPrincipal; i < ArchiveMaxConcurrent; i++ {
		rel, ok := slots.acquire(fmt.Sprintf("user:other-%d", i))
		if !ok {
			t.Fatal("acquire")
		}
		releases = append(releases, rel)
	}
	busy(agentB)

	for _, rel := range releases {
		rel()
		rel() // a second call is a no-op
	}
	f.mustArchive(agentA, id, 1)
	if slots.total != 0 || len(slots.byPrincipal) != 0 {
		t.Errorf("slots after the requests: %d %v", slots.total, slots.byPrincipal)
	}
}

func TestArchiveSlots(t *testing.T) {
	a := &archiveSlots{byPrincipal: map[string]int{}, max: 3, maxEach: 2}
	r1, ok1 := a.acquire("p")
	_, ok2 := a.acquire("p")
	_, ok3 := a.acquire("p")
	_, ok4 := a.acquire("q")
	_, ok5 := a.acquire("r")
	if !ok1 || !ok2 || ok3 || !ok4 || ok5 {
		t.Fatalf("acquire: %v %v %v %v %v", ok1, ok2, ok3, ok4, ok5)
	}
	r1()
	r1()
	if a.total != 2 || a.byPrincipal["p"] != 1 {
		t.Fatalf("after release: %d %v", a.total, a.byPrincipal)
	}
	if _, ok := a.acquire("r"); !ok {
		t.Fatal("a released slot is not reusable")
	}
}

// --- caching and HEAD ---

// TestArchiveConditional: a matching If-None-Match answers 304 only after
// the read check, without reading a blob; a caller who may not read gets
// the 404 whatever it sends.
func TestArchiveConditional(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publishNested("Cached").Artifact.ID
	etag := f.mustArchive(agentA, id, 1).Header().Get("ETag")
	count := f.countDownloads()
	hdr := map[string]string{"If-None-Match": etag}

	rec, _ := f.archive(&agentA, http.MethodGet, archivePath(id, 1), hdr)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
		t.Fatalf("owner: %d %q", rec.Code, rec.Body.String())
	}
	rec, _ = f.archive(&outside, http.MethodGet, archivePath(id, 1), hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("outsider with the ETag: %d", rec.Code)
	}
	rec, _ = f.archive(&agentA, http.MethodGet, archivePath(id, 1), map[string]string{"If-None-Match": `"archive-v1:other"`})
	if rec.Code != http.StatusOK {
		t.Fatalf("other ETag: %d", rec.Code)
	}
	if n := count.downloads.Load(); n != int32(len(nested)) {
		t.Errorf("downloads %d, want %d (only the full GET)", n, len(nested))
	}
}

// TestArchiveHead: HEAD answers the GET's status and headers with no body,
// and opens no blob.
func TestArchiveHead(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publishNested("Head").Artifact.ID
	get := f.mustArchive(agentA, id, 1)
	count := f.countDownloads()
	rec, aborted := f.archive(&agentA, http.MethodHead, archivePath(id, 1), nil)
	if aborted || rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD: aborted=%v %d len %d", aborted, rec.Code, rec.Body.Len())
	}
	for _, k := range []string{"Content-Type", "Content-Disposition", "ETag", "Cache-Control", "X-Content-Type-Options", "Content-Security-Policy", "Referrer-Policy"} {
		if rec.Header().Get(k) != get.Header().Get(k) {
			t.Errorf("%s: HEAD %q, GET %q", k, rec.Header().Get(k), get.Header().Get(k))
		}
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Error("HEAD sets Content-Length")
	}
	if n := count.downloads.Load(); n != 0 {
		t.Errorf("HEAD read %d blobs", n)
	}
	if slots := f.svc.archiveSlotsFor(); slots.total != 0 {
		t.Errorf("HEAD left %d slots taken", slots.total)
	}
}

// --- failures ---

// TestArchiveFirstBlobMissing: when the first blob cannot be opened the
// route answers a clean 500 before any archive byte.
func TestArchiveFirstBlobMissing(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publishNested("Broken").Artifact.ID
	// css/s.css sorts first.
	if err := f.blobs.Delete(context.Background(), BlobPath("hub-1", sha(nested["css/s.css"]))); err != nil {
		t.Fatal(err)
	}
	rec, aborted := f.archive(&agentA, http.MethodGet, archivePath(id, 1), nil)
	if aborted || rec.Code != http.StatusInternalServerError || errCode(t, rec) != "internal" {
		t.Fatalf("aborted=%v %d %s", aborted, rec.Code, rec.Body.String())
	}
	// A plain JSON error: none of the archive's file headers.
	for _, k := range []string{"Content-Disposition", "ETag", "Content-Security-Policy", "Referrer-Policy"} {
		if rec.Header().Get(k) != "" {
			t.Errorf("archive header %s on the 500: %v", k, rec.Header())
		}
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("not the service's JSON error headers: %v", rec.Header())
	}
	if f.svc.archiveSlotsFor().total != 0 {
		t.Error("slot not released")
	}
}

// TestArchiveAbortsOnSizeMismatch: a blob longer or shorter than its
// manifest size, or one that cannot be opened mid-archive, aborts the
// response (the client sees a failed download, never a short archive), and
// the slot is released. The log names the artifact, the version and the
// failure class, never the file's path or bytes outside debug.
func TestArchiveAbortsOnSizeMismatch(t *testing.T) {
	for name, damage := range map[string]func(f *fixture){
		"longer": func(f *fixture) {
			f.overwriteBlob(sha(nested["index.html"]), append(bytes.Clone(nested["index.html"]), "extra-secret-bytes"...))
		},
		"shorter": func(f *fixture) { f.overwriteBlob(sha(nested["index.html"]), []byte("<html>secret")) },
		"missing": func(f *fixture) {
			_ = f.blobs.Delete(context.Background(), BlobPath("hub-1", sha(nested["index.html"])))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newArchiveFixture(t)
			resp := f.publishNested("Damaged")
			damage(f)
			logs := captureLogs(t, slog.LevelInfo)
			rec, aborted := f.archive(&agentA, http.MethodGet, archivePath(resp.Artifact.ID, 1), nil)
			if !aborted {
				t.Fatalf("not aborted: %d, %d bytes", rec.Code, rec.Body.Len())
			}
			if f.svc.archiveSlotsFor().total != 0 {
				t.Error("slot not released after the abort")
			}
			out := logs.String()
			wantClass := map[string]string{"longer": archiveFailSizeMatch, "shorter": archiveFailSizeMatch, "missing": archiveFailBlobOpen}[name]
			for _, want := range []string{resp.Artifact.ID, "class=" + wantClass, "artifacts: archive failed"} {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %q:\n%s", want, out)
				}
			}
			for _, never := range []string{"index.html", "secret"} {
				if strings.Contains(out, never) {
					t.Errorf("log at info carries %q:\n%s", never, out)
				}
			}
		})
	}
}

// TestArchiveTimeLimit: an archive that runs past its time limit is
// aborted.
func TestArchiveTimeLimit(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publishNested("Slow").Artifact.ID
	start := time.Now()
	var calls atomic.Int32
	f.svc.archiveNow = func() time.Time {
		// The limit and the first deadline are computed at start; every
		// write after that sees a clock past the limit.
		if calls.Add(1) <= 2 {
			return start
		}
		return start.Add(time.Hour)
	}
	logs := captureLogs(t, slog.LevelInfo)
	if _, aborted := f.archive(&agentA, http.MethodGet, archivePath(id, 1), nil); !aborted {
		t.Fatal("not aborted")
	}
	if !strings.Contains(logs.String(), "class="+archiveFailTimeLimit) {
		t.Errorf("log: %s", logs.String())
	}
	if f.svc.archiveSlotsFor().total != 0 {
		t.Error("slot not released")
	}
}

func TestArchiveTimeLimitBySize(t *testing.T) {
	for _, c := range []struct {
		bytes int64
		want  time.Duration
	}{
		{0, 2 * time.Minute},
		{1, 3 * time.Minute},
		{16 << 20, 3 * time.Minute},
		{16<<20 + 1, 4 * time.Minute},
		{256 << 20, 18 * time.Minute},
	} {
		if got := archiveTimeLimit(c.bytes); got != c.want {
			t.Errorf("archiveTimeLimit(%d) = %v, want %v", c.bytes, got, c.want)
		}
	}
}

// --- entry names ---

// TestArchiveEntryNameOK pins the entry names the archive refuses, beyond
// what cleanFilePath already refuses.
func TestArchiveEntryNameOK(t *testing.T) {
	seg := strings.Repeat("a", 200)
	at1024 := strings.Join([]string{seg, seg, seg, seg, seg}, "/") + "/" + strings.Repeat("b", 19)
	if len(at1024) != 1024 {
		t.Fatalf("fixture length %d", len(at1024))
	}
	for name, want := range map[string]bool{
		"a.md":                  true,
		"docs/guide/intro.md":   true,
		"ünïcode/文件.md":         true,
		"with space/and-dash_x": true,
		at1024:                  true,
		at1024 + "b":            false,
		"":                      false,
		`a\b.md`:                false,
		"a:b.md":                false,
		"C:/x.md":               false,
		"a\x00b":                false,
		"a\x01b":                false,
		"a\tb":                  false,
		"a\x7fb":                false,
		"a\u0085b":              false,
		"a\u200eb":              false,
		"/abs.md":               false,
		"../up.md":              false,
		"a/../b.md":             false,
		"a/./b.md":              false,
		"a//b.md":               false,
		"a/":                    false,
		"..":                    false,
		"\xff.md":               false,
	} {
		if got := archiveEntryNameOK(name); got != want {
			t.Errorf("archiveEntryNameOK(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestArchiveRefusesUnsupportedName: a version holding a name the archive
// refuses answers 422 before any archive byte, and its response says the
// archive is not available. The name is not logged outside debug.
func TestArchiveRefusesUnsupportedName(t *testing.T) {
	for _, bad := range []string{"notes:v1.md", `dir\notes.md`, "../notes.md"} {
		t.Run(bad, func(t *testing.T) {
			f := newArchiveFixture(t)
			id := f.publish(agentA, "notes.md", []byte("# n"), "").Artifact.ID
			f.execSQL(`UPDATE artifact_file SET path = ? WHERE path = 'notes.md' AND version_id IN (SELECT id FROM artifact_version WHERE artifact_id = ?)`, bad, id)
			count := f.countDownloads()
			logs := captureLogs(t, slog.LevelInfo)
			rec, aborted := f.archive(&agentA, http.MethodGet, archivePath(id, 1), nil)
			if aborted || rec.Code != http.StatusUnprocessableEntity || errCode(t, rec) != CodeArchiveUnsupportedName {
				t.Fatalf("aborted=%v %d %s", aborted, rec.Code, rec.Body.String())
			}
			if n := count.downloads.Load(); n != 0 {
				t.Errorf("%d blob reads", n)
			}
			if strings.Contains(logs.String(), "notes") || !strings.Contains(logs.String(), "class="+archiveFailEntryName) {
				t.Errorf("log: %s", logs.String())
			}
			resp, _ := f.getVersion(agentA, id, 1)
			if resp.Archive == nil || resp.Archive.Available || resp.Archive.Reason != ArchiveReasonUnsupportedName {
				t.Errorf("archive %+v", resp.Archive)
			}
		})
	}
}

// --- names and media ---

func TestArchiveName(t *testing.T) {
	id := "0123abcd-0000-4000-8000-000000000000"
	long := strings.Repeat("a", 100)
	for _, c := range []struct {
		title string
		seq   int
		want  string
	}{
		{"Q3 Report", 1, "q3-report-v1.zip"},
		{"Héllo, Wörld!", 2, "h-llo-w-rld-v2.zip"},
		{`quote"s and \slashes/`, 1, "quote-s-and-slashes-v1.zip"},
		{"  --Trim me--  ", 4, "trim-me-v4.zip"},
		{"", 3, "artifact-0123abcd-v3.zip"},
		{"!!! ???", 3, "artifact-0123abcd-v3.zip"},
		{"日本語", 1, "artifact-0123abcd-v1.zip"},
		{long, 1, strings.Repeat("a", 64) + "-v1.zip"},
		{strings.Repeat("a", 63) + " bcd", 1, strings.Repeat("a", 63) + "-v1.zip"},
	} {
		got := archiveName(&Artifact{ID: id, Title: c.title}, c.seq)
		if got != c.want {
			t.Errorf("archiveName(%q) = %q, want %q", c.title, got, c.want)
		}
		disp := mime.FormatMediaType("attachment", map[string]string{"filename": got})
		if disp != "attachment; filename="+got {
			t.Errorf("disposition %q", disp)
		}
	}
}

func TestArchiveStored(t *testing.T) {
	for mt, want := range map[string]bool{
		"image/png":                true,
		"image/jpeg":               true,
		"IMAGE/WEBP":               true,
		"image/svg+xml":            false,
		"application/zip":          true,
		"application/gzip":         true,
		"application/pdf":          true,
		"video/mp4":                true,
		"audio/mpeg":               true,
		"font/woff2":               true,
		"text/html; charset=utf-8": false,
		"text/markdown":            false,
		"application/json":         false,
		"application/octet-stream": false,
		"image/png; foo=bar":       true,
		"":                         false,
	} {
		if got := archiveStored(mt); got != want {
			t.Errorf("archiveStored(%q) = %v, want %v", mt, got, want)
		}
	}
}

// --- write deadline and client loss ---

// deadlineRecorder records every SetWriteDeadline call
// (http.ResponseController) with the number of body bytes written by then.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	calls []deadlineCall
}

type deadlineCall struct {
	deadline time.Time
	written  int
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.calls = append(d.calls, deadlineCall{deadline: t, written: d.Body.Len()})
	return nil
}

// TestArchiveRollingWriteDeadline: the write deadline is set
// archiveIdleWindow ahead once before the first byte of the archive, then
// again after every archiveDeadlineStep bytes written, so a client that
// reads less than that much in the window is cut off.
func TestArchiveRollingWriteDeadline(t *testing.T) {
	f := newArchiveFixture(t)
	body := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 7<<19)...) // a 3.5 MiB stored entry
	rnd := rand.New(rand.NewSource(7106))
	_, _ = rnd.Read(body[8:])
	id := f.publish(agentA, "big.png", body, "").Artifact.ID
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	f.svc.archiveNow = func() time.Time { return t0 }

	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	if aborted := f.serveArchive(rec, &agentA, http.MethodGet, archivePath(id, 1), nil); aborted || rec.Code != http.StatusOK {
		t.Fatalf("aborted=%v status %d", aborted, rec.Code)
	}
	entries := readArchive(t, rec.Body.Bytes())
	if len(entries) != 1 || !bytes.Equal(entries[0].body, body) {
		t.Fatal("archive content differs")
	}
	total := rec.Body.Len()
	if len(rec.calls) == 0 || rec.calls[0].written != 0 {
		t.Fatalf("first deadline not set before the first byte: %+v", rec.calls)
	}
	for i, c := range rec.calls {
		if !c.deadline.Equal(t0.Add(archiveIdleWindow)) {
			t.Errorf("call %d: deadline %v, want now+%v", i, c.deadline, archiveIdleWindow)
		}
		if i > 0 && c.written-rec.calls[i-1].written < archiveDeadlineStep {
			t.Errorf("call %d after %d bytes, less than one step since the last", i, c.written-rec.calls[i-1].written)
		}
	}
	// One extension per full step; writes are at most 32 KiB, so a step
	// is crossed by at most that much.
	ext := len(rec.calls) - 1
	if lo, hi := total/(archiveDeadlineStep+32<<10), total/archiveDeadlineStep; ext < lo || ext > hi || ext < 3 {
		t.Errorf("%d extensions for %d bytes, want %d..%d (and at least 3)", ext, total, lo, hi)
	}
}

// brokenPipeWriter fails every write after the first n bytes with EPIPE,
// as a connection the client closed does.
type brokenPipeWriter struct {
	*httptest.ResponseRecorder
	n int
}

func (b *brokenPipeWriter) Write(p []byte) (int, error) {
	if b.n <= 0 {
		return 0, syscall.EPIPE
	}
	if len(p) > b.n {
		// A connection that breaks mid-write reports the bytes it took
		// and the error together.
		n, _ := b.ResponseRecorder.Write(p[:b.n])
		b.n = 0
		return n, syscall.EPIPE
	}
	b.n -= len(p)
	return b.ResponseRecorder.Write(p)
}

// TestArchiveClientGone: a download the client abandons (the connection
// breaks) aborts like any failure, but is logged at INFO with class
// client_gone, not as a WARN.
func TestArchiveClientGone(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.publishNested("Gone").Artifact.ID
	logs := captureLogs(t, slog.LevelInfo)
	w := &brokenPipeWriter{ResponseRecorder: httptest.NewRecorder(), n: 100}
	if !f.serveArchive(w, &agentA, http.MethodGet, archivePath(id, 1), nil) {
		t.Fatal("not aborted")
	}
	out := logs.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "class="+archiveFailClientGone) || strings.Contains(out, "level=WARN") {
		t.Errorf("log:\n%s", out)
	}
	if f.svc.archiveSlotsFor().total != 0 {
		t.Error("slot not released")
	}
}

func TestArchiveClientGoneClassifier(t *testing.T) {
	live := context.Background()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name  string
		ctx   context.Context
		class string
		err   error
		want  bool
	}{
		{"write, request context ended", gone, archiveFailWrite, errors.New("short write"), true},
		{"write, broken pipe", live, archiveFailWrite, fmt.Errorf("write: %w", syscall.EPIPE), true},
		{"write, connection reset", live, archiveFailWrite, fmt.Errorf("write: %w", syscall.ECONNRESET), true},
		{"write, closed connection", live, archiveFailWrite, net.ErrClosed, true},
		{"write, other error", live, archiveFailWrite, errors.New("disk full"), false},
		{"blob read cancelled by the client", gone, archiveFailBlobRead, fmt.Errorf("read: %w", context.Canceled), true},
		{"blob read failing, client still there", live, archiveFailBlobRead, errors.New("storage unavailable"), false},
		{"blob read failing while the client is gone", gone, archiveFailBlobRead, errors.New("storage unavailable"), false},
		{"size mismatch while the client is gone", gone, archiveFailSizeMatch, errBlobSize, false},
		{"blob open cancelled by the client", gone, archiveFailBlobOpen, fmt.Errorf("open: %w", context.Canceled), true},
		{"blob open failing while the client is gone", gone, archiveFailBlobOpen, errors.New("storage unavailable"), false},
		{"blob open cancelled, client still there", live, archiveFailBlobOpen, context.Canceled, false},
		{"entry header while the client is gone", gone, archiveFailEntryHeader, errors.New("bad header"), false},
		{"time limit", live, archiveFailTimeLimit, errArchiveTimeLimit, false},
	} {
		if got := archiveClientGone(c.ctx, c.class, c.err); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
