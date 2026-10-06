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
	"reflect"

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

// initiatorCredentialKindFor maps a live request's identity and ambient
// CredentialKind to InitiatorAttribution's smaller, committed domain
// (session|uat|agent|dev_local|legacy_unknown). Attribution never stores
// hub.CredentialKind's full domain: a caller of scheduledInitiator only ever
// sees one of these five values.
//
// Only a genuine interactive session maps to "session". Everything else —
// no ambient credential at all, federation, or broker — maps to
// legacy_unknown, never to session: plan correction (c) and ruling Q2 ("a
// mutation without recordable provenance never falls back to the creator's
// interactive authority") both forbid treating unknown or absent provenance
// as an interactive-style credential.
//
// dev_local (ptone/scion#2342) is the one narrow exception, and it is
// checked first: it requires BOTH isTrustedLocalDevUser(identity) (the
// concrete trusted *DevUser, by exact type assertion, with
// ID()==DevUserID) AND kind==CredentialKindDev. Requiring the ambient
// CredentialKind in addition to the identity assertion is defence in depth,
// not an independent gate: DevAuthMiddleware/UnifiedAuthMiddleware's dev-arm
// always sets both together (contextWithIdentity(devUser) paired with
// contextWithCredentialContext(credentialContextForIdentity(devUser)), see
// auth.go), so every genuine dev-local request satisfies both, and nothing
// here loosens the identity check — a look-alike identity whose Type() is
// "dev" also gets CredentialKindDev from credentialContextForIdentity, but
// still fails isTrustedLocalDevUser and falls through to legacy_unknown
// below. Never derived from identity.Type() == "dev", request fields,
// headers, or stored descriptive labels.
func initiatorCredentialKindFor(identity Identity, kind CredentialKind) string {
	if kind == CredentialKindDev && isTrustedLocalDevUser(identity) {
		return store.InitiatorCredentialKindDevLocal
	}
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
// session|uat|agent|dev_local|legacy_unknown domain back to
// hub.CredentialKind's vocabulary (interactive|uat|agent_jwt|dev), for the
// one place — the scheduled-dispatch success audit — that records a
// credential kind in a column every other mutation-audit writer fills with
// hub.CredentialKind (audit_actor.go:auditActorFromContext). Returns "" for
// legacy_unknown (or any other value), meaning "leave this column unset".
//
// dev_local maps to CredentialKindDev ("dev"), not CredentialKindInteractive
// ("interactive"): audit and log attribution for a trusted local-dev
// initiator must stay visibly distinct from an ordinary browser/API session.
func hubCredentialKindForInitiator(kind string) string {
	switch kind {
	case store.InitiatorCredentialKindUAT:
		return string(CredentialKindUAT)
	case store.InitiatorCredentialKindAgent:
		return string(CredentialKindAgentJWT)
	case store.InitiatorCredentialKindSession:
		return string(CredentialKindInteractive)
	case store.InitiatorCredentialKindDevLocal:
		return string(CredentialKindDev)
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
// path) yields a bare InitiatorAttribution with only InitiatorCredentialKind
// set to legacy_unknown — every other field stays empty, and
// AttributionVersion stays 0. Callers that key legacy detection on
// AttributionVersion (Schedule/ScheduledEvent, via scheduledInitiator) and
// callers that don't (BrokerDispatch has no version field at all) both see
// an explicit legacy_unknown, never an empty string that looks like "no
// value recorded" rather than "no recordable provenance."
//
// The nil check is isNilIdentity, not a bare identity == nil comparison: a
// non-nil Identity value holding a typed-nil concrete pointer (e.g. a nil
// *DevUser) would pass identity == nil, and the identity.Type()/identity.ID()
// calls below would then panic on most concrete Identity implementations
// (GCP#2188 review comment, ptone/scion#2342). GetIdentityFromContext has no
// production path that returns such a value today, but the guard costs
// nothing and removes the need to re-prove that invariant every time this
// function is read.
func captureInitiatorAttribution(ctx context.Context) store.InitiatorAttribution {
	var attr store.InitiatorAttribution

	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		attr.InitiatorCredentialKind = store.InitiatorCredentialKindLegacyUnknown
		return attr
	}
	attr.InitiatorPrincipalKind = identity.Type()
	attr.InitiatorPrincipalID = identity.ID()

	cred := GetCredentialContextFromContext(ctx)
	attr.InitiatorCredentialKind = initiatorCredentialKindFor(identity, cred.Kind)
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
// kind is legacy_unknown for any other reason (no recordable provenance at
// capture time), always reads as LegacyUnknown with every other field empty
// — never as an interactive credential. E.2b's own tests assert this
// record; B.3's tests assert the authority decision built on it (fire-time
// recheck, ceiling, legacy_unknown handling) — this type carries no
// authority of its own.
type ScheduledInitiator struct {
	PrincipalKind      string
	PrincipalID        string
	CredentialKind     string // session | uat | agent | dev_local | legacy_unknown -- never "interactive"
	CredentialID       string
	CredentialSnapshot string // bounded JSON; "" when absent
	LegacyUnknown      bool
}

// scheduledInitiator normalizes a stored InitiatorAttribution (read directly
// off a store.Schedule or store.ScheduledEvent row, both embedding the same
// mixin) for B.3's fire-time authority decision and for E.2b's own
// audit/log attribution. Typed and total: every InitiatorAttribution value
// has a defined normalization, so there is nothing left to error on.
//
// A row is legacy_unknown either because it predates E.2b
// (AttributionVersion 0) or because its credential kind was itself recorded
// as legacy_unknown at capture time — both cases clear every other field
// rather than surfacing partial/stale data (design check (c)).
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

// isNilIdentity reports whether identity is either the nil interface or a
// non-nil Identity value holding a nil concrete pointer (a "typed nil" — for
// example a nil *DevUser or nil *AuthenticatedUser boxed into the Identity
// interface). In Go, an interface value equals nil only when both its type
// and value are nil; a typed nil has a non-nil type descriptor, so
// identity == nil is false for it even though the concrete pointer it holds
// is nil. Every concrete Identity implementation in this package is a
// pointer type, and several of their ID()/Type() methods dereference the
// receiver with no nil guard (e.g. DevUser.ID() returns u.id directly), so
// calling one on a typed-nil identity panics (GCP#2188 review comment on
// initiatorMatchesExecutor, ptone/scion#2342).
//
// Deliberately reflect-based rather than a type switch enumerating every
// concrete Identity implementation, which is what the review comment
// suggested: a hand-written switch must be extended every time a new
// Identity implementation is added anywhere in the package, and silently
// stops catching that type's typed-nil case if a future author forgets. A
// pointer-kind nil check via reflection generalizes over any current or
// future implementation without that maintenance burden, at the cost of one
// reflect call in a defensive guard that is not on any hot path. This helper
// answers only the narrow nil-safety question — it says nothing about which
// concrete type identity is or what it means; principalContextForIdentity
// and credentialContextForIdentity's type switches (authz.go) remain the
// place concrete Identity types are classified by meaning, and are
// unaffected by this helper.
func isNilIdentity(identity Identity) bool {
	if identity == nil {
		return true
	}
	// reflect.ValueOf(identity) already unwraps the interface to its
	// concrete dynamic value, so v.Kind() can never itself be
	// reflect.Interface here — that case is omitted so this list names only
	// the kinds IsNil can actually observe through this call.
	v := reflect.ValueOf(identity)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return v.IsNil()
	default:
		return false
	}
}

// initiatorMatchesExecutor reports whether a non-legacy initiator is the
// same principal as exec — the identity that will actually execute/authorize
// this fire (e.g. server.go's scheduled-dispatch success audit, via
// scheduledCreatorIdentity). Callers use this to decide whether it is safe
// to copy the initiator's credential onto a record that names exec as the
// actor: after an update or resume by a different principal, the initiator
// is not exec, and naming principal A with principal B's credential would
// be wrong.
//
// The general rule is same-kind/same-ID: initiator.PrincipalKind ==
// exec.Type() && initiator.PrincipalID == exec.ID(). dev_local
// (ptone/scion#2342 review round 1, finding 1; the PrincipalKind clause
// below is review round 2, optional item O1) is the one narrow addition,
// checked ONLY in addition to the general rule, never instead of it, and
// every clause below must hold for this exact kind:
//
//	initiator.PrincipalKind == "dev" &&
//		initiator.CredentialKind == store.InitiatorCredentialKindDevLocal &&
//		initiator.PrincipalID == DevUserID &&
//		exec.ID() == DevUserID &&
//		exec.Type() == "user"
//
// This addition exists because a dev_local row's InitiatorPrincipalKind is
// hub.DevUser.Type() ("dev"), but exec — reconstructed by
// scheduledCreatorIdentity (server.go) from CreatedBy, and by B.3's
// fire-time resolution (b3-design §3.6.2) — is always a generic
// NewAuthenticatedUser with Type()=="user", never "dev". Without this
// addition the general rule could never match a genuine dev_local self-fire,
// and the scheduled-dispatch success audit would never show the dev kind
// for one (leaving it audited the same as a legacy or different-principal
// row, contrary to "keep audit and log attribution visibly distinct from
// ordinary browser/API session credentials"). The PrincipalKind=="dev"
// clause costs nothing on any genuine row (captureInitiatorAttribution only
// ever emits dev_local for a *DevUser, whose Type() is always "dev") and
// narrows the exception to precisely the case this comment describes. No
// other kind is loosened: every kind other than dev_local is decided by the
// general rule alone. TestInitiatorMatchesExecutor mutation-pins every
// clause of this arm (ptone/scion#2342 review round 2, R1/O1): replacing
// any one of them with an unconditional true is caught by a dedicated test
// row.
//
// A legacy_unknown initiator, or a nil or typed-nil exec (isNilIdentity;
// GCP#2188 review comment, ptone/scion#2342 — a typed-nil exec such as a nil
// *DevUser or nil *AuthenticatedUser would otherwise pass exec == nil and
// then panic on exec.ID()), never matches.
func initiatorMatchesExecutor(initiator ScheduledInitiator, exec Identity) bool {
	if initiator.LegacyUnknown || isNilIdentity(exec) {
		return false
	}
	if initiator.PrincipalKind == exec.Type() && initiator.PrincipalID == exec.ID() {
		return true
	}
	return initiator.PrincipalKind == "dev" &&
		initiator.CredentialKind == store.InitiatorCredentialKindDevLocal &&
		initiator.PrincipalID == DevUserID &&
		exec.ID() == DevUserID &&
		exec.Type() == "user"
}
