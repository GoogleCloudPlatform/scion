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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestCheckHubImportSource_Default: with no allowlist a hosted Hub fetches
// only https://github.com URLs (without query or fragment). Local and other
// rclone backends, Cloud Storage buckets, plain http and other https hosts
// are refused, and the error never repeats the source.
func TestCheckHubImportSource_Default(t *testing.T) {
	allowed := map[string]string{
		"https://github.com/acme/repo/tree/main/templates": "https://github.com/acme/repo/tree/main/templates",
		"https://GitHub.com/acme/repo/tree/main/templates": "https://github.com/acme/repo/tree/main/templates",
		"HTTPS://github.com/acme/repo":                     "https://github.com/acme/repo",
		"https://github.com/acme/repo/archive/main.tar.gz": "https://github.com/acme/repo/archive/main.tar.gz",
	}
	for raw, want := range allowed {
		got, err := CheckHubImportSource(raw, nil)
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}

	refused := []string{
		":local:/",
		":local:/etc",
		":gcs:some-bucket/templates",
		"gs://some-bucket/templates",
		":s3:bucket/templates",
		":gcs,endpoint=https://evil.example:some-bucket/templates",
		"http://github.com/acme/repo/tree/main/templates",
		"http://127.0.0.1:8080/x.tar.gz",
		"https://169.254.169.254/computeMetadata/v1/x.tar.gz",
		"https://internal.example/templates.tar.gz",
		"https://x-access-token:SECRET@github.com/acme/repo",
		"https://github.com:8443/acme/repo",
		"https://github.com/acme/repo/../../other",
		"https://github.com.evil.example/acme/repo",
		"https://github.com/acme/repo/tree/main/templates?token=SECRET",
		"https://github.com/acme/repo/tree/main/templates?",
		"https://github.com/acme/repo/tree/main/templates#frag",
		"ftp://github.com/acme/repo",
		"",
	}
	for _, raw := range refused {
		_, err := CheckHubImportSource(raw, nil)
		require.ErrorIs(t, err, ErrImportSourceNotAllowed, raw)
		if raw != "" {
			assert.NotContains(t, err.Error(), raw)
		}
		assert.NotContains(t, err.Error(), "SECRET")
	}
}

// TestCheckHubImportSource_Allowlist: an operator allowlist admits the
// listed prefixes on whole path segments, and nothing that could change
// rclone's credentials or endpoint, escape the prefix, or reach the Hub's
// disk.
func TestCheckHubImportSource_Allowlist(t *testing.T) {
	allow, err := NormalizeAllowedImportSources([]string{
		"gs://team-bucket/templates/",
		"https://Artifacts.Example.com/scion",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"gs://team-bucket/templates/", "https://artifacts.example.com/scion"}, allow)

	allowed := map[string]string{
		":gcs:team-bucket/templates":                      ":gcs:team-bucket/templates",
		":gcs:team-bucket/templates/reviewer":             ":gcs:team-bucket/templates/reviewer",
		"gs://team-bucket/templates/reviewer/":            ":gcs:team-bucket/templates/reviewer/",
		"https://artifacts.example.com/scion/t.tar.gz":    "https://artifacts.example.com/scion/t.tar.gz",
		"https://ARTIFACTS.example.com:443/scion/a/t.tgz": "https://artifacts.example.com/scion/a/t.tgz",
		"https://github.com/acme/repo":                    "https://github.com/acme/repo",
	}
	for raw, want := range allowed {
		got, err := CheckHubImportSource(raw, allow)
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}

	refused := []string{
		":gcs:team-bucket/templates-old",
		":gcs:team-bucket/templates/../secrets",
		":gcs:team-bucket/templates//x",
		":gcs:other-bucket/templates",
		":gcs,service_account_file=/etc/key.json:team-bucket/templates",
		":gcs,endpoint=http\\://10.0.0.1:team-bucket/templates",
		":googlecloudstorage:team-bucket/templates",
		"team-bucket:templates",
		":local:/srv/templates",
		"http://artifacts.example.com/scion/t.tar.gz",
		"https://artifacts.example.com/scionx/t.tar.gz",
		"https://artifacts.example.com/scion/%2e%2e/admin/t.tar.gz",
		"https://artifacts.example.com/scion%2F..%2Fadmin/t.tar.gz",
		"https://artifacts.example.com/scion/t.tar.gz?x=1",
		"https://artifacts.example.com:8443/scion/t.tar.gz",
		"https://u:p@artifacts.example.com/scion/t.tar.gz",
	}
	for _, raw := range refused {
		_, err := CheckHubImportSource(raw, allow)
		require.ErrorIs(t, err, ErrImportSourceNotAllowed, raw)
	}
}

