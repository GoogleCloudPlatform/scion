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

package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// catalogFakeRelease is one release served by catalogFake.
type catalogFakeRelease struct {
	Tag       string
	Published time.Time
	Draft     bool
	NoBinary  bool
	NoSums    bool
	// Sums is the SHA256SUMS body; "" means a good file (with a bogus
	// self-entry).
	Sums        string
	SumsUpdated time.Time
}

// catalogFake serves GitHub's release list API (pages, Link, ETag/304) and
// release downloads (github.com redirecting to objects.githubusercontent.com)
// through an http.RoundTripper over httptest recorders, so the lister's real
// URLs, asset pinning and redirect policy are exercised.
type catalogFake struct {
	t *testing.T

	mu       sync.Mutex
	releases []catalogFakeRelease // list order, newest created first
	perPage  int
	failList bool
	// hangSums holds tags whose SHA256SUMS download hangs until the
	// request's context ends (a blackholed download host).
	hangSums map[string]bool
	// started, when set, receives once per list request; block, when set,
	// holds list requests until closed.
	started chan struct{}
	block   chan struct{}

	listOK      int
	list304     int
	pages       []int
	ifNoneMatch []string
	sumsCalls   map[string]int
	requests    []string
}

func newCatalogFake(t *testing.T, rels ...catalogFakeRelease) *catalogFake {
	return &catalogFake{t: t, releases: rels, perPage: 100, sumsCalls: map[string]int{}, hangSums: map[string]bool{}}
}

// catalogDay is 2026-10-<day> 04:00 UTC.
func catalogDay(day int) time.Time {
	return time.Date(2026, 10, day, 4, 0, 0, 0, time.UTC)
}

func catalogGood(tag string, published time.Time) catalogFakeRelease {
	return catalogFakeRelease{Tag: tag, Published: published, SumsUpdated: published}
}

func catalogGoodSums() string {
	h := strings.Repeat("ab", 32)
	return fmt.Sprintf("%s  SHA256SUMS\n%s  %s\n%s  scion-darwin-arm64.tar.gz\n", emptySHA256, h, BinaryAssetName(), h)
}

