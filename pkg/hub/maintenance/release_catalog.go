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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// Release catalog defaults (design D2).
const (
	// DefaultCatalogTTL is how long a fetched catalog is served without a
	// refetch.
	DefaultCatalogTTL = 15 * time.Minute
	// DefaultCatalogRefreshInterval is the minimum time between two fetch
	// attempts: it throttles manual refreshes and backs off after an error.
	DefaultCatalogRefreshInterval = 60 * time.Second
	// DefaultCatalogPerChannel is how many installable releases each channel
	// lists.
	DefaultCatalogPerChannel = 10
	// DefaultCatalogNonInstallable is how many recent non-installable
	// releases each channel lists for context.
	DefaultCatalogNonInstallable = 3
	// DefaultCatalogMaxPages is how many release list pages one fetch reads.
	DefaultCatalogMaxPages = 3

	// catalogFetchTimeout bounds one whole refresh (list pages and SHA256SUMS
	// downloads). Each request has its own 15 s timeout as well.
	catalogFetchTimeout = 2 * time.Minute

	// catalogStateVersion versions the persisted JSON; another version is
	// ignored on load.
	catalogStateVersion = 1
)

// CatalogChannels are the release channels, in display order.
var CatalogChannels = []string{"nightly", "preview", "stable"}

// Catalog release sources.
const (
	SourceGitHub = "github"
	SourceLocal  = "local"
)

// Reasons a release is not installable.
const (
	ReasonNoChecksums          = "no SHA256SUMS"
	ReasonBadChecksum          = "bad checksum entry"
	ReasonChecksumsUnavailable = "SHA256SUMS download failed"
)

// ReasonNoBinary is the reason for a release without the asset tarball.
func ReasonNoBinary(asset string) string { return "no " + asset + " asset" }

// ReasonNoChecksumFor is the reason for a SHA256SUMS without a well-formed
// line for asset.
func ReasonNoChecksumFor(asset string) string { return "no checksum for " + asset }

// CatalogRelease is one release in a catalog channel.
type CatalogRelease struct {
	Tag     string `json:"tag"`
	Channel string `json:"channel"`
	// PublishedAt is GitHub's published_at; zero for a local-only release.
	PublishedAt time.Time `json:"publishedAt"`
	Installable bool      `json:"installable"`
	// Reason says why the release is not installable.
	Reason string `json:"reason,omitempty"`
	// Local is set when the install root retains a copy of this release.
	Local bool `json:"local"`
	// Source is SourceGitHub, or SourceLocal for a retained release that is
	// no longer listed on GitHub (deleted nightly).
	Source   string `json:"source"`
	NotesURL string `json:"notesUrl,omitempty"`
}

// CatalogChannel is one channel of a Snapshot.
type CatalogChannel struct {
	// Latest is the newest installable GitHub release, "" when there is none.
	Latest string `json:"latest"`
	// Releases are newest first: up to PerChannel installable releases plus
	// up to NonInstallable non-installable ones, plus local-only releases.
	Releases []CatalogRelease `json:"releases"`
}

// Snapshot is the release catalog at one point in time. It is a value: the
// caller may keep and read it without locking.
type Snapshot struct {
	Repo string `json:"repo"`
	// FetchedAt is when the release list was last fetched successfully.
	FetchedAt time.Time `json:"fetchedAt"`
	// Stale is set when the latest fetch attempt failed; the snapshot is
	// then the last good one, as of FetchedAt.
	Stale bool `json:"stale"`
	// Error is the failed attempt's error when Stale.
	Error    string                    `json:"error,omitempty"`
	Channels map[string]CatalogChannel `json:"channels"`
}

// Channel returns the named channel; unknown or empty channels have no
// releases.
func (s Snapshot) Channel(name string) CatalogChannel {
	ch := s.Channels[name]
	if ch.Releases == nil {
		ch.Releases = []CatalogRelease{}
	}
	return ch
}

// Latest returns the newest installable release of channel.
func (s Snapshot) Latest(channel string) (CatalogRelease, bool) {
	ch := s.Channels[channel]
	if ch.Latest == "" {
		return CatalogRelease{}, false
	}
	for _, r := range ch.Releases {
		if r.Tag == ch.Latest {
			return r, true
		}
	}
	return CatalogRelease{}, false
}

