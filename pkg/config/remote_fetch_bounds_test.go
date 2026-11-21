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
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zipOf builds a zip whose entries are given by add.
func zipOf(t *testing.T, add func(w *zip.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	add(w)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// addZipSymlinks adds n tiny symbolic-link entries, which extraction skips
// without counting them, so only a bound applied before the central
// directory is parsed refuses them.
func addZipSymlinks(t *testing.T, w *zip.Writer, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		h := &zip.FileHeader{Name: fmt.Sprintf("l%d", i)}
		h.SetMode(os.ModeSymlink | 0o777)
		fw, err := w.CreateHeader(h)
		require.NoError(t, err)
		_, err = fw.Write([]byte("t"))
		require.NoError(t, err)
	}
}

// rawZipDirectory builds a zip that is only a central directory of n
// symbolic-link records (named n0, n1, ...) and an end record declaring
// declared entries. The Go zip reader checks the declared count only
// modulo 65536 and reads records until it meets another signature.
func rawZipDirectory(n int, declared uint16) []byte {
	var cd bytes.Buffer
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("n%d", i)
		rec := make([]byte, 46)
		binary.LittleEndian.PutUint32(rec[0:], 0x02014b50)
		binary.LittleEndian.PutUint16(rec[4:], 3<<8|20) // made by Unix
		binary.LittleEndian.PutUint16(rec[6:], 20)
		binary.LittleEndian.PutUint16(rec[28:], uint16(len(name)))
		binary.LittleEndian.PutUint32(rec[38:], uint32(0o120777)<<16) // symlink
		cd.Write(rec)
		cd.WriteString(name)
	}
	end := make([]byte, 22)
	binary.LittleEndian.PutUint32(end[0:], 0x06054b50)
	binary.LittleEndian.PutUint16(end[8:], declared)
	binary.LittleEndian.PutUint16(end[10:], declared)
	binary.LittleEndian.PutUint32(end[12:], uint32(cd.Len()))
	binary.LittleEndian.PutUint32(end[16:], 0)
	return append(cd.Bytes(), end...)
}

