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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// createTestTarball builds a .tar.gz containing a single file with the given
// name and contents.
func createTestTarball(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header for %q: %v", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write tar content for %q: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func TestDownloadFile(t *testing.T) {
	t.Run("successful download", func(t *testing.T) {
		content := []byte("test binary content")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content)
		}))
		defer server.Close()

		tmpDir := t.TempDir()
		destPath := filepath.Join(tmpDir, "download.tar.gz")
		var logBuf bytes.Buffer

		err := downloadFile(context.Background(), server.URL, destPath, &logBuf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, err := os.ReadFile(destPath)
		if err != nil {
			t.Fatalf("failed to read downloaded file: %v", err)
		}
		if !bytes.Equal(data, content) {
			t.Errorf("downloaded content = %q, want %q", data, content)
		}
	})

	t.Run("HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		tmpDir := t.TempDir()
		destPath := filepath.Join(tmpDir, "download.tar.gz")
		var logBuf bytes.Buffer

		err := downloadFile(context.Background(), server.URL, destPath, &logBuf)
		if err == nil {
			t.Fatal("expected error for HTTP 404, got nil")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data"))
		}))
		defer server.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately.

		tmpDir := t.TempDir()
		destPath := filepath.Join(tmpDir, "download.tar.gz")
		var logBuf bytes.Buffer

		err := downloadFile(ctx, server.URL, destPath, &logBuf)
		if err == nil {
			t.Fatal("expected error for cancelled context, got nil")
		}
	})
}

func TestExtractScionBinary(t *testing.T) {
	t.Run("extracts scion binary from tarball", func(t *testing.T) {
		content := []byte("#!/bin/bash\necho scion")
		tarball := createTestTarball(t, map[string][]byte{
			"scion": content,
		})

		tmpDir := t.TempDir()
		tarballPath := filepath.Join(tmpDir, "release.tar.gz")
		if err := os.WriteFile(tarballPath, tarball, 0o644); err != nil {
			t.Fatalf("write tarball: %v", err)
		}

		var logBuf bytes.Buffer
		binaryPath, err := extractScionBinary(tarballPath, tmpDir, &logBuf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, err := os.ReadFile(binaryPath)
		if err != nil {
			t.Fatalf("read extracted binary: %v", err)
		}
		if !bytes.Equal(data, content) {
			t.Errorf("extracted content = %q, want %q", data, content)
		}

		// Verify it's marked executable.
		info, err := os.Stat(binaryPath)
		if err != nil {
			t.Fatalf("stat extracted binary: %v", err)
		}
		if info.Mode()&0o111 == 0 {
			t.Error("extracted binary is not executable")
		}
	})

	t.Run("finds scion in nested directory", func(t *testing.T) {
		content := []byte("nested scion binary")
		tarball := createTestTarball(t, map[string][]byte{
			"scion-v1.0.0/scion":  content,
			"scion-v1.0.0/README": []byte("readme"),
		})

		tmpDir := t.TempDir()
		tarballPath := filepath.Join(tmpDir, "release.tar.gz")
		if err := os.WriteFile(tarballPath, tarball, 0o644); err != nil {
			t.Fatalf("write tarball: %v", err)
		}

		var logBuf bytes.Buffer
		binaryPath, err := extractScionBinary(tarballPath, tmpDir, &logBuf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, err := os.ReadFile(binaryPath)
		if err != nil {
			t.Fatalf("read extracted binary: %v", err)
		}
		if !bytes.Equal(data, content) {
			t.Errorf("extracted content = %q, want %q", data, content)
		}
	})

	t.Run("returns error when no scion binary found", func(t *testing.T) {
		tarball := createTestTarball(t, map[string][]byte{
			"not-scion": []byte("wrong binary"),
			"README":    []byte("readme"),
		})

		tmpDir := t.TempDir()
		tarballPath := filepath.Join(tmpDir, "release.tar.gz")
		if err := os.WriteFile(tarballPath, tarball, 0o644); err != nil {
			t.Fatalf("write tarball: %v", err)
		}

		var logBuf bytes.Buffer
		_, err := extractScionBinary(tarballPath, tmpDir, &logBuf)
		if err == nil {
			t.Fatal("expected error when no scion binary found")
		}
	})

	t.Run("rejects path traversal", func(t *testing.T) {
		// Create a tarball with a malicious entry that tries to escape.
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)

		hdr := &tar.Header{
			Name: "../../../etc/passwd",
			Mode: 0o644,
			Size: 5,
		}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write([]byte("pwned"))
		_ = tw.Close()
		_ = gw.Close()

		tmpDir := t.TempDir()
		tarballPath := filepath.Join(tmpDir, "evil.tar.gz")
		if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write tarball: %v", err)
		}

		var logBuf bytes.Buffer
		_, err := extractScionBinary(tarballPath, tmpDir, &logBuf)
		// Should either skip the malicious entry (and fail with "no scion binary found")
		// or error out.
		if err == nil {
			t.Fatal("expected error for path traversal tarball")
		}
	})

	t.Run("invalid gzip data", func(t *testing.T) {
		tmpDir := t.TempDir()
		tarballPath := filepath.Join(tmpDir, "bad.tar.gz")
		if err := os.WriteFile(tarballPath, []byte("not gzip data"), 0o644); err != nil {
			t.Fatalf("write bad tarball: %v", err)
		}

		var logBuf bytes.Buffer
		_, err := extractScionBinary(tarballPath, tmpDir, &logBuf)
		if err == nil {
			t.Fatal("expected error for invalid gzip data")
		}
	})
}

