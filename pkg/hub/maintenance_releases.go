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
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// HubSettingSectionReleaseCatalog holds the persisted release catalog
// (opaque JSON owned by maintenance.Catalog), so the catalog is warm after a
// restart.
const HubSettingSectionReleaseCatalog = "system.release_catalog"

// releaseDateRetry is how long a failed single-tag lookup of the running
// release is not repeated by the releases endpoint.
const releaseDateRetry = 15 * time.Minute

// updateReleaseCatalog returns the hub's one release catalog: the
// configured MaintenanceConfig.ReleaseCatalog, else a maintenance.Catalog for
// GitHubRepo built on first use, persisted in system.release_catalog and
// overlaid with the installer's retained releases. One instance holds the
// cache, the refresh throttle and the singleflight for every reader (the
// releases endpoint, the checker, the update executor).
func (s *Server) updateReleaseCatalog() maintenance.CatalogSource {
	s.updateCatalogOnce.Do(func() {
		mc := s.config.MaintenanceConfig
		if mc.ReleaseCatalog != nil {
			s.updateCatalog = mc.ReleaseCatalog
			return
		}
		opts := maintenance.CatalogOptions{
			Repo:  mc.GitHubRepo,
			Store: hubReleaseCatalogStore{s: s},
			Local: func() []maintenance.InstalledRelease { return s.updateInstaller().Retained() },
		}
		if l, ok := mc.ReleaseSource.(maintenance.ReleaseLister); ok {
			opts.Lister = l
		}
		s.updateCatalog = maintenance.NewCatalog(opts)
	})
	return s.updateCatalog
}

// updateReleaseSource returns the single-tag release lookup: the configured
// MaintenanceConfig.ReleaseSource, else GitHub's API for GitHubRepo.
func (s *Server) updateReleaseSource() maintenance.ReleaseSource {
	if src := s.config.MaintenanceConfig.ReleaseSource; src != nil {
		return src
	}
	repo := s.config.MaintenanceConfig.GitHubRepo
	if repo == "" {
		repo = maintenance.DefaultGitHubRepo
	}
	return maintenance.NewGitHubReleases(repo)
}

// runningReleaseDater returns the lookup the releases endpoint uses to date
// a running release the catalog does not list. Lookups are remembered (a
// failure for releaseDateRetry), so page loads do not spend the GitHub API
// budget.
func (s *Server) runningReleaseDater() maintenance.ReleaseSource {
	s.updateDaterOnce.Do(func() {
		s.updateDater = &memoReleaseSource{src: s.updateReleaseSource(), retry: releaseDateRetry, now: time.Now}
	})
	return s.updateDater
}

// hubReleaseCatalogStore persists the release catalog in hub_settings.
type hubReleaseCatalogStore struct{ s *Server }

// LoadReleaseCatalog implements maintenance.CatalogStore.
func (c hubReleaseCatalogStore) LoadReleaseCatalog(ctx context.Context) ([]byte, error) {
	setting, err := c.s.store.GetHubSetting(ctx, HubSettingSectionReleaseCatalog)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return setting.Value, nil
}

// SaveReleaseCatalog implements maintenance.CatalogStore.
func (c hubReleaseCatalogStore) SaveReleaseCatalog(ctx context.Context, data []byte) error {
	_, err := c.s.store.UpsertHubSetting(ctx, HubSettingSectionReleaseCatalog, json.RawMessage(data), "system", -1, "managed")
	return err
}

// memoReleaseSource remembers single-tag lookups: a found release for good
// (its publish date never changes), an upstream failure for retry. A lookup
// ended by the caller's context is not remembered.
type memoReleaseSource struct {
	src   maintenance.ReleaseSource
	retry time.Duration
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]memoRelease
}

type memoRelease struct {
	rel *maintenance.Release
	err error
	at  time.Time
}

// ReleaseByTag implements maintenance.ReleaseSource.
func (m *memoReleaseSource) ReleaseByTag(ctx context.Context, tag string) (*maintenance.Release, error) {
	m.mu.Lock()
	if e, ok := m.entries[tag]; ok && (e.err == nil || m.now().Sub(e.at) < m.retry) {
		m.mu.Unlock()
		return e.rel, e.err
	}
	m.mu.Unlock()

	rel, err := m.src.ReleaseByTag(ctx, tag)
	// A lookup cut short by the caller (cancelled request, deadline) says
	// nothing about the release; remembering it would serve that failure
	// to every caller for the retry window.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return rel, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]memoRelease{}
	}
	m.entries[tag] = memoRelease{rel: rel, err: err, at: m.now()}
	return rel, err
}

// releasesResponse is GET /api/v1/admin/maintenance/releases.
type releasesResponse struct {
	Repo      string                     `json:"repo"`
	FetchedAt time.Time                  `json:"fetchedAt"`
	Stale     bool                       `json:"stale"`
	Channels  map[string]releasesChannel `json:"channels"`
}

