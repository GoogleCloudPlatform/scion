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

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDetectChannel(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "empty", version: "", want: ""},
		{name: "development build", version: "dev", want: ""},
		{name: "nightly", version: "nightly-20260916", want: "nightly"},
		{name: "preview", version: "v0.3.0-preview.2", want: "preview"},
		{name: "release candidate", version: "v0.3.0-rc.1", want: "preview"},
		{name: "stable major", version: "v1.0.0", want: "stable"},
		{name: "stable pre-1.0", version: "v0.3.0", want: "stable"},
		{name: "stable without prefix", version: "1.0.0", want: "stable"},
		{name: "unknown", version: "garbage", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectChannel(tt.version); got != tt.want {
				t.Fatalf("DetectChannel(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestFetchManifest(t *testing.T) {
	t.Run("valid JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"channels": {
					"stable": {
						"version": "v1.0.0",
						"date": "2026-09-15T00:00:00Z",
						"url": "https://example.com/v1.0.0"
					}
				}
			}`))
		}))
		defer server.Close()

		manifest, err := FetchManifest(context.Background(), WithManifestURL(server.URL))
		if err != nil {
			t.Fatalf("FetchManifest() error = %v", err)
		}
		got := manifest.Channels["stable"]
		if got.Version != "v1.0.0" || got.Date != "2026-09-15T00:00:00Z" || got.URL != "https://example.com/v1.0.0" {
			t.Fatalf("FetchManifest() stable channel = %+v, want manifest values", got)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"channels":`))
		}))
		defer server.Close()

		if _, err := FetchManifest(context.Background(), WithManifestURL(server.URL)); err == nil {
			t.Fatal("FetchManifest() error = nil, want JSON parsing error")
		}
	})

	t.Run("HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer server.Close()

		if _, err := FetchManifest(context.Background(), WithManifestURL(server.URL)); err == nil {
			t.Fatal("FetchManifest() error = nil, want HTTP status error")
		}
	})

	t.Run("oversized response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"channels":{}}` + strings.Repeat(" ", maxManifestSize)))
		}))
		defer server.Close()

		if _, err := FetchManifest(context.Background(), WithManifestURL(server.URL)); err == nil {
			t.Fatal("FetchManifest() error = nil, want response size error")
		}
	})

	t.Run("configured timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"channels":{}}`))
		}))
		defer server.Close()

		_, err := FetchManifest(
			context.Background(),
			WithManifestURL(server.URL),
			WithTimeout(time.Millisecond),
		)
		if err == nil || !strings.Contains(err.Error(), "fetch release manifest") {
			t.Fatalf("FetchManifest() error = %v, want descriptive timeout error", err)
		}
	})
}

func TestCheckForUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"channels": {
				"stable": {
					"version": "v1.2.0",
					"date": "2026-09-15T00:00:00Z",
					"url": "https://example.com/v1.2.0"
				},
				"preview": {
					"version": "v1.3.0-preview.3",
					"date": "2026-09-15T00:00:00Z",
					"url": "https://example.com/v1.3.0-preview.3"
				},
				"nightly": {
					"version": "nightly-20260916",
					"date": "2026-09-16T02:00:00Z",
					"url": "https://example.com/nightly-20260916"
				}
			}
		}`))
	}))
	defer server.Close()

	tests := []struct {
		name           string
		currentVersion string
		wantLatest     string
		wantChannel    string
		wantAvailable  bool
		wantReleaseURL string
	}{
		{
			name:           "older stable version",
			currentVersion: "v1.1.0",
			wantLatest:     "v1.2.0",
			wantChannel:    "stable",
			wantAvailable:  true,
			wantReleaseURL: "https://example.com/v1.2.0",
		},
		{
			name:           "equal stable version",
			currentVersion: "v1.2.0",
			wantLatest:     "v1.2.0",
			wantChannel:    "stable",
			wantReleaseURL: "https://example.com/v1.2.0",
		},
		{
			name:           "newer stable version",
			currentVersion: "v1.3.0",
			wantLatest:     "v1.2.0",
			wantChannel:    "stable",
			wantReleaseURL: "https://example.com/v1.2.0",
		},
		{
			name:           "stable version without prefix",
			currentVersion: "1.1.0",
			wantLatest:     "v1.2.0",
			wantChannel:    "stable",
			wantAvailable:  true,
			wantReleaseURL: "https://example.com/v1.2.0",
		},
		{
			name:           "older preview version",
			currentVersion: "v1.3.0-preview.2",
			wantLatest:     "v1.3.0-preview.3",
			wantChannel:    "preview",
			wantAvailable:  true,
			wantReleaseURL: "https://example.com/v1.3.0-preview.3",
		},
		{
			name:           "older nightly version",
			currentVersion: "nightly-20260915",
			wantLatest:     "nightly-20260916",
			wantChannel:    "nightly",
			wantAvailable:  true,
			wantReleaseURL: "https://example.com/nightly-20260916",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckForUpdate(context.Background(), tt.currentVersion, WithManifestURL(server.URL))
			if err != nil {
				t.Fatalf("CheckForUpdate() error = %v", err)
			}
			if got.CurrentVersion != tt.currentVersion {
				t.Errorf("CurrentVersion = %q, want %q", got.CurrentVersion, tt.currentVersion)
			}
			if got.LatestVersion != tt.wantLatest {
				t.Errorf("LatestVersion = %q, want %q", got.LatestVersion, tt.wantLatest)
			}
			if got.Channel != tt.wantChannel {
				t.Errorf("Channel = %q, want %q", got.Channel, tt.wantChannel)
			}
			if got.UpdateAvailable != tt.wantAvailable {
				t.Errorf("UpdateAvailable = %t, want %t", got.UpdateAvailable, tt.wantAvailable)
			}
			if got.ReleaseURL != tt.wantReleaseURL {
				t.Errorf("ReleaseURL = %q, want %q", got.ReleaseURL, tt.wantReleaseURL)
			}
		})
	}

	t.Run("development build skips fetching", func(t *testing.T) {
		got, err := CheckForUpdate(context.Background(), "dev", WithManifestURL("://invalid"))
		if err != nil {
			t.Fatalf("CheckForUpdate() error = %v", err)
		}
		if got.CurrentVersion != "dev" || got.Channel != "" || got.UpdateAvailable {
			t.Fatalf("CheckForUpdate() = %+v, want no update for development build", got)
		}
	})

	t.Run("network error", func(t *testing.T) {
		closedServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedServer.Close()

		if _, err := CheckForUpdate(context.Background(), "v1.0.0", WithManifestURL(closedServer.URL)); err == nil {
			t.Fatal("CheckForUpdate() error = nil, want network error")
		}
	})

	t.Run("empty channel version", func(t *testing.T) {
		emptyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"channels":{"stable":{"version":""}}}`))
		}))
		defer emptyServer.Close()

		got, err := CheckForUpdate(context.Background(), "v1.0.0", WithManifestURL(emptyServer.URL))
		if err != nil {
			t.Fatalf("CheckForUpdate() error = %v", err)
		}
		if got.LatestVersion != "" || got.UpdateAvailable {
			t.Fatalf("CheckForUpdate() = %+v, want no update for empty channel version", got)
		}
	})
}
