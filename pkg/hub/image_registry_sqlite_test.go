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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registryBrokers creates brokers in the SQLite store: one reporting a node
// env, one reporting none, one with a registered default profile, and a
// flat broker.
func registryBrokers(t *testing.T, st store.Store) (reporting, silent, withDefault, flat string) {
	t.Helper()
	ctx := context.Background()
	mk := func(name string, b store.RuntimeBroker) string {
		b.ID = tid(name + "-" + t.Name())
		b.Name, b.Slug, b.Status = name, tidSlugSafe(name+"-"+t.Name()), store.BrokerStatusOnline
		require.NoError(t, st.CreateRuntimeBroker(ctx, &b))
		return b.ID
	}
	reporting = mk("reg-reporting", store.RuntimeBroker{Capabilities: &store.BrokerCapabilities{NodeImageRegistry: "node-1.example"}})
	silent = mk("reg-silent", store.RuntimeBroker{Capabilities: &store.BrokerCapabilities{}})
	withDefault = mk("reg-default", store.RuntimeBroker{DefaultProfile: "k8s"})
	flat = mk("reg-flat", store.RuntimeBroker{Endpoint: "http://flat.invalid",
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("reg-flat-target-" + t.Name()), Type: "docker"}})
	return
}

func TestDispatchImageRegistry_PerBroker(t *testing.T) {
	bootstrap := newFileKoanf(t, map[string]interface{}{
		"image_registry":                "file.example",
		"profiles.k8s.image_registry":   "file-k8s.example",
		"profiles.k8s.runtime":          "kubernetes",
		"profiles.local.runtime":        "docker",
		"profiles.local.image_registry": "",
	})
	srv, st, ops := newSQLiteOpsServer(t, bootstrap, nil)
	srv.config.NodeImageRegistry = "hub-node.example"
	reporting, silent, withDefault, flat := registryBrokers(t, st)

	// Each broker gets its own node env; the hub's own env never applies.
	assert.Equal(t, "node-1.example", srv.dispatchImageRegistryFor(reporting, "k8s"))
	assert.Equal(t, "file-k8s.example", srv.dispatchImageRegistryFor(silent, "k8s"), "a broker reporting none: the hub's env is not used")
	assert.Equal(t, "file.example", srv.dispatchImageRegistryFor(silent, "local"))
	assert.Equal(t, "file.example", srv.dispatchImageRegistryFor(tid("unknown-"+t.Name()), ""), "an unknown broker has no node env")

	// An agent with no saved profile resolves to the broker's default profile.
	assert.Equal(t, "file-k8s.example", srv.dispatchImageRegistryFor(withDefault, ""))
	r, ok := srv.dispatchImageRegistryResolved(withDefault, "")
	require.True(t, ok)
	assert.Equal(t, config.ImageRegistrySourceFileProfile, r.Source)

	// A flat target is never rewritten by the hub: the broker rewrites.
	_, ok = srv.dispatchImageRegistryResolved(flat, "k8s")
	assert.False(t, ok, "the hub does not rewrite for a flat target")
	assert.Equal(t, "", srv.dispatchImageRegistryFor(flat, ""))

	// A managed hub value beats the file layers, but not a broker's env.
	_, err := ops.Update(context.Background(), "endpoints", json.RawMessage(`{"image_registry":"db.example"}`), "admin", -1, "managed")
	require.NoError(t, err)
	assert.Equal(t, "db.example", srv.dispatchImageRegistryFor(silent, "k8s"), "a managed top beats a file profile")
	assert.Equal(t, "node-1.example", srv.dispatchImageRegistryFor(reporting, "k8s"))
	_, err = ops.Update(context.Background(), "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_registry":"db-k8s.example"},"local":{"runtime":"docker"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	r, _ = srv.dispatchImageRegistryResolved(silent, "k8s")
	assert.Equal(t, config.ResolvedImageRegistry{Value: "db-k8s.example", Source: config.ImageRegistrySourceHubDBProfile}, r)
}

// The HTTP dispatcher rewrites each agent's explicit image for its own
// broker and saved profile.
func TestDispatchImageForBroker_UsesAgentBrokerAndProfile(t *testing.T) {
	bootstrap := newFileKoanf(t, map[string]interface{}{"image_registry": "file.example"})
	srv, st, _ := newSQLiteOpsServer(t, bootstrap, nil)
	reporting, silent, _, flat := registryBrokers(t, st)
	d := &HTTPAgentDispatcher{}
	d.SetImageRegistryProvider(srv.dispatchImageRegistryFor)
	agent := func(broker string) *store.Agent {
		return &store.Agent{RuntimeBrokerID: broker, AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{Image: "scion-claude:v1"},
		}}
	}
	assert.Equal(t, "node-1.example/scion-claude:v1", d.dispatchImageForBroker(agent(reporting)))
	assert.Equal(t, "file.example/scion-claude:v1", d.dispatchImageForBroker(agent(silent)))
	assert.Equal(t, "scion-claude:v1", d.dispatchImageForBroker(agent(flat)), "a flat target gets the bare image")
}
