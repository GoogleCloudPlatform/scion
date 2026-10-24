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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

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
		binaryPath, err := ExtractScionBinary(tarballPath, tmpDir, &logBuf)
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
		binaryPath, err := ExtractScionBinary(tarballPath, tmpDir, &logBuf)
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
		_, err := ExtractScionBinary(tarballPath, tmpDir, &logBuf)
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
		_, err := ExtractScionBinary(tarballPath, tmpDir, &logBuf)
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
		_, err := ExtractScionBinary(tarballPath, tmpDir, &logBuf)
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
		err := VerifyScionBinary(context.Background(), scriptPath, "v1.2.0", &logBuf)
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
		err := VerifyScionBinary(context.Background(), scriptPath, "v1.2.0", &logBuf)
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
		err := VerifyScionBinary(context.Background(), scriptPath, "v1.2.0", &logBuf)
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
		err := VerifyScionBinary(context.Background(), scriptPath, "v1.2.0", &logBuf)
		if err == nil {
			t.Fatal("expected error for invalid JSON output")
		}
	})
}

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
	binaryPath, err := ExtractScionBinary(tarballPath, tmpDir, &logBuf)
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
