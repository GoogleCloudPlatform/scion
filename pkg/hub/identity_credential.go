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
	"log/slog"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// This file holds the credential vocabulary the principal types in
// identity.go carry: the credential kind, the token boundary, the request
// credential context, and the descriptive credential decoration with its
// log rendering. Like identity.go it references only itself, identity.go,
// the standard library and packages outside pkg/hub, so the two files can
// move together into their own package. The authorization and issuance
// logic that consumes these types stays in authz.go, authz_boundary.go and
// credential_decoration.go.

// CredentialKind describes the authentication material that established a principal.
// Credential constraints are caveats: they may narrow authority but never grant it.
type CredentialKind = credentialmeta.Kind

const (
	CredentialKindInteractive = credentialmeta.KindInteractive
	CredentialKindUAT         = credentialmeta.KindUAT
	CredentialKindAgentJWT    = credentialmeta.KindAgentJWT
	CredentialKindFederation  = credentialmeta.KindFederation
	CredentialKindBroker      = credentialmeta.KindBroker
	CredentialKindDev         = credentialmeta.KindDev
	// CredentialKindDelegatedAgent is an agent delegated credential, the
	// opaque bearer produced by exchanging an agent delegation grant. Only
	// *DelegatedAgentIdentity classifies to it.
	CredentialKindDelegatedAgent = credentialmeta.KindDelegatedAgent

	// CredentialKindHubDelivery is the internal credential a hub-side
	// material delivery caller presents (ptone/scion#2228 part 2). It is
	// produced only by the unexported newHubDeliveryIdentity constructor
	// (authz_delivery_credential.go); no request context, token or header
	// can carry it.
	CredentialKindHubDelivery CredentialKind = "hub_delivery"
)

// BoundaryKind identifies the credential-side boundary a UAT is issued
// under. Canonical definition lives in pkg/hub/permissions (so permission
// metadata can reference it without an import cycle); this is an alias for
// ergonomic use within pkg/hub.
type BoundaryKind = permissions.BoundaryKind

const (
	BoundaryKindProject = permissions.BoundaryKindProject
	BoundaryKindHub     = permissions.BoundaryKindHub
)

// TokenBoundary is the credential-side boundary of a UAT: confined to one
// project, or spanning the hub. A.2/D.1 own persisting this (store/ent
// schema and mint/issuance wiring); A.1 defines the type, validity, and
// matching semantics only.
//
// The hub boundary means the credential is not restricted to one project —
// it does not by itself mean the holder has access to every project. Every
// request still requires the holder's current, live authority on the
// resolved target (see ProjectTargetAdmission) plus the credential's exact
// permission set.
type TokenBoundary struct {
	Kind      BoundaryKind
	ProjectID string // set iff Kind == BoundaryKindProject
}

// Valid rejects malformed boundary combinations. Calls permissions.ValidBoundary
// so the same rule is reachable from pkg/store: pkg/store cannot import
// pkg/hub, but store-layer validation needs this exact rule too — a shared
// table test pins agreement.
func (b TokenBoundary) Valid() bool {
	return permissions.ValidBoundary(b.Kind, b.ProjectID)
}

// CredentialContext records the credential used for an authorization request.
// ProjectID and Scopes are caveats for scoped bearer credentials. Ceiling is
// the UAT's normalized permission ceiling — the single source every
// credential-scope restriction evaluates through, for every ceiling
// version.
type CredentialContext struct {
	Kind      CredentialKind
	ID        string
	Type      string
	ProjectID string
	// Boundary carries the credential-side boundary (project or hub) for a
	// UAT or agent delegated credential; nil for every other credential
	// kind. ProjectID is
	// filled from the same boundary for a project-scoped UAT, so a caller
	// that reads only ProjectID sees a value consistent with Boundary. A
	// boundary-aware caller should read Boundary directly, never infer
	// hub-vs-project from an empty ProjectID, which is never a positive
	// claim of hub scope by itself.
	Boundary *TokenBoundary
	Scopes   []string
	Ceiling  permissions.FrozenPermissionCeiling

	// Descriptive credential metadata. Decoration is additive,
	// server-derived attribution (token name/boundary/purpose/labels) for
	// logs and audit. It is never read by authorization decisions — see
	// TestCredentialDecorationNotReadByAuthzCode.
	Decoration *CredentialDecoration
}

