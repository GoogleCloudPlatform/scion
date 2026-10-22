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
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
)

// BrokerFamily is the audit family of the Runtime Broker credential
// lifecycle and broker authentication failures (remaining-audit P4 slice 1).
const BrokerFamily = "broker"

// BrokerResourceKind is the resource kind of every broker family event.
const BrokerResourceKind = "runtime_broker"

// BrokerAction is one converted broker family action.
type BrokerAction string

const (
	BrokerActionAuthFailure BrokerAction = "auth_failure"
	BrokerActionRegister    BrokerAction = "register"
	BrokerActionJoin        BrokerAction = "join"
	BrokerActionRotate      BrokerAction = "rotate"
	BrokerActionDeregister  BrokerAction = "deregister"
)

// BrokerFailureCode is the closed, classified reason of a broker
// authentication or join failure. It replaces free-form error text: no
// err.Error() string ever enters a broker envelope.
type BrokerFailureCode string

const (
	BrokerFailureMissingCredentials BrokerFailureCode = "missing_credentials"
	BrokerFailureMalformed          BrokerFailureCode = "malformed"
	BrokerFailureInvalidSignature   BrokerFailureCode = "invalid_signature"
	BrokerFailureExpired            BrokerFailureCode = "expired"
	BrokerFailureClockSkew          BrokerFailureCode = "clock_skew"
	BrokerFailureUnknownBroker      BrokerFailureCode = "unknown_broker"
	BrokerFailureRevokedOrDisabled  BrokerFailureCode = "revoked_or_disabled"
	BrokerFailureReplay             BrokerFailureCode = "replay"
	BrokerFailureTokenInvalid       BrokerFailureCode = "token_invalid"
	BrokerFailureTokenExpired       BrokerFailureCode = "token_expired"
	BrokerFailureRefused            BrokerFailureCode = "refused"
	BrokerFailureInternal           BrokerFailureCode = "internal"
	BrokerFailureOther              BrokerFailureCode = "other"
)

var brokerFailureCodes = []BrokerFailureCode{
	BrokerFailureMissingCredentials, BrokerFailureMalformed, BrokerFailureInvalidSignature,
	BrokerFailureExpired, BrokerFailureClockSkew, BrokerFailureUnknownBroker,
	BrokerFailureRevokedOrDisabled, BrokerFailureReplay, BrokerFailureTokenInvalid,
	BrokerFailureTokenExpired, BrokerFailureRefused, BrokerFailureInternal, BrokerFailureOther,
}

// BrokerFailureCodes returns every declared failure code, in catalog order.
func BrokerFailureCodes() []BrokerFailureCode {
	return append([]BrokerFailureCode(nil), brokerFailureCodes...)
}

// BrokerRegisterOperation distinguishes a first registration from a
// re-registration of an existing broker.
type BrokerRegisterOperation string

const (
	BrokerRegisterNew        BrokerRegisterOperation = "register"
	BrokerRegisterReregister BrokerRegisterOperation = "reregister"
)

// CredentialKindUnrepresentable is the credential_kind payload value set
// when the request credential exists but cannot be represented as a
// validated CredentialRef. The credential leaf is then omitted, and this
// value records that attribution existed rather than letting it disappear.
const CredentialKindUnrepresentable = "unrepresentable"

const (
	brokerSourceIPMaxBytes  = 45
	brokerUserAgentMaxBytes = 256
)

// brokerCredentialKindValues is credentialmeta.Kinds() plus
// CredentialKindUnrepresentable.
func brokerCredentialKindValues() []string {
	kinds := credentialmeta.Kinds()
	values := make([]string, 0, len(kinds)+1)
	for _, kind := range kinds {
		values = append(values, string(kind))
	}
	return append(values, CredentialKindUnrepresentable)
}

func brokerFailureCodeValues() []string {
	values := make([]string, len(brokerFailureCodes))
	for i, code := range brokerFailureCodes {
		values[i] = string(code)
	}
	return values
}

