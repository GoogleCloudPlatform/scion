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
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
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
// Unexported deliberately (pat-refactor ruling on Q8, 2026-09-28): once the
// stand-in below is swapped for A.1's TokenBoundary, this type has the same
// shape ({Kind; ProjectID}) as that type. Keeping it unexported means it is
// only ever a rendering detail of CredentialDecoration.Boundary and LogValue,
// with no external API surface — so there is no duplicate *public* boundary
// model after A.1 integration, satisfying "no duplicate public boundary
// model may remain after integration."
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
	uatMaxNameBytes       = 128
	uatMaxPurposeBytes    = 128
	uatMaxLabelCount      = 8
	uatMaxLabelKeyBytes   = 32
	uatMaxLabelValueBytes = 64
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
	if len(s) == 0 || len(s) > uatMaxLabelKeyBytes {
		return false
	}
	for i, r := range s {
		switch {
		case i == 0 && (r < 'a' || r > 'z'):
			return false
		case i > 0 && !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-'):
			return false
		}
	}
	return true
}

// labelValueCharsetOK reports whether every rune in s is in the allowed
// label-value charset: [A-Za-z0-9 _.:/@+=,-].
func labelValueCharsetOK(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case strings.ContainsRune(" _.:/@+=,-", r):
		default:
			return false
		}
	}
	return true
}

// reservedLabelKeys are exact (case-insensitive) matches that would let an
// issuer-supplied label impersonate an attribution, ancestry, or
// authorization field. Rejected both as an exact key and as a dotted prefix
// (e.g. "user" rejects "user.name" too).
var reservedLabelKeys = map[string]bool{
	"agent": true, "agent_id": true, "actor": true, "principal": true,
	"principal_id": true, "principal_kind": true, "user": true, "user_id": true,
	"email": true, "on_behalf_of": true, "delegate": true, "delegator": true,
	"delegation": true, "ancestry": true, "creator": true, "created_by": true,
	"owner": true, "project_id": true, "broker_id": true, "credential": true,
	"credential_id": true, "token": true, "token_id": true, "role": true,
	"scope": true, "scopes": true, "permission": true, "permissions": true,
	"verified": true, "system": true, "executor": true, "initiator": true,
	// actor_binding is reserved for G's later verified agent binding.
	// "actor"'s dotted-prefix rule does not catch it (no "." between the
	// words), so it is listed explicitly.
	"actor_binding": true,
}

// reservedLabelKeyPrefixes are case-insensitive prefixes rejected outright,
// in addition to the dotted-prefix rule for reservedLabelKeys.
var reservedLabelKeyPrefixes = []string{"scion.", "hub.", "x-"}

// isReservedLabelKey reports whether key collides with a reserved
// attribution/authorization field name, exactly or as a dotted prefix.
// "-" is normalized to "_" before the exact/dotted-prefix comparisons
// (review finding F7): the label key shape allows both separators, so
// "actor-binding", "agent-id" and "on-behalf-of" must be caught the same as
// their "_" forms, or reserving the "_" form alone would be trivially
// sidestepped. The scion./hub./x- prefix check runs against the
// un-normalized lowercase key, since those prefixes are themselves
// dot/hyphen-based and normalizing first would change what they match.
func isReservedLabelKey(key string) bool {
	lower := strings.ToLower(key)
	for _, prefix := range reservedLabelKeyPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	normalized := strings.ReplaceAll(lower, "-", "_")
	if reservedLabelKeys[normalized] {
		return true
	}
	for reserved := range reservedLabelKeys {
		if strings.HasPrefix(normalized, reserved+".") {
			return true
		}
	}
	return false
}

// looksSecretBearing reports whether s appears to contain a pasted token or
// bearer header value. This is cheap defence-in-depth, not a secret scanner.
// The comparison is case-insensitive (review finding F8): "SCION_PAT_..." and
// "bearer abc" are just as much a pasted secret as the canonical casing.
func looksSecretBearing(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "scion_pat_") || strings.Contains(lower, "bearer ")
}

