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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)

// TestConduitRegistry_ListRelaysAndEligibleRelays (design §3.9 "Relay
// selection"): ListRelays reads every relay row; EligibleRelays keeps the
// live (the registry's RelayStaleAfter, not a fixed bound), non-draining,
// addressable relays, for a transport relays serve.
func TestConduitRegistry_ListRelaysAndEligibleRelays(t *testing.T) {
	f := newConduitFixture(t)
	short := registry.New(f.store, registry.Config{Clock: f.clock, RelayStaleAfter: 20 * time.Second})

	genA := f.registerRelay("relay-a")
	f.registerRelay("relay-b")
	genC := f.registerRelay("relay-c")
	_, err := f.reg.RegisterRelay(f.ctx, registry.RelayInstance{InstanceID: "relay-d"}) // unaddressable
	require.NoError(t, err)
	require.NoError(t, f.reg.SetRelayDraining(f.ctx, "relay-c", genC, true))

	f.clock.Advance(30 * time.Second)
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-a", genA))
	now := f.clock.Now()

	all, err := f.store.ListRelays(f.ctx)
	require.NoError(t, err)
	var ids []string
	for _, ri := range all {
		ids = append(ids, ri.InstanceID)
	}
	assert.Equal(t, []string{"relay-a", "relay-b", "relay-c", "relay-d"}, ids, "every row, ordered by instance_id")
	assert.Equal(t, "http://relay-a:9811", all[0].InternalEndpoint)
	assert.Equal(t, genA, all[0].Generation)
	assert.True(t, all[0].LastSeen.Equal(now))
	assert.True(t, all[2].Draining)

	eligible := func(reg *registry.Registry, transport string) []string {
		t.Helper()
		rs, err := reg.EligibleRelays(f.ctx, transport, now)
		require.NoError(t, err)
		out := []string{}
		for _, ri := range rs {
			out = append(out, ri.InstanceID)
		}
		return out
	}
	// Default staleness (60s): relay-b, last seen 30s ago, is live; the
	// draining and the unaddressable relays never are.
	assert.Equal(t, []string{"relay-a", "relay-b"}, eligible(f.reg, registry.TransportWS))
	// RelayStaleAfter 20s: relay-b is stale.
	assert.Equal(t, []string{"relay-a"}, eligible(short, registry.TransportWS))
	// Relays serve ws only.
	assert.Empty(t, eligible(f.reg, registry.TransportGRPC))
}

// TestConduitRegistry_EligibleRelaysFailsClosed: a read error is returned,
// never an empty (or partial) list of eligible relays.
func TestConduitRegistry_EligibleRelaysFailsClosed(t *testing.T) {
	f := newConduitFixture(t)
	f.registerRelay("relay-a")
	fs := &registry.FaultStore{Inner: f.store, Fault: func(_ context.Context, op string) error {
		if op == registry.OpListRelays {
			return assert.AnError
		}
		return nil
	}}
	reg := registry.New(fs, registry.Config{Clock: f.clock})
	_, err := reg.EligibleRelays(f.ctx, registry.TransportWS, f.clock.Now())
	require.ErrorIs(t, err, assert.AnError)
}

