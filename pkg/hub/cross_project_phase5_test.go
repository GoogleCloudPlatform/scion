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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// p5CreateConv creates a direct conversation with two agent participants.
func p5CreateConv(t *testing.T, s store.Store, name string, agentAID, agentBID string) *store.Conversation {
	t.Helper()
	now := time.Now().UTC()
	extRef, err := messages.DMConversationKey("agent", agentAID, "agent", agentBID)
	require_NoError(t, err)
	conv := &store.Conversation{
		ID:             tid(name),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require_NoError(t, s.CreateConversation(context.Background(), conv))
	addConvParticipant(t, s, conv.ID, "agent", agentAID)
	addConvParticipant(t, s, conv.ID, "agent", agentBID)
	return conv
}

// ---------------------------------------------------------------------------
// Phase 5 test fixture — reuses existing crossProjectSetup infrastructure
// ---------------------------------------------------------------------------

type phase5Fixture struct {
	crossProjectFixture
	hubAgentA     *store.Agent
	hubAgentB     *store.Agent
	projectAgentA *store.Agent
	projectAgentB *store.Agent
	branchAgentB  *store.Agent
}

func phase5Setup(t *testing.T) phase5Fixture {
	t.Helper()
	base := crossProjectSetup(t)
	ctx := context.Background()
	s := base.store

	// Create agents with specific modes for testing.
	hubAgentA := &store.Agent{
		ID:          tid("p5-hub-a"),
		Slug:        "hub-agent-a",
		Name:        "Hub Agent A",
		ProjectID:   base.projectA,
		MessageMode: store.MessageModeHub,
		Ancestry:    []string{base.ownerA.ID},
		Phase:       "running",
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	projectAgentA := &store.Agent{
		ID:          tid("p5-proj-a"),
		Slug:        "proj-agent-a",
		Name:        "Project Agent A",
		ProjectID:   base.projectA,
		MessageMode: store.MessageModeProject,
		Ancestry:    []string{base.ownerA.ID},
		Phase:       "running",
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	hubAgentB := &store.Agent{
		ID:          tid("p5-hub-b"),
		Slug:        "hub-agent-b",
		Name:        "Hub Agent B",
		ProjectID:   base.projectB,
		MessageMode: store.MessageModeHub,
		Ancestry:    []string{base.ownerB.ID},
		Phase:       "running",
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	projectAgentB := &store.Agent{
		ID:          tid("p5-proj-b"),
		Slug:        "proj-agent-b",
		Name:        "Project Agent B",
		ProjectID:   base.projectB,
		MessageMode: store.MessageModeProject,
		Ancestry:    []string{base.ownerB.ID},
		Phase:       "running",
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	branchAgentB := &store.Agent{
		ID:          tid("p5-branch-b"),
		Slug:        "branch-agent-b",
		Name:        "Branch Agent B",
		ProjectID:   base.projectB,
		MessageMode: store.MessageModeBranch,
		Ancestry:    []string{base.ownerB.ID},
		Phase:       "running",
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	for _, a := range []*store.Agent{hubAgentA, projectAgentA, hubAgentB, projectAgentB, branchAgentB} {
		require_NoError(t, s.CreateAgent(ctx, a))
	}

	enableCrossProjectMessaging(t, base.srv)

	// Set both projects to accept cross-project messages from any agent.
	_, err := s.UpdateProjectMessagingPolicy(ctx, base.projectA, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)
	_, err = s.UpdateProjectMessagingPolicy(ctx, base.projectB, store.CrossProjectInboundAny, 1)
	require_NoError(t, err)

	return phase5Fixture{
		crossProjectFixture: base,
		hubAgentA:           hubAgentA,
		hubAgentB:           hubAgentB,
		projectAgentA:       projectAgentA,
		projectAgentB:       projectAgentB,
		branchAgentB:        branchAgentB,
	}
}

// ---------------------------------------------------------------------------
// D1: Multi-target DM fan-out tests
// ---------------------------------------------------------------------------

func TestParseQualifiedAgentRef(t *testing.T) {
	tests := []struct {
		input       string
		projectSlug string
		agentSlug   string
	}{
		{"@project-alpha/hub-agent-a", "project-alpha", "hub-agent-a"},
		{"project-alpha/hub-agent-a", "project-alpha", "hub-agent-a"},
		{"@hub-agent-a", "", "hub-agent-a"},
		{"hub-agent-a", "", "hub-agent-a"},
		{"@my-proj/my-agent", "my-proj", "my-agent"},
	}
	for _, tt := range tests {
		ref, err := ParseQualifiedAgentRef(tt.input)
		if err != nil {
			t.Errorf("ParseQualifiedAgentRef(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if ref.ProjectSlug != tt.projectSlug {
			t.Errorf("ParseQualifiedAgentRef(%q).ProjectSlug = %q, want %q", tt.input, ref.ProjectSlug, tt.projectSlug)
		}
		if ref.AgentSlug != tt.agentSlug {
			t.Errorf("ParseQualifiedAgentRef(%q).AgentSlug = %q, want %q", tt.input, ref.AgentSlug, tt.agentSlug)
		}
	}

	// O3: Malformed inputs should return errors.
	malformedCases := []string{"", "@", "@/agent", "@project/", "/"}
	for _, input := range malformedCases {
		_, err := ParseQualifiedAgentRef(input)
		if err == nil {
			t.Errorf("ParseQualifiedAgentRef(%q) expected error for malformed input", input)
		}
	}
}

func TestResolveFanOutTargets_Deduplication(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	refs := []QualifiedAgentRef{
		{AgentSlug: "hub-agent-a"},
		{AgentSlug: "hub-agent-a"}, // duplicate
		{AgentSlug: "proj-agent-a"},
	}

	targets, err := f.srv.ResolveFanOutTargets(ctx, refs, f.projectA)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Errorf("expected 2 deduplicated targets, got %d", len(targets))
	}
}

func TestResolveFanOutTargets_ExceedsLimit(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	refs := make([]QualifiedAgentRef, MaxFanOutTargets+1)
	for i := range refs {
		refs[i] = QualifiedAgentRef{AgentSlug: "agent"}
	}

	_, err := f.srv.ResolveFanOutTargets(ctx, refs, f.projectA)
	if err == nil {
		t.Error("expected error for exceeding fan-out limit")
	}
}

func TestResolveFanOutTargets_CrossProjectRef(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	refs := []QualifiedAgentRef{
		{ProjectSlug: "project-b", AgentSlug: "hub-agent-b"},
	}

	targets, err := f.srv.ResolveFanOutTargets(ctx, refs, f.projectA)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].Agent == nil {
		t.Fatal("expected target agent to be resolved")
	}
	if targets[0].Agent.ID != f.hubAgentB.ID {
		t.Errorf("expected target agent ID %q, got %q", f.hubAgentB.ID, targets[0].Agent.ID)
	}
}

func TestEvaluateFanOutTargets_MixedAuthorizedDenied(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Sender is hub-mode agent in project A.
	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	targets := []FanOutTarget{
		{Agent: f.hubAgentB, ProjectID: f.projectB, AgentSlug: "hub-agent-b"},
		{Agent: f.branchAgentB, ProjectID: f.projectB, AgentSlug: "branch-agent-b"},
	}

	result := f.srv.EvaluateFanOutTargets(ctx, senderIdent, targets)

	// hub-agent-b should be allowed (hub -> hub cross-project with policy "any").
	if !result.Targets[0].Decision.Allowed {
		t.Errorf("hub-agent-b should be allowed, got denied: %s", result.Targets[0].Decision.Reason)
	}
	if !result.Targets[0].Delivered {
		t.Error("hub-agent-b should be marked as Delivered")
	}

	// branch-agent-b should be denied (cross-project target must be project/hub).
	if result.Targets[1].Decision.Allowed {
		t.Error("branch-agent-b should be denied for cross-project")
	}
	if result.Targets[1].Decision.Code != MessageDenialCrossProjectTargetMode {
		t.Errorf("expected denial code %q, got %q",
			MessageDenialCrossProjectTargetMode, result.Targets[1].Decision.Code)
	}

	// R4: Verify the Delivered counter is incremented correctly.
	if result.Delivered != 1 {
		t.Errorf("expected Delivered = 1, got %d", result.Delivered)
	}
	if result.Denied != 1 {
		t.Errorf("expected Denied = 1, got %d", result.Denied)
	}
}

func TestEvaluateFanOutTargets_OnlyApprovedDelivered(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Sender is project-mode agent (cannot send cross-project).
	senderIdent := msgAuthzAgentIdentity(
		f.projectAgentA.ID, f.projectA, f.projectAgentA.Ancestry,
	)

	targets := []FanOutTarget{
		{Agent: f.projectAgentA, ProjectID: f.projectA, AgentSlug: "proj-agent-a"},
		{Agent: f.hubAgentB, ProjectID: f.projectB, AgentSlug: "hub-agent-b"},
	}

	result := f.srv.EvaluateFanOutTargets(ctx, senderIdent, targets)

	// Same-project target should be allowed.
	if !result.Targets[0].Decision.Allowed {
		t.Errorf("same-project target should be allowed, got denied: %s", result.Targets[0].Decision.Reason)
	}

	// Cross-project target should be denied (project-mode sender).
	if result.Targets[1].Decision.Allowed {
		t.Error("cross-project target should be denied for project-mode sender")
	}
	if result.Targets[1].Decision.Code != MessageDenialCrossProjectSenderMode {
		t.Errorf("expected denial code %q, got %q",
			MessageDenialCrossProjectSenderMode, result.Targets[1].Decision.Code)
	}
}

// ---------------------------------------------------------------------------
// D2: Group/broadcast/plugin boundary tests
// ---------------------------------------------------------------------------

func TestValidateCrossProjectGroupBoundary_SameProject(t *testing.T) {
	result := ValidateCrossProjectGroupBoundary("proj-a", "proj-a", "group message")
	if result != nil {
		t.Error("same-project group boundary should return nil")
	}
}

func TestValidateCrossProjectGroupBoundary_DifferentProject(t *testing.T) {
	result := ValidateCrossProjectGroupBoundary("proj-a", "proj-b", "group message")
	if result == nil {
		t.Fatal("cross-project group boundary should return denial")
	}
	if result.Code != MessageDenialCrossProjectGroupsUnsupported {
		t.Errorf("expected code %q, got %q", MessageDenialCrossProjectGroupsUnsupported, result.Code)
	}
}

func TestValidateCrossProjectRoomJoin_DifferentProject(t *testing.T) {
	result := ValidateCrossProjectRoomJoin("proj-a", "proj-b")
	if result == nil {
		t.Fatal("cross-project room join should return denial")
	}
	if result.Code != MessageDenialCrossProjectGroupsUnsupported {
		t.Errorf("expected code %q, got %q", MessageDenialCrossProjectGroupsUnsupported, result.Code)
	}
}

func TestValidateCrossProjectGroupBoundary_BroadcastDeniedEvenInHubMode(t *testing.T) {
	result := ValidateCrossProjectGroupBoundary("proj-a", "proj-b", "broadcast")
	if result == nil {
		t.Fatal("cross-project broadcast should return denial even in hub mode")
	}
	if result.Code != MessageDenialCrossProjectGroupsUnsupported {
		t.Errorf("expected code %q, got %q", MessageDenialCrossProjectGroupsUnsupported, result.Code)
	}
}

func TestValidateCrossProjectRoomJoin_SameProject(t *testing.T) {
	result := ValidateCrossProjectRoomJoin("proj-a", "proj-a")
	if result != nil {
		t.Error("same-project room join should return nil")
	}
}

// ---------------------------------------------------------------------------
// D3: Scheduled message cross-project tests
// ---------------------------------------------------------------------------

func TestScheduledMessageCrossProject_HubEnabled(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	evt := store.ScheduledEvent{
		ID:        tid("p5-evt-1"),
		ProjectID: f.projectA,
		CreatedBy: f.hubAgentA.ID,
	}

	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	err := f.srv.authorizeScheduledMessageCrossProject(ctx, evt, f.hubAgentB, senderIdent)
	if err != nil {
		t.Errorf("cross-project scheduled message should be allowed when hub is enabled: %v", err)
	}
}

func TestScheduledMessageCrossProject_TargetDeleted(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	evt := store.ScheduledEvent{
		ID:        tid("p5-evt-2"),
		ProjectID: f.projectA,
		CreatedBy: f.hubAgentA.ID,
	}

	deletedAgent := *f.hubAgentB
	now := time.Now()
	deletedAgent.DeletedAt = now

	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	err := f.srv.authorizeScheduledMessageCrossProject(ctx, evt, &deletedAgent, senderIdent)
	if err == nil {
		t.Error("should deny scheduled message to deleted target agent")
	}
}

func TestScheduledMessageCrossProject_ModeChanged(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	branchAgent := *f.hubAgentB
	branchAgent.MessageMode = store.MessageModeBranch

	evt := store.ScheduledEvent{
		ID:        tid("p5-evt-3"),
		ProjectID: f.projectA,
		CreatedBy: f.hubAgentA.ID,
	}

	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	err := f.srv.authorizeScheduledMessageCrossProject(ctx, evt, &branchAgent, senderIdent)
	if err == nil {
		t.Error("should deny when target mode changed to branch after scheduling")
	}
}

func TestScheduledMessageCrossProject_SameProject(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Same project — should pass without cross-project check.
	evt := store.ScheduledEvent{
		ID:        tid("p5-evt-4"),
		ProjectID: f.projectA,
		CreatedBy: f.hubAgentA.ID,
	}

	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	err := f.srv.authorizeScheduledMessageCrossProject(ctx, evt, f.projectAgentA, senderIdent)
	if err != nil {
		t.Errorf("same-project scheduled message should not trigger cross-project check: %v", err)
	}
}

func TestScheduledMessageCrossProject_MembershipRemoval(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Set project B to members-only policy. Revision was set to 2 by phase5Setup.
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 2)
	require_NoError(t, err)

	// ownerA is NOT a member of projectB, so the cross-project scheduled
	// message should be denied.
	evt := store.ScheduledEvent{
		ID:        tid("p5-evt-5"),
		ProjectID: f.projectA,
		CreatedBy: f.hubAgentA.ID,
	}

	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	err = f.srv.authorizeScheduledMessageCrossProject(ctx, evt, f.hubAgentB, senderIdent)
	if err == nil {
		t.Error("should deny cross-project scheduled message when origin user is not a member of target project")
	}
}

func TestScheduledMessageCrossProject_RetryReauthorized(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// First attempt succeeds.
	evt := store.ScheduledEvent{
		ID:        tid("p5-evt-6"),
		ProjectID: f.projectA,
		CreatedBy: f.hubAgentA.ID,
	}

	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	err := f.srv.authorizeScheduledMessageCrossProject(ctx, evt, f.hubAgentB, senderIdent)
	if err != nil {
		t.Fatalf("initial attempt should succeed: %v", err)
	}

	// Disable the hub between schedule and fire — retry should be denied.
	f.srv.SetOperationalSettings(nil)

	err = f.srv.authorizeScheduledMessageCrossProject(ctx, evt, f.hubAgentB, senderIdent)
	if err == nil {
		t.Error("retry should be denied when hub is disabled after scheduling")
	}
}

// ---------------------------------------------------------------------------
// D4: Cross-project attachment transfer tests
// ---------------------------------------------------------------------------

func TestAuthorizeAttachmentDownload_Participant(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Create a conversation with two participants.
	conv := p5CreateConv(t, f.store, "p5-conv-att-1", f.hubAgentA.ID, f.hubAgentB.ID)

	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	decision := f.srv.AuthorizeAttachmentDownload(ctx, senderIdent, "attach-1", conv.ID)
	if !decision.Allowed {
		t.Errorf("participant should be allowed to download attachment: %s", decision.Reason)
	}
}

func TestAuthorizeAttachmentDownload_NonParticipant(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Create a conversation without projectAgentA as participant.
	conv := p5CreateConv(t, f.store, "p5-conv-att-2", f.hubAgentA.ID, f.hubAgentB.ID)

	// requester is projectAgentA, who is NOT a participant.
	requesterIdent := msgAuthzAgentIdentity(
		f.projectAgentA.ID, f.projectA, f.projectAgentA.Ancestry,
	)

	decision := f.srv.AuthorizeAttachmentDownload(ctx, requesterIdent, "attach-2", conv.ID)
	if decision.Allowed {
		t.Error("non-participant should be denied attachment download")
	}
	if decision.Code != MessageDenialAttachmentUnauthorized {
		t.Errorf("expected code %q, got %q", MessageDenialAttachmentUnauthorized, decision.Code)
	}
}

func TestAuthorizeAttachmentDownload_AnotherDMsAttachmentDenied(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Conversation A: hubAgentA <-> hubAgentB
	convA := p5CreateConv(t, f.store, "p5-conv-att-a", f.hubAgentA.ID, f.hubAgentB.ID)

	// projectAgentB tries to access an attachment via convA (not their conversation).
	requesterIdent := msgAuthzAgentIdentity(
		f.projectAgentB.ID, f.projectB, f.projectAgentB.Ancestry,
	)

	decision := f.srv.AuthorizeAttachmentDownload(ctx, requesterIdent, "attach-from-conv-a", convA.ID)
	if decision.Allowed {
		t.Error("agent from a different DM should be denied access to another DM's attachment")
	}
}

func TestAuthorizeAttachmentDownload_StaleAccess(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Create conversation and add participant, then remove them.
	conv := p5CreateConv(t, f.store, "p5-conv-att-stale", f.hubAgentA.ID, f.hubAgentB.ID)

	// Remove hubAgentA from the conversation.
	require_NoError(t, f.store.RemoveParticipant(ctx, conv.ID, "agent", f.hubAgentA.ID))

	senderIdent := msgAuthzAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry,
	)

	decision := f.srv.AuthorizeAttachmentDownload(ctx, senderIdent, "attach-stale", conv.ID)
	if decision.Allowed {
		t.Error("removed participant should be denied stale attachment download")
	}
}

func TestRejectCrossProjectFileTransport(t *testing.T) {
	decision := RejectCrossProjectFileTransport("plugin_channel")
	if decision.Allowed {
		t.Error("cross-project file transport should be rejected")
	}
	if decision.Code != MessageDenialCrossProjectAttachUnsupported {
		t.Errorf("expected code %q, got %q", MessageDenialCrossProjectAttachUnsupported, decision.Code)
	}
}

// ---------------------------------------------------------------------------
// D5: Content surface authorization tests
// ---------------------------------------------------------------------------

func TestAuthorizeCrossProjectContentAccess_ParticipantAllowed(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Create a cross-project conversation.
	conv := p5CreateConv(t, f.store, "p5-xp-conv-1", f.hubAgentA.ID, f.hubAgentB.ID)

	// All surfaces should be accessible to participants.
	surfaces := []ContentSurfaceType{
		ContentSurfaceStreamEvents,
		ContentSurfaceSearch,
		ContentSurfacePreview,
		ContentSurfaceNotification,
		ContentSurfaceUnread,
		ContentSurfaceEditDelete,
		ContentSurfaceReceipt,
		ContentSurfaceTyping,
		ContentSurfaceAttachment,
	}

	for _, surface := range surfaces {
		decision := f.srv.AuthorizeCrossProjectContentAccess(ctx, f.hubAgentA.ID, conv.ID, surface)
		if !decision.Allowed {
			t.Errorf("participant should be allowed access on surface %q: %s", surface, decision.Reason)
		}
	}
}

func TestAuthorizeCrossProjectContentAccess_NonParticipantDenied(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Create a cross-project conversation without projectAgentA.
	conv := p5CreateConv(t, f.store, "p5-xp-conv-2", f.hubAgentA.ID, f.hubAgentB.ID)

	// Non-participant should get no DM payload, preview, attachment metadata,
	// unread signal, typing event, or search result.
	surfaces := []ContentSurfaceType{
		ContentSurfaceStreamEvents,
		ContentSurfaceSearch,
		ContentSurfacePreview,
		ContentSurfaceNotification,
		ContentSurfaceUnread,
		ContentSurfaceEditDelete,
		ContentSurfaceReceipt,
		ContentSurfaceTyping,
		ContentSurfaceAttachment,
	}

	for _, surface := range surfaces {
		decision := f.srv.AuthorizeCrossProjectContentAccess(ctx, f.projectAgentA.ID, conv.ID, surface)
		if decision.Allowed {
			t.Errorf("non-participant should be denied access on surface %q", surface)
		}
		if decision.Code != MessageDenialCrossProjectContentUnauthorized {
			t.Errorf("surface %q: expected code %q, got %q",
				surface, MessageDenialCrossProjectContentUnauthorized, decision.Code)
		}
	}
}

func TestAuthorizeCrossProjectContentAccess_NoConversation(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	decision := f.srv.AuthorizeCrossProjectContentAccess(ctx, f.hubAgentA.ID, "", ContentSurfaceSearch)
	if !decision.Allowed {
		t.Error("empty conversation ID should be allowed")
	}
}

func TestAuthorizeCrossProjectContentAccess_SameProjectConversation(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Create a same-project conversation.
	conv := p5CreateConv(t, f.store, "p5-sp-conv-1", f.hubAgentA.ID, f.projectAgentA.ID)

	// Same-project conversations should always be allowed, even for non-participants.
	decision := f.srv.AuthorizeCrossProjectContentAccess(ctx, f.hubAgentB.ID, conv.ID, ContentSurfaceSearch)
	if !decision.Allowed {
		t.Error("same-project conversation should be accessible")
	}
}

func TestReauthorizeSubscription_AfterRevocation(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	// Create cross-project conversation.
	conv := p5CreateConv(t, f.store, "p5-xp-reauth", f.hubAgentA.ID, f.hubAgentB.ID)

	// Initially should be authorized.
	if !f.srv.ReauthorizeSubscription(ctx, f.hubAgentA.ID, conv.ID) {
		t.Error("participant should pass reauthorization")
	}

	// Non-participant should fail reauthorization on cross-project conversation.
	if f.srv.ReauthorizeSubscription(ctx, f.projectAgentA.ID, conv.ID) {
		t.Error("non-participant should fail reauthorization")
	}

	// Disabling the Hub feature should fail reauthorization even for participants.
	f.srv.SetOperationalSettings(nil)

	if f.srv.ReauthorizeSubscription(ctx, f.hubAgentA.ID, conv.ID) {
		t.Error("participant should fail reauthorization when hub is disabled")
	}
}

// ---------------------------------------------------------------------------
// D6: Legacy view compatibility tests
// ---------------------------------------------------------------------------

func TestClassifyLegacyViewQuery_CrossProject(t *testing.T) {
	senderProjID := "proj-a"
	recipProjID := "proj-b"
	msg := &store.Message{
		SenderProjectID:    &senderProjID,
		RecipientProjectID: &recipProjID,
	}
	mode := ClassifyLegacyViewQuery(msg)
	if mode != LegacyViewCanonical {
		t.Errorf("expected canonical mode for cross-project message, got %q", mode)
	}
}

func TestClassifyLegacyViewQuery_SameProject(t *testing.T) {
	projID := "proj-a"
	msg := &store.Message{
		SenderProjectID:    &projID,
		RecipientProjectID: &projID,
	}
	mode := ClassifyLegacyViewQuery(msg)
	if mode != LegacyViewLegacy {
		t.Errorf("expected legacy mode for same-project message without conversation, got %q", mode)
	}
}

func TestClassifyLegacyViewQuery_WithConversation(t *testing.T) {
	msg := &store.Message{
		ConversationID: "conv-123",
	}
	mode := ClassifyLegacyViewQuery(msg)
	if mode != LegacyViewCanonical {
		t.Errorf("expected canonical mode for message with conversation ID, got %q", mode)
	}
}

func TestClassifyLegacyViewQuery_Nil(t *testing.T) {
	mode := ClassifyLegacyViewQuery(nil)
	if mode != LegacyViewLegacy {
		t.Errorf("expected legacy mode for nil message, got %q", mode)
	}
}

func TestValidateLegacyViewClaims_LocalSenderDenied(t *testing.T) {
	result := ValidateLegacyViewClaims("proj-a", true, false)
	if result == nil {
		t.Fatal("should reject local sender claim on cross-project content")
	}
	if result.Code != MessageDenialCrossProjectUnsupported {
		t.Errorf("expected code %q, got %q", MessageDenialCrossProjectUnsupported, result.Code)
	}
}

func TestValidateLegacyViewClaims_SystemPlaneDenied(t *testing.T) {
	result := ValidateLegacyViewClaims("proj-a", false, true)
	if result == nil {
		t.Fatal("should reject system-plane claim on cross-project content")
	}
	if result.Code != MessageDenialCrossProjectUnsupported {
		t.Errorf("expected code %q, got %q", MessageDenialCrossProjectUnsupported, result.Code)
	}
}

func TestValidateLegacyViewClaims_NoProject(t *testing.T) {
	result := ValidateLegacyViewClaims("", true, true)
	if result != nil {
		t.Error("should allow claims when no project context")
	}
}

func TestValidateLegacyViewClaims_NoClaimsAllowed(t *testing.T) {
	result := ValidateLegacyViewClaims("proj-a", false, false)
	if result != nil {
		t.Error("should allow when no claims made")
	}
}

// ---------------------------------------------------------------------------
// D7: Delivery deduplication tests
// ---------------------------------------------------------------------------

func TestDeliveryDeduplicationCheck_NoMessage(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	if f.srv.DeliveryDeduplicationCheck(ctx, "nonexistent-msg-id") {
		t.Error("non-existent message should not be a duplicate")
	}
}

func TestDeliveryDeduplicationCheck_EmptyID(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	if f.srv.DeliveryDeduplicationCheck(ctx, "") {
		t.Error("empty message ID should not be a duplicate")
	}
}

func TestDeliveryDeduplicationCheck_ExistingMessage(t *testing.T) {
	f := phase5Setup(t)
	ctx := context.Background()

	msg := &store.Message{
		ID:          tid("p5-dup-msg"),
		ProjectID:   f.projectA,
		Sender:      "agent:" + f.hubAgentA.Slug,
		SenderID:    f.hubAgentA.ID,
		Recipient:   "agent:" + f.hubAgentB.Slug,
		RecipientID: f.hubAgentB.ID,
		Msg:         "test message",
		AgentID:     f.hubAgentB.ID,
		CreatedAt:   time.Now(),
	}
	require_NoError(t, f.store.CreateMessage(ctx, msg))

	if !f.srv.DeliveryDeduplicationCheck(ctx, msg.ID) {
		t.Error("existing message should be detected as duplicate")
	}
}

// ---------------------------------------------------------------------------
// Audit entry tests (D7)
// ---------------------------------------------------------------------------

func TestLogCrossProjectDecision_DenialLogged(t *testing.T) {
	// Verify the function doesn't panic with various input combinations.
	entry := CrossProjectAuditEntry{
		Timestamp:        time.Now(),
		Action:           "deny",
		SenderID:         "agent-1",
		SenderProjectID:  "proj-a",
		RecipientID:      "agent-2",
		RecipientProject: "proj-b",
		DecisionCode:     MessageDenialCrossProjectDisabled,
		Reason:           "cross-project messaging is disabled",
		CrossProject:     true,
		HubRevision:      1,
		ProjectRevision:  2,
		CorrelationID:    "corr-123",
		Surface:          "mention_fanout",
		ConversationID:   "conv-456",
	}
	LogCrossProjectDecision(entry)

	// Minimal fields.
	LogCrossProjectDecision(CrossProjectAuditEntry{
		Timestamp: time.Now(),
		Action:    "deliver",
	})
}

func TestLogCrossProjectDecision_DeliveryLogged(t *testing.T) {
	LogCrossProjectDecision(CrossProjectAuditEntry{
		Timestamp:        time.Now(),
		Action:           "deliver",
		SenderID:         "agent-1",
		SenderProjectID:  "proj-a",
		RecipientID:      "agent-2",
		RecipientProject: "proj-b",
		CrossProject:     true,
	})
}

// ---------------------------------------------------------------------------
// R1: mapReasonToCode prefix ordering regression test
// ---------------------------------------------------------------------------

func TestMapReasonToCode_CrossProjectPrefixOrdering(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{
			reason: "cross-project messaging is not enabled on this Hub",
			want:   string(MessageDenialCrossProjectDisabled),
		},
		{
			reason: `cross-project messaging requires sender mode hub, got "project"`,
			want:   string(MessageDenialCrossProjectSenderMode),
		},
		{
			reason: "cross-project messaging requires hub-attested ancestry",
			want:   string(MessageDenialCrossProjectUntrusted),
		},
		{
			reason: `cross-project target must be in project or hub mode, got "branch"`,
			want:   string(MessageDenialCrossProjectTargetMode),
		},
		{
			reason: "cross-project something else unsupported",
			want:   string(MessageDenialCrossProjectUnsupported),
		},
	}
	for _, tt := range tests {
		got := mapReasonToCode(tt.reason)
		if got != tt.want {
			t.Errorf("mapReasonToCode(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}
