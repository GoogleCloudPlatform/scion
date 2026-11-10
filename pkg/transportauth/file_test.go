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
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
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

func TestFileSource_RetriesReadAfterHardlinkRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	first := makeTestJWT(time.Now().Add(10 * time.Minute))
	writeTokenFile(t, path, first)
	src := NewFileSource(path, nil)
	got, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, first, got)

	// A replacement with an extra hard link must be rejected, and the
	// previously cached token must not survive the failed read.
	second := makeTestJWT(time.Now().Add(time.Hour))
	writeTokenFile(t, path, second)
	link := path + ".link"
	require.NoError(t, os.Link(path, link))
	before, err := os.Stat(path)
	require.NoError(t, err)

	got, err = src.Token()
	require.Error(t, err)
	assert.Empty(t, got)
	st := src.Status()
	assert.False(t, st.FilePresent)
	assert.ErrorIs(t, st.FileError, dirfd.ErrNotSingleLinkRegular)

	// Removing only the extra link makes the file safe to read without
	// changing any of the fields used to skip an unchanged file.
	require.NoError(t, os.Remove(link))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, before.ModTime(), after.ModTime())
	require.Equal(t, before.Size(), after.Size())

	got, err = src.Token()
	require.NoError(t, err, "a failed read must be retried even without a file replacement")
	assert.Equal(t, second, got)
	st = src.Status()
	assert.True(t, st.FilePresent)
	assert.Equal(t, SourceLabelFile, st.InUse)
	assert.NoError(t, st.FileError)
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
	// Setup: one read before any replacement, so every reader below has an
	// earlier value from this source.
	_, err := src.Token()
	require.NoError(t, err)
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

	// The readers' last reads can be younger than racyRecheck and would be
	// reused; move the source's clock past it so this call reads the file.
	src.mu.Lock()
	src.now = func() time.Time { return time.Now().Add(racyRecheck) }
	src.mu.Unlock()
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, values[writes-1], got)
}

// A read that opens the file just before a refresh renames a new file over
// it fails, because the old file has no link left. The source must read the
// new file instead of dropping the value. The injected reader forces that
// interleaving deterministically: it replaces the file, then fails the way
// the default reader does on the unlinked old file. ptone/scion#5038.
func TestFileSource_ReadRacingReplaceRereadsNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	first := makeTestJWT(time.Now().Add(10 * time.Minute))
	second := makeTestJWT(time.Now().Add(20 * time.Minute))
	third := makeTestJWT(time.Now().Add(30 * time.Minute))
	writeTokenFile(t, path, first)

	armed := false
	reads := 0
	src := NewFileSource(path, func(p string) (string, error) {
		reads++
		if armed {
			armed = false
			writeTokenFile(t, p, third)
			return "", fmt.Errorf("credential file %s: %w", p, dirfd.ErrNotSingleLinkRegular)
		}
		return ReadTransportTokenFile(p)
	})
	got, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, first, got)

	writeTokenFile(t, path, second)
	armed = true
	reads = 0
	got, err = src.Token()
	require.NoError(t, err, "a read racing a replacement must not drop the credential")
	assert.Equal(t, third, got)
	assert.Equal(t, 2, reads)
	st := src.Status()
	assert.NoError(t, st.FileError)
	assert.Equal(t, SourceLabelFile, st.InUse)
}

// A read that fails on a file that was not replaced is a genuine refusal:
// it is not retried within the call, and the file value is cleared.
func TestFileSource_ReadFailureWithoutReplaceNotRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(10*time.Minute)))
	reads := 0
	src := NewFileSource(path, func(p string) (string, error) {
		reads++
		return "", fmt.Errorf("credential file %s: %w", p, dirfd.ErrNotSingleLinkRegular)
	})
	got, err := src.Token()
	assert.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 1, reads)
	st := src.Status()
	assert.False(t, st.FilePresent)
	assert.ErrorIs(t, st.FileError, dirfd.ErrNotSingleLinkRegular)
}

