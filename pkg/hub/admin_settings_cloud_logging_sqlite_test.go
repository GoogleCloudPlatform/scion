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

//go:build !no_sqlite

package hub

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A workstation hub keeps the telemetry section in the database too, so the
// refusal of telemetry.cloud.cloud_logging (ptone/scion#5664) applies there
// as well: neither a DB row nor settings.yaml is written.
func TestPutServerConfigWorkstation_CloudLoggingChangeRefused(t *testing.T) {
	settingsPath := workstationHome(t)
	srv, st, _ := newSQLiteHubInMode(t, true, nil)
	require.True(t, srv.layer0Editable())
	before := hubSettingRevisions(t, st)
	rr := putServerConfig(t, srv, `{"telemetry":{"cloud":{"cloud_logging":true}}}`)
	code, keys := cloudLoggingRefusal(t, rr)
	assert.Equal(t, startupOnlyKeyRejectedCode, code)
	assert.Equal(t, []string{telemetryCloudLoggingKey}, keys)
	assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "cloud_logging")
}
