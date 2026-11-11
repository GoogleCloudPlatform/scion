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

//go:build !no_sqlite && unix

package entadapter

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The snapshot file is 0600 whatever the database file's mode and the
// process umask. Not parallel: the umask is process-wide.
func TestSnapshotSQLite_FileMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dbMode os.FileMode
		umask  int
	}{
		{"permissive db and zero umask", 0o666, 0},
		{"default db and umask", 0o644, 0o022},
		{"private db", 0o600, 0o077},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, path := openOldSchema(t)
			require.NoError(t, os.Chmod(path, tc.dbMode))
			old := syscall.Umask(tc.umask)
			t.Cleanup(func() { syscall.Umask(old) })

			snap, err := SnapshotSQLite(context.Background(), db, "pre-test", time.Now())
			require.NoError(t, err)
			require.False(t, snap.Reused)
			info, err := os.Stat(snap.Path)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			assert.Greater(t, info.Size(), int64(0))
		})
	}
}
