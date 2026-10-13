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
	"fmt"
	"log/slog"
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

	phases := []Phase{
		PhaseAttempt, PhaseDecision, PhaseObservation, PhaseCommit, PhaseFailure, PhaseDelivery,
		"unknown-phase-canary",
	}
	outcomes := []Outcome{
		"", OutcomeAllow, OutcomeDeny, OutcomeSucceeded, OutcomeFailed, OutcomeSkipped, OutcomeDeferred,
		"unknown-outcome-canary",
	}
	allowed := map[Phase]map[Outcome]bool{
		PhaseAttempt:     {"": true},
		PhaseDecision:    {OutcomeAllow: true, OutcomeDeny: true},
		PhaseObservation: {OutcomeSucceeded: true, OutcomeFailed: true},
		PhaseCommit:      {OutcomeSucceeded: true},
		PhaseFailure:     {OutcomeFailed: true},
		PhaseDelivery: {
			OutcomeSucceeded: true,
			OutcomeFailed:    true,
			OutcomeSkipped:   true,
			OutcomeDeferred:  true,
		},
	}

	for _, phase := range phases {
		for _, outcome := range outcomes {
			name := fmt.Sprintf("%s/%s", phase, outcome)
			t.Run(name, func(t *testing.T) {
				err := ValidatePhaseOutcome(phase, outcome)
				if allowed[phase][outcome] {
					assert.NoError(t, err)
					return
				}
				assert.Error(t, err)
			})
		}
	}
}

func TestCatalogRejectsEveryUndeclaredPhaseOutcomePair(t *testing.T) {
	t.Parallel()

	phases := []Phase{
		PhaseAttempt, PhaseDecision, PhaseObservation, PhaseCommit, PhaseFailure, PhaseDelivery,
		"unknown-phase-canary",
	}
	outcomes := []Outcome{
		"", OutcomeAllow, OutcomeDeny, OutcomeSucceeded, OutcomeFailed, OutcomeSkipped, OutcomeDeferred,
		"unknown-outcome-canary",
	}

	for _, entry := range Catalog() {
		entry := entry
		t.Run(entry.Family+"/"+entry.Action, func(t *testing.T) {
			for _, phase := range phases {
				for _, outcome := range outcomes {
					event := validCreateEvent(t)
					if entry.Family == "authorization" && entry.Action == "decide" {
						event = validAuthorizationDecisionEvent(OutcomeAllow)
					}
					event.Family = entry.Family
					event.Action = entry.Action
					event.Phase = phase
					event.Outcome = outcome
					event.Severity = severityForOutcome(outcome)
					wantValid := containsPhaseOutcome(entry.AllowedPairs, phase, outcome)
					err := Validate(event)
					if wantValid {
						assert.NoError(t, err, "%s/%s", phase, outcome)
					} else {
						assert.Error(t, err, "%s/%s", phase, outcome)
					}
				}
			}

			for _, outcome := range []Outcome{OutcomeSucceeded, OutcomeFailed} {
				assert.False(t, containsPhaseOutcome(entry.AllowedPairs, PhaseObservation, outcome),
					"non-diagnostic action must reject observation/%s", outcome)
			}
		})
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
		Scope:          ResourceScopeProject,
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
	var validationErr *ValidationError
	assert.ErrorAs(t, err, &validationErr)
}

func TestAccessBoundaryCreateResourceScopeContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		scope     ResourceScope
		projectID string
		wantErr   bool
	}{
		{name: "system without project", scope: ResourceScopeSystem},
		{name: "project with project", scope: ResourceScopeProject, projectID: "project-1"},
		{name: "system with project", scope: ResourceScopeSystem, projectID: "project-canary", wantErr: true},
		{name: "project without project", scope: ResourceScopeProject, wantErr: true},
		{name: "missing scope", projectID: "project-canary", wantErr: true},
		{name: "unknown scope", scope: "scope-canary", projectID: "project-canary", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event, err := buildAccessBoundaryCreate(
				AuditOperationContext{CorrelationID: "corr-1"},
				AccessBoundaryCreateInput{
					Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
					ConstraintID:   "constraint-1",
					Scope:          tc.scope,
					ProjectID:      tc.projectID,
					Classification: BoundaryTighten,
				},
				uuid.MustParse("11111111-1111-4111-8111-111111111111").String(),
				time.Date(2026, 10, 1, 12, 34, 56, 123456789, time.UTC),
			)
			if tc.wantErr {
				require.Error(t, err)
				var validationErr *ValidationError
				require.ErrorAs(t, err, &validationErr)
				assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, "project-canary")
				assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, "scope-canary")
				return
			}

			rendered, err := Render(event)
			require.NoError(t, err)
			var got struct {
				Resource map[string]any `json:"resource"`
			}
			require.NoError(t, json.Unmarshal(rendered, &got))
			assert.Equal(t, "access_constraint", got.Resource["kind"])
			assert.Equal(t, "constraint-1", got.Resource["id"])
			if tc.scope == ResourceScopeSystem {
				assert.NotContains(t, got.Resource, "project_id")
			} else {
				assert.Equal(t, "project-1", got.Resource["project_id"])
			}
		})
	}
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
	assert.Error(t, err)

	wrongResource := event
	wrongResource.Resource.Kind = "project"
	assert.Error(t, Validate(wrongResource))
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
			require.ErrorAs(t, err, &validationErr)
			assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, tc.canary)
		})
	}
}

