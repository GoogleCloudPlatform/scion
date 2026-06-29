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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
)

func TestCloudRunLogicalBrokerIDIsDeterministic(t *testing.T) {
	settings := &config.VersionedSettings{
		ActiveProfile: "default",
		Profiles: map[string]config.V1ProfileConfig{
			"default": {Runtime: "cloudrun"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"cloudrun": {
				Type: "cloudrun",
				CloudRun: &config.V1CloudRunConfig{
					Project: "test-project",
					Region:  "us-central1",
				},
			},
		},
	}
	rt := &scionruntime.MockRuntime{NameFunc: func() string { return "cloudrun" }}

	id1, err1 := deriveCloudRunLogicalBrokerID(settings, rt)
	assert.NoError(t, err1)
	id2, err2 := deriveCloudRunLogicalBrokerID(settings, rt)
	assert.NoError(t, err2)

	assert.NotEmpty(t, id1)
	assert.Equal(t, id1, id2)
}

func TestResolveBrokerIDPrefersConfiguredIDOverDefault(t *testing.T) {
	cfg := &config.GlobalConfig{}
	settings := &config.Settings{
		Hub: &config.HubClientConfig{BrokerID: "configured-broker"},
	}

	got := resolveBrokerID(cfg, settings, nil, t.TempDir(), "cloudrun-default")

	assert.Equal(t, "configured-broker", got)
}