// TestConduitRegistry_LiveRelays (design §3.9 "Drain through the proxy"):
// LiveRelays keeps the live (RelayStaleAfter), addressable relays,
// draining ones included, since a draining relay may still hold sessions.
func TestConduitRegistry_LiveRelays(t *testing.T) {
	f := newConduitFixture(t)
	short := registry.New(f.store, registry.Config{Clock: f.clock, RelayStaleAfter: 20 * time.Second})

	genA := f.registerRelay("relay-a")
	f.registerRelay("relay-b")
	genC := f.registerRelay("relay-c")
	_, err := f.reg.RegisterRelay(f.ctx, registry.RelayInstance{InstanceID: "relay-d"}) // unaddressable
	require.NoError(t, err)
	require.NoError(t, f.reg.SetRelayDraining(f.ctx, "relay-c", genC, true))

	f.clock.Advance(30 * time.Second)
	require.NoError(t, f.reg.HeartbeatRelay(f.ctx, "relay-a", genA))
	now := f.clock.Now()

	live := func(reg *registry.Registry) []string {
		t.Helper()
		rs, err := reg.LiveRelays(f.ctx, now)
		require.NoError(t, err)
		out := []string{}
		for _, ri := range rs {
			out = append(out, ri.InstanceID)
		}
		return out
	}
	// Default staleness (60s): relay-b (30s ago) and the draining relay-c
	// are live; the unaddressable relay-d never is.
	assert.Equal(t, []string{"relay-a", "relay-b", "relay-c"}, live(f.reg))
	// RelayStaleAfter 20s: only relay-a, heartbeated now.
	assert.Equal(t, []string{"relay-a"}, live(short))

	fs := &registry.FaultStore{Inner: f.store, Fault: func(_ context.Context, op string) error {
		if op == registry.OpListRelays {
			return assert.AnError
		}
		return nil
	}}
	_, err = registry.New(fs, registry.Config{Clock: f.clock}).LiveRelays(f.ctx, now)
	require.ErrorIs(t, err, assert.AnError, "a read error is returned, never a partial list")
}

// TestConduitRegistry_RelayFeatures (design v2.21.54): relay_instances.features
// round-trips a JSON string array written at registration, including names
// a reader does not know; a relay registered with none stores NULL, and NULL
// or a malformed value reads as no features. Re-registration replaces the
// list (fixed per generation).
func TestConduitRegistry_RelayFeatures(t *testing.T) {
	f := newConduitFixture(t)
	_, err := f.reg.RegisterRelay(f.ctx, registry.RelayInstance{InstanceID: "relay-a", InternalEndpoint: "http://relay-a:9811",
		Features: []string{registry.RelayFeatureSingleton, "a_future_feature"}})
	require.NoError(t, err)
	f.registerRelay("relay-b")

	features := func(id string) []string {
		t.Helper()
		all, err := f.store.ListRelays(f.ctx)
		require.NoError(t, err)
		for _, ri := range all {
			if ri.InstanceID == id {
				return ri.Features
			}
		}
		t.Fatalf("relay %s is not registered", id)
		return nil
	}
	assert.Equal(t, []string{registry.RelayFeatureSingleton, "a_future_feature"}, features("relay-a"))
	assert.Nil(t, features("relay-b"), "no features is stored as NULL and reads as none")

	raw, err := f.store.client.RelayInstance.Get(f.ctx, "relay-b")
	require.NoError(t, err)
	assert.Nil(t, raw.Features, "the column is NULL when there are no features")

	require.NoError(t, f.store.client.RelayInstance.UpdateOneID("relay-b").SetFeatures("not json").Exec(f.ctx))
	assert.Nil(t, features("relay-b"), "a malformed value reads as no features")

	_, err = f.reg.RegisterRelay(f.ctx, registry.RelayInstance{InstanceID: "relay-a", InternalEndpoint: "http://relay-a:9811"})
	require.NoError(t, err)
	assert.Nil(t, features("relay-a"), "re-registration replaces the list")
}

// TestConduitRegistry_FeaturesColumnMissing (design v2.21.54, rollout
// order): against a schema the hubs have not migrated yet (no
// relay_instances.features), RegisterRelay fails with
// registry.ErrRelayFeaturesColumnMissing, for every relay, so the relay's
// startup error names the fix.
func TestConduitRegistry_FeaturesColumnMissing(t *testing.T) {
	f := newConduitFixture(t)
	drv, ok := entc.SQLDriver(f.store.client.Driver())
	require.True(t, ok)
	_, err := drv.DB().ExecContext(f.ctx, "ALTER TABLE relay_instances DROP COLUMN features")
	require.NoError(t, err)
	for _, features := range [][]string{nil, {registry.RelayFeatureSingleton}} {
		_, err := f.reg.RegisterRelay(f.ctx, registry.RelayInstance{InstanceID: "relay-a", InternalEndpoint: "http://relay-a:9811", Features: features})
		require.ErrorIs(t, err, registry.ErrRelayFeaturesColumnMissing, "features %v", features)
	}
}