// A read that fails and leaves the path gone is not retried: there is no
// new file to read, and the read error is the one recorded.
func TestFileSource_ReadFailureThenPathGoneNotRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(10*time.Minute)))
	reads := 0
	src := NewFileSource(path, func(p string) (string, error) {
		reads++
		require.NoError(t, os.Remove(p))
		return "", fmt.Errorf("credential file %s: %w", p, dirfd.ErrNotSingleLinkRegular)
	})
	got, err := src.Token()
	assert.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 1, reads)
	// Status re-checks the path and reports a missing file without an
	// error, so inspect the error recorded by the Token call directly.
	assert.ErrorIs(t, src.fileErr, dirfd.ErrNotSingleLinkRegular)
	assert.Empty(t, src.fileToken)
}

// A file replaced during every read is read at most three times in
// one call; then the failure is reported and no value is kept.
func TestFileSource_ReadRetryIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(10*time.Minute)))
	reads := 0
	src := NewFileSource(path, func(p string) (string, error) {
		reads++
		writeTokenFile(t, p, makeTestJWT(time.Now().Add(time.Duration(reads+10)*time.Minute)))
		return "", fmt.Errorf("credential file %s: %w", p, dirfd.ErrNotSingleLinkRegular)
	})
	got, err := src.Token()
	assert.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 3, reads)
	assert.ErrorIs(t, src.Status().FileError, dirfd.ErrNotSingleLinkRegular)
}

// unlinkedReadErr is the error the default reader returns for a file that
// had no link left when it was checked: both dirfd sentinels, wrapped.
func unlinkedReadErr(p string) error {
	return fmt.Errorf("transport token file %s: dirfd: tt: %w: %w", p, dirfd.ErrNotSingleLinkRegular, dirfd.ErrUnlinked)
}

// A read that fails because the opened file had no link left is retried
// even when the path names a file with the same identity before and after
// the read. That happens when two renames land during the read and the
// filesystem gives the newest file the inode number of the one it freed.
// The injected reader reports the unlinked file without changing the path,
// which is what the source sees in that case. ptone/scion#5038.
func TestFileSource_UnlinkedReadRetriedWhenIdentityMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	want := makeTestJWT(time.Now().Add(10 * time.Minute))
	writeTokenFile(t, path, want)

	reads := 0
	src := NewFileSource(path, func(p string) (string, error) {
		reads++
		if reads == 1 {
			return "", unlinkedReadErr(p)
		}
		return ReadTransportTokenFile(p)
	})
	got, err := src.Token()
	require.NoError(t, err, "a read of an unlinked file must be retried")
	assert.Equal(t, want, got)
	assert.Equal(t, 2, reads)
	assert.NoError(t, src.fileErr)
}

// A file that is reported unlinked on every read is read at most three
// times in one call. With no earlier successful read there is nothing to
// keep, so the source refuses: an empty token and an error, with the read
// error recorded.
func TestFileSource_UnlinkedReadRetryIsBoundedAndRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(10*time.Minute)))

	reads := 0
	src := NewFileSource(path, func(p string) (string, error) {
		reads++
		return "", unlinkedReadErr(p)
	})
	got, err := src.Token()
	assert.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 3, reads)
	assert.Empty(t, src.fileToken)
	assert.False(t, src.fileRetained)
	assert.ErrorIs(t, src.fileErr, dirfd.ErrUnlinked)
	assert.ErrorIs(t, src.fileErr, dirfd.ErrNotSingleLinkRegular)

	st := src.Status()
	assert.False(t, st.FilePresent)
	assert.False(t, st.FileRetained)
	assert.Equal(t, "", st.InUse)
	assert.ErrorIs(t, st.FileError, dirfd.ErrNotSingleLinkRegular)
}

// readScript is a FileReadFunc whose reads follow modes, one per read:
// "unlinked" fails the way the default reader does on a file with no link
// left, "refuse" fails with a hardlink refusal, "refuse-replace" replaces
// the file and then fails with a hardlink refusal, and "file" (also used
// once modes runs out) reads the file with the default reader.
type readScript struct {
	t     *testing.T
	modes []string
	reads int
}

