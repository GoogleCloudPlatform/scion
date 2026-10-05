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
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
)

// ---------------------------------------------------------------------------
// E.1: credential decoration.
//
// CredentialDecoration is descriptive, server-derived metadata about the
// credential that authenticated a request. It never grants, narrows, or
// identifies a principal, and authorization code must not branch on it: the
// authenticated principal is always the human user, and decoration is purely
// for logs and audit attribution (canonical design, "Principal decoration
// and bearer automation"). TestCredentialDecorationNotReadByAuthzCode
// mechanically enforces the "never branch on it" half of that rule.
// ---------------------------------------------------------------------------

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

// decorationBoundaryFromToken is the single place E reads A.1's authoritative
// TokenBoundary type (pkg/hub/authz_boundary.go, ptone/scion#2117). It is the
// only adaptation point named in plan §2.8/rulings Q8: if A's boundary shape
// ever changes, only this function and its unit test
// (TestDecorationBoundaryFromToken) need to change.
//
// D.1 has not yet persisted a boundary column on the UAT row, so the caller
// (ValidateToken) builds the TokenBoundary input inline from the token's
// stored project ID (every UAT is project-scoped today). When D.1 lands,
// that call site changes to pass the persisted boundary through; this
// function's signature does not change again.
func decorationBoundaryFromToken(b TokenBoundary) decorationBoundary {
	if !b.Valid() {
		// Descriptive only: E does not enforce this, it only avoids
		// asserting a boundary kind/project-ID combination A.1 itself
		// would reject.
		return decorationBoundary{Kind: "invalid"}
	}
	return decorationBoundary{Kind: string(b.Kind), ProjectID: b.ProjectID}
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

// CredentialDecorationFromContext returns the descriptive credential
// decoration recorded on the request's CredentialContext, if any. It always
// returns a deep copy (see clone) so callers cannot mutate shared state.
func CredentialDecorationFromContext(ctx context.Context) (CredentialDecoration, bool) {
	cc := GetCredentialContextFromContext(ctx)
	if cc.Decoration == nil {
		return CredentialDecoration{}, false
	}
	return cc.Decoration.clone(), true
}

// ---------------------------------------------------------------------------
// Bounded metadata schema and validation (issuance-time only; immutable
// after issuance per the E.1 ruling — there is no update endpoint).
// ---------------------------------------------------------------------------

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

// gVerifiedActorFieldNames are G's verified-agent-actor structured audit
// field names (ruling N3), reserved as exact label keys so an issuer-supplied
// label can never occupy where a future verified actor field will live. This
// is the single canonical list: a same-package consistency test pins it
// against G's actual audit-record field names, so the two never drift apart.
// No wildcard on "actor_"/"source_" — only these exact names are reserved.
// If G renames a column before merging, this list and the pinning test are
// updated in the same change.
var gVerifiedActorFieldNames = credentialmeta.ReservedActorLabelKeys()

// ErrInvalidUATMetadata reports a credential-metadata field that failed
// bounded validation. The message names the field and the violated rule but
// never echoes the offending value, per the E.1 rule that untrusted
// issuer-supplied text must never be reflected back into logs or errors
// unsanitized.
type ErrInvalidUATMetadata struct {
	Field string
	Rule  string
}

func (e *ErrInvalidUATMetadata) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Rule)
}

// ValidateCredentialMetadata validates a token's name, purpose, and labels
// against the bounded schema documented in the E.1 design notes. It is
// called once, at issuance; metadata is immutable afterward (E.1 ruling Q1),
// so the same validator can be reused unchanged if an update path is ever
// added. Existing (pre-E.1) rows are never re-validated: they render through
// the sanitizing LogValue path instead.
func ValidateCredentialMetadata(name, purpose string, labels map[string]string) error {
	err := credentialmeta.ValidateIssuance(name, purpose, labels)
	if err == nil {
		return nil
	}
	var validationErr *credentialmeta.ValidationError
	if errors.As(err, &validationErr) {
		return &ErrInvalidUATMetadata{Field: validationErr.Field, Rule: validationErr.Rule}
	}
	return err
}

// appendCredentialMetadataAuditFields adds E.1's audit-safe metadata
// indicators to an existing JSON mutation-audit summary: whether a purpose
// was set, and which label keys were used. It never adds purpose or label
// values — those are issuer-supplied, unbounded-trust text and do not belong
// in the immutable audit trail (plan §2.4).
func appendCredentialMetadataAuditFields(summaryJSON string, hasPurpose bool, labels map[string]string) string {
	var fields map[string]interface{}
	if err := json.Unmarshal([]byte(summaryJSON), &fields); err != nil {
		// summaryJSON is always built internally as well-formed JSON; fail
		// safe by keeping the original summary rather than losing the audit
		// record over a metadata annotation.
		return summaryJSON
	}
	fields["has_purpose"] = hasPurpose
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields["label_keys"] = keys
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return summaryJSON
	}
	return string(out)
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
