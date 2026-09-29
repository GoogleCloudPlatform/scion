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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BinaryUpdateExecutor.Run must verify a downloaded release tarball
// against the release's own SHA256SUMS asset before extracting it, the
// same way scripts/single-node-vm/deploy.sh does for its own install
// path. Verification is split into two steps so Run() can fetch the
// expected checksum *before* downloading the (potentially large) tarball:
// fetchExpectedChecksum (its checksumsURL parameter is a plain URL, unlike
// resolveReleaseAssets' hardcoded api.github.com endpoint, so it's fully
// exercisable against a local httptest server) and verifyFileChecksum.
// These tests cover both directly, deriveChecksumsURL, and then confirm
// the wiring — including the fetch-before-download ordering — through
// BinaryUpdateExecutor.Run itself.

func TestFetchExpectedChecksum(t *testing.T) {
	const assetName = "scion-linux-amd64.tar.gz"
	correctHash := strings.Repeat("a", 64)

	t.Run("matching entry returns its hash", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := fmt.Fprintf(w, "%s  %s\n", correctHash, assetName); err != nil {
				t.Errorf("write response: %v", err)
			}
		}))
		defer server.Close()

		got, err := fetchExpectedChecksum(context.Background(), server.URL, assetName, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != correctHash {
			t.Errorf("fetchExpectedChecksum() = %q, want %q", got, correctHash)
		}
	})

	t.Run("checksums asset missing (404) fails closed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		}))
		defer server.Close()

		_, err := fetchExpectedChecksum(context.Background(), server.URL, assetName, &bytes.Buffer{})
		if err == nil {
			t.Fatal("expected an error when SHA256SUMS cannot be downloaded, got nil")
		}
		if !strings.Contains(err.Error(), "download checksums") {
			t.Errorf("error should say the checksums download failed, got: %v", err)
		}
	})

	t.Run("no checksums URL at all fails closed", func(t *testing.T) {
		_, err := fetchExpectedChecksum(context.Background(), "", assetName, &bytes.Buffer{})
		if err == nil {
			t.Fatal("expected an error for an empty checksums URL, got nil")
		}
		if !strings.Contains(err.Error(), "no checksums URL") {
			t.Errorf("error should name the missing checksums URL, got: %v", err)
		}
	})

	t.Run("no matching entry fails closed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := fmt.Fprintf(w, "%s  some-other-asset.tar.gz\n", correctHash); err != nil {
				t.Errorf("write response: %v", err)
			}
		}))
		defer server.Close()

		_, err := fetchExpectedChecksum(context.Background(), server.URL, assetName, &bytes.Buffer{})
		if err == nil {
			t.Fatal("expected an error when SHA256SUMS has no entry for this asset, got nil")
		}
		if !strings.Contains(err.Error(), "no checksum entry") {
			t.Errorf("error should say no checksum entry was found, got: %v", err)
		}
	})

	t.Run("binary-mode asterisk-prefixed filename is still matched", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := fmt.Fprintf(w, "%s *%s\n", correctHash, assetName); err != nil {
				t.Errorf("write response: %v", err)
			}
		}))
		defer server.Close()

		got, err := fetchExpectedChecksum(context.Background(), server.URL, assetName, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("unexpected error for a binary-mode (asterisk-prefixed) SHA256SUMS entry: %v", err)
		}
		if got != correctHash {
			t.Errorf("fetchExpectedChecksum() = %q, want %q", got, correctHash)
		}
	})
}