var (
	brokerFailureCodeLeaf = PayloadLeafSchema{Name: "failure_code", Type: PayloadString, MaxBytes: 32, AllowedValues: brokerFailureCodeValues()}
	// credential_kind is set ONLY when the credential exists but is
	// unrepresentable, so it never duplicates credential.kind.
	brokerCredentialKindLeaf = PayloadLeafSchema{Name: "credential_kind", Type: PayloadString, MaxBytes: 32, AllowedValues: brokerCredentialKindValues()}
	// source_ip is caller-influenced, exactly as the legacy ip_address
	// attribute was: it is the first X-Forwarded-For hop, else X-Real-IP,
	// else the connection's remote address. The builder emits only a
	// netip-canonical address (zone stripped); unparseable input is never
	// emitted and sets source_ip_unparsed instead.
	brokerSourceIPLeaf         = PayloadLeafSchema{Name: "source_ip", Type: PayloadString, MaxBytes: brokerSourceIPMaxBytes}
	brokerSourceIPUnparsedLeaf = brokerTrueLeaf("source_ip_unparsed")
	// user_agent is caller-chosen text, emitted on auth_failure only (the
	// only legacy event that carried it). The builder replaces invalid
	// UTF-8, control runes and Unicode format (Cf) runes with U+FFFD
	// (user_agent_sanitized) and
	// truncates at a UTF-8 boundary to 256 bytes (user_agent_truncated).
	brokerUserAgentLeaf          = PayloadLeafSchema{Name: "user_agent", Type: PayloadString, MaxBytes: brokerUserAgentMaxBytes}
	brokerUserAgentTruncatedLeaf = brokerTrueLeaf("user_agent_truncated")
	brokerUserAgentSanitizedLeaf = brokerTrueLeaf("user_agent_sanitized")
	// broker_id_invalid is set when the caller-claimed broker ID fails the
	// resource.id bound or holds a Unicode format (Cf) rune (R15), in which
	// case the resource is omitted.
	brokerIDInvalidLeaf  = brokerTrueLeaf("broker_id_invalid")
	brokerOperationLeaf  = PayloadLeafSchema{Name: "operation", Type: PayloadString, MaxBytes: 16, AllowedValues: []string{string(BrokerRegisterNew), string(BrokerRegisterReregister)}}
	brokerSystemScope    = []ResourceScopeSchema{{Scope: ResourceScopeSystem, ProjectID: ResourceProjectIDOmitted}}
	brokerEnvelopeLeaves = []string{"schema_version", "event_id", "occurred_at", "family", "action", "phase", "outcome", "severity", "correlation_id"}
)

func brokerTrueLeaf(name string) PayloadLeafSchema {
	return PayloadLeafSchema{Name: name, Type: PayloadString, MaxBytes: 4, AllowedValues: []string{"true"}}
}

func brokerEnvelopeLeavesWith(extra ...string) []string {
	return append(append([]string(nil), brokerEnvelopeLeaves...), extra...)
}

// BrokerPayload is the exact v1 payload for broker family events. Only
// set fields are emitted; booleans are emitted as the string "true".
type BrokerPayload struct {
	FailureCode        BrokerFailureCode
	CredentialKind     string
	SourceIP           string
	SourceIPUnparsed   bool
	UserAgent          string
	UserAgentTruncated bool
	UserAgentSanitized bool
	BrokerIDInvalid    bool
	Operation          BrokerRegisterOperation
}

func (p BrokerPayload) auditPayloadLeaves() map[string]any {
	leaves := map[string]any{}
	if p.FailureCode != "" {
		leaves["failure_code"] = string(p.FailureCode)
	}
	if p.CredentialKind != "" {
		leaves["credential_kind"] = p.CredentialKind
	}
	if p.SourceIP != "" {
		leaves["source_ip"] = p.SourceIP
	}
	if p.SourceIPUnparsed {
		leaves["source_ip_unparsed"] = "true"
	}
	if p.UserAgent != "" {
		leaves["user_agent"] = p.UserAgent
	}
	if p.UserAgentTruncated {
		leaves["user_agent_truncated"] = "true"
	}
	if p.UserAgentSanitized {
		leaves["user_agent_sanitized"] = "true"
	}
	if p.BrokerIDInvalid {
		leaves["broker_id_invalid"] = "true"
	}
	if p.Operation != "" {
		leaves["operation"] = string(p.Operation)
	}
	return leaves
}

