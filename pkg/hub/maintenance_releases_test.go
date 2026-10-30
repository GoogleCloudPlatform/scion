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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// fakeReleaseCatalog is a maintenance.CatalogSource double serving a fixed
// snapshot (or error) and recording how it was asked.
type fakeReleaseCatalog struct {
	snap maintenance.Snapshot
	err  error

	mu        sync.Mutex
	gets      int
	refreshes int
}

func (f *fakeReleaseCatalog) Get(_ context.Context, refresh bool) (maintenance.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if refresh {
		f.refreshes++
	}
	return f.snap, f.err
}

// countingReleaseSource is a maintenance.ReleaseSource double: it knows the
// releases in rels and counts lookups.
type countingReleaseSource struct {
	rels map[string]time.Time

	mu    sync.Mutex
	calls int
}

func (c *countingReleaseSource) ReleaseByTag(ctx context.Context, tag string) (*maintenance.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d, ok := c.rels[tag]; ok {
		return &maintenance.Release{Tag: tag, PublishedAt: d}, nil
	}
	return nil, maintenance.ErrReleaseNotFound
}

func (c *countingReleaseSource) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// releasesTestSnapshot is a catalog with three nightlies, an installable
// and a non-installable preview, a local-only preview and an empty stable
// channel.
func releasesTestSnapshot(t *testing.T) maintenance.Snapshot {
	gh := func(tag, published string, installable bool) maintenance.CatalogRelease {
		r := maintenance.CatalogRelease{Tag: tag, Channel: update.DetectChannel(tag), PublishedAt: mustTime(t, published),
			Installable: installable, Source: maintenance.SourceGitHub,
			NotesURL: "https://github.com/GoogleCloudPlatform/scion/releases/tag/" + tag}
		if !installable {
			r.Reason = maintenance.ReasonNoChecksums
		}
		return r
	}
	return maintenance.Snapshot{
		Repo:      "GoogleCloudPlatform/scion",
		FetchedAt: mustTime(t, "2026-10-27T05:00:00Z"),
		Channels: map[string]maintenance.CatalogChannel{
			"nightly": {Latest: "nightly-20261023", Releases: []maintenance.CatalogRelease{
				gh("nightly-20261023", "2026-10-23T04:00:00Z", true),
				gh("nightly-20261022", "2026-10-22T04:00:00Z", true),
				gh("nightly-20261021", "2026-10-21T04:00:00Z", true),
			}},
			"preview": {Latest: "v0.3.0-preview.4", Releases: []maintenance.CatalogRelease{
				gh("v0.3.0-preview.4", "2026-10-22T18:00:00Z", true),
				gh("v0.3.0-preview.3", "2026-10-15T10:00:00Z", false),
				{Tag: "v0.3.0-preview.1", Channel: "preview", Installable: true, Local: true, Source: maintenance.SourceLocal},
			}},
		},
	}
}

func releasesClasses(resp *releasesResponse) map[string]string {
	got := map[string]string{}
	for _, ch := range resp.Channels {
		for _, r := range ch.Releases {
			got[r.Tag] = r.Class
		}
	}
	return got
}