func (r *readScript) read(p string) (string, error) {
	r.reads++
	mode := "file"
	if len(r.modes) > 0 {
		mode, r.modes = r.modes[0], r.modes[1:]
	}
	switch mode {
	case "unlinked":
		return "", unlinkedReadErr(p)
	case "refuse":
		return "", fmt.Errorf("transport token file %s: dirfd: tt: %w", p, dirfd.ErrNotSingleLinkRegular)
	case "refuse-replace":
		writeTokenFile(r.t, p, makeTestJWT(time.Now().Add(time.Duration(r.reads+20)*time.Minute)))
		return "", fmt.Errorf("transport token file %s: dirfd: tt: %w", p, dirfd.ErrNotSingleLinkRegular)
	default:
		return ReadTransportTokenFile(p)
	}
}

// retainFixture is a FileSource on a fake clock that has read tok once, at
// clock0. The file's modification time is clock0, so every later call that
// is at least racyRecheck after the last read reads the file again.
type retainFixture struct {
	path  string
	tok   string
	src   *FileSource
	r     *readScript
	clock time.Time
	warns []string
}

func newRetainFixture(t *testing.T, tok string, clock0 time.Time) *retainFixture {
	t.Helper()
	f := &retainFixture{path: filepath.Join(t.TempDir(), "tt"), tok: tok, clock: clock0}
	writeTokenFile(t, f.path, tok)
	setModTime(t, f.path, clock0)
	f.r = &readScript{t: t}
	f.src = NewFileSource(f.path, f.r.read)
	f.src.now = func() time.Time { return f.clock }
	f.src.WarnLog = func(format string, args ...interface{}) {
		f.warns = append(f.warns, fmt.Sprintf(format, args...))
	}
	got, err := f.src.Token()
	require.NoError(t, err)
	require.Equal(t, tok, got)
	return f
}

// next advances the clock by racyRecheck, so the next call reads the file,
// and scripts the reads of that call.
func (f *retainFixture) next(modes ...string) {
	f.clock = f.clock.Add(racyRecheck)
	f.script(modes...)
}

// script sets the reads of the next call without moving the clock.
func (f *retainFixture) script(modes ...string) {
	f.r.modes = append([]string(nil), modes...)
	f.r.reads = 0
}

func (f *retainFixture) retainWarnings() int {
	n := 0
	for _, w := range f.warns {
		if strings.Contains(w, "is being replaced") {
			n++
		}
	}
	return n
}

// makeTestJWTNoExp returns a JWT whose claims have no exp.
func makeTestJWTNoExp() string {
	enc := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	return enc(`{"alg":"none","typ":"JWT"}`) + "." + enc(`{"iss":"test"}`) + "." + enc("fakesig")
}

// When every read in a call opens a file that had no link left, the value
// from the earlier successful read is kept: Token returns it with no error,
// and Status reports FileRetained with the read error.
func TestFileSource_RetainsLastGoodValueWhileReplaced(t *testing.T) {
	f := newRetainFixture(t, makeTestJWT(time.Now().Add(10*time.Minute)), time.Now())
	f.next("unlinked", "unlinked", "unlinked", "unlinked", "unlinked", "unlinked")

	got, err := f.src.Token()
	require.NoError(t, err, "the earlier value must be kept while the file is being replaced")
	assert.Equal(t, f.tok, got)
	assert.Equal(t, 3, f.r.reads)

	st := f.src.Status()
	assert.Equal(t, 6, f.r.reads)
	assert.True(t, st.FileRetained)
	assert.True(t, st.FilePresent)
	assert.Equal(t, SourceLabelFile, st.InUse)
	assert.ErrorIs(t, st.FileError, dirfd.ErrUnlinked)
	assert.Equal(t, 1, f.retainWarnings())
}

// The warning is logged once per retained episode: again only after a
// successful read has ended the previous one.
func TestFileSource_RetainWarnsOncePerEpisode(t *testing.T) {
	f := newRetainFixture(t, makeTestJWT(time.Now().Add(10*time.Minute)), time.Now())
	f.next("unlinked", "unlinked", "unlinked")
	_, err := f.src.Token()
	require.NoError(t, err)
	f.next("unlinked", "unlinked", "unlinked")
	_, err = f.src.Token()
	require.NoError(t, err)
	assert.Equal(t, 1, f.retainWarnings(), "one warning per episode")
	require.True(t, f.src.fileRetained)

	f.next()
	_, err = f.src.Token()
	require.NoError(t, err)
	assert.False(t, f.src.fileRetained, "a successful read ends the episode")

	f.next("unlinked", "unlinked", "unlinked")
	_, err = f.src.Token()
	require.NoError(t, err)
	assert.Equal(t, 2, f.retainWarnings(), "a new episode warns again")
	assert.Contains(t, f.warns[len(f.warns)-1], f.path)
}

