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

// f25Manifest is LATEST.json as it was for the findings' §2.5 reproduction.
const f25Manifest = `{"channels":{
	"stable":{"version":"","date":"","url":""},
	"preview":{"version":"v0.3.0-preview.4","date":"2026-10-24T00:21:09Z","url":"https://github.com/GoogleCloudPlatform/scion/releases/tag/v0.3.0-preview.4"},
	"nightly":{"version":"nightly-20261023","date":"2026-10-23T02:10:00Z","url":"https://github.com/GoogleCloudPlatform/scion/releases/tag/nightly-20261023"}}}`

func newCheckGitHub(t *testing.T, manifest string) (*fakeGitHub, ReleaseCheckOptions) {
	t.Helper()
	gh := newFakeGitHub(t)
	gh.latest = manifest
	client := &http.Client{Transport: gh}
	return gh, ReleaseCheckOptions{
		Repo:       testRepo,
		HTTPClient: client,
		Releases:   &GitHubReleases{Repo: testRepo, Client: client},
	}
}

// TestCheckForReleaseUpdates_F25Table covers the six rows of findings §2.5
// (the checker re-detected the channel from the running version, so an
// up-to-date nightly hub configured for preview was never offered preview),
// plus the edges of the cross-channel date comparison.
func TestCheckForReleaseUpdates_F25Table(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		channel   string // effective channel
		releases  map[string]string
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
		// F§2.5 row 4: preview.3's publish date comes from the release API.
		{name: "older preview, nightly channel", current: "v0.3.0-preview.3", channel: "nightly",
			releases:  map[string]string{"v0.3.0-preview.3": "2026-10-15T10:00:00Z"},
			wantAvail: true, wantLatst: "nightly-20261023", wantClass: "switch-newer"},
		// F§2.5 row 5: stable is empty.
		{name: "preview, empty stable channel", current: "v0.3.0-preview.4", channel: "stable",
			wantAvail: false, wantLatst: "", wantClass: ""},
		// F§2.5 row 6: a git build with no channel is not checked.
		{name: "git build, no channel", current: "", channel: "",
			wantAvail: false, wantLatst: "", wantClass: ""},

		{name: "same nightly", current: "nightly-20261023", channel: "nightly",
			wantAvail: false, wantLatst: "nightly-20261023", wantClass: "same"},
		{name: "newer nightly than the manifest", current: "nightly-20261024", channel: "nightly",
			wantAvail: false, wantLatst: "nightly-20261023", wantClass: "downgrade"},
		{name: "nightly published after the latest preview", current: "nightly-20261024", channel: "preview",
			releases:  map[string]string{"nightly-20261024": "2026-10-24T02:08:49Z"},
			wantAvail: false, wantLatst: "v0.3.0-preview.4", wantClass: "switch-older"},
		{name: "preview with no publish date known", current: "v0.3.0-preview.3", channel: "nightly",
			wantAvail: false, wantLatst: "nightly-20261023", wantClass: "unknown"},
		{name: "git build with build time, preview channel", current: "", channel: "preview",
			wantAvail: true, wantLatst: "v0.3.0-preview.4", wantClass: "switch-newer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh, opts := newCheckGitHub(t, f25Manifest)
			for tag, published := range tt.releases {
				gh.setRelease(tag, published, "", "")
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

// TestCheckForReleaseUpdates_ReleaseLookup checks the publish-date lookup of
// the running release through the GitHub API: it is made only for a
// cross-channel comparison whose running release is not the manifest entry.
func TestCheckForReleaseUpdates_ReleaseLookup(t *testing.T) {
	t.Run("cross channel looks up the running release", func(t *testing.T) {
		gh, opts := newCheckGitHub(t, f25Manifest)
		gh.setRelease("v0.3.0-preview.3", "2026-10-15T10:00:00Z", "", "")
		opts.CurrentVersion, opts.Channel = "v0.3.0-preview.3", "nightly"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if !gh.requested("api.github.com/repos/" + testRepo + "/releases/tags/v0.3.0-preview.3") {
			t.Errorf("expected a single-tag lookup of the running release, got %v", gh.requests)
		}
	})
	t.Run("running release named by the manifest needs no lookup", func(t *testing.T) {
		gh, opts := newCheckGitHub(t, f25Manifest)
		opts.CurrentVersion, opts.Channel = "nightly-20261023", "preview"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if gh.requested("api.github.com") {
			t.Errorf("unexpected API request: %v", gh.requests)
		}
	})
	t.Run("same channel needs no lookup", func(t *testing.T) {
		gh, opts := newCheckGitHub(t, f25Manifest)
		opts.CurrentVersion, opts.Channel = "nightly-20261020", "nightly"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if gh.requested("api.github.com") {
			t.Errorf("unexpected API request: %v", gh.requests)
		}
	})
	t.Run("manifest entry without a date is looked up", func(t *testing.T) {
		gh, opts := newCheckGitHub(t, strings.Replace(f25Manifest, `"date":"2026-10-24T00:21:09Z"`, `"date":""`, 1))
		gh.setRelease("v0.3.0-preview.4", "2026-10-24T00:21:09Z", "", "")
		opts.CurrentVersion, opts.Channel = "nightly-20261023", "preview"
		got, err := CheckForReleaseUpdates(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if !got.UpdateAvailable || got.Class != "switch-newer" {
			t.Errorf("got %+v, want switch-newer available", got)
		}
	})
}

func TestCheckForReleaseUpdates_Errors(t *testing.T) {
	t.Run("manifest fetch failure", func(t *testing.T) {
		_, opts := newCheckGitHub(t, "not json")
		opts.CurrentVersion, opts.Channel = "nightly-20261020", "nightly"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err == nil {
			t.Fatal("expected an error for an unreadable manifest")
		}
	})
	t.Run("manifest entry from another channel", func(t *testing.T) {
		_, opts := newCheckGitHub(t, `{"channels":{"preview":{"version":"nightly-20261023","date":"2026-10-23T02:10:00Z"}}}`)
		opts.CurrentVersion, opts.Channel = "v0.3.0-preview.3", "preview"
		if _, err := CheckForReleaseUpdates(context.Background(), opts); err == nil {
			t.Fatal("expected an error for a manifest entry that is not a preview tag")
		}
	})
}

// TestReleaseUpdateCheckResult_NoDownloadURLs pins that the check result no
// longer carries asset URLs: update-binary resolves and pins them itself and
// refuses download_url/checksums_url.
func TestReleaseUpdateCheckResult_NoDownloadURLs(t *testing.T) {
	_, opts := newCheckGitHub(t, f25Manifest)
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
