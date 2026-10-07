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

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestOutDirDoesNotReplaceAFileThatAppears: a file created at the target
// name while the new bytes are being written is not replaced without
// replace, because the final move fails if the name exists.
func TestOutDirDoesNotReplaceAFileThatAppears(t *testing.T) {
	dir := t.TempDir()
	d, err := openOutDir(dir)
	require.NoError(t, err)
	defer func() { _ = d.Close() }()
	err = d.WriteFile("a.md", false, func(w io.Writer) error {
		if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("theirs"), 0o644); err != nil {
			return err
		}
		_, err := w.Write([]byte("ours"))
		return err
	})
	assert.ErrorContains(t, err, "already exists")
	got, _ := os.ReadFile(filepath.Join(dir, "a.md"))
	assert.Equal(t, "theirs", string(got))
	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 1, "no temporary file left behind")
}

// TestOutDirWithoutHardLinks: on a file system without hard links, a new
// file is still written, and an existing one is still not replaced.
func TestOutDirWithoutHardLinks(t *testing.T) {
	prev := linkat
	linkat = func(int, string, int, string, int) error { return unix.EPERM }
	t.Cleanup(func() { linkat = prev })
	dir := t.TempDir()
	d, err := openOutDir(dir)
	require.NoError(t, err)
	defer func() { _ = d.Close() }()
	write := func(w io.Writer) error { _, err := w.Write([]byte("new")); return err }
	require.NoError(t, d.WriteFile("a.md", false, write))
	got, _ := os.ReadFile(filepath.Join(dir, "a.md"))
	assert.Equal(t, "new", string(got))
	assert.ErrorContains(t, d.WriteFile("a.md", false, write), "already exists")

	// A file that appears while the new one is written, on such a file
	// system, is found by the check made just before the move.
	linkat = func(int, string, int, string, int) error {
		if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("theirs"), 0o644); err != nil {
			return err
		}
		return unix.EPERM
	}
	assert.ErrorContains(t, d.WriteFile("b.md", false, write), "already exists")
	got, _ = os.ReadFile(filepath.Join(dir, "b.md"))
	assert.Equal(t, "theirs", string(got))
	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 2, "no temporary file left behind")
}