// TestFetchRemoteTemplateWithOptions_ZipBoundedBeforeParse: a limited fetch
// refuses a zip whose central directory is over the limits before the
// directory is parsed into memory, including entries extraction would skip
// and an end record whose entry count does not match the directory.
func TestFetchRemoteTemplateWithOptions_ZipBoundedBeforeParse(t *testing.T) {
	const uri = "https://artifacts.example.com/t.zip"
	fetch := func(t *testing.T, archive []byte, maxFiles int) error {
		t.Setenv("HOME", t.TempDir())
		download := func(context.Context, string, string) ([]byte, error) { return archive, nil }
		_, err := FetchRemoteTemplateWithOptions(context.Background(), uri,
			FetchOptions{Download: download, MaxFiles: maxFiles, PrivateCacheDir: true})
		return err
	}

	t.Run("many tiny entries", func(t *testing.T) {
		archive := zipOf(t, func(w *zip.Writer) { addZipSymlinks(t, w, 500) })
		err := fetch(t, archive, 100)
		require.ErrorIs(t, err, ErrRemoteFetchLimit)
		assert.Contains(t, err.Error(), "more than 100 archive entries")
	})

	t.Run("entry count understated in the end record", func(t *testing.T) {
		// 65536+10 records declared as 10: the Go reader accepts it.
		archive := rawZipDirectory(65536+10, 10)
		_, zerr := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		require.NoError(t, zerr, "the crafted directory must parse")
		// A limit whose directory-size bound (3000 records) admits this
		// directory, so only the record walk refuses it.
		err := fetch(t, archive, 3000)
		require.ErrorIs(t, err, ErrRemoteFetchLimit)
	})

	small := func(t *testing.T) []byte {
		return zipOf(t, func(w *zip.Writer) {
			fw, err := w.Create("root/scion-agent.yaml")
			require.NoError(t, err)
			_, _ = fw.Write([]byte("x"))
		})
	}

	t.Run("zip64 entry count", func(t *testing.T) {
		archive := small(t)
		end := archive[len(archive)-22:]
		binary.LittleEndian.PutUint16(end[8:], 0xffff)
		binary.LittleEndian.PutUint16(end[10:], 0xffff)
		// A limit above 0xffff, so only the ZIP64 check refuses it.
		require.ErrorIs(t, fetch(t, archive, 70000), ErrRemoteFetchLimit)
	})

	t.Run("zip64 locator", func(t *testing.T) {
		// A ZIP64 locator before the end record, counted into the
		// directory size so the directory still ends at the end record.
		archive := small(t)
		end := append([]byte(nil), archive[len(archive)-22:]...)
		locator := make([]byte, 20)
		binary.LittleEndian.PutUint32(locator[0:], 0x07064b50)
		binary.LittleEndian.PutUint32(locator[16:], 1)
		size := binary.LittleEndian.Uint32(end[12:])
		binary.LittleEndian.PutUint32(end[12:], size+20)
		crafted := append(append(append([]byte(nil), archive[:len(archive)-22]...), locator...), end...)
		require.ErrorIs(t, fetch(t, crafted, 100), ErrRemoteFetchLimit)
	})

	t.Run("central directory too large for the entry limit", func(t *testing.T) {
		archive := zipOf(t, func(w *zip.Writer) {
			for i := 0; i < 3; i++ {
				fw, err := w.CreateHeader(&zip.FileHeader{
					Name: fmt.Sprintf("root/f%d", i), Comment: strings.Repeat("c", 60000),
				})
				require.NoError(t, err)
				_, _ = fw.Write([]byte("x"))
			}
		})
		require.ErrorIs(t, fetch(t, archive, 5), ErrRemoteFetchLimit)
	})

	t.Run("entry name too long", func(t *testing.T) {
		archive := zipOf(t, func(w *zip.Writer) {
			h := &zip.FileHeader{Name: strings.Repeat("n", maxFetchEntryName+1)}
			h.SetMode(os.ModeSymlink | 0o777)
			fw, err := w.CreateHeader(h)
			require.NoError(t, err)
			_, _ = fw.Write([]byte("t"))
		})
		err := fetch(t, archive, 100)
		require.ErrorIs(t, err, ErrRemoteFetchLimit)
		assert.Contains(t, err.Error(), "entry name is longer")
	})

	t.Run("records with large comments", func(t *testing.T) {
		// About 4 KB per record: more than a real zip carries, and over
		// the directory-size bound for 10 entries.
		archive := zipOf(t, func(w *zip.Writer) {
			for i := 0; i < 5; i++ {
				fw, err := w.CreateHeader(&zip.FileHeader{
					Name: fmt.Sprintf("root/f%d", i), Comment: strings.Repeat("c", 4000),
				})
				require.NoError(t, err)
				_, _ = fw.Write([]byte("x"))
			}
		})
		require.ErrorIs(t, fetch(t, archive, 10), ErrRemoteFetchLimit)
	})

	t.Run("a zip within the limits still extracts", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		// An archive comment (as on GitHub's zips) and data descriptors
		// (zip.Writer.Create writes them) are accepted.
		archive := zipOf(t, func(w *zip.Writer) {
			require.NoError(t, w.SetComment(strings.Repeat("c", 40)))
			for _, n := range []string{"root/scion-agent.yaml", "root/a/b.txt"} {
				fw, err := w.Create(n)
				require.NoError(t, err)
				_, _ = fw.Write([]byte("x"))
			}
		})
		download := func(context.Context, string, string) ([]byte, error) { return archive, nil }
		path, err := FetchRemoteTemplateWithOptions(context.Background(), uri,
			FetchOptions{Download: download, MaxFiles: 4, PrivateCacheDir: true})
		require.NoError(t, err)
		assert.FileExists(t, filepath.Join(path, "scion-agent.yaml"))
		assert.FileExists(t, filepath.Join(path, "a", "b.txt"))
	})
}

