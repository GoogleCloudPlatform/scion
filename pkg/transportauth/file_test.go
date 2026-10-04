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

package transportauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateTransportTokenFile points HOME at a temp dir and clears
// SCION_TRANSPORT_TOKEN_FILE so FromEnv never consults a real
// ~/.scion/transport-token. Returns the default file path under that HOME.
func isolateTransportTokenFile(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvTransportTokenFile, "")
	return filepath.Join(home, ".scion", TransportTokenFileName)
}

func writeTokenFile(t *testing.T, path, token string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(token), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func TestFileSource_ExpiredEnvValidFileUsesFile(t *testing.T) {
	path := isolateTransportTokenFile(t)
	expired := makeTestJWT(time.Now().Add(-2 * time.Hour))
	valid := makeTestJWT(time.Now().Add(50 * time.Minute))
	writeTokenFile(t, path, valid)
	t.Setenv(EnvTransportToken, expired)

	src, err := FromEnv()
	require.NoError(t, err)
	require.NotNil(t, src)

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, valid, got, "valid file value must win over expired env value")

	st := src.(*FileSource).Status()
	assert.Equal(t, SourceLabelFile, st.InUse)
	assert.True(t, st.EnvPresent)
	assert.True(t, st.FilePresent)
	assert.True(t, st.EnvExpiry.Before(time.Now()))
	assert.True(t, st.FileExpiry.After(time.Now()))
}

func TestFileSource_EnvOnlyBootstraps(t *testing.T) {
	isolateTransportTokenFile(t)
	tok := makeTestJWT(time.Now().Add(time.Hour))
	t.Setenv(EnvTransportToken, tok)

	src, err := FromEnv()
	require.NoError(t, err)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, tok, got)
	assert.Equal(t, SourceLabelEnv, src.(*FileSource).Status().InUse)
}

func TestFileSource_ExplicitPathWithoutEnv(t *testing.T) {
	isolateTransportTokenFile(t)
	t.Setenv(EnvTransportToken, "")
	path := filepath.Join(t.TempDir(), "tt")
	tok := makeTestJWT(time.Now().Add(time.Hour))
	writeTokenFile(t, path, tok)
	t.Setenv(EnvTransportTokenFile, path)

	src, err := FromEnv()
	require.NoError(t, err)
	require.NotNil(t, src)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, tok, got)
}

func TestFileSource_PicksUpRewrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	first := makeTestJWT(time.Now().Add(10 * time.Minute))
	writeTokenFile(t, path, first)

	src := NewFileSource(path, nil)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, first, got)

	fi1, err := os.Stat(path)
	require.NoError(t, err)

	second := makeTestJWT(time.Now().Add(60 * time.Minute))
	require.Equal(t, len(first), len(second), "test needs same-length values")
	writeTokenFile(t, path, second)
	// Same size and same mtime (as on a filesystem with coarse
	// timestamps): the rename's new inode alone must trigger a re-read.
	require.NoError(t, os.Chtimes(path, fi1.ModTime(), fi1.ModTime()))

	got, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, second, got, "rewritten file must be re-read")
	assert.WithinDuration(t, time.Now().Add(60*time.Minute), src.Expiry(), 2*time.Second)
}

func TestFileSource_MissingFileAndNoEnv(t *testing.T) {
	src := NewFileSource(filepath.Join(t.TempDir(), "missing"), nil)
	_, err := src.Token()
	assert.Error(t, err)
	st := src.Status()
	assert.Equal(t, "", st.InUse)
	assert.False(t, st.FilePresent)
	assert.NoError(t, st.FileError, "a missing file is not a read error")
}

func TestFileSource_SetTokenUsedUntilFileCatchesUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	src := NewFileSource(path, nil)
	src.SetBootstrap(makeTestJWT(time.Now().Add(-time.Minute)))
	src.SetToken("refreshed", time.Now().Add(time.Hour))

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, "refreshed", got)
	assert.Equal(t, SourceLabelRefreshed, src.Status().InUse)
}

func TestFileSource_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte(makeTestJWT(time.Now().Add(time.Hour))), 0o600))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))

	src := NewFileSource(link, nil)
	_, err := src.Token()
	assert.Error(t, err)
	assert.Error(t, src.Status().FileError)
}

