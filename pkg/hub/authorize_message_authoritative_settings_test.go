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
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Authoritative security-settings tests (#1686)
//
// These tests verify that cross-project admission decisions read the Hub
// security setting directly from the store (authoritative read) rather than
// from the replica-local cache. The boundary is "requests starting after the
// policy write completes" — no recall or in-flight cancellation promise.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Test: Two hub instances sharing a store — disabling on A denies on B
// immediately, without B's cache being refreshed.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_TwoInstances_DisableOnA_DeniesOnB(t *testing.T) {
	// Setup: two servers sharing the same backing store.
	f := crossProjectSetup(t)
	ctx := context.Background()

	// Create a shared fakeHubSettingStore that both operational-settings
	// instances will read from. This simulates two hub replicas sharing a
	// Postgres-backed settings table.
	sharedSettingStore := newFakeHubSettingStore()

	// Enable cross-project messaging in the shared store.
	sharedSettingStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": true}`))

	// Create two OperationalSettings instances backed by the same store.
	// Instance A (the writer) will get a cache refresh.
	// Instance B (the reader) will NOT get a cache refresh, simulating a
	// missed notification.
	opsA := NewOperationalSettings(sharedSettingStore, emptyKoanf(), emptyKoanf())
	opsB := NewOperationalSettings(sharedSettingStore, emptyKoanf(), emptyKoanf())

	// Refresh both — they now both see enabled=true in their caches.
	if _, err := opsA.Refresh(ctx); err != nil {
		t.Fatalf("opsA refresh: %v", err)
	}
	if _, err := opsB.Refresh(ctx); err != nil {
		t.Fatalf("opsB refresh: %v", err)
	}

	// Verify both caches show enabled.
	if !opsA.CrossProjectMessagingEnabled() {
		t.Fatal("opsA cache should show enabled after refresh")
	}
	if !opsB.CrossProjectMessagingEnabled() {
		t.Fatal("opsB cache should show enabled after refresh")
	}

	// Setup agents and policy for a cross-project send.
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "auth-cp-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "auth-cp-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})

	// Wire server B with opsB (stale cache).
	f.srv.SetOperationalSettings(opsB)

	// Verify: with enabled cache, cross-project send is allowed.
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("expected allowed with enabled setting, got denied: %s (code: %s)",
			decision.Reason, decision.Code)
	}

	// Now disable cross-project messaging via server A (shared store update).
	// Do NOT refresh B's cache — simulating a missed notification.
	sharedSettingStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": false}`))

	// Refresh only A's cache.
	if _, err := opsA.Refresh(ctx); err != nil {
		t.Fatalf("opsA refresh after disable: %v", err)
	}

	// Verify: B's cache is stale (still shows enabled).
	if !opsB.CrossProjectMessagingEnabled() {
		t.Fatal("opsB cache should still show enabled (stale)")
	}

	// The very next send decision on B must deny — the authoritative read
	// bypasses the stale cache.
	decision = f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("cross-project send must be denied after disabling, even with stale cache")
	}
	if decision.Code != MessageDenialCrossProjectDisabled {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectDisabled, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Re-enabling restores access immediately, without cache refresh.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_ReEnableRestoresAccess(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	sharedSettingStore := newFakeHubSettingStore()

	// Start disabled.
	sharedSettingStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": false}`))

	ops := NewOperationalSettings(sharedSettingStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("ops refresh: %v", err)
	}
	f.srv.SetOperationalSettings(ops)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "reen-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "reen-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)

	// Verify disabled.
	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("expected denied when disabled")
	}

	// Re-enable in the shared store without refreshing the cache.
	sharedSettingStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": true}`))

	// The very next decision should allow — authoritative read sees the update.
	decision = f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("expected allowed after re-enable, got denied: %s (code: %s)",
			decision.Reason, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Absent/default settings deny consistently.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_AbsentSettingDenies(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	// Empty store — no messaging section.
	emptySettingStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(emptySettingStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("ops refresh: %v", err)
	}
	f.srv.SetOperationalSettings(ops)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "absent-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "absent-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)

	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("expected denied when messaging section is absent from store")
	}
	if decision.Code != MessageDenialCrossProjectDisabled {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectDisabled, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Setting present but field omitted → deny.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_FieldOmittedDenies(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	settingStore := newFakeHubSettingStore()
	// Messaging section exists but cross_project_messaging_enabled is not set.
	settingStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch": true}`))
	ops := NewOperationalSettings(settingStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("ops refresh: %v", err)
	}
	f.srv.SetOperationalSettings(ops)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "omit-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "omit-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)

	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("expected denied when cross_project_messaging_enabled field is omitted")
	}
	if decision.Code != MessageDenialCrossProjectDisabled {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectDisabled, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Store read failure fails closed (deny).
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_StoreFailureFailsClosed(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	failStore := &allFailingHubSettingStore{
		err: fmt.Errorf("simulated store failure"),
	}
	ops := NewOperationalSettings(failStore, emptyKoanf(), emptyKoanf())
	// Skip Refresh — it would also fail. The cache is empty but irrelevant;
	// the authoritative read is what matters.
	f.srv.SetOperationalSettings(ops)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "fail-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "fail-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)

	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("expected denied when store read fails")
	}
	if decision.Code != MessageDenialCrossProjectDisabled {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectDisabled, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Malformed JSON in store fails closed.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_MalformedJSONFailsClosed(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	settingStore := newFakeHubSettingStore()
	settingStore.seed("messaging", json.RawMessage(`{invalid json`))
	ops := NewOperationalSettings(settingStore, emptyKoanf(), emptyKoanf())
	// Refresh will mark it as malformed in cache.
	_, _ = ops.Refresh(ctx)
	f.srv.SetOperationalSettings(ops)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "malform-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "malform-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)

	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("expected denied when store JSON is malformed")
	}
	if decision.Code != MessageDenialCrossProjectDisabled {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectDisabled, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: HubPolicyRevision is reported in the decision.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_HubPolicyRevisionReported(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	sharedSettingStore := newFakeHubSettingStore()
	sharedSettingStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": true}`))
	ops := NewOperationalSettings(sharedSettingStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("ops refresh: %v", err)
	}
	f.srv.SetOperationalSettings(ops)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	sender := msgAuthzAgent(t, f.store, "rev-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "rev-target", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)

	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("expected allowed, got denied: %s (code: %s)", decision.Reason, decision.Code)
	}
	if decision.HubPolicyRevision == 0 {
		t.Fatal("expected non-zero HubPolicyRevision in allowed decision")
	}

	// Also verify revision is reported on denial.
	sharedSettingStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": false}`))
	// Bump revision to distinguish.
	sharedSettingStore.mu.Lock()
	sharedSettingStore.settings["messaging"].Revision = 42
	sharedSettingStore.mu.Unlock()

	decision = f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("expected denied after disabling")
	}
	if decision.HubPolicyRevision != 42 {
		t.Fatalf("expected HubPolicyRevision 42, got %d", decision.HubPolicyRevision)
	}
}