func TestValidationErrorsAreTypedStableAndValueFree(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name   string
		canary string
		mutate func(*EnvelopeV1, string)
	}
	tests := []testCase{
		{"event id", "event-id-canary", func(e *EnvelopeV1, c string) { e.EventID = c }},
		{"family", "family-canary", func(e *EnvelopeV1, c string) { e.Family = c }},
		{"action", "action-canary", func(e *EnvelopeV1, c string) { e.Action = c }},
		{"phase", "phase-canary", func(e *EnvelopeV1, c string) { e.Phase = Phase(c) }},
		{"outcome", "outcome-canary", func(e *EnvelopeV1, c string) { e.Outcome = Outcome(c) }},
		{"severity", "severity-canary", func(e *EnvelopeV1, c string) { e.Severity = Severity(c) }},
		{"correlation id", "correlation-canary", func(e *EnvelopeV1, c string) { e.CorrelationID = c + "\n" }},
		{"causation id", "causation-canary", func(e *EnvelopeV1, c string) { e.CausationID = c }},
		{"request id", "request-id-canary", func(e *EnvelopeV1, c string) {
			e.Request = &RequestRef{ID: c}
		}},
		{"request method", "method-canary", func(e *EnvelopeV1, c string) {
			e.Request = &RequestRef{Method: c}
		}},
		{"request route", "route-canary", func(e *EnvelopeV1, c string) {
			e.Request = &RequestRef{Route: c + "\n"}
		}},
		{"request surface", "surface-canary", func(e *EnvelopeV1, c string) {
			e.Request = &RequestRef{Surface: c + "\n"}
		}},
		{"initiator kind", "initiator-kind-canary", func(e *EnvelopeV1, c string) {
			e.Initiator = &IdentityRef{Kind: IdentityKind(c), ID: "safe"}
		}},
		{"initiator id", "initiator-id-canary", func(e *EnvelopeV1, c string) {
			e.Initiator = &IdentityRef{Kind: IdentityUser, ID: c + "\n"}
		}},
		{"principal kind", "principal-kind-canary", func(e *EnvelopeV1, c string) {
			e.Principal.Kind = IdentityKind(c)
		}},
		{"principal id", "principal-id-canary", func(e *EnvelopeV1, c string) {
			e.Principal.ID = c + "\n"
		}},
		{"executor kind", "executor-kind-canary", func(e *EnvelopeV1, c string) {
			e.Executor = &IdentityRef{Kind: IdentityKind(c), ID: "safe"}
		}},
		{"executor id", "executor-id-canary", func(e *EnvelopeV1, c string) {
			e.Executor = &IdentityRef{Kind: IdentitySystem, ID: c + "\n"}
		}},
		{"resource kind", "resource-kind-canary", func(e *EnvelopeV1, c string) { e.Resource.Kind = c }},
		{"resource id", "resource-id-canary", func(e *EnvelopeV1, c string) { e.Resource.ID = c + "\n" }},
		{"resource project id", "resource-project-canary", func(e *EnvelopeV1, c string) {
			e.Resource.ProjectID = c + "\n"
		}},
		{"payload classification", "classification-canary", func(e *EnvelopeV1, c string) {
			e.Payload = AccessBoundaryPayload{Classification: BoundaryClassification(c)}
		}},
		{"payload preview id", "preview-canary", func(e *EnvelopeV1, c string) {
			e.Payload = AccessBoundaryPayload{Classification: BoundaryTighten, PreviewID: c + "\n"}
		}},
		{"payload draft hash", "draft-hash-canary", func(e *EnvelopeV1, c string) {
			e.Payload = AccessBoundaryPayload{Classification: BoundaryTighten, DraftHash: c}
		}},
		{"payload changed field", "changed-field-canary", func(e *EnvelopeV1, c string) {
			e.Payload = AccessBoundaryPayload{Classification: BoundaryTighten, ChangedFields: []string{c + "\n"}}
		}},
		{"undeclared payload leaf", "undeclared-leaf-canary", func(e *EnvelopeV1, c string) {
			e.Payload = retainedMapPayload{leaves: map[string]any{
				"classification": "tighten",
				c:                "safe",
			}}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event := validCreateEvent(t)
			tc.mutate(&event, tc.canary)
			err := Validate(event)
			require.Error(t, err)
			var validationErr *ValidationError
			require.ErrorAs(t, err, &validationErr)
			assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, tc.canary)
		})
	}
}

