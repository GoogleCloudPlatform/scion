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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var brokerFixtureTime = time.Date(2026, 10, 22, 3, 30, 0, 0, time.UTC)

const brokerFixtureEventID = "22222222-2222-4222-8222-222222222222"

// validBrokerEnvelopeFixture is a minimal valid envelope for one broker
// action, for catalog-wide validation tables.
func validBrokerEnvelopeFixture(action string) EnvelopeV1 {
	event := EnvelopeV1{
		SchemaVersion: SchemaVersion,
		EventID:       brokerFixtureEventID,
		OccurredAt:    brokerFixtureTime,
		Family:        BrokerFamily,
		Action:        action,
		Phase:         PhaseCommit,
		Outcome:       OutcomeSucceeded,
		Severity:      SeverityInfo,
		CorrelationID: "corr-1",
		Principal:     &IdentityRef{Kind: IdentityUser, ID: "user-1"},
		Resource:      &ResourceRef{Kind: BrokerResourceKind, ID: "broker-1", Scope: ResourceScopeSystem},
		Payload:       BrokerPayload{},
	}
	switch BrokerAction(action) {
	case BrokerActionAuthFailure:
		event.Phase, event.Outcome, event.Severity = PhaseDecision, OutcomeDeny, SeverityWarning
		event.Principal = nil
		event.Payload = BrokerPayload{FailureCode: BrokerFailureOther}
	case BrokerActionJoin:
		// A failed join, so failure_code is meaningful (the builder ties it
		// to the failure pair; see TestBuildBrokerEvent_RejectsContractViolations).
		event.Phase, event.Outcome, event.Severity = PhaseFailure, OutcomeFailed, SeverityWarning
		event.Principal = nil
		event.Payload = BrokerPayload{FailureCode: BrokerFailureInternal}
	}
	return event
}

func buildBrokerFixture(t *testing.T, input BrokerEventInput) EnvelopeV1 {
	t.Helper()
	event, err := buildBrokerEvent(AuditOperationContext{CorrelationID: "corr-1"}, input, brokerFixtureEventID, brokerFixtureTime)
	require.NoError(t, err)
	return event
}

