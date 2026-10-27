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

	mu         sync.Mutex
	releases   []catalogFakeRelease // list order, newest created first
	orphanTags []string             // git tags without a release
	perPage    int
	failList   bool
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
	return &catalogFake{t: t, releases: rels, perPage: 100, sumsCalls: map[string]int{}}
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
	f.mu.Unlock()
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
type catalogMemStore struct {
	mu    sync.Mutex
	data  []byte
	saves int
}

func (s *catalogMemStore) LoadReleaseCatalog(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.data), nil
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
	f.orphanTags = []string{"nightly-20261026", "v0.3.0-preview.5"}
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
	// JSON: stable is {"latest":"","releases":[]}, never null releases.
	b, _ := json.Marshal(snap.Channels["stable"])
	if string(b) != `{"latest":"","releases":[]}` {
		t.Errorf("stable JSON = %s", b)
	}

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

	const n = 10
	type result struct {
		snap Snapshot
		err  error
	}
	results := make(chan result, n)
	get := func() {
		s, err := c.Get(context.Background(), true)
		results <- result{s, err}
	}
	go get()
	<-f.started // the first fetch is in flight and blocked
	for range n - 1 {
		go get()
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

func TestCatalog_CancelledCallerDoesNotFailTheFetch(t *testing.T) {
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
	close(f.block)
	if err := <-done; err != nil {
		t.Fatalf("Get with a cancelled caller: %v", err)
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