func TestValidationErrorContractIsStableThroughRender(t *testing.T) {
	t.Parallel()

	event := validCreateEvent(t)
	event.Request = &RequestRef{Method: "method-contract-canary"}

	for _, validate := range []func() error{
		func() error { return Validate(event) },
		func() error {
			_, err := Render(event)
			return err
		},
	} {
		err := validate()
		require.Error(t, err)
		var validationErr *ValidationError
		require.ErrorAs(t, err, &validationErr)
		assert.Equal(t, "request.method", validationErr.Field)
		assert.Equal(t, "must be a standard HTTP method", validationErr.Rule)
		assert.Equal(t,
			"invalid audit event field request.method: must be a standard HTTP method",
			err.Error(),
		)
		assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, "method-contract-canary")
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

type retainedMapPayload struct {
	leaves map[string]any
}

func (p retainedMapPayload) auditPayloadLeaves() map[string]any {
	return p.leaves
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

func TestRenderSnapshotDoesNotRetainEnvelopeOrPayloadAliases(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"purpose": "automation"}
	credential := mustCredentialRef(t, CredentialRefInput{Kind: CredentialUAT, Labels: labels})
	request := &RequestRef{Method: "POST", Route: "/safe", Surface: "api"}
	principal := &IdentityRef{Kind: IdentityUser, ID: "user-1"}
	resource := &ResourceRef{Kind: "access_constraint", ID: "constraint-1", Scope: ResourceScopeProject, ProjectID: "project-1"}
	changedFields := []string{"permissions"}
	leaves := map[string]any{
		"classification": "tighten",
		"changed_fields": changedFields,
	}
	event := validCreateEvent(t)
	event.Request = request
	event.Principal = principal
	event.Credential = &credential
	event.Resource = resource
	event.Payload = retainedMapPayload{leaves: leaves}

	snapshot := newRenderSnapshot(event)

	request.Method = "method-alias-canary"
	principal.ID = "principal-alias-canary"
	resource.ID = "resource-alias-canary"
	labels["purpose"] = "label-alias-canary"
	changedFields[0] = "slice-alias-canary"
	leaves["classification"] = "payload-map-alias-canary"
	leaves["undeclared-alias-canary"] = "unsafe"

	rendered, err := snapshot.render()
	require.NoError(t, err)
	for _, canary := range []string{
		"method-alias-canary",
		"principal-alias-canary",
		"resource-alias-canary",
		"label-alias-canary",
		"slice-alias-canary",
		"payload-map-alias-canary",
		"undeclared-alias-canary",
	} {
		assert.NotContains(t, string(rendered), canary)
	}
	assert.Contains(t, string(rendered), `"method":"POST"`)
	assert.Contains(t, string(rendered), `"changed_fields":["permissions"]`)
}

func TestRenderIsRaceSafeAgainstMutationOfBuilderInputAliases(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"purpose": "automation"}
	credential := mustCredentialRef(t, CredentialRefInput{Kind: CredentialUAT, Labels: labels})
	changedFields := []string{"permissions"}
	event, err := buildAccessBoundaryCreate(
		AuditOperationContext{CorrelationID: "corr-1"},
		AccessBoundaryCreateInput{
			Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
			Credential:     &credential,
			ConstraintID:   "constraint-1",
			Scope:          ResourceScopeProject,
			ProjectID:      "project-1",
			Classification: BoundaryTighten,
			ChangedFields:  changedFields,
		},
		uuid.MustParse("11111111-1111-4111-8111-111111111111").String(),
		time.Date(2026, 10, 1, 12, 34, 56, 123456789, time.UTC),
	)
	require.NoError(t, err)
	retainedFields := []string{"subjects"}
	retainedLeaves := map[string]any{
		"classification": "tighten",
		"changed_fields": retainedFields,
	}
	retainedEvent := validCreateEvent(t)
	retainedEvent.Payload = retainedMapPayload{leaves: retainedLeaves}
	retainedSnapshot := newRenderSnapshot(retainedEvent)

	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for i := range 100 {
			labels["purpose"] = fmt.Sprintf("alias-%d", i)
			changedFields[0] = fmt.Sprintf("alias-%d", i)
			retainedFields[0] = fmt.Sprintf("retained-alias-%d", i)
			retainedLeaves["classification"] = fmt.Sprintf("retained-alias-%d", i)
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for range 100 {
			rendered, renderErr := Render(event)
			assert.NoError(t, renderErr)
			assert.NotContains(t, string(rendered), "alias-")

			retainedRendered, retainedRenderErr := retainedSnapshot.render()
			assert.NoError(t, retainedRenderErr)
			assert.NotContains(t, string(retainedRendered), "retained-alias-")
		}
	}()
	close(start)
	workers.Wait()
}