func (f *catalogFake) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.String())
	started, block := f.started, f.block
	hang := r.URL.Host == "objects.githubusercontent.com" &&
		f.hangSums[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"+testRepo+"/releases/download/"), "/"+ChecksumsAssetName)]
	f.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	isList := r.URL.Host == "api.github.com" &&
		(r.URL.Path == "/repos/"+testRepo+"/releases" || r.URL.Path == "/repositories/1/releases")
	if isList {
		if started != nil {
			started <- struct{}{}
		}
		if block != nil {
			<-block
		}
	}
	rec := httptest.NewRecorder()
	f.mu.Lock()
	if isList {
		f.serveList(rec, r)
	} else {
		f.serveDownload(rec, r)
	}
	f.mu.Unlock()
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func (f *catalogFake) serveList(w http.ResponseWriter, r *http.Request) {
	if f.failList {
		http.Error(w, "boom", http.StatusBadGateway)
		return
	}
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	f.pages = append(f.pages, page)
	lo := (page - 1) * f.perPage
	hi := min(lo+f.perPage, len(f.releases))
	lo = min(lo, hi)
	var out []map[string]any
	for _, rel := range f.releases[lo:hi] {
		dl := "https://github.com/" + testRepo + "/releases/download/" + rel.Tag + "/"
		var assets []map[string]any
		if !rel.NoBinary {
			assets = append(assets, map[string]any{"name": BinaryAssetName(), "browser_download_url": dl + BinaryAssetName(), "size": 100, "updated_at": rel.Published})
		}
		if !rel.NoSums {
			assets = append(assets, map[string]any{"name": ChecksumsAssetName, "browser_download_url": dl + ChecksumsAssetName, "size": 10, "updated_at": rel.SumsUpdated})
		}
		out = append(out, map[string]any{
			"tag_name": rel.Tag, "draft": rel.Draft, "published_at": rel.Published,
			"html_url": "https://github.com/" + testRepo + "/releases/tag/" + rel.Tag, "assets": assets,
		})
	}
	body, _ := json.Marshal(out)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if hi < len(f.releases) {
		next := fmt.Sprintf("https://api.github.com/repositories/1/releases?per_page=%d&page=%d", f.perPage, page+1)
		w.Header().Add("Link", fmt.Sprintf(`<%s>; rel="next", <https://api.github.com/repositories/1/releases?page=99>; rel="last"`, next))
	}
	inm := r.Header.Get("If-None-Match")
	f.ifNoneMatch = append(f.ifNoneMatch, inm)
	w.Header().Set("ETag", etag)
	if inm == etag {
		f.list304++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f.listOK++
	_, _ = w.Write(body)
}

func (f *catalogFake) serveDownload(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + testRepo + "/releases/download/"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, "/"+ChecksumsAssetName) {
		f.t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
		return
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/"+ChecksumsAssetName)
	switch r.URL.Host {
	case "github.com":
		http.Redirect(w, r, "https://objects.githubusercontent.com"+r.URL.Path, http.StatusFound)
	case "objects.githubusercontent.com":
		for _, rel := range f.releases {
			if rel.Tag == tag && !rel.NoSums {
				f.sumsCalls[tag]++
				body := rel.Sums
				if body == "" {
					body = catalogGoodSums()
				}
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	default:
		f.t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
	}
}

func (f *catalogFake) counts() (ok, notModified int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listOK, f.list304
}

func (f *catalogFake) listCalls() int {
	ok, nm := f.counts()
	return ok + nm
}

func (f *catalogFake) sums(tag string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sumsCalls[tag]
}

func (f *catalogFake) totalSums() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.sumsCalls {
		n += c
	}
	return n
}

func (f *catalogFake) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// catalogClock is a settable clock.
type catalogClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *catalogClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *catalogClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// catalogMemStore is an in-memory CatalogStore.
// Like a database client, it fails with ctx's error once ctx has ended.
type catalogMemStore struct {
	mu       sync.Mutex
	data     []byte
	saves    int
	loads    int
	loadErrs int  // the next loadErrs loads fail
	block    bool // loads hang until their ctx ends (a stalled database)
	// hang, when set, makes loads wait for it to close and then return
	// data, ignoring ctx throughout (a store that breaks the CatalogStore
	// contract).
	hang chan struct{}
}

func (s *catalogMemStore) LoadReleaseCatalog(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	s.loads++
	block, hang := s.block, s.hang
	s.mu.Unlock()
	if hang != nil {
		<-hang
		s.mu.Lock()
		defer s.mu.Unlock()
		return slices.Clone(s.data), nil
	}
	if block {
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErrs > 0 {
		s.loadErrs--
		return nil, errors.New("database is locked")
	}
	return slices.Clone(s.data), nil
}

func (s *catalogMemStore) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func (s *catalogMemStore) SaveReleaseCatalog(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = slices.Clone(data)
	s.saves++
	return nil
}

func newTestCatalog(f *catalogFake, clock *catalogClock, mod func(*CatalogOptions)) *Catalog {
	opts := CatalogOptions{
		Repo:   testRepo,
		Lister: &GitHubReleases{Repo: testRepo, Client: &http.Client{Transport: f}},
		Now:    clock.Now,
	}
	if mod != nil {
		mod(&opts)
	}
	return NewCatalog(opts)
}

func newCatalogClock() *catalogClock {
	return &catalogClock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
}

func mustGet(t *testing.T, c *Catalog, refresh bool) Snapshot {
	t.Helper()
	snap, err := c.Get(context.Background(), refresh)
	if err != nil {
		t.Fatalf("Get(refresh=%v): %v", refresh, err)
	}
	return snap
}

func catalogTags(ch CatalogChannel) []string {
	var tags []string
	for _, r := range ch.Releases {
		tags = append(tags, r.Tag)
	}
	return tags
}

func TestCatalog_Classification(t *testing.T) {
	asset := BinaryAssetName()
	good := strings.Repeat("cd", 32)
	f := newCatalogFake(t,
		catalogGood("nightly-20261024", catalogDay(24)),
		catalogFakeRelease{Tag: "nightly-20261025", Published: catalogDay(25), Draft: true},
		catalogGood("v0.3.0-preview.4", catalogDay(23)),
		catalogFakeRelease{Tag: "v0.3.0-preview.3", Published: catalogDay(22), NoSums: true},
		catalogFakeRelease{Tag: "v0.3.0-preview.2", Published: catalogDay(21), SumsUpdated: catalogDay(21),
			Sums: good + "  scion-darwin-arm64.tar.gz\n" + "not a checksum line for " + asset + "\n"},
		catalogFakeRelease{Tag: "nightly-20261021", Published: catalogDay(21), SumsUpdated: catalogDay(21),
			Sums: emptySHA256 + "  " + asset + "\n"},
		catalogFakeRelease{Tag: "nightly-20261020", Published: catalogDay(20), SumsUpdated: catalogDay(20),
			Sums: emptySHA256 + "  SHA256SUMS\n" + good + " *" + asset + "\n"},
		catalogFakeRelease{Tag: "nightly-20261019", Published: catalogDay(19), NoBinary: true},
		catalogGood("nightly-bogus", catalogDay(18)),
		catalogGood("some-other-tag", catalogDay(18)),
	)
	// Orphan tags (git tags without a release, here nightly-20261026 and
	// v0.3.0-preview.5) are excluded by construction: the catalog lists
	// releases, never tags. The fake fails the test on any /tags request.
	snap := mustGet(t, newTestCatalog(f, newCatalogClock(), nil), false)

	nightly := snap.Channel("nightly")
	if got, want := catalogTags(nightly), []string{"nightly-20261024", "nightly-20261021", "nightly-20261020", "nightly-20261019"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("nightly tags = %v, want %v (draft, malformed tags and orphan tags excluded)", got, want)
	}
	if nightly.Latest != "nightly-20261024" {
		t.Errorf("nightly latest = %q", nightly.Latest)
	}
	cases := map[string]struct {
		installable bool
		reason      string
	}{
		"nightly-20261024": {true, ""},
		"nightly-20261021": {false, ReasonBadChecksum},          // empty-file hash for the asset
		"nightly-20261020": {true, ""},                          // bogus self-entry ignored
		"nightly-20261019": {false, ReasonNoBinary(asset)},      // no tarball
		"v0.3.0-preview.4": {true, ""},                          // good
		"v0.3.0-preview.3": {false, ReasonNoChecksums},          // no SHA256SUMS
		"v0.3.0-preview.2": {false, ReasonNoChecksumFor(asset)}, // no line for the asset
	}
	for tag, want := range cases {
		r, ok := snap.Lookup(tag)
		if !ok {
			t.Errorf("Lookup(%s) missing", tag)
			continue
		}
		if r.Installable != want.installable || r.Reason != want.reason {
			t.Errorf("%s: installable=%v reason=%q, want %v %q", tag, r.Installable, r.Reason, want.installable, want.reason)
		}
		if r.Source != SourceGitHub || r.Local || r.NotesURL != "https://github.com/"+testRepo+"/releases/tag/"+tag {
			t.Errorf("%s: source=%q local=%v notes=%q", tag, r.Source, r.Local, r.NotesURL)
		}
	}
	for _, tag := range []string{"nightly-20261025", "nightly-20261026", "v0.3.0-preview.5", "nightly-bogus", "some-other-tag"} {
		if _, ok := snap.Lookup(tag); ok {
			t.Errorf("Lookup(%s) found; drafts, orphan tags and malformed tags must not appear", tag)
		}
	}
	if l, ok := snap.Latest("preview"); !ok || l.Tag != "v0.3.0-preview.4" {
		t.Errorf("preview latest = %+v %v", l, ok)
	}

	stable := snap.Channel("stable")
	if stable.Latest != "" || stable.Releases == nil || len(stable.Releases) != 0 {
		t.Errorf("stable = %+v, want empty (non-nil) releases and no latest", stable)
	}
	if _, ok := snap.Latest("stable"); ok {
		t.Error("Latest(stable) found on an empty channel")
	}
	if got := snap.Channel("bogus"); got.Releases == nil || len(got.Releases) != 0 {
		t.Errorf("unknown channel = %+v", got)
	}
	// The Snapshot is not the wire DTO: hu-p2-hub builds the D5 response
	// (mapping Latest "" to null and a zero PublishedAt to omitted).

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.requests {
		if strings.Contains(u, "/tags") {
			t.Errorf("catalog requested the tag list: %s", u)
		}
	}
	if f.sumsCalls["nightly-20261025"] != 0 || f.sumsCalls["nightly-bogus"] != 0 {
		t.Errorf("SHA256SUMS of a skipped release downloaded: %v", f.sumsCalls)
	}
}

func TestCatalog_ChannelLimits(t *testing.T) {
	var rels []catalogFakeRelease
	// Newest first: 5 broken nightlies, then 14 good ones.
	for i := range 5 {
		rels = append(rels, catalogFakeRelease{Tag: fmt.Sprintf("nightly-202611%02d", 30-i), Published: time.Date(2026, 11, 30-i, 0, 0, 0, 0, time.UTC), NoSums: true})
	}
	for i := range 14 {
		d := time.Date(2026, 11, 20-i, 0, 0, 0, 0, time.UTC)
		rels = append(rels, catalogGood(fmt.Sprintf("nightly-202611%02d", 20-i), d))
	}
	f := newCatalogFake(t, rels...)
	snap := mustGet(t, newTestCatalog(f, newCatalogClock(), nil), false)

	var installable, other []string
	for _, r := range snap.Channel("nightly").Releases {
		if r.Installable {
			installable = append(installable, r.Tag)
		} else {
			other = append(other, r.Tag)
		}
	}
	if len(installable) != DefaultCatalogPerChannel {
		t.Errorf("installable = %d %v, want %d", len(installable), installable, DefaultCatalogPerChannel)
	}
	if want := []string{"nightly-20261130", "nightly-20261129", "nightly-20261128"}; !reflect.DeepEqual(other, want) {
		t.Errorf("non-installable = %v, want the newest 3 %v", other, want)
	}
	if got := snap.Channel("nightly").Latest; got != "nightly-20261120" {
		t.Errorf("latest = %q", got)
	}
	// Releases past the 10th installable are not classified: no download.
	if n := f.totalSums(); n != DefaultCatalogPerChannel {
		t.Errorf("SHA256SUMS downloads = %d, want %d", n, DefaultCatalogPerChannel)
	}
	if f.sums("nightly-20261107") != 0 {
		t.Error("SHA256SUMS of the 11th installable release downloaded")
	}
}

func TestCatalog_PaginationStop(t *testing.T) {
	t.Run("stops when every channel is full", func(t *testing.T) {
		f := newCatalogFake(t,
			catalogGood("nightly-20261024", catalogDay(24)),
			catalogGood("nightly-20261023", catalogDay(23)),
			catalogGood("v0.3.0-preview.4", catalogDay(22)),
			catalogGood("v0.3.0", catalogDay(21)),
			catalogGood("nightly-20261020", catalogDay(20)),
			catalogGood("nightly-20261019", catalogDay(19)),
		)
		f.perPage = 2
		c := newTestCatalog(f, newCatalogClock(), func(o *CatalogOptions) { o.PerChannel = 1 })
		snap := mustGet(t, c, false)
		if !reflect.DeepEqual(f.pages, []int{1, 2}) {
			t.Errorf("pages = %v, want [1 2]", f.pages)
		}
		if snap.Channel("stable").Latest != "v0.3.0" {
			t.Errorf("stable = %+v", snap.Channel("stable"))
		}
		// PerChannel=1: the second nightly on page 1 is never classified.
		if f.sums("nightly-20261023") != 0 {
			t.Error("SHA256SUMS of a release past N downloaded")
		}
	})
	t.Run("stops after 3 pages", func(t *testing.T) {
		var rels []catalogFakeRelease
		for i := range 10 {
			rels = append(rels, catalogGood(fmt.Sprintf("nightly-202610%02d", 25-i), catalogDay(25-i)))
		}
		f := newCatalogFake(t, rels...)
		f.perPage = 2 // 5 pages; stable never fills
		snap := mustGet(t, newTestCatalog(f, newCatalogClock(), nil), false)
		if !reflect.DeepEqual(f.pages, []int{1, 2, 3}) {
			t.Errorf("pages = %v, want [1 2 3]", f.pages)
		}
		if n := len(snap.Channel("nightly").Releases); n != 6 {
			t.Errorf("nightly releases = %d, want the 6 from 3 pages", n)
		}
	})
	t.Run("stops on the last page", func(t *testing.T) {
		f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
		mustGet(t, newTestCatalog(f, newCatalogClock(), nil), false)
		if !reflect.DeepEqual(f.pages, []int{1}) {
			t.Errorf("pages = %v, want [1]", f.pages)
		}
	})
}

func TestCatalog_ETag304DoesNotCount(t *testing.T) {
	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)), catalogGood("v0.3.0-preview.4", catalogDay(23)))
	clock := newCatalogClock()
	c := newTestCatalog(f, clock, nil)
	first := mustGet(t, c, false)

	clock.Advance(DefaultCatalogTTL)
	second := mustGet(t, c, false)
	ok, nm := f.counts()
	if ok != 1 || nm != 1 {
		t.Fatalf("200s=%d 304s=%d, want 1 and 1", ok, nm)
	}
	f.mu.Lock()
	inm := slices.Clone(f.ifNoneMatch)
	f.mu.Unlock()
	if len(inm) != 2 || inm[0] != "" || inm[1] == "" {
		t.Errorf("If-None-Match sent = %q, want none then the stored ETag", inm)
	}
	if !reflect.DeepEqual(first.Channels, second.Channels) {
		t.Errorf("304 changed the catalog:\n%+v\n%+v", first.Channels, second.Channels)
	}
	if !second.FetchedAt.Equal(clock.Now()) || second.Stale {
		t.Errorf("after 304: fetchedAt=%v stale=%v, want %v and fresh", second.FetchedAt, second.Stale, clock.Now())
	}
	if n := f.totalSums(); n != 2 {
		t.Errorf("SHA256SUMS downloads = %d, want 2 (none on 304)", n)
	}
	// The 304 restarts the TTL: no request until it runs out again.
	clock.Advance(DefaultCatalogTTL - time.Second)
	mustGet(t, c, false)
	if n := f.listCalls(); n != 2 {
		t.Errorf("list calls = %d, want 2", n)
	}
}

func TestCatalog_SumsCacheKeyedByTagAndUpdatedAt(t *testing.T) {
	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)), catalogGood("nightly-20261023", catalogDay(23)))
	clock := newCatalogClock()
	c := newTestCatalog(f, clock, nil)
	mustGet(t, c, false)

	// A new release changes the list (200, new ETag); the cached verdicts
	// of the two old releases are reused.
	f.set(func() {
		f.releases = append([]catalogFakeRelease{catalogGood("nightly-20261025", catalogDay(25))}, f.releases...)
	})
	clock.Advance(DefaultCatalogTTL)
	mustGet(t, c, false)
	if ok, _ := f.counts(); ok != 2 {
		t.Fatalf("list 200s = %d, want 2", ok)
	}
	for tag, want := range map[string]int{"nightly-20261025": 1, "nightly-20261024": 1, "nightly-20261023": 1} {
		if got := f.sums(tag); got != want {
			t.Errorf("SHA256SUMS downloads of %s = %d, want %d", tag, got, want)
		}
	}

	// Re-uploading SHA256SUMS (new updated_at) invalidates that tag only.
	f.set(func() {
		f.releases[1].SumsUpdated = catalogDay(26)
		f.releases[1].Sums = emptySHA256 + "  " + BinaryAssetName() + "\n"
	})
	clock.Advance(DefaultCatalogTTL)
	snap := mustGet(t, c, false)
	if got := f.sums("nightly-20261024"); got != 2 {
		t.Errorf("downloads of the re-uploaded tag = %d, want 2", got)
	}
	if got := f.sums("nightly-20261023") + f.sums("nightly-20261025"); got != 2 {
		t.Errorf("downloads of unchanged tags = %d, want 2", got)
	}
	if r, _ := snap.Lookup("nightly-20261024"); r.Installable || r.Reason != ReasonBadChecksum {
		t.Errorf("re-uploaded release = %+v, want the new verdict", r)
	}
}

