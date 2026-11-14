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
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setHubExperiments replaces srv's experiment overrides with on.
func setHubExperiments(t *testing.T, srv *Server, on map[string]bool) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"overrides": on})
	require.NoError(t, err)
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", raw)
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

// TestDispatchExperiments_ConduitDeliver: hub.conduit_deliver reaches the
// broker and the agent only while it is on and conduit is served (gap G6);
// turning either off removes it, so an agent launched then never
// registers deliver-message (A3).
func TestDispatchExperiments_ConduitDeliver(t *testing.T) {
	f := newRelayFixture(t, nil)
	has := func() bool { return slices.Contains(f.srv.dispatchExperiments(), experiments.ConduitDeliver) }

	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ConduitDeliver: true})
	assert.True(t, has(), "both on")
	assert.True(t, slices.Contains(f.srv.dispatchExperiments(), conduitExperiment))

	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true})
	assert.False(t, has(), "conduit_deliver off")

	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: false, experiments.ConduitDeliver: true})
	assert.False(t, has(), "conduit off")
}

// TestApplyAgentExperiments_ConduitDeliver: the launch flag is sent to the
// agent next to hub.conduit, and only when dispatched.
func TestApplyAgentExperiments_ConduitDeliver(t *testing.T) {
	env := map[string]string{}
	cls := map[string]api.EnvKind{}
	applyAgentExperiments(env, &cls, []string{conduitExperiment, experiments.ConduitDeliver, experiments.K8sNFSHome})
	assert.Equal(t, "hub.conduit,hub.conduit_deliver", env[envHubExperiments])

	env = map[string]string{envHubExperiments: "hub.conduit,hub.conduit_deliver"}
	applyAgentExperiments(env, &cls, []string{conduitExperiment})
	assert.Equal(t, "hub.conduit", env[envHubExperiments], "a forged flag does not survive")
}