func TestCatalogSnapshotAccessBoundaryCreate(t *testing.T) {
	t.Parallel()

	entries := Catalog()
	require.Len(t, entries, 2)
	assert.Equal(t, []CatalogEntry{{
		Family:                 "access_boundary",
		Action:                 "create",
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
		Destinations: []Destination{DestinationStructuredLog, DestinationHistory},
	}}, entries[:1])
}

func TestAuthorizationDecisionEnvelope_AllowDeny(t *testing.T) {
	t.Parallel()

	entries := Catalog()
	require.Len(t, entries, 2)
	assert.Equal(t, CatalogEntry{
		Family:                  "authorization",
		Action:                  "decide",
		AllowedPairs:            []PhaseOutcome{{Phase: PhaseDecision, Outcome: OutcomeAllow}, {Phase: PhaseDecision, Outcome: OutcomeDeny}},
		ResourceKind:            "project",
		AdditionalResourceKinds: []string{"agent"},
		RequiredEnvelopeLeaves:  []string{"schema_version", "event_id", "occurred_at", "family", "action", "phase", "outcome", "severity", "correlation_id", "principal", "resource"},
		ResourceScopes:          []ResourceScopeSchema{{Scope: ResourceScopeSystem, ProjectID: ResourceProjectIDOmitted}},
		RequiredPayloadLeaves: []PayloadLeafSchema{
			{Name: "permission_id", Type: PayloadString, MaxBytes: 128},
			{Name: "permission", Type: PayloadString, MaxBytes: 128},
			{Name: "reason", Type: PayloadString, MaxBytes: 256},
			{Name: "sampled", Type: PayloadString, MaxBytes: 5, AllowedValues: []string{"true", "false"}},
		},
		OptionalPayloadLeaves: []PayloadLeafSchema{{Name: "denied_by", Type: PayloadString, MaxBytes: 64}},
		Destinations:          []Destination{DestinationStructuredLog},
	}, entries[1])

	for _, kind := range []string{"project", "agent"} {
		for _, outcome := range []Outcome{OutcomeAllow, OutcomeDeny} {
			for _, sampled := range []string{"true", "false"} {
				t.Run(kind+"/"+string(outcome)+"/sampled-"+sampled, func(t *testing.T) {
					event := validAuthorizationDecisionEvent(outcome)
					event.Resource.Kind = kind
					event.Resource.ID = kind + "-1"
					payload := event.Payload.(AuthorizationDecisionPayload)
					payload.Sampled = sampled
					event.Payload = payload
					require.NoError(t, Validate(event))
					rendered, err := Render(event)
					require.NoError(t, err)
					var got map[string]any
					require.NoError(t, json.Unmarshal(rendered, &got))
					assert.Equal(t, "decision", got["phase"])
					assert.Equal(t, string(outcome), got["outcome"])
					assert.Equal(t, string(severityForOutcome(outcome)), got["severity"])
					assert.Equal(t, map[string]any{"kind": kind, "id": kind + "-1"}, got["resource"])
					deniedByJSON := ""
					wantPayload := map[string]any{"permission_id": "fixture.permission", "permission": "read", "reason": "fixture decision", "sampled": sampled}
					if outcome == OutcomeDeny {
						deniedByJSON = `"denied_by":"delegation_ceiling",`
						wantPayload["denied_by"] = "delegation_ceiling"
					}
					assert.Equal(t, `{"schema_version":1,"event_id":"11111111-1111-4111-8111-111111111111","occurred_at":"2026-10-01T12:34:56.123456789Z","family":"authorization","action":"decide","phase":"decision","outcome":"`+string(outcome)+`","severity":"`+string(severityForOutcome(outcome))+`","correlation_id":"corr-1","principal":{"kind":"user","id":"user-1"},"resource":{"kind":"`+kind+`","id":"`+kind+`-1"},"payload":{`+deniedByJSON+`"permission":"read","permission_id":"fixture.permission","reason":"fixture decision","sampled":"`+sampled+`"}}`, string(rendered))
					assert.Equal(t, wantPayload, got["payload"])
					// The schema preserves explicit permission data; it does not resolve
					// or infer an operation from a permission or resource.
					assert.NotContains(t, got, "operation_id")
					assert.NotContains(t, got, "credential")
				})
			}
		}
	}
}

