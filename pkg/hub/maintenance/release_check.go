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

// CatalogSource is the release catalog as the checker and the update
// executor read it. *Catalog implements it; the hub passes its one shared
// Catalog so every reader uses the same cache and fetch budget.
type CatalogSource interface {
	Get(ctx context.Context, refresh bool) (Snapshot, error)
}

// ReleaseCheckOptions configures CheckForReleaseUpdates.
type ReleaseCheckOptions struct {
	// CurrentVersion is the running version ("" for a git build).
	CurrentVersion string
	// CurrentBuildTime dates a running git build; zero when unknown.
	CurrentBuildTime time.Time
	// Channel is the effective channel to check (the hub's release
	// selection, else the configured release_channel, else the running
	// version's channel). Empty means no check.
	Channel string
	// Repo is the release repository (owner/name); default DefaultGitHubRepo.
	Repo string
	// Catalog lists the channel's releases. Default: a new Catalog for Repo
	// over Releases (no cache survives the call; callers that check
	// repeatedly pass a shared one).
	Catalog CatalogSource
	// Refresh asks the catalog for a fresh list (still throttled by the
	// catalog's refresh interval), as a manual check does.
	Refresh bool
	// Releases dates a running release that the catalog does not list, for
	// a cross-channel comparison; default GitHub's API for Repo.
	Releases ReleaseSource
}

// CheckForReleaseUpdates finds the latest installable release of the
// effective channel in the release catalog and reports an update when it is
// an upgrade (same channel, newer) or a switch to a release published after
// the running one (update.ClassSwitchNewer).
//
// It compares against opts.Channel only. It does not re-detect the channel
// from the running version, so a nightly hub configured for preview is
// offered the latest preview once that preview is newer than the nightly.
// A cross-channel comparison uses publish dates (see RunningRef). Without a
// date the order is unknown and no update is reported. A stale catalog (the
// last fetch failed) is still used: its releases did not change.
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

	releases := opts.Releases
	if releases == nil {
		releases = NewGitHubReleases(repo)
	}
	catalog := opts.Catalog
	if catalog == nil {
		catalog = NewCatalog(CatalogOptions{Repo: repo, Lister: catalogLister(releases, repo)})
	}
	snap, err := catalog.Get(ctx, opts.Refresh)
	if err != nil {
		return nil, fmt.Errorf("read the release catalog: %w", err)
	}
	if snap.Stale {
		log.Warn("Checking against a stale release catalog", "fetched_at", snap.FetchedAt, "error", snap.Error)
	}

	latest, ok := snap.Latest(opts.Channel)
	if !ok {
		log.Debug("No installable release published for channel", "channel", opts.Channel)
		return result, nil
	}
	result.LatestVersion = latest.Tag
	result.ReleaseURL = latest.NotesURL

	// Only a cross-channel comparison needs the running release's date; a
	// same-channel one compares the versions.
	var dater ReleaseSource
	if update.DetectChannel(opts.CurrentVersion) != opts.Channel {
		dater = releases
	}
	class := update.CompareRefs(
		RunningRef(ctx, snap, dater, opts.CurrentVersion, opts.CurrentBuildTime),
		update.Ref{Version: latest.Tag, Date: latest.PublishedAt},
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

// RunningRef dates the running version for a comparison against catalog
// releases. A git build ("" or any non-release version) is dated by
// buildTime. A release is dated by its publish date in snap, else by a
// single-tag lookup through releases (nil skips the lookup); when neither
// has it the date stays zero, and update.CompareRefs falls back to a
// nightly tag's own date.
func RunningRef(ctx context.Context, snap Snapshot, releases ReleaseSource, current string, buildTime time.Time) update.Ref {
	cur := update.Ref{Version: current}
	if !update.IsReleaseTag(current) {
		cur.Date = buildTime
		return cur
	}
	if r, ok := snap.Lookup(current); ok && !r.PublishedAt.IsZero() {
		cur.Date = r.PublishedAt
		return cur
	}
	if releases != nil {
		if rel, err := releases.ReleaseByTag(ctx, current); err == nil && !rel.PublishedAt.IsZero() {
			cur.Date = rel.PublishedAt
		}
	}
	return cur
}

// catalogLister is the ReleaseLister for a default catalog: releases itself
// when it can list (GitHubReleases can), else GitHub's API for repo.
func catalogLister(releases ReleaseSource, repo string) ReleaseLister {
	if l, ok := releases.(ReleaseLister); ok {
		return l
	}
	return NewGitHubReleases(repo)
}