func renderBrokerJSON(t *testing.T, event EnvelopeV1) map[string]any {
	t.Helper()
	raw, err := Render(event)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestBrokerCatalogEntriesSnapshot(t *testing.T) {
	t.Parallel()

	entries := Catalog()
	require.Len(t, entries, catalogEntryCount)
	base := []string{"schema_version", "event_id", "occurred_at", "family", "action", "phase", "outcome", "severity", "correlation_id"}
	withActors := append(append([]string(nil), base...), "principal", "resource")
	system := []ResourceScopeSchema{{Scope: ResourceScopeSystem, ProjectID: ResourceProjectIDOmitted}}
	codes := []string{"missing_credentials", "malformed", "invalid_signature", "expired", "clock_skew", "unknown_broker",
		"revoked_or_disabled", "replay", "token_invalid", "token_expired", "refused", "internal", "other"}
	failureCode := PayloadLeafSchema{Name: "failure_code", Type: PayloadString, MaxBytes: 32, AllowedValues: codes}
	credentialKind := PayloadLeafSchema{Name: "credential_kind", Type: PayloadString, MaxBytes: 32, AllowedValues: []string{
		"interactive", "uat", "agent_jwt", "federation", "broker", "dev", "delegated_agent", "workload_delegation", "unrepresentable"}}
	sourceIP := PayloadLeafSchema{Name: "source_ip", Type: PayloadString, MaxBytes: 45}
	flag := func(name string) PayloadLeafSchema {
		return PayloadLeafSchema{Name: name, Type: PayloadString, MaxBytes: 4, AllowedValues: []string{"true"}}
	}
	structured := []Destination{DestinationStructuredLog}

	assert.Equal(t, []CatalogEntry{{
		Family:                 "broker",
		Action:                 "auth_failure",
		AllowedPairs:           []PhaseOutcome{{Phase: PhaseDecision, Outcome: OutcomeDeny}},
		ResourceKind:           "runtime_broker",
		RequiredEnvelopeLeaves: base,
		ResourceScopes:         system,
		RequiredPayloadLeaves:  []PayloadLeafSchema{failureCode},
		OptionalPayloadLeaves: []PayloadLeafSchema{credentialKind, sourceIP, flag("source_ip_unparsed"),
			{Name: "user_agent", Type: PayloadString, MaxBytes: 256}, flag("user_agent_truncated"), flag("user_agent_sanitized"),
			flag("broker_id_invalid")},
		Destinations: structured,
	}, {
		Family:                 "broker",
		Action:                 "register",
		AllowedPairs:           []PhaseOutcome{{Phase: PhaseCommit, Outcome: OutcomeSucceeded}},
		ResourceKind:           "runtime_broker",
		RequiredEnvelopeLeaves: withActors,
		ResourceScopes: []ResourceScopeSchema{
			{Scope: ResourceScopeSystem, ProjectID: ResourceProjectIDOmitted},
			{Scope: ResourceScopeProject, ProjectID: ResourceProjectIDRequired},
		},
		RequiredPayloadLeaves: []PayloadLeafSchema{},
		OptionalPayloadLeaves: []PayloadLeafSchema{credentialKind, sourceIP, flag("source_ip_unparsed"),
			{Name: "operation", Type: PayloadString, MaxBytes: 16, AllowedValues: []string{"register", "reregister"}}},
		Destinations: structured,
	}, {
		Family:                 "broker",
		Action:                 "join",
		AllowedPairs:           []PhaseOutcome{{Phase: PhaseCommit, Outcome: OutcomeSucceeded}, {Phase: PhaseFailure, Outcome: OutcomeFailed}},
		ResourceKind:           "runtime_broker",
		RequiredEnvelopeLeaves: base,
		ResourceScopes:         system,
		RequiredPayloadLeaves:  []PayloadLeafSchema{},
		OptionalPayloadLeaves:  []PayloadLeafSchema{failureCode, credentialKind, sourceIP, flag("source_ip_unparsed"), flag("broker_id_invalid")},
		Destinations:           structured,
	}, {
		Family:                 "broker",
		Action:                 "rotate",
		AllowedPairs:           []PhaseOutcome{{Phase: PhaseCommit, Outcome: OutcomeSucceeded}},
		ResourceKind:           "runtime_broker",
		RequiredEnvelopeLeaves: withActors,
		ResourceScopes:         system,
		RequiredPayloadLeaves:  []PayloadLeafSchema{},
		OptionalPayloadLeaves:  []PayloadLeafSchema{credentialKind, sourceIP, flag("source_ip_unparsed")},
		Destinations:           structured,
	}, {
		Family:                 "broker",
		Action:                 "deregister",
		AllowedPairs:           []PhaseOutcome{{Phase: PhaseCommit, Outcome: OutcomeSucceeded}},
		ResourceKind:           "runtime_broker",
		RequiredEnvelopeLeaves: withActors,
		ResourceScopes:         system,
		RequiredPayloadLeaves:  []PayloadLeafSchema{},
		OptionalPayloadLeaves:  []PayloadLeafSchema{credentialKind, sourceIP, flag("source_ip_unparsed")},
		Destinations:           structured,
	}}, entries[catalogIndex(t, entries, "authorization", "decide")+1:])

	gotCodes := make([]string, 0, len(BrokerFailureCodes()))
	for _, code := range BrokerFailureCodes() {
		gotCodes = append(gotCodes, string(code))
	}
	assert.Equal(t, codes, gotCodes, "the failure_code enum is closed and final (R12 Q3)")
}

// Q1: resource is required only when the catalog entry lists it. Both
// pre-existing entries list it, so their behavior is unchanged.
func TestValidateResourceRequiredOnlyWhenListed(t *testing.T) {
	t.Parallel()

	create := validCreateEvent(t)
	create.Resource = nil
	var verr *ValidationError
	require.ErrorAs(t, Validate(create), &verr)
	assert.Equal(t, "resource", verr.Field, "access_boundary/create still requires resource")

	decide := validAuthorizationDecisionEvent(OutcomeDeny)
	decide.Resource = nil
	require.ErrorAs(t, Validate(decide), &verr)
	assert.Equal(t, "resource", verr.Field, "authorization/decide still requires resource")

	for _, action := range []string{"register", "rotate", "deregister"} {
		event := validBrokerEnvelopeFixture(action)
		event.Resource = nil
		require.ErrorAs(t, Validate(event), &verr, action)
		assert.Equal(t, "resource", verr.Field, action)
	}
	for _, action := range []string{"auth_failure", "join"} {
		event := validBrokerEnvelopeFixture(action)
		event.Resource = nil
		assert.NoError(t, Validate(event), "%s admits an event without resource", action)

		// The payload is still validated when the resource is absent.
		event.Payload = BrokerPayload{FailureCode: "made_up"}
		require.ErrorAs(t, Validate(event), &verr, action)
		assert.Equal(t, "payload.failure_code", verr.Field, action)
		event.Payload = nil
		require.ErrorAs(t, Validate(event), &verr, action)
		assert.Equal(t, "payload", verr.Field, action)
	}
}

// T4: every converted action and pair builds, validates and renders with
// only declared leaves.
func TestBuildBrokerEvent_T4_EveryActionAndPairValidates(t *testing.T) {
	t.Parallel()

	user := &IdentityRef{Kind: IdentityUser, ID: "user-9"}
	cases := []struct {
		name         string
		input        BrokerEventInput
		wantPhase    string
		wantOutcome  string
		wantSeverity string
		wantPayload  map[string]any
		wantResource map[string]any
		principal    bool
	}{
		{
			name:         "auth_failure",
			input:        BrokerEventInput{Action: BrokerActionAuthFailure, FailureCode: BrokerFailureInvalidSignature, BrokerID: "broker-1", SourceIP: "203.0.113.7:4444", UserAgent: "scion-broker/1.0"},
			wantPhase:    "decision",
			wantOutcome:  "deny",
			wantSeverity: "warning",
			wantPayload:  map[string]any{"failure_code": "invalid_signature", "source_ip": "203.0.113.7", "user_agent": "scion-broker/1.0"},
			wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-1"},
		},
		{
			name:         "register system scope",
			input:        BrokerEventInput{Action: BrokerActionRegister, Principal: user, BrokerID: "broker-1", SourceIP: "203.0.113.7", Operation: BrokerRegisterNew},
			wantPhase:    "commit",
			wantOutcome:  "succeeded",
			wantSeverity: "info",
			wantPayload:  map[string]any{"source_ip": "203.0.113.7", "operation": "register"},
			wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-1"},
			principal:    true,
		},
		{
			name:         "register project scope",
			input:        BrokerEventInput{Action: BrokerActionRegister, Principal: &IdentityRef{Kind: IdentityUser, ID: "user-9"}, BrokerID: "broker-1", ProjectID: "project-1", Operation: BrokerRegisterReregister},
			wantPhase:    "commit",
			wantOutcome:  "succeeded",
			wantSeverity: "info",
			wantPayload:  map[string]any{"operation": "reregister"},
			wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-1", "project_id": "project-1"},
			principal:    true,
		},
		{
			name:         "join succeeded",
			input:        BrokerEventInput{Action: BrokerActionJoin, BrokerID: "broker-1", SourceIP: "[2001:db8::1]:443"},
			wantPhase:    "commit",
			wantOutcome:  "succeeded",
			wantSeverity: "info",
			wantPayload:  map[string]any{"source_ip": "2001:db8::1"},
			wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-1"},
		},
		{
			name:         "join failed",
			input:        BrokerEventInput{Action: BrokerActionJoin, Failed: true, FailureCode: BrokerFailureTokenExpired, BrokerID: "broker-1"},
			wantPhase:    "failure",
			wantOutcome:  "failed",
			wantSeverity: "warning",
			wantPayload:  map[string]any{"failure_code": "token_expired"},
			wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-1"},
		},
		{
			name:         "rotate by broker",
			input:        BrokerEventInput{Action: BrokerActionRotate, Principal: &IdentityRef{Kind: IdentityBroker, ID: "broker-1"}, BrokerID: "broker-1"},
			wantPhase:    "commit",
			wantOutcome:  "succeeded",
			wantSeverity: "info",
			wantPayload:  map[string]any{},
			wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-1"},
			principal:    true,
		},
		{
			name:         "deregister",
			input:        BrokerEventInput{Action: BrokerActionDeregister, Principal: user, BrokerID: "broker-1", CredentialUnrepresentable: true},
			wantPhase:    "commit",
			wantOutcome:  "succeeded",
			wantSeverity: "info",
			wantPayload:  map[string]any{"credential_kind": "unrepresentable"},
			wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-1"},
			principal:    true,
		},
	}
	allowedTop := map[string]bool{
		"schema_version": true, "event_id": true, "occurred_at": true, "family": true, "action": true,
		"phase": true, "outcome": true, "severity": true, "correlation_id": true, "request": true,
		"principal": true, "credential": true, "resource": true, "payload": true,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := buildBrokerFixture(t, tc.input)
			require.NoError(t, Validate(event))
			out := renderBrokerJSON(t, event)
			for key := range out {
				assert.True(t, allowedTop[key], "unlisted top-level key %q", key)
			}
			assert.Equal(t, "broker", out["family"])
			assert.Equal(t, string(tc.input.Action), out["action"])
			assert.Equal(t, tc.wantPhase, out["phase"])
			assert.Equal(t, tc.wantOutcome, out["outcome"])
			assert.Equal(t, tc.wantSeverity, out["severity"])
			assert.Equal(t, "corr-1", out["correlation_id"])
			assert.Equal(t, tc.wantPayload, out["payload"])
			assert.Equal(t, tc.wantResource, out["resource"])
			if tc.principal {
				assert.Contains(t, out, "principal")
			} else {
				assert.NotContains(t, out, "principal")
			}
			assert.NotContains(t, out, "request", "no request ID was given")
			assert.NotContains(t, out, "credential")
		})
	}
}