func TestAuthorizationDecisionEnvelope_RejectsInvalidContract(t *testing.T) {
	t.Parallel()

	const canary = "rejected-value-canary"
	cases := []struct {
		name          string
		rejectedValue string
		mutate        func(*EnvelopeV1)
	}{
		{"family", canary, func(e *EnvelopeV1) { e.Family = canary }},
		{"action", canary, func(e *EnvelopeV1) { e.Action = canary }},
		{"commit-succeeded", "succeeded", func(e *EnvelopeV1) { e.Phase, e.Outcome = PhaseCommit, OutcomeSucceeded }},
		{"decision-succeeded", "succeeded", func(e *EnvelopeV1) { e.Outcome = OutcomeSucceeded }},
		{"missing-principal", "", func(e *EnvelopeV1) { e.Principal = nil }},
		{"missing-resource", "", func(e *EnvelopeV1) { e.Resource = nil }},
		{"wrong-resource-kind", canary, func(e *EnvelopeV1) { e.Resource.Kind = canary }},
		{"unknown-resource-scope", canary, func(e *EnvelopeV1) { e.Resource.Scope = ResourceScope(canary) }},
		{"unexpected-project-id", canary, func(e *EnvelopeV1) { e.Resource.ProjectID = canary }},
		{"wrong-severity", "warning", func(e *EnvelopeV1) { e.Severity = SeverityWarning }},
		{"missing-payload", "", func(e *EnvelopeV1) { e.Payload = nil }},
		{"sampled-enum", "TRUE", func(e *EnvelopeV1) {
			payload := e.Payload.(AuthorizationDecisionPayload)
			payload.Sampled = "TRUE"
			e.Payload = payload
		}},
		{"raw-credential-leaf", "Bearer " + canary, func(e *EnvelopeV1) {
			leaves := e.Payload.auditPayloadLeaves()
			leaves["raw_credential"] = "Bearer " + canary
			e.Payload = retainedMapPayload{leaves: leaves}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := validAuthorizationDecisionEvent(OutcomeAllow)
			tc.mutate(&event)
			err := Validate(event)
			require.Error(t, err)
			var validationErr *ValidationError
			require.ErrorAs(t, err, &validationErr)
			if tc.rejectedValue != "" {
				assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, tc.rejectedValue)
			}
			rendered, renderErr := Render(event)
			require.Error(t, renderErr)
			assert.Nil(t, rendered, "rejected values must never cross the render boundary")
			require.ErrorAs(t, renderErr, &validationErr)
			if tc.rejectedValue != "" {
				assertValidationErrorDoesNotRetain(t, renderErr, validationErr.Field, validationErr.Rule, tc.rejectedValue)
			}
		})
	}

	for _, leaf := range []struct {
		name string
		cap  int
	}{
		{"permission_id", 128}, {"permission", 128}, {"reason", 256}, {"denied_by", 64}, {"sampled", 5},
	} {
		for _, mutation := range []string{"missing", "empty", "oversize"} {
			t.Run(leaf.name+"/"+mutation, func(t *testing.T) {
				event := validAuthorizationDecisionEvent(OutcomeAllow)
				leaves := event.Payload.auditPayloadLeaves()
				switch mutation {
				case "missing":
					delete(leaves, leaf.name)
				case "empty":
					leaves[leaf.name] = ""
				case "oversize":
					leaves[leaf.name] = strings.Repeat("x", leaf.cap+1)
				}
				event.Payload = retainedMapPayload{leaves: leaves}
				if leaf.name == "denied_by" && mutation == "missing" {
					require.NoError(t, Validate(event))
					_, err := Render(event)
					require.NoError(t, err)
					return
				}
				err := Validate(event)
				require.Error(t, err)
				var validationErr *ValidationError
				require.ErrorAs(t, err, &validationErr)
				assert.Equal(t, "payload."+leaf.name, validationErr.Field)
				if mutation == "oversize" {
					assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, strings.Repeat("x", leaf.cap+1))
				}
				rendered, renderErr := Render(event)
				require.Error(t, renderErr)
				assert.Nil(t, rendered)
				require.ErrorAs(t, renderErr, &validationErr)
				if mutation == "oversize" {
					assertValidationErrorDoesNotRetain(t, renderErr, validationErr.Field, validationErr.Rule, strings.Repeat("x", leaf.cap+1))
				}
			})
		}
		// The valid maximum separates byte-cap enforcement from enum rejection.
		t.Run(leaf.name+"/at-cap", func(t *testing.T) {
			event := validAuthorizationDecisionEvent(OutcomeAllow)
			leaves := event.Payload.auditPayloadLeaves()
			leaves[leaf.name] = strings.Repeat("x", leaf.cap)
			if leaf.name == "sampled" {
				leaves[leaf.name] = "false"
			}
			event.Payload = retainedMapPayload{leaves: leaves}
			require.NoError(t, Validate(event))
		})
	}
}