// A kept value is never reused without reading: the next call reads the
// file even though its metadata is unchanged and the clock has not moved.
func TestFileSource_RetainedValueIsNotReused(t *testing.T) {
	f := newRetainFixture(t, makeTestJWT(time.Now().Add(10*time.Minute)), time.Now())
	f.next("unlinked", "unlinked", "unlinked")
	_, err := f.src.Token()
	require.NoError(t, err)
	require.True(t, f.src.fileRetained)

	f.script()
	got, err := f.src.Token()
	require.NoError(t, err)
	assert.Equal(t, 1, f.r.reads, "the call after a kept value must read the unchanged file")
	assert.Equal(t, f.tok, got)
	assert.False(t, f.src.fileRetained)
	assert.NoError(t, f.src.fileErr)

	newer := makeTestJWT(time.Now().Add(20 * time.Minute))
	writeTokenFile(t, f.path, newer)
	f.script()
	got, err = f.src.Token()
	require.NoError(t, err)
	assert.Equal(t, newer, got)
}

// A refusal in the same call clears the value even when the other reads
// failed only because the file was being replaced.
func TestFileSource_RetainClearedByRefusalInSameCall(t *testing.T) {
	f := newRetainFixture(t, makeTestJWT(time.Now().Add(10*time.Minute)), time.Now())
	f.next("unlinked", "unlinked", "refuse")

	got, err := f.src.Token()
	assert.Error(t, err)
	assert.Empty(t, got, "a refusal in the call must clear the earlier value")
	assert.Equal(t, 3, f.r.reads)
	assert.False(t, f.src.fileRetained)
	assert.ErrorIs(t, f.src.fileErr, dirfd.ErrNotSingleLinkRegular)
	assert.NotErrorIs(t, f.src.fileErr, dirfd.ErrUnlinked)
}

// A refusal before the reads of a file with no link left also clears the
// value: every failed read in the call counts, not only the last one.
func TestFileSource_RetainClearedByEarlierRefusalInSameCall(t *testing.T) {
	f := newRetainFixture(t, makeTestJWT(time.Now().Add(10*time.Minute)), time.Now())
	f.next("refuse-replace", "unlinked", "unlinked")

	got, err := f.src.Token()
	assert.Error(t, err)
	assert.Empty(t, got, "an earlier refusal in the call must clear the earlier value")
	assert.Equal(t, 3, f.r.reads)
	assert.False(t, f.src.fileRetained)
}

// Reads that are retried because the file was replaced, but fail with a
// refusal, do not keep the earlier value.
func TestFileSource_IdentityChangeAloneDoesNotRetain(t *testing.T) {
	f := newRetainFixture(t, makeTestJWT(time.Now().Add(10*time.Minute)), time.Now())
	f.next("refuse-replace", "refuse-replace", "refuse-replace")

	got, err := f.src.Token()
	assert.Error(t, err)
	assert.Empty(t, got, "only reads of a file with no link left may keep the earlier value")
	assert.Equal(t, 3, f.r.reads)
	assert.False(t, f.src.fileRetained)
}

// The earlier value is kept for at most keepMax after the read that
// produced it.
func TestFileSource_RetainBoundedByKeepMax(t *testing.T) {
	clock0 := time.Now()
	f := newRetainFixture(t, makeTestJWT(time.Now().Add(10*time.Minute)), clock0)

	f.clock = clock0.Add(keepMax)
	f.script("unlinked", "unlinked", "unlinked")
	got, err := f.src.Token()
	require.NoError(t, err, "kept at exactly keepMax after the read")
	assert.Equal(t, f.tok, got)

	f.clock = clock0.Add(keepMax + time.Nanosecond)
	f.script("unlinked", "unlinked", "unlinked")
	got, err = f.src.Token()
	assert.Error(t, err)
	assert.Empty(t, got, "the earlier value must not be kept beyond keepMax")
	assert.False(t, f.src.fileRetained)
}

