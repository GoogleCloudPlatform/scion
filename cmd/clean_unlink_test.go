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

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarkProjectUnlinked pins the unlink that 'scion clean' (its "unlink
// first" step) and 'scion hub unlink' share: hub.linked=false and the hub
// project ID cleared, while the retired hub.enabled switch is never written.
// A project without the key gets none, and an existing value is left as it
// was.
func TestMarkProjectUnlinked(t *testing.T) {
	tests := []struct {
		name        string
		settings    string
		wantEnabled *bool
	}{
		{name: "no hub.enabled key: none is written",
			settings: "project_id: clean-p\nhub:\n  linked: true\n  projectId: hub-p\n"},
		{name: "hub.enabled=true stays true",
			settings:    "project_id: clean-p\nhub:\n  enabled: true\n  linked: true\n  projectId: hub-p\n",
			wantEnabled: boolPtrForTest(true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hermeticCLIEnv(t)
			dir := filepath.Join(t.TempDir(), ".scion")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(tc.settings), 0o644))

			require.NoError(t, markProjectUnlinked(io.Discard, dir, false))

			own, err := config.LoadSettingsFromDir(dir)
			require.NoError(t, err)
			require.NotNil(t, own.Hub)
			require.NotNil(t, own.Hub.Linked, "hub.linked is recorded")
			assert.False(t, *own.Hub.Linked)
			assert.Empty(t, own.Hub.ProjectID, "hub project ID cleared")
			assert.Equal(t, tc.wantEnabled, own.Hub.Enabled, "hub.enabled is not written")
		})
	}
}
