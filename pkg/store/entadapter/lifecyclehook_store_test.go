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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLifecycleHookStore(t *testing.T) *LifecycleHookStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewLifecycleHookStore(client)
}

func sampleHook(id string) *store.LifecycleHook {
	return &store.LifecycleHook{
		ID:        id,
		Name:      "register-on-running",
		ScopeType: store.LifecycleHookScopeHub,
		Selector: &store.LifecycleHookSelector{
			Template: "registry-agent",
		},
		Trigger: store.LifecycleHookTriggerRunning,
		Action: &store.LifecycleHookAction{
			Type:                 store.LifecycleHookActionWebhook,
			Method:               "POST",
			URL:                  "https://registry.example.com/agents",
			Headers:              map[string]string{"Content-Type": "application/json"},
			Body:                 `{"name":"${AGENT_NAME}"}`,
			OnError:              store.LifecycleHookOnErrorRetry,
			TimeoutSeconds:       30,
			AllowedUntrustedVars: []string{"AGENT_NAME"},
		},
		ExecutionIdentity: uuid.New().String(),
		Enabled:           true,
		CreatedBy:         "admin@example.com",
	}
}

// =============================================================================
// LifecycleHook CRUD tests
// =============================================================================

func TestCreateLifecycleHook(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	h := sampleHook(uuid.New().String())
	require.NoError(t, s.CreateLifecycleHook(ctx, h))
	assert.False(t, h.Created.IsZero())
	assert.False(t, h.Updated.IsZero())
	assert.Equal(t, int64(1), h.StateVersion)
}