func TestCatalog_StaleOnError(t *testing.T) {
	t.Run("serves the last good catalog", func(t *testing.T) {
		f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
		clock := newCatalogClock()
		c := newTestCatalog(f, clock, nil)
		good := mustGet(t, c, false)
		fetchedAt := clock.Now()

		f.set(func() { f.failList = true })
		clock.Advance(DefaultCatalogTTL)
		stale := mustGet(t, c, false)
		if !stale.Stale || stale.Error == "" || !stale.FetchedAt.Equal(fetchedAt) {
			t.Errorf("stale=%v error=%q fetchedAt=%v, want stale as of %v", stale.Stale, stale.Error, stale.FetchedAt, fetchedAt)
		}
		if !reflect.DeepEqual(stale.Channels, good.Channels) {
			t.Errorf("stale catalog differs from the last good one")
		}

		// Backoff: no new attempt within the refresh interval, even on
		// a manual refresh.
		calls := len(f.requests)
		clock.Advance(DefaultCatalogRefreshInterval - time.Second)
		if s := mustGet(t, c, true); !s.Stale {
			t.Error("not stale during backoff")
		}
		if len(f.requests) != calls {
			t.Errorf("fetch attempted during backoff")
		}

		f.set(func() { f.failList = false })
		clock.Advance(time.Second)
		fresh := mustGet(t, c, false)
		if fresh.Stale || fresh.Error != "" || !fresh.FetchedAt.Equal(clock.Now()) {
			t.Errorf("after recovery: %+v", fresh)
		}
	})
	t.Run("errors without any catalog", func(t *testing.T) {
		f := newCatalogFake(t)
		f.failList = true
		_, err := newTestCatalog(f, newCatalogClock(), nil).Get(context.Background(), false)
		if err == nil || !strings.Contains(err.Error(), "502") {
			t.Errorf("err = %v, want the fetch error", err)
		}
	})
}