// TestFromEnv_HubclientStyleUsesFile mirrors how hubclient.New wires the
// source: a request built after the file was refreshed carries the file
// value, in the header selected by SCION_TRANSPORT_MODE.
func TestFromEnv_HubclientStyleUsesFile(t *testing.T) {
	path := isolateTransportTokenFile(t)
	t.Setenv(EnvTransportToken, makeTestJWT(time.Now().Add(-time.Hour)))
	t.Setenv(EnvTransportMode, "iap")
	fresh := makeTestJWT(time.Now().Add(time.Hour))
	writeTokenFile(t, path, fresh)

	var gotProxy string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProxy = r.Header.Get("Proxy-Authorization")
	}))
	defer srv.Close()

	src, err := FromEnv()
	require.NoError(t, err)
	client := &http.Client{Transport: Wrap(nil, src, ModeFromEnv())}
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, "Bearer "+fresh, gotProxy)
}

// N1: a file value that is not a JWT has no known expiry and must not beat
// a valid env value.
func TestFileSource_MalformedFileLosesToValidEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, "not-a-jwt")
	env := makeTestJWT(time.Now().Add(30 * time.Minute))

	src := NewFileSource(path, nil)
	src.SetBootstrap(env)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, env, got)
	st := src.Status()
	assert.Equal(t, SourceLabelEnv, st.InUse)
	assert.True(t, st.FilePresent)
	assert.True(t, st.FileExpiry.IsZero())
}

// A malformed file is still used when it is the only candidate.
func TestFileSource_MalformedFileOnlyCandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, "opaque-value")
	src := NewFileSource(path, nil)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, "opaque-value", got)
	assert.True(t, src.Expiry().IsZero())
}

func TestFileSource_OversizedFileIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, strings.Repeat("a", transportTokenFileMaxBytes+10))
	env := makeTestJWT(time.Now().Add(30 * time.Minute))

	src := NewFileSource(path, nil)
	src.SetBootstrap(env)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, env, got)
	st := src.Status()
	assert.False(t, st.FilePresent)
	assert.Error(t, st.FileError)
}

func TestFileSource_EnvExpiringLaterWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(10*time.Minute)))
	env := makeTestJWT(time.Now().Add(50 * time.Minute))

	src := NewFileSource(path, nil)
	src.SetBootstrap(env)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, env, got)
	assert.Equal(t, SourceLabelEnv, src.Status().InUse)
}

func TestFileSource_FileDeletedFallsBackToEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	fileTok := makeTestJWT(time.Now().Add(50 * time.Minute))
	writeTokenFile(t, path, fileTok)
	env := makeTestJWT(time.Now().Add(20 * time.Minute))

	src := NewFileSource(path, nil)
	src.SetBootstrap(env)
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, fileTok, got)

	require.NoError(t, os.Remove(path))
	got, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, env, got)
	assert.False(t, src.Status().FilePresent)
}

// One writer replacing the file (atomic renames) while readers call Token
// and Status: readers never see an empty or partial value, and the final
// value is the last one written. Run with -race.
func TestFileSource_ConcurrentRefreshAndRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tt")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(time.Minute)))

	src := NewFileSource(path, nil)
	const writes = 50
	values := make([]string, writes)
	for i := range values {
		values[i] = makeTestJWT(time.Now().Add(time.Duration(i+2) * time.Minute))
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				tok, err := src.Token()
				if err != nil || tok == "" {
					errs <- fmt.Errorf("Token() = %q, %v", tok, err)
					return
				}
				if _, perr := ParseTokenExpiry(tok); perr != nil {
					errs <- fmt.Errorf("partial or malformed value read: %v", perr)
					return
				}
				_ = src.Status()
			}
		}()
	}
	for i, v := range values {
		tmp := filepath.Join(dir, fmt.Sprintf("tt.tmp%d", i))
		require.NoError(t, os.WriteFile(tmp, []byte(v), 0o600))
		require.NoError(t, os.Rename(tmp, path))
	}
	close(done)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, values[writes-1], got)
}

