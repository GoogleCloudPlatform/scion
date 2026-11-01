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

package shareddirs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A directory removed after it was opened is an error for firstDirName, never
// "" (empty): Linux's getdents64 reports ENOENT for it. readDirNames reads the
// same case as "nothing left"; the emptiness check must not. No permission is
// involved, so the test runs as root too.
func TestFirstDirName_DirectoryRemovedAfterOpenIsAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	require.NoError(t, os.Mkdir(dir, 0o755))
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	require.NoError(t, os.Remove(dir))

	name, err := firstDirName(fd)
	require.Error(t, err, "a removed directory must not read as empty (got name %q)", name)
	assert.ErrorIs(t, err, unix.ENOENT)
	assert.Equal(t, "", name)
}

// The positive controls: an empty directory gives "" with no error, and a
// directory with one entry gives that name.
func TestFirstDirName_EmptyAndOneEntry(t *testing.T) {
	empty := t.TempDir()
	fd, err := unix.Open(empty, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	name, err := firstDirName(fd)
	require.NoError(t, err)
	assert.Equal(t, "", name)

	one := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(one, "entry"), 0o755))
	fd2, err := unix.Open(one, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd2) })
	name, err = firstDirName(fd2)
	require.NoError(t, err)
	assert.Equal(t, "entry", name)
}
