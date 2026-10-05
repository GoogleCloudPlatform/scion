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

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubAgentDefaults_RuntimeBrokerAndTimezoneSurviveReseed pins
// ptone/scion#2073 review finding 11 on the hub side: settings.yaml
// default_runtime_broker and default_timezone, extracted into the
// agent_defaults row as syncHubSettings does on boot (pinned in cmd by
// TestBootReseed_FileRuntimeBrokerAndTimezoneSurvive), reach the hub's
// agent defaults after Refresh -> Snapshot -> ApplySnapshot.
func TestHubAgentDefaults_RuntimeBrokerAndTimezoneSurviveReseed(t *testing.T) {
	bootK := newFileKoanf(t, map[string]interface{}{
		"default_runtime_broker": "broker-1",
		"default_timezone":       "America/Los_Angeles",
	})
	doc, err := opsettings.ExtractSectionFromKoanf(bootK, "agent_defaults")
	require.NoError(t, err)

	settings := newFakeHubSettingStore()
	settings.seedWithOrigin("agent_defaults", doc, "seeded")
	ops := NewOperationalSettings(settings, bootK, emptyKoanf())
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)

	srv := &Server{}
	ApplySnapshot(srv, ops.Snapshot())
	d := srv.hubAgentDefaults()
	assert.Equal(t, "broker-1", d.DefaultRuntimeBroker)
	assert.Equal(t, "America/Los_Angeles", d.DefaultTimezone)
}
