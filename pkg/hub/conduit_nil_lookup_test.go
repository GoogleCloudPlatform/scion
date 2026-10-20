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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A store that answers a lookup with neither a record nor an error is an
// internal inconsistency. Renewal and the grant key ring fail closed on it:
// unavailable (renewal) or unreadable (ring), never a decision.

// renewNilAgentStore answers GetAgent for one agent with (nil, nil).
type renewNilAgentStore struct {
	store.Store
	id string
}

func (s *renewNilAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.id {
		return nil, nil
	}
	return s.Store.GetAgent(ctx, id)
}

// renewNilUserStore answers GetUser for one user with (nil, nil).
type renewNilUserStore struct {
	store.Store
	id string
}

func (s *renewNilUserStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.id {
		return nil, nil
	}
	return s.Store.GetUser(ctx, id)
}

// TestConduitRenewStream_NilAgentIsUnavailable: an agent lookup that
// returns no agent and no error mints no renewal and is unavailable, not a
// gone agent (which closes the stream) and not a denial.
func TestConduitRenewStream_NilAgentIsUnavailable(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *renewNilAgentStore {
		return &renewNilAgentStore{Store: inner, id: f.agent.ID}
	})

	f.clock.Advance(time.Minute)
	r, err := f.renew(tok, open, 7)
	assert.Nil(t, r, "a renewal was minted without an agent")
	require.ErrorIs(t, err, errConduitAuthzUnavailable)
	assert.NotErrorIs(t, err, errConduitAgentGone)
	assert.NotErrorIs(t, err, errConduitForbidden)
	assert.NotErrorIs(t, err, errConduitPrincipalInactive)
}

// TestConduitRenewStream_NilUserIsUnavailable: a user lookup that returns
// no user and no error mints no renewal and is unavailable, not an inactive
// user (which closes the stream) and not a denial.
func TestConduitRenewStream_NilUserIsUnavailable(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *renewNilUserStore {
		return &renewNilUserStore{Store: inner, id: f.user.ID()}
	})

	f.clock.Advance(time.Minute)
	r, err := f.renew(tok, open, 7)
	assert.Nil(t, r, "a renewal was minted without a user")
	require.ErrorIs(t, err, errConduitAuthzUnavailable)
	assert.NotErrorIs(t, err, errConduitPrincipalInactive)
	assert.NotErrorIs(t, err, errConduitForbidden)
	assert.NotErrorIs(t, err, errConduitAgentGone)
}

// nilRingGrantKeyStore loads a ring correctly for the first okLoads loads
// and then answers Load with (nil, 0, nil).
type nilRingGrantKeyStore struct {
	ring    *grant.KeyRing
	okLoads int32
	loads   atomic.Int32
}

func (s *nilRingGrantKeyStore) Load(context.Context) (*grant.KeyRing, int, error) {
	if s.loads.Add(1) <= s.okLoads {
		return cloneKeyRing(s.ring), 1, nil
	}
	return nil, 0, nil
}
func (s *nilRingGrantKeyStore) Create(context.Context, *grant.KeyRing) error { return nil }
func (s *nilRingGrantKeyStore) CompareAndSwap(context.Context, *grant.KeyRing, int) (bool, error) {
	return true, nil
}

func nilRingTestRing(t *testing.T, now time.Time) *grant.KeyRing {
	t.Helper()
	key, err := grant.NewRingKey(now, now)
	require.NoError(t, err)
	return &grant.KeyRing{Keys: []grant.RingKey{key}}
}

// TestConduitGrantKeys_NilRingAfterLoadIsUnreadable: a ring load that
// returns no ring and no error makes the key ring unreadable, so no grant is
// signed, instead of panicking or proceeding without keys.
func TestConduitGrantKeys_NilRingAfterLoadIsUnreadable(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	k := newConduitGrantKeys(&nilRingGrantKeyStore{}, func() time.Time { return now })
	_, err := k.signer(context.Background())
	require.ErrorIs(t, err, errConduitGrantRingUnreadable)
}

