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
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperationContextRoundTripAndCreation(t *testing.T) {
	t.Parallel()

	op, err := NewOperationContext("trusted-request-id")
	require.NoError(t, err)
	ctx := ContextWithOperation(context.Background(), op)

	got, ok := OperationFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "trusted-request-id", got.CorrelationID)

	generated := StartOperation()
	assert.NotEqual(t, op.CorrelationID, generated.CorrelationID)
	_, err = uuid.Parse(generated.CorrelationID)
	assert.NoError(t, err)

	_, err = NewOperationContext("")
	assert.Error(t, err)
	_, err = NewOperationContext(strings.Repeat("x", 129))
	assert.Error(t, err)
}

func TestPhaseOutcomeMatrix(t *testing.T) {
	t.Parallel()

	valid := []struct {
		phase   Phase
		outcome Outcome
	}{
		{PhaseAttempt, ""},
		{PhaseDecision, OutcomeAllow},
		{PhaseDecision, OutcomeDeny},
		{PhaseObservation, OutcomeSucceeded},
		{PhaseObservation, OutcomeFailed},
		{PhaseCommit, OutcomeSucceeded},
		{PhaseFailure, OutcomeFailed},
		{PhaseDelivery, OutcomeSucceeded},
		{PhaseDelivery, OutcomeFailed},
		{PhaseDelivery, OutcomeSkipped},
		{PhaseDelivery, OutcomeDeferred},
	}
	for _, tc := range valid {
		assert.NoError(t, ValidatePhaseOutcome(tc.phase, tc.outcome), "%s/%s", tc.phase, tc.outcome)
	}

	invalid := []struct {
		phase   Phase
		outcome Outcome
	}{
		{PhaseAttempt, OutcomeSucceeded},
		{PhaseDecision, ""},
		{PhaseDecision, OutcomeSucceeded},
		{PhaseObservation, ""},
		{PhaseObservation, OutcomeAllow},
		{PhaseObservation, OutcomeSkipped},
		{PhaseCommit, OutcomeFailed},
		{PhaseFailure, OutcomeSucceeded},
		{PhaseDelivery, OutcomeAllow},
		{"invented", OutcomeSucceeded},
	}
	for _, tc := range invalid {
		assert.Error(t, ValidatePhaseOutcome(tc.phase, tc.outcome), "%s/%s", tc.phase, tc.outcome)
	}
}

func TestAccessBoundaryCreateBuildRenderAndCapture(t *testing.T) {
	t.Parallel()

	ctx := ContextWithOperation(context.Background(), AuditOperationContext{CorrelationID: "request-123"})
	before := int64(0)
	after := int64(1)
	credential := mustCredentialRef(t, CredentialRefInput{
		Kind: CredentialUAT, ID: "token-1", Name: "deploy",
		BoundaryKind: CredentialBoundaryProject, BoundaryProjectID: "project-1",
		Labels: map[string]string{"purpose": "automation"},
	})
	event, err := BuildAccessBoundaryCreate(ctx, AccessBoundaryCreateInput{
		Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
		Credential:     &credential,
		ConstraintID:   "constraint-1",
		ProjectID:      "project-1",
		BeforeRevision: &before,
		AfterRevision:  &after,
		Classification: BoundaryTighten,
		PreviewID:      "preview-1",
		DraftHash:      strings.Repeat("a", 64),
		ImpactCounts:   &ImpactCounts{Agents: 1, Users: 2, Projects: 3},
		ChangedFields:  []string{"permissions", "subjects"},
	})
	require.NoError(t, err)
	assert.Equal(t, PhaseCommit, event.Phase)
	assert.Equal(t, OutcomeSucceeded, event.Outcome)
	assert.Equal(t, SeverityInfo, event.Severity)
	assert.Equal(t, "request-123", event.CorrelationID)
	assert.Equal(t, "access_boundary", event.Family)
	assert.Equal(t, "create", event.Action)

	rendered, err := Render(event)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rendered, &got))
	assert.Equal(t, float64(1), got["schema_version"])
	assert.Equal(t, "request-123", got["correlation_id"])
	assert.NotContains(t, got, "initiator")
	assert.NotContains(t, got, "executor")
	assert.NotContains(t, got, "causation_id")
	assert.NotContains(t, string(rendered), "unknown")
	assert.Equal(t, map[string]any{"kind": "user", "id": "user-1"}, got["principal"])
	assert.Equal(t, map[string]any{
		"kind": "uat", "id": "token-1", "name": "deploy",
		"boundary_kind": "project", "boundary_project_id": "project-1",
		"labels": map[string]any{"purpose": "automation"},
	}, got["credential"])
	assert.Equal(t, map[string]any{"kind": "access_constraint", "id": "constraint-1", "project_id": "project-1"}, got["resource"])
	assert.Equal(t, map[string]any{
		"before_revision": float64(0),
		"after_revision":  float64(1),
		"classification":  "tighten",
		"preview_id":      "preview-1",
		"draft_hash":      strings.Repeat("a", 64),
		"impact_counts":   map[string]any{"agents": float64(1), "users": float64(2), "projects": float64(3)},
		"changed_fields":  []any{"permissions", "subjects"},
	}, got["payload"])

	sink := NewCaptureSink()
	require.NoError(t, sink.Emit(ctx, event))
	require.Len(t, sink.Records(), 1)
	assert.JSONEq(t, string(rendered), string(sink.Records()[0]))
}