func TestAuthorizationDecisionEnvelope_RenderAndSinkAgree(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"project", "agent"} {
		for _, outcome := range []Outcome{OutcomeAllow, OutcomeDeny} {
			t.Run(kind+"/"+string(outcome), func(t *testing.T) {
				event := validAuthorizationDecisionEvent(outcome)
				event.Resource.Kind = kind
				event.Resource.ID = kind + "-1"
				want, err := Render(event)
				require.NoError(t, err)
				// One concrete local capture handler, no decorators or exporter.
				handler := &captureSlogHandler{}
				sink, err := NewSlogSink(slog.New(handler))
				require.NoError(t, err)
				require.NoError(t, sink.Emit(context.Background(), event))
				records := handler.Records()
				require.Len(t, records, 1)
				assert.Equal(t, EventName, records[0].Message)
				assert.Equal(t, event.OccurredAt, records[0].Time)
				level := slog.LevelInfo
				if outcome == OutcomeDeny {
					level = slog.LevelWarn
				}
				assert.Equal(t, level, records[0].Level)
				got, err := json.Marshal(slogRecordMap(t, records[0]))
				require.NoError(t, err)
				assert.JSONEq(t, string(want), string(got))
				var envelope map[string]any
				require.NoError(t, json.Unmarshal(got, &envelope))
				assert.Equal(t, map[string]any{"kind": kind, "id": kind + "-1"}, envelope["resource"])
				assert.NotContains(t, string(got), "raw_credential")
				assert.NotContains(t, string(got), "operation_id")
			})
		}
	}
}

