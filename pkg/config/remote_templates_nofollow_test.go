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

package config

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests in this file pin that fetching a remote template never follows,
// recreates or copies through file-system links found in the fetched content,
// and never writes outside the destination.

const outsideSecret = "outside-content-marker\n"

func skipWithoutSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixtures require a POSIX file system")
	}
}

// outsideFixture creates parent/outside/secret.txt and returns parent and the
// outside directory. Fixture repos and destinations are created under parent
// so relative links can reach the outside directory.
func outsideFixture(t *testing.T) (parent, outside string) {
	t.Helper()
	parent = t.TempDir()
	outside = filepath.Join(parent, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(outsideSecret), 0o644))
	return parent, outside
}

// assertRegularOnly fails if dir contains anything other than directories
// and regular files, or any file whose content is the outside secret.
func assertRegularOnly(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			t.Errorf("non-regular entry %s (%s) in result", path, d.Type())
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "outside-content-marker") {
			t.Errorf("%s contains content from outside the source", path)
		}
		return nil
	}))
}

// assertOutsideUntouched fails if anything other than secret.txt (with its
// original content) exists in the outside directory.
func assertOutsideUntouched(t *testing.T, outside string) {
	t.Helper()
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"secret.txt"}, names, "nothing may be written outside the destination")
	b, err := os.ReadFile(filepath.Join(outside, "secret.txt"))
	require.NoError(t, err)
	assert.Equal(t, outsideSecret, string(b))
}

// tarEntry is one header (plus body for regular files) of a test archive.
type tarEntry struct {
	typ      byte
	name     string
	linkname string
	body     string
	mode     int64
}

func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Typeflag: e.typ, Name: e.name, Linkname: e.linkname, Mode: mode}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if e.typ == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func writeArchive(t *testing.T, data []byte, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, data, 0o644))
	return p
}

// linkedTarEntries returns archive entries under root/ with absolute,
// relative and chained links, hard links, and regular files written through
// link names.
func linkedTarEntries(outside string) []tarEntry {
	return []tarEntry{
		{typ: tar.TypeDir, name: "root/", mode: 0o755},
		{typ: tar.TypeReg, name: "root/scion-agent.yaml", body: "schema_version: \"1\"\n"},
		{typ: tar.TypeDir, name: "root/home/", mode: 0o755},
		{typ: tar.TypeReg, name: "root/home/.bashrc", body: "export FOO=1\n"},
		{typ: tar.TypeSymlink, name: "root/abs-file", linkname: filepath.Join(outside, "secret.txt")},
		{typ: tar.TypeSymlink, name: "root/abs-dir", linkname: outside},
		{typ: tar.TypeSymlink, name: "root/rel-file", linkname: "../outside/secret.txt"},
		{typ: tar.TypeSymlink, name: "root/rel-dir", linkname: "../outside"},
		{typ: tar.TypeSymlink, name: "root/inside", linkname: "scion-agent.yaml"},
		// A chain of relative links, each lexically inside the archive.
		{typ: tar.TypeSymlink, name: "root/a", linkname: "."},
		{typ: tar.TypeSymlink, name: "root/home/b", linkname: "../a/a/home"},
		{typ: tar.TypeSymlink, name: "root/c", linkname: "home/b/../../outside"},
		{typ: tar.TypeLink, name: "root/hard", linkname: filepath.Join(outside, "secret.txt")},
		{typ: tar.TypeLink, name: "root/hard-rel", linkname: "root/scion-agent.yaml"},
		// Regular files named below the link names land as regular files
		// in real directories inside the destination.
		{typ: tar.TypeReg, name: "root/abs-dir/data.txt", body: "data\n"},
		{typ: tar.TypeReg, name: "root/rel-dir/data.txt", body: "data\n"},
		{typ: tar.TypeReg, name: "root/c/data.txt", body: "data\n"},
		{typ: tar.TypeReg, name: "root/abs-file", body: "replaced\n"},
	}
}

func TestExtractTarGz_SkipsLinksAndStaysInside(t *testing.T) {
	skipWithoutSymlinks(t)
	parent, outside := outsideFixture(t)
	archive := writeArchive(t, buildTarGz(t, linkedTarEntries(outside)), "a.tar.gz")
	dest := filepath.Join(parent, "dest")
	require.NoError(t, os.MkdirAll(dest, 0o755))

	require.NoError(t, extractTarGz(archive, dest))

	assert.Equal(t, map[string]string{
		"scion-agent.yaml": "schema_version: \"1\"\n",
		"home/.bashrc":     "export FOO=1\n",
		"abs-dir/data.txt": "data\n",
		"rel-dir/data.txt": "data\n",
		"c/data.txt":       "data\n",
		"abs-file":         "replaced\n",
	}, readTree(t, dest))
	assertRegularOnly(t, dest)
	assertOutsideUntouched(t, outside)
}

