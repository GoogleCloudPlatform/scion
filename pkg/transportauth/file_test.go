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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	second := makeTestJWT(time.Now().Add(60 * time.Minute))
	writeTokenFile(t, path, second)
	// Make sure the change is visible even on filesystems with coarse
	// timestamps.
	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

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