// decorationBoundary is E's own descriptive copy of a credential's boundary,
// rendered for logs and (later) audit. It is derived from A.1's authoritative
// boundary type through the single adapter decorationBoundaryFromToken below
// and is never itself consulted for enforcement — enforcement is A.1/D.1's
// job, using their own type.
//
// Unexported deliberately (pat-refactor ruling on Q8, 2026-09-28): it has the
// same shape ({Kind; ProjectID}) as A.1's TokenBoundary. Keeping it
// unexported means it is only ever a rendering detail of
// CredentialDecoration.Boundary and LogValue, with no external API surface —
// so there is no duplicate *public* boundary model after A.1 integration,
// satisfying "no duplicate public boundary model may remain after
// integration."
type decorationBoundary struct {
	Kind      string // "project" | "hub" | "invalid" (mapped from A's kind)
	ProjectID string // set iff Kind == "project"
}

// CredentialDecoration is descriptive, server-derived metadata about the
// credential that authenticated a request. It is populated only from the
// server-validated token record (never from a header, query parameter, or
// body field) and is attached to the existing CredentialContext, separate
// from the authenticated principal.
//
// Deliberately absent: plaintext, hash, prefix, scopes/permissions (the
// ceiling is A.2's concern; audit may reference its version separately as a
// plain integer), and any actor/agent field (reserved for a future
// verified-agent extension).
type CredentialDecoration struct {
	// Kind is the credential kind this decoration describes. E.1 populates
	// it only for CredentialKindUAT.
	Kind CredentialKind
	// TokenID is the persisted, immutable UUID of the access token
	// (store.UserAccessToken.ID).
	TokenID string
	// TokenName is the issuer-supplied label, validated at issuance for new
	// tokens and sanitized at render time for all tokens (including legacy
	// rows created before this field was bounded).
	TokenName string
	// Boundary is E's own descriptive render of the credential's boundary.
	Boundary decorationBoundary
	// Purpose is optional, issuer-supplied, bounded descriptive text.
	Purpose string
	// Labels is optional, issuer-supplied, bounded descriptive metadata.
	// Always rendered as untrusted, issuer-supplied text — never treated as
	// a verified actor, ancestry, or authorization signal.
	Labels map[string]string
}

// IsZero reports whether d carries no credential attribution at all (for
// example, a non-UAT credential or a request context with no decoration).
func (d CredentialDecoration) IsZero() bool {
	return d.Kind == "" && d.TokenID == ""
}

// LogValue implements slog.LogValuer so callers can log decoration with
// slog.Any("credential", d) and get a stable, sanitized group of attributes.
// Labels are always nested under "labels" (never promoted to the top level),
// and a constant "labels_source" marker signals that they are issuer-supplied
// and unverified. See the canonical rendering table in the E.1 design notes.
func (d CredentialDecoration) LogValue() slog.Value {
	if d.IsZero() {
		return slog.GroupValue()
	}
	attrs := []slog.Attr{
		slog.String("kind", string(d.Kind)),
		slog.String("id", d.TokenID),
		slog.String("name", sanitizeForLog(d.TokenName, uatMaxNameBytes)),
		slog.String("boundary.kind", d.Boundary.Kind),
	}
	if d.Boundary.ProjectID != "" {
		attrs = append(attrs, slog.String("boundary.project_id", d.Boundary.ProjectID))
	}
	if d.Purpose != "" {
		attrs = append(attrs, slog.String("purpose", sanitizeForLog(d.Purpose, uatMaxPurposeBytes)))
	}
	if len(d.Labels) > 0 {
		keys := make([]string, 0, len(d.Labels))
		for k := range d.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		labelAttrs := make([]any, 0, len(keys))
		for _, k := range keys {
			// Sanitize the key too, not just the value (review finding F11):
			// a row written directly to the store does not go through the
			// validator, so its keys cannot be trusted to already satisfy
			// the bounded shape either.
			renderKey := k
			if !isValidLabelKeyShape(k) {
				renderKey = sanitizeForLog(k, uatMaxLabelKeyBytes)
			}
			labelAttrs = append(labelAttrs, slog.String(renderKey, sanitizeForLog(d.Labels[k], uatMaxLabelValueBytes)))
		}
		attrs = append(attrs, slog.Group("labels", labelAttrs...))
		attrs = append(attrs, slog.String("labels_source", "issuer"))
	}
	return slog.GroupValue(attrs...)
}