// Lookup returns the catalog entry for tag, in whichever channel lists it.
func (s Snapshot) Lookup(tag string) (CatalogRelease, bool) {
	ch := s.Channels[update.DetectChannel(tag)]
	for _, r := range ch.Releases {
		if r.Tag == tag {
			return r, true
		}
	}
	return CatalogRelease{}, false
}

// CatalogStore persists the catalog across restarts (the hub stores it in
// the hub_settings row system.release_catalog). Load returns nil, nil when
// nothing is stored. The data is opaque JSON owned by the catalog.
type CatalogStore interface {
	LoadReleaseCatalog(ctx context.Context) ([]byte, error)
	SaveReleaseCatalog(ctx context.Context, data []byte) error
}

// CatalogOptions configures NewCatalog. Zero values take the defaults.
type CatalogOptions struct {
	// Repo is the release repository (owner/name); default DefaultGitHubRepo.
	Repo string
	// Lister fetches from GitHub; default NewGitHubReleases(Repo).
	Lister ReleaseLister
	// Store persists the catalog; nil keeps it in memory only.
	Store CatalogStore
	// Local reports the releases retained in the install root
	// (Installer.Retained); nil means none.
	Local func() []InstalledRelease
	// Asset is the tarball a release must carry; default BinaryAssetName().
	Asset string

	TTL             time.Duration
	RefreshInterval time.Duration
	PerChannel      int
	// NonInstallable is the non-installable releases listed per channel;
	// 0 means the default, a negative value means none.
	NonInstallable int
	MaxPages       int

	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Catalog is the release catalog of one repository (design D2): GitHub's
// release list, classified per channel, cached in memory and in Store,
// refreshed at most every TTL (and on demand at most every
// RefreshInterval), with concurrent refreshes collapsed into one.
type Catalog struct {
	opts  CatalogOptions
	group singleflight.Group

	mu          sync.Mutex
	loaded      bool
	inflight    bool
	state       catalogState
	lastAttempt time.Time
	lastErr     error
}

// catalogState is the cached catalog, persisted as JSON.
type catalogState struct {
	Version   int       `json:"version"`
	Repo      string    `json:"repo"`
	Asset     string    `json:"asset"`
	FetchedAt time.Time `json:"fetchedAt"`
	// Pages are the release list pages with their ETags, for If-None-Match.
	Pages []catalogPage `json:"pages"`
	// Sums caches SHA256SUMS verdicts by sumsKey (tag + asset updated_at).
	Sums map[string]sumsVerdict `json:"sums"`
	// Channels is the classified catalog, without local overlay.
	Channels map[string]CatalogChannel `json:"channels"`
}

type catalogPage struct {
	URL      string    `json:"url"` // "" for the first page
	ETag     string    `json:"etag"`
	Next     string    `json:"next"`
	Releases []Release `json:"releases"`
}

type sumsVerdict struct {
	Installable bool   `json:"installable"`
	Reason      string `json:"reason,omitempty"`
}

// NewCatalog returns a catalog for opts.Repo. Nothing is fetched until Get.
func NewCatalog(opts CatalogOptions) *Catalog {
	if opts.Repo == "" {
		opts.Repo = DefaultGitHubRepo
	}
	if opts.Lister == nil {
		opts.Lister = NewGitHubReleases(opts.Repo)
	}
	if opts.Asset == "" {
		opts.Asset = BinaryAssetName()
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultCatalogTTL
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultCatalogRefreshInterval
	}
	if opts.PerChannel <= 0 {
		opts.PerChannel = DefaultCatalogPerChannel
	}
	if opts.NonInstallable < 0 {
		opts.NonInstallable = 0
	} else if opts.NonInstallable == 0 {
		opts.NonInstallable = DefaultCatalogNonInstallable
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = DefaultCatalogMaxPages
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Catalog{opts: opts}
}

// Get returns the catalog. It fetches from GitHub when the cached copy is
// older than the TTL, or when refresh is set and no fetch was attempted in
// the last RefreshInterval; otherwise it serves the cache. When a fetch
// fails and a previous catalog exists, that catalog is returned with Stale
// set and a nil error. The error is non-nil only when there is no catalog
// at all.
func (c *Catalog) Get(ctx context.Context, refresh bool) (Snapshot, error) {
	c.mu.Lock()
	c.loadLocked(ctx)
	// A caller with nothing to serve, or asking for a refresh, waits for a
	// fetch already in flight instead of returning what is cached.
	need := c.needsFetchLocked(refresh) || (c.inflight && (refresh || c.state.FetchedAt.IsZero()))
	c.mu.Unlock()

	if need {
		// Do collapses concurrent fetches: a caller arriving during a fetch
		// joins it. The re-check inside stops a caller that decided to
		// fetch just before another fetch finished from fetching again.
		_, _, _ = c.group.Do("refresh", func() (any, error) {
			c.mu.Lock()
			again := c.needsFetchLocked(refresh)
			c.mu.Unlock()
			if again {
				c.refresh(ctx)
			}
			return nil, nil
		})
	}

	var local []InstalledRelease
	if c.opts.Local != nil {
		local = c.opts.Local()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.FetchedAt.IsZero() {
		if c.lastErr != nil {
			return Snapshot{}, fmt.Errorf("release catalog of %s unavailable: %w", c.opts.Repo, c.lastErr)
		}
		return Snapshot{}, fmt.Errorf("release catalog of %s unavailable", c.opts.Repo)
	}
	return c.snapshotLocked(local), nil
}

// needsFetchLocked decides whether Get fetches. Every path respects
// RefreshInterval since the last attempt, which both throttles manual
// refreshes and backs off after a failed fetch.
func (c *Catalog) needsFetchLocked(refresh bool) bool {
	now := c.opts.Now()
	if !c.lastAttempt.IsZero() && now.Sub(c.lastAttempt) < c.opts.RefreshInterval {
		return false
	}
	if refresh || c.state.FetchedAt.IsZero() {
		return true
	}
	return now.Sub(c.state.FetchedAt) >= c.opts.TTL
}

// loadLocked reads the persisted copy once. A copy for another repo, asset
// or format version is ignored.
func (c *Catalog) loadLocked(ctx context.Context) {
	if c.loaded {
		return
	}
	c.loaded = true
	if c.opts.Store == nil {
		return
	}
	log := logging.Subsystem("hub.maintenance.release-catalog")
	data, err := c.opts.Store.LoadReleaseCatalog(ctx)
	if err != nil {
		log.Warn("Failed to load the persisted release catalog", "error", err)
		return
	}
	if len(data) == 0 {
		return
	}
	var st catalogState
	if err := json.Unmarshal(data, &st); err != nil {
		log.Warn("Ignoring an unreadable persisted release catalog", "error", err)
		return
	}
	if st.Version != catalogStateVersion || !strings.EqualFold(st.Repo, c.opts.Repo) || st.Asset != c.opts.Asset || st.FetchedAt.IsZero() {
		log.Info("Ignoring a persisted release catalog for another repo, asset or format",
			"repo", st.Repo, "asset", st.Asset, "version", st.Version)
		return
	}
	c.state = st
}

// refresh fetches the catalog and replaces the cached copy on success. It
// runs detached from the caller's cancellation, because other callers may
// be waiting on the same fetch.
func (c *Catalog) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogFetchTimeout)
	defer cancel()
	log := logging.Subsystem("hub.maintenance.release-catalog")

	c.mu.Lock()
	c.lastAttempt = c.opts.Now()
	c.inflight = true
	prev := c.state
	c.mu.Unlock()

	st, err := c.fetch(ctx, prev)

	c.mu.Lock()
	c.inflight = false
	if err != nil {
		c.lastErr = err
		c.mu.Unlock()
		log.Warn("Release catalog fetch failed", "repo", c.opts.Repo, "error", err)
		return
	}
	c.state = st
	c.lastErr = nil
	c.mu.Unlock()

	if c.opts.Store == nil {
		return
	}
	data, err := json.Marshal(st)
	if err == nil {
		err = c.opts.Store.SaveReleaseCatalog(ctx, data)
	}
	if err != nil {
		log.Warn("Failed to persist the release catalog", "error", err)
	}
}

// fetch reads release list pages (reusing prev's pages on 304) until every
// channel has PerChannel installable releases, there is no next page, or
// MaxPages pages were read, and classifies the releases.
func (c *Catalog) fetch(ctx context.Context, prev catalogState) (catalogState, error) {
	prevPages := make(map[string]catalogPage, len(prev.Pages))
	for _, p := range prev.Pages {
		prevPages[p.URL] = p
	}
	cl := &classifier{
		ctx:      ctx,
		lister:   c.opts.Lister,
		asset:    c.opts.Asset,
		prevSums: prev.Sums,
		sums:     map[string]sumsVerdict{},
		verdicts: map[string]sumsVerdict{},
	}
	st := catalogState{
		Version: catalogStateVersion,
		Repo:    c.opts.Repo,
		Asset:   c.opts.Asset,
		Sums:    cl.sums,
	}

	var releases []Release
	pageURL := ""
	for i := 0; i < c.opts.MaxPages; i++ {
		old, hasOld := prevPages[pageURL]
		etag := ""
		if hasOld {
			etag = old.ETag
		}
		p, err := c.opts.Lister.ListReleases(ctx, pageURL, etag)
		if err != nil {
			return catalogState{}, err
		}
		page := old
		if !p.NotModified {
			page = catalogPage{URL: pageURL, ETag: p.ETag, Next: p.Next, Releases: p.Releases}
		} else if !hasOld {
			return catalogState{}, fmt.Errorf("GitHub answered 304 Not Modified for an uncached release list page")
		}
		st.Pages = append(st.Pages, page)
		releases = append(releases, page.Releases...)

		channels, err := c.build(cl, releases)
		if err != nil {
			return catalogState{}, err
		}
		st.Channels = channels
		if page.Next == "" || c.full(channels) {
			break
		}
		pageURL = page.Next
	}
	st.FetchedAt = c.opts.Now()
	return st, nil
}

// full reports whether every channel lists PerChannel installable releases.
func (c *Catalog) full(channels map[string]CatalogChannel) bool {
	for _, name := range CatalogChannels {
		n := 0
		for _, r := range channels[name].Releases {
			if r.Installable {
				n++
			}
		}
		if n < c.opts.PerChannel {
			return false
		}
	}
	return true
}

// build groups releases by channel, newest first, and classifies them until
// each channel has PerChannel installable ones. Older releases are not
// classified, so their SHA256SUMS is never downloaded.
func (c *Catalog) build(cl *classifier, releases []Release) (map[string]CatalogChannel, error) {
	byChannel := map[string][]CatalogRelease{}
	byTag := make(map[string]Release, len(releases))
	for _, r := range releases {
		// Drafts are skipped; so is any tag that is not a well-formed
		// release tag of a channel (it would be unsafe in a URL path).
		if _, dup := byTag[r.Tag]; r.Draft || dup || !update.IsReleaseTag(r.Tag) {
			continue
		}
		byTag[r.Tag] = r
		ch := update.DetectChannel(r.Tag)
		byChannel[ch] = append(byChannel[ch], CatalogRelease{
			Tag: r.Tag, Channel: ch, PublishedAt: r.PublishedAt, Source: SourceGitHub, NotesURL: r.HTMLURL,
		})
	}

	out := make(map[string]CatalogChannel, len(CatalogChannels))
	for _, name := range CatalogChannels {
		cands := byChannel[name]
		slices.SortStableFunc(cands, compareNewestFirst)
		ch := CatalogChannel{Releases: []CatalogRelease{}}
		installable, other := 0, 0
		for _, cr := range cands {
			if installable >= c.opts.PerChannel {
				break
			}
			v, err := cl.classify(byTag[cr.Tag])
			if err != nil {
				return nil, err
			}
			cr.Installable, cr.Reason = v.Installable, v.Reason
			if cr.Installable {
				installable++
				if ch.Latest == "" {
					ch.Latest = cr.Tag
				}
			} else {
				if other >= c.opts.NonInstallable {
					continue
				}
				other++
			}
			ch.Releases = append(ch.Releases, cr)
		}
		out[name] = ch
	}
	return out, nil
}

// classifier decides installability, downloading SHA256SUMS at most once
// per refresh and reusing verdicts cached per tag + asset updated_at.
type classifier struct {
	ctx      context.Context
	lister   ReleaseLister
	asset    string
	prevSums map[string]sumsVerdict // from the previous catalog
	sums     map[string]sumsVerdict // carried into the new catalog
	verdicts map[string]sumsVerdict // this refresh, by tag
}

// sumsKey keys a SHA256SUMS verdict: a re-uploaded asset gets a new
// updated_at and so a new verdict.
func sumsKey(tag string, sums ReleaseAsset) string {
	return tag + "@" + sums.UpdatedAt.UTC().Format(time.RFC3339Nano)
}

func (cl *classifier) classify(r Release) (sumsVerdict, error) {
	if v, ok := cl.verdicts[r.Tag]; ok {
		return v, nil
	}
	v, err := cl.decide(r)
	if err != nil {
		return sumsVerdict{}, err
	}
	cl.verdicts[r.Tag] = v
	return v, nil
}

func (cl *classifier) decide(r Release) (sumsVerdict, error) {
	if _, ok := r.Asset(cl.asset); !ok {
		return sumsVerdict{Reason: ReasonNoBinary(cl.asset)}, nil
	}
	sumsAsset, ok := r.Asset(ChecksumsAssetName)
	if !ok {
		return sumsVerdict{Reason: ReasonNoChecksums}, nil
	}
	key := sumsKey(r.Tag, sumsAsset)
	if v, ok := cl.prevSums[key]; ok {
		cl.sums[key] = v
		return v, nil
	}
	data, err := cl.lister.FetchChecksums(cl.ctx, r.Tag, sumsAsset)
	if err != nil {
		if ctxErr := cl.ctx.Err(); ctxErr != nil {
			return sumsVerdict{}, fmt.Errorf("release catalog fetch: %w", ctxErr)
		}
		logging.Subsystem("hub.maintenance.release-catalog").Warn("SHA256SUMS download failed",
			"tag", r.Tag, "error", err)
		// Not cached: the next refresh tries again.
		return sumsVerdict{Reason: ReasonChecksumsUnavailable}, nil
	}
	// The same per-asset rule the executor applies at install time: only
	// the asset's own well-formed line counts, and the empty-file hash is
	// treated as no entry.
	var v sumsVerdict
	switch _, err := ExpectedChecksum(data, cl.asset); {
	case err == nil:
		v.Installable = true
	case errors.Is(err, ErrChecksumMissing):
		v.Reason = ReasonNoChecksumFor(cl.asset)
	default:
		v.Reason = ReasonBadChecksum
	}
	cl.sums[key] = v
	return v, nil
}

// compareNewestFirst orders catalog releases newest first: by publish date
// (a nightly tag's own date when there is none), then by version, so that
// nightly-YYYYMMDD.N suffixes order numerically, then by tag for a stable
// order.
func compareNewestFirst(a, b CatalogRelease) int {
	da, db := releaseSortDate(a), releaseSortDate(b)
	if c := db.Compare(da); c != 0 {
		return c
	}
	switch update.Compare(b.Tag, a.Tag) {
	case update.ClassUpgrade: // a is newer than b
		return -1
	case update.ClassDowngrade:
		return 1
	}
	return strings.Compare(b.Tag, a.Tag)
}

func releaseSortDate(r CatalogRelease) time.Time {
	if !r.PublishedAt.IsZero() {
		return r.PublishedAt
	}
	if d, _, ok := update.ParseNightly(r.Tag); ok {
		return d
	}
	return time.Time{}
}

// snapshotLocked copies the cached catalog and overlays the retained local
// releases: a listed release is marked Local, and a retained release that
// GitHub no longer lists is added with Source "local". Local-only releases
// were verified when installed, so they are installable, but they are never
// a channel's Latest.
func (c *Catalog) snapshotLocked(local []InstalledRelease) Snapshot {
	snap := Snapshot{
		Repo:      c.opts.Repo,
		FetchedAt: c.state.FetchedAt,
		Stale:     c.lastErr != nil,
		Channels:  make(map[string]CatalogChannel, len(CatalogChannels)),
	}
	if c.lastErr != nil {
		snap.Error = c.lastErr.Error()
	}
	localTags := map[string]bool{}
	for _, l := range local {
		if update.IsReleaseTag(l.Tag) {
			localTags[l.Tag] = true
		}
	}
	listed := map[string]bool{}
	for _, name := range CatalogChannels {
		src := c.state.Channels[name]
		ch := CatalogChannel{Latest: src.Latest, Releases: make([]CatalogRelease, 0, len(src.Releases))}
		for _, r := range src.Releases {
			r.Local = localTags[r.Tag]
			listed[r.Tag] = true
			ch.Releases = append(ch.Releases, r)
		}
		snap.Channels[name] = ch
	}
	added := false
	for tag := range localTags {
		if listed[tag] {
			continue
		}
		name := update.DetectChannel(tag)
		ch := snap.Channels[name]
		ch.Releases = append(ch.Releases, CatalogRelease{
			Tag: tag, Channel: name, Installable: true, Local: true, Source: SourceLocal,
		})
		snap.Channels[name] = ch
		added = true
	}
	if added {
		for name, ch := range snap.Channels {
			slices.SortStableFunc(ch.Releases, compareNewestFirst)
			snap.Channels[name] = ch
		}
	}
	return snap
}
