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

//go:build unix

package provision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyGitWorkspace returns a workspace (with its state directory beside
// it) that has a .git/info directory.
func legacyGitWorkspace(t *testing.T) (ws, info string) {
	t.Helper()
	ws = newWorkspaceTree(t)
	info = filepath.Join(ws, ".git", "info")
	require.NoError(t, os.MkdirAll(info, 0o755))
	return ws, info
}

// An exclude file above the cap (a sparse file, so the test writes almost
// nothing) is never read: the pre-check treats it as holding no legacy line,
// the promotion leaves it as it is and still promotes the marker.
func TestPromoteLegacyMarker_OverCapExcludeNotRead(t *testing.T) {
	ws, info := legacyGitWorkspace(t)
	exclude := filepath.Join(info, "exclude")
	require.NoError(t, os.WriteFile(exclude, []byte(legacyExcludeLine+"\n"), 0o644))
	const size = 64 << 20
	require.NoError(t, os.Truncate(exclude, size))

	_, _, err := readLegacyExclude(ws)
	require.ErrorIs(t, err, errLegacyExcludeTooLarge)
	assert.False(t, hasLegacyExcludeLine(ws))
	assert.False(t, LegacyPromotionNeeded(ws), "an over-cap exclude alone is nothing to promote")

	require.NoError(t, writeSentinel(filepath.Join(ws, ProvisionSentinelFile)))
	res, err := PromoteLegacyMarker(t.Context(), ws)
	require.NoError(t, err)
	assert.True(t, res.Promoted)
	assert.False(t, res.ExcludeRemoved)
	fi, err := os.Stat(exclude)
	require.NoError(t, err)
	assert.Equal(t, int64(size), fi.Size(), "the over-cap file is left as it is")
}

// A file at the cap is read; one byte above it is refused.
func TestReadLegacyExclude_Cap(t *testing.T) {
	ws, info := legacyGitWorkspace(t)
	exclude := filepath.Join(info, "exclude")
	data := make([]byte, legacyExcludeMaxBytes)
	copy(data, legacyExcludeLine+"\n")
	for i := len(legacyExcludeLine) + 1; i < len(data); i++ {
		data[i] = '#'
	}
	require.NoError(t, os.WriteFile(exclude, data, 0o644))
	f, got, err := readLegacyExclude(ws)
	require.NoError(t, err)
	require.NotNil(t, f)
	_ = f.Close()
	assert.Len(t, got, legacyExcludeMaxBytes)
	assert.True(t, hasLegacyExcludeLine(ws))

	require.NoError(t, os.WriteFile(exclude, append(data, '#'), 0o644))
	_, _, err = readLegacyExclude(ws)
	require.ErrorIs(t, err, errLegacyExcludeTooLarge)
}

// A symlinked exclude (to another file, or to a device) is never followed:
// the target is neither read nor changed.
func TestPromoteLegacyMarker_SymlinkedExcludeNotFollowed(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"file", ""},
		{"dev-zero", "/dev/zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, info := legacyGitWorkspace(t)
			target := tc.target
			if target == "" {
				target = filepath.Join(t.TempDir(), "outside-exclude")
				require.NoError(t, os.WriteFile(target, []byte(legacyExcludeLine+"\n"), 0o644))
			}
			require.NoError(t, os.Symlink(target, filepath.Join(info, "exclude")))
			require.NoError(t, writeSentinel(filepath.Join(ws, ProvisionSentinelFile)))

			assert.False(t, hasLegacyExcludeLine(ws))
			res, err := PromoteLegacyMarker(t.Context(), ws)
			require.NoError(t, err)
			assert.False(t, res.ExcludeRemoved)
			if tc.target == "" {
				got, err := os.ReadFile(target)
				require.NoError(t, err)
				assert.Equal(t, legacyExcludeLine+"\n", string(got), "the symlink target is untouched")
			}
		})
	}
}

// A FIFO planted as the exclude file never blocks the promotion.
func TestPromoteLegacyMarker_FIFOExcludeDoesNotBlock(t *testing.T) {
	ws, info := legacyGitWorkspace(t)
	require.NoError(t, syscall.Mkfifo(filepath.Join(info, "exclude"), 0o644))
	require.NoError(t, writeSentinel(filepath.Join(ws, ProvisionSentinelFile)))

	type result struct {
		res LegacyPromotion
		err error
	}
	done := make(chan result, 1)
	go func() {
		needed := LegacyPromotionNeeded(ws)
		res, err := PromoteLegacyMarker(t.Context(), ws)
		if err == nil && !needed {
			err = errors.New("the marker alone must make the promotion needed")
		}
		done <- result{res, err}
	}()
	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.True(t, r.res.Promoted)
		assert.False(t, r.res.ExcludeRemoved)
	case <-time.After(10 * time.Second):
		t.Fatal("the promotion blocked on a FIFO exclude file")
	}
}

// The workspace root is read in chunks and only lock-litter names are kept:
// many other entries do not grow the result.
func TestLockLitterEntries_KeepsOnlyLitter(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3000; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%04d", i)), nil, 0o644))
	}
	for _, n := range []string{provisionFileLockName, provisionFileLockName + ".evict-a", provisionFileLockName + ".stage-b"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, n), 0o755))
	}
	got, err := lockLitterEntries(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range got {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{provisionFileLockName + ".evict-a", provisionFileLockName + ".stage-b"}, names)
}

// One listing returns at most lockLitterMaxEntries litter entries: with one
// litter entry more than the cap (next to non-litter entries and the live
// lock, which are never returned), exactly the cap is returned, each a
// distinct litter name, and a later sweep handles the rest.
func TestLockLitterEntries_StopsAtCap(t *testing.T) {
	dir := t.TempDir()
	prefix := provisionFileLockName + "."
	for i := 0; i <= lockLitterMaxEntries; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%sevict-%05d", prefix, i)), nil, 0o644))
	}
	for i := 0; i < 100; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%03d", i)), nil, 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, provisionFileLockName), 0o755))

	got, err := lockLitterEntries(dir)
	require.NoError(t, err)
	require.Len(t, got, lockLitterMaxEntries, "one listing stops at the cap")
	seen := make(map[string]bool, len(got))
	for _, e := range got {
		name := e.Name()
		assert.True(t, strings.HasPrefix(name, prefix), "not a litter entry: %s", name)
		assert.False(t, seen[name], "returned twice: %s", name)
		seen[name] = true
	}
}