func TestBuildReleasesResponse(t *testing.T) {
	ctx := context.Background()

	t.Run("classes against a running nightly", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseCatalog: &fakeReleaseCatalog{snap: releasesTestSnapshot(t)},
			ReleaseSource: &countingReleaseSource{}}
		resp, err := srv.buildReleasesResponse(ctx, runningBuild{Version: "nightly-20261022"}, false)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"nightly-20261023": "upgrade",
			"nightly-20261022": "same",
			"nightly-20261021": "downgrade",
			// Published after the running nightly (2026-10-22T04:00Z).
			"v0.3.0-preview.4": "switch-newer",
			"v0.3.0-preview.3": "switch-older",
			// Local-only: no publish date, so the order is unknown and the
			// release is reported as older.
			"v0.3.0-preview.1": "switch-older",
		}
		got := releasesClasses(resp)
		for tag, class := range want {
			if got[tag] != class {
				t.Errorf("%s: class = %q, want %q", tag, got[tag], class)
			}
		}
		if len(got) != len(want) {
			t.Errorf("classes = %v", got)
		}
	})

	t.Run("a git build is classed switch by its build time", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseCatalog: &fakeReleaseCatalog{snap: releasesTestSnapshot(t)}}
		resp, err := srv.buildReleasesResponse(ctx, runningBuild{Commit: "abc", BuildTime: "2026-10-22T12:00:00Z"}, false)
		if err != nil {
			t.Fatal(err)
		}
		got := releasesClasses(resp)
		for tag, class := range map[string]string{
			"nightly-20261023": "switch-newer", "nightly-20261022": "switch-older",
			"v0.3.0-preview.4": "switch-newer", "v0.3.0-preview.3": "switch-older",
		} {
			if got[tag] != class {
				t.Errorf("%s: class = %q, want %q", tag, got[tag], class)
			}
		}
	})

	t.Run("a running release the catalog does not list is looked up once", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		src := &countingReleaseSource{rels: map[string]time.Time{"v0.2.0": mustTime(t, "2026-10-22T00:00:00Z")}}
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseCatalog: &fakeReleaseCatalog{snap: releasesTestSnapshot(t)},
			ReleaseSource: src}
		for i := 0; i < 3; i++ {
			resp, err := srv.buildReleasesResponse(ctx, runningBuild{Version: "v0.2.0"}, false)
			if err != nil {
				t.Fatal(err)
			}
			got := releasesClasses(resp)
			if got["nightly-20261023"] != "switch-newer" || got["nightly-20261021"] != "switch-older" ||
				got["v0.3.0-preview.4"] != "switch-newer" {
				t.Fatalf("classes = %v", got)
			}
		}
		if n := src.callCount(); n != 1 {
			t.Errorf("single-tag lookups = %d, want 1 (remembered)", n)
		}
	})

	t.Run("refresh is passed to the catalog", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		cat := &fakeReleaseCatalog{snap: releasesTestSnapshot(t)}
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseCatalog: cat}
		if _, err := srv.buildReleasesResponse(ctx, runningBuild{Version: "nightly-20261022"}, true); err != nil {
			t.Fatal(err)
		}
		if cat.refreshes != 1 {
			t.Errorf("refreshes = %d", cat.refreshes)
		}
	})
}

func getReleases(t *testing.T, srv *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/releases"+query, nil)
	rr := httptest.NewRecorder()
	srv.handleMaintenanceReleases(rr, req)
	return rr
}