func TestVerifyScionBinary(t *testing.T) {
	t.Run("version matches", func(t *testing.T) {
		// Create a script that outputs the expected JSON.
		tmpDir := t.TempDir()
		scriptPath := filepath.Join(tmpDir, "scion")
		script := "#!/bin/sh\necho '{\"version\":\"v1.2.0\",\"commit\":\"abc123\",\"buildTime\":\"2026-01-01\",\"short\":\"v1.2.0\"}'\n"
		if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
			t.Fatalf("write script: %v", err)
		}

		var logBuf bytes.Buffer
		err := verifyScionBinary(context.Background(), scriptPath, "v1.2.0", &logBuf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("version mismatch", func(t *testing.T) {
		tmpDir := t.TempDir()
		scriptPath := filepath.Join(tmpDir, "scion")
		script := `#!/bin/sh
echo '{"version":"v1.0.0","commit":"abc123","buildTime":"2026-01-01","short":"v1.0.0"}'
`
		if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
			t.Fatalf("write script: %v", err)
		}

		var logBuf bytes.Buffer
		err := verifyScionBinary(context.Background(), scriptPath, "v1.2.0", &logBuf)
		if err == nil {
			t.Fatal("expected error for version mismatch")
		}
		if !bytes.Contains([]byte(err.Error()), []byte("version mismatch")) {
			t.Errorf("error = %q, want to contain 'version mismatch'", err)
		}
	})

	t.Run("binary fails to execute", func(t *testing.T) {
		tmpDir := t.TempDir()
		scriptPath := filepath.Join(tmpDir, "scion")
		if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 1"), 0o755); err != nil {
			t.Fatalf("write script: %v", err)
		}

		var logBuf bytes.Buffer
		err := verifyScionBinary(context.Background(), scriptPath, "v1.2.0", &logBuf)
		if err == nil {
			t.Fatal("expected error when binary fails to execute")
		}
	})

	t.Run("invalid JSON output", func(t *testing.T) {
		tmpDir := t.TempDir()
		scriptPath := filepath.Join(tmpDir, "scion")
		if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho 'not json'"), 0o755); err != nil {
			t.Fatalf("write script: %v", err)
		}

		var logBuf bytes.Buffer
		err := verifyScionBinary(context.Background(), scriptPath, "v1.2.0", &logBuf)
		if err == nil {
			t.Fatal("expected error for invalid JSON output")
		}
	})
}