func TestAccessBoundaryCreateRequiresOperationContext(t *testing.T) {
	t.Parallel()

	_, err := BuildAccessBoundaryCreate(context.Background(), AccessBoundaryCreateInput{
		Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
		ConstraintID:   "constraint-1",
		Classification: BoundaryTighten,
	})
	assert.ErrorContains(t, err, "operation context")
}

func TestValidationRejectsCatalogAndPayloadViolations(t *testing.T) {
	t.Parallel()

	event := validCreateEvent(t)

	wrongPair := event
	wrongPair.Phase = PhaseFailure
	wrongPair.Outcome = OutcomeFailed
	wrongPair.Severity = SeverityWarning
	assert.ErrorContains(t, Validate(wrongPair), "catalog")

	undeclared := event
	undeclared.Payload = testPayloadWithUndeclaredLeaf{}
	_, err := Render(undeclared)
	assert.ErrorContains(t, err, "undeclared payload leaf")

	wrongResource := event
	wrongResource.Resource.Kind = "project"
	assert.ErrorContains(t, Validate(wrongResource), "resource kind")
}

func TestValidationRejectsBoundsAndInvalidEnums(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*EnvelopeV1)
	}{
		{"missing principal", func(e *EnvelopeV1) { e.Principal = nil }},
		{"bad identity kind", func(e *EnvelopeV1) { e.Principal.Kind = "person" }},
		{"empty resource id", func(e *EnvelopeV1) { e.Resource.ID = "" }},
		{"empty project id", func(e *EnvelopeV1) { e.Resource.ProjectID = "" }},
		{"bad event id", func(e *EnvelopeV1) { e.EventID = "not-a-uuid" }},
		{"nil event id", func(e *EnvelopeV1) { e.EventID = uuid.Nil.String() }},
		{"noncanonical event id", func(e *EnvelopeV1) { e.EventID = strings.ReplaceAll(e.EventID, "-", "") }},
		{"nil causation id", func(e *EnvelopeV1) { e.CausationID = uuid.Nil.String() }},
		{"non utc timestamp", func(e *EnvelopeV1) { e.OccurredAt = e.OccurredAt.In(time.FixedZone("offset", 3600)) }},
		{"wrong severity", func(e *EnvelopeV1) { e.Severity = SeverityWarning }},
		{"oversized changed fields", func(e *EnvelopeV1) {
			e.Payload = AccessBoundaryPayload{Classification: BoundaryTighten, ChangedFields: make([]string, 33)}
		}},
		{"invalid draft hash", func(e *EnvelopeV1) {
			e.Payload = AccessBoundaryPayload{Classification: BoundaryTighten, DraftHash: "secret"}
		}},
		{"invalid classification", func(e *EnvelopeV1) {
			e.Payload = AccessBoundaryPayload{Classification: "unknown"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event := validCreateEvent(t)
			tc.mutate(&event)
			assert.Error(t, Validate(event))
		})
	}
}

