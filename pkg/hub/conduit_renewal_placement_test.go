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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Renewal applies the open path's placement guard (mintConduitGrant,
// checkAgentPinnedPlacement): a stream to a pinned agent whose saved
// placement is inconsistent gets no renewal.

// renewalPlacementAgents creates a flat Runtime Broker with a runtime target
// and returns three copies of the fixture's agent (assigned to "broker-1"):
// pinned to the flat broker while assigned elsewhere (a stale pin), pinned
// to another target on the flat broker (a mismatch), and pinned
// consistently.
func renewalPlacementAgents(t *testing.T, f *renewalFixture, name string) (stale, mismatch, consistent store.Agent) {
	t.Helper()
	flat := &store.RuntimeBroker{ID: tid(name), Name: name, Slug: name, Status: store.BrokerStatusOnline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid(name + "-target"), Type: "docker"}}
	require.NoError(t, f.store.CreateRuntimeBroker(context.Background(), flat))

	stale = *f.agent
	stale.PinnedRuntimeBrokerID = flat.ID
	stale.PinnedRuntimeTargetID = flat.RuntimeTarget.ID
	stale.PinnedRuntimeTargetType = "docker"
	mismatch = *f.agent
	mismatch.RuntimeBrokerID = flat.ID
	mismatch.PinnedRuntimeBrokerID = flat.ID
	mismatch.PinnedRuntimeTargetID = "another-target"
	mismatch.PinnedRuntimeTargetType = "docker"
	consistent = mismatch
	consistent.PinnedRuntimeTargetID = flat.RuntimeTarget.ID
	return stale, mismatch, consistent
}

// installRenewalAgentOverride makes the server's agent reads return a copy
// of whichever agent is set on the returned store, so renewal sees placement
// states the store would not accept as a row.
func installRenewalAgentOverride(t *testing.T, f *renewalFixture) *agentOverrideStore {
	t.Helper()
	o, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *agentOverrideStore {
		return &agentOverrideStore{Store: inner}
	})
	return o
}

// TestConduitRenewStream_PlacementRefusal: a stream renews while its agent
// is placed consistently; once the agent is a pinned agent with a stale pin
// or a target mismatch, a renewal is refused with the placement refusal and
// none is minted; placed consistently again, it renews.
func TestConduitRenewStream_PlacementRefusal(t *testing.T) {
	f := newRenewalFixture(t)
	stale, mismatch, consistent := renewalPlacementAgents(t, f, "renew-flat")
	override := installRenewalAgentOverride(t, f)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)

	f.clock.Advance(time.Minute)
	r, err := f.renew(tok, open, 7)
	require.NoError(t, err, "an unpinned agent renews")
	f.targetAccepts(t, r, open, 7, open.AuthzUntil)
	last := r

	for _, tc := range []struct {
		name  string
		agent store.Agent
		code  string
	}{
		{"stale pin", stale, ErrCodeRuntimeTargetPinStale},
		{"target mismatch", mismatch, ErrCodeRuntimeTargetMismatch},
	} {
		override.set(&tc.agent)
		f.clock.Advance(time.Minute)
		r, err := f.renew(last.Token, open, 7)
		assert.Nil(t, r, "%s: a renewal was minted", tc.name)
		require.ErrorIs(t, err, errConduitRenewalUnavailable, tc.name)
		requirePlacementRefusal(t, err, tc.code)
		assert.NotErrorIs(t, err, errConduitAuthzUnavailable, tc.name)
		assert.Equal(t, ConduitRenewalUnavailable, ClassifyConduitRenewal(err), "%s: a placement refusal must not close the stream", tc.name)
	}

	override.set(&consistent)
	f.clock.Advance(time.Minute)
	r, err = f.renew(last.Token, open, 7)
	require.NoError(t, err, "a consistently pinned agent renews")
	f.targetAccepts(t, r, open, 7, last.AuthzUntil)
}

// TestConduitRenewStream_PlacementReadFailure: when the agent's Runtime
// Broker row cannot be read, a pinned agent's stream gets no renewal and the
// failure is a transient one (errConduitAuthzUnavailable), never a refusal
// or a success; an unpinned agent keeps today's path and renews, and a
// managed runtime is not checked.
func TestConduitRenewStream_PlacementReadFailure(t *testing.T) {
	boom := errors.New("injected broker read failure")

	t.Run("pinned agent", func(t *testing.T) {
		f := newRenewalFixture(t)
		_, _, consistent := renewalPlacementAgents(t, f, "renew-fault-flat")
		override := installRenewalAgentOverride(t, f)
		override.set(&consistent)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		_, fault := installStoreFault(t, f.srv, brokerReadFaultWrap(boom))
		fault.Arm()

		f.clock.Advance(time.Minute)
		r, err := f.renew(tok, open, 7)
		assert.Nil(t, r, "a renewal was minted without a placement answer")
		require.ErrorIs(t, err, errConduitAuthzUnavailable)
		require.ErrorIs(t, err, errConduitRenewalUnavailable)
		require.ErrorIs(t, err, boom)
		assert.Equal(t, ConduitRenewalUnavailable, ClassifyConduitRenewal(err), "a placement read failure must not close the stream")
		_, refused := placementRefusalOf(err)
		assert.False(t, refused)
	})

	t.Run("unpinned and managed agents", func(t *testing.T) {
		f := newRenewalFixture(t)
		override := installRenewalAgentOverride(t, f)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		_, fault := installStoreFault(t, f.srv, brokerReadFaultWrap(boom))
		fault.Arm()

		f.clock.Advance(time.Minute)
		r, err := f.renew(tok, open, 7) // on "broker-1", whose row read now fails
		require.NoError(t, err, "an unpinned agent keeps today's path")
		f.targetAccepts(t, r, open, 7, open.AuthzUntil)

		managed := *f.agent
		managed.Runtime = ManagedRuntimePrefix + "test"
		managed.PinnedRuntimeBrokerID = "broker-1"
		managed.PinnedRuntimeTargetID = "managed-target"
		managed.PinnedRuntimeTargetType = "docker"
		override.set(&managed)
		f.clock.Advance(time.Minute)
		r2, err := f.renew(r.Token, open, 7)
		require.NoError(t, err, "a managed runtime is not checked")
		f.targetAccepts(t, r2, open, 7, r.AuthzUntil)
	})
}