// The earlier value is not kept once its expiry has passed.
func TestFileSource_RetainNotPastExpiry(t *testing.T) {
	exp := time.Unix(time.Now().Add(time.Hour).Unix(), 0)
	f := newRetainFixture(t, makeTestJWT(exp), exp.Add(-time.Second))

	f.clock = exp.Add(-time.Nanosecond)
	f.script("unlinked", "unlinked", "unlinked")
	got, err := f.src.Token()
	require.NoError(t, err, "kept just before its expiry")
	assert.Equal(t, f.tok, got)

	f.clock = exp
	f.script("unlinked", "unlinked", "unlinked")
	got, err = f.src.Token()
	assert.Error(t, err)
	assert.Empty(t, got, "an expired value must not be kept")
	assert.False(t, f.src.fileRetained)
}

// A value whose expiry cannot be parsed is served by a fresh read but is
// never kept while the file is being replaced.
func TestFileSource_RetainRefusedForUnparseableExpiry(t *testing.T) {
	f := newRetainFixture(t, "header.payload.sig", time.Now())
	f.next("unlinked", "unlinked", "unlinked")

	got, err := f.src.Token()
	assert.Error(t, err)
	assert.Empty(t, got, "a value without a parsed expiry must not be kept")
	assert.False(t, f.src.fileRetained)
}

// A JWT with no expiry claim is served by a fresh read but is never kept.
func TestFileSource_RetainRefusedWithoutExpiryClaim(t *testing.T) {
	f := newRetainFixture(t, makeTestJWTNoExp(), time.Now())
	f.next("unlinked", "unlinked", "unlinked")

	got, err := f.src.Token()
	assert.Error(t, err)
	assert.Empty(t, got, "a value without a parsed expiry must not be kept")
	assert.False(t, f.src.fileRetained)
}

// Removing the file clears a kept value.
func TestFileSource_RetainClearedWhenFileRemoved(t *testing.T) {
	f := newRetainFixture(t, makeTestJWT(time.Now().Add(10*time.Minute)), time.Now())
	f.next("unlinked", "unlinked", "unlinked")
	_, err := f.src.Token()
	require.NoError(t, err)
	require.True(t, f.src.fileRetained)

	require.NoError(t, os.Remove(f.path))
	f.next()
	got, err := f.src.Token()
	assert.Error(t, err)
	assert.Empty(t, got)
	assert.False(t, f.src.fileRetained)
	assert.False(t, f.src.Status().FilePresent)
}

// canRetainLocked keeps a value only when there is one from a successful
// read at most keepMax old, with a parsed expiry that has not passed. Each
// case changes one input from a state that is kept.
func TestFileSource_CanRetainLocked(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name   string
		change func(s *FileSource, now *time.Time)
		want   bool
	}{
		{"value read now with a parsed future expiry", func(*FileSource, *time.Time) {}, true},
		{"expiry not parsed", func(s *FileSource, _ *time.Time) { s.fileExpiryParsed = false }, false},
		{"no value", func(s *FileSource, _ *time.Time) { s.fileToken = "" }, false},
		{"no successful read", func(s *FileSource, _ *time.Time) { s.lastGoodAt = time.Time{} }, false},
		{"read exactly keepMax ago", func(_ *FileSource, now *time.Time) { *now = t0.Add(keepMax) }, true},
		{"read just over keepMax ago", func(_ *FileSource, now *time.Time) { *now = t0.Add(keepMax + time.Nanosecond) }, false},
		{"at the expiry", func(s *FileSource, now *time.Time) {
			s.fileExpiry = t0.Add(time.Second)
			*now = t0.Add(time.Second)
		}, false},
		{"just before the expiry", func(s *FileSource, now *time.Time) {
			s.fileExpiry = t0.Add(time.Second)
			*now = t0.Add(time.Second - time.Nanosecond)
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &FileSource{
				fileToken:        "tok",
				lastGoodAt:       t0,
				fileExpiry:       t0.Add(10 * time.Minute),
				fileExpiryParsed: true,
			}
			now := t0
			tt.change(s, &now)
			assert.Equal(t, tt.want, s.canRetainLocked(now))
		})
	}
}

