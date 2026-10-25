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
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Atomic admission for the per-project agent limit (ptone/scion#6033): the
// project-wide count and the insert run under the per-scope locks of
// QuotaService.Reserve, so two admissions with one slot left can never both
// pass, whichever creators they are for.

// projectCountHookStore wraps a store for a QuotaService built by the test.
// While its hooks are set it runs onCount inside the project limit's
// grouped count (the count Reserve makes under its locks) and onLookup when
// the project limit definition is read with a marked context. It tracks
// how many advisory locks it handed out and are not yet released, and
// forwards the advisory lock methods, which store.Store does not include.
type projectCountHookStore struct {
	store.Store
	defID      string
	onCount    func()
	onLookup   func()
	locksHeld  atomic.Int32
	countCalls atomic.Int32
}

type markedLookupKey struct{}

func (w *projectCountHookStore) CountActiveReservationsByScope(ctx context.Context, limitDefID, scopeType string) (map[string]int64, error) {
	if limitDefID == w.defID && scopeType == store.QuotaScopeProject {
		w.countCalls.Add(1)
		if w.onCount != nil {
			w.onCount()
		}
	}
	return w.Store.CountActiveReservationsByScope(ctx, limitDefID, scopeType)
}

func (w *projectCountHookStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	if name == store.LimitMaxAgentsPerProject && ctx.Value(markedLookupKey{}) != nil && w.onLookup != nil {
		w.onLookup()
	}
	return w.Store.GetLimitDefinitionByName(ctx, name)
}

func (w *projectCountHookStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return w.Store.(store.AdvisoryLocker).TryAdvisoryLock(ctx, key)
}

func (w *projectCountHookStore) TryAdvisoryLockObject(ctx context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	acquired, release, err := w.Store.(store.AdvisoryLocker).TryAdvisoryLockObject(ctx, classID, objID)
	if !acquired || err != nil {
		return acquired, release, err
	}
	w.locksHeld.Add(1)
	var once sync.Once
	return true, func() error {
		once.Do(func() { w.locksHeld.Add(-1) })
		return release()
	}, nil
}

// newProjectQuotaTestService returns a QuotaService over a hook store on s,
// with the project limit set to cap.
func newProjectQuotaTestService(t *testing.T, s store.Store, cap int64) (*QuotaService, *projectCountHookStore) {
	t.Helper()
	setProjectAgentCeiling(t, s, cap)
	w := &projectCountHookStore{Store: s, defID: projectLimitDefID(t, s)}
	return &QuotaService{store: w, logger: slog.Default()}, w
}

// T-race-lock: Reserve's project-wide count runs while the in-process scope
// mutex and the advisory lock are held.
func TestProjectQuota_ReserveCountsUnderTheScopeLocks(t *testing.T) {
	_, s := testServer(t)
	qs, w := newProjectQuotaTestService(t, s, 1)
	projectID := tid("proj-race-lock")
	var checked bool
	w.onCount = func() {
		checked = true
		m := qs.lockForScope(store.LimitMaxAgentsPerProject, store.QuotaScopeProject, projectID)
		if m.TryLock() {
			m.Unlock()
			t.Error("the scope mutex is not held while the project total is counted")
		}
		assert.EqualValues(t, 1, w.locksHeld.Load(), "the advisory lock is held while the project total is counted")
	}

	created, err := qs.Reserve(context.Background(), store.LimitMaxAgentsPerProject, tid("user-a"), store.QuotaScopeProject, projectID, tid("agent-race-lock"))
	require.NoError(t, err)
	assert.True(t, created)
	assert.True(t, checked, "the hook ran inside the count")
	assert.Zero(t, w.locksHeld.Load(), "the advisory lock is released afterwards")
}

// T-race-admit: two admissions with one slot left, for different creators
// (the project total is shared) and for one creator. The first is held inside its count
// until the second has entered Reserve; exactly one is admitted, the other
// is refused at the project total, and one row exists.
func TestProjectQuota_ConcurrentAdmissionsOneSlot(t *testing.T) {
	for _, sameCreator := range []bool{false, true} {
		name := "different-creators"
		if sameCreator {
			name = "same-creator"
		}
		t.Run(name, func(t *testing.T) {
			_, s := testServer(t)
			qs, w := newProjectQuotaTestService(t, s, 1)
			projectID := tid("proj-race-admit-" + name)
			secondCreator := tid("user-b")
			if sameCreator {
				secondCreator = tid("user-a")
			}
			secondEntered := make(chan struct{})
			var enteredOnce, countOnce sync.Once
			w.onLookup = func() { enteredOnce.Do(func() { close(secondEntered) }) }
			w.onCount = func() { countOnce.Do(func() { <-secondEntered }) }

			type result struct {
				created bool
				err     error
			}
			first := make(chan result, 1)
			go func() {
				c, err := qs.Reserve(context.Background(), store.LimitMaxAgentsPerProject, tid("user-a"), store.QuotaScopeProject, projectID, tid("agent-race-1-"+name))
				first <- result{c, err}
			}()
			for w.countCalls.Load() == 0 {
				// The first admission is now inside its count, waiting.
				runtime.Gosched()
			}
			marked := context.WithValue(context.Background(), markedLookupKey{}, true)
			c2, err2 := qs.Reserve(marked, store.LimitMaxAgentsPerProject, secondCreator, store.QuotaScopeProject, projectID, tid("agent-race-2-"+name))
			r1 := <-first

			require.NoError(t, r1.err)
			assert.True(t, r1.created, "the first admission is admitted")
			var exceeded *QuotaExceededError
			require.True(t, errors.As(err2, &exceeded), "the second is refused at the project total: %v", err2)
			assert.False(t, c2)
			assert.EqualValues(t, 1, exceeded.Count)
			assert.EqualValues(t, 1, exceeded.Max)
			assert.EqualValues(t, 1, projectRowCount(t, s, projectID), "exactly one row")
		})
	}
}
