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

	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlatInstanceServerConfig_ForwardsConduitDeliverRuntimes(t *testing.T) {
	runtimes, err := config.ResolveConduitDeliverRuntimes([]string{"cloudrun", "podman"})
	require.NoError(t, err)

	cfg := config.DefaultGlobalConfig()
	sh := flatServerShared{
		cfg:                    &cfg,
		conduitDeliverRuntimes: runtimes,
	}
	rhCfg := flatInstanceServerConfig(sh, brokerhost.InstanceContext{
		Identity:   &brokeridentity.Identity{RuntimeBrokerID: "broker-1"},
		Instance:   config.V1RuntimeBrokerInstanceConfig{Name: "flat-1"},
		Activation: &brokerhost.Activation{},
	})
	assert.Equal(t, []string{"cloudrun", "podman"}, rhCfg.ConduitDeliverRuntimes)
}