func TestCreateLifecycleHook_DuplicateID(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	require.NoError(t, s.CreateLifecycleHook(ctx, sampleHook(id)))
	err := s.CreateLifecycleHook(ctx, sampleHook(id))
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestGetLifecycleHook(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	h := sampleHook(id)
	require.NoError(t, s.CreateLifecycleHook(ctx, h))

	got, err := s.GetLifecycleHook(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "register-on-running", got.Name)
	assert.Equal(t, store.LifecycleHookScopeHub, got.ScopeType)
	assert.Equal(t, store.LifecycleHookTriggerRunning, got.Trigger)
	assert.Equal(t, h.ExecutionIdentity, got.ExecutionIdentity)
	assert.True(t, got.Enabled)
	assert.Equal(t, int64(1), got.StateVersion)

	require.NotNil(t, got.Selector)
	assert.Equal(t, "registry-agent", got.Selector.Template)

	require.NotNil(t, got.Action)
	assert.Equal(t, store.LifecycleHookActionWebhook, got.Action.Type)
	assert.Equal(t, "POST", got.Action.Method)
	assert.Equal(t, "https://registry.example.com/agents", got.Action.URL)
	assert.Equal(t, "application/json", got.Action.Headers["Content-Type"])
	assert.Equal(t, store.LifecycleHookOnErrorRetry, got.Action.OnError)
	assert.Equal(t, 30, got.Action.TimeoutSeconds)
	assert.Equal(t, []string{"AGENT_NAME"}, got.Action.AllowedUntrustedVars)
}

func TestGetLifecycleHook_ActionTypeAndAllowedVars_RoundTrip(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	h := sampleHook(id)
	h.Action.Type = store.LifecycleHookActionHTTP
	h.Action.AllowedUntrustedVars = []string{"AGENT_NAME", "AGENT_ID"}
	require.NoError(t, s.CreateLifecycleHook(ctx, h))

	// Verify Type and AllowedUntrustedVars survive Create→Get.
	got, err := s.GetLifecycleHook(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got.Action)
	assert.Equal(t, store.LifecycleHookActionHTTP, got.Action.Type)
	assert.Equal(t, []string{"AGENT_NAME", "AGENT_ID"}, got.Action.AllowedUntrustedVars)

	// Update the hook with different values.
	got.Action.Type = store.LifecycleHookActionWebhook
	got.Action.AllowedUntrustedVars = []string{"CALLBACK_URL"}
	require.NoError(t, s.UpdateLifecycleHook(ctx, got))

	// Verify Type and AllowedUntrustedVars survive Update→Get.
	got2, err := s.GetLifecycleHook(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got2.Action)
	assert.Equal(t, store.LifecycleHookActionWebhook, got2.Action.Type)
	assert.Equal(t, []string{"CALLBACK_URL"}, got2.Action.AllowedUntrustedVars)
}

func TestGetLifecycleHook_NotFound(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	_, err := s.GetLifecycleHook(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateLifecycleHook(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	h := sampleHook(id)
	require.NoError(t, s.CreateLifecycleHook(ctx, h))

	h.Name = "updated-name"
	h.Enabled = false
	h.Trigger = store.LifecycleHookTriggerStopped
	h.Action.URL = "https://registry.example.com/deregister"

	require.NoError(t, s.UpdateLifecycleHook(ctx, h))
	// Optimistic-locking version is incremented on success.
	assert.Equal(t, int64(2), h.StateVersion)

	got, err := s.GetLifecycleHook(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "updated-name", got.Name)
	assert.False(t, got.Enabled)
	assert.Equal(t, store.LifecycleHookTriggerStopped, got.Trigger)
	assert.Equal(t, "https://registry.example.com/deregister", got.Action.URL)
	assert.Equal(t, int64(2), got.StateVersion)
}

func TestUpdateLifecycleHook_NotFound(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	h := sampleHook(uuid.New().String())
	h.StateVersion = 1
	err := s.UpdateLifecycleHook(ctx, h)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateLifecycleHook_VersionConflict(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	h := sampleHook(id)
	require.NoError(t, s.CreateLifecycleHook(ctx, h))

	// Simulate a concurrent reader holding the original version.
	stale, err := s.GetLifecycleHook(ctx, id)
	require.NoError(t, err)

	// First writer succeeds and bumps the version to 2.
	h.Name = "first-writer"
	require.NoError(t, s.UpdateLifecycleHook(ctx, h))
	assert.Equal(t, int64(2), h.StateVersion)

	// Stale writer still holds version 1 → must conflict.
	stale.Name = "stale-writer"
	err = s.UpdateLifecycleHook(ctx, stale)
	assert.ErrorIs(t, err, store.ErrVersionConflict)

	// The conflicting write must not have applied.
	got, err := s.GetLifecycleHook(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "first-writer", got.Name)
	assert.Equal(t, int64(2), got.StateVersion)
}

func TestUpdateLifecycleHook_ClearsOptionalFields(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	h := sampleHook(id)
	h.ScopeID = "some-project"
	require.NoError(t, s.CreateLifecycleHook(ctx, h))

	h.ScopeID = ""
	h.Selector = nil
	h.Action = nil
	h.ExecutionIdentity = ""
	require.NoError(t, s.UpdateLifecycleHook(ctx, h))

	got, err := s.GetLifecycleHook(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, got.ScopeID)
	assert.Nil(t, got.Selector)
	assert.Nil(t, got.Action)
	assert.Empty(t, got.ExecutionIdentity)
}

func TestDeleteLifecycleHook(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	require.NoError(t, s.CreateLifecycleHook(ctx, sampleHook(id)))

	require.NoError(t, s.DeleteLifecycleHook(ctx, id))

	_, err := s.GetLifecycleHook(ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteLifecycleHook_NotFound(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	err := s.DeleteLifecycleHook(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestListLifecycleHooks(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	h1 := sampleHook(uuid.New().String())
	h1.Trigger = store.LifecycleHookTriggerRunning
	h1.Enabled = true
	require.NoError(t, s.CreateLifecycleHook(ctx, h1))

	h2 := sampleHook(uuid.New().String())
	h2.Trigger = store.LifecycleHookTriggerStopped
	h2.Enabled = false
	require.NoError(t, s.CreateLifecycleHook(ctx, h2))

	h3 := sampleHook(uuid.New().String())
	h3.Trigger = store.LifecycleHookTriggerRunning
	h3.Enabled = true
	require.NoError(t, s.CreateLifecycleHook(ctx, h3))

	// No filter → all three.
	all, err := s.ListLifecycleHooks(ctx, store.LifecycleHookFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, all.TotalCount)
	assert.Len(t, all.Items, 3)

	// Filter by trigger.
	running, err := s.ListLifecycleHooks(ctx, store.LifecycleHookFilter{
		Trigger: store.LifecycleHookTriggerRunning,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, running.TotalCount)

	// Filter by enabled.
	enabled := true
	enabledOnly, err := s.ListLifecycleHooks(ctx, store.LifecycleHookFilter{
		Enabled: &enabled,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, enabledOnly.TotalCount)

	// Filter by scope type.
	hubScoped, err := s.ListLifecycleHooks(ctx, store.LifecycleHookFilter{
		ScopeType: store.LifecycleHookScopeHub,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, hubScoped.TotalCount)
}

// =============================================================================
// HookPhase CAS dedup tests
// =============================================================================

func TestCompareAndSetHookPhase_FirstInsert(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	agentID := uuid.New().String()

	// First CAS for a new agent → changed=true (row inserted).
	changed, err := s.CompareAndSetHookPhase(ctx, agentID, "running")
	require.NoError(t, err)
	assert.True(t, changed, "first insert should return changed=true")
}

func TestCompareAndSetHookPhase_SamePhaseNoChange(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	agentID := uuid.New().String()

	// Insert initial phase.
	changed, err := s.CompareAndSetHookPhase(ctx, agentID, "running")
	require.NoError(t, err)
	assert.True(t, changed)

	// Same phase again → changed=false.
	changed, err = s.CompareAndSetHookPhase(ctx, agentID, "running")
	require.NoError(t, err)
	assert.False(t, changed, "same phase should return changed=false")
}

func TestCompareAndSetHookPhase_DifferentPhaseChanges(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	agentID := uuid.New().String()

	// Insert "running".
	changed, err := s.CompareAndSetHookPhase(ctx, agentID, "running")
	require.NoError(t, err)
	assert.True(t, changed)

	// Transition to "stopped" → changed=true.
	changed, err = s.CompareAndSetHookPhase(ctx, agentID, "stopped")
	require.NoError(t, err)
	assert.True(t, changed, "different phase should return changed=true")

	// Same "stopped" again → changed=false.
	changed, err = s.CompareAndSetHookPhase(ctx, agentID, "stopped")
	require.NoError(t, err)
	assert.False(t, changed)

	// Back to "running" → changed=true.
	changed, err = s.CompareAndSetHookPhase(ctx, agentID, "running")
	require.NoError(t, err)
	assert.True(t, changed)
}

func TestCompareAndSetHookPhase_ConcurrentDedup(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	agentID := uuid.New().String()

	// Pre-populate with an initial phase so the concurrent CAS is an update.
	changed, err := s.CompareAndSetHookPhase(ctx, agentID, "starting")
	require.NoError(t, err)
	require.True(t, changed)

	// N goroutines race to set the same new phase. Exactly one should win
	// (changed=true), all others should see changed=false.
	const N = 10
	var (
		wg      sync.WaitGroup
		winners int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			c, e := s.CompareAndSetHookPhase(ctx, agentID, "running")
			if e != nil {
				t.Errorf("unexpected error in concurrent CAS: %v", e)
				return
			}
			if c {
				atomic.AddInt64(&winners, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), winners,
		"exactly one concurrent CAS should win (changed=true)")
}

// TestCompareAndSetHookPhase_ConcurrentFirstInsertRace validates that when
// two goroutines both see "not found" and race to INSERT, the loser gets
// changed=false (not an error). On SQLite this is serialised by the
// single-writer lock so only one goroutine enters the Insert path at a time;
// on Postgres both transactions see NotFound concurrently and the loser hits
// a unique-constraint violation that the code now handles gracefully.
//
// NOTE: True concurrent-insert contention cannot be reproduced on SQLite
// because SQLite serialises all writes. This test documents the expected
// contract and verifies the graceful handling path compiles and runs
// correctly; full Postgres concurrency testing requires a Postgres backend.
func TestCompareAndSetHookPhase_ConcurrentFirstInsertRace(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	agentID := uuid.New().String()

	// N goroutines race to do the FIRST insert (no pre-existing row).
	// Exactly one should win (changed=true), all others should get
	// changed=false with NO error.
	const N = 10
	var (
		wg      sync.WaitGroup
		winners int64
		errors  int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			c, e := s.CompareAndSetHookPhase(ctx, agentID, "running")
			if e != nil {
				atomic.AddInt64(&errors, 1)
				t.Errorf("unexpected error in concurrent first-insert CAS: %v", e)
				return
			}
			if c {
				atomic.AddInt64(&winners, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(0), errors,
		"no errors should be returned — constraint violations should be handled gracefully")
	assert.Equal(t, int64(1), winners,
		"exactly one concurrent first-insert CAS should win (changed=true)")
}

func TestDeleteHookPhase(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	agentID := uuid.New().String()

	// Insert a phase.
	changed, err := s.CompareAndSetHookPhase(ctx, agentID, "running")
	require.NoError(t, err)
	require.True(t, changed)

	// Delete it.
	require.NoError(t, s.DeleteHookPhase(ctx, agentID))

	// After deletion, a fresh CAS should act as a new insert (changed=true).
	changed, err = s.CompareAndSetHookPhase(ctx, agentID, "running")
	require.NoError(t, err)
	assert.True(t, changed, "CAS after delete should re-insert and return changed=true")
}

func TestDeleteHookPhase_NonExistent(t *testing.T) {
	s := newTestLifecycleHookStore(t)
	ctx := context.Background()

	// Deleting a phase that was never created should not error.
	err := s.DeleteHookPhase(ctx, uuid.New().String())
	assert.NoError(t, err)
}
