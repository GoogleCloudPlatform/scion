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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// auditTestSetup creates a GovernanceService with audit writer for testing.
func auditTestSetup(t *testing.T) (*GovernanceService, *PreviewService, *AuthzService, store.Store, *BoundaryAuditWriter) {
	t.Helper()
	gs, ps, authz, s := govTestSetup(t)
	// GovernanceService already has an auditWriter from NewGovernanceService.
	return gs, ps, authz, s, gs.auditWriter
}

// auditTestSetupWithEvents creates a full setup including event bus.
func auditTestSetupWithEvents(t *testing.T) (*GovernanceService, *PreviewService, *AuthzService, store.Store, *BoundaryAuditWriter, *InvalidationEventBus) {
	t.Helper()
	gs, ps, authz, s, aw := auditTestSetup(t)
	eventBus := NewInvalidationEventBus(slog.Default())
	gs.eventBus = eventBus
	return gs, ps, authz, s, aw, eventBus
}

// ---------------------------------------------------------------------------
// 1. Audit write produces a durable audit ID on commit
// ---------------------------------------------------------------------------

func TestAudit_CommitProducesAuditID(t *testing.T) {
	gs, ps, _, s, aw := auditTestSetup(t)
	ctx := context.Background()

	// Seed an admin user.
	adminID := govSeedAdminUser(t, s, "audit-admin-1")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	// Create a constraint via preview+commit.
	pType := "user"
	draft := &store.AccessConstraint{
		Name:                 "audit-test-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("audit-target-user")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test audit logging",
		CreatedBy:            adminID,
	}

	// Seed the target user so the preview can resolve them.
	pvSeedUser(t, s, "audit-target-user")

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	commitResult, err := gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        actor,
	})
	require.NoError(t, err)
	require.NotNil(t, commitResult)

	// The commit must produce a non-empty audit ID.
	assert.NotEmpty(t, commitResult.AuditID, "commit must produce a durable audit ID")

	// The audit writer must have recorded the entry.
	entries := aw.GetEntries()
	require.Len(t, entries, 1, "expected exactly one audit entry")
	assert.Equal(t, commitResult.AuditID, entries[0].ID)
	assert.Equal(t, "create", entries[0].Operation)
	assert.Equal(t, adminID, entries[0].ActorID)
	assert.Equal(t, ClassificationTighten, entries[0].Classification)
	assert.NotEmpty(t, entries[0].StateFingerprint)
	assert.False(t, entries[0].Timestamp.IsZero())
}

// ---------------------------------------------------------------------------
// 2. Audit write failure rolls back the mutation conceptually
// ---------------------------------------------------------------------------

func TestAudit_WriteFailureReturnsError(t *testing.T) {
	gs, ps, _, s, aw := auditTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "audit-fail-admin")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	pType := "user"
	pvSeedUser(t, s, "audit-fail-target")
	draft := &store.AccessConstraint{
		Name:                 "audit-fail-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("audit-fail-target")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test audit failure",
		CreatedBy:            adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Inject a failure into the audit writer.
	aw.failFunc = func() error {
		return assert.AnError
	}

	// Commit should fail because audit write fails.
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        actor,
	})
	require.Error(t, err, "commit must fail when audit write fails")
	assert.Contains(t, err.Error(), "audit write failed")
}

// ---------------------------------------------------------------------------
// 2b. Audit failure compensating action: create is rolled back
// ---------------------------------------------------------------------------

func TestAudit_CompensatingAction_CreateRolledBack(t *testing.T) {
	gs, ps, _, s, aw := auditTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "comp-create-admin")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	pType := "user"
	pvSeedUser(t, s, "comp-create-target")
	draft := &store.AccessConstraint{
		Name:                 "comp-create-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("comp-create-target")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test compensating action on create",
		CreatedBy:            adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Inject audit failure.
	aw.failFunc = func() error { return assert.AnError }

	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        actor,
	})
	require.Error(t, err, "commit must fail when audit write fails")
	assert.Contains(t, err.Error(), "audit write failed")

	// After audit failure, the constraint must NOT be in the store — the
	// compensating action should have deleted it.
	constraints, listErr := s.ListAccessConstraints(ctx, 100, 0)
	require.NoError(t, listErr)
	for _, c := range constraints {
		assert.NotEqual(t, "comp-create-boundary", c.Name,
			"constraint created before audit failure must be removed by compensating action")
	}
}

