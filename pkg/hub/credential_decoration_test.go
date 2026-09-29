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
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Unit-level tests for the E.1 credential decoration type, adapter, and
// bounded metadata validator. AC-level (end-to-end) tests live in
// credential_decoration_ac_test.go.
// ---------------------------------------------------------------------------

// TestDecorationBoundaryFromToken is the one E test that imports A.1's
// TokenBoundary type (plan §2.8, rulings Q8): it pins the mapping for
// project, hub, and invalid boundaries at the single adapter function.
func TestDecorationBoundaryFromToken(t *testing.T) {
	t.Run("project", func(t *testing.T) {
		b := decorationBoundaryFromToken(TokenBoundary{Kind: BoundaryKindProject, ProjectID: "proj-1"})
		if b.Kind != "project" || b.ProjectID != "proj-1" {
			t.Fatalf("got %+v, want project boundary for proj-1", b)
		}
	})
	t.Run("hub", func(t *testing.T) {
		b := decorationBoundaryFromToken(TokenBoundary{Kind: BoundaryKindHub})
		if b.Kind != "hub" || b.ProjectID != "" {
			t.Fatalf("got %+v, want hub boundary with no project ID", b)
		}
	})
	t.Run("invalid: project boundary with no project ID", func(t *testing.T) {
		b := decorationBoundaryFromToken(TokenBoundary{Kind: BoundaryKindProject})
		if b.Kind != "invalid" {
			t.Fatalf("got %+v, want invalid boundary", b)
		}
	})
	t.Run("invalid: hub boundary carrying a project ID", func(t *testing.T) {
		b := decorationBoundaryFromToken(TokenBoundary{Kind: BoundaryKindHub, ProjectID: "proj-1"})
		if b.Kind != "invalid" {
			t.Fatalf("got %+v, want invalid boundary", b)
		}
	})
	t.Run("invalid: unrecognized kind", func(t *testing.T) {
		b := decorationBoundaryFromToken(TokenBoundary{Kind: BoundaryKind("bogus")})
		if b.Kind != "invalid" {
			t.Fatalf("got %+v, want invalid boundary", b)
		}
	})
}

func TestCredentialDecoration_IsZero(t *testing.T) {
	if !(CredentialDecoration{}).IsZero() {
		t.Fatal("zero-value CredentialDecoration must report IsZero() == true")
	}
	d := CredentialDecoration{Kind: CredentialKindUAT, TokenID: "t1"}
	if d.IsZero() {
		t.Fatal("a decoration with Kind/TokenID set must not report IsZero() == true")
	}
}

// TestCredentialDecoration_NoPlaintextOrHashFields reflects over the type to
// pin its exact field set (E.1 AC4: no token plaintext/hash in decoration).
// A future field addition must update this list deliberately.
func TestCredentialDecoration_NoPlaintextOrHashFields(t *testing.T) {
	typ := reflect.TypeOf(CredentialDecoration{})
	want := map[string]bool{
		"Kind": true, "TokenID": true, "TokenName": true,
		"Boundary": true, "Purpose": true, "Labels": true,
	}
	got := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got[typ.Field(i).Name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CredentialDecoration field set changed: got %v, want %v", got, want)
	}
	for name := range got {
		lower := strings.ToLower(name)
		if lower == "keyhash" || lower == "hash" || lower == "prefix" || lower == "plaintext" || name == "Token" {
			t.Fatalf("CredentialDecoration must never carry a %s field", name)
		}
	}
}

func TestCredentialDecorationFromContext_CopiesLabels(t *testing.T) {
	original := map[string]string{"env": "prod"}
	cc := CredentialContext{Kind: CredentialKindUAT, ID: "t1", Decoration: &CredentialDecoration{
		Kind: CredentialKindUAT, TokenID: "t1", TokenName: "n", Labels: original,
	}}
	ctx := contextWithCredentialContext(context.Background(), cc)

	got, ok := CredentialDecorationFromContext(ctx)
	if !ok {
		t.Fatal("expected a decoration to be present")
	}
	got.Labels["env"] = "mutated"
	if original["env"] != "prod" {
		t.Fatal("CredentialDecorationFromContext must return a defensive copy of Labels")
	}

	// A second read must not observe the first caller's mutation either.
	got2, ok := CredentialDecorationFromContext(ctx)
	if !ok || got2.Labels["env"] != "prod" {
		t.Fatalf("second read observed mutated state: %+v", got2)
	}

	if _, ok := CredentialDecorationFromContext(context.Background()); ok {
		t.Fatal("a context with no credential context must report no decoration")
	}
}

