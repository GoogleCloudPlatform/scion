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

// Package update checks release manifests for newer Scion versions.
package update

import (
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	// DefaultManifestURL is the raw GitHub content URL for LATEST.json.
	DefaultManifestURL = "https://raw.githubusercontent.com/GoogleCloudPlatform/scion/main/LATEST.json"

	// DefaultTimeout is the HTTP timeout for fetching the manifest.
	DefaultTimeout = 5 * time.Second
)

// ChannelInfo represents a single release channel's latest version.
type ChannelInfo struct {
	Version string `json:"version"`
	Date    string `json:"date"`
	URL     string `json:"url"`
}

// Manifest represents the LATEST.json structure.
type Manifest struct {
	Channels map[string]ChannelInfo `json:"channels"`
}

// UpdateInfo is returned by CheckForUpdate with the comparison result.
type UpdateInfo struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	Channel         string `json:"channel"`
	UpdateAvailable bool   `json:"updateAvailable"`
	ReleaseURL      string `json:"releaseUrl,omitempty"`
}

// Option configures the update checker.
type Option func(*options)

type options struct {
	manifestURL string
	timeout     time.Duration
	httpClient  *http.Client
}

// WithManifestURL sets a custom manifest URL.
func WithManifestURL(url string) Option {
	return func(o *options) { o.manifestURL = url }
}

// WithTimeout sets the HTTP timeout.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithHTTPClient sets a custom HTTP client (useful for testing).
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// DetectChannel returns the release channel associated with version.
func DetectChannel(version string) string {
	switch {
	case version == "", version == "dev":
		return ""
	case strings.HasPrefix(version, "nightly-"):
		return "nightly"
	case strings.Contains(version, "-preview."), strings.Contains(version, "-rc."):
		return "preview"
	case semver.IsValid(withVersionPrefix(version)):
		return "stable"
	default:
		return ""
	}
}

func withVersionPrefix(version string) string {
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}