// TestNormalizeAllowedImportSources_RefusesUnsafeEntries: an entry that
// is not an https or Cloud Storage prefix fails startup, naming the entry by
// position only.
func TestNormalizeAllowedImportSources_RefusesUnsafeEntries(t *testing.T) {
	for _, entry := range []string{
		":local:/",
		"http://artifacts.example.com/",
		":gcs,service_account_file=/k:bucket/templates",
		":s3:bucket",
		"https://u:SECRET@artifacts.example.com/",
		"https://artifacts.example.com/?q",
		"https://github.com/acme/repo?q=1",
		"gs://Bad_Bucket/x",
		"*",
	} {
		_, err := NormalizeAllowedImportSources([]string{"https://ok.example.com/", entry})
		require.Error(t, err, entry)
		assert.Contains(t, err.Error(), AllowedImportSourcesKey+"[1]")
		assert.NotContains(t, err.Error(), "SECRET")
	}
	got, err := NormalizeAllowedImportSources([]string{" ", ""})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// gzipBombTarGz returns a .tar.gz holding n files of size bytes each.
func gzipBombTarGz(t *testing.T, n int, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := make([]byte, size)
	for i := 0; i < n; i++ {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("root/f%04d", i), Mode: 0o644, Size: int64(size), Typeflag: tar.TypeReg}))
		_, err := tw.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// TestFetchRemoteTemplateWithOptions_Limits: with limits set, an archive
// that unpacks past the byte, file or decompressed-stream limit fails with
// ErrRemoteFetchLimit and leaves no cache directory behind; the Download
// hook replaces http.DefaultClient.
func TestFetchRemoteTemplateWithOptions_Limits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	archive := gzipBombTarGz(t, 40, 64<<10) // 40 files, 2.5 MiB unpacked
	var fetched []string
	download := func(_ context.Context, rawURL, authorization string) ([]byte, error) {
		fetched = append(fetched, rawURL)
		assert.Empty(t, authorization)
		return archive, nil
	}
	const uri = "https://artifacts.example.com/t.tar.gz"

	cases := map[string]FetchOptions{
		"bytes":    {Download: download, MaxBytes: 1 << 20},
		"files":    {Download: download, MaxFiles: 10},
		"unpacked": {Download: download, MaxUnpackedBytes: 1 << 20},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := FetchRemoteTemplateWithOptions(context.Background(), uri, opts)
			require.ErrorIs(t, err, ErrRemoteFetchLimit)
			cacheDir, cerr := remoteCacheDir()
			require.NoError(t, cerr)
			_, statErr := os.Stat(filepath.Join(cacheDir, generateCacheKey(uri)))
			assert.True(t, os.IsNotExist(statErr), "a fetch over a limit leaves no cache directory")
		})
	}

	path, err := FetchRemoteTemplateWithOptions(context.Background(), uri,
		FetchOptions{Download: download, MaxBytes: 8 << 20, MaxFiles: 100, MaxUnpackedBytes: 8 << 20})
	require.NoError(t, err)
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	assert.Len(t, entries, 40)
	assert.Equal(t, []string{uri, uri, uri, uri}, fetched)
}

// TestFetchRemoteTemplateWithOptions_RcloneLimits: an rclone source over
// the file or byte limit is refused before anything is copied.
func TestFetchRemoteTemplateWithOptions_RcloneLimits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	for i := 0; i < 5; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d", i)), []byte(strings.Repeat("x", 1000)), 0o644))
	}
	uri := ":local:" + src

	_, err := FetchRemoteTemplateWithOptions(context.Background(), uri, FetchOptions{MaxFiles: 3})
	require.ErrorIs(t, err, ErrRemoteFetchLimit)
	_, err = FetchRemoteTemplateWithOptions(context.Background(), uri, FetchOptions{MaxBytes: 2000})
	require.ErrorIs(t, err, ErrRemoteFetchLimit)

	path, err := FetchRemoteTemplateWithOptions(context.Background(), uri, FetchOptions{MaxFiles: 10, MaxBytes: 10000})
	require.NoError(t, err)
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	assert.Len(t, entries, 5)
}

// TestNormalizeTemplateSourceURL_KeepsGSScheme: a gs:// source is not
// turned into an https:// URL.
func TestNormalizeTemplateSourceURL_KeepsGSScheme(t *testing.T) {
	assert.Equal(t, "gs://team-bucket/templates", NormalizeTemplateSourceURL("gs://team-bucket/templates"))
}

// TestAllowedImportSources_V1YAMLRoundTrip: the settings-file key reaches
// GlobalConfig and back.
func TestAllowedImportSources_V1YAMLRoundTrip(t *testing.T) {
	var v1 V1ServerConfig
	require.NoError(t, yaml.Unmarshal([]byte("hub:\n  allowed_import_sources:\n    - gs://team-bucket/templates\n"), &v1))
	require.NotNil(t, v1.Hub)
	gc := ConvertV1ServerToGlobalConfig(&v1)
	assert.Equal(t, []string{"gs://team-bucket/templates"}, gc.Hub.AllowedImportSources)
	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Hub)
	assert.Equal(t, []string{"gs://team-bucket/templates"}, back.Hub.AllowedImportSources)
}