func TestVerifyFileChecksum(t *testing.T) {
	tarballContent := []byte("fake scion release tarball bytes")
	sum := sha256.Sum256(tarballContent)
	correctHash := hex.EncodeToString(sum[:])
	const assetName = "scion-linux-amd64.tar.gz"

	writeTarball := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "release.tar.gz")
		if err := os.WriteFile(path, tarballContent, 0o644); err != nil {
			t.Fatalf("write fake tarball: %v", err)
		}
		return path
	}

	t.Run("matching checksum succeeds", func(t *testing.T) {
		var logBuf bytes.Buffer
		err := verifyFileChecksum(writeTarball(t), assetName, correctHash, &logBuf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(logBuf.String(), "Checksum verified") {
			t.Errorf("expected log output to confirm verification, got: %q", logBuf.String())
		}
	})

	t.Run("hash mismatch fails closed", func(t *testing.T) {
		wrongHash := strings.Repeat("0", 64)
		var logBuf bytes.Buffer
		err := verifyFileChecksum(writeTarball(t), assetName, wrongHash, &logBuf)
		if err == nil {
			t.Fatal("expected an error for a checksum mismatch, got nil")
		}
		if !strings.Contains(err.Error(), "checksum mismatch") {
			t.Errorf("error should say checksum mismatch, got: %v", err)
		}
	})
}

func TestDeriveChecksumsURL(t *testing.T) {
	tests := []struct {
		name        string
		downloadURL string
		want        string
	}{
		{
			name:        "typical GitHub release asset URL",
			downloadURL: "https://github.com/GoogleCloudPlatform/scion/releases/download/v1.2.3/scion-linux-amd64.tar.gz",
			want:        "https://github.com/GoogleCloudPlatform/scion/releases/download/v1.2.3/SHA256SUMS",
		},
		{
			name:        "no slash at all",
			downloadURL: "scion-linux-amd64.tar.gz",
			want:        "",
		},
		{
			name:        "trailing slash",
			downloadURL: "https://example.com/release/",
			want:        "https://example.com/release/SHA256SUMS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveChecksumsURL(tt.downloadURL); got != tt.want {
				t.Errorf("deriveChecksumsURL(%q) = %q, want %q", tt.downloadURL, got, tt.want)
			}
		})
	}
}

