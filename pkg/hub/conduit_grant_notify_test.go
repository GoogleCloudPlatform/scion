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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
)

// Grant key changes across hub nodes (design v2.21.27 §3.5, Key-set
// freshness; criterion 29).

// grantKeyNodes is two hub nodes sharing one stored ring and one event bus.
type grantKeyNodes struct {
	a, b  *Server
	clock *conduitTestClock
	bus   *ChannelEventPublisher
}

// newGrantKeyNodes starts the stream re-check (and with it the grant key
// change binding) on node a, and on node b when bindB is true.
func newGrantKeyNodes(t *testing.T, bindB bool) *grantKeyNodes {
	t.Helper()
	n := &grantKeyNodes{clock: &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}, bus: NewChannelEventPublisher()}
	mem := &memoryConduitGrantKeyStore{}
	n.a, _ = testServer(t)
	n.b, _ = testServer(t)
	for i, srv := range []*Server{n.a, n.b} {
		srv.conduitGrants = newConduitGrantKeys(mem, n.clock.Now)
		setConduitExperiment(t, srv, true)
		srv.SetEventPublisher(n.bus)
		if i == 1 && !bindB {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		_, stop, err := srv.startConduitStreamAuthz(ctx, clock.NewFake(time.Now()), -1)
		require.NoError(t, err)
		t.Cleanup(stop)
	}
	return n
}

func publishedKIDs(t *testing.T, srv *Server) map[string]bool {
	t.Helper()
	pubs, err := srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	out := map[string]bool{}
	for _, pk := range pubs {
		out[pk.KeyID] = true
	}
	return out
}

func recheckHas(t *testing.T, srv *Server, kid string) bool {
	t.Helper()
	keys, err := srv.conduitGrants.recheckKeys(context.Background())
	require.NoError(t, err)
	_, ok := keys.Lookup(kid)
	return ok
}

// TestConduitGrantKeyChangeReachesOtherNode: a rotation and a revocation
// stored by node a apply on node b at once, before b's periodic ring reload
// would pick them up; the clock does not move.
func TestConduitGrantKeyChangeReachesOtherNode(t *testing.T) {
	n := newGrantKeyNodes(t, true)
	ctx := context.Background()
	first, err := n.a.conduitGrants.signer(ctx)
	require.NoError(t, err)
	require.True(t, publishedKIDs(t, n.b)[first.KeyID], "b caches the ring")

	rot, err := n.a.RotateConduitGrantKey(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return publishedKIDs(t, n.b)[rot.KeyID] }, 5*time.Second, 10*time.Millisecond,
		"b publishes the rotated-in key without waiting for its reload")

	n.clock.Advance(rot.ActivateAt.Sub(n.clock.Now()) + conduitGrantKeyRefresh)
	require.True(t, recheckHas(t, n.b, first.KeyID), "b holds the outgoing key")
	require.NoError(t, n.a.RevokeConduitGrantKey(ctx, first.KeyID))
	require.Eventually(t, func() bool { return !recheckHas(t, n.b, first.KeyID) && !publishedKIDs(t, n.b)[first.KeyID] },
		5*time.Second, 10*time.Millisecond, "b stops using the revoked key without waiting for its reload")
}

// TestConduitGrantKeyChangeMissedEventBoundedByReload: a node that misses
// the change event still applies it at its next ring reload, within
// conduitGrantKeyRefresh.
func TestConduitGrantKeyChangeMissedEventBoundedByReload(t *testing.T) {
	n := newGrantKeyNodes(t, false)
	ctx := context.Background()
	_, err := n.a.conduitGrants.signer(ctx)
	require.NoError(t, err)
	before := publishedKIDs(t, n.b)

	rot, err := n.a.RotateConduitGrantKey(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, publishedKIDs(t, n.b), "b, not subscribed, still serves its cached ring")
	n.clock.Advance(conduitGrantKeyRefresh)
	assert.True(t, publishedKIDs(t, n.b)[rot.KeyID], "b applies the change at its reload")
}

// TestConduitGrantKeyChangeEventNamesOnlyTheKey: the event carries the key
// id and nothing else, and is published only after the change is stored
// (not for a refused revocation) and only while hub.conduit is on.
func TestConduitGrantKeyChangeEventNamesOnlyTheKey(t *testing.T) {
	n := newGrantKeyNodes(t, false)
	ctx := context.Background()
	ch, unsubscribe := n.bus.Subscribe(conduitGrantKeysChangedSubject)
	defer unsubscribe()
	first, err := n.a.conduitGrants.signer(ctx)
	require.NoError(t, err)

	assert.ErrorIs(t, n.a.RevokeConduitGrantKey(ctx, first.KeyID), errConduitGrantKeyRevokeActive)
	rot, err := n.a.RotateConduitGrantKey(ctx)
	require.NoError(t, err)
	select {
	case evt := <-ch:
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(evt.Data, &raw))
		assert.ElementsMatch(t, []string{"kid"}, jsonFieldNames(raw))
		var got conduitGrantKeysChanged
		require.NoError(t, json.Unmarshal(evt.Data, &got))
		assert.Equal(t, rot.KeyID, got.KeyID, "the first event is the rotation's, not the refused revocation's")
	case <-time.After(5 * time.Second):
		t.Fatal("no change event")
	}

	setConduitExperiment(t, n.a, false)
	n.a.publishConduitGrantKeysChanged("cg-any")
	select {
	case evt := <-ch:
		t.Fatalf("event published with hub.conduit off: %s", evt.Data)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestConduitGrantKeyChangeHintsHubHeldTargets: after a rotation, the hub
// sends GrantKeysChanged on the stream_renewal.v2 agent session it holds,
// and the agent fetches and installs the rotated-in key without waiting
// for its periodic refresh (whose timers do not fire in this test).
func TestConduitGrantKeyChangeHintsHubHeldTargets(t *testing.T) {
	f := newConduitProxyFixture(t, http.NotFoundHandler())
	f.srv.SetEventPublisher(NewChannelEventPublisher())
	f.startAgent(t)
	ctx := context.Background()
	first, err := f.srv.conduitGrants.signer(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := f.sciontool.Keys().Lookup(first.KeyID); return ok },
		10*time.Second, 10*time.Millisecond, "the agent's first key fetch")

	// Past the agent's spacing between fetches, so the hint is served at
	// once.
	f.clock.Advance(sconduit.GrantKeysHintInterval + time.Second)
	rot, err := f.srv.RotateConduitGrantKey(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := f.sciontool.Keys().Lookup(rot.KeyID); return ok },
		10*time.Second, 10*time.Millisecond, "the agent installs the rotated-in key after the hint")
}

