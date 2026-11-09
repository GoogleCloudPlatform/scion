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

package brokerhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestHostPrepare_PreconditionRunsBeforeNewRuntime: a refusing Precondition
// means NewRuntime and ProbeScope are never called for that instance, no
// identity is minted, its reason code reaches Status, and a sibling the
// Precondition passes activates.
func TestHostPrepare_PreconditionRunsBeforeNewRuntime(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(t, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"))
	var order []string
	newRuntime, probe := cfg.NewRuntime, cfg.ProbeScope
	cfg.Precondition = func(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig) error {
		order = append(order, "precondition:"+inst.Key)
		if inst.Key == "docker-a" {
			return WithReason("cloudrun_storage_backend", errors.New("the storage backend is not nfs"))
		}
		return nil
	}
	cfg.NewRuntime = func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
		order = append(order, "runtime:"+inst.Key)
		return newRuntime(ctx, inst)
	}
	cfg.ProbeScope = func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig, rt runtime.Runtime) (brokeridentity.ExecutionScope, error) {
		order = append(order, "probe:"+inst.Key)
		return probe(ctx, inst, rt)
	}
	h, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))

	assert.Equal(t, []string{"precondition:docker-a", "precondition:docker-b", "runtime:docker-b", "probe:docker-b"}, order)
	st := statusByKey(h)
	assert.Equal(t, StateRefused, st["docker-a"].State)
	assert.Equal(t, "cloudrun_storage_backend", st["docker-a"].Reason, "the reason code reaches Status")
	assert.Empty(t, st["docker-a"].RuntimeBrokerID, "no identity for a refused instance")
	_, statErr := os.Stat(brokeridentity.InstanceDir(f.globalDir, "docker-a"))
	assert.True(t, os.IsNotExist(statErr), "a refused instance mints nothing")
	assert.Equal(t, StateActive, st["docker-b"].State, "a sibling activates")
	assert.Contains(t, f.activator.refused, "docker-a")
}

// TestHostPrepare_CloudRunUnresolvedScopeNamesNoNumber: an instance refused
// because a sibling found unresolved ownership on their shared Cloud Run
// scope gets the drain-step refusal, which never names the scope identity
// (it carries the project number).
func TestHostPrepare_CloudRunUnresolvedScopeNamesNoNumber(t *testing.T) {
	const number = "123456789012"
	err := scopeUnresolvedError("cr-b", brokeridentity.ExecutionScope{Type: brokeridentity.TargetTypeCloudRun,
		CloudRun: &brokeridentity.CloudRunScope{ProjectNumber: number, Region: "us-central1", ProjectID: "example-project"}}, []string{"cr-a"})
	assert.Equal(t, `flat Runtime Broker instance "cr-b" not activated: Cloud Run agent instances in its project and region have unresolved ownership (found by instances cr-a); `+
		"drain them through the Runtime Broker that created them, then restart", err.Error())
	assert.NotContains(t, err.Error(), number)
	assert.NotContains(t, err.Error(), "example-project")

	docker := scopeUnresolvedError("docker-b", brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "d1"}}, []string{"docker-a"})
	assert.Contains(t, docker.Error(), "docker:d1", "other targets keep naming their scope")
}

// TestCheckInstanceDefaultVolumesAt: the per-instance form refuses a
// control-path mount and names the path it is given, and New's whole-config
// check keeps its index paths.
func TestCheckInstanceDefaultVolumesAt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	inst := dockerInstance("docker-a", "a")
	require.NoError(t, CheckInstanceDefaultVolumesAt("server.broker.instances[3]", inst))
	inst.AgentDefaults = &config.V1InstanceAgentDefaults{Volumes: []api.VolumeMount{{Source: "~/.scion", Target: "/x", ReadOnly: true}}}
	err = CheckInstanceDefaultVolumesAt("server.broker.instances[3]", inst)
	require.ErrorIs(t, err, brokercontrol.ErrControlPathMount)
	assert.Contains(t, err.Error(), "server.broker.instances[3].agent_defaults.volumes[0]")
	err = checkInstanceDefaultVolumes([]config.V1RuntimeBrokerInstanceConfig{dockerInstance("docker-z", "z"), inst})
	require.ErrorIs(t, err, brokercontrol.ErrControlPathMount)
	assert.Contains(t, err.Error(), "server.broker.instances[1].agent_defaults.volumes[0]")
}
