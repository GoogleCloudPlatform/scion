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
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// The per-agent record is read bounded and without following links; a
// record that cannot be read safely fails the start closed (it is not
// treated as "no pin").
func TestSharedDirRecordPinRefusal_BoundedRead(t *testing.T) {
	local := projectfs.Layout{Backend: projectfs.BackendLocal}

	t.Run("symlink record", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		require.NoError(t, os.WriteFile(target, []byte(`{"backend":"nfs"}`), 0o644))
		require.NoError(t, os.Symlink(target, filepath.Join(dir, legacySharedDirStorageRecordFile)))
		err := sharedDirRecordPinRefusal(dir, local, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot check this agent's shared-dir storage record")
		// A typed refusal (422 at the broker) whose public text names no path.
		se := storageRefusalOf(t, err)
		assert.NotContains(t, se.PublicMessage(), dir)
	})
	t.Run("oversized record", func(t *testing.T) {
		dir := t.TempDir()
		big := `{"backend":"local","dirs":{"x":"` + strings.Repeat("a", maxSharedDirRecordSize) + `"}}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, legacySharedDirStorageRecordFile), []byte(big), 0o644))
		err := sharedDirRecordPinRefusal(dir, local, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds")
	})
	t.Run("fifo record", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, syscall.Mkfifo(filepath.Join(dir, legacySharedDirStorageRecordFile+retiredSharedDirRecordSuffix), 0o644))
		err := sharedDirRecordPinRefusal(dir, local, nil)
		require.Error(t, err, "a FIFO is refused without blocking")
		assert.Contains(t, err.Error(), "not a regular file")
	})
	t.Run("record within the bound", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, legacySharedDirStorageRecordFile), []byte(`{"backend":"local"}`), 0o644))
		assert.NoError(t, sharedDirRecordPinRefusal(dir, local, nil))
	})
}