func TestBuildBrokerEvent_RejectsContractViolations(t *testing.T) {
	t.Parallel()

	user := &IdentityRef{Kind: IdentityUser, ID: "user-1"}
	cases := []struct {
		name  string
		input BrokerEventInput
		field string
	}{
		{"unknown action", BrokerEventInput{Action: "revoke", BrokerID: "b"}, "action"},
		{"auth_failure without failure code", BrokerEventInput{Action: BrokerActionAuthFailure, BrokerID: "b"}, "payload.failure_code"},
		{"auth_failure undeclared failure code", BrokerEventInput{Action: BrokerActionAuthFailure, BrokerID: "b", FailureCode: "fail_reason text"}, "payload.failure_code"},
		{"auth_failure with principal", BrokerEventInput{Action: BrokerActionAuthFailure, BrokerID: "b", FailureCode: BrokerFailureOther, Principal: user}, "principal"},
		{"join with principal", BrokerEventInput{Action: BrokerActionJoin, BrokerID: "b", Principal: user}, "principal"},
		{"successful join with failure code", BrokerEventInput{Action: BrokerActionJoin, BrokerID: "b", FailureCode: BrokerFailureOther}, "payload.failure_code"},
		{"failed join without failure code", BrokerEventInput{Action: BrokerActionJoin, BrokerID: "b", Failed: true}, "payload.failure_code"},
		{"failed join undeclared failure code", BrokerEventInput{Action: BrokerActionJoin, BrokerID: "b", Failed: true, FailureCode: "boom"}, "payload.failure_code"},
		{"register without principal", BrokerEventInput{Action: BrokerActionRegister, BrokerID: "b"}, "principal"},
		{"rotate without principal", BrokerEventInput{Action: BrokerActionRotate, BrokerID: "b"}, "principal"},
		{"deregister without principal", BrokerEventInput{Action: BrokerActionDeregister, BrokerID: "b"}, "principal"},
		{"register empty principal id", BrokerEventInput{Action: BrokerActionRegister, BrokerID: "b", Principal: &IdentityRef{Kind: IdentityUser}}, "principal.id"},
		{"register empty broker id", BrokerEventInput{Action: BrokerActionRegister, Principal: user}, "resource.id"},
		{"failed register", BrokerEventInput{Action: BrokerActionRegister, BrokerID: "b", Principal: user, Failed: true}, "phase_outcome"},
		{"rotate with failure code", BrokerEventInput{Action: BrokerActionRotate, BrokerID: "b", Principal: user, FailureCode: BrokerFailureOther}, "payload"},
		{"rotate with project", BrokerEventInput{Action: BrokerActionRotate, BrokerID: "b", Principal: user, ProjectID: "p"}, "resource.project_id"},
		{"rotate with operation", BrokerEventInput{Action: BrokerActionRotate, BrokerID: "b", Principal: user, Operation: BrokerRegisterNew}, "payload"},
		{"register with user agent", BrokerEventInput{Action: BrokerActionRegister, BrokerID: "b", Principal: user, UserAgent: "ua"}, "payload"},
		{"undeclared operation", BrokerEventInput{Action: BrokerActionRegister, BrokerID: "b", Principal: user, Operation: "path=embedded"}, "payload.operation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildBrokerEvent(AuditOperationContext{CorrelationID: "corr-1"}, tc.input, brokerFixtureEventID, brokerFixtureTime)
			var verr *ValidationError
			require.ErrorAs(t, err, &verr)
			assert.Equal(t, tc.field, verr.Field)
		})
	}

	unrepresentable := BrokerEventInput{Action: BrokerActionRotate, BrokerID: "b", Principal: user, CredentialUnrepresentable: true}
	ref, err := NewCredentialRef(CredentialRefInput{Kind: CredentialBroker, ID: "b"})
	require.NoError(t, err)
	unrepresentable.Credential = &ref
	_, err = buildBrokerEvent(AuditOperationContext{CorrelationID: "corr-1"}, unrepresentable, brokerFixtureEventID, brokerFixtureTime)
	assert.Error(t, err, "a credential cannot be both present and unrepresentable")
}

