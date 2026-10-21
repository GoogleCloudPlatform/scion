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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Startup repair of orphaned agents and project defaults with broker
// presence (design v2.21.50 §3.4).

// orphanRepairFixture holds two offline former brokers, each the default of
// one project and the broker of one running agent.
type orphanRepairFixture struct {
	s                         store.Store
	presentID, absentID       string
	presentAgent, absentAgent string
	presentProj, absentProj   string
}

func newOrphanRepairFixture(t *testing.T) *orphanRepairFixture {
	t.Helper()
	ctx := context.Background()
	f := &orphanRepairFixture{s: newTestStore(t), presentID: tid("orphan-present-broker"), absentID: tid("orphan-absent-broker")}
	for _, b := range []struct{ id, name string }{{f.presentID, "orphan-present"}, {f.absentID, "orphan-absent"}} {
		require.NoError(t, f.s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: b.id, Name: b.name, Slug: b.name, Status: store.BrokerStatusOffline, Created: time.Now(), Updated: time.Now(),
		}))
	}
	mk := func(name, brokerID string) (projectID, agentID string) {
		projectID, agentID = tid(name+"-project"), tid(name+"-agent")
		require.NoError(t, f.s.CreateProject(ctx, &store.Project{
			ID: projectID, Slug: name + "-project", Name: name + " project", DefaultRuntimeBrokerID: brokerID, Created: time.Now(), Updated: time.Now(),
		}))
		require.NoError(t, f.s.CreateAgent(ctx, &store.Agent{
			ID: agentID, ProjectID: projectID, Name: name + " agent", Slug: name + "-agent", Phase: "running", RuntimeBrokerID: brokerID,
		}))
		return projectID, agentID
	}
	f.presentProj, f.presentAgent = mk("orphan-present", f.presentID)
	f.absentProj, f.absentAgent = mk("orphan-absent", f.absentID)
	return f
}

func (f *orphanRepairFixture) register(t *testing.T, present brokerPresentFunc) string {
	t.Helper()
	newID := tid("orphan-new-broker")
	_, err := registerGlobalProjectAndBrokerPresence(context.Background(), f.s, present, newID, "orphan-new", "http://localhost:9800", nil, true, &config.Settings{}, nil, nil)
	require.NoError(t, err)
	return newID
}

func (f *orphanRepairFixture) agentBroker(t *testing.T, id string) string {
	t.Helper()
	a, err := f.s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a.RuntimeBrokerID
}

func (f *orphanRepairFixture) projectDefault(t *testing.T, id string) string {
	t.Helper()
	p, err := f.s.GetProject(context.Background(), id)
	require.NoError(t, err)
	return p.DefaultRuntimeBrokerID
}

// TestOrphanRepair_SkipsPresentBrokers: an agent whose former broker is
// present stays on it and that broker's project default is kept; the agent
// and project default of a broker that is not present move to the new
// broker.
func TestOrphanRepair_SkipsPresentBrokers(t *testing.T) {
	f := newOrphanRepairFixture(t)
	newID := f.register(t, func(_ context.Context, id string) (bool, error) { return id == f.presentID, nil })
	assert.Equal(t, f.presentID, f.agentBroker(t, f.presentAgent))
	assert.Equal(t, f.presentID, f.projectDefault(t, f.presentProj))
	assert.Equal(t, newID, f.agentBroker(t, f.absentAgent))
	assert.Equal(t, newID, f.projectDefault(t, f.absentProj))
}

// TestOrphanRepair_PresenceUnknownNotMoved: a broker whose presence cannot
// be read keeps its agent and its project default.
func TestOrphanRepair_PresenceUnknownNotMoved(t *testing.T) {
	f := newOrphanRepairFixture(t)
	newID := f.register(t, func(_ context.Context, id string) (bool, error) {
		if id == f.presentID {
			return false, errors.New("presence read failed")
		}
		return false, nil
	})
	assert.Equal(t, f.presentID, f.agentBroker(t, f.presentAgent))
	assert.Equal(t, f.presentID, f.projectDefault(t, f.presentProj))
	assert.Equal(t, newID, f.agentBroker(t, f.absentAgent))
}

// TestOrphanRepair_NilPresenceUsesTheStoreGuards: with no presence func the
// store's guards alone decide, as before: both offline brokers' agents and
// project defaults move.
func TestOrphanRepair_NilPresenceUsesTheStoreGuards(t *testing.T) {
	f := newOrphanRepairFixture(t)
	newID := f.register(t, nil)
	for _, id := range []string{f.presentAgent, f.absentAgent} {
		assert.Equal(t, newID, f.agentBroker(t, id))
	}
	for _, id := range []string{f.presentProj, f.absentProj} {
		assert.Equal(t, newID, f.projectDefault(t, id))
	}
}

// TestOrphanRepair_StartupLegacySignalsDecide: at startup, with hub.conduit
// on and no conduit registry open, the hub's presence helper decides on the
// legacy signals. A former broker stored offline whose affinity columns
// carry a fresh heartbeat (connected through a peer hub) keeps its agent
// and project default; a former broker with no legacy signal loses both.
func TestOrphanRepair_StartupLegacySignalsDecide(t *testing.T) {
	f := newOrphanRepairFixture(t)
	ctx := context.Background()
	b, err := f.s.GetRuntimeBroker(ctx, f.presentID)
	require.NoError(t, err)
	peerHub := "peer-hub"
	b.ConnectedHubID = &peerHub
	b.LastHeartbeat = time.Now()
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, b))

	_, err = f.s.UpsertHubSetting(ctx, "experiments", json.RawMessage(`{"overrides":{"hub.conduit":true}}`), "test", -1, "managed")
	require.NoError(t, err)
	srv, err := hub.New(hub.ServerConfig{}, f.s)
	require.NoError(t, err)
	ops := hub.NewOperationalSettings(f.s, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	require.True(t, srv.ConduitEnabled(), "hub.conduit is on")

	newID := f.register(t, srv.BrokerPresent)
	assert.Equal(t, f.presentID, f.agentBroker(t, f.presentAgent))
	assert.Equal(t, f.presentID, f.projectDefault(t, f.presentProj))
	assert.Equal(t, newID, f.agentBroker(t, f.absentAgent))
	assert.Equal(t, newID, f.projectDefault(t, f.absentProj))
}

// TestWithoutPresentBrokers_ReadsEachBrokerOnce: presence is read once per
// broker however many of its agents are orphan candidates.
func TestWithoutPresentBrokers_ReadsEachBrokerOnce(t *testing.T) {
	calls := map[string]int{}
	present := func(_ context.Context, id string) (bool, error) {
		calls[id]++
		return id == "b-present", nil
	}
	in := []*store.Agent{
		{ID: "a1", RuntimeBrokerID: "b-present"}, {ID: "a2", RuntimeBrokerID: "b-absent"},
		{ID: "a3", RuntimeBrokerID: "b-present"}, {ID: "a4", RuntimeBrokerID: "b-absent"},
	}
	got := withoutPresentBrokers(context.Background(), in, present)
	ids := make([]string, 0, len(got))
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	assert.Equal(t, []string{"a2", "a4"}, ids)
	assert.Equal(t, map[string]int{"b-present": 1, "b-absent": 1}, calls)
	assert.Len(t, in, 4, "the input slice is not changed")
	assert.Equal(t, "a1", in[0].ID)
}
