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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
	"github.com/stretchr/testify/require"
)

func TestReadHomeResetID(t *testing.T) {
	dir := t.TempDir()
	id, err := readHomeResetID(dir)
	require.NoError(t, err)
	require.Empty(t, id)
	for _, data := range []string{`{"reset_id":"reset-A19"}`, `{`, `{}`, `{"reset_id":"../escape"}`, `{"reset_id":"a_b"}`, `{"reset_id":"` + strings.Repeat("x", 65) + `"}`} {
		writePrivateResetRecord(t, dir, data)
		id, err := readHomeResetID(dir)
		if data == `{"reset_id":"reset-A19"}` {
			require.NoError(t, err)
			require.Equal(t, "reset-A19", id)
		} else {
			require.Error(t, err)
			require.Empty(t, id)
		}
	}
	require.NoError(t, os.Remove(filepath.Join(dir, homeResetRecordFile)))
	require.NoError(t, os.Mkdir(filepath.Join(dir, homeResetRecordFile), 0o700))
	_, err = readHomeResetID(dir)
	require.Error(t, err)
}

func TestReadHomeResetIDRejectsOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	// A valid record prefix must not hide trailing data beyond the read bound.
	data := `{"reset_id":"reset-1"}` + strings.Repeat(" ", 1024)
	writePrivateResetRecord(t, dir, data)
	id, err := readHomeResetID(dir)
	require.Error(t, err)
	require.Empty(t, id)
}

func TestReadHomeResetIDMissingAgentDir(t *testing.T) {
	id, err := readHomeResetID(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	require.Empty(t, id)
}

func writePrivateResetRecord(t *testing.T, dir, data string) {
	t.Helper()
	tree, err := privatefs.OpenTree(dir, privatefs.TreeOptions{})
	require.NoError(t, err)
	defer func() { require.NoError(t, tree.Close()) }()
	require.NoError(t, tree.WriteFile(homeResetRecordFile, []byte(data), privatefs.Private))
}