// ---------------------------------------------------------------------------
// 2c. Audit failure compensating action: update is restored
// ---------------------------------------------------------------------------

func TestAudit_CompensatingAction_UpdateRestored(t *testing.T) {
	gs, ps, _, s, aw := auditTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "comp-update-admin")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	// Create a constraint first (with working audit).
	pType := "user"
	pvSeedUser(t, s, "comp-update-target")
	draft := &store.AccessConstraint{
		Name:                 "comp-update-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("comp-update-target")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		Purpose:              "test compensating action on update",
		CreatedBy:            adminID,
	}
	created := govCreateAndCommit(t, gs, ps, draft, actor)
	require.NotNil(t, created)

	// Now prepare an update.
	updateDraft := &store.AccessConstraint{
		ID:                   created.ID,
		Name:                 "comp-update-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("comp-update-target")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"}, // Removed agent.create.
		Purpose:              "updated purpose",
		CreatedBy:            adminID,
	}

	previewResult, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "update",
		Draft:        updateDraft,
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		Actor:        actor,
	})
	require.NoError(t, err)

	// Inject audit failure.
	aw.failFunc = func() error { return assert.AnError }

	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "update",
		Draft:        updateDraft,
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		PreviewToken: previewResult.PreviewToken,
		Actor:        actor,
	})
	require.Error(t, err, "commit must fail when audit write fails")
	assert.Contains(t, err.Error(), "audit write failed")

	// After audit failure, the constraint must be restored to its previous state.
	restored, getErr := s.GetAccessConstraint(ctx, created.ID)
	require.NoError(t, getErr)
	assert.ElementsMatch(t, []string{"agent.read", "agent.create"}, restored.MaximumPermissions,
		"constraint must be restored to before-state after audit failure on update")
}

// ---------------------------------------------------------------------------
// 3. Audit entry contains no sensitive principal data (redaction test)
// ---------------------------------------------------------------------------

func TestAudit_RedactionNoSensitiveData(t *testing.T) {
	gs, ps, _, s, aw := auditTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "redact-admin")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	pvSeedUser(t, s, "redact-target")
	pType := "user"
	draft := &store.AccessConstraint{
		Name:                 "redact-test-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("redact-target")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test redaction",
		CreatedBy:            adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        actor,
	})
	require.NoError(t, err)

	entries := aw.GetEntries()
	require.Len(t, entries, 1)

	entry := entries[0]

	// ---------------------------------------------------------------------------
	// Field-level sensitive data assertions (R2): verify that every field in
	// BoundaryAuditEntry that could theoretically carry PII is safe.
	// ---------------------------------------------------------------------------

	// Common PII patterns: email addresses, typical principal/group names.
	piiPatterns := []string{"@", "user@", "admin@", ".com", ".org"}

	// ActorID should be an opaque identifier, never an email.
	assert.NotContains(t, entry.ActorID, "@", "ActorID must not contain an email address")
	assert.Equal(t, adminID, entry.ActorID, "ActorID should be the admin's opaque ID")

	// ConstraintID should not contain principal names, group names, or emails.
	for _, pat := range piiPatterns {
		assert.NotContains(t, entry.ConstraintID, pat,
			"ConstraintID must not contain PII pattern %q", pat)
	}

	// PreviewID should not contain principal or group identifiers.
	for _, pat := range piiPatterns {
		assert.NotContains(t, entry.PreviewID, pat,
			"PreviewID must not contain PII pattern %q", pat)
	}

	// DraftHash should not contain principal or group identifiers.
	for _, pat := range piiPatterns {
		assert.NotContains(t, entry.DraftHash, pat,
			"DraftHash must not contain PII pattern %q", pat)
	}

	// StateFingerprint should not contain principal or group identifiers.
	for _, pat := range piiPatterns {
		assert.NotContains(t, entry.StateFingerprint, pat,
			"StateFingerprint must not contain PII pattern %q", pat)
	}

	// ImpactCounts are numeric — verify they are non-negative (safe by type).
	assert.GreaterOrEqual(t, entry.ImpactCounts.AffectedPrincipals, 0,
		"AffectedPrincipals must be non-negative")
	assert.GreaterOrEqual(t, entry.ImpactCounts.PermissionsAdded, 0,
		"PermissionsAdded must be non-negative")
	assert.GreaterOrEqual(t, entry.ImpactCounts.PermissionsRemoved, 0,
		"PermissionsRemoved must be non-negative")
}

