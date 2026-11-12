//go:build unix

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

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestReadHomeResetIDRejectsLinks(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		name := "live"
		if dangling {
			name = "dangling"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(t.TempDir(), "record")
			if !dangling {
				require.NoError(t, os.WriteFile(target, []byte(`{"reset_id":"external"}`), 0o600))
			}
			require.NoError(t, os.Symlink(target, filepath.Join(dir, homeResetRecordFile)))
			id, err := readHomeResetID(dir)
			require.ErrorIs(t, err, privatefs.ErrReparsePoint)
			require.Empty(t, id)
		})
	}
}

func TestReadHomeResetIDRejectsNonPrivateRecord(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, homeResetRecordFile)
	require.NoError(t, os.WriteFile(record, []byte(`{"reset_id":"reset-1"}`), 0o600))
	require.NoError(t, os.Chmod(record, 0o644))
	id, err := readHomeResetID(dir)
	require.ErrorIs(t, err, privatefs.ErrNotPrivate)
	require.Empty(t, id)
}

func TestReadHomeResetIDRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, unix.Mkfifo(filepath.Join(dir, homeResetRecordFile), 0o600))
	id, err := readHomeResetID(dir)
	require.Error(t, err)
	require.Empty(t, id)
}