// hasControlOrFormatRune reports whether s contains a Unicode Cc (control),
// Cf (format, including bidi and zero-width), Zl (line separator, U+2028) or
// Zp (paragraph separator, U+2029) rune. Zl/Zp are included so "single line"
// actually means single line: they are not Cc/Cf but render as line breaks
// in most consumers (review finding F6).
func hasControlOrFormatRune(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return true
		}
	}
	return false
}

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
	if !utf8.ValidString(name) {
		return &ErrInvalidUATMetadata{Field: "name", Rule: "must be valid UTF-8"}
	}
	if len(name) > uatMaxNameBytes {
		return &ErrInvalidUATMetadata{Field: "name", Rule: fmt.Sprintf("must be at most %d bytes", uatMaxNameBytes)}
	}
	if hasControlOrFormatRune(name) {
		return &ErrInvalidUATMetadata{Field: "name", Rule: "must not contain control or formatting characters"}
	}
	if looksSecretBearing(name) {
		return &ErrInvalidUATMetadata{Field: "name", Rule: "must not resemble a bearer token or credential value"}
	}

	trimmedPurpose := strings.TrimSpace(purpose)
	if trimmedPurpose != "" {
		if !utf8.ValidString(trimmedPurpose) {
			return &ErrInvalidUATMetadata{Field: "purpose", Rule: "must be valid UTF-8"}
		}
		if len(trimmedPurpose) > uatMaxPurposeBytes {
			return &ErrInvalidUATMetadata{Field: "purpose", Rule: fmt.Sprintf("must be at most %d bytes", uatMaxPurposeBytes)}
		}
		if hasControlOrFormatRune(trimmedPurpose) {
			return &ErrInvalidUATMetadata{Field: "purpose", Rule: "must not contain control or formatting characters, and must be a single line"}
		}
		if looksSecretBearing(trimmedPurpose) {
			return &ErrInvalidUATMetadata{Field: "purpose", Rule: "must not resemble a bearer token or credential value"}
		}
	}

	if len(labels) > uatMaxLabelCount {
		return &ErrInvalidUATMetadata{Field: "labels", Rule: fmt.Sprintf("at most %d labels are allowed", uatMaxLabelCount)}
	}
	for key, value := range labels {
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return &ErrInvalidUATMetadata{Field: "labels", Rule: "label key and value must be valid UTF-8"}
		}
		if !isValidLabelKeyShape(key) {
			return &ErrInvalidUATMetadata{Field: "labels", Rule: fmt.Sprintf("label key must match ^[a-z][a-z0-9_.-]{0,%d}$", uatMaxLabelKeyBytes-1)}
		}
		if isReservedLabelKey(key) {
			return &ErrInvalidUATMetadata{Field: "labels", Rule: "label key is reserved"}
		}
		if len(value) > uatMaxLabelValueBytes {
			return &ErrInvalidUATMetadata{Field: "labels", Rule: fmt.Sprintf("label value must be at most %d bytes", uatMaxLabelValueBytes)}
		}
		if value != strings.TrimSpace(value) {
			return &ErrInvalidUATMetadata{Field: "labels", Rule: "label value must not have leading or trailing whitespace"}
		}
		if !labelValueCharsetOK(value) {
			return &ErrInvalidUATMetadata{Field: "labels", Rule: "label value contains a disallowed character"}
		}
		if looksSecretBearing(key) || looksSecretBearing(value) {
			return &ErrInvalidUATMetadata{Field: "labels", Rule: "must not resemble a bearer token or credential value"}
		}
	}
	return nil
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

// sanitizeForLog is the render-time defence for legacy rows (created before
// validation existed) and general defence in depth: it never trusts stored
// text to already satisfy the bounded schema. Cc/Cf/Zl/Zp runes are replaced
// with U+FFFD, then the result is truncated to maxBytes with a "…" marker.
func sanitizeForLog(s string, maxBytes int) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
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
	// (rebuilt rune-by-rune above), so cutting mid-rune can only ever
	// produce a split final rune — checking utf8.ValidString directly
	// (review-2 nit 3) drops exactly that one incomplete rune, unlike the
	// previous byte-scan, which over-trimmed a complete multi-byte rune
	// whenever it happened to end exactly at maxBytes.
	truncated := out[:maxBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "…"
}
