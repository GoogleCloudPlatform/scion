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
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// ReleaseUpdateCheckResult is the result of a release update check for the
// effective channel.
type ReleaseUpdateCheckResult struct {
	Tier            string `json:"tier"`
	UpdateAvailable bool   `json:"update_available"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	Channel         string `json:"channel"`
	// Class is how LatestVersion relates to CurrentVersion (update.Class);
	// empty when there is no latest release to compare against.
	Class      string `json:"class,omitempty"`
	ReleaseURL string `json:"release_url,omitempty"`
}

// ReleaseCheckOptions configures CheckForReleaseUpdates.
type ReleaseCheckOptions struct {
	// CurrentVersion is the running version ("" for a git build).
	CurrentVersion string
	// CurrentBuildTime dates a running git build; zero when unknown.
	CurrentBuildTime time.Time
	// Channel is the effective channel to check (configured release_channel,
	// else the running version's channel). Empty means no check.
	Channel string
	// Repo is the release repository (owner/name); default DefaultGitHubRepo.
	Repo string
	// ManifestURL is LATEST.json; default
	// https://raw.githubusercontent.com/<Repo>/main/LATEST.json.
	ManifestURL string
	// HTTPClient fetches LATEST.json; default the update package's client.
	HTTPClient *http.Client
	// Releases looks up publish dates for cross-channel comparisons; default
	// GitHub's API for Repo (through HTTPClient when set).
	Releases ReleaseSource
}

// CheckForReleaseUpdates finds the latest release of the effective channel
// (from LATEST.json) and reports an update when it is an upgrade (same
// channel, newer) or a switch to a release published after the running one
// (update.ClassSwitchNewer).
//
// It compares against opts.Channel only. It does not re-detect the channel
// from the running version, so a nightly hub configured for preview is
// offered the latest preview once that preview is newer than the nightly.
// A cross-channel comparison uses publish dates: the manifest's date for the
// latest release and a single-tag lookup for the running release (nightly
// tags carry their own date when the lookup fails). Without a date the
// order is unknown and no update is reported.
func CheckForReleaseUpdates(ctx context.Context, opts ReleaseCheckOptions) (*ReleaseUpdateCheckResult, error) {
	log := logging.Subsystem("hub.maintenance.check-release-updates")

	repo := opts.Repo
	if repo == "" {
		repo = DefaultGitHubRepo
	}
	result := &ReleaseUpdateCheckResult{
		Tier:           "binary",
		CurrentVersion: opts.CurrentVersion,
		Channel:        opts.Channel,
	}
	if opts.Channel == "" {
		log.Debug("No release channel, skipping update check", "version", opts.CurrentVersion)
		return result, nil
	}

	manifestURL := opts.ManifestURL
	if manifestURL == "" {
		manifestURL = fmt.Sprintf("https://raw.githubusercontent.com/%s/main/LATEST.json", repo)
	}
	mopts := []update.Option{update.WithManifestURL(manifestURL)}
	if opts.HTTPClient != nil {
		mopts = append(mopts, update.WithHTTPClient(opts.HTTPClient))
	}
	manifest, err := update.FetchManifest(ctx, mopts...)
	if err != nil {
		return nil, fmt.Errorf("fetch release manifest: %w", err)
	}

	latest := manifest.Channels[opts.Channel]
	if latest.Version == "" {
		log.Debug("No release published for channel", "channel", opts.Channel)
		return result, nil
	}
	if !update.IsReleaseTag(latest.Version) || update.DetectChannel(latest.Version) != opts.Channel {
		return nil, fmt.Errorf("release manifest lists %q as latest %s, which is not a %s release tag",
			latest.Version, opts.Channel, opts.Channel)
	}
	result.LatestVersion = latest.Version
	result.ReleaseURL = latest.URL

	releases := opts.Releases
	if releases == nil {
		gh := NewGitHubReleases(repo)
		if opts.HTTPClient != nil {
			c := *opts.HTTPClient
			c.Timeout = releaseAPITimeout
			gh.Client = &c
		}
		releases = gh
	}
	class := update.CompareRefs(
		currentRef(ctx, releases, opts, manifest),
		update.Ref{Version: latest.Version, Date: manifestDate(ctx, releases, latest)},
	)
	result.Class = string(class)
	result.UpdateAvailable = class == update.ClassUpgrade || class == update.ClassSwitchNewer

	log.Info("Release update check complete",
		"current", opts.CurrentVersion,
		"latest", result.LatestVersion,
		"channel", opts.Channel,
		"class", class,
		"update_available", result.UpdateAvailable)
	return result, nil
}

// currentRef dates the running version for a comparison against the
// channel's latest release. A same-channel comparison needs no date. Across
// channels a release is dated by its publish date (a manifest entry naming
// it, else a single-tag lookup); a git build by its build time.
func currentRef(ctx context.Context, releases ReleaseSource, opts ReleaseCheckOptions, m *update.Manifest) update.Ref {
	cur := update.Ref{Version: opts.CurrentVersion}
	if !update.IsReleaseTag(cur.Version) {
		cur.Date = opts.CurrentBuildTime
		return cur
	}
	ch := update.DetectChannel(cur.Version)
	if ch == opts.Channel {
		return cur
	}
	if entry := m.Channels[ch]; entry.Version == cur.Version {
		if t, err := time.Parse(time.RFC3339, entry.Date); err == nil {
			cur.Date = t
			return cur
		}
	}
	if rel, err := releases.ReleaseByTag(ctx, cur.Version); err == nil && !rel.PublishedAt.IsZero() {
		cur.Date = rel.PublishedAt
	}
	return cur
}

// manifestDate returns the publish date of a manifest entry, looking the
// release up when the manifest has no usable date.
func manifestDate(ctx context.Context, releases ReleaseSource, entry update.ChannelInfo) time.Time {
	if t, err := time.Parse(time.RFC3339, entry.Date); err == nil {
		return t
	}
	if rel, err := releases.ReleaseByTag(ctx, entry.Version); err == nil {
		return rel.PublishedAt
	}
	return time.Time{}
}
