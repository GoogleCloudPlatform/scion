// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auditevent

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func accessBoundaryFullPayloadEntry(action string, destinations []Destination) CatalogEntry {
	return CatalogEntry{
		Family:                 "access_boundary",
		Action:                 action,
		AllowedPairs:           []PhaseOutcome{{Phase: PhaseCommit, Outcome: OutcomeSucceeded}},
		ResourceKind:           "access_constraint",
		RequiredEnvelopeLeaves: []string{"schema_version", "event_id", "occurred_at", "family", "action", "phase", "outcome", "severity", "correlation_id", "principal", "resource"},
		ResourceScopes: []ResourceScopeSchema{
			{Scope: ResourceScopeSystem, ProjectID: ResourceProjectIDOmitted},
			{Scope: ResourceScopeProject, ProjectID: ResourceProjectIDRequired},
		},
		RequiredPayloadLeaves: []PayloadLeafSchema{{
			Name:          "classification",
			Type:          PayloadString,
			MaxBytes:      9,
			AllowedValues: []string{"tighten", "relax", "mixed", "no_effect"},
		}},
		OptionalPayloadLeaves: []PayloadLeafSchema{
			{Name: "before_revision", Type: PayloadInt64},
			{Name: "after_revision", Type: PayloadInt64},
			{Name: "preview_id", Type: PayloadString, MaxBytes: 128},
			{Name: "draft_hash", Type: PayloadHexString, ExactLength: 64},
			{Name: "impact_counts", Type: PayloadImpactCounts},
			{Name: "changed_fields", Type: PayloadStringArray, MaxItems: 32, ItemMaxBytes: 256},
		},
		Destinations: destinations,
	}
}

func TestCatalogSnapshotAccessBoundaryHistoryOnlyAndDelete(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"update", "recovery_disable", "recovery_enable"} {
		entry, ok := catalogEntry("access_boundary", action)
		require.True(t, ok, action)
		assert.Equal(t, accessBoundaryFullPayloadEntry(action, []Destination{DestinationHistory}), entry, action)
	}

	deleteEntry, ok := catalogEntry("access_boundary", "delete")
	require.True(t, ok)
	assert.Equal(t, CatalogEntry{
		Family:                 "access_boundary",
		Action:                 "delete",
		AllowedPairs:           []PhaseOutcome{{Phase: PhaseCommit, Outcome: OutcomeSucceeded}},
		ResourceKind:           "access_constraint",
		RequiredEnvelopeLeaves: []string{"schema_version", "event_id", "occurred_at", "family", "action", "phase", "outcome", "severity", "correlation_id", "principal", "resource"},
		ResourceScopes: []ResourceScopeSchema{
			{Scope: ResourceScopeSystem, ProjectID: ResourceProjectIDOmitted},
			{Scope: ResourceScopeProject, ProjectID: ResourceProjectIDRequired},
		},
		RequiredPayloadLeaves: []PayloadLeafSchema{{
			Name:          "classification",
			Type:          PayloadString,
			MaxBytes:      9,
			AllowedValues: []string{"tighten", "relax", "mixed", "no_effect"},
		}},
		OptionalPayloadLeaves: []PayloadLeafSchema{{Name: "before_revision", Type: PayloadInt64}},
		Destinations:          []Destination{DestinationStructuredLog},
	}, deleteEntry)
}

func historyOnlyContext(t *testing.T) context.Context {
	t.Helper()
	operation, err := NewOperationContext("request-history-only")
	require.NoError(t, err)
	return ContextWithOperation(context.Background(), operation)
}

func TestAccessBoundaryHistoryOnlyBuilders(t *testing.T) {
	t.Parallel()

	before := int64(3)
	after := int64(4)
	for _, tc := range []struct {
		action         AccessBoundaryHistoryAction
		classification BoundaryClassification
	}{
		{action: AccessBoundaryUpdate, classification: BoundaryMixed},
		{action: AccessBoundaryRecoveryDisable, classification: BoundaryRelax},
		{action: AccessBoundaryRecoveryEnable, classification: BoundaryTighten},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			event, err := BuildAccessBoundaryHistory(historyOnlyContext(t), tc.action, AccessBoundaryHistoryInput{
				Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
				ConstraintID:   "constraint-1",
				Scope:          ResourceScopeProject,
				ProjectID:      "project-1",
				BeforeRevision: &before,
				AfterRevision:  &after,
				Classification: tc.classification,
			})
			require.NoError(t, err)
			require.NoError(t, Validate(event))

			assert.Equal(t, "access_boundary", event.Family)
			assert.Equal(t, string(tc.action), event.Action)
			assert.Equal(t, PhaseCommit, event.Phase)
			assert.Equal(t, OutcomeSucceeded, event.Outcome)
			assert.Equal(t, "request-history-only", event.CorrelationID)
			parsed, err := uuid.Parse(event.EventID)
			require.NoError(t, err)
			assert.Equal(t, parsed.String(), event.EventID, "event_id must be a canonical UUID")
			assert.False(t, event.OccurredAt.IsZero())
			assert.Equal(t, time.UTC, event.OccurredAt.Location())
			payload, ok := event.Payload.(AccessBoundaryPayload)
			require.True(t, ok)
			assert.Equal(t, tc.classification, payload.Classification)
			require.NotNil(t, payload.BeforeRevision)
			require.NotNil(t, payload.AfterRevision)
			assert.Equal(t, before, *payload.BeforeRevision)
			assert.Equal(t, after, *payload.AfterRevision)

			entry, ok := catalogEntry(event.Family, event.Action)
			require.True(t, ok)
			assert.Equal(t, []Destination{DestinationHistory}, entry.Destinations,
				"history-only actions declare no log destination")
		})
	}
}