func TestCatalogAdditionalResourceKindsAreDefensiveCopies(t *testing.T) {
	t.Parallel()

	want := Catalog()
	require.Len(t, want, 2)
	assert.Nil(t, want[0].AdditionalResourceKinds)
	require.Equal(t, []string{"agent"}, want[1].AdditionalResourceKinds)
	for _, boundary := range []string{"enumeration", "lookup"} {
		t.Run(boundary, func(t *testing.T) {
			var entry CatalogEntry
			if boundary == "enumeration" {
				entry = Catalog()[1]
			} else {
				var ok bool
				entry, ok = catalogEntry("authorization", "decide")
				require.True(t, ok)
			}
			require.Equal(t, []string{"agent"}, entry.AdditionalResourceKinds)
			entry.AdditionalResourceKinds[0] = "broker"
			entry.AdditionalResourceKinds = append(entry.AdditionalResourceKinds, "skill")
			entry.ResourceKind = "user"
			assert.Equal(t, want, Catalog())
			fresh, ok := catalogEntry("authorization", "decide")
			require.True(t, ok)
			assert.Equal(t, "project", fresh.ResourceKind)
			assert.Equal(t, []string{"agent"}, fresh.AdditionalResourceKinds)
			for _, kind := range []string{"project", "agent", "broker", "skill", "user"} {
				event := validAuthorizationDecisionEvent(OutcomeAllow)
				event.Resource.Kind = kind
				if kind == "project" || kind == "agent" {
					require.NoError(t, Validate(event))
				} else {
					require.Error(t, Validate(event))
				}
			}
		})
	}
	boundary, ok := catalogEntry("access_boundary", "create")
	require.True(t, ok)
	assert.Nil(t, boundary.AdditionalResourceKinds)
	missing, ok := catalogEntry("unknown", "unknown")
	assert.False(t, ok)
	assert.Equal(t, CatalogEntry{}, missing)
}

func TestCatalogResourceKindMembershipContract(t *testing.T) {
	t.Parallel()

	for _, contract := range []struct {
		name    string
		outcome Outcome
	}{
		{"authorization-allow", OutcomeAllow},
		{"authorization-deny", OutcomeDeny},
		{"access-boundary-create", OutcomeSucceeded},
	} {
		for _, kind := range []string{"project", "agent", "access_constraint", "", "Agent", "PROJECT", "agent-child", "project-child", "broker", "user", "skill", "template", "unknown-kind-canary"} {
			name := kind
			if name == "" {
				name = "empty"
			}
			t.Run(contract.name+"/"+name, func(t *testing.T) {
				event := validAuthorizationDecisionEvent(contract.outcome)
				wantValid := kind == "project" || kind == "agent"
				if contract.outcome == OutcomeSucceeded {
					event = validCreateEvent(t)
					wantValid = kind == "access_constraint"
				}
				event.Resource.Kind = kind
				handler := &captureSlogHandler{}
				sink, err := NewSlogSink(slog.New(handler))
				require.NoError(t, err)
				err = Validate(event)
				if wantValid {
					require.NoError(t, err)
					_, err = Render(event)
					require.NoError(t, err)
					require.NoError(t, sink.Emit(context.Background(), event))
					require.Len(t, handler.Records(), 1)
					return
				}
				require.Error(t, err)
				var validationErr *ValidationError
				require.ErrorAs(t, err, &validationErr)
				assert.Equal(t, "resource.kind", validationErr.Field)
				if kind != "" {
					assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, kind)
				}
				rendered, err := Render(event)
				require.Error(t, err)
				assert.Nil(t, rendered)
				require.ErrorAs(t, err, &validationErr)
				if kind != "" {
					assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, kind)
				}
				err = sink.Emit(context.Background(), event)
				require.Error(t, err)
				require.ErrorAs(t, err, &validationErr)
				if kind != "" {
					assertValidationErrorDoesNotRetain(t, err, validationErr.Field, validationErr.Rule, kind)
				}
				assert.Empty(t, handler.Records(), "rejected resource kinds must not reach Handle")
			})
		}
	}
}