// TestCredentialDecoration_LogValue_SanitizesAndGroups pins the log-safe
// rendering shape: a group with sanitized name/purpose/labels, labels
// nested (never promoted), and a labels_source marker.
func TestCredentialDecoration_LogValue_SanitizesAndGroups(t *testing.T) {
	d := CredentialDecoration{
		Kind:      CredentialKindUAT,
		TokenID:   "tok-1",
		TokenName: "bad\x00name",
		Boundary:  decorationBoundary{Kind: "project", ProjectID: "proj-1"},
		Purpose:   "ci\x00automation",
		Labels:    map[string]string{"env": "prod"},
	}
	v := d.LogValue()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue must return a group, got %v", v.Kind())
	}
	attrs := attrMap(t, v.Group())

	if got := attrs["name"].String(); strings.Contains(got, "\x00") || !strings.Contains(got, "�") {
		t.Fatalf("name not sanitized: %q", got)
	}
	if got := attrs["purpose"].String(); strings.Contains(got, "\x00") || !strings.Contains(got, "�") {
		t.Fatalf("purpose not sanitized: %q", got)
	}
	if attrs["boundary.kind"].String() != "project" || attrs["boundary.project_id"].String() != "proj-1" {
		t.Fatalf("boundary not rendered correctly: %+v", attrs)
	}
	labelsGroup, ok := attrs["labels"]
	if !ok || labelsGroup.Kind() != slog.KindGroup {
		t.Fatalf("labels must be a nested group, got %+v", attrs)
	}
	labelAttrs := attrMap(t, labelsGroup.Group())
	if labelAttrs["env"].String() != "prod" {
		t.Fatalf("expected label env=prod, got %+v", labelAttrs)
	}
	if attrs["labels_source"].String() != "issuer" {
		t.Fatalf("expected labels_source=issuer marker, got %+v", attrs)
	}
	// Every top-level key must be a scalar or the single "labels" group —
	// labels must never be promoted to top-level attribute names.
	if _, ok := attrs["env"]; ok {
		t.Fatal("label value leaked to a top-level attribute; must be nested under labels")
	}
}

func TestCredentialDecoration_LogValue_TruncatesOversizeText(t *testing.T) {
	d := CredentialDecoration{
		Kind:      CredentialKindUAT,
		TokenID:   "tok-1",
		TokenName: strings.Repeat("a", 200),
		Boundary:  decorationBoundary{Kind: "project", ProjectID: "p"},
	}
	attrs := attrMap(t, d.LogValue().Group())
	name := attrs["name"].String()
	if !strings.HasSuffix(name, "…") {
		t.Fatalf("expected a truncation marker, got %q", name)
	}
	if len(name) > uatMaxNameBytes+len("…") {
		t.Fatalf("truncated name too long: %d bytes", len(name))
	}
}

func TestCredentialDecoration_LogValue_ZeroValueIsEmptyGroup(t *testing.T) {
	v := CredentialDecoration{}.LogValue()
	if len(v.Group()) != 0 {
		t.Fatalf("zero-value decoration must render as an empty group, got %+v", v.Group())
	}
}

func attrMap(t *testing.T, attrs []slog.Attr) map[string]slog.Value {
	t.Helper()
	out := make(map[string]slog.Value, len(attrs))
	for _, a := range attrs {
		out[a.Key] = a.Value
	}
	return out
}

// ---------------------------------------------------------------------------
// ValidateCredentialMetadata: bounded schema (plan §2.3).
// ---------------------------------------------------------------------------