// TestBinaryUpdateExecutor_ChecksumVerificationWiring confirms Run() itself
// calls into checksum verification (not just fetchExpectedChecksum /
// verifyFileChecksum in isolation) and fails closed before extraction when
// it fails. It supplies target_version, download_url, and checksums_url
// directly so Run() skips CheckForReleaseUpdates entirely (which hits the
// real, hardcoded GitHub API and can't be pointed at a test server) --
// see resolveReleaseAssets' own doc comment. Every case here either fails
// at or before the checksum step, or (the "derives it from download_url"
// case) passes the checksum step and then fails at extraction, because
// its fixture is a plain string, not a real gzip tarball. Either way,
// none of them reach the sudo-based install Run() would attempt next,
// which this test environment cannot support (see TestRestoreBackup).
func TestBinaryUpdateExecutor_ChecksumVerificationWiring(t *testing.T) {
	tarballContent := []byte("fake scion release tarball bytes")
	sum := sha256.Sum256(tarballContent)
	correctHash := hex.EncodeToString(sum[:])
	const assetName = "scion-linux-amd64.tar.gz"

	// tarballHits, when non-nil, counts requests to the asset endpoint —
	// used to prove Run() fetches SHA256SUMS *before* downloading the
	// tarball: an unattended, recurring update check should not pull a
	// full release tarball only to then discover checksums are missing.
	newAssetServer := func(t *testing.T, sha256sumsBody string, tarballHits *int) *httptest.Server {
		t.Helper()
		mux := http.NewServeMux()
		mux.HandleFunc("/"+assetName, func(w http.ResponseWriter, _ *http.Request) {
			if tarballHits != nil {
				*tarballHits++
			}
			_, _ = w.Write(tarballContent)
		})
		mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
			if sha256sumsBody == "" {
				http.NotFound(w, nil)
				return
			}
			_, _ = fmt.Fprint(w, sha256sumsBody)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		return server
	}

	t.Run("missing SHA256SUMS asset fails before extraction, without downloading the tarball", func(t *testing.T) {
		var tarballHits int
		server := newAssetServer(t, "", &tarballHits)
		executor := &BinaryUpdateExecutor{serviceName: "test-hub"}
		var logBuf bytes.Buffer
		params := map[string]string{
			"target_version": "v9.9.9",
			"download_url":   server.URL + "/" + assetName,
			"checksums_url":  server.URL + "/SHA256SUMS",
		}
		err := executor.Run(context.Background(), &logBuf, params)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "checksum verification failed") {
			t.Errorf("expected a checksum verification error, got: %v", err)
		}
		if strings.Contains(logBuf.String(), "Found scion binary") {
			t.Error("extraction must not have run after a failed checksum verification")
		}
		if tarballHits != 0 {
			t.Errorf("the tarball endpoint was hit %d time(s); it must not be downloaded before SHA256SUMS is confirmed to exist and cover this asset", tarballHits)
		}
	})

	t.Run("no matching entry fails before extraction, without downloading the tarball", func(t *testing.T) {
		var tarballHits int
		server := newAssetServer(t, fmt.Sprintf("%s  some-other-asset.tar.gz\n", correctHash), &tarballHits)
		executor := &BinaryUpdateExecutor{serviceName: "test-hub"}
		var logBuf bytes.Buffer
		params := map[string]string{
			"target_version": "v9.9.9",
			"download_url":   server.URL + "/" + assetName,
			"checksums_url":  server.URL + "/SHA256SUMS",
		}
		err := executor.Run(context.Background(), &logBuf, params)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "checksum verification failed") {
			t.Errorf("expected a checksum verification error, got: %v", err)
		}
		if tarballHits != 0 {
			t.Errorf("the tarball endpoint was hit %d time(s); a SHA256SUMS with no entry for this asset must be caught before downloading it", tarballHits)
		}
	})

	t.Run("hash mismatch fails before extraction", func(t *testing.T) {
		wrongHash := strings.Repeat("0", 64)
		server := newAssetServer(t, fmt.Sprintf("%s  %s\n", wrongHash, assetName), nil)
		executor := &BinaryUpdateExecutor{serviceName: "test-hub"}
		var logBuf bytes.Buffer
		params := map[string]string{
			"target_version": "v9.9.9",
			"download_url":   server.URL + "/" + assetName,
			"checksums_url":  server.URL + "/SHA256SUMS",
		}
		err := executor.Run(context.Background(), &logBuf, params)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "checksum verification failed") {
			t.Errorf("expected a checksum verification error, got: %v", err)
		}
		if strings.Contains(logBuf.String(), "Found scion binary") {
			t.Error("extraction must not have run after a failed checksum verification")
		}
	})

	t.Run("no checksums_url param derives it from download_url and still verifies", func(t *testing.T) {
		server := newAssetServer(t, fmt.Sprintf("%s  %s\n", correctHash, assetName), nil)
		executor := &BinaryUpdateExecutor{serviceName: "test-hub"}
		var logBuf bytes.Buffer
		params := map[string]string{
			"target_version": "v9.9.9",
			"download_url":   server.URL + "/" + assetName,
			// No checksums_url: Run() must derive server.URL + "/SHA256SUMS"
			// from download_url itself.
		}
		err := executor.Run(context.Background(), &logBuf, params)
		// This case has a *matching* checksum, so it proceeds past
		// verification into extraction, which then fails because
		// tarballContent (above) is a plain string, not a real gzip
		// tarball -- not because of anything environment-dependent like a
		// missing sudo. Reaching extraction at all is exactly the point:
		// it proves the derived checksums_url was fetched and matched the
		// real entry.
		if err == nil {
			t.Fatal("expected an error from the later extraction step, got nil")
		}
		if strings.Contains(err.Error(), "checksum verification failed") {
			t.Errorf("checksum verification should have succeeded with a derived, matching checksums_url, got: %v", err)
		}
		if !strings.Contains(err.Error(), "extraction failed") {
			t.Errorf("expected the error to come from extraction (proving verification passed), got: %v", err)
		}
		if !strings.Contains(logBuf.String(), "Checksum verified") {
			t.Errorf("expected log output to show the derived checksums_url was verified, got: %q", logBuf.String())
		}
	})
}
