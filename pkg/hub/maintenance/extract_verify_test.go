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
	"fmt"
	"io"
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

// rawTarBlock is a hand-built ustar header block. archive/tar's writer drops
// GNU.sparse.* PAX records, so sparse fixtures are assembled by hand.
func rawTarBlock(name string, typeflag byte, size int64) []byte {
	b := make([]byte, 512)
	copy(b[0:100], name)
	copy(b[100:108], "0000755\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], fmt.Sprintf("%011o\x00", size))
	copy(b[136:148], "00000000000\x00")
	b[156] = typeflag
	copy(b[257:263], "ustar\x00")
	copy(b[263:265], "00")
	for i := 148; i < 156; i++ {
		b[i] = ' '
	}
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

func padTarBlock(b []byte) []byte {
	if r := len(b) % 512; r != 0 {
		b = append(b, make([]byte, 512-r)...)
	}
	return b
}

func paxRecord(k, v string) string {
	rec := " " + k + "=" + v + "\n"
	n := len(rec)
	for {
		s := fmt.Sprintf("%d%s", n, rec)
		if len(s) == n {
			return s
		}
		n = len(s)
	}
}

// PAX GNU sparse encodings understood by archive/tar.
const (
	sparseV00          = "0.0" // GNU.sparse.offset/numbytes pairs
	sparseV01          = "0.1" // GNU.sparse.map, with version keys
	sparseV01NoVersion = "0.1-no-version"
	sparseV10          = "1.0" // map at the start of the data section
)

// sparseScionTarball returns a gzipped tar whose only entry is a PAX GNU
// sparse file named "scion" in the given encoding, with the given real
// size: 4 bytes of data, the rest holes that archive/tar synthesizes as
// zeros without reading any input.
func sparseScionTarball(t *testing.T, version string, realsize int64) []byte {
	t.Helper()
	size := fmt.Sprint(realsize)
	var kvs [][2]string
	name := "scion"
	data := []byte("ABCD")
	switch version {
	case sparseV00:
		kvs = [][2]string{{"GNU.sparse.size", size}, {"GNU.sparse.numblocks", "1"},
			{"GNU.sparse.offset", "0"}, {"GNU.sparse.numbytes", "4"}}
	case sparseV01:
		kvs = [][2]string{{"GNU.sparse.major", "0"}, {"GNU.sparse.minor", "1"},
			{"GNU.sparse.size", size}, {"GNU.sparse.numblocks", "1"}, {"GNU.sparse.map", "0,4"}}
	case sparseV01NoVersion:
		// No GNU.sparse.major/minor: archive/tar still guesses 0.1 from
		// the map, so the check must not key on the version records.
		kvs = [][2]string{{"GNU.sparse.realsize", size}, {"GNU.sparse.numblocks", "1"}, {"GNU.sparse.map", "0,4"}}
	case sparseV10:
		kvs = [][2]string{{"GNU.sparse.major", "1"}, {"GNU.sparse.minor", "0"},
			{"GNU.sparse.name", "scion"}, {"GNU.sparse.realsize", size}}
		name = "GNUSparseFile.0/scion"
		data = append(padTarBlock([]byte("1\n0\n4\n")), data...)
	default:
		t.Fatalf("unknown sparse version %q", version)
	}
	var recs string
	for _, kv := range kvs {
		recs += paxRecord(kv[0], kv[1])
	}
	var raw []byte
	raw = append(raw, rawTarBlock("PaxHeaders/scion", 'x', int64(len(recs)))...)
	raw = append(raw, padTarBlock([]byte(recs))...)
	raw = append(raw, rawTarBlock(name, '0', int64(len(data)))...)
	raw = append(raw, padTarBlock(data)...)
	raw = append(raw, make([]byte, 1024)...)

	// The fixture really is sparse: archive/tar reports it as a regular
	// "scion" of the real size and produces that many bytes from a ~2 KiB
	// stream.
	tr := tar.NewReader(bytes.NewReader(raw))
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "scion" || hdr.Typeflag != tar.TypeReg || hdr.Size != realsize {
		t.Fatalf("%s fixture not parsed as sparse: hdr=%+v err=%v", version, hdr, err)
	}
	if realsize <= 1<<20 {
		if n, err := io.Copy(io.Discard, tr); err != nil || n != realsize {
			t.Fatalf("%s fixture produced %d bytes (%v), want %d", version, n, err, realsize)
		}
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractScionBinary_SparseAndDeclaredSize(t *testing.T) {
	const testCap = 1 << 20

	for _, v := range []string{sparseV00, sparseV01, sparseV01NoVersion, sparseV10} {
		t.Run("sparse "+v+" scion entry is refused", func(t *testing.T) {
			// Real size under the cap: refused for being sparse, not for size.
			_, err := extractTarball(t, sparseScionTarball(t, v, 64<<10), testCap)
			if err == nil || !strings.Contains(err.Error(), "sparse") {
				t.Fatalf("err = %v, want sparse-entry refusal", err)
			}
		})
	}

	t.Run("sparse scion entry beyond the cap writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "release.tar.gz")
		if err := os.WriteFile(p, sparseScionTarball(t, sparseV10, 8*testCap), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := extractScionBinary(p, dir, &bytes.Buffer{}, testCap); err == nil {
			t.Fatal("sparse entry beyond the cap accepted")
		}
		if _, err := os.Stat(filepath.Join(dir, "extracted", "scion")); !os.IsNotExist(err) {
			t.Fatalf("scion left behind for a refused sparse entry: %v", err)
		}
	})

	t.Run("declared size over the cap is refused before writing", func(t *testing.T) {
		// A header claiming more than the cap; the stream itself is small
		// (the body is truncated), so only the header check can catch it
		// before any write.
		var raw []byte
		raw = append(raw, rawTarBlock("scion", '0', testCap+1)...)
		raw = append(raw, make([]byte, 512)...)
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		_, _ = gw.Write(raw)
		_ = gw.Close()

		dir := t.TempDir()
		p := filepath.Join(dir, "release.tar.gz")
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := extractScionBinary(p, dir, &bytes.Buffer{}, testCap)
		if !errors.Is(err, errExtractTooLarge) {
			t.Fatalf("err = %v, want size-limit error", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "extracted", "scion")); !os.IsNotExist(err) {
			t.Fatalf("scion was written despite the declared size: %v", err)
		}
	})

	t.Run("declared size under the cap is accepted", func(t *testing.T) {
		// The stream cap also counts the 512-byte header, so through
		// extractScionBinary an entry can never be exactly maxBytes; the
		// exact bound is tested on writeExtracted directly.
		body := make([]byte, 4096)
		if _, err := extractTarball(t, orderedTarball(t, tarEntry{name: "scion", body: body}), int64(len(body))+4096); err != nil {
			t.Fatalf("extract: %v", err)
		}
	})
}

func TestExtractScionBinary_StreamCapMidCopyCleansUp(t *testing.T) {
	// The header fits and declares less than the cap, but header plus body
	// exceed it, so the stream cap trips while the binary is being written.
	body := make([]byte, 8192)
	tarball := orderedTarball(t, tarEntry{name: "scion", body: body})
	dir := t.TempDir()
	p := filepath.Join(dir, "release.tar.gz")
	if err := os.WriteFile(p, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := extractScionBinary(p, dir, &bytes.Buffer{}, int64(len(body))+100)
	if !errors.Is(err, errExtractTooLarge) {
		t.Fatalf("err = %v, want size-limit error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "extracted", "scion")); !os.IsNotExist(err) {
		t.Fatalf("partial scion left behind: %v", err)
	}
}

// oldGNUSparseScionTarball returns a gzipped tar whose only entry is an
// old-GNU sparse file (Typeflag 'S', GNU magic, sparse map in the header)
// named "scion": 4 stored bytes at offset 0, realsize bytes in total.
// archive/tar's writer cannot produce this format, so it is built by hand.
func oldGNUSparseScionTarball(t *testing.T, realsize int64) []byte {
	t.Helper()
	b := make([]byte, 512)
	copy(b[0:100], "scion")
	copy(b[100:108], "0000755\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], fmt.Sprintf("%011o\x00", 4)) // stored data size
	copy(b[136:148], "00000000000\x00")
	b[156] = tar.TypeGNUSparse
	copy(b[257:265], "ustar  \x00")                      // GNU magic + version
	copy(b[386:398], fmt.Sprintf("%011o\x00", 0))        // sparse[0].offset
	copy(b[398:410], fmt.Sprintf("%011o\x00", 4))        // sparse[0].numbytes
	b[482] = 0                                           // isextended
	copy(b[483:495], fmt.Sprintf("%011o\x00", realsize)) // realsize
	for i := 148; i < 156; i++ {
		b[i] = ' '
	}
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))

	raw := append(b, padTarBlock([]byte("ABCD"))...)
	raw = append(raw, make([]byte, 1024)...)

	// archive/tar reads it as an old-GNU sparse "scion" of the real size.
	tr := tar.NewReader(bytes.NewReader(raw))
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "scion" || hdr.Typeflag != tar.TypeGNUSparse || hdr.Size != realsize {
		t.Fatalf("old-GNU sparse fixture not parsed as such: hdr=%+v err=%v", hdr, err)
	}
	if n, err := io.Copy(io.Discard, tr); err != nil || n != realsize {
		t.Fatalf("old-GNU sparse fixture produced %d bytes (%v), want %d", n, err, realsize)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractScionBinary_OldGNUSparseRefused(t *testing.T) {
	const testCap = 1 << 20
	for name, realsize := range map[string]int64{
		"under the cap":  64 << 10,
		"beyond the cap": 8 * testCap,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "release.tar.gz")
			if err := os.WriteFile(p, oldGNUSparseScionTarball(t, realsize), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := extractScionBinary(p, dir, &bytes.Buffer{}, testCap); err == nil {
				t.Fatal("old-GNU sparse scion entry accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, "extracted", "scion")); !os.IsNotExist(err) {
				t.Fatalf("scion written for a refused old-GNU sparse entry: %v", err)
			}
		})
	}
	// isSparseEntry also recognises the format on its own (defence in depth
	// behind the regular-file check).
	if !isSparseEntry(&tar.Header{Typeflag: tar.TypeGNUSparse}) {
		t.Fatal("isSparseEntry does not recognise TypeGNUSparse")
	}
}

// boundedZeros yields zeros and counts what is read. It fails once more
// than limit bytes are requested, so a copy that ignores its own bound
// errors out instead of filling the disk.
type boundedZeros struct {
	read, limit int64
}

func (z *boundedZeros) Read(p []byte) (int, error) {
	if z.read >= z.limit {
		return 0, errors.New("read past the test's hard limit")
	}
	if int64(len(p)) > z.limit-z.read {
		p = p[:z.limit-z.read]
	}
	clear(p)
	z.read += int64(len(p))
	return len(p), nil
}

func TestWriteExtracted_StopsReadingAtTheBound(t *testing.T) {
	const bound = 4096
	src := &boundedZeros{limit: 1 << 20} // effectively endless for this bound
	dst := filepath.Join(t.TempDir(), "scion")
	if err := writeExtracted(dst, src, bound); !errors.Is(err, errExtractTooLarge) {
		t.Fatalf("err = %v, want size-limit error", err)
	}
	if src.read > bound+1 {
		t.Fatalf("read %d bytes from the source, want at most bound+1 = %d", src.read, bound+1)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("oversize output left behind: %v", err)
	}
}

func TestWriteExtracted_OutputBound(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "scion")
	if err := writeExtracted(dst, bytes.NewReader(make([]byte, 101)), 100); !errors.Is(err, errExtractTooLarge) {
		t.Fatalf("101 bytes with a 100-byte bound: err = %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("oversize output left behind: %v", err)
	}
	if err := writeExtracted(dst, bytes.NewReader(make([]byte, 100)), 100); err != nil {
		t.Fatalf("exactly at the bound: %v", err)
	}
	if fi, _ := os.Stat(dst); fi.Size() != 100 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("size=%d mode=%v", fi.Size(), fi.Mode().Perm())
	}
}