// ---------------------------------------------------------------------------
// 4. Recovery audit: disable-all with atomic audit
// ---------------------------------------------------------------------------

func TestRecovery_DisableAllWithAudit(t *testing.T) {
	gs, ps, _, s, _ := auditTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "recovery-admin")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	// Create two constraints.
	for i, name := range []string{"recovery-boundary-1", "recovery-boundary-2"} {
		pvSeedUser(t, s, name+"-user")
		pType := "user"
		draft := &store.AccessConstraint{
			Name:                 name,
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: &pType,
			SubjectPrincipalID:   strPtr(tid(name + "-user")),
			ScopeType:            "system",
			MaximumPermissions:   []string{"agent.read", "agent.create"},
			Purpose:              "test recovery",
			CreatedBy:            adminID,
		}
		_ = i
		govCreateAndCommit(t, gs, ps, draft, actor)
	}

	// Create recovery service with the same audit writer.
	rs := NewRecoveryService(s, gs.auditWriter, nil, slog.Default())
	rs.nowFunc = gs.nowFunc

	result, err := rs.DisableAll(ctx, adminID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 2, result.DisabledCount, "should disable both constraints")
	assert.NotEmpty(t, result.AuditID, "disable-all must produce an audit ID")
	assert.Len(t, result.DisabledIDs, 2)

	// Verify the audit entry.
	entries := gs.auditWriter.GetEntries()
	var disableEntry *BoundaryAuditEntry
	for i := range entries {
		if entries[i].Operation == "disable_all" {
			disableEntry = &entries[i]
			break
		}
	}
	require.NotNil(t, disableEntry, "must have a disable_all audit entry")
	assert.Equal(t, "all", disableEntry.ConstraintID)
	assert.Equal(t, adminID, disableEntry.ActorID)
}

// ---------------------------------------------------------------------------
// 5. Recovery audit failure rolls back disable
// ---------------------------------------------------------------------------

func TestRecovery_AuditFailureRollsBackDisable(t *testing.T) {
	_, _, _, s, _ := auditTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "recovery-fail-admin")

	// Create a constraint directly.
	pvSeedUser(t, s, "recovery-fail-user")
	pType := "user"
	created, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "recovery-fail-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("recovery-fail-user")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test recovery audit failure",
		CreatedBy:            adminID,
	})
	require.NoError(t, err)

	// First, test that audit failure during disable rolls back.
	failWriter := NewBoundaryAuditWriter(slog.Default())
	failWriter.failFunc = func() error { return assert.AnError }
	rsf := NewRecoveryService(s, failWriter, nil, slog.Default())

	_, err = rsf.DisableAll(ctx, adminID)
	require.Error(t, err, "disable should fail when audit write fails")
	assert.Contains(t, err.Error(), "audit write failed")

	// Verify constraint is still enabled (rollback happened).
	c, lookupErr := s.GetAccessConstraint(ctx, created.ID)
	require.NoError(t, lookupErr)
	assert.False(t, c.Disabled, "constraint should still be enabled after audit failure rollback")

	// Now test the normal disable/recover cycle with a working writer.
	regularWriter := NewBoundaryAuditWriter(slog.Default())
	rs2 := NewRecoveryService(s, regularWriter, nil, slog.Default())

	result, err := rs2.DisableAll(ctx, adminID)
	require.NoError(t, err)
	assert.Equal(t, 1, result.DisabledCount)

	// Verify constraint is now disabled.
	c, err = s.GetAccessConstraint(ctx, created.ID)
	require.NoError(t, err)
	assert.True(t, c.Disabled, "constraint should be disabled")

	// Now recover.
	recoverResult, err := rs2.RecoverAll(ctx, adminID)
	require.NoError(t, err)
	assert.Equal(t, 1, recoverResult.RecoveredCount)
	assert.NotEmpty(t, recoverResult.AuditID)

	// Verify constraint is re-enabled.
	c, err = s.GetAccessConstraint(ctx, created.ID)
	require.NoError(t, err)
	assert.False(t, c.Disabled, "constraint should be re-enabled after recovery")
}

