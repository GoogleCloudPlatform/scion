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

package hub

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// keyringTestClock is a settable clock for the grant key cache.
type keyringTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *keyringTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *keyringTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// delayedGrantKeyStore counts ring reads and, while armed, holds the next
// read until release is closed, closing entered when that read begins.
type delayedGrantKeyStore struct {
	inner conduitGrantKeyStore
	reads atomic.Int64

	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (d *delayedGrantKeyStore) arm() (entered, release chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed, d.entered, d.release = true, make(chan struct{}), make(chan struct{})
	return d.entered, d.release
}

func (d *delayedGrantKeyStore) Load(ctx context.Context) (*grant.KeyRing, int, error) {
	d.reads.Add(1)
	d.mu.Lock()
	armed, entered, release := d.armed, d.entered, d.release
	d.armed = false
	d.mu.Unlock()
	if armed {
		close(entered)
		<-release
	}
	return d.inner.Load(ctx)
}

func (d *delayedGrantKeyStore) Create(ctx context.Context, ring *grant.KeyRing) error {
	return d.inner.Create(ctx, ring)
}

func (d *delayedGrantKeyStore) CompareAndSwap(ctx context.Context, ring *grant.KeyRing, rev int) (bool, error) {
	return d.inner.CompareAndSwap(ctx, ring, rev)
}

// newDelayedKeyring returns a key cache over a delayable store whose ring
// exists and is cached.
func newDelayedKeyring(t *testing.T) (*conduitGrantKeys, *delayedGrantKeyStore, *keyringTestClock) {
	t.Helper()
	clk := &keyringTestClock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	st := &delayedGrantKeyStore{inner: &memoryConduitGrantKeyStore{}}
	k := newConduitGrantKeys(st, clk.Now)
	_, _, err := k.recheckKeysGen(context.Background())
	require.NoError(t, err, "the ring is created and cached on first use")
	return k, st, clk
}

// publicKeysInBackground starts a published-key read whose store read is
// held; it returns once that read has begun, and the returned func lets it
// finish and waits for it.
func publicKeysInBackground(t *testing.T, k *conduitGrantKeys, st *delayedGrantKeyStore) (finish func()) {
	t.Helper()
	entered, release := st.arm()
	done := make(chan error, 1)
	go func() {
		_, err := k.freshPublicKeys(context.Background())
		done <- err
	}()
	<-entered
	return func() {
		close(release)
		require.NoError(t, <-done)
	}
}

// TestConduitGrantKeys_UnknownKidReloadOrdering pins when an unknown-kid
// reload reads the store again, by the order in which ring reads begin
// relative to the renewal's key snapshot.
func TestConduitGrantKeys_UnknownKidReloadOrdering(t *testing.T) {
	ctx := context.Background()

	t.Run("an unchanged cache reloads", func(t *testing.T) {
		k, st, _ := newDelayedKeyring(t)
		_, seen, err := k.recheckKeysGen(ctx)
		require.NoError(t, err)
		before := st.reads.Load()
		_, err = k.reloadRecheckKeys(ctx, seen)
		require.NoError(t, err)
		assert.Equal(t, before+1, st.reads.Load())
	})

	t.Run("a read begun after the snapshot stands in for the reload", func(t *testing.T) {
		k, st, _ := newDelayedKeyring(t)
		_, seen, err := k.recheckKeysGen(ctx)
		require.NoError(t, err)
		_, err = k.freshPublicKeys(ctx)
		require.NoError(t, err)
		before := st.reads.Load()
		_, err = k.reloadRecheckKeys(ctx, seen)
		require.NoError(t, err)
		assert.Equal(t, before, st.reads.Load(), "the newer ring is used without another read")
	})

	t.Run("a read begun before the snapshot and installed after it does not", func(t *testing.T) {
		k, st, _ := newDelayedKeyring(t)
		finish := publicKeysInBackground(t, k, st)
		_, seen, err := k.recheckKeysGen(ctx)
		require.NoError(t, err)
		finish()
		before := st.reads.Load()
		_, err = k.reloadRecheckKeys(ctx, seen)
		require.NoError(t, err)
		assert.Equal(t, before+1, st.reads.Load(), "the slow read's ring is read again")
	})

	t.Run("the snapshot comes before the re-check's own stale load", func(t *testing.T) {
		k, st, clk := newDelayedKeyring(t)
		clk.Advance(conduitGrantKeyRecheckFresh)
		before := st.reads.Load()
		_, seen, err := k.recheckKeysGen(ctx)
		require.NoError(t, err)
		require.Equal(t, before+1, st.reads.Load(), "a stale cache is read again by the re-check")
		_, err = k.reloadRecheckKeys(ctx, seen)
		require.NoError(t, err)
		assert.Equal(t, before+1, st.reads.Load(), "that fresh read counts as the reload")
	})

	t.Run("concurrent renewals share one reload", func(t *testing.T) {
		k, st, _ := newDelayedKeyring(t)
		const n = 16
		seen := make([]uint64, n)
		for i := range n {
			_, s, err := k.recheckKeysGen(ctx)
			require.NoError(t, err)
			seen[i] = s
		}
		before := st.reads.Load()
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = k.reloadRecheckKeys(ctx, seen[i])
			}()
		}
		wg.Wait()
		for i, err := range errs {
			assert.NoError(t, err, "renewal %d", i)
		}
		assert.Equal(t, before+1, st.reads.Load(), "one reload for all renewals")
	})
}

// TestConduitGrantKeys_FreshnessCountsFromReadStart: the cached ring's age
// is measured from when its read began, so a slow read does not extend how
// long the cache is served.
func TestConduitGrantKeys_FreshnessCountsFromReadStart(t *testing.T) {
	k, st, clk := newDelayedKeyring(t)
	ctx := context.Background()

	start := clk.Now()
	finish := publicKeysInBackground(t, k, st)
	clk.Advance(conduitGrantKeyRecheckFresh)
	finish()

	k.mu.Lock()
	loadedAt := k.loadedAt
	k.mu.Unlock()
	assert.True(t, loadedAt.Equal(start), "loadedAt is the read's start, %s, not %s", start, loadedAt)

	before := st.reads.Load()
	_, _, err := k.recheckKeysGen(ctx)
	require.NoError(t, err)
	assert.Equal(t, before+1, st.reads.Load(), "a ring as old as the re-check freshness bound is read again")
}