// clone returns a deep copy of d: a fresh Labels map, so no caller can
// mutate another caller's (or the identity's own) stored decoration through
// the returned value. Every accessor that hands a CredentialDecoration to
// calling code (CredentialDecorationFromContext, ScopedUserIdentity.Decoration)
// returns clone()'s result, never the internally-held value directly.
func (d CredentialDecoration) clone() CredentialDecoration {
	c := d
	if d.Labels != nil {
		c.Labels = make(map[string]string, len(d.Labels))
		for k, v := range d.Labels {
			c.Labels[k] = v
		}
	}
	return c
}

const (
	uatMaxNameBytes       = credentialmeta.MaxNameBytes
	uatMaxPurposeBytes    = credentialmeta.MaxPurposeBytes
	uatMaxLabelCount      = credentialmeta.MaxLabelCount
	uatMaxLabelKeyBytes   = credentialmeta.MaxLabelKeyBytes
	uatMaxLabelValueBytes = credentialmeta.MaxLabelValueBytes
	// There is no separate serialized-labels size cap: uatMaxLabelCount *
	// (uatMaxLabelKeyBytes + uatMaxLabelValueBytes) is already well under
	// 1KiB (≤8 * (32+64) = 768 bytes of raw content, plus JSON punctuation),
	// so a dedicated check here could never fire and would be untested dead
	// code (review finding F10). If any per-field cap above is ever
	// loosened, reconsider whether a total-size cap is needed again.
)

// isValidLabelKeyShape reports whether s matches the bounded label-key rule
// ^[a-z][a-z0-9_.-]{0,31}$ without pulling in a regexp for something this
// simple.
func isValidLabelKeyShape(s string) bool {
	return credentialmeta.ValidLabelKey(s)
}

// isDisplayUnsafeRune reports whether r can alter how surrounding text is
// displayed rather than being displayed itself: control characters (Cc),
// format characters (Cf: bidi overrides and isolates, zero-width
// characters, BOM, ...) and line/paragraph separators (Zl, Zp). Shared by
// sanitizeForLog and sanitizeFailureReason so the two stay on one set.
func isDisplayUnsafeRune(r rune) bool {
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

// sanitizeForLog is the render-time defence for legacy rows (created before
// validation existed) and general defence in depth: it never trusts stored
// text to already satisfy the bounded schema. Cc/Cf/Zl/Zp runes are replaced
// with U+FFFD, then the result is truncated to maxBytes with a "…" marker.
func sanitizeForLog(s string, maxBytes int) string {
	var b strings.Builder
	for _, r := range s {
		if isDisplayUnsafeRune(r) {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) <= maxBytes {
		return out
	}
	// Truncate at a rune boundary at or before maxBytes. out is valid UTF-8
	// (rebuilt rune-by-rune above), so the only way cutting at maxBytes can
	// be wrong is landing inside the final rune's byte sequence. Checking
	// utf8.RuneStart on the byte immediately after the cut tells us that
	// directly: if it starts a new rune, the cut is already clean; if it is
	// a continuation byte, walk back to where that rune began and drop it
	// whole. This is O(1) (at most 3 steps back) instead of re-validating
	// the whole prefix with utf8.ValidString on every trim.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + "…"
}