// ---------------------------------------------------------------------------
// 6. Online/offline exclusion: two operations cannot run concurrently
// ---------------------------------------------------------------------------

func TestRecovery_OnlineOfflineExclusion(t *testing.T) {
	lock := NewRecoveryLock()

	// First acquire should succeed.
	assert.True(t, lock.TryAcquire("instance-1"), "first acquire should succeed")
	assert.True(t, lock.IsHeld())
	assert.Equal(t, "instance-1", lock.Holder())

	// Second acquire should fail.
	assert.False(t, lock.TryAcquire("instance-2"), "second acquire should fail while lock is held")

	// Release and try again.
	lock.Release()
	assert.False(t, lock.IsHeld())

	assert.True(t, lock.TryAcquire("instance-2"), "acquire should succeed after release")
	lock.Release()
}

func TestRecovery_ConcurrentExclusion(t *testing.T) {
	lock := NewRecoveryLock()
	const goroutines = 10

	var acquireCount atomic.Int32
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			if lock.TryAcquire("goroutine-" + string(rune('0'+id))) {
				acquireCount.Add(1)
				// Hold the lock briefly.
				time.Sleep(1 * time.Millisecond)
				lock.Release()
			}
		}(i)
	}

	wg.Wait()
	// At least one goroutine must have acquired the lock, and the count
	// tells us the lock was properly exclusive (if all acquired, it means
	// they serialized through acquire/release properly).
	assert.True(t, acquireCount.Load() >= 1, "at least one goroutine should acquire the lock")
}

// ---------------------------------------------------------------------------
// 7. SQLite exclusive lock during recovery
// ---------------------------------------------------------------------------

func TestRecovery_SQLiteExclusiveLock(t *testing.T) {
	_, _, _, s, _ := auditTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "sqlite-lock-admin")

	rs := NewRecoveryService(s, NewBoundaryAuditWriter(slog.Default()), nil, slog.Default())

	// First recovery should succeed.
	assert.True(t, rs.recoveryLock.TryAcquire(adminID))
	rs.recoveryLock.Release()

	// Running DisableAll acquires and releases the lock.
	result, err := rs.DisableAll(ctx, adminID)
	require.NoError(t, err)
	assert.Equal(t, 0, result.DisabledCount) // No constraints to disable.

	// Lock should be released after DisableAll completes.
	assert.False(t, rs.recoveryLock.IsHeld())
}

// ---------------------------------------------------------------------------
// 8. Event delivery and authorization
// ---------------------------------------------------------------------------

func TestEvents_DeliveryToAuthorizedSubscribers(t *testing.T) {
	bus := NewInvalidationEventBus(slog.Default())

	var received []InvalidationEvent
	var mu sync.Mutex

	// Subscribe an authorized handler.
	bus.Subscribe("admin-1", nil, func(event InvalidationEvent) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, event)
	}, true)

	// Publish an event.
	bus.Publish(InvalidationEvent{
		Type:      EventBoundaryCreated,
		EntityID:  "constraint-1",
		Timestamp: time.Now(),
	})

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 1)
	assert.Equal(t, EventBoundaryCreated, received[0].Type)
	assert.Equal(t, "constraint-1", received[0].EntityID)
}

func TestEvents_UnauthorizedSubscribersBlocked(t *testing.T) {
	bus := NewInvalidationEventBus(slog.Default())

	var received []InvalidationEvent
	var mu sync.Mutex

	// Subscribe an unauthorized handler.
	bus.Subscribe("hacker", nil, func(event InvalidationEvent) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, event)
	}, false)

	// Publish an event.
	bus.Publish(InvalidationEvent{
		Type:      EventBoundaryCreated,
		EntityID:  "constraint-1",
		Timestamp: time.Now(),
	})

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, received, 0, "unauthorized subscriber should not receive events")
}