// placementDecision names the placement outcome of an open (mint) or a
// renewal: allowed, refused with a placement code, or a transient failure
// carrying the injected read error.
func placementDecision(err, readErr error) string {
	switch {
	case err == nil:
		return "allowed"
	case readErr != nil && errors.Is(err, readErr):
		return "transient"
	}
	if r, refused := placementRefusalOf(err); refused {
		return "refused " + r.Code
	}
	return "other: " + err.Error()
}

// TestConduitPlacement_OpenAndRenewalDecideAlike: for every placement state
// (consistent, stale pin, target mismatch, unpinned on a flat Runtime
// Broker, pinned with a missing Runtime Broker row, unpinned, managed, and a failed
// Runtime Broker read for pinned, unpinned and managed agents) an open
// (mintConduitGrant) and a renewal give the same decision, and that
// decision is the expected one.
func TestConduitPlacement_OpenAndRenewalDecideAlike(t *testing.T) {
	type state struct {
		name  string
		agent func(f *renewalFixture, stale, mismatch, consistent store.Agent) store.Agent
		want  string
	}
	unpinned := func(f *renewalFixture, _, _, _ store.Agent) store.Agent { return *f.agent }
	managed := func(f *renewalFixture, _, _, _ store.Agent) store.Agent {
		a := *f.agent
		a.Runtime = ManagedRuntimePrefix + "test"
		a.PinnedRuntimeBrokerID = "broker-1"
		a.PinnedRuntimeTargetID = "managed-target"
		a.PinnedRuntimeTargetType = "docker"
		return a
	}
	pinned := func(_ *renewalFixture, _, _, consistent store.Agent) store.Agent { return consistent }
	// An unpinned agent assigned to the flat Runtime Broker.
	unpinnedOnFlat := func(_ *renewalFixture, _, _, consistent store.Agent) store.Agent {
		a := consistent
		a.PinnedRuntimeBrokerID, a.PinnedRuntimeTargetID, a.PinnedRuntimeTargetType = "", "", ""
		return a
	}
	// A pinned agent whose Runtime Broker row does not exist.
	pinnedMissingRow := func(_ *renewalFixture, _, _, consistent store.Agent) store.Agent {
		a := consistent
		a.RuntimeBrokerID = tid("parity-no-such-broker")
		a.PinnedRuntimeBrokerID = a.RuntimeBrokerID
		return a
	}

	run := func(t *testing.T, readErr error, states []state) {
		f := newRenewalFixture(t)
		stale, mismatch, consistent := renewalPlacementAgents(t, f, "parity-flat")
		override := installRenewalAgentOverride(t, f)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		if readErr != nil {
			_, fault := installStoreFault(t, f.srv, brokerReadFaultWrap(readErr))
			fault.Arm()
		}
		ctx := context.Background()
		for _, st := range states {
			a := st.agent(f, stale, mismatch, consistent)
			_, _, mintErr := f.srv.mintConduitGrant(ctx, conduitGrantRequest{Identity: f.user, Agent: &a, Stream: tcpHeader("3000"), Target: f.regTarget})
			override.set(&a)
			f.clock.Advance(time.Minute)
			r, renewErr := f.renew(tok, open, 7)
			openDecision, renewDecision := placementDecision(mintErr, readErr), placementDecision(renewErr, readErr)
			assert.Equal(t, openDecision, renewDecision, "%s: open and renewal decide differently", st.name)
			assert.Equal(t, st.want, openDecision, "%s: open", st.name)
			assert.Equal(t, st.want, renewDecision, "%s: renewal", st.name)
			if renewDecision != "allowed" {
				assert.Nil(t, r, "%s: a renewal was minted", st.name)
			}
		}
	}

	t.Run("placement read succeeds", func(t *testing.T) {
		run(t, nil, []state{
			{"consistent pin", pinned, "allowed"},
			{"stale pin", func(_ *renewalFixture, stale, _, _ store.Agent) store.Agent { return stale }, "refused " + ErrCodeRuntimeTargetPinStale},
			{"target mismatch", func(_ *renewalFixture, _, mismatch, _ store.Agent) store.Agent { return mismatch }, "refused " + ErrCodeRuntimeTargetMismatch},
			{"unpinned on a flat Runtime Broker", unpinnedOnFlat, "refused " + ErrCodeRuntimeTargetPinStale},
			{"pinned, Runtime Broker row missing", pinnedMissingRow, "refused " + ErrCodeRuntimeBrokerMissing},
			{"unpinned", unpinned, "allowed"},
			{"managed", managed, "allowed"},
		})
	})
	t.Run("placement read fails", func(t *testing.T) {
		run(t, errors.New("injected broker read failure"), []state{
			{"pinned", pinned, "transient"},
			{"unpinned", unpinned, "allowed"},
			{"managed", managed, "allowed"},
		})
	})
}