// ---------------------------------------------------------------------------
// Test: Sender/target mode checks still use authoritative store records.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_ModeChecksUseCurrentRecords(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	// Sender in hub mode, target in project mode — should be allowed.
	sender := msgAuthzAgent(t, f.store, "mode-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "mode-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)

	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("hub→project should be allowed: %s (code: %s)", decision.Reason, decision.Code)
	}

	// Sender in project mode — should be denied (sender must be hub mode).
	senderProject := msgAuthzAgent(t, f.store, "mode-sender-proj", f.projectA, store.MessageModeProject,
		[]string{f.ownerA.ID})
	senderProjIdent := msgAuthzAgentIdentity(senderProject.ID, f.projectA, senderProject.Ancestry)

	decision = f.srv.EvaluateAgentMessage(ctx, senderProjIdent, target)
	if decision.Allowed {
		t.Fatal("project-mode sender should be denied for cross-project")
	}
	if decision.Code != MessageDenialCrossProjectSenderMode {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectSenderMode, decision.Code)
	}

	// Target in branch mode — should be denied.
	targetBranch := msgAuthzAgent(t, f.store, "mode-target-branch", f.projectB, store.MessageModeBranch,
		[]string{f.ownerB.ID})
	decision = f.srv.EvaluateAgentMessage(ctx, senderIdent, targetBranch)
	if decision.Allowed {
		t.Fatal("branch-mode target should be denied for cross-project")
	}
	if decision.Code != MessageDenialCrossProjectTargetMode {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectTargetMode, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: Membership checks continue to use authoritative records.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_MembershipCheckStillAuthoritative(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	enableCrossProjectMessaging(t, f.srv)

	// Set members-only policy.
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require_NoError(t, err)

	// ownerA is NOT a member of projectB — should be denied.
	sender := msgAuthzAgent(t, f.store, "memchk-sender", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	target := msgAuthzAgent(t, f.store, "memchk-target", f.projectB, store.MessageModeProject,
		[]string{f.ownerB.ID})
	senderIdent := msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry)

	decision := f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if decision.Allowed {
		t.Fatal("non-member origin should be denied with members policy")
	}
	if decision.Code != MessageDenialCrossProjectNotMember {
		t.Fatalf("expected code %s, got %s", MessageDenialCrossProjectNotMember, decision.Code)
	}

	// Add ownerA as member of projectB — should now be allowed.
	msgAuthzAddProjectMember(t, f.store, f.ownerA.ID, f.projectB, "project-b", store.GroupMemberRoleMember)

	decision = f.srv.EvaluateAgentMessage(ctx, senderIdent, target)
	if !decision.Allowed {
		t.Fatalf("member origin should be allowed: %s (code: %s)", decision.Reason, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: ReadAuthoritativeCrossProjectEnabled unit tests.
// ---------------------------------------------------------------------------

func TestReadAuthoritativeCrossProjectEnabled_Enabled(t *testing.T) {
	ctx := context.Background()
	st := newFakeHubSettingStore()
	st.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": true}`))
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())

	result := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if !result.Enabled {
		t.Fatal("expected enabled=true")
	}
	if result.Revision != 1 {
		t.Fatalf("expected revision 1, got %d", result.Revision)
	}
}

func TestReadAuthoritativeCrossProjectEnabled_Disabled(t *testing.T) {
	ctx := context.Background()
	st := newFakeHubSettingStore()
	st.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": false}`))
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())

	result := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Enabled {
		t.Fatal("expected enabled=false")
	}
}

func TestReadAuthoritativeCrossProjectEnabled_SectionAbsent(t *testing.T) {
	ctx := context.Background()
	st := newFakeHubSettingStore()
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())

	result := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Enabled {
		t.Fatal("expected enabled=false when section absent")
	}
	if result.Revision != 0 {
		t.Fatalf("expected revision 0 when section absent, got %d", result.Revision)
	}
}

func TestReadAuthoritativeCrossProjectEnabled_FieldOmitted(t *testing.T) {
	ctx := context.Background()
	st := newFakeHubSettingStore()
	st.seed("messaging", json.RawMessage(`{"conversation_envelope_switch": true}`))
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())

	result := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Enabled {
		t.Fatal("expected enabled=false when field is omitted")
	}
}