func TestCredentialValidationRejectsUnsafeMetadataWithoutEchoingValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  CredentialRefInput
		canary string
	}{
		{
			name:   "secret-shaped kind",
			input:  CredentialRefInput{Kind: CredentialKind("SCION_PAT_kind-canary")},
			canary: "SCION_PAT_kind-canary",
		},
		{
			name:   "secret-shaped id",
			input:  CredentialRefInput{Kind: CredentialUAT, ID: "scion_pat_id-canary"},
			canary: "scion_pat_id-canary",
		},
		{
			name:   "secret-shaped name",
			input:  CredentialRefInput{Kind: CredentialUAT, Name: "scion_pat_name-canary"},
			canary: "scion_pat_name-canary",
		},
		{
			name:   "secret-shaped boundary kind",
			input:  CredentialRefInput{Kind: CredentialUAT, BoundaryKind: CredentialBoundaryKind("Bearer boundary-kind-canary")},
			canary: "Bearer boundary-kind-canary",
		},
		{
			name: "secret-shaped boundary project id",
			input: CredentialRefInput{Kind: CredentialUAT, BoundaryKind: CredentialBoundaryProject,
				BoundaryProjectID: "Bearer boundary-project-canary"},
			canary: "Bearer boundary-project-canary",
		},
		{
			name:   "invalid label key",
			input:  CredentialRefInput{Kind: CredentialUAT, Labels: map[string]string{"InvalidKeyCanary": "safe"}},
			canary: "InvalidKeyCanary",
		},
		{
			name:   "reserved label key",
			input:  CredentialRefInput{Kind: CredentialUAT, Labels: map[string]string{"principal": "safe"}},
			canary: "principal",
		},
		{
			name:   "secret-shaped label key",
			input:  CredentialRefInput{Kind: CredentialUAT, Labels: map[string]string{"scion_pat_key-canary": "safe"}},
			canary: "scion_pat_key-canary",
		},
		{
			name:   "secret-shaped label value",
			input:  CredentialRefInput{Kind: CredentialUAT, Labels: map[string]string{"purpose": "Bearer value-canary"}},
			canary: "Bearer value-canary",
		},
		{
			name:   "disallowed label value character",
			input:  CredentialRefInput{Kind: CredentialUAT, Labels: map[string]string{"purpose": "value$canary"}},
			canary: "value$canary",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCredentialRef(tc.input)
			require.Error(t, err)
			var validationErr *CredentialValidationError
			assert.ErrorAs(t, err, &validationErr)
			assert.NotContains(t, err.Error(), tc.canary)
		})
	}
}

func TestCredentialValidationUsesCanonicalMetadataBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input CredentialRefInput
	}{
		{"label count", CredentialRefInput{Kind: CredentialUAT, Labels: map[string]string{
			"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6", "g": "7", "h": "8", "i": "9",
		}}},
		{"label key length", CredentialRefInput{Kind: CredentialUAT, Labels: map[string]string{strings.Repeat("a", 33): "safe"}}},
		{"label value length", CredentialRefInput{Kind: CredentialUAT, Labels: map[string]string{"purpose": strings.Repeat("a", 65)}}},
		{"name format character", CredentialRefInput{Kind: CredentialUAT, Name: "safe\u200bname"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCredentialRef(tc.input)
			assert.Error(t, err)
		})
	}
}

func mustCredentialRef(t *testing.T, input CredentialRefInput) CredentialRef {
	t.Helper()
	credential, err := NewCredentialRef(input)
	require.NoError(t, err)
	return credential
}

type testPayloadWithUndeclaredLeaf struct{}

func (testPayloadWithUndeclaredLeaf) auditPayloadLeaves() map[string]any {
	return map[string]any{
		"classification": "tighten",
		"secret_value":   "canary-secret",
	}
}

