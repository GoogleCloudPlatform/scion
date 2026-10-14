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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// TestFlatInstanceServerConfig covers the production per-instance server
// configuration: the host NFS mount owner every instance shares, and the
// per-mode Hub wiring (co-located: HubInProcess + in-memory credentials;
// remote: validated instance credentials enable the Hub integration,
// control channel and heartbeat).
func TestFlatInstanceServerConfig(t *testing.T) {
	id := &brokeridentity.Identity{InstanceKey: "docker-a", RuntimeBrokerID: "rb-a",
		RuntimeTarget: api.RuntimeTargetDescriptor{ID: "t-a", Type: "docker"}}
	inst := config.V1RuntimeBrokerInstanceConfig{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}
	inMemory := &brokercredentials.BrokerCredentials{Name: "local", BrokerID: "rb-a", SecretKey: "c2VjcmV0"}
	remote := []brokercredentials.BrokerCredentials{{Name: "hub", BrokerID: "rb-a", SecretKey: "c2VjcmV0", HubEndpoint: "https://hub.example"}}

	for _, tc := range []struct {
		mode        brokerhost.Mode
		hubEndpoint string
		act         brokerhost.Activation
	}{
		{brokerhost.ModeColocated, "http://localhost:8080", brokerhost.Activation{InMemoryCredentials: inMemory}},
		{brokerhost.ModeRemote, "", brokerhost.Activation{RemoteCredentials: remote}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			act := tc.act
			sh := flatServerShared{cfg: &config.GlobalConfig{RuntimeBroker: config.RuntimeBrokerConfig{Host: "127.0.0.1", Port: 9800}},
				mode: tc.mode, hubEndpoint: tc.hubEndpoint, nfsMounter: runtimebroker.NewHostNFSMounter(nil, nil)}
			c := flatInstanceServerConfig(sh, brokerhost.InstanceContext{Instance: inst, Identity: id, Activation: &act})

			assert.Equal(t, "rb-a", c.BrokerID)
			assert.Equal(t, "a", c.BrokerName)
			assert.Same(t, sh.nfsMounter, c.NFSHostMounter, "every instance uses the host's single NFS mount owner")
			if assert.NotNil(t, c.FlatInstance) {
				assert.Same(t, id, c.FlatInstance.Identity)
				assert.Equal(t, tc.mode == brokerhost.ModeColocated, c.FlatInstance.HubInProcess)
				assert.Equal(t, act.RemoteCredentials, c.FlatInstance.RemoteCredentials)
			}
			assert.Equal(t, act.InMemoryCredentials, c.InMemoryCredentials)
			assert.True(t, c.HubEnabled, "Hub integration on (co-located endpoint or remote credentials)")
			assert.True(t, c.ControlChannelEnabled)
			assert.True(t, c.HeartbeatEnabled)
			assert.True(t, c.BrokerAuthEnabled)
			assert.True(t, c.BrokerAuthStrictMode)
			assert.Nil(t, c.DefaultProfile, "a flat instance reports no Runtime Broker Profile")
		})
	}
}

// TestFlatInstanceServerConfig_SharesOneWorkspaceLockService: every
// instance of a host gets the same process-wide workspace lock service.
func TestFlatInstanceServerConfig_SharesOneWorkspaceLockService(t *testing.T) {
	locks := runtimebroker.NewWorkspaceLocks()
	caches := &runtimebroker.SharedCaches{}
	sh := flatServerShared{cfg: &config.GlobalConfig{}, mode: brokerhost.ModeRemote, workspaceLocks: locks, sharedCaches: caches}
	for _, key := range []string{"docker-a", "docker-b"} {
		id := &brokeridentity.Identity{InstanceKey: key, RuntimeBrokerID: "rb-" + key}
		inst := config.V1RuntimeBrokerInstanceConfig{Key: key, Name: key, RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}
		c := flatInstanceServerConfig(sh, brokerhost.InstanceContext{Instance: inst, Identity: id, Activation: &brokerhost.Activation{}})
		assert.Same(t, locks, c.WorkspaceLocks, key)
		assert.Same(t, caches, c.SharedCaches, "every instance uses the host's caches: %s", key)
	}
}

// TestFlatInstanceServerConfig_ContainerHubAndColocatedStorage: an
// instance's container Hub settings come from its own runtime, and only a
// co-located instance (in-memory credentials) gets the co-located Hub's
// storage.
func TestFlatInstanceServerConfig_ContainerHubAndColocatedStorage(t *testing.T) {
	st, err := storage.NewLocal(storage.Config{LocalPath: t.TempDir()})
	require.NoError(t, err)
	var asked []string
	sh := flatServerShared{cfg: &config.GlobalConfig{}, mode: brokerhost.ModeColocated, colocatedStorage: st,
		containerHub: func(rtName string) containerHubEndpointResult {
			asked = append(asked, rtName)
			return containerHubEndpointResult{Endpoint: "http://hub-for-" + rtName, ColocatedPublicHubEndpoint: "https://public", HubListenPort: 8080}
		}}
	id := &brokeridentity.Identity{InstanceKey: "docker-a", RuntimeBrokerID: "rb-a"}
	inst := config.V1RuntimeBrokerInstanceConfig{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	colocated := flatInstanceServerConfig(sh, brokerhost.InstanceContext{Instance: inst, Identity: id, Runtime: rt,
		Activation: &brokerhost.Activation{InMemoryCredentials: &brokercredentials.BrokerCredentials{BrokerID: "rb-a"}}})
	assert.Equal(t, []string{"docker"}, asked, "the container Hub is resolved for the instance's own runtime")
	assert.Equal(t, "http://hub-for-docker", colocated.ContainerHubEndpoint)
	assert.Equal(t, "https://public", colocated.ColocatedPublicHubEndpoint)
	assert.Equal(t, 8080, colocated.HubListenPort)
	assert.Same(t, st, colocated.ColocatedStorage)
	withDir := flatInstanceServerConfig(sh, brokerhost.InstanceContext{Instance: inst, Identity: id, Runtime: rt, StateDir: "/state/rb-a",
		Activation: &brokerhost.Activation{InMemoryCredentials: &brokercredentials.BrokerCredentials{BrokerID: "rb-a"}}})
	assert.Equal(t, "/state/rb-a", withDir.StateDir, "the server uses the state root the host resolved")

	remote := flatInstanceServerConfig(sh, brokerhost.InstanceContext{Instance: inst, Identity: id, Runtime: rt,
		Activation: &brokerhost.Activation{RemoteCredentials: []brokercredentials.BrokerCredentials{{BrokerID: "rb-a"}}}})
	assert.Nil(t, remote.ColocatedStorage, "a remote instance never uses the co-located Hub's storage")
}
