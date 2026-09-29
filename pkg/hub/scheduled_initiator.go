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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ---------------------------------------------------------------------------
// E.2b: initiator attribution for scheduled events and schedules (plan §3.5,
// rulings "E.2b field names" / "B.3 adoption" / Cutover).
//
// This file is the single write side (captureInitiatorAttribution /
// newInitiatorAttribution / reattributeInitiator) and read side
// (scheduledInitiator) for store.InitiatorAttribution. Authoring/mutation
// handlers call the write side; fire-time code and B.3 call the read side.
//
// Attribution is not authority: nothing here changes who is authorized to
// act at authoring or fire time. It only records, and later exposes, who
// initiated the work.
// ---------------------------------------------------------------------------

// initiatorAttributionVersion is the current attribution schema version
// written by E.2b. A stored row with AttributionVersion 0 (NULL) predates
// E.2b and reads as legacy_unknown.
const initiatorAttributionVersion = 1

// initiatorCredentialKindFor maps a live request's ambient CredentialKind to
// InitiatorAttribution's smaller, committed domain
// (session|uat|agent|legacy_unknown). Attribution never stores
// hub.CredentialKind's full domain: a caller of scheduledInitiator only ever
// sees one of these four values.
//
// Only a genuine interactive session maps to "session". Everything else —
// no ambient credential at all, federation, broker, or dev — maps to
// legacy_unknown, never to session (review R5): plan correction (c) and
// ruling Q2 ("a mutation without recordable provenance never falls back to
// the creator's interactive authority") both forbid treating unknown or
// absent provenance as an interactive-style credential.
func initiatorCredentialKindFor(kind CredentialKind) string {
	switch kind {
	case CredentialKindUAT:
		return store.InitiatorCredentialKindUAT
	case CredentialKindAgentJWT:
		return store.InitiatorCredentialKindAgent
	case CredentialKindInteractive:
		return store.InitiatorCredentialKindSession
	default:
		return store.InitiatorCredentialKindLegacyUnknown
	}
}

// hubCredentialKindForInitiator maps InitiatorAttribution's committed
// session|uat|agent|legacy_unknown domain back to hub.CredentialKind's
// vocabulary (interactive|uat|agent_jwt), for the one place — the
// scheduled-dispatch success audit — that records a credential kind in a
// column every other mutation-audit writer fills with hub.CredentialKind
// (audit_actor.go:auditActorFromContext). Returns "" for legacy_unknown (or
// any other value), meaning "leave this column unset" (review R4).
func hubCredentialKindForInitiator(kind string) string {
	switch kind {
	case store.InitiatorCredentialKindUAT:
		return string(CredentialKindUAT)
	case store.InitiatorCredentialKindAgent:
		return string(CredentialKindAgentJWT)
	case store.InitiatorCredentialKindSession:
		return string(CredentialKindInteractive)
	default:
		return ""
	}
}

// maxInitiatorSnapshotBytes bounds the serialized initiator_credential_snapshot
// column. Its components (name, purpose, labels) are already individually
// bounded by E.1's schema (uatMaxNameBytes, uatMaxPurposeBytes,
// maxAuditLabelsBytes), so this is defence in depth, not the primary bound.
const maxInitiatorSnapshotBytes = 2048

// initiatorCredentialSnapshot is the JSON shape of
// InitiatorAttribution.InitiatorCredentialSnapshot.
type initiatorCredentialSnapshot struct {
	Name              string          `json:"name,omitempty"`
	BoundaryKind      string          `json:"boundaryKind,omitempty"`
	BoundaryProjectID string          `json:"boundaryProjectId,omitempty"`
	Purpose           string          `json:"purpose,omitempty"`
	Labels            json.RawMessage `json:"labels,omitempty"`
}

// initiatorCredentialSnapshotJSON renders a bounded, sanitized snapshot of
// the credential decoration for persistence in InitiatorCredentialSnapshot.
// It reuses E.2a/E.1's sanitization and bounding primitives (sanitizeForLog,
// boundedLabelsJSON) rather than a second scheme, so a legacy row and a live
// audit row degrade identically. Returns "" when the credential carries no
// decoration (non-UAT credentials, or no ambient credential at all).
func initiatorCredentialSnapshotJSON(cred CredentialContext) string {
	if cred.Decoration == nil {
		return ""
	}
	d := cred.Decoration
	snap := initiatorCredentialSnapshot{
		Name:              sanitizeForLog(d.TokenName, uatMaxNameBytes),
		BoundaryKind:      d.Boundary.Kind,
		BoundaryProjectID: d.Boundary.ProjectID,
		Purpose:           sanitizeForLog(d.Purpose, uatMaxPurposeBytes),
	}
	if len(d.Labels) > 0 {
		snap.Labels = json.RawMessage(boundedLabelsJSON(d.Labels))
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return ""
	}
	if len(b) > maxInitiatorSnapshotBytes {
		// Defence in depth: every component above is already individually
		// bounded, so this should not happen in practice. Never persist an
		// unbounded blob regardless.
		snap.Labels = nil
		if b2, err2 := json.Marshal(snap); err2 == nil {
			b = b2
		}
	}
	return string(b)
}