type changingTestPayload struct {
	calls int
}

func (p *changingTestPayload) auditPayloadLeaves() map[string]any {
	p.calls++
	if p.calls == 1 {
		return map[string]any{"classification": "tighten"}
	}
	return map[string]any{
		"classification": "tighten",
		"secret_value":   "snapshot-canary",
	}
}

func TestRenderValidatesAndSerializesOnePayloadSnapshot(t *testing.T) {
	t.Parallel()

	event := validCreateEvent(t)
	payload := &changingTestPayload{}
	event.Payload = payload

	rendered, err := Render(event)
	require.NoError(t, err)
	assert.Equal(t, 1, payload.calls)
	assert.NotContains(t, string(rendered), "snapshot-canary")
	assert.JSONEq(t, `{"classification":"tighten"}`, extractPayloadJSON(t, rendered))
}

func TestCatalogSnapshotAccessBoundaryCreate(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []CatalogEntry{{
		Family:                 "access_boundary",
		Action:                 "create",
		AllowedPairs:           []PhaseOutcome{{Phase: PhaseCommit, Outcome: OutcomeSucceeded}},
		ResourceKind:           "access_constraint",
		RequiredEnvelopeLeaves: []string{"schema_version", "event_id", "occurred_at", "family", "action", "phase", "outcome", "severity", "correlation_id", "principal", "resource", "resource.project_id"},
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
		Destinations: []Destination{DestinationStructuredLog, DestinationHistory},
	}}, Catalog())
}

func TestRenderIsStableAndOmitsUnknownOptionalFields(t *testing.T) {
	t.Parallel()

	event := validCreateEvent(t)
	first, err := Render(event)
	require.NoError(t, err)
	second, err := Render(event)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, `{"schema_version":1,"event_id":"`+event.EventID+`","occurred_at":"2026-10-01T12:34:56.123456789Z","family":"access_boundary","action":"create","phase":"commit","outcome":"succeeded","severity":"info","correlation_id":"corr-1","principal":{"kind":"user","id":"user-1"},"resource":{"kind":"access_constraint","id":"constraint-1","project_id":"project-1"},"payload":{"classification":"tighten"}}`, string(first))
}

func TestCaptureSinkConcurrentEmitAndRecords(t *testing.T) {
	t.Parallel()

	const (
		emitters       = 8
		recordsPerEmit = 25
		readers        = 4
		readsPerReader = 50
	)

	event := validCreateEvent(t)
	expected, err := Render(event)
	require.NoError(t, err)
	sink := NewCaptureSink()
	start := make(chan struct{})
	errs := make(chan error, emitters*recordsPerEmit)
	var workers sync.WaitGroup

	for range emitters {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range recordsPerEmit {
				if err := sink.Emit(context.Background(), event); err != nil {
					errs <- err
				}
			}
		}()
	}
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range readsPerReader {
				_ = sink.Records()
			}
		}()
	}

	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	records := sink.Records()
	require.Len(t, records, emitters*recordsPerEmit)
	for _, record := range records {
		assert.JSONEq(t, string(expected), string(record))
	}

	records[0][0] = '!'
	assert.Equal(t, expected, sink.Records()[0])
}

func extractPayloadJSON(t *testing.T, record []byte) string {
	t.Helper()
	var envelope struct {
		Payload json.RawMessage `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(record, &envelope))
	return string(envelope.Payload)
}

func validCreateEvent(t *testing.T) EnvelopeV1 {
	t.Helper()
	event, err := buildAccessBoundaryCreate(
		AuditOperationContext{CorrelationID: "corr-1"},
		AccessBoundaryCreateInput{
			Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
			ConstraintID:   "constraint-1",
			ProjectID:      "project-1",
			Classification: BoundaryTighten,
		},
		uuid.MustParse("11111111-1111-4111-8111-111111111111").String(),
		time.Date(2026, 10, 1, 12, 34, 56, 123456789, time.UTC),
	)
	require.NoError(t, err)
	return event
}