func TestReadAuthoritativeCrossProjectEnabled_MalformedJSON(t *testing.T) {
	ctx := context.Background()
	st := newFakeHubSettingStore()
	st.seed("messaging", json.RawMessage(`{not valid json`))
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())

	result := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if result.Err != nil {
		t.Fatalf("unexpected error for malformed JSON (should fail closed without error): %v", result.Err)
	}
	if result.Enabled {
		t.Fatal("expected enabled=false on malformed JSON")
	}
}

func TestReadAuthoritativeCrossProjectEnabled_StoreError(t *testing.T) {
	ctx := context.Background()
	st := &allFailingHubSettingStore{err: fmt.Errorf("connection refused")}
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())

	result := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if result.Err == nil {
		t.Fatal("expected error when store fails")
	}
	if result.Enabled {
		t.Fatal("expected enabled=false on store error (fail closed)")
	}
}

// ---------------------------------------------------------------------------
// Test: Authoritative read and legacy cache can disagree — authoritative wins.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_CacheStaleDoesNotOverrideStore(t *testing.T) {
	ctx := context.Background()

	st := newFakeHubSettingStore()
	// Store says enabled.
	st.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": true}`))
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Cache says enabled.
	if !ops.CrossProjectMessagingEnabled() {
		t.Fatal("cache should say enabled")
	}

	// Now update store to disabled WITHOUT refreshing cache.
	st.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": false}`))

	// Cache is stale (still says enabled).
	if !ops.CrossProjectMessagingEnabled() {
		t.Fatal("stale cache should still say enabled")
	}

	// Authoritative read should see disabled.
	result := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Enabled {
		t.Fatal("authoritative read should see disabled despite stale cache")
	}
}