// captureInitiatorAttribution builds an InitiatorAttribution from the live
// request's identity/credential context. It never reads or writes
// AuthorizationRevision — callers (newInitiatorAttribution /
// reattributeInitiator) set that separately, since it depends on the prior
// value on the row being written.
//
// A context with no ambient identity (should not happen on an authenticated
// path) yields a zero InitiatorAttribution, which reads back as
// legacy_unknown — never as an interactive credential.
func captureInitiatorAttribution(ctx context.Context) store.InitiatorAttribution {
	var attr store.InitiatorAttribution

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return attr
	}
	attr.InitiatorPrincipalKind = identity.Type()
	attr.InitiatorPrincipalID = identity.ID()

	cred := GetCredentialContextFromContext(ctx)
	attr.InitiatorCredentialKind = initiatorCredentialKindFor(cred.Kind)
	attr.InitiatorCredentialID = cred.ID
	attr.InitiatorCredentialSnapshot = initiatorCredentialSnapshotJSON(cred)
	attr.AttributionVersion = initiatorAttributionVersion

	return attr
}

// newInitiatorAttribution captures attribution the first time a row is
// authored (one-shot scheduled event create, schedule create). Its
// AuthorizationRevision starts at 1.
func newInitiatorAttribution(ctx context.Context) store.InitiatorAttribution {
	attr := captureInitiatorAttribution(ctx)
	attr.AuthorizationRevision = 1
	return attr
}

// reattributeInitiator replaces attribution on a schedule whose future
// dispatch was just re-authorized (ruling Q2: a fully reauthorized mutation
// that changes payload/target/type/timing, or resume/enable, replaces the
// attribution and bumps the revision atomically in the same write; old and
// new authority are never unioned). prev is the row's current
// InitiatorAttribution (0 for a never-attributed or legacy schedule), so the
// first re-attribution of a legacy schedule starts its revision at 1, same
// as a fresh create.
func reattributeInitiator(ctx context.Context, prev store.InitiatorAttribution) store.InitiatorAttribution {
	attr := captureInitiatorAttribution(ctx)
	attr.AuthorizationRevision = prev.AuthorizationRevision + 1
	return attr
}

// setBrokerDispatchInitiator populates a store.BrokerDispatch row's initiator
// and correlation fields from the live request context that originated a
// cross-node op (path F: deferredDataOpResult / deferredLifecycle). Set once
// at insert; never updated afterward. Deliberately does not set a snapshot
// or version: a broker dispatch is a transport retry of an already-
// authorized operation (ruling Q4), not a re-evaluated authoring point.
func setBrokerDispatchInitiator(ctx context.Context, d *store.BrokerDispatch) {
	attr := captureInitiatorAttribution(ctx)
	d.InitiatorPrincipalKind = attr.InitiatorPrincipalKind
	d.InitiatorPrincipalID = attr.InitiatorPrincipalID
	d.InitiatorCredentialKind = attr.InitiatorCredentialKind
	d.InitiatorCredentialID = attr.InitiatorCredentialID
	d.CorrelationID = logging.RequestIDFromContext(ctx)
}

// ScheduledInitiator is the read-time view of a scheduled row's
// InitiatorAttribution (plan §3.5's scheduledInitiator read helper). A row
// written before E.2b (AttributionVersion NULL/0), or one whose credential
// kind is legacy_unknown for any other reason (review R5: no recordable
// provenance at capture time), always reads as LegacyUnknown with every
// other field empty — never as an interactive credential. E.2b's own tests
// assert this record; B.3's tests assert the authority decision built on it
// (fire-time recheck, ceiling, legacy_unknown handling) — this type carries
// no authority of its own.
type ScheduledInitiator struct {
	PrincipalKind      string
	PrincipalID        string
	CredentialKind     string // session | uat | agent | legacy_unknown -- never "interactive"
	CredentialID       string
	CredentialSnapshot string // bounded JSON; "" when absent
	LegacyUnknown      bool
}

// scheduledInitiator normalizes a stored InitiatorAttribution (read directly
// off a store.Schedule or store.ScheduledEvent row, both embedding the same
// mixin) for B.3's fire-time authority decision and for E.2b's own
// audit/log attribution. Typed and total (review O3): every
// InitiatorAttribution value has a defined normalization, so there is
// nothing left to error on.
//
// A row is legacy_unknown either because it predates E.2b
// (AttributionVersion 0) or because its credential kind was itself recorded
// as legacy_unknown at capture time (review R5) — both cases clear every
// other field rather than surfacing partial/stale data (design check (c)).
func (s *Server) scheduledInitiator(attr store.InitiatorAttribution) ScheduledInitiator {
	if attr.AttributionVersion == 0 || attr.InitiatorCredentialKind == "" ||
		attr.InitiatorCredentialKind == store.InitiatorCredentialKindLegacyUnknown {
		return ScheduledInitiator{
			CredentialKind: store.InitiatorCredentialKindLegacyUnknown,
			LegacyUnknown:  true,
		}
	}

	return ScheduledInitiator{
		PrincipalKind:      attr.InitiatorPrincipalKind,
		PrincipalID:        attr.InitiatorPrincipalID,
		CredentialKind:     attr.InitiatorCredentialKind,
		CredentialID:       attr.InitiatorCredentialID,
		CredentialSnapshot: attr.InitiatorCredentialSnapshot,
	}
}
