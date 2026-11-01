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
	"net/http"
	"strings"
	"testing"
	"time"
)

// newCheckGitHub returns a fake GitHub that publishes the releases of the
// findings' §2.5 reproduction (the latest nightly and preview.4; stable is
// empty) and check options that read it through the release catalog.
func newCheckGitHub(t *testing.T) (*fakeGitHub, ReleaseCheckOptions) {
	t.Helper()
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T02:10:00Z", "nightly-20261023")
	gh.addRelease("v0.3.0-preview.4", "2026-10-24T00:21:09Z", "v0.3.0-preview.4")
	client := &http.Client{Transport: gh}
	return gh, ReleaseCheckOptions{
		Repo:     testRepo,
		Releases: &GitHubReleases{Repo: testRepo, Client: client},
	}
}

// TestCheckForReleaseUpdates_F25Table covers the six rows of findings §2.5
// (the checker re-detected the channel from the running version, so an
// up-to-date nightly hub configured for preview was never offered preview),
// plus the edges of the cross-channel date comparison, with the latest
// release read from the release catalog.
func TestCheckForReleaseUpdates_F25Table(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		channel   string            // effective channel
		listed    map[string]string // extra listed releases (tag → published), not installable
		unlisted  map[string]string // releases only a single-tag lookup finds
		wantAvail bool
		wantLatst string
		wantClass string
	}{
		// F§2.5 row 1.
		{name: "older nightly, nightly channel", current: "nightly-20261020", channel: "nightly",
			wantAvail: true, wantLatst: "nightly-20261023", wantClass: "upgrade"},
		// F§2.5 row 2.
		{name: "older nightly, preview channel", current: "nightly-20261020", channel: "preview",
			wantAvail: true, wantLatst: "v0.3.0-preview.4", wantClass: "switch-newer"},
		// F§2.5 row 3: the bug. An up-to-date nightly is offered the newer preview.
		{name: "latest nightly, preview channel (nightly to preview)", current: "nightly-20261023", channel: "preview",
			wantAvail: true, wantLatst: "v0.3.0-preview.4", wantClass: "switch-newer"},
		// F§2.5 row 4: preview.3's publish date comes from the catalog.
		{name: "older preview, nightly channel", current: "v0.3.0-preview.3", channel: "nightly",
			listed:    map[string]string{"v0.3.0-preview.3": "2026-10-15T10:00:00Z"},
			wantAvail: true, wantLatst: "nightly-20261023", wantClass: "switch-newer"},
		// F§2.5 row 5: stable is empty.
		{name: "preview, empty stable channel", current: "v0.3.0-preview.4", channel: "stable",
			wantAvail: false, wantLatst: "", wantClass: ""},
		// F§2.5 row 6: a git build with no channel is not checked.
		{name: "git build, no channel", current: "", channel: "",
			wantAvail: false, wantLatst: "", wantClass: ""},

		{name: "same nightly", current: "nightly-20261023", channel: "nightly",
			wantAvail: false, wantLatst: "nightly-20261023", wantClass: "same"},
		{name: "newer nightly than the catalog's latest", current: "nightly-20261024", channel: "nightly",
			wantAvail: false, wantLatst: "nightly-20261023", wantClass: "downgrade"},
		{name: "nightly published after the latest preview", current: "nightly-20261024", channel: "preview",
			listed:    map[string]string{"nightly-20261024": "2026-10-24T02:08:49Z"},
			wantAvail: false, wantLatst: "v0.3.0-preview.4", wantClass: "switch-older"},
		{name: "preview past the catalog, dated by a single-tag lookup", current: "v0.3.0-preview.1", channel: "nightly",
			unlisted:  map[string]string{"v0.3.0-preview.1": "2026-09-01T10:00:00Z"},
			wantAvail: true, wantLatst: "nightly-20261023", wantClass: "switch-newer"},
		{name: "preview with no publish date known", current: "v0.3.0-preview.3", channel: "nightly",
			wantAvail: false, wantLatst: "nightly-20261023", wantClass: "unknown"},
		{name: "git build with build time, preview channel", current: "", channel: "preview",
			wantAvail: true, wantLatst: "v0.3.0-preview.4", wantClass: "switch-newer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh, opts := newCheckGitHub(t)
			for tag, published := range tt.listed {
				gh.setRelease(tag, published, "", "")
			}
			for tag, published := range tt.unlisted {
				gh.setRelease(tag, published, "", "")
				gh.unlisted[tag] = true
			}
			opts.CurrentVersion = tt.current
			opts.Channel = tt.channel
			if tt.current == "" && tt.channel != "" {
				opts.CurrentBuildTime = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
			}

			got, err := CheckForReleaseUpdates(context.Background(), opts)
			if err != nil {
				t.Fatalf("CheckForReleaseUpdates: %v", err)
			}
			if got.UpdateAvailable != tt.wantAvail || got.LatestVersion != tt.wantLatst || got.Class != tt.wantClass {
				t.Errorf("got available=%v latest=%q class=%q, want available=%v latest=%q class=%q",
					got.UpdateAvailable, got.LatestVersion, got.Class, tt.wantAvail, tt.wantLatst, tt.wantClass)
			}
			if got.Channel != tt.channel || got.CurrentVersion != tt.current || got.Tier != "binary" {
				t.Errorf("result identity = %+v", got)
			}
			if tt.channel == "" && gh.requestCount() != 0 {
				t.Errorf("no channel must make no request, got %d", gh.requestCount())
			}
		})
	}
}