func TestCatalog_RefreshThrottle(t *testing.T) {
	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
	clock := newCatalogClock()
	c := newTestCatalog(f, clock, nil)
	mustGet(t, c, false)

	mustGet(t, c, false)
	mustGet(t, c, true)
	clock.Advance(DefaultCatalogRefreshInterval - time.Second)
	mustGet(t, c, true)
	if n := f.listCalls(); n != 1 {
		t.Fatalf("list calls within the refresh interval = %d, want 1", n)
	}
	clock.Advance(time.Second)
	mustGet(t, c, true)
	if n := f.listCalls(); n != 2 {
		t.Fatalf("list calls after the refresh interval = %d, want 2", n)
	}
	mustGet(t, c, true)
	if n := f.listCalls(); n != 2 {
		t.Fatalf("second manual refresh not throttled: %d calls", n)
	}
}

func TestCatalog_Singleflight(t *testing.T) {
	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
	f.started = make(chan struct{}, 10)
	f.block = make(chan struct{})
	c := newTestCatalog(f, newCatalogClock(), nil)
	waiting := make(chan struct{}, 10)
	c.onWait = func() { waiting <- struct{}{} }

	const n = 10
	type result struct {
		snap Snapshot
		err  error
	}
	results := make(chan result, n)
	get := func(refresh bool) {
		s, err := c.Get(context.Background(), refresh)
		results <- result{s, err}
	}
	go get(true)
	<-f.started // the first fetch is in flight and blocked
	<-waiting
	// Cold callers (refresh or not) join the fetch in flight instead of
	// failing with "unavailable".
	for i := range n - 1 {
		go get(i%2 == 0)
	}
	for range n - 1 {
		<-waiting
	}
	select {
	case r := <-results:
		t.Fatalf("a caller returned before the shared fetch finished: %+v", r)
	default:
	}
	close(f.block)
	for range n {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent Get: %v", r.err)
		}
		if r.snap.Channel("nightly").Latest != "nightly-20261024" {
			t.Errorf("concurrent Get snapshot = %+v", r.snap)
		}
	}
	if calls := f.listCalls(); calls != 1 {
		t.Errorf("list calls = %d, want 1", calls)
	}
	if s := f.sums("nightly-20261024"); s != 1 {
		t.Errorf("SHA256SUMS downloads = %d, want 1", s)
	}
}

func TestCatalog_CancelledCallerStopsWaiting(t *testing.T) {
	t.Run("cold: ctx error, and the fetch still completes", func(t *testing.T) {
		f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
		f.started = make(chan struct{}, 1)
		f.block = make(chan struct{})
		c := newTestCatalog(f, newCatalogClock(), nil)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := c.Get(ctx, false)
			done <- err
		}()
		<-f.started
		cancel()
		// Get returns while the fetch is still blocked.
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cold Get: err = %v, want context.Canceled", err)
		}
		close(f.block)
		// The detached fetch finishes; the next caller gets its result
		// without another request.
		snap := mustGet(t, c, false)
		if snap.Channel("nightly").Latest != "nightly-20261024" {
			t.Errorf("snapshot after the detached fetch = %+v", snap)
		}
		if n := f.listCalls(); n != 1 {
			t.Errorf("list calls = %d, want 1", n)
		}
	})
	t.Run("warm: the cached catalog", func(t *testing.T) {
		f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
		clock := newCatalogClock()
		c := newTestCatalog(f, clock, nil)
		mustGet(t, c, false)

		f.set(func() {
			f.started = make(chan struct{}, 1)
			f.block = make(chan struct{})
		})
		defer close(f.block)
		clock.Advance(DefaultCatalogRefreshInterval)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan catalogGetResult, 1)
		go func() {
			s, err := c.Get(ctx, true)
			done <- catalogGetResult{s, err}
		}()
		<-f.started
		cancel()
		r := <-done
		if r.err != nil || r.snap.Channel("nightly").Latest != "nightly-20261024" {
			t.Errorf("cancelled warm Get = %+v, %v; want the cached catalog", r.snap, r.err)
		}
	})
}

