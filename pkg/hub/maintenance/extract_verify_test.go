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
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	t.Run("rejects scion in a nested directory", func(t *testing.T) {
		tarball := createTestTarball(t, map[string][]byte{
			"scion-v1.0.0/scion":  []byte("nested scion binary"),
			"scion-v1.0.0/README": []byte("readme"),
		})
		_, err := extractTarball(t, tarball, DefaultMaxExtractBytes)
		if err == nil || !strings.Contains(err.Error(), "nested") {
			t.Fatalf("err = %v, want nested-entry error", err)
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

// tarEntry is one entry for orderedTarball.
type tarEntry struct {
	name     string
	typeflag byte
	body     []byte
	linkname string
}

// orderedTarball builds a .tar.gz with the entries in order (duplicates
// allowed, unlike createTestTarball's map).
func orderedTarball(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: tf, Mode: 0o755, Linkname: e.linkname}
		if tf == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// extractTarball writes tarball to a temp dir and extracts it with maxBytes.
// It returns the extract directory's entries on success, for assertions.
func extractTarball(t *testing.T, tarball []byte, maxBytes int64) (string, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "release.tar.gz")
	if err := os.WriteFile(p, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	return extractScionBinary(p, dir, &bytes.Buffer{}, maxBytes)
}

func TestExtractScionBinary_ReleaseLayout(t *testing.T) {
	bin := []byte("#!/bin/sh\necho '{\"version\":\"v1.0.0\"}'")

	for _, name := range []string{"scion", "./scion"} {
		t.Run("root entry "+name, func(t *testing.T) {
			got, err := extractTarball(t, orderedTarball(t,
				tarEntry{name: "LICENSE", body: []byte("Apache 2.0")},
				tarEntry{name: "docs/", typeflag: tar.TypeDir},
				tarEntry{name: "docs/README", body: []byte("readme")},
				tarEntry{name: name, body: bin},
			), DefaultMaxExtractBytes)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			data, err := os.ReadFile(got)
			if err != nil || !bytes.Equal(data, bin) {
				t.Fatalf("binary = %q, %v", data, err)
			}
			if fi, _ := os.Stat(got); fi.Mode().Perm() != 0o755 {
				t.Fatalf("mode = %v", fi.Mode().Perm())
			}
			// Only the scion entry is written.
			entries, err := os.ReadDir(filepath.Dir(got))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "scion" {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("extracted entries = %v, want only scion", names)
			}
		})
	}
}

func TestExtractScionBinary_Rejections(t *testing.T) {
	bin := []byte("binary")
	tests := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{"duplicate root entry", []tarEntry{{name: "scion", body: bin}, {name: "scion", body: []byte("second")}}, "more than one"},
		{"duplicate via ./", []tarEntry{{name: "./scion", body: bin}, {name: "scion", body: []byte("second")}}, "more than one"},
		{"nested after root", []tarEntry{{name: "scion", body: bin}, {name: "x/scion", body: bin}}, "nested"},
		{"nested before root", []tarEntry{{name: "x/scion", body: bin}, {name: "scion", body: bin}}, "nested"},
		{"traversal named scion", []tarEntry{{name: "../scion", body: bin}}, "nested"},
		{"root scion symlink", []tarEntry{{name: "scion", typeflag: tar.TypeSymlink, linkname: "/bin/sh"}}, "not a regular file"},
		{"root scion directory", []tarEntry{{name: "scion/", typeflag: tar.TypeDir}}, "not a regular file"},
		{"no scion", []tarEntry{{name: "README", body: bin}}, "no 'scion' binary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := extractTarball(t, orderedTarball(t, tt.entries...), DefaultMaxExtractBytes)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestExtractScionBinary_SizeCap(t *testing.T) {
	// A gzip bomb in miniature: 4 MiB of zeros compresses to a few KB.
	zeros := make([]byte, 4<<20)
	const testCap = 1 << 20

	t.Run("oversize scion entry", func(t *testing.T) {
		tarball := orderedTarball(t, tarEntry{name: "scion", body: zeros})
		if len(tarball) > 64<<10 {
			t.Fatalf("test tarball unexpectedly large: %d", len(tarball))
		}
		_, err := extractTarball(t, tarball, testCap)
		if !errors.Is(err, errExtractTooLarge) {
			t.Fatalf("err = %v, want size-limit error", err)
		}
	})

	t.Run("oversize skipped entry counts too", func(t *testing.T) {
		tarball := orderedTarball(t, tarEntry{name: "padding", body: zeros}, tarEntry{name: "scion", body: []byte("bin")})
		_, err := extractTarball(t, tarball, testCap)
		if !errors.Is(err, errExtractTooLarge) {
			t.Fatalf("err = %v, want size-limit error", err)
		}
	})

	t.Run("under the cap", func(t *testing.T) {
		tarball := orderedTarball(t, tarEntry{name: "scion", body: zeros[:testCap/2]})
		if _, err := extractTarball(t, tarball, testCap); err != nil {
			t.Fatalf("extract: %v", err)
		}
	})

	if DefaultMaxExtractBytes != DefaultMaxDownloadBytes {
		t.Fatalf("DefaultMaxExtractBytes = %d, documented to match the 512 MiB download cap", DefaultMaxExtractBytes)
	}
}
