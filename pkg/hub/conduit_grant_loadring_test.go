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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// loadRingResult is one scripted answer of loadRingScriptStore.Load.
type loadRingResult struct {
	ring *grant.KeyRing
	err  error
}

// loadRingScriptStore answers Load from a script (the last answer repeats),
// Create with createErr, and CompareAndSwap with success.
type loadRingScriptStore struct {
	mu        sync.Mutex
	loads     []loadRingResult
	createErr error
}

func (s *loadRingScriptStore) Load(context.Context) (*grant.KeyRing, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.loads[0]
	if len(s.loads) > 1 {
		s.loads = s.loads[1:]
	}
	if r.ring != nil {
		return cloneKeyRing(r.ring), 1, r.err
	}
	return nil, 0, r.err
}

func (s *loadRingScriptStore) Create(context.Context, *grant.KeyRing) error {
	return s.createErr
}

func (s *loadRingScriptStore) CompareAndSwap(context.Context, *grant.KeyRing, int) (bool, error) {
	return true, nil
}

// TestConduitGrantKeys_NoRingErrorsPerReadSite: at every ring read site, a
// store that returns neither a ring nor an error is an unreadable ring,
// with the same error text as before, and a store error passes through
// unchanged (a missing ring still bootstraps).
func TestConduitGrantKeys_NoRingErrorsPerReadSite(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	key, err := grant.NewRingKey(now, now)
	require.NoError(t, err)
	good := &grant.KeyRing{Keys: []grant.RingKey{key}}
	noRing := errConduitGrantRingUnreadable.Error() + ": the store returned no ring"
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		store *loadRingScriptStore
		call  func(k *conduitGrantKeys) error
		want  string
	}{
		{
			name:  "settled read",
			store: &loadRingScriptStore{loads: []loadRingResult{{}}},
			call:  func(k *conduitGrantKeys) error { _, err := k.loadSettled(ctx); return err },
			want:  noRing,
		},
		{
			name:  "bootstrap lost to another node",
			store: &loadRingScriptStore{loads: []loadRingResult{{err: store.ErrNotFound}, {}}, createErr: store.ErrAlreadyExists},
			call:  func(k *conduitGrantKeys) error { _, _, err := k.recheckKeysGen(ctx); return err },
			want:  "conduit grant key ring: " + noRing,
		},
		{
			name:  "rotate",
			store: &loadRingScriptStore{loads: []loadRingResult{{ring: good}, {}}},
			call: func(k *conduitGrantKeys) error {
				_, err := k.rotate(ctx, conduitGrantKeyRefresh, time.Hour)
				return err
			},
			want: "load conduit grant key ring: " + noRing,
		},
		{
			name:  "revoke",
			store: &loadRingScriptStore{loads: []loadRingResult{{ring: good}, {}}},
			call:  func(k *conduitGrantKeys) error { return k.revoke(ctx, "no-such-kid") },
			want:  "load conduit grant key ring: " + noRing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(newConduitGrantKeys(tc.store, clock))
			require.Error(t, err)
			assert.ErrorIs(t, err, errConduitGrantRingUnreadable)
			assert.Equal(t, tc.want, err.Error())
		})
	}

	t.Run("a store error passes through unchanged", func(t *testing.T) {
		boom := errors.New("injected ring read failure")
		k := newConduitGrantKeys(&loadRingScriptStore{loads: []loadRingResult{{err: boom}}}, clock)
		_, rev, err := k.loadRing(ctx)
		assert.Same(t, boom, err)
		assert.Zero(t, rev)
		_, _, err = k.loadRing(ctx)
		assert.NotErrorIs(t, err, errConduitGrantRingUnreadable)
	})

	t.Run("a missing ring still bootstraps", func(t *testing.T) {
		k := newConduitGrantKeys(&loadRingScriptStore{loads: []loadRingResult{{err: store.ErrNotFound}}}, clock)
		_, _, err := k.loadRing(ctx)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, _, err = k.recheckKeysGen(ctx)
		assert.NoError(t, err, "ErrNotFound reaches the bootstrap, which creates the ring")
	})

	t.Run("the revision passes through", func(t *testing.T) {
		k := newConduitGrantKeys(&loadRingScriptStore{loads: []loadRingResult{{ring: good}}}, clock)
		ring, rev, err := k.loadRing(ctx)
		require.NoError(t, err)
		require.NotNil(t, ring)
		assert.Equal(t, 1, rev)
	})
}