type catalogGetResult struct {
	snap Snapshot
	err  error
}

func TestCatalog_BlackholedChecksumsHost(t *testing.T) {
	f := newCatalogFake(t,
		catalogGood("nightly-20261024", catalogDay(24)),
		catalogGood("nightly-20261023", catalogDay(23)),
		catalogGood("nightly-20261022", catalogDay(22)),
	)
	f.hangSums["nightly-20261023"] = true
	clock := newCatalogClock()
	// The budget starts at the first download; nightly-20261024's in-process
	// download has 1.5 s, and the hang on nightly-20261023 lasts until the
	// budget runs out.
	c := newTestCatalog(f, clock, func(o *CatalogOptions) { o.ChecksumsBudget = 1500 * time.Millisecond })

	// A cold hub still gets a catalog when the list succeeds.
	snap := mustGet(t, c, false)
	if snap.Stale {
		t.Errorf("catalog stale after a SUMS timeout: %+v", snap)
	}
	if r, _ := snap.Lookup("nightly-20261024"); !r.Installable {
		t.Errorf("release classified before the hang = %+v, want installable", r)
	}
	for _, tag := range []string{"nightly-20261023", "nightly-20261022"} {
		if r, _ := snap.Lookup(tag); r.Installable || r.Reason != ReasonChecksumsUnavailable {
			t.Errorf("%s = %+v, want %q", tag, r, ReasonChecksumsUnavailable)
		}
	}
	if got := f.sums("nightly-20261022"); got != 0 {
		t.Errorf("SHA256SUMS of nightly-20261022 downloaded after the budget ran out: %d", got)
	}
	if l, _ := snap.Latest("nightly"); l.Tag != "nightly-20261024" {
		t.Errorf("latest = %q", l.Tag)
	}

	// The host recovers: the unavailable verdicts were not cached, the good
	// one was.
	f.set(func() { delete(f.hangSums, "nightly-20261023") })
	clock.Advance(DefaultCatalogTTL)
	snap = mustGet(t, c, false)
	for _, tag := range []string{"nightly-20261024", "nightly-20261023", "nightly-20261022"} {
		if r, _ := snap.Lookup(tag); !r.Installable {
			t.Errorf("%s after recovery = %+v", tag, r)
		}
	}
	if got := f.sums("nightly-20261024"); got != 1 {
		t.Errorf("downloads of the cached verdict = %d, want 1", got)
	}
}

func TestCatalog_PersistedLoadRetriedAfterStoreError(t *testing.T) {
	rels := []catalogFakeRelease{catalogGood("nightly-20261024", catalogDay(24))}
	clock := newCatalogClock()
	store := &catalogMemStore{}
	a := newTestCatalog(newCatalogFake(t, rels...), clock, func(o *CatalogOptions) { o.Store = store })
	warm := mustGet(t, a, false)

	// A restarted hub whose first store read fails, while GitHub is down.
	clock.Advance(DefaultCatalogTTL)
	f := newCatalogFake(t, rels...)
	f.failList = true
	store.loadErrs = 1
	b := newTestCatalog(f, clock, func(o *CatalogOptions) { o.Store = store })
	if _, err := b.Get(context.Background(), false); err == nil {
		t.Fatal("first Get succeeded with no catalog available")
	}
	// Within the backoff the store is not read again.
	if _, err := b.Get(context.Background(), false); err == nil {
		t.Fatal("Get during the backoff succeeded with no catalog available")
	}
	if n := store.loadCount(); n != 2 { // a's load and b's failed one
		t.Fatalf("store loads = %d, want 2 (no retry within the backoff)", n)
	}
	// After the backoff the next Get reads the persisted copy and serves it
	// (stale, as GitHub is still down).
	clock.Advance(DefaultCatalogRefreshInterval)
	snap := mustGet(t, b, false)
	if !snap.Stale || !reflect.DeepEqual(snap.Channels, warm.Channels) || !snap.FetchedAt.Equal(warm.FetchedAt) {
		t.Errorf("after a transient store error: %+v, want the warm copy (stale)", snap)
	}

	// A cancelled first caller does not lose the persisted copy either: the
	// read is detached from its ctx (the store fails on an ended ctx), and
	// it starts no fetch.
	f2 := newCatalogFake(t, rels...)
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	cl := newTestCatalog(f2, clock, func(o *CatalogOptions) {
		o.Store = store
		o.TTL = 4 * DefaultCatalogTTL
	})
	if s, err := cl.Get(cctx, false); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled first Get = %+v, %v; want the persisted copy or context.Canceled", s, err)
	}
	if s := mustGet(t, cl, false); !s.FetchedAt.Equal(warm.FetchedAt) {
		t.Errorf("Get after a cancelled first caller = %+v, want the persisted copy", s)
	}
	if len(f2.requests) != 0 {
		t.Errorf("requests with a warm persisted copy: %v", f2.requests)
	}
}