// BrokerEventInput holds the typed inputs of one broker family event. There
// is no free-form field: every value maps to a declared, bounded leaf.
type BrokerEventInput struct {
	Action BrokerAction
	// Failed selects the failure pair. It is meaningful for join only.
	Failed bool
	// FailureCode is required for auth_failure and a failed join, and must
	// be empty otherwise.
	FailureCode BrokerFailureCode
	// Principal is required for register, rotate and deregister, and must
	// be nil for auth_failure and join.
	Principal *IdentityRef
	// BrokerID is the affected broker. For auth_failure and join it is the
	// caller's claimed ID, omitted from the envelope (with
	// broker_id_invalid=true) when it fails the resource.id bound.
	BrokerID string
	// ProjectID selects project scope; register only.
	ProjectID string
	// Credential is the validated request credential, if any.
	Credential *CredentialRef
	// CredentialUnrepresentable records a request credential that could
	// not be validated as a CredentialRef. Credential must then be nil.
	CredentialUnrepresentable bool
	// SourceIP is the raw client address; the builder canonicalizes it.
	SourceIP string
	// UserAgent is the raw User-Agent; auth_failure only. The builder
	// sanitizes and truncates it.
	UserAgent string
	// Operation is register only.
	Operation BrokerRegisterOperation
	// RequestID is the current request's ID. It becomes request.id only
	// when it equals the operation's correlation ID.
	RequestID string
}

// BuildBrokerEvent builds and validates one broker family event. The
// correlation comes from the operation context on ctx; without one the
// build fails and no event exists.
func BuildBrokerEvent(ctx context.Context, input BrokerEventInput) (EnvelopeV1, error) {
	operation, err := requireOperation(ctx)
	if err != nil {
		return EnvelopeV1{}, err
	}
	return buildBrokerEvent(operation, input, uuid.NewString(), time.Now().UTC())
}

func buildBrokerEvent(operation AuditOperationContext, input BrokerEventInput, eventID string, occurredAt time.Time) (EnvelopeV1, error) {
	event := EnvelopeV1{
		SchemaVersion: SchemaVersion,
		EventID:       eventID,
		OccurredAt:    occurredAt,
		Family:        BrokerFamily,
		Action:        string(input.Action),
		CorrelationID: operation.CorrelationID,
	}
	payload := BrokerPayload{FailureCode: input.FailureCode, Operation: input.Operation}

	claimedID := false
	switch input.Action {
	case BrokerActionAuthFailure:
		event.Phase, event.Outcome, event.Severity = PhaseDecision, OutcomeDeny, SeverityWarning
		claimedID = true
	case BrokerActionJoin:
		claimedID = true
		if input.Failed {
			event.Phase, event.Outcome, event.Severity = PhaseFailure, OutcomeFailed, SeverityWarning
			// The catalog has no per-pair required leaves, so the builder
			// enforces that a failed join is classified.
			if input.FailureCode == "" {
				return EnvelopeV1{}, invalid("payload.failure_code", "is required for a failed join")
			}
		} else {
			event.Phase, event.Outcome, event.Severity = PhaseCommit, OutcomeSucceeded, SeverityInfo
			if input.FailureCode != "" {
				return EnvelopeV1{}, invalid("payload.failure_code", "must be omitted for a successful join")
			}
		}
	case BrokerActionRegister, BrokerActionRotate, BrokerActionDeregister:
		event.Phase, event.Outcome, event.Severity = PhaseCommit, OutcomeSucceeded, SeverityInfo
	default:
		return EnvelopeV1{}, invalid("action", "must be a declared broker action")
	}
	if input.Failed && input.Action != BrokerActionJoin {
		return EnvelopeV1{}, invalid("phase_outcome", "must be allowed by the catalog entry")
	}
	if input.Action != BrokerActionJoin && input.Action != BrokerActionAuthFailure && input.FailureCode != "" {
		return EnvelopeV1{}, invalid("payload", "contains an undeclared leaf")
	}

	if claimedID {
		if input.Principal != nil {
			return EnvelopeV1{}, invalid("principal", "must be omitted for the action")
		}
		if validateBoundedString("resource.id", input.BrokerID, 128) == nil && !containsFormatRune(input.BrokerID) {
			event.Resource = &ResourceRef{Kind: BrokerResourceKind, ID: input.BrokerID, Scope: ResourceScopeSystem}
		} else {
			payload.BrokerIDInvalid = true
		}
	} else {
		event.Principal = cloneIdentity(input.Principal)
		scope := ResourceScopeSystem
		if input.ProjectID != "" {
			scope = ResourceScopeProject
		}
		event.Resource = &ResourceRef{Kind: BrokerResourceKind, ID: input.BrokerID, Scope: scope, ProjectID: input.ProjectID}
	}
	if input.ProjectID != "" && input.Action != BrokerActionRegister {
		return EnvelopeV1{}, invalid("resource.project_id", "must be omitted for the resource scope")
	}

	// request.id is set only when the current request ID IS the
	// correlation. On re-entry (an outer operation kept by
	// auditOperationMiddleware) the two differ, and auditevent.Validate
	// rejects request.id != correlation_id (validate.go:185 at fce92df9),
	// so the request ref is omitted there.
	if input.RequestID != "" && input.RequestID == operation.CorrelationID {
		event.Request = &RequestRef{ID: input.RequestID}
	}

	if input.CredentialUnrepresentable {
		if input.Credential != nil {
			return EnvelopeV1{}, invalid("credential", "must be omitted when unrepresentable")
		}
		payload.CredentialKind = CredentialKindUnrepresentable
	}
	event.Credential = cloneCredential(input.Credential)

	payload.SourceIP, payload.SourceIPUnparsed = canonicalSourceIP(input.SourceIP)

	if input.UserAgent != "" {
		if input.Action != BrokerActionAuthFailure {
			return EnvelopeV1{}, invalid("payload", "contains an undeclared leaf")
		}
		payload.UserAgent, payload.UserAgentSanitized, payload.UserAgentTruncated = sanitizeUserAgent(input.UserAgent, brokerUserAgentMaxBytes)
	}

	event.Payload = payload
	if err := Validate(event); err != nil {
		return EnvelopeV1{}, err
	}
	return event, nil
}

