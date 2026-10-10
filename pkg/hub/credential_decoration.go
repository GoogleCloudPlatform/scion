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
	"sort"

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

// decorationBoundaryFromToken is the single place E reads A.1's authoritative
// TokenBoundary type (pkg/hub/identity_credential.go, ptone/scion#2117). It is the
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