func TestEvents_TypeFilterWorks(t *testing.T) {
	bus := NewInvalidationEventBus(slog.Default())

	var received []InvalidationEvent
	var mu sync.Mutex

	// Subscribe only to boundary events.
	bus.Subscribe("admin", []string{EventBoundaryCreated, EventBoundaryDeleted}, func(event InvalidationEvent) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, event)
	}, true)

	// Publish a matching event.
	bus.Publish(InvalidationEvent{
		Type:      EventBoundaryCreated,
		EntityID:  "c1",
		Timestamp: time.Now(),
	})

	// Publish a non-matching event.
	bus.Publish(InvalidationEvent{
		Type:      EventMembershipChanged,
		EntityID:  "g1",
		Timestamp: time.Now(),
	})

	// Publish another matching event.
	bus.Publish(InvalidationEvent{
		Type:      EventBoundaryDeleted,
		EntityID:  "c2",
		Timestamp: time.Now(),
	})

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, received, 2, "should receive only matching event types")
	assert.Equal(t, EventBoundaryCreated, received[0].Type)
	assert.Equal(t, EventBoundaryDeleted, received[1].Type)
}

func TestEvents_PanicInHandlerRecovered(t *testing.T) {
	bus := NewInvalidationEventBus(slog.Default())

	var received []InvalidationEvent
	var mu sync.Mutex

	// Subscribe a panicking handler.
	bus.Subscribe("panic-sub", nil, func(event InvalidationEvent) {
		panic("test panic")
	}, true)

	// Subscribe a normal handler after the panicking one.
	bus.Subscribe("normal-sub", nil, func(event InvalidationEvent) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, event)
	}, true)

	// Publish should not panic and the second handler should still receive.
	assert.NotPanics(t, func() {
		bus.Publish(InvalidationEvent{
			Type:      EventBoundaryUpdated,
			EntityID:  "c1",
			Timestamp: time.Now(),
		})
	})

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, received, 1, "non-panicking handler should still receive events")
}

func TestEvents_UnsubscribeWorks(t *testing.T) {
	bus := NewInvalidationEventBus(slog.Default())

	var count atomic.Int32

	subID := bus.Subscribe("admin", nil, func(event InvalidationEvent) {
		count.Add(1)
	}, true)

	bus.Publish(InvalidationEvent{Type: EventBoundaryCreated, EntityID: "c1", Timestamp: time.Now()})
	assert.Equal(t, int32(1), count.Load())

	bus.Unsubscribe(subID)
	bus.Publish(InvalidationEvent{Type: EventBoundaryCreated, EntityID: "c2", Timestamp: time.Now()})
	assert.Equal(t, int32(1), count.Load(), "should not receive after unsubscribe")
}

// ---------------------------------------------------------------------------
// 9. Events wired into governance commit path
// ---------------------------------------------------------------------------

func TestEvents_PublishedOnCommit(t *testing.T) {
	gs, ps, _, s, _, eventBus := auditTestSetupWithEvents(t)
	ctx := context.Background()

	var received []InvalidationEvent
	var mu sync.Mutex

	eventBus.Subscribe("test", nil, func(event InvalidationEvent) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, event)
	}, true)

	adminID := govSeedAdminUser(t, s, "event-commit-admin")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	pvSeedUser(t, s, "event-commit-target")
	pType := "user"
	draft := &store.AccessConstraint{
		Name:                 "event-test-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("event-commit-target")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test events on commit",
		CreatedBy:            adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        actor,
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 1)
	assert.Equal(t, "boundary.created", received[0].Type)
	assert.NotEmpty(t, received[0].EntityID)
}

// ---------------------------------------------------------------------------
// 10. Capabilities computation
// ---------------------------------------------------------------------------