// Missing correlation: no operation context means no event.
func TestBuildBrokerEvent_RequiresOperationContext(t *testing.T) {
	t.Parallel()

	_, err := BuildBrokerEvent(context.Background(), BrokerEventInput{Action: BrokerActionAuthFailure, BrokerID: "b", FailureCode: BrokerFailureOther})
	var verr *ValidationError
	require.ErrorAs(t, err, &verr)
	assert.Equal(t, "operation_context", verr.Field)

	op, err := NewOperationContext("req-1")
	require.NoError(t, err)
	event, err := BuildBrokerEvent(ContextWithOperation(context.Background(), op), BrokerEventInput{
		Action: BrokerActionAuthFailure, BrokerID: "b", FailureCode: BrokerFailureOther, RequestID: "req-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "req-1", event.CorrelationID)
	require.NotNil(t, event.Request)
	assert.Equal(t, "req-1", event.Request.ID)
	assert.NoError(t, Validate(event))
}

// Q10 characterization: on re-entry the correlation is the outer operation
// and the current request ID differs, so request.id is omitted
// (validate.go:185 forbids request.id != correlation_id).
func TestBuildBrokerEvent_ReentryOmitsRequestID(t *testing.T) {
	t.Parallel()

	event := buildBrokerFixture(t, BrokerEventInput{Action: BrokerActionAuthFailure, BrokerID: "b", FailureCode: BrokerFailureOther, RequestID: "req-inner"})
	assert.Equal(t, "corr-1", event.CorrelationID)
	assert.Nil(t, event.Request)

	forced := event
	forced.Request = &RequestRef{ID: "req-inner"}
	var verr *ValidationError
	require.ErrorAs(t, Validate(forced), &verr)
	assert.Equal(t, "request.id", verr.Field, "the validator rule the omission follows")

	same := buildBrokerFixture(t, BrokerEventInput{Action: BrokerActionAuthFailure, BrokerID: "b", FailureCode: BrokerFailureOther, RequestID: "corr-1"})
	require.NotNil(t, same.Request)
	assert.Equal(t, "corr-1", same.Request.ID)
}

// Q2: a claimed broker ID that fails the resource.id bound is omitted and
// flagged, never dropped and never echoed.
func TestBuildBrokerEvent_ClaimedBrokerIDBound(t *testing.T) {
	t.Parallel()

	for _, action := range []BrokerAction{BrokerActionAuthFailure, BrokerActionJoin} {
		for name, claimed := range map[string]string{
			"too long":      strings.Repeat("x", 129) + "-canary",
			"control chars": "broker\x00canary",
			"invalid utf8":  "broker\xffcanary",
			"empty":         "",
			"bidi override": "broker\u202ecanary",
			"bidi isolate":  "broker\u2066canary\u2069",
			"zero width":    "broker\u200bcanary",
		} {
			t.Run(string(action)+"/"+name, func(t *testing.T) {
				input := BrokerEventInput{Action: action, BrokerID: claimed}
				if action == BrokerActionAuthFailure {
					input.FailureCode = BrokerFailureUnknownBroker
				}
				event := buildBrokerFixture(t, input)
				assert.Nil(t, event.Resource)
				out := renderBrokerJSON(t, event)
				assert.NotContains(t, out, "resource")
				assert.Equal(t, "true", out["payload"].(map[string]any)["broker_id_invalid"])
				raw, err := Render(event)
				require.NoError(t, err)
				assert.NotContains(t, string(raw), "canary")
			})
		}
		at := buildBrokerFixture(t, BrokerEventInput{Action: action, BrokerID: strings.Repeat("y", 128), FailureCode: func() BrokerFailureCode {
			if action == BrokerActionAuthFailure {
				return BrokerFailureOther
			}
			return ""
		}()})
		require.NotNil(t, at.Resource, "a 128-byte claimed ID is admitted")
		assert.Equal(t, strings.Repeat("y", 128), at.Resource.ID)
	}
}

// Q5: a validated credential is carried as the credential leaf; the
// decoration name and labels are never set by the hub mapping, and an
// unrepresentable credential is recorded only as credential_kind.
func TestBuildBrokerEvent_Credential(t *testing.T) {
	t.Parallel()

	ref, err := NewCredentialRef(CredentialRefInput{Kind: CredentialUAT, ID: "token-1", BoundaryKind: CredentialBoundaryProject, BoundaryProjectID: "project-1"})
	require.NoError(t, err)
	event := buildBrokerFixture(t, BrokerEventInput{Action: BrokerActionRotate, BrokerID: "b", Principal: &IdentityRef{Kind: IdentityUser, ID: "u"}, Credential: &ref})
	out := renderBrokerJSON(t, event)
	assert.Equal(t, map[string]any{"kind": "uat", "id": "token-1", "boundary_kind": "project", "boundary_project_id": "project-1"}, out["credential"])
	assert.NotContains(t, out["payload"], "credential_kind")
}

func TestCanonicalSourceIP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw      string
		want     string
		unparsed bool
	}{
		{"", "", false},
		{"203.0.113.7", "203.0.113.7", false},
		{" 203.0.113.7 ", "203.0.113.7", false},
		{"203.0.113.7:8080", "203.0.113.7", false},
		{"[2001:DB8:0:0::1]:443", "2001:db8::1", false},
		{"2001:0db8::0001", "2001:db8::1", false},
		{"fe80::1%eth0", "fe80::1", false},
		{"[fe80::1%25zone-canary]:80", "fe80::1", false},
		{"::ffff:192.0.2.1", "::ffff:192.0.2.1", false},
		{"   ", "", true},
		{"unknown-canary", "", true},
		{"203.0.113.7, 10.0.0.1", "", true},
		{"999.1.1.1", "", true},
	}
	for _, tc := range cases {
		ip, unparsed := canonicalSourceIP(tc.raw)
		assert.Equal(t, tc.want, ip, "%q", tc.raw)
		assert.Equal(t, tc.unparsed, unparsed, "%q", tc.raw)
		assert.NotContains(t, ip, "canary")
		assert.LessOrEqual(t, len(ip), brokerSourceIPMaxBytes)
	}

	event := buildBrokerFixture(t, BrokerEventInput{Action: BrokerActionJoin, BrokerID: "b", SourceIP: "garbage-canary"})
	raw, err := Render(event)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "canary", "raw source text never enters the envelope")
	assert.Equal(t, map[string]any{"source_ip_unparsed": "true"}, renderBrokerJSON(t, event)["payload"])
}