// countingReader returns a FileReadFunc that uses the default reader and
// counts its calls.
func countingReader(reads *int) FileReadFunc {
	return func(p string) (string, error) {
		*reads++
		return ReadTransportTokenFile(p)
	}
}

// setModTime sets path's modification time to mtime and returns it as the
// filesystem stores it.
func setModTime(t *testing.T, path string, mtime time.Time) time.Time {
	t.Helper()
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	return fi.ModTime()
}

// A file modified within racyModTimeWindow of the last read is read again
// once that read is racyRecheck old: a write in the same timestamp tick
// could have left all the metadata unchanged. A racy read younger than
// racyRecheck is reused, and once the modification time is safely before
// the start of a read, that read is reused.
func TestFileSource_RacyModTimeIsReread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tt")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(10*time.Minute)))
	mtime := setModTime(t, path, time.Now().Add(-time.Hour).Truncate(time.Second))

	clock := mtime.Add(racyModTimeWindow / 4)
	reads := 0
	src := NewFileSource(path, countingReader(&reads))
	src.now = func() time.Time { return clock }

	_, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, 1, reads)

	clock = clock.Add(racyRecheck / 2)
	_, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, 1, reads, "a racy read less than racyRecheck old is reused")

	clock = clock.Add(racyRecheck)
	_, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, 2, reads, "a file modified close to the read must be read again")

	// The last read started close to the modification time, so the next
	// call still reads; from then on the read is reused.
	clock = mtime.Add(racyModTimeWindow + 5*time.Second)
	_, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, 3, reads)
	_, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, 3, reads, "a read that started well after the modification time is reused")
}

// cachedReadValidLocked reuses an earlier read only when every piece of
// metadata matches and the modification time is safely before the start of
// that read, or that read is less than racyRecheck old. Each case changes
// one input from a state that is reused. The
// identities are real Lstat results (os.SameFile only compares results from
// os.Stat or os.Lstat); the other inputs are plain values, so the table
// needs no clock or kernel timing.
func TestFileSource_CachedReadValidLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tt")
	other := filepath.Join(dir, "other")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(10*time.Minute)))
	writeTokenFile(t, other, makeTestJWT(time.Now().Add(10*time.Minute)))
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	otherFi, err := os.Lstat(other)
	require.NoError(t, err)
	require.False(t, os.SameFile(fi, otherFi))

	mtime := fi.ModTime()
	ctime := time.Unix(1_700_000_000, 123)
	const nlink = uint64(1)

	type input struct {
		src       *FileSource
		ctime     time.Time
		nlink     uint64
		haveExtra bool
		age       time.Duration // how long after fileReadAt the call is made
	}
	// reusable returns the state in which the earlier read is reused.
	reusable := func() input {
		return input{
			src: &FileSource{
				path:        path,
				fileInfo:    fi,
				fileModTime: mtime,
				fileSize:    fi.Size(),
				fileCtime:   ctime,
				fileNlink:   nlink,
				fileReadAt:  mtime.Add(racyModTimeWindow + time.Nanosecond),
			},
			ctime:     ctime,
			nlink:     nlink,
			haveExtra: true,
			age:       racyRecheck,
		}
	}
	tests := []struct {
		name   string
		change func(in *input)
		want   bool
	}{
		{"all equal, modified before the racy window", func(*input) {}, true},
		{"different file", func(in *input) { in.src.fileInfo = otherFi }, false},
		{"modification time differs", func(in *input) { in.src.fileModTime = mtime.Add(-time.Hour) }, false},
		{"size differs", func(in *input) { in.src.fileSize++ }, false},
		{"change time differs", func(in *input) { in.ctime = ctime.Add(time.Nanosecond) }, false},
		{"link count differs", func(in *input) { in.nlink = nlink + 1 }, false},
		{"modified inside the racy window", func(in *input) { in.src.fileReadAt = mtime.Add(racyModTimeWindow / 2) }, false},
		{"modified exactly at the window boundary", func(in *input) { in.src.fileReadAt = mtime.Add(racyModTimeWindow) }, false},
		{"modified after the read started", func(in *input) { in.src.fileReadAt = mtime.Add(-time.Second) }, false},
		{"racy read younger than racyRecheck", func(in *input) {
			in.src.fileReadAt = mtime.Add(racyModTimeWindow / 2)
			in.age = racyRecheck - time.Nanosecond
		}, true},
		{"racy read exactly racyRecheck old", func(in *input) {
			in.src.fileReadAt = mtime.Add(racyModTimeWindow / 2)
			in.age = racyRecheck
		}, false},
		{"racy read young but size differs", func(in *input) {
			in.src.fileReadAt = mtime.Add(racyModTimeWindow / 2)
			in.age = 0
			in.src.fileSize++
		}, false},
		{"read young but last read failed", func(in *input) {
			in.age = 0
			in.src.fileErr = errors.New("read failed")
		}, false},
		{"change time and link count unavailable", func(in *input) { in.haveExtra = false }, false},
		{"last read failed", func(in *input) { in.src.fileErr = errors.New("read failed") }, false},
		{"no earlier read", func(in *input) { in.src.fileInfo = nil }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := reusable()
			tt.change(&in)
			got := in.src.cachedReadValidLocked(fi, in.ctime, in.nlink, in.haveExtra, in.src.fileReadAt.Add(in.age))
			assert.Equal(t, tt.want, got)
		})
	}
}