// TestConduitGrantKeys_RevokeNilRingAfterLoadIsUnreadable: when revoke
// rereads the ring and the store returns no ring and no error, revoke fails
// as unreadable and writes nothing.
func TestConduitGrantKeys_RevokeNilRingAfterLoadIsUnreadable(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	st := &nilRingGrantKeyStore{ring: nilRingTestRing(t, now), okLoads: 1}
	k := newConduitGrantKeys(st, func() time.Time { return now })
	err := k.revoke(context.Background(), "some-kid")
	require.ErrorIs(t, err, errConduitGrantRingUnreadable)
	assert.Equal(t, int32(2), st.loads.Load(), "the reread inside revoke was not the load that returned no ring")
}

// scriptedGrantKeyStore answers Load from loads in order (the last entry
// repeats), CompareAndSwap with casApplied, and Create with createErr.
type scriptedGrantKeyStore struct {
	loads      []func() (*grant.KeyRing, int, error)
	casApplied bool
	createErr  error
	n          atomic.Int32
}

func (s *scriptedGrantKeyStore) Load(context.Context) (*grant.KeyRing, int, error) {
	i := int(s.n.Add(1)) - 1
	if i >= len(s.loads) {
		i = len(s.loads) - 1
	}
	return s.loads[i]()
}
func (s *scriptedGrantKeyStore) Create(context.Context, *grant.KeyRing) error { return s.createErr }
func (s *scriptedGrantKeyStore) CompareAndSwap(context.Context, *grant.KeyRing, int) (bool, error) {
	return s.casApplied, nil
}

func noRingLoad() (*grant.KeyRing, int, error) { return nil, 0, nil }

// TestConduitGrantKeys_RereadNilRingAfterLostSwapIsUnreadable: when the
// settled ring loses its compare-and-swap and the reread returns no ring and
// no error, the ring is unreadable.
func TestConduitGrantKeys_RereadNilRingAfterLostSwapIsUnreadable(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ring := nilRingTestRing(t, now)
	// A retained key forces Settle to change the ring, so it is written back.
	ring.Retained = []grant.RetainedKey{{KeyID: "old", PublicKey: []byte("pub"), RetiredAt: now.Add(-48 * time.Hour), RetainUntil: now.Add(-time.Hour)}}
	st := &scriptedGrantKeyStore{loads: []func() (*grant.KeyRing, int, error){
		func() (*grant.KeyRing, int, error) { return cloneKeyRing(ring), 1, nil },
		noRingLoad,
	}}
	k := newConduitGrantKeys(st, func() time.Time { return now })
	_, err := k.loadSettled(context.Background())
	require.ErrorIs(t, err, errConduitGrantRingUnreadable)
	assert.Equal(t, int32(2), st.n.Load(), "the reread after the lost swap was not the load that returned no ring")
}

// TestConduitGrantKeys_BootstrapNilRingIsUnreadable: when another node won
// the bootstrap and loading its ring returns no ring and no error, the ring
// is unreadable and no grant is signed.
func TestConduitGrantKeys_BootstrapNilRingIsUnreadable(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	st := &scriptedGrantKeyStore{
		loads: []func() (*grant.KeyRing, int, error){
			func() (*grant.KeyRing, int, error) { return nil, 0, store.ErrNotFound },
			noRingLoad,
		},
		createErr: store.ErrAlreadyExists,
	}
	k := newConduitGrantKeys(st, func() time.Time { return now })
	_, err := k.signer(context.Background())
	require.ErrorIs(t, err, errConduitGrantRingUnreadable)
	assert.Equal(t, int32(2), st.n.Load(), "the load after the lost bootstrap was not the load that returned no ring")
}

// TestConduitGrantKeys_RotateNilRingIsUnreadable: when rotate rereads the
// ring and the store returns no ring and no error, rotate fails as
// unreadable and writes nothing.
func TestConduitGrantKeys_RotateNilRingIsUnreadable(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ring := nilRingTestRing(t, now)
	st := &scriptedGrantKeyStore{loads: []func() (*grant.KeyRing, int, error){
		func() (*grant.KeyRing, int, error) { return cloneKeyRing(ring), 1, nil },
		noRingLoad,
	}, casApplied: true}
	k := newConduitGrantKeys(st, func() time.Time { return now })
	_, err := k.rotate(context.Background(), conduitGrantKeyRefresh, time.Hour)
	require.ErrorIs(t, err, errConduitGrantRingUnreadable)
	assert.Equal(t, int32(2), st.n.Load(), "the reread inside rotate was not the load that returned no ring")
}