func TestSanitizeUserAgent(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 300)
	// 255 ASCII bytes then a 3-byte rune: the rune would end at byte 258, so
	// the cut lands before it.
	boundary := strings.Repeat("b", 255) + "€tail"
	cases := []struct {
		name          string
		raw           string
		want          string
		sanitized     bool
		truncated     bool
		wantMaxLength int
	}{
		{"plain", "scion-broker/1.0 (linux)", "scion-broker/1.0 (linux)", false, false, 256},
		{"tab", "a\tb", "a�b", true, false, 256},
		{"c1 control", "a\u0085b", "a�b", true, false, 256},
		{"invalid utf8", "a\xffb", "a�b", true, false, 256},
		{"genuine replacement char is not sanitization", "a�b", "a�b", false, false, 256},
		{"bidi override (Cf, R15)", "a\u202eb", "a�b", true, false, 256},
		{"bidi isolate (Cf, R15)", "a\u2066b\u2069c", "a�b�c", true, false, 256},
		{"zero width space (Cf, R15)", "a\u200bb", "a�b", true, false, 256},
		{"Cf straddling the cut", strings.Repeat("e", 254) + "\u202e", strings.Repeat("e", 254), false, true, 256},
		{"long", long, strings.Repeat("a", 256), false, true, 256},
		{"cut at rune boundary", boundary, strings.Repeat("b", 255), false, true, 256},
		{"replacement only beyond the cut", strings.Repeat("c", 300) + "\x00", strings.Repeat("c", 256), false, true, 256},
		{"exactly 256", strings.Repeat("d", 256), strings.Repeat("d", 256), false, false, 256},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, sanitized, truncated := sanitizeUserAgent(tc.raw, brokerUserAgentMaxBytes)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.sanitized, sanitized, "sanitized")
			assert.Equal(t, tc.truncated, truncated, "truncated")
			assert.LessOrEqual(t, len(got), tc.wantMaxLength)
			assert.NoError(t, validateBoundedString("payload.user_agent", got, brokerUserAgentMaxBytes))
		})
	}

	// Through the builder: the flags are emitted and the event validates.
	event := buildBrokerFixture(t, BrokerEventInput{Action: BrokerActionAuthFailure, BrokerID: "b", FailureCode: BrokerFailureOther, UserAgent: "x\ty" + long})
	payload := renderBrokerJSON(t, event)["payload"].(map[string]any)
	assert.Equal(t, "true", payload["user_agent_sanitized"])
	assert.Equal(t, "true", payload["user_agent_truncated"])
}