// revokedOnA rotates and activates a new key and caches the ring on b, then
// revokes the outgoing key on a; b receives no event. It returns the
// revoked kid.
func revokedOnA(t *testing.T, n *grantKeyNodes) string {
	t.Helper()
	ctx := context.Background()
	first, err := n.a.conduitGrants.signer(ctx)
	require.NoError(t, err)
	rot, err := n.a.RotateConduitGrantKey(ctx)
	require.NoError(t, err)
	n.clock.Advance(rot.ActivateAt.Sub(n.clock.Now()) + conduitGrantKeyRefresh)
	require.True(t, publishedKIDs(t, n.b)[first.KeyID], "b caches a ring holding the outgoing key")
	require.True(t, recheckHas(t, n.b, first.KeyID))
	require.NoError(t, n.a.RevokeConduitGrantKey(ctx, first.KeyID))
	return first.KeyID
}

// TestConduitGrantKeysHubDirectReadIsFresh: a target's key fetch served by
// a node that has not received the change event, and still caches the old
// ring, reflects a revocation stored by another node at once, through both
// hub-direct paths (the grant-key route and the token-refresh response).
func TestConduitGrantKeysHubDirectReadIsFresh(t *testing.T) {
	n := newGrantKeyNodes(t, false) // b is not subscribed
	kid := revokedOnA(t, n)
	assert.True(t, publishedKIDs(t, n.b)[kid], "b's cache still holds the revoked kid")
	keys, err := n.b.conduitHubDirectGrantKeys(context.Background())
	require.NoError(t, err)
	for _, k := range keys {
		assert.NotEqual(t, kid, k.KeyID, "the grant-key route on b serves the revoked kid")
	}
	for _, k := range n.b.conduitRefreshGrantKeys(context.Background()) {
		assert.NotEqual(t, kid, k.KeyID, "the token-refresh response on b carries the revoked kid")
	}
}

// TestConduitGrantRecheckKeysFreshWithinSeconds: on a node that has not
// received the change event, the re-check key set refuses a kid revoked by
// another node once its cached ring is conduitGrantKeyRecheckFresh old.
func TestConduitGrantRecheckKeysFreshWithinSeconds(t *testing.T) {
	n := newGrantKeyNodes(t, false)
	kid := revokedOnA(t, n)
	assert.True(t, recheckHas(t, n.b, kid), "within conduitGrantKeyRecheckFresh the cached ring is used")
	n.clock.Advance(conduitGrantKeyRecheckFresh)
	assert.False(t, recheckHas(t, n.b, kid), "b's re-check set still accepts the revoked kid")
}

// listenBus is an in-process event bus with a LISTEN (re)connect hook, as
// PostgresEventPublisher has.
type listenBus struct {
	*ChannelEventPublisher
	mu  sync.Mutex
	fns map[int]func()
	seq int
}

func (b *listenBus) AddOnListen(fn func()) (remove func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fns == nil {
		b.fns = map[int]func(){}
	}
	b.seq++
	id := b.seq
	b.fns[id] = fn
	return func() {
		b.mu.Lock()
		delete(b.fns, id)
		b.mu.Unlock()
	}
}

// reconnect runs every registered hook, as a LISTEN reconnect does.
func (b *listenBus) reconnect() {
	b.mu.Lock()
	fns := make([]func(), 0, len(b.fns))
	for _, fn := range b.fns {
		fns = append(fns, fn)
	}
	b.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// TestConduitGrantKeyCacheDroppedOnListenReconnect: a node whose LISTEN
// connection (re)connects drops its cached ring, so a change published while
// it was not listening applies at once; the clock does not move.
func TestConduitGrantKeyCacheDroppedOnListenReconnect(t *testing.T) {
	clk := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	mem := &memoryConduitGrantKeyStore{}
	a, _ := testServer(t)
	b, _ := testServer(t)
	busB := &listenBus{ChannelEventPublisher: NewChannelEventPublisher()}
	for srv, bus := range map[*Server]EventPublisher{a: NewChannelEventPublisher(), b: busB} {
		srv.conduitGrants = newConduitGrantKeys(mem, clk.Now)
		setConduitExperiment(t, srv, true)
		srv.SetEventPublisher(bus)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, stop, err := b.startConduitStreamAuthz(ctx, clock.NewFake(time.Now()), -1)
	require.NoError(t, err)
	t.Cleanup(stop)

	_, err = a.conduitGrants.signer(ctx)
	require.NoError(t, err)
	before := publishedKIDs(t, b)
	rot, err := a.RotateConduitGrantKey(ctx) // published on a's bus only
	require.NoError(t, err)
	require.Equal(t, before, publishedKIDs(t, b), "b, missing the event, serves its cached ring")
	busB.reconnect()
	assert.True(t, publishedKIDs(t, b)[rot.KeyID], "b did not drop its cached ring on LISTEN reconnect")
}