// canonicalSourceIP returns the netip-canonical form of a client address
// given as host:port or a bare address, with any zone stripped. Raw text
// never leaves this function: an address that does not parse yields
// unparsed=true and no value. Empty input yields neither.
func canonicalSourceIP(raw string) (ip string, unparsed bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", raw != ""
	}
	if addrPort, err := netip.ParseAddrPort(trimmed); err == nil {
		return addrPort.Addr().WithZone("").String(), false
	}
	if addr, err := netip.ParseAddr(trimmed); err == nil {
		return addr.WithZone("").String(), false
	}
	return "", true
}

// containsFormatRune reports whether s holds a Unicode format character
// (category Cf: bidi overrides and isolates such as U+202E and U+2066-2069,
// zero-width characters such as U+200B). A caller-claimed broker ID holding
// one fails the Q2 bound (architect ruling R15), so Trojan-Source-style text
// never becomes a resource ID. The validator itself is unchanged.
func containsFormatRune(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) >= 0
}

// sanitizeUserAgent replaces invalid UTF-8, control runes and Unicode
// format (Cf) runes (R15) with U+FFFD and truncates the result at a UTF-8
// boundary to at most maxBytes.
// sanitized reports a replacement inside the returned value; truncated
// reports that input was cut.
func sanitizeUserAgent(raw string, maxBytes int) (value string, sanitized, truncated bool) {
	var b strings.Builder
	firstReplacement := -1
	for i := 0; i < len(raw); {
		if b.Len() > maxBytes {
			truncated = true
			break
		}
		r, size := utf8.DecodeRuneInString(raw[i:])
		if (r == utf8.RuneError && size <= 1) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			if firstReplacement < 0 {
				firstReplacement = b.Len()
			}
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(raw[i : i+size])
		}
		i += size
	}
	value = b.String()
	if len(value) > maxBytes {
		truncated = true
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		value = value[:cut]
	}
	sanitized = firstReplacement >= 0 && firstReplacement < len(value)
	return value, sanitized, truncated
}