// TestExtractTarGz_RefusesWriteThroughExistingLink pins the real-path
// confinement (not just the lexical name checks): a link already present in
// the destination is not written through.
func TestExtractTarGz_RefusesWriteThroughExistingLink(t *testing.T) {
	skipWithoutSymlinks(t)
	for _, entry := range []tarEntry{
		{typ: tar.TypeReg, name: "root/linked/data.txt", body: "data\n"},
		{typ: tar.TypeDir, name: "root/linked/sub/", mode: 0o755},
		{typ: tar.TypeReg, name: "root/linked-rel/data.txt", body: "data\n"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			parent, outside := outsideFixture(t)
			dest := filepath.Join(parent, "dest")
			require.NoError(t, os.MkdirAll(dest, 0o755))
			require.NoError(t, os.Symlink(outside, filepath.Join(dest, "linked")))
			require.NoError(t, os.Symlink("../outside", filepath.Join(dest, "linked-rel")))
			archive := writeArchive(t, buildTarGz(t, []tarEntry{
				{typ: tar.TypeDir, name: "root/", mode: 0o755},
				entry,
			}), "a.tar.gz")

			require.Error(t, extractTarGz(archive, dest))
			assertOutsideUntouched(t, outside)
		})
	}
}

func TestExtractTarGz_KeepsLexicalChecks(t *testing.T) {
	parent, outside := outsideFixture(t)
	dest := filepath.Join(parent, "dest")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	archive := writeArchive(t, buildTarGz(t, []tarEntry{
		{typ: tar.TypeReg, name: "ok.txt", body: "ok\n"},
		{typ: tar.TypeReg, name: "../outside/data.txt", body: "data\n"},
		{typ: tar.TypeReg, name: "x/../../outside/data2.txt", body: "data\n"},
		{typ: tar.TypeReg, name: filepath.Join(outside, "data3.txt"), body: "data\n"},
	}), "a.tar.gz")

	// archive/tar may itself refuse insecure names (GODEBUG
	// tarinsecurepath=0); either way nothing lands outside the destination.
	err := extractTarGz(archive, dest)
	assertOutsideUntouched(t, outside)
	assertRegularOnly(t, dest)
	if err == nil {
		assert.Equal(t, "ok\n", readTree(t, dest)["ok.txt"])
	}
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		hdr.SetMode(e.mode)
		w, err := zw.CreateHeader(hdr)
		require.NoError(t, err)
		_, err = io.WriteString(w, e.body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestExtractZip_SkipsLinksAndStaysInside(t *testing.T) {
	skipWithoutSymlinks(t)
	parent, outside := outsideFixture(t)
	dest := filepath.Join(parent, "dest")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	archive := writeArchive(t, buildZip(t, []zipEntry{
		{name: "root/", mode: fs.ModeDir | 0o755},
		{name: "root/scion-agent.yaml", body: "schema_version: \"1\"\n", mode: 0o644},
		{name: "root/abs-file", body: filepath.Join(outside, "secret.txt"), mode: fs.ModeSymlink | 0o777},
		{name: "root/abs-dir", body: outside, mode: fs.ModeSymlink | 0o777},
		{name: "root/rel-dir", body: "../outside", mode: fs.ModeSymlink | 0o777},
		{name: "root/abs-dir/data.txt", body: "data\n", mode: 0o644},
		{name: "root/rel-dir/data.txt", body: "data\n", mode: 0o644},
	}), "a.zip")

	require.NoError(t, extractZip(archive, dest))
	assert.Equal(t, map[string]string{
		"scion-agent.yaml": "schema_version: \"1\"\n",
		"abs-dir/data.txt": "data\n",
		"rel-dir/data.txt": "data\n",
	}, readTree(t, dest))
	assertRegularOnly(t, dest)
	assertOutsideUntouched(t, outside)
}

func TestExtractZip_RefusesWriteThroughExistingLink(t *testing.T) {
	skipWithoutSymlinks(t)
	parent, outside := outsideFixture(t)
	dest := filepath.Join(parent, "dest")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(dest, "linked")))
	archive := writeArchive(t, buildZip(t, []zipEntry{
		{name: "root/ok.txt", body: "ok\n", mode: 0o644},
		{name: "root/linked/data.txt", body: "data\n", mode: 0o644},
	}), "a.zip")

	require.Error(t, extractZip(archive, dest))
	assertOutsideUntouched(t, outside)
}

// serveTarball makes http.DefaultClient answer every request with body.
// Not safe for t.Parallel(): it replaces the global transport.
func serveTarball(t *testing.T, body []byte) {
	t.Helper()
	old := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	http.DefaultClient.Transport = remoteTemplatesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})
}

func TestCheckRegularTree(t *testing.T) {
	skipWithoutSymlinks(t)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "b.txt"), []byte("b\n"), 0o644))
	require.NoError(t, checkRegularTree(dir))

	require.NoError(t, os.Symlink("b.txt", filepath.Join(dir, "a", "link")))
	err := checkRegularTree(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a/link")
}