// ---------------------------------------------------------------------------
// Test: EvaluateCrossProjectReadAccess uses authoritative read.
// ---------------------------------------------------------------------------

func TestAuthoritativeSettings_ReadAccessUsesAuthoritativeRead(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()

	sharedSettingStore := newFakeHubSettingStore()
	sharedSettingStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": true}`))
	ops := NewOperationalSettings(sharedSettingStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("ops refresh: %v", err)
	}
	f.srv.SetOperationalSettings(ops)

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	reader := msgAuthzAgent(t, f.store, "read-reader", f.projectA, store.MessageModeHub,
		[]string{f.ownerA.ID})
	peer := msgAuthzAgent(t, f.store, "read-peer", f.projectB, store.MessageModeHub,
		[]string{f.ownerB.ID})

	// Verify read access allowed when enabled.
	allowed, reason := f.srv.EvaluateCrossProjectReadAccess(ctx, reader, peer)
	if !allowed {
		t.Fatalf("expected read access allowed: %s", reason)
	}

	// Disable in store without cache refresh.
	sharedSettingStore.seed("messaging", json.RawMessage(`{"cross_project_messaging_enabled": false}`))

	// Read access should be denied immediately.
	allowed, reason = f.srv.EvaluateCrossProjectReadAccess(ctx, reader, peer)
	if allowed {
		t.Fatal("expected read access denied after store disable, even with stale cache")
	}
	if reason != "cross_project_disabled" {
		t.Fatalf("expected reason cross_project_disabled, got %s", reason)
	}
}

// ---------------------------------------------------------------------------
// Test helpers: failing store implementations
// ---------------------------------------------------------------------------

// allFailingHubSettingStore is a HubSettingStore that returns an error on all reads.
type allFailingHubSettingStore struct {
	err error
}

func (f *allFailingHubSettingStore) GetHubSetting(_ context.Context, _ string) (*store.HubSetting, error) {
	return nil, f.err
}

func (f *allFailingHubSettingStore) ListHubSettings(_ context.Context) ([]store.HubSetting, error) {
	return nil, f.err
}

func (f *allFailingHubSettingStore) UpsertHubSetting(_ context.Context, _ string, _ json.RawMessage, _ string, _ int64, _ string) (*store.HubSetting, error) {
	return nil, f.err
}

func (f *allFailingHubSettingStore) DeleteHubSetting(_ context.Context, _ string) error {
	return f.err
}

func (f *allFailingHubSettingStore) BackfillOrigin(_ context.Context) error {
	return f.err
}