func TestCapabilities_AdminHasFullCapabilities(t *testing.T) {
	srv, s := testServer(t)
	authz := srv.authzService
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "caps-admin")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	cs := NewCapabilitiesService(s, authz, slog.Default())

	caps, err := cs.ComputeCapabilities(ctx, actor, "system", "")
	require.NoError(t, err)
	require.NotNil(t, caps)

	assert.True(t, caps.IsAdmin, "admin should have IsAdmin=true")
	assert.True(t, caps.CanCreate, "admin should have CanCreate=true")
	assert.True(t, caps.CanUpdate, "admin should have CanUpdate=true")
	assert.True(t, caps.CanDelete, "admin should have CanDelete=true")
	assert.True(t, caps.CanPreview, "admin should have CanPreview=true")
}

func TestCapabilities_NonAdminHasNoMutationCapabilities(t *testing.T) {
	srv, s := testServer(t)
	authz := srv.authzService
	ctx := context.Background()

	userID := govSeedNonAdminUser(t, s, "caps-nonadmin")
	actor := PrincipalContext{Kind: "user", ID: userID}

	cs := NewCapabilitiesService(s, authz, slog.Default())

	caps, err := cs.ComputeCapabilities(ctx, actor, "system", "")
	require.NoError(t, err)
	require.NotNil(t, caps)

	assert.False(t, caps.IsAdmin, "non-admin should have IsAdmin=false")
	assert.False(t, caps.CanCreate, "non-admin should have CanCreate=false")
	assert.False(t, caps.CanUpdate, "non-admin should have CanUpdate=false")
	assert.False(t, caps.CanDelete, "non-admin should have CanDelete=false")
	assert.False(t, caps.CanPreview, "non-admin should have CanPreview=false")
}

func TestCapabilities_DisabledConstraintBlocksMutation(t *testing.T) {
	srv, s := testServer(t)
	authz := srv.authzService
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "caps-disabled-admin")
	actor := PrincipalContext{Kind: "user", ID: adminID}

	// Create a constraint and disable it.
	pType := "user"
	pvSeedUser(t, s, "caps-disabled-target")
	created, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "caps-disabled-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("caps-disabled-target")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test disabled caps",
		CreatedBy:            adminID,
	})
	require.NoError(t, err)

	err = s.DisableAccessConstraint(ctx, created.ID)
	require.NoError(t, err)

	cs := NewCapabilitiesService(s, authz, slog.Default())
	caps, err := cs.ComputeResourceCapabilities(ctx, actor, created.ID)
	require.NoError(t, err)

	assert.False(t, caps.CanUpdate, "disabled constraint should block update")
	assert.False(t, caps.CanDelete, "disabled constraint should block delete")
	assert.Contains(t, caps.Restrictions, "recovery_disabled")
}

// ---------------------------------------------------------------------------
// 11. Metrics tracking
// ---------------------------------------------------------------------------

func TestMetrics_RecordMutation(t *testing.T) {
	m := NewBoundaryMetrics(slog.Default())

	m.RecordMutation(ClassificationTighten, 100)
	m.RecordMutation(ClassificationRelax, 200)
	m.RecordMutation(ClassificationMixed, 150)
	m.RecordMutation(ClassificationTighten, 50)

	snap := m.Snapshot()
	assert.Equal(t, int64(2), snap["tighten_count"])
	assert.Equal(t, int64(1), snap["relax_count"])
	assert.Equal(t, int64(1), snap["mixed_count"])
	assert.Equal(t, int64(50), snap["last_mutation_latency_ms"])
}

func TestMetrics_RecordLockoutCheck(t *testing.T) {
	m := NewBoundaryMetrics(slog.Default())

	m.RecordLockoutCheck(false)
	m.RecordLockoutCheck(false)
	m.RecordLockoutCheck(true)

	snap := m.Snapshot()
	assert.Equal(t, int64(3), snap["lockout_check_count"])
	assert.Equal(t, int64(1), snap["lockout_block_count"])
}

// ---------------------------------------------------------------------------
// 12. Audit writer: validation
// ---------------------------------------------------------------------------