// End to end: after a hard link is added to the file, the next call reads
// it again and the reader refuses it, so the earlier value is not served.
// Adding the link changes the change time as well as the link count, so this
// does not isolate the link-count comparison; TestFileSource_CachedReadValidLocked
// covers each comparison on its own.
func TestFileSource_LinkAddedAfterReadIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tt")
	writeTokenFile(t, path, makeTestJWT(time.Now().Add(10*time.Minute)))
	setModTime(t, path, time.Now().Add(-time.Hour).Truncate(time.Second))

	reads := 0
	src := NewFileSource(path, countingReader(&reads))
	_, err := src.Token()
	require.NoError(t, err)

	require.NoError(t, os.Link(path, filepath.Join(dir, "other")))
	got, err := src.Token()
	assert.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 2, reads)
	assert.ErrorIs(t, src.fileErr, dirfd.ErrNotSingleLinkRegular)
	assert.NotErrorIs(t, src.fileErr, dirfd.ErrUnlinked)
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
	require.NoError(t, mkfifoForTest(fifo, 0600))
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

// TestReadTransportTokenFile_UnderHome covers the default reader's rule
// for a path under the user's home directory: the home directory itself
// is trusted (it may be reached through a linked prefix), and links,
// multi-link files and oversized files below it are refused.
func TestReadTransportTokenFile_UnderHome(t *testing.T) {
	setup := func(t *testing.T) (home, scionDir string) {
		t.Helper()
		home = t.TempDir()
		t.Setenv("HOME", home)
		scionDir = filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0700))
		return home, scionDir
	}

	t.Run("reads a regular file", func(t *testing.T) {
		_, scionDir := setup(t)
		path := filepath.Join(scionDir, TransportTokenFileName)
		require.NoError(t, os.WriteFile(path, []byte("tok"), 0600))
		got, err := ReadTransportTokenFile(path)
		require.NoError(t, err)
		assert.Equal(t, "tok", got)
	})

	t.Run("reads through a home reached via a linked prefix", func(t *testing.T) {
		root := t.TempDir()
		realHome := filepath.Join(root, "var", "home", "user")
		require.NoError(t, os.MkdirAll(filepath.Join(realHome, ".scion"), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(realHome, ".scion", TransportTokenFileName), []byte("linked"), 0600))
		require.NoError(t, os.Symlink(filepath.Join(root, "var", "home"), filepath.Join(root, "home")))
		home := filepath.Join(root, "home", "user")
		t.Setenv("HOME", home)

		got, err := ReadTransportTokenFile(filepath.Join(home, ".scion", TransportTokenFileName))
		require.NoError(t, err)
		assert.Equal(t, "linked", got)
	})

	t.Run("refuses a linked directory below home", func(t *testing.T) {
		home, _ := setup(t)
		other := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(other, TransportTokenFileName), []byte("other"), 0600))
		require.NoError(t, os.Symlink(other, filepath.Join(home, "linked")))

		_, err := ReadTransportTokenFile(filepath.Join(home, "linked", TransportTokenFileName))
		assert.Error(t, err)
	})

	t.Run("refuses a hard link", func(t *testing.T) {
		_, scionDir := setup(t)
		target := filepath.Join(scionDir, "target")
		require.NoError(t, os.WriteFile(target, []byte("tok"), 0600))
		path := filepath.Join(scionDir, TransportTokenFileName)
		require.NoError(t, os.Link(target, path))

		_, err := ReadTransportTokenFile(path)
		assert.ErrorIs(t, err, dirfd.ErrNotSingleLinkRegular)
	})

	t.Run("refuses an oversized file", func(t *testing.T) {
		_, scionDir := setup(t)
		path := filepath.Join(scionDir, TransportTokenFileName)
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", transportTokenFileMaxBytes+1)), 0600))

		got, err := ReadTransportTokenFile(path)
		assert.ErrorIs(t, err, dirfd.ErrTooLarge)
		assert.Equal(t, "", got)
	})

	t.Run("outside home: parent trusted, leaf link refused", func(t *testing.T) {
		setup(t)
		outside := t.TempDir()
		regular := filepath.Join(outside, "regular")
		require.NoError(t, os.WriteFile(regular, []byte("tok"), 0600))
		got, err := ReadTransportTokenFile(regular)
		require.NoError(t, err)
		assert.Equal(t, "tok", got)

		link := filepath.Join(outside, "link")
		require.NoError(t, os.Symlink(regular, link))
		_, err = ReadTransportTokenFile(link)
		assert.Error(t, err)
	})
}