func TestCatalog_PersistedRoundTrip(t *testing.T) {
	rels := []catalogFakeRelease{
		catalogGood("nightly-20261024", catalogDay(24)),
		catalogGood("v0.3.0-preview.4", catalogDay(23)),
		{Tag: "v0.3.0-preview.3", Published: catalogDay(22), NoSums: true},
	}
	store := &catalogMemStore{}
	clock := newCatalogClock()
	f1 := newCatalogFake(t, rels...)
	a := newTestCatalog(f1, clock, func(o *CatalogOptions) { o.Store = store })
	snapA := mustGet(t, a, false)
	if store.saves != 1 || len(store.data) == 0 {
		t.Fatalf("store saves = %d, %d bytes", store.saves, len(store.data))
	}

	// A new catalog (a restarted hub) serves the persisted copy without
	// any request while it is within the TTL.
	clock.Advance(time.Minute)
	f2 := newCatalogFake(t, rels...)
	b := newTestCatalog(f2, clock, func(o *CatalogOptions) { o.Store = store })
	snapB := mustGet(t, b, false)
	if len(f2.requests) != 0 {
		t.Errorf("requests after a warm start = %v", f2.requests)
	}
	if !reflect.DeepEqual(snapA, snapB) {
		t.Errorf("persisted round trip differs:\n%+v\n%+v", snapA, snapB)
	}

	// After the TTL it revalidates with the persisted ETag (304) and needs
	// no SHA256SUMS download: the verdicts are persisted too.
	clock.Advance(DefaultCatalogTTL)
	mustGet(t, b, false)
	if ok, nm := f2.counts(); ok != 0 || nm != 1 {
		t.Errorf("after TTL: 200s=%d 304s=%d, want 0 and 1", ok, nm)
	}
	if n := f2.totalSums(); n != 0 {
		t.Errorf("SHA256SUMS downloads after a warm start = %d, want 0", n)
	}
	if store.saves != 2 {
		t.Errorf("store saves = %d, want 2", store.saves)
	}

	t.Run("ignores a copy for another repo or format", func(t *testing.T) {
		for name, data := range map[string][]byte{
			"other repo": []byte(strings.Replace(string(store.data), `"repo":"`+testRepo+`"`, `"repo":"someone/else"`, 1)),
			"version":    []byte(strings.Replace(string(store.data), `"version":1`, `"version":99`, 1)),
			"garbage":    []byte("{not json"),
		} {
			f := newCatalogFake(t, rels...)
			c := newTestCatalog(f, clock, func(o *CatalogOptions) { o.Store = &catalogMemStore{data: data} })
			mustGet(t, c, false)
			if ok, _ := f.counts(); ok != 1 {
				t.Errorf("%s: list 200s = %d, want a fresh fetch", name, ok)
			}
		}
	})
}

func TestCatalog_NightlyOrderingByDate(t *testing.T) {
	same := catalogDay(23)
	// Listed in an order that matches neither date nor string order.
	f := newCatalogFake(t,
		catalogGood("nightly-20261023.2", same),
		catalogGood("nightly-20261009", catalogDay(9)),
		catalogGood("nightly-20261023.10", same),
		catalogGood("nightly-20261023", same),
		catalogGood("nightly-20261101", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)),
		catalogGood("nightly-20261023.1", same),
		catalogGood("v0.3.0-preview.9", same),
		catalogGood("v0.3.0-preview.10", same),
	)
	snap := mustGet(t, newTestCatalog(f, newCatalogClock(), nil), false)
	want := []string{"nightly-20261101", "nightly-20261023.10", "nightly-20261023.2", "nightly-20261023.1", "nightly-20261023", "nightly-20261009"}
	if got := catalogTags(snap.Channel("nightly")); !reflect.DeepEqual(got, want) {
		t.Errorf("nightly order = %v, want %v", got, want)
	}
	if got := catalogTags(snap.Channel("preview")); !reflect.DeepEqual(got, []string{"v0.3.0-preview.10", "v0.3.0-preview.9"}) {
		t.Errorf("preview order = %v", got)
	}
	if l, _ := snap.Latest("nightly"); l.Tag != "nightly-20261101" {
		t.Errorf("latest = %q", l.Tag)
	}

	// Without publish dates (local-only entries) the tag's own date orders.
	rs := []CatalogRelease{{Tag: "nightly-20261009"}, {Tag: "nightly-20261023.1"}, {Tag: "nightly-20261101"}, {Tag: "nightly-20261023"}}
	slices.SortStableFunc(rs, compareNewestFirst)
	var got []string
	for _, r := range rs {
		got = append(got, r.Tag)
	}
	if want := []string{"nightly-20261101", "nightly-20261023.1", "nightly-20261023", "nightly-20261009"}; !reflect.DeepEqual(got, want) {
		t.Errorf("undated order = %v, want %v", got, want)
	}
}

func TestCatalog_LocalRetained(t *testing.T) {
	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)), catalogGood("nightly-20261020", catalogDay(20)))
	local := []InstalledRelease{
		{Tag: "nightly-20261024", Current: true},
		{Tag: "nightly-20260901"}, // deleted from GitHub by nightly retention
		{Tag: "git-abcdef123456"}, // not a release tag
	}
	c := newTestCatalog(f, newCatalogClock(), func(o *CatalogOptions) {
		o.Local = func() []InstalledRelease { return local }
	})
	snap := mustGet(t, c, false)
	nightly := snap.Channel("nightly")
	if got, want := catalogTags(nightly), []string{"nightly-20261024", "nightly-20261020", "nightly-20260901"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("nightly = %v, want %v", got, want)
	}
	if r, _ := snap.Lookup("nightly-20261024"); !r.Local || r.Source != SourceGitHub {
		t.Errorf("listed retained release = %+v", r)
	}
	if r, _ := snap.Lookup("nightly-20261020"); r.Local {
		t.Errorf("non-retained release marked local: %+v", r)
	}
	if r, _ := snap.Lookup("nightly-20260901"); !r.Local || r.Source != SourceLocal || !r.Installable {
		t.Errorf("local-only release = %+v", r)
	}
	if nightly.Latest != "nightly-20261024" {
		t.Errorf("latest = %q", nightly.Latest)
	}

	// The overlay is applied per Get and never persisted into the cache.
	local = nil
	snap = mustGet(t, c, false)
	if _, ok := snap.Lookup("nightly-20260901"); ok {
		t.Error("local-only release kept after it was pruned")
	}
	if r, _ := snap.Lookup("nightly-20261024"); r.Local {
		t.Error("Local flag kept after the copy was pruned")
	}
}

func TestCatalog_ChecksumsDownloadFailureIsRetried(t *testing.T) {
	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
	clock := newCatalogClock()
	// The lister's SHA256SUMS download fails once.
	lister := &flakySumsLister{ReleaseLister: &GitHubReleases{Repo: testRepo, Client: &http.Client{Transport: f}}, fail: 1}
	c := newTestCatalog(f, clock, func(o *CatalogOptions) { o.Lister = lister })
	snap := mustGet(t, c, false)
	if r, _ := snap.Lookup("nightly-20261024"); r.Installable || r.Reason != ReasonChecksumsUnavailable {
		t.Fatalf("after a failed download: %+v", r)
	}
	clock.Advance(DefaultCatalogTTL)
	snap = mustGet(t, c, false)
	if r, _ := snap.Lookup("nightly-20261024"); !r.Installable {
		t.Errorf("failed download was cached: %+v", r)
	}
}

type flakySumsLister struct {
	ReleaseLister
	fail int
}

func (l *flakySumsLister) FetchChecksums(ctx context.Context, tag string, a ReleaseAsset) ([]byte, error) {
	if l.fail > 0 {
		l.fail--
		return nil, errors.New("connection reset")
	}
	return l.ReleaseLister.FetchChecksums(ctx, tag, a)
}

