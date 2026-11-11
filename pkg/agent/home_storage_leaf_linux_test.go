//go:build linux

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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func homeStatMode(t *testing.T, p string) uint32 {
	t.Helper()
	var st unix.Stat_t
	require.NoError(t, unix.Lstat(p, &st))
	return st.Mode & 0o7777
}

// The broker creates the agent directory with the shared leaf modes and the
// home directory 2771, with no workspace tree, and leaves an existing home
// alone.
func TestOSHomeLeafHost_EnsureHome(t *testing.T) {
	base := t.TempDir()
	rel := "trees/" + hsTestProjectID + "/agents/agent-a"
	home := "home-" + hsTestAgentID
	require.NoError(t, osHomeLeafHost{}.EnsureHome(base, rel, home, os.Getgid()))
	assert.Equal(t, uint32(0o2775), homeStatMode(t, filepath.Join(base, rel)))
	assert.Equal(t, uint32(homeDirMode), homeStatMode(t, filepath.Join(base, rel, home)))
	_, err := os.Stat(filepath.Join(base, rel, "workspace"))
	assert.True(t, os.IsNotExist(err), "no workspace tree is created")

	require.NoError(t, unix.Chmod(filepath.Join(base, rel, home), 0o2770))
	require.NoError(t, os.WriteFile(filepath.Join(base, rel, home, "f"), []byte("x"), 0o600))
	require.NoError(t, osHomeLeafHost{}.EnsureHome(base, rel, home, os.Getgid()))
	assert.Equal(t, uint32(0o2770), homeStatMode(t, filepath.Join(base, rel, home)), "an existing home is left alone")

	// A link in place of the home is refused, not followed.
	outside := t.TempDir()
	home2 := "home-11111111-2222-4333-8444-555555555555"
	require.NoError(t, os.Symlink(outside, filepath.Join(base, rel, home2)))
	assert.Error(t, osHomeLeafHost{}.EnsureHome(base, rel, home2, os.Getgid()))
}

func TestOSHomeLeafHost_CheckMount(t *testing.T) {
	assert.Error(t, osHomeLeafHost{}.CheckMount(t.TempDir()), "a local directory is not an NFS mount")
	assert.Error(t, osHomeLeafHost{}.CheckMount(filepath.Join(t.TempDir(), "missing")))
}