func TestAccessBoundaryHistoryOnlyBuildersRejectInvalid(t *testing.T) {
	t.Parallel()

	valid := AccessBoundaryHistoryInput{
		Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
		ConstraintID:   "constraint-1",
		Scope:          ResourceScopeSystem,
		Classification: BoundaryRelax,
	}
	for _, action := range []AccessBoundaryHistoryAction{AccessBoundaryUpdate, AccessBoundaryRecoveryDisable, AccessBoundaryRecoveryEnable} {
		t.Run(string(action), func(t *testing.T) {
			_, err := BuildAccessBoundaryHistory(context.Background(), action, valid)
			require.Error(t, err, "missing operation context must be rejected")

			emptyPrincipal := valid
			emptyPrincipal.Principal = IdentityRef{Kind: IdentityUser}
			_, err = BuildAccessBoundaryHistory(historyOnlyContext(t), action, emptyPrincipal)
			require.Error(t, err, "empty principal ID must be rejected")

			badClassification := valid
			badClassification.Classification = "widen"
			_, err = BuildAccessBoundaryHistory(historyOnlyContext(t), action, badClassification)
			require.Error(t, err, "undeclared classification must be rejected")
		})
	}

	for _, action := range []AccessBoundaryHistoryAction{"create", "delete", "unknown"} {
		_, err := BuildAccessBoundaryHistory(historyOnlyContext(t), action, valid)
		require.Error(t, err, "%s is not a history-only action", action)
	}
}

func TestAccessBoundaryDeleteBuilder(t *testing.T) {
	t.Parallel()

	before := int64(7)
	event, err := BuildAccessBoundaryDelete(historyOnlyContext(t), AccessBoundaryDeleteInput{
		Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
		ConstraintID:   "constraint-1",
		Scope:          ResourceScopeSystem,
		BeforeRevision: &before,
		Classification: BoundaryRelax,
	})
	require.NoError(t, err)
	require.NoError(t, Validate(event))
	assert.Equal(t, "delete", event.Action)
	assert.Equal(t, SeverityInfo, event.Severity)
	_, err = uuid.Parse(event.EventID)
	require.NoError(t, err)
	payload, ok := event.Payload.(AccessBoundaryPayload)
	require.True(t, ok)
	leaves := make([]string, 0)
	for leaf := range payload.auditPayloadLeaves() {
		leaves = append(leaves, leaf)
	}
	sort.Strings(leaves)
	assert.Equal(t, []string{"before_revision", "classification"}, leaves)

	withoutRevision, err := BuildAccessBoundaryDelete(historyOnlyContext(t), AccessBoundaryDeleteInput{
		Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
		ConstraintID:   "constraint-1",
		Scope:          ResourceScopeSystem,
		Classification: BoundaryRelax,
	})
	require.NoError(t, err)
	require.NoError(t, Validate(withoutRevision))

	// The delete schema admits no other payload leaf.
	afterRevision := int64(8)
	extended := event
	extended.Payload = AccessBoundaryPayload{Classification: BoundaryRelax, AfterRevision: &afterRevision}
	require.Error(t, Validate(extended))
	extended.Payload = AccessBoundaryPayload{Classification: BoundaryRelax, PreviewID: "preview-1"}
	require.Error(t, Validate(extended))

	_, err = BuildAccessBoundaryDelete(context.Background(), AccessBoundaryDeleteInput{
		Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
		ConstraintID:   "constraint-1",
		Scope:          ResourceScopeSystem,
		Classification: BoundaryRelax,
	})
	require.Error(t, err, "missing operation context must be rejected")
}
