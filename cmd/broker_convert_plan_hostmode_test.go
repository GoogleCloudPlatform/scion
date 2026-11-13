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
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
)

// TestBrokerConvertPlanJSON_IncludesHostMode verifies that
// `scion runtime-broker convert --plan --format json` includes `hostMode`
// (`colocated`, `remote`, or `unknown`) under `schemaVersion: 1`, agrees with
// the human `--plan` Host mode line, writes nothing to disk, and leaks no
// local paths (ptone/scion#7627).
func TestBrokerConvertPlanJSON_IncludesHostMode(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(t *testing.T, globalDir string)
		wantMode  string
		wantHuman string
	}{
		{
			name: "colocated",
			setup: func(t *testing.T, globalDir string) {
				require.NoError(t, daemon.SaveArgs(serverDaemonComponent, globalDir, []string{"--enable-hub", "--enable-runtime-broker"}))
			},
			wantMode:  brokerconvert.HostModeColocated,
			wantHuman: "Host mode: colocated",
		},
		{
			name: "remote_broker_daemon",
			setup: func(t *testing.T, globalDir string) {
				require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, []string{}))
			},
			wantMode:  brokerconvert.HostModeRemote,
			wantHuman: "Host mode: remote",
		},
		{
			name: "remote_simulated_on_server",
			setup: func(t *testing.T, globalDir string) {
				require.NoError(t, daemon.SaveArgs(serverDaemonComponent, globalDir, []string{"--enable-hub", "--enable-runtime-broker", "--simulate-remote-broker"}))
			},
			wantMode:  brokerconvert.HostModeRemote,
			wantHuman: "Host mode: remote",
		},
		{
			name:      "unknown_without_saved_launch",
			setup:     func(*testing.T, string) {},
			wantMode:  brokerconvert.HostModeUnknown,
			wantHuman: "Host mode: unknown: --host-mode required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := convertTestHome(t, convertTestSettings)
			t.Setenv("USERPROFILE", home)
			globalDir := filepath.Join(home, ".scion")
			tc.setup(t, globalDir)
			before := convertTreeSnapshot(t, home)

			setConvertFlags(t, true, true, "json", "k8s")
			jsonOut, err := runConvertForTest(t)
			require.NoError(t, err)

			var doc struct {
				SchemaVersion int    `json:"schemaVersion"`
				HostMode      string `json:"hostMode"`
			}
			require.NoError(t, json.Unmarshal([]byte(jsonOut), &doc), jsonOut)
			assert.Equal(t, 1, doc.SchemaVersion)
			assert.Equal(t, tc.wantMode, doc.HostMode)
			assert.NotContains(t, jsonOut, home, "local paths must never appear in --plan JSON")
			assert.Equal(t, before, convertTreeSnapshot(t, home), "--plan writes nothing")

			setConvertFlags(t, true, true, "", "k8s")
			humanOut, err := runConvertForTest(t)
			require.NoError(t, err)
			assert.Contains(t, humanOut, tc.wantHuman)
		})
	}
}
