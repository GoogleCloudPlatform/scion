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
	"context"
	"errors"
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

func TestFetchGitHubTarball_LinkedContent(t *testing.T) {
	skipWithoutSymlinks(t)
	parent, outside := outsideFixture(t)
	entries := []tarEntry{
		{typ: tar.TypeDir, name: "repo-main/", mode: 0o755},
		{typ: tar.TypeReg, name: "repo-main/templates/foo/scion-agent.yaml", body: "schema_version: \"1\"\n"},
		{typ: tar.TypeSymlink, name: "repo-main/templates/foo/abs-file", linkname: filepath.Join(outside, "secret.txt")},
		{typ: tar.TypeSymlink, name: "repo-main/templates/foo/rel-file", linkname: "../../../outside/secret.txt"},
		{typ: tar.TypeSymlink, name: "repo-main/templates/foo/abs-dir", linkname: outside},
		{typ: tar.TypeSymlink, name: "repo-main/templates/foo/rel-dir", linkname: "../../../outside"},
		{typ: tar.TypeSymlink, name: "repo-main/templates/linked", linkname: outside},
		{typ: tar.TypeSymlink, name: "repo-main/templates/alias", linkname: "foo"},
	}
	serveTarball(t, buildTarGz(t, entries))

	t.Run("links inside the template are dropped", func(t *testing.T) {
		dest := filepath.Join(parent, "dest-ok")
		require.NoError(t, os.MkdirAll(dest, 0o755))
		parts := &GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "main", Path: "templates/foo"}
		require.NoError(t, fetchGitHubTarball(context.Background(), parts, dest, ""))
		assert.Equal(t, map[string]string{"scion-agent.yaml": "schema_version: \"1\"\n"}, readTree(t, dest))
		assertRegularOnly(t, dest)
		assertOutsideUntouched(t, outside)
	})

	t.Run("whole repo without a sub-path", func(t *testing.T) {
		dest := filepath.Join(parent, "dest-all")
		require.NoError(t, os.MkdirAll(dest, 0o755))
		parts := &GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "main"}
		require.NoError(t, fetchGitHubTarball(context.Background(), parts, dest, ""))
		assert.Equal(t, map[string]string{"templates/foo/scion-agent.yaml": "schema_version: \"1\"\n"}, readTree(t, dest))
		assertRegularOnly(t, dest)
		assertOutsideUntouched(t, outside)
	})

	for _, sub := range []string{"templates/linked", "templates/alias", "../outside", "templates/../../outside"} {
		t.Run("sub-path "+sub+" fails closed", func(t *testing.T) {
			dest := t.TempDir()
			parts := &GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "main", Path: sub}
			require.Error(t, fetchGitHubTarball(context.Background(), parts, dest, ""))
			assert.Empty(t, readTree(t, dest))
			assertOutsideUntouched(t, outside)
		})
	}
}

