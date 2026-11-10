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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
)

// TestResolveHubIDSource covers resolveHubIDSource's precedence (pinned,
// then Cloud Run's K_SERVICE, then the persisted ~/.scion/hub-id when it
// matches the resolved ID, else hostname) and its fallbacks: no persisted
// file, an unreadable home and an empty resolved ID all give hostname.
// The inputs come from the process environment and HOME, so the subtests
// set them with t.Setenv and are not parallel.
func TestResolveHubIDSource(t *testing.T) {
	const resolved = "a1b2c3"
	for _, tt := range []struct {
		name     string
		pinned   bool
		kService string
		// persisted is the hub-id file content; "-" writes no file;
		// "nohome" leaves HOME empty (os.UserHomeDir fails).
		persisted string
		resolved  string
		want      string
	}{
		{"pinned wins over everything", true, "svc", resolved, resolved, lifecycleevents.HubIDSourcePinned},
		{"pinned with nothing else", true, "", "-", resolved, lifecycleevents.HubIDSourcePinned},
		{"K_SERVICE before the persisted file", false, "svc", resolved, resolved, lifecycleevents.HubIDSourceKService},
		{"persisted file matching the resolved ID", false, "", resolved, resolved, lifecycleevents.HubIDSourcePersistedFile},
		{"persisted file with surrounding whitespace", false, "", "  " + resolved + "\n", resolved, lifecycleevents.HubIDSourcePersistedFile},
		{"persisted file naming another ID", false, "", "ffffff", resolved, lifecycleevents.HubIDSourceHostname},
		{"no persisted file", false, "", "-", resolved, lifecycleevents.HubIDSourceHostname},
		{"home unavailable", false, "", "nohome", resolved, lifecycleevents.HubIDSourceHostname},
		{"empty resolved ID never matches an empty file", false, "", "", "", lifecycleevents.HubIDSourceHostname},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("K_SERVICE", tt.kService)
			switch tt.persisted {
			case "nohome":
				t.Setenv("HOME", "")
			default:
				home := t.TempDir()
				t.Setenv("HOME", home)
				if tt.persisted != "-" {
					dir := filepath.Join(home, ".scion")
					require.NoError(t, os.MkdirAll(dir, 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(dir, "hub-id"), []byte(tt.persisted), 0o600))
				}
			}
			assert.Equal(t, tt.want, resolveHubIDSource(tt.pinned, tt.resolved))
		})
	}
}
