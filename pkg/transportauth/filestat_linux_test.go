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

//go:build linux

package transportauth

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fileStatExtra reports the change time and link count from the Lstat
// result itself, so a hard link shows up as a different link count.
func TestFileStatExtra_Linux(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tt")
	require.NoError(t, os.WriteFile(path, []byte("tok"), 0o600))

	fi, err := os.Lstat(path)
	require.NoError(t, err)
	ctime, nlink, ok := fileStatExtra(fi)
	require.True(t, ok)
	st := fi.Sys().(*syscall.Stat_t)
	assert.True(t, ctime.Equal(time.Unix(st.Ctim.Unix())))
	assert.Equal(t, uint64(1), nlink)

	// Move the modification time an hour into the past. The change time is
	// set to the time of that call, so it must differ from the modification
	// time and be close to now; reading the wrong timestamp would fail here.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, past, past))
	fi, err = os.Lstat(path)
	require.NoError(t, err)
	require.True(t, fi.ModTime().Equal(past))
	ctime, _, ok = fileStatExtra(fi)
	require.True(t, ok)
	assert.False(t, ctime.Equal(fi.ModTime()), "change time must not be the modification time")
	assert.WithinDuration(t, time.Now(), ctime, time.Minute)

	require.NoError(t, os.Link(path, filepath.Join(dir, "other")))
	fi, err = os.Lstat(path)
	require.NoError(t, err)
	_, nlink, ok = fileStatExtra(fi)
	require.True(t, ok)
	assert.Equal(t, uint64(2), nlink)
}