// installCheckoutFakeGit puts a fake git on PATH whose `checkout` writes a
// tree into the working directory the way git materialises a checkout:
// regular files plus absolute and relative file and directory symlinks
// (pointing at the directory named by SCION_FAKE_OUTSIDE). Every other
// subcommand succeeds without doing anything.
func installCheckoutFakeGit(t *testing.T, outside string) {
	t.Helper()
	binDir := t.TempDir()
	script := `#!/bin/sh
while [ "$1" = "-c" ]; do shift 2; done
case "$1" in
checkout)
  mkdir -p .git templates/foo/home
  printf 'schema_version: "1"\n' > templates/foo/scion-agent.yaml
  printf 'export FOO=1\n' > templates/foo/home/.bashrc
  printf 'ref\n' > .git/HEAD
  printf '# repo\n' > README.md
  ln -s "$SCION_FAKE_OUTSIDE/secret.txt" templates/foo/abs-file
  ln -s "$SCION_FAKE_OUTSIDE" templates/foo/abs-dir
  ln -s "$SCION_FAKE_REL/secret.txt" templates/foo/rel-file
  ln -s "$SCION_FAKE_REL" templates/foo/rel-dir
  ln -s scion-agent.yaml templates/foo/inside-file
  ln -s "$SCION_FAKE_OUTSIDE" templates/linked
  ln -s "$SCION_FAKE_OUTSIDE/secret.txt" top-file
  ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	// The fake needs mkdir/printf/ln, so keep the system PATH after it.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCION_FAKE_OUTSIDE", outside)
}

func TestExecSparseGitCheckout_LinkedContent(t *testing.T) {
	skipWithoutSymlinks(t)
	_, outside := outsideFixture(t)
	installCheckoutFakeGit(t, outside)
	// The checkout lands in a fresh temp dir; a relative link with enough
	// ".." segments reaches the file-system root, then the outside dir.
	t.Setenv("SCION_FAKE_REL", strings.Repeat("../", 32)+strings.TrimPrefix(outside, "/"))

	t.Run("template sub-path", func(t *testing.T) {
		dest := t.TempDir()
		parts := &GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "main", Path: "templates/foo"}
		require.NoError(t, execSparseGitCheckout(context.Background(), parts, dest, ""))
		assert.Equal(t, map[string]string{
			"scion-agent.yaml": "schema_version: \"1\"\n",
			"home/.bashrc":     "export FOO=1\n",
		}, readTree(t, dest))
		assertRegularOnly(t, dest)
		assertOutsideUntouched(t, outside)
	})

	t.Run("whole repo", func(t *testing.T) {
		dest := t.TempDir()
		parts := &GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "main"}
		require.NoError(t, execSparseGitCheckout(context.Background(), parts, dest, ""))
		assert.Equal(t, map[string]string{
			"README.md":                      "# repo\n",
			"templates/foo/scion-agent.yaml": "schema_version: \"1\"\n",
			"templates/foo/home/.bashrc":     "export FOO=1\n",
		}, readTree(t, dest))
		assertRegularOnly(t, dest)
		assertOutsideUntouched(t, outside)
	})

	for _, sub := range []string{"templates/linked", "templates/foo/abs-dir", "templates/foo/rel-dir", "top-file", "../.."} {
		t.Run("sub-path "+sub+" fails closed", func(t *testing.T) {
			dest := t.TempDir()
			parts := &GitHubURLParts{Owner: "acme", Repo: "repo", Branch: "main", Path: sub}
			require.Error(t, execSparseGitCheckout(context.Background(), parts, dest, ""))
			assert.Empty(t, readTree(t, dest))
			assertOutsideUntouched(t, outside)
		})
	}
}

// TestFetchRemoteTemplate_RefusesNonRegularResult pins the final check in
// FetchRemoteTemplate: whichever fetch path produced the cache, a result that
// holds a link is refused and the cache is removed.
func TestFetchRemoteTemplate_RefusesNonRegularResult(t *testing.T) {
	skipWithoutSymlinks(t)
	t.Setenv("HOME", t.TempDir())
	_, outside := outsideFixture(t)
	t.Cleanup(SetGitLsRemoteForTest(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("ls-remote disabled in test")
	}))
	old := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	http.DefaultClient.Transport = remoteTemplatesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})

	var cache string
	t.Cleanup(SetGitSparseCheckoutForTest(func(_ context.Context, _ *GitHubURLParts, destPath, _ string) error {
		cache = destPath
		if err := os.WriteFile(filepath.Join(destPath, "scion-agent.yaml"), []byte("x\n"), 0o644); err != nil {
			return err
		}
		return os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(destPath, "leak"))
	}))

	_, err := FetchRemoteTemplate(context.Background(), "https://github.com/acme/repo/tree/main/templates/foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-regular file: leak")
	require.NotEmpty(t, cache)
	_, statErr := os.Lstat(cache)
	assert.True(t, os.IsNotExist(statErr), "the refused cache is removed")
	assertOutsideUntouched(t, outside)

	// A regular result is returned unchanged.
	t.Cleanup(SetGitSparseCheckoutForTest(func(_ context.Context, _ *GitHubURLParts, destPath, _ string) error {
		return os.WriteFile(filepath.Join(destPath, "scion-agent.yaml"), []byte("x\n"), 0o644)
	}))
	got, err := FetchRemoteTemplate(context.Background(), "https://github.com/acme/repo/tree/main/templates/foo")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"scion-agent.yaml": "x\n"}, readTree(t, got))
}

// TestFetchGitHubFolder_RefusalKeepsClearError pins that a refused sub-path
// or refused tarball content is reported as is, without a fallback to the
// sparse checkout (which would replace the error with a git failure).
func TestFetchGitHubFolder_RefusalKeepsClearError(t *testing.T) {
	skipWithoutSymlinks(t)
	t.Cleanup(SetGitLsRemoteForTest(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("ls-remote disabled in test")
	}))
	requests := 0
	old := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	body := buildTarGz(t, []tarEntry{
		{typ: tar.TypeDir, name: "repo-main/", mode: 0o755},
		{typ: tar.TypeReg, name: "repo-main/templates/file", body: "x\n"},
		{typ: tar.TypeReg, name: "repo-main/templates/foo/scion-agent.yaml", body: "schema_version: \"1\"\n"},
	})
	http.DefaultClient.Transport = remoteTemplatesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})
	sparseCalls := 0
	t.Cleanup(SetGitSparseCheckoutForTest(func(context.Context, *GitHubURLParts, string, string) error {
		sparseCalls++
		return errors.New("git init failed (stub)")
	}))

	tests := []struct {
		name, uri, wantErr string
		wantRequests       int
	}{
		{"dot-dot sub-path", "https://github.com/acme/repo/tree/main/templates/../../x", "not inside the repository", 0},
		{"git sub-path", "https://github.com/acme/repo/tree/main/.git", "inside a .git directory", 0},
		{"sub-path is a file", "https://github.com/acme/repo/tree/main/templates/file", "is not a directory", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests, sparseCalls = 0, 0
			dest := t.TempDir()
			err := fetchGitHubFolder(context.Background(), tt.uri, dest, "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.ErrorIs(t, err, errRemoteContentRefused)
			assert.Equal(t, tt.wantRequests, requests)
			assert.Zero(t, sparseCalls, "no sparse-checkout fallback after a refusal")
			assert.Empty(t, readTree(t, dest))
		})
	}

	// A sub-path that is missing from the tarball still falls back.
	requests, sparseCalls = 0, 0
	err := fetchGitHubFolder(context.Background(), "https://github.com/acme/repo/tree/main/templates/missing", t.TempDir(), "")
	require.Error(t, err)
	assert.Equal(t, 1, sparseCalls)
}
