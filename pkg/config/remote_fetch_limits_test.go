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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/sync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFetchRemoteTemplateWithOptions_CountsImplicitDirectories: the
// directories a deep file path creates count toward the file limit, so an
// archive of a few files cannot create many directories.
func TestFetchRemoteTemplateWithOptions_CountsImplicitDirectories(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	deep := "root/" + strings.Repeat("d/", 30) + "f"
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: deep, Mode: 0o644, Typeflag: tar.TypeReg}))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	archive := buf.Bytes()
	download := func(context.Context, string, string) ([]byte, error) { return archive, nil }

	_, err := FetchRemoteTemplateWithOptions(context.Background(), "https://artifacts.example.com/t.tar.gz",
		FetchOptions{Download: download, MaxFiles: 10})
	require.ErrorIs(t, err, ErrRemoteFetchLimit)

	_, err = FetchRemoteTemplateWithOptions(context.Background(), "https://artifacts.example.com/t.tar.gz",
		FetchOptions{Download: download, MaxFiles: 31})
	require.NoError(t, err, "30 directories and one file fit a limit of 31")
}

// TestFetchRemoteTemplateWithOptions_RcloneCapIsPerFetch: the rclone
// transfer cap counts one fetch only, not every fetch since the process
// started.
func TestFetchRemoteTemplateWithOptions_RcloneCapIsPerFetch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f"), []byte(strings.Repeat("x", 3000)), 0o644))
	opts := FetchOptions{MaxBytes: 4000, MaxFiles: 10, PrivateCacheDir: true}
	for i := 0; i < 3; i++ {
		_, err := FetchRemoteTemplateWithOptions(context.Background(), ":local:"+src, opts)
		require.NoError(t, err, "fetch %d", i)
	}
}

// TestFetchRemoteTemplateWithOptions_RcloneSyncEnforcesFileCap: limitedDestFs
// stops sync.Sync with ErrRemoteFetchLimit even for 0-byte objects that a
// byte-only MaxTransfer cap would not stop.
func TestFetchRemoteTemplateWithOptions_RcloneSyncEnforcesFileCap(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	for i := 0; i < 5; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("zero-%d", i)), nil, 0o644))
	}
	ctx := accounting.WithStatsGroup(context.Background(), "test-rclone-sync-file-cap")
	defer deleteRcloneStatsGroup("test-rclone-sync-file-cap")
	srcFs, err := fs.NewFs(ctx, ":local:"+src)
	require.NoError(t, err)
	rawDst, err := fs.NewFs(ctx, dst)
	require.NoError(t, err)

	err = sync.Sync(ctx, &limitedDestFs{Fs: rawDst, maxFiles: 2}, srcFs, false)
	require.ErrorIs(t, err, ErrRemoteFetchLimit)
}

// TestFetchRemoteTemplateWithOptions_UnsupportedTypeDoesNotEchoURI: in a
// limited fetch, an unsupported remote type does not echo the URI.
func TestFetchRemoteTemplateWithOptions_UnsupportedTypeDoesNotEchoURI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const uri = "https://artifacts.example.com/secret-path/not-an-archive"
	_, err := FetchRemoteTemplateWithOptions(context.Background(), uri, FetchOptions{MaxFiles: 10, PrivateCacheDir: true})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-path")
	assert.NotContains(t, err.Error(), uri)
}

// TestFetchRemoteTemplateWithOptions_PrivateCacheDir: each fetch gets its
// own directory.
func TestFetchRemoteTemplateWithOptions_PrivateCacheDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644))
	a, err := FetchRemoteTemplateWithOptions(context.Background(), ":local:"+src, FetchOptions{PrivateCacheDir: true})
	require.NoError(t, err)
	b, err := FetchRemoteTemplateWithOptions(context.Background(), ":local:"+src, FetchOptions{PrivateCacheDir: true})
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	assert.FileExists(t, filepath.Join(a, "f"))
	assert.FileExists(t, filepath.Join(b, "f"))
}
