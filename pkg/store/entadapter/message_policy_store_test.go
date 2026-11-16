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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/messagepolicydecision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func sampleMessagePolicy(projectID, name string) *store.MessagePolicy {
	return &store.MessagePolicy{
		ProjectID:   projectID,
		Name:        name,
		Description: "Require structured choice prompt",
		Enabled:     true,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{
				store.MessagePolicySurfaceAgentToUser,
				store.MessagePolicySurfaceAgentToConversation,
			},
			Sender: store.MessagePolicySenderSelector{
				AllAgents: true,
			},
			Recipient: store.MessagePolicyRecipientSelector{
				Kinds: []string{"user", "conversation"},
			},
		},
		ViolationRule: `!message.has_choices && message.body.matches("(?i)\\bchoose\\b")`,
		OnFailed:      store.MessagePolicyOutcomeAdmit,
		OnFailedReasons: map[string]string{
			"cost_limit": store.MessagePolicyOutcomeReject,
		},
		RejectReason:      "Policy {policy} blocked prose choice to {recipient}",
		RejectRemediation: "Resend using scion message --choices.",
		CreatedBy:         "user-author-1",
		UpdatedBy:         "user-author-1",
	}
}

func TestMessagePolicyStore_CreateGetDelete(t *testing.T) {
	s := NewMessagePolicyStore(enttest.NewClient(t))
	ctx := context.Background()
	projectID := uuid.NewString()

	pol := sampleMessagePolicy(projectID, "no-prose-choices")
	require.NoError(t, s.CreateMessagePolicy(ctx, pol))

	require.NotEmpty(t, pol.ID)
	_, err := uuid.Parse(pol.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessagePolicyModeShadow, pol.Mode, "empty mode must default to shadow")
	assert.Equal(t, int64(1), pol.Revision)
	assert.False(t, pol.CreatedAt.IsZero())
	assert.False(t, pol.UpdatedAt.IsZero())
	assert.Equal(t, time.UTC, pol.CreatedAt.Location())
	assert.Equal(t, time.UTC, pol.UpdatedAt.Location())

	// Mutating caller slices/maps after Create must not affect persisted state.
	pol.Scope.Surfaces[0] = "mutated"
	pol.OnFailedReasons["cost_limit"] = store.MessagePolicyOutcomeAdmit

	byName, err := s.GetMessagePolicyByName(ctx, projectID, "no-prose-choices")
	require.NoError(t, err)
	assert.Equal(t, pol.ID, byName.ID)
	assert.Equal(t, projectID, byName.ProjectID)
	assert.Equal(t, "no-prose-choices", byName.Name)
	assert.Equal(t, store.MessagePolicyModeShadow, byName.Mode)
	assert.Equal(t, []string{
		store.MessagePolicySurfaceAgentToUser,
		store.MessagePolicySurfaceAgentToConversation,
	}, byName.Scope.Surfaces)
	assert.Equal(t, map[string]string{"cost_limit": store.MessagePolicyOutcomeReject}, byName.OnFailedReasons)
	assert.Equal(t, int64(1), byName.Revision)

	byID, err := s.GetMessagePolicy(ctx, pol.ID)
	require.NoError(t, err)
	assert.Equal(t, byName, byID)

	require.NoError(t, s.DeleteMessagePolicy(ctx, pol.ID))
	_, err = s.GetMessagePolicy(ctx, pol.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	err = s.DeleteMessagePolicy(ctx, pol.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetMessagePolicy(ctx, "not-a-uuid")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetMessagePolicyByName(ctx, "not-a-uuid", "no-prose-choices")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestMessagePolicyStore_ValidationAndUniqueness(t *testing.T) {
	s := NewMessagePolicyStore(enttest.NewClient(t))
	ctx := context.Background()
	projA := uuid.NewString()
	projB := uuid.NewString()

	cases := []struct {
		name   string
		mutate func(*store.MessagePolicy)
	}{
		{"invalid_project_id", func(p *store.MessagePolicy) { p.ProjectID = "bad" }},
		{"nil_project_id", func(p *store.MessagePolicy) { p.ProjectID = uuid.Nil.String() }},
		{"invalid_slug", func(p *store.MessagePolicy) { p.Name = "Not_Slug" }},
		{"invalid_mode", func(p *store.MessagePolicy) { p.Mode = "audit" }},
		{"empty_surfaces", func(p *store.MessagePolicy) { p.Scope.Surfaces = nil }},
		{"empty_violation_rule", func(p *store.MessagePolicy) { p.ViolationRule = "" }},
		{"oversized_violation_rule", func(p *store.MessagePolicy) {
			p.ViolationRule = strings.Repeat("a", store.MaxMessagePolicyViolationRuleBytes+1)
		}},
		{"oversized_system_one_rule", func(p *store.MessagePolicy) {
			p.SystemOneRule = strings.Repeat("a", store.MaxMessagePolicySystemOneRuleBytes+1)
		}},
		{"missing_on_failed", func(p *store.MessagePolicy) { p.OnFailed = "" }},
		{"invalid_on_failed", func(p *store.MessagePolicy) { p.OnFailed = "retry" }},
		{"invalid_on_uncertain", func(p *store.MessagePolicy) { p.OnUncertain = "maybe" }},
		{"invalid_on_failed_reasons", func(p *store.MessagePolicy) {
			p.OnFailedReasons = map[string]string{"timeout": "ignore"}
		}},
		{"oversized_reject_reason", func(p *store.MessagePolicy) {
			p.RejectReason = strings.Repeat("x", store.MaxMessagePolicyTemplateBytes+1)
		}},
		{"oversized_reject_remediation", func(p *store.MessagePolicy) {
			p.RejectRemediation = strings.Repeat("x", store.MaxMessagePolicyTemplateBytes+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sampleMessagePolicy(projA, "valid-name")
			tc.mutate(p)
			err := s.CreateMessagePolicy(ctx, p)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}

	first := sampleMessagePolicy(projA, "shared-slug")
	require.NoError(t, s.CreateMessagePolicy(ctx, first))

	dupSameProj := sampleMessagePolicy(projA, "shared-slug")
	err := s.CreateMessagePolicy(ctx, dupSameProj)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)

	sameSlugOtherProj := sampleMessagePolicy(projB, "shared-slug")
	require.NoError(t, s.CreateMessagePolicy(ctx, sameSlugOtherProj))
}

func TestMessagePolicyStore_MaxPoliciesPerProject(t *testing.T) {
	s := NewMessagePolicyStore(enttest.NewClient(t))
	ctx := context.Background()
	projA := uuid.NewString()
	projB := uuid.NewString()

	var firstID string
	for i := 0; i < store.MaxMessagePoliciesPerProject; i++ {
		p := sampleMessagePolicy(projA, fmt.Sprintf("policy-%02d", i))
		require.NoError(t, s.CreateMessagePolicy(ctx, p))
		if i == 0 {
			firstID = p.ID
		}
	}

	over := sampleMessagePolicy(projA, "policy-overflow")
	err := s.CreateMessagePolicy(ctx, over)
	assert.ErrorIs(t, err, store.ErrQuotaExceeded)

	// Another project is unaffected by projA's cap.
	other := sampleMessagePolicy(projB, "policy-00")
	require.NoError(t, s.CreateMessagePolicy(ctx, other))

	// Deleting one policy in projA frees a slot.
	require.NoError(t, s.DeleteMessagePolicy(ctx, firstID))
	require.NoError(t, s.CreateMessagePolicy(ctx, over))
}

func TestMessagePolicyStore_ListAndPagination(t *testing.T) {
	s := NewMessagePolicyStore(enttest.NewClient(t))
	ctx := context.Background()
	projID := uuid.NewString()

	specs := []struct {
		name    string
		mode    string
		enabled bool
	}{
		{"charlie", store.MessagePolicyModeShadow, true},
		{"alpha", store.MessagePolicyModeEnforce, true},
		{"delta", store.MessagePolicyModeEnforce, false},
		{"bravo", store.MessagePolicyModeShadow, true},
	}
	for _, sp := range specs {
		p := sampleMessagePolicy(projID, sp.name)
		p.Mode = sp.mode
		p.Enabled = sp.enabled
		require.NoError(t, s.CreateMessagePolicy(ctx, p))
	}

	// Paginate 2 at a time in deterministic (name ASC, id ASC) order.
	var names []string
	cursor := ""
	for {
		page, err := s.ListMessagePolicies(ctx, store.MessagePolicyFilter{ProjectID: projID}, store.ListOptions{
			Limit:  2,
			Cursor: cursor,
		})
		require.NoError(t, err)
		assert.Equal(t, 4, page.TotalCount)
		for _, item := range page.Items {
			names = append(names, item.Name)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, []string{"alpha", "bravo", "charlie", "delta"}, names)

	// Filter by Enabled=true and Mode=enforce.
	tr := true
	filtered, err := s.ListMessagePolicies(ctx, store.MessagePolicyFilter{
		ProjectID: projID,
		Enabled:   &tr,
		Mode:      store.MessagePolicyModeEnforce,
	}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, filtered.Items, 1)
	assert.Equal(t, "alpha", filtered.Items[0].Name)

	// Invalid cursor wraps ErrInvalidCursor.
	_, err = s.ListMessagePolicies(ctx, store.MessagePolicyFilter{ProjectID: projID}, store.ListOptions{Cursor: "!!!bad"})
	assert.ErrorIs(t, err, store.ErrInvalidCursor)
}

func TestMessagePolicyStore_UpdateAndRevisionConflict(t *testing.T) {
	s := NewMessagePolicyStore(enttest.NewClient(t))
	ctx := context.Background()
	projID := uuid.NewString()

	pol := sampleMessagePolicy(projID, "gate-choices")
	pol.SystemOneRefs = []string{"typesafe-prod"}
	require.NoError(t, s.CreateMessagePolicy(ctx, pol))
	assert.Equal(t, int64(1), pol.Revision)

	// Every edit bumps Revision by 1 (consumer design §5.3).
	pol.Mode = store.MessagePolicyModeEnforce
	pol.Description = "Promoted to enforce"
	pol.SystemOneRefs = nil
	pol.OnFailedReasons = nil
	require.NoError(t, s.UpdateMessagePolicy(ctx, pol))
	assert.Equal(t, int64(2), pol.Revision)

	got, err := s.GetMessagePolicy(ctx, pol.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Revision)
	assert.Equal(t, store.MessagePolicyModeEnforce, got.Mode)
	assert.Equal(t, "Promoted to enforce", got.Description)
	assert.Empty(t, got.SystemOneRefs)
	assert.Empty(t, got.OnFailedReasons)

	// Stale revision fails with ErrRevisionConflict.
	stale := got.Clone()
	stale.Revision = 1
	stale.Description = "stale write"
	err = s.UpdateMessagePolicy(ctx, stale)
	assert.ErrorIs(t, err, store.ErrRevisionConflict)

	// Update by (ProjectID, Name) with Revision=0 (unconditional) bumps to 3.
	byNameUpd := got.Clone()
	byNameUpd.ID = ""
	byNameUpd.Revision = 0
	byNameUpd.Enabled = false
	require.NoError(t, s.UpdateMessagePolicy(ctx, byNameUpd))
	assert.Equal(t, int64(3), byNameUpd.Revision)

	// Changing ProjectID is rejected.
	wrongProj := byNameUpd.Clone()
	wrongProj.ProjectID = uuid.NewString()
	err = s.UpdateMessagePolicy(ctx, wrongProj)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestMessagePolicyStore_WithTxAndDeleteProjectCascade(t *testing.T) {
	client := enttest.NewClient(t)
	composite := NewCompositeStore(client)
	ctx := context.Background()

	projA := &store.Project{ID: uuid.NewString(), Name: "Project A", Slug: "project-a"}
	projB := &store.Project{ID: uuid.NewString(), Name: "Project B", Slug: "project-b"}
	require.NoError(t, composite.CreateProject(ctx, projA))
	require.NoError(t, composite.CreateProject(ctx, projB))

	// Rollback inside WithTx leaves nothing persisted.
	boom := errors.New("rollback sentinel")
	err := composite.WithTx(ctx, func(tx store.Store) error {
		p := sampleMessagePolicy(projA.ID, "rolled-back")
		if err := tx.CreateMessagePolicy(ctx, p); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	_, err = composite.GetMessagePolicyByName(ctx, projA.ID, "rolled-back")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Seed policies and decisions in both projects.
	polA := sampleMessagePolicy(projA.ID, "keep-until-delete")
	polB := sampleMessagePolicy(projB.ID, "survives")
	require.NoError(t, composite.CreateMessagePolicy(ctx, polA))
	require.NoError(t, composite.CreateMessagePolicy(ctx, polB))

	require.NoError(t, composite.CreateMessagePolicyDecision(ctx, &store.MessagePolicyDecision{
		ProjectID:      projA.ID,
		PolicyID:       polA.ID,
		PolicyName:     polA.Name,
		PolicyRevision: polA.Revision,
		Mode:           store.MessagePolicyModeEnforce,
		Surface:        store.MessagePolicySurfaceAgentToUser,
		Class:          store.MessagePolicyClassViolation,
		Action:         store.MessagePolicyActionRejected,
	}))
	require.NoError(t, composite.CreateMessagePolicyDecision(ctx, &store.MessagePolicyDecision{
		ProjectID:      projB.ID,
		PolicyID:       polB.ID,
		PolicyName:     polB.Name,
		PolicyRevision: polB.Revision,
		Mode:           store.MessagePolicyModeShadow,
		Surface:        store.MessagePolicySurfaceAgentToUser,
		Class:          store.MessagePolicyClassPass,
		Action:         store.MessagePolicyActionAdmitted,
	}))

	// Deleting projA cascades its message policies and decisions while sparing projB.
	require.NoError(t, composite.DeleteProject(ctx, projA.ID))

	listA, err := composite.ListMessagePolicies(ctx, store.MessagePolicyFilter{ProjectID: projA.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, listA.Items)

	decA, err := composite.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: projA.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, decA.Items)

	gotB, err := composite.GetMessagePolicy(ctx, polB.ID)
	require.NoError(t, err)
	assert.Equal(t, polB.ID, gotB.ID)

	decB, err := composite.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: projB.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, decB.Items, 1)
}

func TestMessagePolicyDecisionStore_ContentFreeAndListPagination(t *testing.T) {
	// Target 8: schema columns and store struct must never store message body/content/attachments.
	forbiddenSubstrings := []string{"body", "msg", "content", "text", "attachment", "payload"}
	for _, col := range messagepolicydecision.Columns {
		for _, bad := range forbiddenSubstrings {
			assert.NotContains(t, strings.ToLower(col), bad,
				"message_policy_decisions column %q must not hold message content", col)
		}
	}
	rt := reflect.TypeOf(store.MessagePolicyDecision{})
	for i := 0; i < rt.NumField(); i++ {
		fname := strings.ToLower(rt.Field(i).Name)
		for _, bad := range forbiddenSubstrings {
			assert.NotContains(t, fname, bad,
				"store.MessagePolicyDecision field %q must not hold message content", rt.Field(i).Name)
		}
	}

	s := NewMessagePolicyStore(enttest.NewClient(t))
	ctx := context.Background()
	projID := uuid.NewString()
	polA := uuid.NewString()
	polB := uuid.NewString()
	msgID := uuid.NewString()
	base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	// Rejected row before message persistence: MessageID is empty (stored as NULL).
	d1 := &store.MessagePolicyDecision{
		ID:             "00000000-0000-0000-0000-000000000001",
		ProjectID:      projID,
		PolicyID:       polA,
		PolicyName:     "no-prose-choices",
		PolicyRevision: 2,
		Mode:           store.MessagePolicyModeEnforce,
		Surface:        store.MessagePolicySurfaceAgentToUser,
		SenderKind:     "agent",
		SenderID:       "agent-1",
		RecipientKind:  "user",
		RecipientID:    "user-1",
		MessageID:      "",
		Class:          store.MessagePolicyClassViolation,
		Action:         store.MessagePolicyActionRejected,
		Reason:         "violation_matched",
		ElapsedMs:      3,
		CreatedAt:      base,
	}
	// Tie on CreatedAt with higher UUID so newest-first (created_at DESC, id DESC) orders d2 before d1.
	d2 := &store.MessagePolicyDecision{
		ID:             "00000000-0000-0000-0000-000000000002",
		ProjectID:      projID,
		PolicyID:       polA,
		PolicyName:     "no-prose-choices",
		PolicyRevision: 2,
		Mode:           store.MessagePolicyModeShadow,
		Surface:        store.MessagePolicySurfaceAgentToConversation,
		SenderKind:     "agent",
		SenderID:       "agent-1",
		RecipientKind:  "conversation",
		RecipientID:    "conv-1",
		MessageID:      msgID,
		Class:          store.MessagePolicyClassFailed,
		Action:         store.MessagePolicyActionShadow,
		Reason:         "cel_failed",
		CELErrorReason: "cost_limit",
		Evaluation:     json.RawMessage(`{"dialect":"cel","reason":"cost_limit"}`),
		ElapsedMs:      11,
		CreatedAt:      base,
	}
	d3 := &store.MessagePolicyDecision{
		ID:             "00000000-0000-0000-0000-000000000003",
		ProjectID:      projID,
		PolicyID:       polB,
		PolicyName:     "other-policy",
		PolicyRevision: 1,
		Mode:           store.MessagePolicyModeEnforce,
		Surface:        store.MessagePolicySurfaceAgentToUser,
		SenderKind:     "agent",
		SenderID:       "agent-2",
		RecipientKind:  "user",
		RecipientID:    "user-1",
		MessageID:      msgID,
		Class:          store.MessagePolicyClassPass,
		Action:         store.MessagePolicyActionAdmitted,
		ElapsedMs:      1,
		CreatedAt:      base.Add(time.Minute),
	}

	require.NoError(t, s.CreateMessagePolicyDecision(ctx, d1))
	require.NoError(t, s.CreateMessagePolicyDecision(ctx, d2))
	require.NoError(t, s.CreateMessagePolicyDecision(ctx, d3))

	// Page 1 (Limit=2): d3, then d2.
	p1, err := s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: projID}, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, 3, p1.TotalCount)
	require.Len(t, p1.Items, 2)
	assert.Equal(t, d3.ID, p1.Items[0].ID)
	assert.Equal(t, d2.ID, p1.Items[1].ID)
	assert.Equal(t, msgID, p1.Items[1].MessageID)
	assert.Equal(t, "cost_limit", p1.Items[1].CELErrorReason)
	assert.JSONEq(t, `{"dialect":"cel","reason":"cost_limit"}`, string(p1.Items[1].Evaluation))
	require.NotEmpty(t, p1.NextCursor)

	// Page 2: d1 (with empty MessageID).
	p2, err := s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: projID}, store.ListOptions{
		Limit:  2,
		Cursor: p1.NextCursor,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, p2.TotalCount)
	require.Len(t, p2.Items, 1)
	assert.Equal(t, d1.ID, p2.Items[0].ID)
	assert.Empty(t, p2.Items[0].MessageID)
	assert.Empty(t, p2.NextCursor)

	// Filter by PolicyID, Mode, Surface, Class, Action.
	filtered, err := s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: projID,
		PolicyID:  polA,
		Mode:      store.MessagePolicyModeShadow,
		Surface:   store.MessagePolicySurfaceAgentToConversation,
		Class:     store.MessagePolicyClassFailed,
		Action:    store.MessagePolicyActionShadow,
	}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, filtered.Items, 1)
	assert.Equal(t, d2.ID, filtered.Items[0].ID)

	// Invalid cursor wraps ErrInvalidCursor.
	_, err = s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: projID}, store.ListOptions{Cursor: "not-base64!!!"})
	assert.ErrorIs(t, err, store.ErrInvalidCursor)
}

func TestMessagePolicyDecisionStore_Trim(t *testing.T) {
	s := NewMessagePolicyStore(enttest.NewClient(t))
	s.decisionTrimPassLimit = 3 // force multi-pass bounded trim
	ctx := context.Background()

	projA := uuid.NewString()
	projB := uuid.NewString()
	polID := uuid.NewString()
	now := time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)

	insertAt := func(proj string, createdAt time.Time) string {
		d := &store.MessagePolicyDecision{
			ProjectID:      proj,
			PolicyID:       polID,
			PolicyName:     "trim-policy",
			PolicyRevision: 1,
			Mode:           store.MessagePolicyModeShadow,
			Surface:        store.MessagePolicySurfaceAgentToUser,
			Class:          store.MessagePolicyClassPass,
			Action:         store.MessagePolicyActionAdmitted,
			CreatedAt:      createdAt,
		}
		require.NoError(t, s.CreateMessagePolicyDecision(ctx, d))
		return d.ID
	}

	// 2 aged rows (> 7 days old) across projA and projB.
	agedA := insertAt(projA, now.Add(-8*24*time.Hour))
	agedB := insertAt(projB, now.Add(-9*24*time.Hour))

	// 5 fresh rows in projA (cap will be 2 -> 3 overflow) and 2 fresh rows in projB (under cap).
	var freshA []string
	for i := 0; i < 5; i++ {
		freshA = append(freshA, insertAt(projA, now.Add(-time.Duration(5-i)*time.Hour)))
	}
	freshB1 := insertAt(projB, now.Add(-2*time.Hour))
	freshB2 := insertAt(projB, now.Add(-1*time.Hour))

	// Pass 1 (budget=3): deletes 2 aged rows + 1 oldest overflow row from projA; More=true.
	pass1, err := s.TrimMessagePolicyDecisions(ctx, now, store.DefaultMessagePolicyDecisionMaxAge, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, pass1.AgedDeleted)
	assert.Equal(t, 1, pass1.OverflowDeleted)
	assert.Equal(t, 3, pass1.TotalDeleted())
	assert.True(t, pass1.More)

	// Pass 2 (budget=3): deletes remaining 2 overflow rows from projA; More=false.
	pass2, err := s.TrimMessagePolicyDecisions(ctx, now, store.DefaultMessagePolicyDecisionMaxAge, 2)
	require.NoError(t, err)
	assert.Equal(t, 0, pass2.AgedDeleted)
	assert.Equal(t, 2, pass2.OverflowDeleted)
	assert.False(t, pass2.More)

	// Verify projA retains only the 2 newest fresh rows; agedA and oldest 3 freshA are gone.
	remA, err := s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: projA}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, remA.Items, 2)
	assert.Equal(t, freshA[4], remA.Items[0].ID)
	assert.Equal(t, freshA[3], remA.Items[1].ID)
	assert.NotEqual(t, agedA, remA.Items[0].ID)

	// Verify projB lost agedB and retained both fresh rows untouched.
	remB, err := s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: projB}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, remB.Items, 2)
	assert.Equal(t, freshB2, remB.Items[0].ID)
	assert.Equal(t, freshB1, remB.Items[1].ID)
	assert.NotEqual(t, agedB, remB.Items[0].ID)
}
