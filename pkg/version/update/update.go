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
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	maxManifestSize = 1 << 20
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

// FetchManifest retrieves and parses the release manifest.
func FetchManifest(ctx context.Context, opts ...Option) (*Manifest, error) {
	config := newOptions(opts...)
	requestCtx, cancel := context.WithTimeout(ctx, config.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, config.manifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create release manifest request: %w", err)
	}

	client := config.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch release manifest: unexpected HTTP status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("read release manifest: %w", err)
	}
	if len(body) > maxManifestSize {
		return nil, fmt.Errorf("read release manifest: response exceeds %d bytes", maxManifestSize)
	}

	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("decode release manifest: %w", err)
	}
	return &manifest, nil
}

// CheckForUpdate reports whether currentVersion has a newer release in its channel.
func CheckForUpdate(ctx context.Context, currentVersion string, opts ...Option) (*UpdateInfo, error) {
	channel := DetectChannel(currentVersion)
	result := &UpdateInfo{
		CurrentVersion: currentVersion,
		Channel:        channel,
	}
	if channel == "" {
		return result, nil
	}

	manifest, err := FetchManifest(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("check for update: %w", err)
	}

	latest := manifest.Channels[channel]
	result.LatestVersion = latest.Version
	result.ReleaseURL = latest.URL
	if latest.Version == "" {
		return result, nil
	}

	if channel == "nightly" {
		result.UpdateAvailable = currentVersion < latest.Version
	} else {
		result.UpdateAvailable = semver.Compare(
			withVersionPrefix(currentVersion),
			withVersionPrefix(latest.Version),
		) < 0
	}
	return result, nil
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

func newOptions(opts ...Option) options {
	config := options{
		manifestURL: DefaultManifestURL,
		timeout:     DefaultTimeout,
	}
	for _, opt := range opts {
		opt(&config)
	}
	return config
}