// gitHubTarball builds a GitHub-style tarball (repo-<ref>/ prefix).
func gitHubTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: "repo-x/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// TestFetchRemoteTemplateWithOptions_GitHubTagAndCommitRefs: a limited
// GitHub fetch whose ref is not a branch tries the tag archive, then the
// commit archive, and names the ref when none exists.
func TestFetchRemoteTemplateWithOptions_GitHubTagAndCommitRefs(t *testing.T) {
	restoreLs := SetGitLsRemoteForTest(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("no git")
	})
	t.Cleanup(restoreLs)
	archive := gitHubTarball(t, map[string]string{"templates/scion-agent.yaml": "x"})
	serving := func(okURL string, asked *[]string) RemoteDownloadFunc {
		return func(_ context.Context, rawURL, _ string) ([]byte, error) {
			*asked = append(*asked, rawURL)
			if rawURL == okURL {
				return archive, nil
			}
			return nil, errors.New("the source could not be downloaded")
		}
	}
	const base = "https://github.com/acme/repo/archive/"

	for _, tc := range []struct{ name, ref, okURL string }{
		{"tag", "v1.2.0", base + "refs/tags/v1.2.0.tar.gz"},
		{"commit", "0123456789abcdef0123456789abcdef01234567", base + "0123456789abcdef0123456789abcdef01234567.tar.gz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var asked []string
			path, err := FetchRemoteTemplateWithOptions(context.Background(),
				"https://github.com/acme/repo/tree/"+tc.ref+"/templates",
				FetchOptions{Download: serving(tc.okURL, &asked), MaxFiles: 100, PrivateCacheDir: true})
			require.NoError(t, err, "asked %v", asked)
			assert.FileExists(t, filepath.Join(path, "scion-agent.yaml"))
			assert.Equal(t, base+"refs/heads/"+tc.ref+".tar.gz", asked[0], "a branch is tried first")
		})
	}

	t.Run("no such ref", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		var asked []string
		_, err := FetchRemoteTemplateWithOptions(context.Background(),
			"https://github.com/acme/repo/tree/nope/templates",
			FetchOptions{Download: serving("", &asked), MaxFiles: 100, PrivateCacheDir: true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `branch, tag or commit "nope" was not found`)
		assert.Equal(t, []string{
			base + "refs/heads/nope.tar.gz", base + "refs/tags/nope.tar.gz", base + "nope.tar.gz",
		}, asked)
	})

	t.Run("a download limit or timeout stops the fallback", func(t *testing.T) {
		for _, failure := range []error{
			fmt.Errorf("%w (the download is larger than 64 MiB)", ErrRemoteFetchLimit),
			ErrRemoteDownloadTimeout,
		} {
			t.Setenv("HOME", t.TempDir())
			var asked []string
			download := func(_ context.Context, rawURL, _ string) ([]byte, error) {
				asked = append(asked, rawURL)
				return nil, failure
			}
			_, err := FetchRemoteTemplateWithOptions(context.Background(),
				"https://github.com/acme/repo/tree/v1.2.0/templates",
				FetchOptions{Download: download, MaxFiles: 100, PrivateCacheDir: true})
			require.ErrorIs(t, err, failure)
			assert.NotContains(t, err.Error(), "was not found")
			assert.Equal(t, []string{base + "refs/heads/v1.2.0.tar.gz"}, asked)
		}
	})

	t.Run("whole repository over the limit", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		files := map[string]string{"templates/scion-agent.yaml": "x"}
		for i := 0; i < 20; i++ {
			files[fmt.Sprintf("other/f%d", i)] = "x"
		}
		big := gitHubTarball(t, files)
		download := func(context.Context, string, string) ([]byte, error) { return big, nil }
		_, err := FetchRemoteTemplateWithOptions(context.Background(),
			"https://github.com/acme/repo/tree/main/templates",
			FetchOptions{Download: download, MaxFiles: 10, PrivateCacheDir: true})
		require.ErrorIs(t, err, ErrRemoteFetchLimit)
		assert.Contains(t, err.Error(), "more than 10")
		assert.Contains(t, err.Error(), "whole repository")
		assert.Contains(t, err.Error(), "release archive")
	})
}