func TestSplitUnderHome(t *testing.T) {
	tests := []struct {
		name, home, path, wantBase, wantRel string
	}{
		{"under home", "/home/user", "/home/user/.scion/transport-token", "/home/user", ".scion/transport-token"},
		{"home with trailing slash", "/home/user/", "/home/user/.scion/t", "/home/user", ".scion/t"},
		{"uncleaned path", "/home/user", "/home/user/a/../.scion/./t", "/home/user", ".scion/t"},
		{"sibling sharing a prefix is not under home", "/home/user", "/home/userX/.scion/t", "/home/userX/.scion", "t"},
		{"home itself is not under home", "/home/user", "/home/user", "/home", "user"},
		{"dot-dot out of home", "/home/user", "/home/user/../other/t", "/home/other", "t"},
		{"no home", "", "/home/user/.scion/t", "/home/user/.scion", "t"},
		{"root home", "/", "/a/b", "/", "a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, rel, err := splitUnderHome(tt.home, tt.path)
			require.NoError(t, err)
			assert.Equal(t, tt.wantBase, base)
			assert.Equal(t, tt.wantRel, filepath.ToSlash(rel))
		})
	}
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

// FromEnvWithReader uses the given reader for both file-backed steps.
func TestFromEnvWithReader_BothFileSteps(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  func(t *testing.T, path string)
	}{
		{"injected file", func(t *testing.T, path string) {
			t.Setenv(EnvTransportTokenFile, path)
		}},
		{"proxy-mode late file", func(t *testing.T, path string) {
			t.Setenv(EnvTransportMode, "iap")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			for _, k := range []string{EnvTransportToken, EnvTransportTokenFile, EnvTransportMode,
				EnvTransportAudience, EnvHubOIDCAudience} {
				t.Setenv(k, "")
			}
			orig := IsOnGCEFunc
			IsOnGCEFunc = func() bool { return false }
			t.Cleanup(func() { IsOnGCEFunc = orig })

			path := DefaultTransportTokenFilePath()
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("placeholder-file-value"), 0600); err != nil {
				t.Fatal(err)
			}
			tc.env(t, path)

			var calls []string
			read := func(p string) (string, error) {
				calls = append(calls, p)
				return "", errors.New("refused by test reader")
			}
			src, err := FromEnvWithReader(read)
			if err != nil || src == nil {
				t.Fatalf("FromEnvWithReader: src=%v err=%v", src, err)
			}
			if got, _ := src.Token(); got == "placeholder-file-value" {
				t.Errorf("default reader used instead of the injected one")
			}
			if len(calls) == 0 || calls[0] != path {
				t.Errorf("injected reader not called for %s: calls=%v", path, calls)
			}
		})
	}
}