// getWithin runs c.Get and fails the test when it does not return within d.
func getWithin(t *testing.T, c *Catalog, d time.Duration) (Snapshot, error) {
	t.Helper()
	type res struct {
		s   Snapshot
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := c.Get(context.Background(), false)
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(d):
		t.Fatalf("Get did not return within %v", d)
		return Snapshot{}, nil
	}
}

func TestCatalog_StalledStoreIsBounded(t *testing.T) {
	const bound = 10 * time.Second // far above loadTimeout; only catches a hang
	t.Run("GitHub up: a fresh catalog", func(t *testing.T) {
		f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
		store := &catalogMemStore{block: true}
		c := newTestCatalog(f, newCatalogClock(), func(o *CatalogOptions) { o.Store = store })
		c.loadTimeout = 50 * time.Millisecond
		snap, err := getWithin(t, c, bound)
		if err != nil || snap.Stale || snap.Channel("nightly").Latest != "nightly-20261024" {
			t.Fatalf("Get with a stalled store = %+v, %v", snap, err)
		}
		// With a fetched catalog the store is never read again.
		for range 3 {
			if _, err := getWithin(t, c, bound); err != nil {
				t.Fatal(err)
			}
		}
		if n := store.loadCount(); n != 1 {
			t.Errorf("store loads = %d, want 1", n)
		}
	})
	t.Run("GitHub down: an error, then backoff", func(t *testing.T) {
		f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
		f.failList = true
		clock := newCatalogClock()
		store := &catalogMemStore{block: true}
		c := newTestCatalog(f, clock, func(o *CatalogOptions) { o.Store = store })
		c.loadTimeout = 50 * time.Millisecond
		if _, err := getWithin(t, c, bound); err == nil {
			t.Fatal("Get succeeded with no catalog anywhere")
		}
		// Later Gets within the backoff neither read the store nor block.
		for range 3 {
			if _, err := getWithin(t, c, bound); err == nil {
				t.Fatal("Get succeeded with no catalog anywhere")
			}
		}
		if n := store.loadCount(); n != 1 {
			t.Errorf("store loads within the backoff = %d, want 1", n)
		}
		// After the backoff the store is retried once (now answering
		// "nothing stored") and GitHub, recovered, is fetched.
		store.mu.Lock()
		store.block = false
		store.mu.Unlock()
		f.set(func() { f.failList = false })
		clock.Advance(DefaultCatalogRefreshInterval)
		snap, err := getWithin(t, c, bound)
		if err != nil || snap.Stale {
			t.Fatalf("after recovery: %+v, %v", snap, err)
		}
		if n := store.loadCount(); n != 2 {
			t.Errorf("store loads = %d, want 2", n)
		}
	})
}

func TestCatalog_CtxIgnoringStoreIsAbandoned(t *testing.T) {
	const bound = 10 * time.Second // far above loadWait; only catches a hang
	clock := newCatalogClock()

	// What the stuck read will finally return: a persisted catalog with a
	// different latest nightly.
	other := &catalogMemStore{}
	mustGet(t, newTestCatalog(newCatalogFake(t, catalogGood("nightly-20261030", catalogDay(30))), clock,
		func(o *CatalogOptions) { o.Store = other }), false)

	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
	store := &catalogMemStore{hang: make(chan struct{})}
	c := newTestCatalog(f, clock, func(o *CatalogOptions) { o.Store = store })
	c.loadTimeout = 50 * time.Millisecond
	var joinsMu sync.Mutex
	joins := 0
	c.onLoadWait = func() {
		joinsMu.Lock()
		joins++
		joinsMu.Unlock()
	}
	loadDone := make(chan struct{}, 1)
	c.onLoadDone = func() { loadDone <- struct{}{} }

	// The first caller gives up on the stuck read and fetches from GitHub.
	snap, err := getWithin(t, c, bound)
	if err != nil || snap.Channel("nightly").Latest != "nightly-20261024" {
		t.Fatalf("Get with a ctx-ignoring store = %+v, %v", snap, err)
	}
	// While the read is still stuck, later callers neither join it nor
	// start another, even after the backoff and the TTL.
	clock.Advance(DefaultCatalogTTL)
	for range 3 {
		if _, err := getWithin(t, c, bound); err != nil {
			t.Fatal(err)
		}
	}
	joinsMu.Lock()
	gotJoins := joins
	joinsMu.Unlock()
	if gotJoins != 1 {
		t.Errorf("callers that waited on the store read = %d, want 1 (only the first)", gotJoins)
	}
	if n := store.loadCount(); n != 1 {
		t.Errorf("store loads = %d, want 1", n)
	}
	if n := f.listCalls(); n != 2 {
		t.Errorf("list calls = %d, want 2 (initial fetch and one after the TTL)", n)
	}

	// The stuck read finally returns another catalog: it is discarded in
	// favour of the fetched one.
	other.mu.Lock()
	persisted := slices.Clone(other.data)
	other.mu.Unlock()
	store.mu.Lock()
	store.data = persisted
	store.mu.Unlock()
	close(store.hang)
	select {
	case <-loadDone:
	case <-time.After(bound):
		t.Fatal("the stuck store read did not finish after it was released")
	}
	snap, err = getWithin(t, c, bound)
	if err != nil || snap.Channel("nightly").Latest != "nightly-20261024" {
		t.Errorf("after the late read: %+v, %v; want the fetched catalog kept", snap, err)
	}
	if _, ok := snap.Lookup("nightly-20261030"); ok {
		t.Error("the late read replaced the fetched catalog")
	}
}

// catalogSlowSaveStore blocks saves until their ctx ends.
type catalogSlowSaveStore struct {
	catalogMemStore
	saveErr error
}

func (s *catalogSlowSaveStore) SaveReleaseCatalog(ctx context.Context, _ []byte) error {
	<-ctx.Done()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveErr = ctx.Err()
	return s.saveErr
}