func TestAuthorizationDecisionEnvelope_OptionalDeniedBy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		outcome        Outcome
		value          any
		present, valid bool
	}{
		{"allow-absent", OutcomeAllow, nil, false, true},
		{"deny-unattributed-absent", OutcomeDeny, nil, false, true},
		{"deny-attributed-present", OutcomeDeny, "delegation_ceiling", true, true},
		{"absent-leaf", OutcomeDeny, nil, false, true},
		{"present-empty", OutcomeDeny, "", true, false},
		{"present-nonstring", OutcomeDeny, 7, true, false},
		{"present-invalid-utf8", OutcomeDeny, string([]byte{0xff}), true, false},
		{"present-control", OutcomeDeny, "cause\ncanary", true, false},
		{"present-over-cap", OutcomeDeny, strings.Repeat("x", 65), true, false},
		{"present-at-cap", OutcomeDeny, strings.Repeat("x", 64), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := validAuthorizationDecisionEvent(tc.outcome)
			payload := event.Payload.(AuthorizationDecisionPayload)
			payload.DeniedBy = ""
			event.Payload = payload
			assert.NotContains(t, event.Payload.auditPayloadLeaves(), "denied_by")
			leaves := event.Payload.auditPayloadLeaves()
			if tc.present {
				leaves["denied_by"] = tc.value
			}
			event.Payload = retainedMapPayload{leaves: leaves}
			handler := &captureSlogHandler{}
			sink, err := NewSlogSink(slog.New(handler))
			require.NoError(t, err)
			validationErr := Validate(event)
			rendered, renderErr := Render(event)
			sinkErr := sink.Emit(context.Background(), event)
			if tc.valid {
				require.NoError(t, validationErr)
				require.NoError(t, renderErr)
				require.NoError(t, sinkErr)
				require.Len(t, handler.Records(), 1)
				var got map[string]any
				require.NoError(t, json.Unmarshal(rendered, &got))
				gotPayload := got["payload"].(map[string]any)
				if tc.present {
					assert.Equal(t, tc.value, gotPayload["denied_by"])
				} else {
					assert.NotContains(t, gotPayload, "denied_by")
				}
				return
			}
			assert.Nil(t, rendered)
			assert.Empty(t, handler.Records())
			for _, rejected := range []error{validationErr, renderErr, sinkErr} {
				var typed *ValidationError
				require.ErrorAs(t, rejected, &typed)
				assert.Equal(t, "payload.denied_by", typed.Field)
				if value, ok := tc.value.(string); ok && value != "" {
					assertValidationErrorDoesNotRetain(t, rejected, typed.Field, typed.Rule, value)
				}
			}
		})
	}
}

func validAuthorizationDecisionEvent(outcome Outcome) EnvelopeV1 {
	deniedBy := ""
	if outcome == OutcomeDeny {
		deniedBy = "delegation_ceiling"
	}
	return EnvelopeV1{
		SchemaVersion: SchemaVersion,
		EventID:       "11111111-1111-4111-8111-111111111111",
		OccurredAt:    time.Date(2026, 10, 1, 12, 34, 56, 123456789, time.UTC),
		Family:        "authorization",
		Action:        "decide",
		Phase:         PhaseDecision,
		Outcome:       outcome,
		Severity:      severityForOutcome(outcome),
		CorrelationID: "corr-1",
		Principal:     &IdentityRef{Kind: IdentityUser, ID: "user-1"},
		Resource:      &ResourceRef{Kind: "project", ID: "project-1", Scope: ResourceScopeSystem},
		Payload: AuthorizationDecisionPayload{
			PermissionID: "fixture.permission", Permission: "read", Reason: "fixture decision",
			DeniedBy: deniedBy, Sampled: "false",
		},
	}
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

func containsPhaseOutcome(pairs []PhaseOutcome, phase Phase, outcome Outcome) bool {
	for _, pair := range pairs {
		if pair.Phase == phase && pair.Outcome == outcome {
			return true
		}
	}
	return false
}

func severityForOutcome(outcome Outcome) Severity {
	if outcome == OutcomeDeny || outcome == OutcomeFailed {
		return SeverityWarning
	}
	return SeverityInfo
}

func assertValidationErrorDoesNotRetain(t *testing.T, err error, field, rule, canary string) {
	t.Helper()
	assert.NotContains(t, err.Error(), canary)
	assert.NotContains(t, field, canary)
	assert.NotContains(t, rule, canary)
	assert.NotContains(t, fmt.Sprintf("%#v", err), canary)
}

func validCreateEvent(t *testing.T) EnvelopeV1 {
	t.Helper()
	event, err := buildAccessBoundaryCreate(
		AuditOperationContext{CorrelationID: "corr-1"},
		AccessBoundaryCreateInput{
			Principal:      IdentityRef{Kind: IdentityUser, ID: "user-1"},
			ConstraintID:   "constraint-1",
			Scope:          ResourceScopeProject,
			ProjectID:      "project-1",
			Classification: BoundaryTighten,
		},
		uuid.MustParse("11111111-1111-4111-8111-111111111111").String(),
		time.Date(2026, 10, 1, 12, 34, 56, 123456789, time.UTC),
	)
	require.NoError(t, err)
	return event
}