// TestReadTransportTokenFile_RefusesNonRegular verifies the default reader
// refuses a symlink at the leaf (at open time, not via a separate check), a
// FIFO (without blocking), and a directory, and reads a regular file.
func TestReadTransportTokenFile_RefusesNonRegular(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "regular")
	require.NoError(t, os.WriteFile(regular, []byte("header.payload.sig\n"), 0600))
	got, err := ReadTransportTokenFile(regular)
	require.NoError(t, err)
	assert.Equal(t, "header.payload.sig\n", got)

	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(regular, link))
	_, err = ReadTransportTokenFile(link)
	assert.Error(t, err, "symlink must be refused")

	fifo := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))
	done := make(chan error, 1)
	go func() {
		_, err := ReadTransportTokenFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		assert.Error(t, err, "FIFO must be refused")
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}

	sub := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(sub, 0700))
	_, err = ReadTransportTokenFile(sub)
	assert.Error(t, err, "directory must be refused")
}

// isolateLateFileSource models an agent that started without an injected
// transport token: no SCION_TRANSPORT_TOKEN(_FILE), no metadata source.
func isolateLateFileSource(t *testing.T, mode string) string {
	t.Helper()
	path := isolateTransportTokenFile(t)
	t.Setenv(EnvTransportToken, "")
	t.Setenv(EnvTransportAudience, "")
	t.Setenv(EnvHubOIDCAudience, "")
	t.Setenv(EnvMetadataMode, "")
	t.Setenv(EnvTransportMode, mode)
	orig := IsOnGCEFunc
	IsOnGCEFunc = func() bool { return false }
	t.Cleanup(func() { IsOnGCEFunc = orig })
	return path
}

// TestFromEnv_ProxyModeLateFile: in a proxy mode without an injected
// token, FromEnv returns nothing until the default file appears, then a
// FileSource for it (no bootstrap), sent in the mode's header.
func TestFromEnv_ProxyModeLateFile(t *testing.T) {
	for mode, header := range map[string]string{
		"iap":              "Proxy-Authorization",
		"cloudrun_invoker": "X-Serverless-Authorization",
	} {
		t.Run(mode, func(t *testing.T) {
			path := isolateLateFileSource(t, mode)

			src, err := FromEnv()
			require.NoError(t, err)
			assert.Nil(t, src, "no file yet: no source")

			fresh := makeTestJWT(time.Now().Add(time.Hour))
			writeTokenFile(t, path, fresh)
			src, err = FromEnv()
			require.NoError(t, err)
			fs, ok := src.(*FileSource)
			require.True(t, ok, "got %T", src)
			assert.Equal(t, path, fs.Path())
			st := fs.Status()
			assert.Equal(t, SourceLabelFile, st.InUse)
			assert.False(t, st.EnvPresent)

			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get(header)
			}))
			defer srv.Close()
			resp, err := (&http.Client{Transport: Wrap(nil, src, ModeFromEnv())}).Get(srv.URL)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, "Bearer "+fresh, got)
		})
	}
}

// TestFromEnv_LateFileNeedsProxyMode: without a proxy mode the default
// file alone selects nothing.
func TestFromEnv_LateFileNeedsProxyMode(t *testing.T) {
	for _, mode := range []string{"", "unknown"} {
		t.Run("mode="+mode, func(t *testing.T) {
			path := isolateLateFileSource(t, mode)
			writeTokenFile(t, path, makeTestJWT(time.Now().Add(time.Hour)))
			src, err := FromEnv()
			require.NoError(t, err)
			assert.Nil(t, src)
		})
	}
}

// TestFromEnv_LateFileDoesNotDisplaceMetadata: a metadata source that
// would be selected is kept even in a proxy mode with the file present.
func TestFromEnv_LateFileDoesNotDisplaceMetadata(t *testing.T) {
	path := isolateLateFileSource(t, "iap")
	IsOnGCEFunc = func() bool { return true }
	t.Setenv(EnvTransportAudience, "test-audience")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(time.Hour)))
	src, err := FromEnv()
	require.NoError(t, err)
	_, ok := src.(*MetadataSource)
	assert.True(t, ok, "got %T", src)
}

func TestIsProxyMode(t *testing.T) {
	assert.True(t, IsProxyMode("iap"))
	assert.True(t, IsProxyMode("cloudrun_invoker"))
	assert.False(t, IsProxyMode(""))
	assert.False(t, IsProxyMode("IAP"))
	assert.False(t, IsProxyMode("other"))
}
