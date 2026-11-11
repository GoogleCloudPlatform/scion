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
	"log/slog"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

type lockingStoreWrapper struct {
	store.Store
	mu    sync.Mutex
	locks map[lockKey]*sync.Mutex
}

func newLockingStoreWrapper(s store.Store) *lockingStoreWrapper {
	return &lockingStoreWrapper{
		Store: s,
		locks: make(map[lockKey]*sync.Mutex),
	}
}

func newTestQuotaService(t *testing.T) (*QuotaService, store.Store) {
	t.Helper()
	baseStore, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}

	wrapped := newLockingStoreWrapper(baseStore)

	qs := &QuotaService{
		store:  wrapped,
		logger: slog.Default().With("component", "quota-test"),
	}
	return qs, wrapped
}

// seedLimit creates a LimitDefinition in the store and returns it.
func seedLimit(t *testing.T, s store.Store, name string, defaultValue int64) *store.LimitDefinition {
	t.Helper()
	def, err := s.CreateLimitDefinition(context.Background(), &store.LimitDefinition{
		Name:         name,
		ResourceType: "test",
		Unit:         "count",
		Description:  "test limit: " + name,
		DefaultValue: defaultValue,
		System:       true,
	})
	require.NoError(t, err)
	return def
}

// seedBinding creates an EntitlementBinding in the store.
func seedBinding(t *testing.T, s store.Store, limitDefID, subjectType, subjectID, scopeType, scopeID string, value int64) {
	t.Helper()
	_, err := s.CreateEntitlementBinding(context.Background(), &store.EntitlementBinding{
		LimitDefinitionID: limitDefID,
		SubjectType:       subjectType,
		SubjectID:         subjectID,
		ScopeType:         scopeType,
		ScopeID:           scopeID,
		Value:             value,
		CreatedBy:         "test",
	})
	require.NoError(t, err)
}

func (w *lockingStoreWrapper) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return w.TryAdvisoryLockObject(ctx, key, 0)
}

func (w *lockingStoreWrapper) TryAdvisoryLockObject(_ context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	k := lockKey{classID, objID}

	w.mu.Lock()
	m, ok := w.locks[k]
	if !ok {
		m = &sync.Mutex{}
		w.locks[k] = m
	}
	w.mu.Unlock()

	if !m.TryLock() {
		return false, func() error { return nil }, nil
	}

	return true, func() error {
		m.Unlock()
		return nil
	}, nil
}

type lockKey struct {
	classID store.AdvisoryLockKey
	objID   int32
}
