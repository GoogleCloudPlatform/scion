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
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hdHoldFaultStore fails HasActiveAgentHold, for every agent or only for
// holdErrID when it is set.
type hdHoldFaultStore struct {
	store.Store
	holdErr   error
	holdErrID string
}

func (s *hdHoldFaultStore) HasActiveAgentHold(ctx context.Context, agentID string) (bool, error) {
	if s.holdErr != nil && (s.holdErrID == "" || s.holdErrID == agentID) {
		return false, s.holdErr
	}
	return s.Store.HasActiveAgentHold(ctx, agentID)
}

// TestHubDelivery_HeldAgentOnChainDenied pins that the admission walk
// rejects a held agent at any position of the chain, as agentStanding and
// checkAgentHoldsPermission do, and that a hold lookup error fails closed.
func TestHubDelivery_HeldAgentOnChainDenied(t *testing.T) {
	twoHop := func(t *testing.T, f *goldenFixture, name string) (a, c string) {
		a, c = tid("md-held-"+name+"-a"), tid("md-held-"+name+"-c")
		hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.projectOwnerID})
		hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID, a})
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, a, f.projectAlpha.ID, nil)
		hdEdge(t, f.store, store.DelegationPrincipalAgent, a, c, f.projectAlpha.ID, withBoundedCeiling("secret.deliver"))
		return a, c
	}
	res := func(f *goldenFixture) Resource { return Resource{Type: "secret", ID: f.secretID} }

	t.Run("control_not_held", func(t *testing.T) {
		f := newGoldenFixture(t)
		_, c := twoHop(t, f, "control")
		r := hdDirect(t, f.authz, hdIdentity(t, f.authz, c), res(f), "secret.deliver")
		assert.True(t, r.allowed, r.reason)
	})
	t.Run("target_held", func(t *testing.T) {
		f := newGoldenFixture(t)
		_, c := twoHop(t, f, "target")
		h := hdIdentity(t, f.authz, c)
		holdAgentFor(t, f.store, c, f.projectAlpha.ID, f.projectOwnerID)
		r := hdDirect(t, f.authz, h, res(f), "secret.deliver")
		assert.False(t, r.allowed)
		assert.Equal(t, "agent "+c+" is held", r.reason)
		assert.Equal(t, DenyCauseCeilingOrphaned, r.cause)
	})
	t.Run("delegator_agent_held", func(t *testing.T) {
		f := newGoldenFixture(t)
		a, c := twoHop(t, f, "delegator")
		h := hdIdentity(t, f.authz, c)
		holdAgentFor(t, f.store, a, f.projectAlpha.ID, f.projectOwnerID)
		r := hdDirect(t, f.authz, h, res(f), "secret.deliver")
		assert.False(t, r.allowed)
		assert.Equal(t, "delegator agent "+a+" is held", r.reason)
		assert.Equal(t, DenyCauseCeilingOrphaned, r.cause)
	})
	t.Run("deeper_agent_held", func(t *testing.T) {
		// U -> A -> B -> C: A, two hops above the target, is held.
		f := newGoldenFixture(t)
		a, b, c := tid("md-held-deep-a"), tid("md-held-deep-b"), tid("md-held-deep-c")
		hdAgent(t, f.store, a, f.projectAlpha.ID, []string{f.projectOwnerID})
		hdAgent(t, f.store, b, f.projectAlpha.ID, []string{f.projectOwnerID, a})
		hdAgent(t, f.store, c, f.projectAlpha.ID, []string{f.projectOwnerID, a, b})
		hdEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, a, f.projectAlpha.ID, nil)
		hdEdge(t, f.store, store.DelegationPrincipalAgent, a, b, f.projectAlpha.ID, withBoundedCeiling("secret.deliver"))
		hdEdge(t, f.store, store.DelegationPrincipalAgent, b, c, f.projectAlpha.ID, withBoundedCeiling("secret.deliver"))
		h := hdIdentity(t, f.authz, c)
		ctl := hdDirect(t, f.authz, h, res(f), "secret.deliver")
		require.True(t, ctl.allowed, "control: %s", ctl.reason)
		holdAgentFor(t, f.store, a, f.projectAlpha.ID, f.projectOwnerID)
		r := hdDirect(t, f.authz, h, res(f), "secret.deliver")
		assert.False(t, r.allowed)
		assert.Equal(t, DenyCauseCeilingOrphaned, r.cause, r.reason)
		assert.Contains(t, r.reason, a)
	})
	t.Run("hold_lookup_error_target", func(t *testing.T) {
		f := newGoldenFixture(t)
		_, c := twoHop(t, f, "err-target")
		h := hdIdentity(t, f.authz, c)
		a := NewAuthzService(&hdHoldFaultStore{Store: f.store, holdErr: errors.New("injected hold failure"), holdErrID: c}, slog.Default())
		r := hdDirect(t, a, h, res(f), "secret.deliver")
		assert.False(t, r.allowed)
		assert.Equal(t, DenyCauseCeilingError, r.cause)
		assert.True(t, strings.HasPrefix(r.reason, "delivery ceiling check failed (fail-closed): "), r.reason)
	})
	t.Run("hold_lookup_error_delegator", func(t *testing.T) {
		f := newGoldenFixture(t)
		ag, c := twoHop(t, f, "err-delegator")
		h := hdIdentity(t, f.authz, c)
		a := NewAuthzService(&hdHoldFaultStore{Store: f.store, holdErr: errors.New("injected hold failure"), holdErrID: ag}, slog.Default())
		r := hdDirect(t, a, h, res(f), "secret.deliver")
		assert.False(t, r.allowed)
		assert.Equal(t, DenyCauseCeilingError, r.cause)
	})
}