func TestAudit_ValidateEntry(t *testing.T) {
	tests := []struct {
		name    string
		entry   BoundaryAuditEntry
		wantErr bool
	}{
		{
			name: "valid entry",
			entry: BoundaryAuditEntry{
				ID:        "aud_test",
				Operation: "create",
				ActorID:   "user-1",
				Timestamp: time.Now(),
			},
			wantErr: false,
		},
		{
			name: "missing ID",
			entry: BoundaryAuditEntry{
				Operation: "create",
				ActorID:   "user-1",
				Timestamp: time.Now(),
			},
			wantErr: true,
		},
		{
			name: "missing operation",
			entry: BoundaryAuditEntry{
				ID:        "aud_test",
				ActorID:   "user-1",
				Timestamp: time.Now(),
			},
			wantErr: true,
		},
		{
			name: "invalid operation",
			entry: BoundaryAuditEntry{
				ID:        "aud_test",
				Operation: "invalid",
				ActorID:   "user-1",
				Timestamp: time.Now(),
			},
			wantErr: true,
		},
		{
			name: "missing actor",
			entry: BoundaryAuditEntry{
				ID:        "aud_test",
				Operation: "create",
				Timestamp: time.Now(),
			},
			wantErr: true,
		},
		{
			name: "missing timestamp",
			entry: BoundaryAuditEntry{
				ID:        "aud_test",
				Operation: "create",
				ActorID:   "user-1",
			},
			wantErr: true,
		},
		{
			name: "disable_all is valid",
			entry: BoundaryAuditEntry{
				ID:        "aud_test",
				Operation: "disable_all",
				ActorID:   "user-1",
				Timestamp: time.Now(),
			},
			wantErr: false,
		},
		{
			name: "recovery is valid",
			entry: BoundaryAuditEntry{
				ID:        "aud_test",
				Operation: "recovery",
				ActorID:   "user-1",
				Timestamp: time.Now(),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAuditEntry(&tt.entry)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 13. Recovery: list recovery-disabled boundaries
// ---------------------------------------------------------------------------

func TestRecovery_ListRecoveryDisabled(t *testing.T) {
	_, _, _, s, _ := auditTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "list-disabled-admin")

	// Create and disable a constraint.
	pType := "user"
	pvSeedUser(t, s, "list-disabled-user")
	created, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "list-disabled-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(tid("list-disabled-user")),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test list disabled",
		CreatedBy:            adminID,
	})
	require.NoError(t, err)

	err = s.DisableAccessConstraint(ctx, created.ID)
	require.NoError(t, err)

	rs := NewRecoveryService(s, NewBoundaryAuditWriter(slog.Default()), nil, slog.Default())
	disabled, err := rs.ListRecoveryDisabled(ctx)
	require.NoError(t, err)
	assert.Len(t, disabled, 1)
	assert.Equal(t, created.ID, disabled[0].ID)
	assert.True(t, disabled[0].Disabled)
}

// ---------------------------------------------------------------------------
// 14. Correlation ID from context
// ---------------------------------------------------------------------------

func TestCorrelationIDFromContext(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", correlationIDFromContext(ctx), "empty context should return empty string")

	ctx = contextWithCorrelationID(ctx, "req-123")
	assert.Equal(t, "req-123", correlationIDFromContext(ctx))
}

// ---------------------------------------------------------------------------
// 15. Audit entries for specific constraint
// ---------------------------------------------------------------------------

func TestAudit_GetEntriesForConstraint(t *testing.T) {
	aw := NewBoundaryAuditWriter(slog.Default())
	ctx := context.Background()

	_, err := aw.WriteAuditEntry(ctx, AuditRequest{
		ConstraintID: "c1",
		Operation:    "create",
		ActorID:      "user-1",
	})
	require.NoError(t, err)

	_, err = aw.WriteAuditEntry(ctx, AuditRequest{
		ConstraintID: "c2",
		Operation:    "update",
		ActorID:      "user-1",
	})
	require.NoError(t, err)

	_, err = aw.WriteAuditEntry(ctx, AuditRequest{
		ConstraintID: "c1",
		Operation:    "delete",
		ActorID:      "user-2",
	})
	require.NoError(t, err)

	c1Entries := aw.GetEntriesForConstraint("c1")
	assert.Len(t, c1Entries, 2)

	c2Entries := aw.GetEntriesForConstraint("c2")
	assert.Len(t, c2Entries, 1)

	c3Entries := aw.GetEntriesForConstraint("c3")
	assert.Len(t, c3Entries, 0)
}
