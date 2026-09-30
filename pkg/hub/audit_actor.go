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

package hub

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ---------------------------------------------------------------------------
// Every mutation-audit writer builds its actor/credential/correlation
// snapshot through auditActorFromContext and applies it with ApplyActor, so
// the extraction logic and the fields it populates exist in exactly one
// place.
// ---------------------------------------------------------------------------

// AuditActor is the consolidated actor/credential/correlation snapshot
// recorded on a mutation audit record. It mirrors (and, via ApplyActor, feeds
// directly into) the actor-shaped fields of store.MutationAuditRecord.
type AuditActor struct {
	PrincipalKind  string
	PrincipalID    string
	CredentialKind string
	CredentialID   string

	// CredentialName/CredentialBoundaryKind/CredentialBoundaryProjectID/
	// CredentialLabels are E.1's descriptive decoration, snapshotted at audit
	// time (not a reference — see plan §3.2 "audits outlive tokens"). Empty
	// for credentials E.1 does not decorate (non-UAT).
	CredentialName              string
	CredentialBoundaryKind      string
	CredentialBoundaryProjectID string
	CredentialLabels            string // bounded JSON object, "" when absent

	// CorrelationID is the request ID shared with the request log and
	// decision audit for the same request (plan §3.1(4)).
	CorrelationID string

	// ExecutorKind/ExecutorID identify what is currently executing, as
	// distinct from the initiating principal/credential above (plan §3.5).
	// Empty for an ordinary live request.
	ExecutorKind string
	ExecutorID   string
}

// auditActorFromContext builds the actor/credential/correlation snapshot for
// mutation audit from request context. This is the single extraction point
// plan §3.3 names; every mutation-audit writer reaches it through
// ApplyActor.
func auditActorFromContext(ctx context.Context) AuditActor {
	var actor AuditActor

	if identity := GetIdentityFromContext(ctx); identity != nil {
		actor.PrincipalKind = identity.Type()
		actor.PrincipalID = identity.ID()
	}

	cred := GetCredentialContextFromContext(ctx)
	if cred.Kind != "" {
		actor.CredentialKind = string(cred.Kind)
		actor.CredentialID = cred.ID
	}
	if cred.Decoration != nil {
		actor.CredentialName = sanitizeForLog(cred.Decoration.TokenName, uatMaxNameBytes)
		actor.CredentialBoundaryKind = cred.Decoration.Boundary.Kind
		actor.CredentialBoundaryProjectID = cred.Decoration.Boundary.ProjectID
		actor.CredentialLabels = boundedLabelsJSON(cred.Decoration.Labels)
	}

	actor.CorrelationID = logging.RequestIDFromContext(ctx)

	if ec, ok := ExecutorContextFromContext(ctx); ok {
		actor.ExecutorKind = ec.Kind
		actor.ExecutorID = ec.ID
	}

	// INTEGRATION POINT (D.1 broker on-behalf-of, not yet landed — see the
	// E.2a handoff): D.1's shared hub-attested broker OBO context path will
	// add an identity.go accessor BrokerOnBehalfOfFromContext(ctx)
	// (BrokerOnBehalfOf, bool), set only by the shared broker-auth helper
	// (contextWithBrokerOnBehalfOf, unexported, no request-header path).
	// When present, PrincipalKind/PrincipalID above are already correct (the
	// effective user, still read from GetIdentityFromContext) and
	// CredentialKind/CredentialID above are already the broker's own
	// credential (GetCredentialContextFromContext, Kind "broker") — but that
	// conflates "whose action is this" with "which credential carried it."
	// Once BrokerOnBehalfOfFromContext exists, add a SEPARATE
	// BrokerActorID/BrokerCredentialID pair to AuditActor (and the matching
	// mutation-audit columns) sourced from it, rather than overwriting
	// PrincipalID/CredentialID — the effective user must never be replaced
	// by or merged with the broker's own identity. Do not adopt the name
	// above before D.1 lands it; confirm the final shape with pat-d-lead
	// first.

	return actor
}