func TestHandleMaintenanceReleases(t *testing.T) {
	t.Run("shape", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		snap := releasesTestSnapshot(t)
		snap.Stale, snap.Error = true, "GitHub said no"
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseCatalog: &fakeReleaseCatalog{snap: snap},
			ReleaseSource: &countingReleaseSource{}}
		rr := getReleases(t, srv, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["repo"] != "GoogleCloudPlatform/scion" || body["fetchedAt"] != "2026-10-27T05:00:00Z" || body["stale"] != true {
			t.Errorf("header = %v", body)
		}
		if _, ok := body["error"]; ok {
			t.Errorf("the fetch error must not be returned: %v", body)
		}
		if strings.Contains(rr.Body.String(), "GitHub said no") {
			t.Errorf("the fetch error leaks under some field: %s", rr.Body)
		}
		channels := body["channels"].(map[string]any)
		if len(channels) != 3 {
			t.Fatalf("channels = %v", channels)
		}
		stable := channels["stable"].(map[string]any)
		if v, ok := stable["latest"]; !ok || v != nil {
			t.Errorf("stable.latest = %v, want null", stable["latest"])
		}
		if rels, ok := stable["releases"].([]any); !ok || len(rels) != 0 {
			t.Errorf("stable.releases = %v, want []", stable["releases"])
		}
		nightly := channels["nightly"].(map[string]any)
		if nightly["latest"] != "nightly-20261023" {
			t.Errorf("nightly.latest = %v", nightly["latest"])
		}
		first := nightly["releases"].([]any)[0].(map[string]any)
		for _, k := range []string{"tag", "publishedAt", "installable", "class", "local", "notesUrl"} {
			if _, ok := first[k]; !ok {
				t.Errorf("release lacks %q: %v", k, first)
			}
		}
		preview := channels["preview"].(map[string]any)["releases"].([]any)
		broken := preview[1].(map[string]any)
		if broken["installable"] != false || broken["reason"] != maintenance.ReasonNoChecksums {
			t.Errorf("non-installable release = %v", broken)
		}
		if _, ok := first["reason"]; ok {
			t.Errorf("an installable release has a reason: %v", first)
		}

		// Exact wire shape of one GitHub release and one local-only release.
		var wire struct {
			Channels map[string]struct {
				Releases []json.RawMessage `json:"releases"`
			} `json:"channels"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		// The handler classes against the test binary's own build, which
		// has no version stamped; take the classes from the same input.
		classes := map[string]string{}
		if resp, err := srv.buildReleasesResponse(context.Background(), currentRunningBuild(), false); err == nil {
			classes = releasesClasses(resp)
		}
		wantGitHub := `{"tag":"nightly-20261023","channel":"nightly","publishedAt":"2026-10-23T04:00:00Z","installable":true,` +
			`"class":"` + classes["nightly-20261023"] + `","local":false,"source":"github","notesUrl":"https://github.com/GoogleCloudPlatform/scion/releases/tag/nightly-20261023"}`
		if got := string(wire.Channels["nightly"].Releases[0]); got != wantGitHub {
			t.Errorf("GitHub release wire =\n%s\nwant\n%s", got, wantGitHub)
		}
		wantLocal := `{"tag":"v0.3.0-preview.1","channel":"preview","installable":true,"class":"` + classes["v0.3.0-preview.1"] + `","local":true,"source":"local"}`
		if got := string(wire.Channels["preview"].Releases[2]); got != wantLocal {
			t.Errorf("local-only release wire =\n%s\nwant\n%s", got, wantLocal)
		}
	})

	t.Run("refresh parameter", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		cat := &fakeReleaseCatalog{snap: releasesTestSnapshot(t)}
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseCatalog: cat, ReleaseSource: &countingReleaseSource{}}
		for _, query := range []string{"?refresh=0", "?refresh=1", "?refresh=true"} {
			if rr := getReleases(t, srv, query); rr.Code != http.StatusOK {
				t.Fatalf("%s: status = %d", query, rr.Code)
			}
		}
		if cat.refreshes != 2 || cat.gets != 3 {
			t.Errorf("gets = %d refreshes = %d, want 3/2", cat.gets, cat.refreshes)
		}
		if rr := getReleases(t, srv, "?refresh=yes"); rr.Code != http.StatusBadRequest {
			t.Errorf("refresh=yes: status = %d, want 400", rr.Code)
		}
	})

	t.Run("no catalog at all is 503", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseCatalog: &fakeReleaseCatalog{err: errors.New("dial tcp: no route")}}
		rr := getReleases(t, srv, "")
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rr.Code)
		}
		if body := rr.Body.String(); strings.Contains(body, "dial tcp") {
			t.Errorf("the fetch error leaks: %s", body)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/releases", nil)
		rr := httptest.NewRecorder()
		srv.handleMaintenanceReleases(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", rr.Code)
		}
	})
}

func TestMemoReleaseSource(t *testing.T) {
	ctx := context.Background()
	now := mustTime(t, "2026-10-27T00:00:00Z")
	src := &countingReleaseSource{rels: map[string]time.Time{"v0.2.0": now}}
	m := &memoReleaseSource{src: src, retry: time.Minute, now: func() time.Time { return now }}

	for i := 0; i < 2; i++ {
		if rel, err := m.ReleaseByTag(ctx, "v0.2.0"); err != nil || rel.Tag != "v0.2.0" {
			t.Fatalf("found release: %v, %v", rel, err)
		}
		if _, err := m.ReleaseByTag(ctx, "v0.1.0"); !errors.Is(err, maintenance.ErrReleaseNotFound) {
			t.Fatalf("missing release: %v", err)
		}
	}
	if n := src.callCount(); n != 2 {
		t.Fatalf("lookups = %d, want 2 (both remembered)", n)
	}
	now = now.Add(2 * time.Minute)
	_, _ = m.ReleaseByTag(ctx, "v0.2.0")
	_, _ = m.ReleaseByTag(ctx, "v0.1.0")
	if n := src.callCount(); n != 3 {
		t.Fatalf("lookups = %d, want 3 (only the failure retried)", n)
	}

	t.Run("a cancelled caller is not remembered", func(t *testing.T) {
		src := &countingReleaseSource{rels: map[string]time.Time{"v0.2.0": now}}
		m := &memoReleaseSource{src: src, retry: time.Hour, now: func() time.Time { return now }}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := m.ReleaseByTag(cancelled, "v0.2.0"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled lookup = %v", err)
		}
		rel, err := m.ReleaseByTag(ctx, "v0.2.0")
		if err != nil || rel == nil || rel.Tag != "v0.2.0" {
			t.Fatalf("lookup after a cancelled one = %v, %v; the cancellation was remembered", rel, err)
		}
		if n := src.callCount(); n != 2 {
			t.Fatalf("lookups = %d, want 2", n)
		}
	})

	t.Run("a deadline error from the source is not remembered", func(t *testing.T) {
		src := &deadlineOnceReleaseSource{}
		m := &memoReleaseSource{src: src, retry: time.Hour, now: func() time.Time { return now }}
		if _, err := m.ReleaseByTag(ctx, "v0.2.0"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first lookup = %v", err)
		}
		if _, err := m.ReleaseByTag(ctx, "v0.2.0"); err != nil {
			t.Fatalf("second lookup = %v; the deadline error was remembered", err)
		}
	})
}

// deadlineOnceReleaseSource fails its first lookup with a deadline error
// (a request timeout inside the source) and finds the release afterwards.
type deadlineOnceReleaseSource struct {
	mu    sync.Mutex
	calls int
}

func (d *deadlineOnceReleaseSource) ReleaseByTag(_ context.Context, tag string) (*maintenance.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls == 1 {
		return nil, context.DeadlineExceeded
	}
	return &maintenance.Release{Tag: tag}, nil
}

func TestHubReleaseCatalogStore(t *testing.T) {
	ctx := context.Background()
	srv, s := newTestServerWithStore(t)
	cs := hubReleaseCatalogStore{s: srv}
	if data, err := cs.LoadReleaseCatalog(ctx); err != nil || data != nil {
		t.Fatalf("empty load = %q, %v", data, err)
	}
	want := `{"version":1,"repo":"GoogleCloudPlatform/scion"}`
	if err := cs.SaveReleaseCatalog(ctx, []byte(want)); err != nil {
		t.Fatal(err)
	}
	data, err := cs.LoadReleaseCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got, exp map[string]any
	_ = json.Unmarshal(data, &got)
	_ = json.Unmarshal([]byte(want), &exp)
	if got["repo"] != exp["repo"] || got["version"] != exp["version"] {
		t.Fatalf("round trip = %s", data)
	}
	if _, err := s.GetHubSetting(ctx, HubSettingSectionReleaseCatalog); err != nil {
		t.Fatalf("not stored in %s: %v", HubSettingSectionReleaseCatalog, err)
	}
}

// catalogCtxKey marks the context a test passes to the catalog store.
type catalogCtxKey struct{}

// catalogCtxStore records whether the hub_settings calls of the catalog
// store received the caller's context, and fails them when it is done.
type catalogCtxStore struct {
	store.Store
	mu   sync.Mutex
	seen []string
}

func (c *catalogCtxStore) note(ctx context.Context, op string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Value(catalogCtxKey{}) == "caller" {
		c.seen = append(c.seen, op)
	}
	return ctx.Err()
}

func (c *catalogCtxStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if err := c.note(ctx, "get"); err != nil {
		return nil, err
	}
	return c.Store.GetHubSetting(ctx, section)
}

func (c *catalogCtxStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	if err := c.note(ctx, "upsert"); err != nil {
		return nil, err
	}
	return c.Store.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
}

// TestHubReleaseCatalogStore_PassesCtx pins that Load and Save hand the
// caller's context to the store, so a cancelled refresh cannot hang in a
// database call (a ctx-ignoring Save would hold the catalog's singleflight).
func TestHubReleaseCatalogStore_PassesCtx(t *testing.T) {
	_, s := newTestServerWithStore(t)
	rec := &catalogCtxStore{Store: s}
	cs := hubReleaseCatalogStore{s: &Server{store: rec}}

	ctx := context.WithValue(context.Background(), catalogCtxKey{}, "caller")
	if err := cs.SaveReleaseCatalog(ctx, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.LoadReleaseCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 2 || rec.seen[0] != "upsert" || rec.seen[1] != "get" {
		t.Fatalf("store calls with the caller's ctx = %v, want [upsert get]", rec.seen)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := cs.SaveReleaseCatalog(cancelled, []byte(`{}`)); !errors.Is(err, context.Canceled) {
		t.Errorf("Save on a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := cs.LoadReleaseCatalog(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("Load on a cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestUpdateReleaseCatalog(t *testing.T) {
	t.Run("configured catalog is used", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		cat := &fakeReleaseCatalog{}
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseCatalog: cat}
		if srv.updateReleaseCatalog() != maintenance.CatalogSource(cat) {
			t.Fatal("the configured catalog was not used")
		}
	})
	t.Run("one shared default catalog", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		a, b := srv.updateReleaseCatalog(), srv.updateReleaseCatalog()
		if a == nil || a != b {
			t.Fatal("the default catalog must be built once and shared")
		}
		if _, ok := a.(*maintenance.Catalog); !ok {
			t.Fatalf("default catalog is %T", a)
		}
	})
}

func getReleaseSelection(t *testing.T, s store.Store) *ReleaseSelection {
	t.Helper()
	setting, err := s.GetHubSetting(context.Background(), HubSettingSectionReleaseSelection)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var sel ReleaseSelection
	if err := json.Unmarshal(setting.Value, &sel); err != nil {
		t.Fatal(err)
	}
	return &sel
}

func putReleaseSelection(t *testing.T, s store.Store, sel ReleaseSelection) {
	t.Helper()
	b, _ := json.Marshal(sel)
	if _, err := s.UpsertHubSetting(context.Background(), HubSettingSectionReleaseSelection, b, "test", -1, "managed"); err != nil {
		t.Fatal(err)
	}
}

func TestReconcilePendingUpdate_ReleaseSelection(t *testing.T) {
	ctx := context.Background()
	pending := func(runID string) PendingUpdate {
		return PendingUpdate{RunID: runID, From: "nightly-20261023", To: "v0.3.0-preview.4", Strategy: "symlink",
			At: "2026-10-24T03:00:00Z", Channel: "preview", Pin: "v0.3.0-preview.4", SetBy: "admin@example.com"}
	}

	t.Run("verified writes the selection", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pending(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "v0.3.0-preview.4"); err != nil {
			t.Fatal(err)
		}
		sel := getReleaseSelection(t, s)
		if sel == nil {
			t.Fatal("no release selection written")
		}
		if sel.Mode != UpdateModeRelease || sel.Channel != "preview" || sel.Pin != "v0.3.0-preview.4" ||
			sel.Version != "v0.3.0-preview.4" || sel.SetBy != "admin@example.com" || sel.RunID != run.ID {
			t.Errorf("selection = %+v", sel)
		}
		if _, err := time.Parse(time.RFC3339, sel.SetAt); err != nil {
			t.Errorf("set_at = %q", sel.SetAt)
		}
		var raw map[string]any
		setting, _ := s.GetHubSetting(ctx, HubSettingSectionReleaseSelection)
		_ = json.Unmarshal(setting.Value, &raw)
		for _, k := range []string{"mode", "channel", "pin", "version", "set_by", "set_at", "run_id"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("selection JSON lacks %q: %s", k, setting.Value)
			}
		}
	})

	t.Run("failed keeps the previous selection", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		prev := ReleaseSelection{Mode: UpdateModeRelease, Channel: "nightly", Pin: maintenance.PinLatest, Version: "nightly-20261023"}
		putReleaseSelection(t, s, prev)
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pending(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		if sel := getReleaseSelection(t, s); sel == nil || *sel != prev {
			t.Errorf("selection = %+v, want unchanged %+v", sel, prev)
		}
	})

	t.Run("a pre-Phase-2 record writes no selection", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		run := newRestartingRun(t, s)
		// The row as a Phase 1 hub wrote it: no channel, pin or setBy.
		b := []byte(`{"runId":"` + run.ID + `","from":"nightly-20261023","to":"nightly-20261024","snapshot":"","strategy":"symlink","at":"2026-10-24T03:00:00Z"}`)
		if _, err := s.UpsertHubSetting(ctx, HubSettingSectionPendingUpdate, b, "test", -1, "managed"); err != nil {
			t.Fatal(err)
		}
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last == nil || last.Status != LastUpdateVerified {
			t.Fatalf("lastUpdate = %+v", last)
		}
		if sel := getReleaseSelection(t, s); sel != nil {
			t.Errorf("selection = %+v, want none", sel)
		}
		requireNoPendingUpdate(t, s)
	})

	t.Run("a missing pin is latest", func(t *testing.T) {
		p := pending("r1")
		p.Pin = ""
		sel, skip := selectionFromPending(p, time.Now())
		if skip != "" || sel.Pin != maintenance.PinLatest {
			t.Errorf("selection = %+v, skip %q", sel, skip)
		}
	})

	t.Run("an auto-update records no selection", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", ReleaseChannel: "nightly"}
		run := newRestartingRun(t, s)
		p := pending(run.ID)
		p.To, p.Channel, p.Pin, p.SetBy = "nightly-20261024", "nightly", maintenance.PinLatest, autoUpdateStartedBy
		putPendingUpdate(t, s, p)
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last == nil || last.Status != LastUpdateVerified {
			t.Fatalf("lastUpdate = %+v", last)
		}
		if sel := getReleaseSelection(t, s); sel != nil {
			t.Fatalf("an auto-update wrote a selection: %+v", sel)
		}
		// The operator's later config edit takes effect.
		srv.config.MaintenanceConfig.ReleaseChannel = "preview"
		st, err := srv.buildUpdateStatus(ctx, runningBuild{Version: "nightly-20261024"})
		if err != nil {
			t.Fatal(err)
		}
		if st.Channel != (updateStatusChannel{Effective: "preview", Configured: "preview", Source: ChannelSourceConfig}) {
			t.Errorf("channel after a config edit = %+v", st.Channel)
		}
	})

	t.Run("an auto-update leaves an operator selection unchanged", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		prev := ReleaseSelection{Mode: UpdateModeRelease, Channel: "nightly", Pin: maintenance.PinLatest,
			Version: "nightly-20261023", SetBy: "admin@example.com", SetAt: "2026-10-23T05:00:00Z", RunID: "r0"}
		putReleaseSelection(t, s, prev)
		run := newRestartingRun(t, s)
		p := pending(run.ID)
		p.To, p.Channel, p.Pin, p.SetBy = "nightly-20261024", "nightly", maintenance.PinLatest, autoUpdateStartedBy
		putPendingUpdate(t, s, p)
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		if sel := getReleaseSelection(t, s); sel == nil || *sel != prev {
			t.Errorf("selection = %+v, want unchanged %+v", sel, prev)
		}
	})

	t.Run("skip reasons", func(t *testing.T) {
		p := pending("r1")
		p.SetBy = autoUpdateStartedBy
		if _, skip := selectionFromPending(p, time.Now()); skip == "" {
			t.Error("an auto-update row must be skipped")
		}
		p = pending("r1")
		p.Channel = ""
		if _, skip := selectionFromPending(p, time.Now()); skip == "" {
			t.Error("a row without a channel must be skipped")
		}
	})
}

func TestMarkRunRestarting_RecordsSelectionInputs(t *testing.T) {
	ctx := context.Background()
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	p := maintenance.PendingRestart{From: "nightly-20261023", To: "v0.3.0-preview.4", Strategy: "symlink",
		Channel: "preview", Pin: maintenance.PinLatest}
	if err := srv.markRunRestarting(ctx, run, &runLogBuffer{}, p); err != nil {
		t.Fatal(err)
	}
	var got PendingUpdate
	found, err := srv.readHubSettingJSON(ctx, HubSettingSectionPendingUpdate, &got)
	if err != nil || !found {
		t.Fatalf("pending_update: found=%v err=%v", found, err)
	}
	if got.Channel != "preview" || got.Pin != maintenance.PinLatest || got.SetBy != "admin@example.com" || got.RunID != run.ID {
		t.Errorf("pending_update = %+v", got)
	}
}

// TestUpdateRun_SelectionAfterReboot follows an update from the
// before-restart hook through boot reconciliation to update-status: the
// operator's channel and pin take effect once the new version runs.
func TestUpdateRun_SelectionAfterReboot(t *testing.T) {
	ctx := context.Background()
	srv, s := newTestServerWithStore(t)
	srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", ReleaseChannel: "nightly"}
	run := newRun(t, s)
	p := maintenance.PendingRestart{From: "nightly-20261023", To: "v0.3.0-preview.4", Strategy: "symlink",
		Channel: "preview", Pin: "v0.3.0-preview.4"}
	if err := srv.markRunRestarting(ctx, run, &runLogBuffer{}, p); err != nil {
		t.Fatal(err)
	}

	// Before the reboot the configured channel still applies.
	st, err := srv.buildUpdateStatus(ctx, runningBuild{Version: "nightly-20261023"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Channel.Source != ChannelSourceConfig || st.Pin != maintenance.PinLatest {
		t.Fatalf("before reboot: channel = %+v, pin = %q", st.Channel, st.Pin)
	}

	if err := srv.reconcilePendingUpdate(ctx, "v0.3.0-preview.4"); err != nil {
		t.Fatal(err)
	}
	st, err = srv.buildUpdateStatus(ctx, runningBuild{Version: "v0.3.0-preview.4"})
	if err != nil {
		t.Fatal(err)
	}
	want := updateStatusChannel{Effective: "preview", Configured: "nightly", Source: ChannelSourceSelection}
	if st.Channel != want || st.Pin != "v0.3.0-preview.4" || st.Mode.Effective != UpdateModeRelease {
		t.Errorf("after reboot: channel = %+v, pin = %q, mode = %q", st.Channel, st.Pin, st.Mode.Effective)
	}
}

func TestBuildUpdateStatus_ReleaseSelection(t *testing.T) {
	ctx := context.Background()
	release := runningBuild{Version: "nightly-20261023"}

	t.Run("selection beats config and detection; mode from the selection", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "source", ReleaseChannel: "nightly"}
		putReleaseSelection(t, s, ReleaseSelection{Mode: UpdateModeRelease, Channel: "stable", Pin: "v1.0.0", Version: "v1.0.0"})
		st, err := srv.buildUpdateStatus(ctx, release)
		if err != nil {
			t.Fatal(err)
		}
		if st.Channel != (updateStatusChannel{Effective: "stable", Configured: "nightly", Source: ChannelSourceSelection}) {
			t.Errorf("channel = %+v", st.Channel)
		}
		if st.Pin != "v1.0.0" || st.Mode.Effective != UpdateModeRelease {
			t.Errorf("pin = %q, mode = %q", st.Pin, st.Mode.Effective)
		}
	})

	t.Run("an unreadable selection is ignored", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		if _, err := s.UpsertHubSetting(ctx, HubSettingSectionReleaseSelection, json.RawMessage(`"not an object"`), "test", -1, "managed"); err != nil {
			t.Fatal(err)
		}
		st, err := srv.buildUpdateStatus(ctx, release)
		if err != nil {
			t.Fatal(err)
		}
		if st.Channel.Source != ChannelSourceDetected || st.Pin != maintenance.PinLatest {
			t.Errorf("channel = %+v, pin = %q", st.Channel, st.Pin)
		}
	})
}

func TestEffectiveReleaseChannel(t *testing.T) {
	tests := []struct {
		name        string
		configured  string
		running     string
		sel         *ReleaseSelection
		wantChannel string
		wantSource  string
	}{
		{"selection", "nightly", "nightly-20261023", &ReleaseSelection{Channel: "preview"}, "preview", ChannelSourceSelection},
		{"selection without channel", "nightly", "v0.3.0-preview.4", &ReleaseSelection{}, "nightly", ChannelSourceConfig},
		{"config", "stable", "nightly-20261023", nil, "stable", ChannelSourceConfig},
		{"detected", "", "v0.3.0-preview.4", nil, "preview", ChannelSourceDetected},
		{"git build", "", "", nil, "", ChannelSourceDetected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, src := effectiveReleaseChannel(MaintenanceConfig{ReleaseChannel: tt.configured}, tt.running, tt.sel)
			if ch != tt.wantChannel || src != tt.wantSource {
				t.Errorf("got %q/%q, want %q/%q", ch, src, tt.wantChannel, tt.wantSource)
			}
		})
	}
	if effectivePin(nil) != maintenance.PinLatest || effectivePin(&ReleaseSelection{Pin: "v1.0.0"}) != "v1.0.0" {
		t.Error("effectivePin")
	}
	if effectiveUpdateMode(MaintenanceConfig{DeploymentTier: "source"}, &ReleaseSelection{Mode: UpdateModeRelease}) != UpdateModeRelease ||
		effectiveUpdateMode(MaintenanceConfig{DeploymentTier: "binary"}, &ReleaseSelection{Mode: UpdateModeGit}) != UpdateModeGit ||
		effectiveUpdateMode(MaintenanceConfig{DeploymentTier: "source"}, nil) != UpdateModeGit {
		t.Error("effectiveUpdateMode")
	}
}

func TestRunReleaseUpdateCheck_ReleaseSelection(t *testing.T) {
	ctx := context.Background()

	t.Run("checks the selected channel through the shared catalog", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		cat := &fakeReleaseCatalog{}
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "notify",
			ReleaseChannel: "nightly", ReleaseCatalog: cat}
		putReleaseSelection(t, s, ReleaseSelection{Mode: UpdateModeRelease, Channel: "preview", Pin: maintenance.PinLatest})
		p := &releaseCheckProbe{checkResult: releaseCheckResult(false)}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261023"))
		if p.gotOpts.Channel != "preview" {
			t.Errorf("checked channel %q, want the selection's preview", p.gotOpts.Channel)
		}
		if p.gotOpts.Catalog != maintenance.CatalogSource(cat) || p.gotOpts.Releases == nil {
			t.Errorf("check options = %+v, want the hub's catalog and release source", p.gotOpts)
		}
	})

	t.Run("pinned hub under auto is only notified", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategySymlink)}
		putReleaseSelection(t, s, ReleaseSelection{Mode: UpdateModeRelease, Channel: "nightly", Pin: "nightly-20261020"})
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261020"))
		if p.resolved != 0 {
			t.Fatal("the update executor was resolved for a pinned hub")
		}
		if runs, _ := s.ListMaintenanceRuns(ctx, "update-binary", 5); len(runs) != 0 {
			t.Fatalf("a pinned hub was auto-updated: %+v", runs)
		}
		if _, err := s.GetHubSetting(ctx, HubSettingSectionUpdateAvailable); err != nil {
			t.Fatalf("the update must still be notified: %v", err)
		}
	})

	t.Run("auto follows the selected channel", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategySymlink)}
		putReleaseSelection(t, s, ReleaseSelection{Mode: UpdateModeRelease, Channel: "preview", Pin: maintenance.PinLatest})
		exec := newParamRecordingExecutor()
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true), executor: exec}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261020"))
		select {
		case <-exec.ran:
		case <-time.After(5 * time.Second):
			t.Fatal("auto update executor never ran")
		}
		exec.mu.Lock()
		defer exec.mu.Unlock()
		if exec.params[maintenance.ParamTargetVersion] != "latest:preview" {
			t.Errorf("params = %v, want latest:preview", exec.params)
		}
	})

	t.Run("git mode from the selection is skipped", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto"}
		putReleaseSelection(t, s, ReleaseSelection{Mode: UpdateModeGit})
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261020"))
		if p.checks != 0 {
			t.Fatal("a hub whose selection is git mode must not be checked")
		}
	})
}
