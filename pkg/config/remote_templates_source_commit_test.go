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
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildTestGitHubTarGz(t *testing.T, paxComment string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if paxComment != "" {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeXGlobalHeader,
			Name:       "pax_global_header",
			PAXRecords: map[string]string{"comment": paxComment},
		}))
	}
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

func TestFetchRemoteTemplateWithResult_GitHubTarballPAXCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const wantSHA = "0123456789abcdef0123456789abcdef01234567"

	t.Run("captures valid 40-char PAX commit from GitHub folder tarball", func(t *testing.T) {
		tarball := buildTestGitHubTarGz(t, wantSHA, map[string]string{
			"repo-main/templates/demo/scion-agent.yaml": "harness: claude\n",
		})
		res, err := FetchRemoteTemplateWithResult(context.Background(),
			"https://github.com/acme/repo/tree/main/templates/demo",
			FetchOptions{
				PrivateCacheDir: true,
				Download: func(_ context.Context, _, _ string) ([]byte, error) {
					return tarball, nil
				},
			})
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(res.Path) })
		assert.Equal(t, wantSHA, res.Commit)
		assert.FileExists(t, filepath.Join(res.Path, "scion-agent.yaml"))
	})

	t.Run("returns empty commit when PAX header absent or malformed", func(t *testing.T) {
		for _, comment := range []string{"", "not-a-sha", "0123456789ABCDEF0123456789ABCDEF01234567"} {
			tarball := buildTestGitHubTarGz(t, comment, map[string]string{
				"repo-main/templates/demo/scion-agent.yaml": "harness: claude\n",
			})
			res, err := FetchRemoteTemplateWithResult(context.Background(),
				"https://github.com/acme/repo/tree/main/templates/demo",
				FetchOptions{
					PrivateCacheDir: true,
					Download: func(_ context.Context, _, _ string) ([]byte, error) {
						return tarball, nil
					},
				})
			require.NoError(t, err)
			_ = os.RemoveAll(res.Path)
			assert.Empty(t, res.Commit, "comment=%q", comment)
		}
	})

	t.Run("non-GitHub archive URL never records a source commit", func(t *testing.T) {
		tarball := buildTestGitHubTarGz(t, wantSHA, map[string]string{
			"demo/scion-agent.yaml": "harness: claude\n",
		})
		res, err := FetchRemoteTemplateWithResult(context.Background(),
			"https://example.com/templates/demo.tar.gz",
			FetchOptions{
				PrivateCacheDir: true,
				Download: func(_ context.Context, _, _ string) ([]byte, error) {
					return tarball, nil
				},
			})
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(res.Path) })
		assert.Empty(t, res.Commit)
	})
}

func TestExecSparseGitCheckoutWithCommit_ReadsHeadSHA(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake git is unix-only")
	}
	const wantSHA = "fedcba9876543210fedcba9876543210fedcba98"

	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	script := `#!/bin/sh
# Skip -c key=val pairs from util.HostGitArgs().
while [ "$1" = "-c" ]; do
  shift 2
done
case "$1" in
  init)
    mkdir -p .git/info
    ;;
  checkout)
    mkdir -p templates/demo
    printf 'harness: claude\n' > templates/demo/scion-agent.yaml
    ;;
  rev-parse)
    printf '%s\n' "` + wantSHA + `"
    ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dest := t.TempDir()
	commit, err := execSparseGitCheckoutWithCommit(context.Background(), &GitHubURLParts{
		Owner:  "acme",
		Repo:   "repo",
		Branch: "main",
		Path:   "templates/demo",
	}, dest, "")
	require.NoError(t, err)
	assert.Equal(t, wantSHA, commit)
	assert.FileExists(t, filepath.Join(dest, "scion-agent.yaml"))
}