// ApplyActor copies the AuditActor snapshot onto a MutationAuditRecord's
// actor/credential/correlation/executor fields, filling only fields the
// caller has not already set explicitly. This preserves existing callers
// that pre-populate specific actor fields (e.g. attributing a mutation to a
// resource's original creator rather than the live request's identity).
//
// Principal and credential are treated as one unit, not filled field by
// field: the credential fields (ID, type, name, boundary, labels) are filled
// only when this call also filled the principal, or when a caller-preset
// principal is exactly this AuditActor's own principal. Otherwise a caller
// that presets the principal to someone other than the live request's actor
// would still inherit the ambient request credential's ID, name, boundary,
// and labels — a record naming principal A with principal B's credential.
// CorrelationID and Executor* are independent of principal/credential and
// are always filled when empty.
func (a AuditActor) ApplyActor(record *store.MutationAuditRecord) {
	principalPreset := record.ActorPrincipalKind != "" || record.ActorPrincipalID != ""
	principalMatchesActor := record.ActorPrincipalKind == a.PrincipalKind && record.ActorPrincipalID == a.PrincipalID

	if record.ActorPrincipalKind == "" {
		record.ActorPrincipalKind = a.PrincipalKind
	}
	if record.ActorPrincipalID == "" {
		record.ActorPrincipalID = a.PrincipalID
	}

	if !principalPreset || principalMatchesActor {
		if record.ActorCredentialID == "" {
			record.ActorCredentialID = a.CredentialID
		}
		if record.ActorCredentialType == "" {
			record.ActorCredentialType = a.CredentialKind
		}
		if record.CredentialName == "" {
			record.CredentialName = a.CredentialName
		}
		if record.CredentialBoundaryKind == "" {
			record.CredentialBoundaryKind = a.CredentialBoundaryKind
		}
		if record.CredentialBoundaryProjectID == "" {
			record.CredentialBoundaryProjectID = a.CredentialBoundaryProjectID
		}
		if record.CredentialLabels == "" {
			record.CredentialLabels = a.CredentialLabels
		}
	}

	if record.CorrelationID == "" {
		record.CorrelationID = a.CorrelationID
	}
	if record.ExecutorKind == "" {
		record.ExecutorKind = a.ExecutorKind
	}
	if record.ExecutorID == "" {
		record.ExecutorID = a.ExecutorID
	}
}

// maxAuditLabelsBytes is the plan's bound on the serialized credential_labels
// audit snapshot (plan §3.2/§3.3/§3.6: "credential_labels JSON ≤ 1 KiB").
const maxAuditLabelsBytes = 1024

// auditLabelsTruncatedMarker is the fixed key added whenever any source
// entry was dropped — by the count cap or the byte cap — so a truncated
// snapshot is distinguishable from a complete one that merely happens to be
// small. A source key that would render as this exact string is renamed so
// it can never be mistaken for the marker.
const auditLabelsTruncatedMarker = "_truncated"

// boundedLabelsJSON renders a credential decoration's labels as a bounded,
// sanitized JSON object for audit snapshotting. It never trusts labels to
// already satisfy the bounded schema — a row written directly to the store
// (a legacy row, or one written outside ValidateCredentialMetadata) skips
// validation — so it applies the same per-key and per-value treatment
// CredentialDecoration.LogValue uses: a key that fails the bounded shape is
// sanitized like a value, and every value is sanitized and length-capped.
// The result is capped at uatMaxLabelCount entries (sorted by key, so the
// selection is deterministic) and at maxAuditLabelsBytes total — JSON
// escaping (e.g. a literal `<` becomes the six-byte escape sequence
// `\u003c`) can widen sanitized content past the per-field
// caps' sum, so the byte cap is a real path, not a defensive no-op. If
// entries are dropped by either cap, auditLabelsTruncatedMarker is set,
// so the output is always valid, bounded, unambiguously-marked JSON.
func boundedLabelsJSON(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	countTruncated := len(keys) > uatMaxLabelCount
	if countTruncated {
		keys = keys[:uatMaxLabelCount]
	}

	build := func(keys []string, truncated bool) (string, bool) {
		out := make(map[string]string, len(keys)+1)
		for _, k := range keys {
			renderKey := k
			if !isValidLabelKeyShape(k) {
				renderKey = sanitizeForLog(k, uatMaxLabelKeyBytes)
			}
			// A source key must never be able to render as the marker key:
			// that would let issuer-supplied content spoof a truncation
			// state that did not occur.
			if renderKey == auditLabelsTruncatedMarker {
				renderKey = renderKey + "_key"
			}
			// Two distinct source keys can sanitize or get renamed to the
			// same renderKey (for example two differently-invalid keys that
			// both sanitize to the same replacement string, or a legacy key
			// literally named "_truncated" colliding with another key
			// already renamed to "_truncated_key"). Overwriting silently
			// would lose one entry with no sign it happened, so a collision
			// is treated as a dropped entry instead.
			if _, exists := out[renderKey]; exists {
				truncated = true
				continue
			}
			out[renderKey] = sanitizeForLog(labels[k], uatMaxLabelValueBytes)
		}
		if truncated {
			out[auditLabelsTruncatedMarker] = "true"
		}
		b, err := json.Marshal(out)
		if err != nil {
			return "", false
		}
		return string(b), len(b) <= maxAuditLabelsBytes
	}

	if out, ok := build(keys, countTruncated); ok {
		return out
	}
	for len(keys) > 0 {
		keys = keys[:len(keys)-1]
		if out, ok := build(keys, true); ok {
			return out
		}
	}
	out, _ := build(nil, true)
	return out
}