type releasesChannel struct {
	// Latest is the newest installable release; null when there is none.
	Latest   *string         `json:"latest"`
	Releases []releasesEntry `json:"releases"`
}

// releasesEntry is one release on the wire (design D5). It is mapped from
// maintenance.CatalogRelease rather than embedding it: the catalog's JSON is
// its persisted format, not the API's.
type releasesEntry struct {
	Tag     string `json:"tag"`
	Channel string `json:"channel"`
	// PublishedAt is omitted for a local-only release (no GitHub date).
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
	Installable bool       `json:"installable"`
	Reason      string     `json:"reason,omitempty"`
	// Class is how the release relates to the running version (see
	// releaseClass).
	Class    string `json:"class"`
	Local    bool   `json:"local"`
	Source   string `json:"source"`
	NotesURL string `json:"notesUrl,omitempty"`
}

// newReleasesEntry maps a catalog release to the wire format.
func newReleasesEntry(r maintenance.CatalogRelease, class string) releasesEntry {
	e := releasesEntry{
		Tag:         r.Tag,
		Channel:     r.Channel,
		Installable: r.Installable,
		Reason:      r.Reason,
		Class:       class,
		Local:       r.Local,
		Source:      r.Source,
		NotesURL:    r.NotesURL,
	}
	if !r.PublishedAt.IsZero() {
		published := r.PublishedAt
		e.PublishedAt = &published
	}
	return e
}

// releaseClassBelowFloor is the releases API class of a release older than
// the running binary's downgrade floor.
const releaseClassBelowFloor = "below-floor"

// releaseClass classifies a catalog release against the running version
// cur. A running git build is compared by its build time, so every release
// is a switch-newer or switch-older (design D3). An order that cannot be
// decided (no date on one side of a cross-channel comparison) is reported
// as switch-older: the update executor refuses such a target as a possible
// downgrade, so the picker must offer it only behind the typed
// confirmation. A release older than floor (version.DowngradeFloor) is
// below-floor whatever its order: the executor refuses it
// (below_downgrade_floor). A release whose floor order cannot be decided
// (no publish date) keeps its class; the executor decides.
func releaseClass(cur update.Ref, r maintenance.CatalogRelease, floor string) string {
	tgt := update.Ref{Version: r.Tag, Date: r.PublishedAt}
	c := update.CompareRefs(cur, tgt)
	if c != update.ClassSame {
		if below, known := update.BelowFloor(floor, tgt); known && below {
			return releaseClassBelowFloor
		}
	}
	if c == update.ClassUnknown {
		return string(update.ClassSwitchOlder)
	}
	return string(c)
}

// buildReleasesResponse reads the release catalog (refreshing it on
// request, within the catalog's throttle) and classifies every release
// against the running build cur.
func (s *Server) buildReleasesResponse(ctx context.Context, cur runningBuild, refresh bool) (*releasesResponse, error) {
	snap, err := s.updateReleaseCatalog().Get(ctx, refresh)
	if err != nil {
		return nil, err
	}
	var buildTime time.Time
	if t, err := time.Parse(time.RFC3339, cur.BuildTime); err == nil {
		buildTime = t
	}
	running := maintenance.RunningRef(ctx, snap, s.runningReleaseDater(), cur.Version, buildTime)

	resp := &releasesResponse{
		Repo:      snap.Repo,
		FetchedAt: snap.FetchedAt,
		Stale:     snap.Stale,
		Channels:  make(map[string]releasesChannel),
	}
	for _, name := range maintenance.CatalogChannels() {
		ch := snap.Channel(name)
		out := releasesChannel{Releases: make([]releasesEntry, 0, len(ch.Releases))}
		if ch.Latest != "" {
			latest := ch.Latest
			out.Latest = &latest
		}
		for _, r := range ch.Releases {
			out.Releases = append(out.Releases, newReleasesEntry(r, releaseClass(running, r, version.DowngradeFloor)))
		}
		resp.Channels[name] = out
	}
	return resp, nil
}

// handleMaintenanceReleases handles GET
// /api/v1/admin/maintenance/releases?refresh=0|1: the release catalog per
// channel, each release classified against the running version. refresh=1
// bypasses the catalog's TTL (at most one fetch per refresh interval). When
// GitHub cannot be reached the last good catalog is served with stale=true;
// with no catalog at all the answer is 503.
//
// Authorization: enforced by routeGuard via hub.maintenance.execute permission.
func (s *Server) handleMaintenanceReleases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	refresh := false
	switch v := r.URL.Query().Get("refresh"); v {
	case "", "0", "false":
	case "1", "true":
		refresh = true
	default:
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "refresh must be 0 or 1", nil)
		return
	}
	resp, err := s.buildReleasesResponse(r.Context(), currentRunningBuild(), refresh)
	if err != nil {
		s.maintenanceLog.Warn("Release catalog unavailable", "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"The release list could not be fetched from GitHub; try again later", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
