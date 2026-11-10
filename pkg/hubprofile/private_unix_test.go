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

package hubprofile

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertPrivateFile: on unix a private file has mode 0600.
func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), path)
}

// assertPrivateDir: on unix a private directory has mode 0700.
func assertPrivateDir(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), path)
}

// Refuse + control: a cache entry or profile others can read (mode
// 0644) is refused on read; at 0600 it is read.
func TestReadRefusesOpenModes(t *testing.T) {
	home := t.TempDir()
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	require.NoError(t, res.Fill(sampleEntry()))
	require.NoError(t, os.Chmod(res.Path(), 0o644))
	_, err = ReadEntry(home, "t1", "r1", "owner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be private to you (mode 0600)")
	require.NoError(t, os.Chmod(res.Path(), 0o600))
	_, err = ReadEntry(home, "t1", "r1", "owner")
	require.NoError(t, err, "control: the 0600 entry is read")

	require.NoError(t, Save(home, validProfile(), false))
	path := Dir(home) + "/t1.yaml"
	require.NoError(t, os.Chmod(path, 0o644))
	_, err = Load(home, "t1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be private to you (mode 0600)")
	require.NoError(t, os.Chmod(path, 0o600))
	_, err = Load(home, "t1")
	require.NoError(t, err, "control: the 0600 profile is read")
}
