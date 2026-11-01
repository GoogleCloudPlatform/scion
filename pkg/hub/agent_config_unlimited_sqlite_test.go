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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnlimitedCountRefusal: a change of a count limit to 0 is refused for
// an agent whose broker lacks unlimitedCountLimits, or that has no broker,
// and accepted for a broker that has it; an edit that sets no count limit
// to 0 needs no broker lookup at all (ptone/scion#5605).
func TestUnlimitedCountRefusal(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	_, fault := installBrokerReadFault(t, srv)
	ctx := context.Background()
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	base := &api.ScionConfig{MaxTurns: api.IntPtr(5)}
	unlimited := &api.ScionConfig{MaxTurns: api.IntPtr(0)}

	got, err := srv.unlimitedCountRefusal(ctx, agent, unlimited, base)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"config.max_turns": unlimitedNotSupportedByBroker}, got)

	got, err = srv.unlimitedCountRefusal(ctx, agent, unlimited, &api.ScionConfig{MaxTurns: api.IntPtr(0)})
	require.NoError(t, err)
	assert.Nil(t, got, "an echo of a stored Unlimited is not refused")

	b, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
	got, err = srv.unlimitedCountRefusal(ctx, agent, unlimited, base)
	require.NoError(t, err)
	assert.Nil(t, got, "a broker with unlimitedCountLimits applies Unlimited")

	noBroker := *agent
	noBroker.RuntimeBrokerID = ""
	got, err = srv.unlimitedCountRefusal(ctx, &noBroker, unlimited, base)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"config.max_turns": unlimitedBrokerUnknown}, got)

	// A broker record that no longer exists is not retryable: broker_unknown.
	missing := *agent
	missing.RuntimeBrokerID = "no-such-broker"
	got, err = srv.unlimitedCountRefusal(ctx, &missing, unlimited, base)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"config.max_turns": unlimitedBrokerUnknown}, got)

	// A failing broker read is a 503 (errBrokerLookup), and an edit that
	// sets no count limit to 0 never looks the broker up.
	fault.Arm()
	_, err = srv.unlimitedCountRefusal(ctx, agent, unlimited, base)
	assert.ErrorIs(t, err, errBrokerLookup)
	got, err = srv.unlimitedCountRefusal(ctx, agent, &api.ScionConfig{Model: "m"}, base)
	require.NoError(t, err, "an edit that sets no count limit to 0 never looks the broker up")
	assert.Nil(t, got)
}

// brokerReadSwitchStore makes runtime broker reads fail with a store error
// (not "not found") once its switch is armed; before that it delegates.
type brokerReadSwitchStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (b *brokerReadSwitchStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if !b.fault.Active() {
		return b.Store.GetRuntimeBroker(ctx, id)
	}
	return nil, errors.New("store unavailable")
}

// installBrokerReadFault installs brokerReadSwitchStore on srv through the
// store fault switch (no srv.store reassignment in the test).
func installBrokerReadFault(t *testing.T, srv *Server) (*brokerReadSwitchStore, *storeFaultSwitch) {
	t.Helper()
	return installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *brokerReadSwitchStore {
		return &brokerReadSwitchStore{Store: inner, fault: f}
	})
}

// TestWarnUnlimitedUnsupported_DispatchWarning: a create or start that
// dispatches an inline count limit of 0 to a broker without
// unlimitedCountLimits warns that the agent keeps the inherited limit; a
// capable broker, or no Unlimited, gives no warning.
func TestWarnUnlimitedUnsupported_DispatchWarning(t *testing.T) {
	_, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	d := &HTTPAgentDispatcher{store: s}
	inline := &api.ScionConfig{MaxTurns: api.IntPtr(0)}

	ctx, warns := withDispatchWarnings(context.Background())
	d.warnUnlimitedUnsupported(ctx, agent, inline)
	require.Len(t, warns.Warnings(), 1)
	assert.Contains(t, warns.Warnings()[0], "config.max_turns: this agent's runtime broker keeps the inherited limit")

	ctx, warns = withDispatchWarnings(context.Background())
	d.warnUnlimitedUnsupported(ctx, agent, &api.ScionConfig{MaxTurns: api.IntPtr(4)})
	assert.Empty(t, warns.Warnings())

	b, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true}
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), b))
	ctx, warns = withDispatchWarnings(context.Background())
	d.warnUnlimitedUnsupported(ctx, agent, inline)
	assert.Empty(t, warns.Warnings())
}