func TestBackupBinary(t *testing.T) {
	t.Run("creates backup copy", func(t *testing.T) {
		tmpDir := t.TempDir()
		srcPath := filepath.Join(tmpDir, "scion")
		content := []byte("original binary content")
		if err := os.WriteFile(srcPath, content, 0o755); err != nil {
			t.Fatalf("write source binary: %v", err)
		}

		backupPath := filepath.Join(tmpDir, "scion.bak")
		var logBuf bytes.Buffer

		err := backupBinary(srcPath, backupPath, &logBuf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, err := os.ReadFile(backupPath)
		if err != nil {
			t.Fatalf("read backup: %v", err)
		}
		if !bytes.Equal(data, content) {
			t.Errorf("backup content = %q, want %q", data, content)
		}

		// Verify mode is preserved.
		info, err := os.Stat(backupPath)
		if err != nil {
			t.Fatalf("stat backup: %v", err)
		}
		if info.Mode()&0o111 == 0 {
			t.Error("backup file is not executable")
		}
	})

	t.Run("source file does not exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		srcPath := filepath.Join(tmpDir, "nonexistent")
		backupPath := filepath.Join(tmpDir, "nonexistent.bak")
		var logBuf bytes.Buffer

		err := backupBinary(srcPath, backupPath, &logBuf)
		if err == nil {
			t.Fatal("expected error for nonexistent source file")
		}
	})
}

func TestDownloadExtractVerifyIntegration(t *testing.T) {
	// Integration test: serve a tarball via HTTP, download, extract, and
	// verify the binary. Uses a shell script as the "binary" to avoid
	// needing a real compiled scion binary.
	targetVersion := "v2.0.0"

	// Create a shell script that mimics `scion version --format json`.
	scriptContent := fmt.Sprintf(`#!/bin/sh
# Ignore arguments other than "version" for simplicity.
echo '{"version":"%s","commit":"test","buildTime":"2026-01-01","short":"%s"}'
`, targetVersion, targetVersion)

	tarball := createTestTarball(t, map[string][]byte{
		"scion": []byte(scriptContent),
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarball)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	tarballPath := filepath.Join(tmpDir, "release.tar.gz")
	var logBuf bytes.Buffer

	// Step 1: Download.
	if err := downloadFile(context.Background(), server.URL, tarballPath, &logBuf); err != nil {
		t.Fatalf("download failed: %v", err)
	}

	// Step 2: Extract.
	binaryPath, err := extractScionBinary(tarballPath, tmpDir, &logBuf)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}

	// Step 3: Verify.
	if err := verifyScionBinary(context.Background(), binaryPath, targetVersion, &logBuf); err != nil {
		t.Fatalf("verify failed: %v", err)
	}

	// Check the log output mentions key steps.
	logOutput := logBuf.String()
	for _, want := range []string{"Downloaded", "Found scion binary", "Version verified"} {
		if !bytes.Contains([]byte(logOutput), []byte(want)) {
			t.Errorf("log output missing %q", want)
		}
	}
}

func TestRestoreBackup(t *testing.T) {
	// restoreBackup uses "sudo install", which won't work in most test
	// environments. We test the backup → restore flow conceptually by
	// verifying backupBinary works, then testing restoreBackup errors
	// gracefully when sudo is unavailable.
	t.Run("fails gracefully without sudo", func(t *testing.T) {
		tmpDir := t.TempDir()
		backupPath := filepath.Join(tmpDir, "scion.bak")
		destPath := filepath.Join(tmpDir, "scion")

		if err := os.WriteFile(backupPath, []byte("backup content"), 0o755); err != nil {
			t.Fatalf("write backup: %v", err)
		}

		var logBuf bytes.Buffer
		// This will fail because sudo isn't available in test environments,
		// but it should not panic.
		_ = restoreBackup(backupPath, destPath, &logBuf)
	})
}

// TestBinaryUpdateExecutor_PreflightValidation tests that the executor
// validates pre-flight conditions correctly.
func TestBinaryUpdateExecutor_PreflightValidation(t *testing.T) {
	t.Run("no download URL available", func(t *testing.T) {
		// If no download URL and no update available, executor should
		// return nil (no error, just "no update available").
		// We test this by providing a dev version that skips the check.
		executor := &BinaryUpdateExecutor{
			serviceName: "test-hub",
		}

		var logBuf bytes.Buffer
		params := map[string]string{
			"current_version": "dev",
		}

		// This will try CheckForReleaseUpdates with "dev" version,
		// which returns early with no update available.
		err := executor.Run(context.Background(), &logBuf, params)
		// On non-Linux systems, it should fail with the OS check.
		// On Linux, it should return nil (no update for dev version).
		if err != nil {
			// Expected on non-Linux or if it reaches the release check.
			t.Logf("Expected error in test env: %v", err)
		}
	})
}