func TestCatalog_SlowSaveIsBounded(t *testing.T) {
	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
	store := &catalogSlowSaveStore{}
	c := newTestCatalog(f, newCatalogClock(), func(o *CatalogOptions) { o.Store = store })
	c.loadTimeout = 50 * time.Millisecond
	snap, err := getWithin(t, c, 10*time.Second)
	if err != nil || snap.Channel("nightly").Latest != "nightly-20261024" {
		t.Fatalf("Get with a stalled save = %+v, %v", snap, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !errors.Is(store.saveErr, context.DeadlineExceeded) {
		t.Errorf("save ended with %v, want its own deadline", store.saveErr)
	}
}

func TestGitHubReleases_ListReleases(t *testing.T) {
	var gotAuth, gotINM, gotQuery []string
	var next string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		gotINM = append(gotINM, r.Header.Get("If-None-Match"))
		gotQuery = append(gotQuery, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("If-None-Match") == `"e1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if next != "" {
			w.Header().Set("Link", `<`+next+`>; rel="next"`)
		}
		w.Header().Set("ETag", `"e1"`)
		_, _ = w.Write([]byte(`[{"tag_name":"nightly-20261024","published_at":"2026-10-24T04:00:00Z",
			"html_url":"https://github.com/o/r/releases/tag/nightly-20261024",
			"assets":[{"name":"SHA256SUMS","browser_download_url":"u","size":3,"updated_at":"2026-10-24T05:00:00Z"}]}]`))
	}))
	defer srv.Close()
	g := &GitHubReleases{Repo: "o/r", APIBase: srv.URL, Token: "tok"}
	ctx := context.Background()

	next = srv.URL + "/repositories/7/releases?per_page=100&page=2"
	p, err := g.ListReleases(ctx, "", "")
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if gotQuery[0] != "/repos/o/r/releases?per_page=100" || gotAuth[0] != "Bearer tok" || gotINM[0] != "" {
		t.Errorf("first request = %q auth=%q inm=%q", gotQuery[0], gotAuth[0], gotINM[0])
	}
	if p.ETag != `"e1"` || p.Next != next || p.NotModified || len(p.Releases) != 1 {
		t.Fatalf("page = %+v", p)
	}
	r := p.Releases[0]
	if r.Tag != "nightly-20261024" || r.HTMLURL == "" || r.Assets[0].UpdatedAt.IsZero() {
		t.Errorf("release = %+v", r)
	}

	p, err = g.ListReleases(ctx, next, `"e1"`)
	if err != nil || !p.NotModified || p.ETag != `"e1"` {
		t.Fatalf("304 page = %+v, %v", p, err)
	}
	if gotQuery[1] != "/repositories/7/releases?per_page=100&page=2" || gotINM[1] != `"e1"` {
		t.Errorf("second request = %q inm=%q", gotQuery[1], gotINM[1])
	}

	// A next link off the API host is refused (it would get the token).
	next = "https://evil.example.com/repositories/7/releases?page=2"
	if _, err := g.ListReleases(ctx, "", ""); err == nil {
		t.Error("foreign next link accepted")
	}
	// So is a page URL off the API host, before any request.
	before := len(gotQuery)
	for _, u := range []string{"https://evil.example.com/repos/o/r/releases", srv.URL + "/repos/o/r/contents", "http://user@" + strings.TrimPrefix(srv.URL, "http://") + "/repos/o/r/releases"} {
		if _, err := g.ListReleases(ctx, u, ""); err == nil {
			t.Errorf("page URL %q accepted", u)
		}
	}
	if len(gotQuery) != before {
		t.Error("request made for a refused page URL")
	}
}

func TestNextPageLink(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{`<https://a/x?page=2>; rel="next", <https://a/x?page=5>; rel="last"`}, "https://a/x?page=2"},
		{[]string{`<https://a/x?page=1>; rel="prev"`, `<https://a/x?page=3>; rel=next`}, "https://a/x?page=3"},
		{[]string{`<https://a/x?page=5>; rel="last"`}, ""},
		{[]string{`https://a/x; rel="next"`}, ""},
	} {
		if got := nextPageLink(tc.in); got != tc.want {
			t.Errorf("nextPageLink(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGitHubReleases_FetchChecksumsPinned(t *testing.T) {
	f := newCatalogFake(t, catalogGood("nightly-20261024", catalogDay(24)))
	g := &GitHubReleases{Repo: testRepo, Client: &http.Client{Transport: f}}
	ctx := context.Background()
	if _, err := g.FetchChecksums(ctx, "nightly-20261024", ReleaseAsset{Name: ChecksumsAssetName, URL: "https://evil.example.com/SHA256SUMS"}); err == nil {
		t.Error("foreign SHA256SUMS URL accepted")
	}
	if _, err := g.FetchChecksums(ctx, "nightly-20261024", ReleaseAsset{URL: "https://github.com/" + testRepo + "/releases/download/nightly-20261023/SHA256SUMS"}); err == nil {
		t.Error("SHA256SUMS URL of another tag accepted")
	}
	if len(f.requests) != 0 {
		t.Errorf("requests for refused URLs: %v", f.requests)
	}
	data, err := g.FetchChecksums(ctx, "nightly-20261024", ReleaseAsset{URL: "https://github.com/" + testRepo + "/releases/download/nightly-20261024/SHA256SUMS"})
	if err != nil || string(data) != catalogGoodSums() {
		t.Errorf("FetchChecksums = %q, %v", data, err)
	}
}

func TestExpectedChecksum_ErrorKinds(t *testing.T) {
	asset := BinaryAssetName()
	good := strings.Repeat("ab", 32)
	for name, tc := range map[string]struct {
		sums string
		kind error
	}{
		"missing":    {good + "  other.tar.gz\n", ErrChecksumMissing},
		"empty hash": {emptySHA256 + "  " + asset + "\n", ErrChecksumBad},
		"conflict":   {good + "  " + asset + "\n" + strings.Repeat("cd", 32) + "  " + asset + "\n", ErrChecksumBad},
	} {
		_, err := ExpectedChecksum([]byte(tc.sums), asset)
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.kind)
		}
	}
}

// TestCatalogChannels_ReturnsCopy pins that the channel list cannot be
// changed through the accessor: each call returns a fresh slice in display
// order, and changing one leaves the next call (and ValidateParams) intact.
func TestCatalogChannels_ReturnsCopy(t *testing.T) {
	want := []string{"nightly", "preview", "stable"}
	got := CatalogChannels()
	if !slices.Equal(got, want) {
		t.Fatalf("CatalogChannels() = %v, want %v", got, want)
	}
	got[0] = "beta"
	if again := CatalogChannels(); !slices.Equal(again, want) {
		t.Fatalf("CatalogChannels() after mutating a returned slice = %v, want %v", again, want)
	}
	e := NewBinaryUpdateExecutor(BinaryUpdateOptions{CurrentVersion: "nightly-20261022"})
	if err := e.ValidateParams(map[string]string{ParamTargetVersion: "latest:nightly"}); err != nil {
		t.Fatalf("latest:nightly refused after mutating a returned slice: %v", err)
	}
	if err := e.ValidateParams(map[string]string{ParamTargetVersion: "latest:beta"}); err == nil {
		t.Fatal("latest:beta accepted after mutating a returned slice")
	}
}