func TestValidateCredentialMetadata_Accepts(t *testing.T) {
	cases := []struct {
		name    string
		purpose string
		labels  map[string]string
	}{
		{name: "plain name only"},
		{name: "with purpose", purpose: "nightly CI automation"},
		{name: "with labels", labels: map[string]string{"env": "prod", "team.owner": "platform"}},
		{name: "empty label value allowed", labels: map[string]string{"note": ""}},
		{name: "label value looks like an agent name but key is not reserved",
			labels: map[string]string{"automation_role": "nightly-cleanup-agent"}},
		{name: "max length name", purpose: strings.Repeat("p", uatMaxPurposeBytes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCredentialMetadata(tc.name, tc.purpose, tc.labels); err != nil {
				t.Fatalf("expected acceptance, got error: %v", err)
			}
		})
	}
}

func TestValidateCredentialMetadata_Rejects(t *testing.T) {
	longKey := strings.Repeat("k", uatMaxLabelKeyBytes+1)
	nineLabels := map[string]string{}
	for i := 0; i < uatMaxLabelCount+1; i++ {
		nineLabels[strings.Repeat("k", 1)+string(rune('a'+i))] = "placeholderval"
	}
	invalidUTF8 := string([]byte{0xff, 0xfe})

	const (
		ruleUTF8              = "must be valid UTF-8"
		ruleControlOrFormat   = "must not contain control or formatting characters"
		rulePurposeSingleLine = "must not contain control or formatting characters, and must be a single line"
		ruleSecret            = "must not resemble a bearer token or credential value"
		ruleNameLength        = "must be at most 128 bytes"
		rulePurposeLength     = "must be at most 128 bytes"
		ruleLabelCount        = "at most 8 labels are allowed"
		ruleLabelUTF8         = "label key and value must be valid UTF-8"
		ruleLabelKeyShape     = "label key must match ^[a-z][a-z0-9_.-]{0,31}$"
		ruleLabelKeyReserved  = "label key is reserved"
		ruleLabelValueLength  = "label value must be at most 64 bytes"
		ruleLabelValueSpacing = "label value must not have leading or trailing whitespace"
		ruleLabelValueCharset = "label value contains a disallowed character"
	)

	cases := []struct {
		name      string
		token     string
		purpose   string
		labels    map[string]string
		wantField string
		wantRule  string
	}{
		{name: "NUL byte in purpose", token: "n", purpose: "bad\x00purpose", wantField: "purpose", wantRule: rulePurposeSingleLine},
		{name: "newline in purpose (single line rule)", token: "n", purpose: "line1\nline2", wantField: "purpose", wantRule: rulePurposeSingleLine},
		{name: "bidi override in purpose", token: "n", purpose: "bad\u202epurpose", wantField: "purpose", wantRule: rulePurposeSingleLine},
		{name: "zero-width space in purpose", token: "n", purpose: "bad\u200bpurpose", wantField: "purpose", wantRule: rulePurposeSingleLine},
		{name: "line separator U+2028 in purpose", token: "n", purpose: "line1\u2028line2", wantField: "purpose", wantRule: rulePurposeSingleLine},
		{name: "paragraph separator U+2029 in purpose", token: "n", purpose: "para1\u2029para2", wantField: "purpose", wantRule: rulePurposeSingleLine},
		{name: "invalid UTF-8 in purpose", token: "n", purpose: "bad" + invalidUTF8, wantField: "purpose", wantRule: ruleUTF8},
		{name: "129-byte purpose", token: "n", purpose: strings.Repeat("p", uatMaxPurposeBytes+1), wantField: "purpose", wantRule: rulePurposeLength},
		{name: "invalid UTF-8 in name", token: "n" + invalidUTF8, wantField: "name", wantRule: ruleUTF8},
		{name: "invalid UTF-8 in label key", token: "n", labels: map[string]string{"k" + invalidUTF8: "placeholderval"}, wantField: "labels", wantRule: ruleLabelUTF8},
		{name: "invalid UTF-8 in label value", token: "n", labels: map[string]string{"k": "placeholderval" + invalidUTF8}, wantField: "labels", wantRule: ruleLabelUTF8},
		{name: "33-char label key", token: "n", labels: map[string]string{longKey: "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyShape},
		{name: "uppercase label key", token: "n", labels: map[string]string{"Env": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyShape},
		{name: "9 labels exceeds count cap", token: "n", labels: nineLabels, wantField: "labels", wantRule: ruleLabelCount},
		{name: "label value contains scion_pat_", token: "n", labels: map[string]string{"k": "has scion_pat_abc"}, wantField: "labels", wantRule: ruleSecret},
		{name: "label value contains Bearer prefix", token: "n", labels: map[string]string{"k": "Bearer x"}, wantField: "labels", wantRule: ruleSecret},
		{name: "label value contains lowercase bearer prefix", token: "n", labels: map[string]string{"k": "bearer x"}, wantField: "labels", wantRule: ruleSecret},
		{name: "label value contains uppercase SCION_PAT_", token: "n", labels: map[string]string{"k": "SCION_PAT_abc"}, wantField: "labels", wantRule: ruleSecret},
		{name: "reserved key exact match", token: "n", labels: map[string]string{"user_id": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "reserved dotted-prefix match", token: "n", labels: map[string]string{"agent.name": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "actor_binding reserved for G's verified agent binding", token: "n", labels: map[string]string{"actor_binding": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "actor_binding dotted-prefix match", token: "n", labels: map[string]string{"actor_binding.id": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "actor-binding hyphen variant reserved", token: "n", labels: map[string]string{"actor-binding": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "agent-id hyphen variant reserved", token: "n", labels: map[string]string{"agent-id": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "on-behalf-of hyphen variant reserved", token: "n", labels: map[string]string{"on-behalf-of": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "x- prefix reserved", token: "n", labels: map[string]string{"x-custom": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "scion. prefix reserved", token: "n", labels: map[string]string{"scion.internal": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "hub. prefix reserved", token: "n", labels: map[string]string{"hub.internal": "placeholderval"}, wantField: "labels", wantRule: ruleLabelKeyReserved},
		{name: "leading/trailing space in value", token: "n", labels: map[string]string{"k": " v "}, wantField: "labels", wantRule: ruleLabelValueSpacing},
		{name: "disallowed char in value", token: "n", labels: map[string]string{"k": "v!"}, wantField: "labels", wantRule: ruleLabelValueCharset},
		{name: "65-byte label value", token: "n", labels: map[string]string{"k": strings.Repeat("v", uatMaxLabelValueBytes+1)}, wantField: "labels", wantRule: ruleLabelValueLength},
		{name: "129-byte name", token: strings.Repeat("n", uatMaxNameBytes+1), wantField: "name", wantRule: ruleNameLength},
		{name: "control char in name", token: "bad\x00name", wantField: "name", wantRule: ruleControlOrFormat},
		{name: "name resembles a bearer token", token: "scion_pat_abcdefgh", wantField: "name", wantRule: ruleSecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCredentialMetadata(tc.token, tc.purpose, tc.labels)
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			var metaErr *ErrInvalidUATMetadata
			if !asMetadataErr(err, &metaErr) {
				t.Fatalf("expected *ErrInvalidUATMetadata, got %T: %v", err, err)
			}
			// Pin exactly which rule fired (review finding F13): without
			// this, a case could pass through an unintended rule and the
			// table would still go green.
			if metaErr.Field != tc.wantField || metaErr.Rule != tc.wantRule {
				t.Fatalf("got field=%q rule=%q, want field=%q rule=%q", metaErr.Field, metaErr.Rule, tc.wantField, tc.wantRule)
			}
			// The error must name the field/rule but never the offending value.
			for _, v := range tc.labels {
				if v != "" && strings.Contains(err.Error(), v) {
					t.Fatalf("error message echoed the offending label value: %q in %q", v, err.Error())
				}
			}
			if tc.purpose != "" && strings.Contains(err.Error(), tc.purpose) {
				t.Fatalf("error message echoed the offending purpose: %q in %q", tc.purpose, err.Error())
			}
		})
	}
}

func asMetadataErr(err error, target **ErrInvalidUATMetadata) bool {
	if e, ok := err.(*ErrInvalidUATMetadata); ok {
		*target = e
		return true
	}
	return false
}