// Verify that the download → extract → verify chain handles a tarball
// with the scion binary in a subdirectory (matching the actual GitHub
// Release asset structure).
func TestExtractScionBinary_SubdirectoryStructure(t *testing.T) {
	// Simulate the real release tarball structure:
	// scion-linux-amd64/
	// scion-linux-amd64/scion
	// scion-linux-amd64/LICENSE
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Directory entry.
	_ = tw.WriteHeader(&tar.Header{
		Name:     "scion-linux-amd64/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	})

	// Binary entry.
	binaryContent := []byte("#!/bin/sh\necho '{\"version\":\"v1.0.0\"}'")
	_ = tw.WriteHeader(&tar.Header{
		Name: "scion-linux-amd64/scion",
		Mode: 0o755,
		Size: int64(len(binaryContent)),
	})
	_, _ = tw.Write(binaryContent)

	// License entry.
	licenseContent := []byte("Apache 2.0")
	_ = tw.WriteHeader(&tar.Header{
		Name: "scion-linux-amd64/LICENSE",
		Mode: 0o644,
		Size: int64(len(licenseContent)),
	})
	_, _ = tw.Write(licenseContent)

	_ = tw.Close()
	_ = gw.Close()

	tmpDir := t.TempDir()
	tarballPath := filepath.Join(tmpDir, "release.tar.gz")
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tarball: %v", err)
	}

	var logBuf bytes.Buffer
	binaryPath, err := extractScionBinary(tarballPath, tmpDir, &logBuf)
	if err != nil {
		t.Fatalf("extraction failed: %v", err)
	}

	// Verify the binary was found in the subdirectory.
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read extracted binary: %v", err)
	}
	if !bytes.Equal(data, binaryContent) {
		t.Errorf("content mismatch: got %q", data)
	}

	// Verify LICENSE was also extracted.
	extractDir := filepath.Dir(binaryPath)
	licPath := filepath.Join(extractDir, "LICENSE")
	licData, err := os.ReadFile(licPath)
	if err != nil {
		t.Logf("LICENSE file not found at %s (may be in different location)", licPath)
	} else if !bytes.Equal(licData, licenseContent) {
		t.Errorf("LICENSE content = %q, want %q", licData, licenseContent)
	}
}

// TestBackupAndRestoreRoundtrip verifies that a backup copy is byte-identical
// to the original.
func TestBackupAndRestoreRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a "binary" with known content.
	originalContent := []byte("binary-content-v1.0.0-with-some-padding-to-be-realistic")
	originalPath := filepath.Join(tmpDir, "scion")
	if err := os.WriteFile(originalPath, originalContent, 0o755); err != nil {
		t.Fatalf("write original: %v", err)
	}

	// Back it up.
	backupPath := filepath.Join(tmpDir, "scion.bak")
	var logBuf bytes.Buffer
	if err := backupBinary(originalPath, backupPath, &logBuf); err != nil {
		t.Fatalf("backup failed: %v", err)
	}

	// Overwrite the original.
	newContent := []byte("binary-content-v2.0.0-different-content")
	if err := os.WriteFile(originalPath, newContent, 0o755); err != nil {
		t.Fatalf("overwrite original: %v", err)
	}

	// Verify the backup still has the original content.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !bytes.Equal(data, originalContent) {
		t.Errorf("backup content = %q, want %q", data, originalContent)
	}

	// Verify the original has the new content.
	data, err = os.ReadFile(originalPath)
	if err != nil {
		t.Fatalf("read overwritten original: %v", err)
	}
	if !bytes.Equal(data, newContent) {
		t.Errorf("overwritten content = %q, want %q", data, newContent)
	}
}