// TestCheckForReleaseUpdates_ReleaseLookup checks where the running
// release's publish date comes from: the catalog when it lists the release,
// a single-tag lookup otherwise, and nothing for a same-channel check.
func TestCheckForReleaseUpdates_ReleaseLookup(t *testing.T) {
	tagLookup := "api.github.com/repos/" + testRepo + "/releases/tags/"

	t.Run("cross channel, running release not listed: single-tag lookup", func(t *testing.T) {
		gh, opts := newCheckGitHub(t)
		gh.setRelease("v0.3.0-preview.1", "2026-09-01T10:00:00Z", "", "")
		gh.unlisted["v0.3.0-preview.1"] = true
		opts.CurrentVersion, opts.Channel = "v0.3.0-preview.1", "nightly"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if !gh.requested(tagLookup + "v0.3.0-preview.1") {
			t.Errorf("expected a single-tag lookup of the running release, got %v", gh.requests)
		}
	})
	t.Run("running release listed in the catalog needs no lookup", func(t *testing.T) {
		gh, opts := newCheckGitHub(t)
		opts.CurrentVersion, opts.Channel = "nightly-20261023", "preview"
		got, err := CheckForReleaseUpdates(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if gh.requested(tagLookup) {
			t.Errorf("unexpected single-tag lookup: %v", gh.requests)
		}
		if got.Class != "switch-newer" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("same channel needs no lookup", func(t *testing.T) {
		gh, opts := newCheckGitHub(t)
		opts.CurrentVersion, opts.Channel = "nightly-20261020", "nightly"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if gh.requested(tagLookup) {
			t.Errorf("unexpected single-tag lookup: %v", gh.requests)
		}
	})
}

// TestCheckForReleaseUpdates_Catalog checks the catalog-specific behaviour:
// latest is the newest installable release, a shared catalog is reused, and
// a catalog that cannot be read at all is an error.
func TestCheckForReleaseUpdates_Catalog(t *testing.T) {
	t.Run("latest is the newest installable release", func(t *testing.T) {
		gh, opts := newCheckGitHub(t)
		// A newer nightly without a checksum line for the tarball.
		gh.addRelease("nightly-20261024", "2026-10-24T02:08:49Z", "nightly-20261024")
		gh.files[downloadPath("nightly-20261024", ChecksumsAssetName)] = []byte(emptySHA256 + "  SHA256SUMS\n")
		opts.CurrentVersion, opts.Channel = "nightly-20261020", "nightly"
		got, err := CheckForReleaseUpdates(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if got.LatestVersion != "nightly-20261023" || !got.UpdateAvailable {
			t.Errorf("got %+v, want the installable nightly-20261023", got)
		}
	})
	t.Run("a shared catalog is reused across checks", func(t *testing.T) {
		gh, opts := newCheckGitHub(t)
		opts.Catalog = NewCatalog(CatalogOptions{Repo: testRepo, Lister: opts.Releases.(*GitHubReleases)})
		opts.CurrentVersion, opts.Channel = "nightly-20261020", "nightly"
		for i := 0; i < 2; i++ {
			if _, err := CheckForReleaseUpdates(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
		}
		lists := 0
		for _, r := range gh.requests {
			if strings.Contains(r, "/repos/"+testRepo+"/releases?") {
				lists++
			}
		}
		if lists != 1 {
			t.Errorf("release list fetched %d times, want 1 (cached)", lists)
		}
	})
	t.Run("catalog unavailable", func(t *testing.T) {
		gh, opts := newCheckGitHub(t)
		gh.listStatus = http.StatusInternalServerError
		opts.CurrentVersion, opts.Channel = "nightly-20261020", "nightly"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err == nil {
			t.Fatal("expected an error when the release list cannot be read")
		}
	})
	t.Run("LATEST.json is not read", func(t *testing.T) {
		gh, opts := newCheckGitHub(t)
		opts.CurrentVersion, opts.Channel = "nightly-20261020", "nightly"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if gh.requested("LATEST.json") || gh.requested("raw.githubusercontent.com") {
			t.Errorf("the checker read LATEST.json: %v", gh.requests)
		}
	})
}

// TestReleaseUpdateCheckResult_NoDownloadURLs pins that the check result no
// longer carries asset URLs: update-binary resolves and pins them itself and
// refuses download_url/checksums_url.
func TestReleaseUpdateCheckResult_NoDownloadURLs(t *testing.T) {
	_, opts := newCheckGitHub(t)
	opts.CurrentVersion, opts.Channel = "nightly-20261020", "nightly"
	got, err := CheckForReleaseUpdates(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	for _, k := range []string{"download_url", "checksums_url"} {
		if strings.Contains(string(b), k) {
			t.Errorf("result JSON has %s: %s", k, b)
		}
	}
}
