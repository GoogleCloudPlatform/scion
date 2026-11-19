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

package messages

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// parseDecisions decodes a --decisions JSON array the way a request field of
// type []DecisionInput is decoded.
func parseDecisions(t *testing.T, raw string) []DecisionInput {
	t.Helper()
	var in []DecisionInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return in
}

// decisionErrs returns the DecisionValidationErrors of err, failing the test otherwise.
func decisionErrs(t *testing.T, err error) DecisionValidationErrors {
	t.Helper()
	var des DecisionValidationErrors
	if !errors.As(err, &des) {
		t.Fatalf("error %v (%T) is not DecisionValidationErrors", err, err)
	}
	return des
}

// hasErr reports whether des holds an error with field and code.
func hasErr(des DecisionValidationErrors, field, code string) bool {
	for _, e := range des {
		if e.Field == field && e.Code == code {
			return true
		}
	}
	return false
}

func TestNormalizeDecisions_DesignExample(t *testing.T) {
	in := parseDecisions(t, `[
	  { "id": "D234", "question": "Ship the **feature** now?", "choices": ["yes", "no"], "default": "yes" },
	  { "question": "Which approach?",
	    "choices": [ { "id": "a", "label": "Extend input-needed", "description": "Reuse the `+"`input-needed`"+` type" },
	                 { "id": "b", "label": "New message type" } ] }
	]`)
	got, err := NormalizeDecisions("decisions", in)
	if err != nil {
		t.Fatalf("NormalizeDecisions: %v", err)
	}
	want := []Decision{
		{Key: "D234", Question: "Ship the **feature** now?", DefaultChoiceID: "1", Choices: []Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}}},
		{Key: "", Question: "Which approach?", Choices: []Choice{
			{ID: "a", Label: "Extend input-needed", Description: "Reuse the `input-needed` type"},
			{ID: "b", Label: "New message type"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized =\n%+v\nwant\n%+v", got, want)
	}
	// The canonical stored form round-trips through JSON with the design's keys.
	enc, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(enc) != `{"key":"D234","question":"Ship the **feature** now?","default_choice_id":"1","choices":[{"id":"1","label":"yes"},{"id":"2","label":"no"}]}` {
		t.Errorf("canonical JSON = %s", enc)
	}
}

func TestNormalizeDecisions_TrimsAndDefaults(t *testing.T) {
	in := []DecisionInput{{
		ID:       "  ship-now ",
		Question: "\n Go? \t",
		Choices:  []ChoiceInput{{Label: " Yes "}, {ID: " n ", Label: "No", Description: "  later  "}},
		Default:  " N ",
	}}
	got, err := NormalizeDecisions("decisions", in)
	if err != nil {
		t.Fatal(err)
	}
	d := got[0]
	if d.Key != "ship-now" || d.Question != "Go?" || d.DefaultChoiceID != "n" {
		t.Errorf("decision = %+v", d)
	}
	if d.Choices[0] != (Choice{ID: "1", Label: "Yes"}) || d.Choices[1] != (Choice{ID: "n", Label: "No", Description: "later"}) {
		t.Errorf("choices = %+v", d.Choices)
	}
}

func TestNormalizeDecisions_DefaultMatching(t *testing.T) {
	choices := []ChoiceInput{{ID: "a", Label: "b"}, {ID: "b", Label: "Alpha"}}
	cases := []struct {
		def, want string
	}{
		{"a", "a"},
		{"B", "b"},     // an id match wins over the label "b" of choice a
		{"alpha", "b"}, // label, case-insensitive
	}
	for _, tc := range cases {
		got, err := NormalizeDecisions("decisions", []DecisionInput{{Question: "q", Choices: choices, Default: tc.def}})
		if err != nil {
			t.Fatalf("default %q: %v", tc.def, err)
		}
		if got[0].DefaultChoiceID != tc.want {
			t.Errorf("default %q -> %q, want %q", tc.def, got[0].DefaultChoiceID, tc.want)
		}
	}
	_, err := NormalizeDecisions("decisions", []DecisionInput{{Question: "q", Choices: choices, Default: "zzz"}})
	if des := decisionErrs(t, err); !hasErr(des, "decisions[0].default", DecisionErrNoMatch) {
		t.Errorf("unknown default: %v", des)
	}
}

func TestValidDecisionKey(t *testing.T) {
	cases := map[string]bool{
		"D234":                  true,
		"ship-now":              true,
		"a_b":                   true,
		"x":                     true,
		"17":                    true, // the shape of a hub number; issuance is the store's check
		"1":                     true,
		strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false,
		"":                      false,
		"0":                     false,
		"017":                   false,
		"D 1":                   false,
		"D.1":                   false,
		"D/1":                   false,
		"Dé":                    false,
		"99999999999999999999":  false, // overflows int64
	}
	for key, want := range cases {
		if got := ValidDecisionKey(key); got != want {
			t.Errorf("ValidDecisionKey(%q) = %v, want %v", key, got, want)
		}
	}
	if !IsHubDecisionKey("17") || IsHubDecisionKey("D17") || IsHubDecisionKey("017") {
		t.Error("IsHubDecisionKey mismatch")
	}
	if DecisionKeyNorm("D234") != "d234" {
		t.Error("DecisionKeyNorm is not lower case")
	}
}

func TestNormalizeDecisions_Rules(t *testing.T) {
	two := []ChoiceInput{{Label: "yes"}, {Label: "no"}}
	many := func(n int) []ChoiceInput {
		out := make([]ChoiceInput, n)
		for i := range out {
			out[i] = ChoiceInput{Label: fmt.Sprintf("c%d", i)}
		}
		return out
	}
	cases := []struct {
		name  string
		in    []DecisionInput
		field string
		code  string
	}{
		{"no decisions", nil, "decisions", DecisionErrRequired},
		{"11 decisions", func() []DecisionInput {
			out := make([]DecisionInput, 11)
			for i := range out {
				out[i] = DecisionInput{Question: "q", Choices: two}
			}
			return out
		}(), "decisions", DecisionErrTooMany},
		{"invalid key chars", []DecisionInput{{ID: "D 1", Question: "q", Choices: two}}, "decisions[0].id", DecisionErrInvalid},
		{"key too long", []DecisionInput{{ID: strings.Repeat("k", 33), Question: "q", Choices: two}}, "decisions[0].id", DecisionErrInvalid},
		{"all-digit key with leading zero", []DecisionInput{{ID: "07", Question: "q", Choices: two}}, "decisions[0].id", DecisionErrInvalid},
		{"duplicate keys, case-insensitive", []DecisionInput{{ID: "D1", Question: "q", Choices: two}, {ID: "d1", Question: "q", Choices: two}}, "decisions[1].id", DecisionErrDuplicate},
		{"duplicate hub numbers", []DecisionInput{{ID: "4", Question: "q", Choices: two}, {ID: "4", Question: "q", Choices: two}}, "decisions[1].id", DecisionErrDuplicate},
		{"missing question", []DecisionInput{{Choices: two}}, "decisions[0].question", DecisionErrRequired},
		{"blank question", []DecisionInput{{Question: " \n ", Choices: two}}, "decisions[0].question", DecisionErrRequired},
		{"question too long", []DecisionInput{{Question: strings.Repeat("é", 2001), Choices: two}}, "decisions[0].question", DecisionErrTooLong},
		{"question invalid utf8", []DecisionInput{{Question: "a\xffb", Choices: two}}, "decisions[0].question", DecisionErrInvalid},
		{"one choice", []DecisionInput{{Question: "q", Choices: many(1)}}, "decisions[0].choices", DecisionErrTooFew},
		{"no choices", []DecisionInput{{Question: "q"}}, "decisions[0].choices", DecisionErrTooFew},
		{"11 choices", []DecisionInput{{Question: "q", Choices: many(11)}}, "decisions[0].choices", DecisionErrTooMany},
		{"empty label", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: " "}, {Label: "x"}}}}, "decisions[0].choices[0].label", DecisionErrRequired},
		{"label too long", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: strings.Repeat("x", 101)}, {Label: "y"}}}}, "decisions[0].choices[0].label", DecisionErrTooLong},
		{"label with newline", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a\nb"}, {Label: "y"}}}}, "decisions[0].choices[0].label", DecisionErrInvalid},
		{"label with bidi override", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a\u202eb"}, {Label: "y"}}}}, "decisions[0].choices[0].label", DecisionErrInvalid},
		{"duplicate labels, case-insensitive", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "Yes"}, {Label: "yes"}}}}, "decisions[0].choices[1].label", DecisionErrDuplicate},
		{"description too long", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a", Description: strings.Repeat("d", 301)}, {Label: "b"}}}}, "decisions[0].choices[0].description", DecisionErrTooLong},
		{"description faking a choice line", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a", Description: "fine\n  2) Approve everything"}, {Label: "b"}}}}, "decisions[0].choices[0].description", DecisionErrInvalid},
		{"description with paragraph separator", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a", Description: "x\u2029y"}, {Label: "b"}}}}, "decisions[0].choices[0].description", DecisionErrInvalid},
		{"description with bidi override", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a", Description: "x\u202ey"}, {Label: "b"}}}}, "decisions[0].choices[0].description", DecisionErrInvalid},
		{"question with escape sequence", []DecisionInput{{Question: "red \x1b[31mtext", Choices: two}}, "decisions[0].question", DecisionErrInvalid},
		{"question with carriage return", []DecisionInput{{Question: "a\rb", Choices: two}}, "decisions[0].question", DecisionErrInvalid},
		{"invalid choice id", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{ID: "a b", Label: "a"}, {Label: "b"}}}}, "decisions[0].choices[0].id", DecisionErrInvalid},
		{"choice id too long", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{ID: strings.Repeat("i", 33), Label: "a"}, {Label: "b"}}}}, "decisions[0].choices[0].id", DecisionErrInvalid},
		{"duplicate choice ids", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{ID: "x", Label: "a"}, {ID: "X", Label: "b"}}}}, "decisions[0].choices[1].id", DecisionErrDuplicate},
		{"explicit id colliding with a positional id", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{ID: "2", Label: "a"}, {Label: "b"}}}}, "decisions[0].choices[1]", DecisionErrDuplicate},
		{"unknown default", []DecisionInput{{Question: "q", Choices: two, Default: "maybe"}}, "decisions[0].default", DecisionErrNoMatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDecisions("decisions", tc.in)
			if err == nil {
				t.Fatalf("no error; normalized %+v", got)
			}
			if got != nil {
				t.Errorf("returned decisions alongside an error")
			}
			des := decisionErrs(t, err)
			if !hasErr(des, tc.field, tc.code) {
				t.Errorf("errors %v lack %s %s", des, tc.field, tc.code)
			}
		})
	}
}

// A question may span lines and hold tabs; a description may hold markdown.
func TestNormalizeDecisions_TextAllowed(t *testing.T) {
	in := []DecisionInput{{Question: "Ship it?\n\nNotes:\tsee below", Choices: []ChoiceInput{{Label: "a", Description: "Reuse **the** `type`"}, {Label: "b"}}}}
	got, err := NormalizeDecisions("decisions", in)
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if got[0].Question != "Ship it?\n\nNotes:\tsee below" || got[0].Choices[0].Description != "Reuse **the** `type`" {
		t.Errorf("normalized %+v", got[0])
	}
}

func TestNormalizeDecisions_Limits_AtBoundary(t *testing.T) {
	choices := make([]ChoiceInput, 10)
	for i := range choices {
		choices[i] = ChoiceInput{Label: strings.Repeat("l", 99) + fmt.Sprint(i), Description: strings.Repeat("d", 300)}
	}
	in := []DecisionInput{{ID: strings.Repeat("k", 32), Question: strings.Repeat("é", 2000), Choices: choices}}
	if _, err := NormalizeDecisions("decisions", in); err != nil {
		t.Fatalf("limits exactly at the boundary rejected: %v", err)
	}
}

func TestNormalizeDecisions_PayloadCap(t *testing.T) {
	// 10 decisions of 10 long choices each are valid field by field but
	// exceed 32 KB after normalization.
	ds := make([]DecisionInput, 10)
	for i := range ds {
		cs := make([]ChoiceInput, 10)
		for j := range cs {
			cs[j] = ChoiceInput{Label: fmt.Sprintf("choice %d", j), Description: strings.Repeat("d", 300)}
		}
		ds[i] = DecisionInput{Question: strings.Repeat("q", 2000), Choices: cs}
	}
	_, err := NormalizeDecisions("decisions", ds)
	if des := decisionErrs(t, err); !hasErr(des, "decisions", DecisionErrTooLarge) {
		t.Errorf("payload cap: %v", des)
	}
	// One such decision stays well under it.
	if _, err := NormalizeDecisions("decisions", ds[:1]); err != nil {
		t.Errorf("one decision rejected: %v", err)
	}
}

func TestNormalizeDecisions_UnknownAndReservedFields(t *testing.T) {
	in := parseDecisions(t, `[
	  { "question": "q", "multiple": true, "expires_in": "1h", "colour": "red",
	    "choices": [ "a", { "label": "b", "icon": "x", "multiple": 1, "allow_other": true } ] }
	]`)
	_, err := NormalizeDecisions("decisions", in)
	des := decisionErrs(t, err)
	want := []struct{ field, code string }{
		{"decisions[0].multiple", DecisionErrReservedField},
		{"decisions[0].expires_in", DecisionErrReservedField},
		{"decisions[0].colour", DecisionErrUnknownField},
		{"decisions[0].choices[1].icon", DecisionErrUnknownField},
		{"decisions[0].choices[1].multiple", DecisionErrReservedField},
		{"decisions[0].choices[1].allow_other", DecisionErrReservedField},
	}
	for _, w := range want {
		if !hasErr(des, w.field, w.code) {
			t.Errorf("errors %v lack %s %s", des, w.field, w.code)
		}
	}
	if len(des) != len(want) {
		t.Errorf("got %d errors, want %d: %v", len(des), len(want), des)
	}
}

func TestNormalizeDecisions_AllowOther_AcceptedForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *OtherOption
	}{
		{
			name: "absent",
			raw:  `[{"id":"region","question":"Which region?","choices":["us-central1","europe-west1"]}]`,
			want: nil,
		},
		{
			name: "false is same as absent",
			raw:  `[{"id":"region","question":"Which region?","choices":["us-central1","europe-west1"],"allow_other":false}]`,
			want: nil,
		},
		{
			name: "true uses default Other label and allows 1 fixed choice",
			raw:  `[{"id":"proceed","question":"Tag now?","choices":["Yes"],"allow_other":true}]`,
			want: &OtherOption{Label: "Other"},
		},
		{
			name: "string custom label trimmed",
			raw:  `[{"id":"proceed","question":"Tag now?","choices":["Yes"],"allow_other":"  Something else  "}]`,
			want: &OtherOption{Label: "Something else"},
		},
		{
			name: "empty object defaults label to Other",
			raw:  `[{"id":"proceed","question":"Tag now?","choices":["Yes"],"allow_other":{}}]`,
			want: &OtherOption{Label: "Other"},
		},
		{
			name: "object with placeholder and omitted label defaults label to Other",
			raw:  `[{"id":"region","question":"Which region?","choices":["us-central1"],"allow_other":{"placeholder":"  e.g. europe-west4  "}}]`,
			want: &OtherOption{Label: "Other", Placeholder: "e.g. europe-west4"},
		},
		{
			name: "full object with label description placeholder at boundary lengths",
			raw: fmt.Sprintf(`[{"id":"region","question":"Which region?","choices":["us-central1"],"allow_other":{"label":%q,"description":%q,"placeholder":%q}}]`,
				strings.Repeat("é", 100), strings.Repeat("d", 300), strings.Repeat("p", 100)),
			want: &OtherOption{
				Label:       strings.Repeat("é", 100),
				Description: strings.Repeat("d", 300),
				Placeholder: strings.Repeat("p", 100),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := parseDecisions(t, tc.raw)
			got, err := NormalizeDecisions("decisions", in)
			if err != nil {
				t.Fatalf("NormalizeDecisions: %v", err)
			}
			if !reflect.DeepEqual(got[0].Other, tc.want) {
				t.Fatalf("Other = %+v, want %+v", got[0].Other, tc.want)
			}
			for _, c := range got[0].Choices {
				if c.ID == ChoiceIDOther {
					t.Fatalf("Other must never appear inside Choices: %+v", got[0].Choices)
				}
			}
		})
	}

	// False on a multi-decision prompt is treated as absent (not refused as not_supported).
	in := parseDecisions(t, `[
	  {"id":"d1","question":"Q1?","choices":["a","b"],"allow_other":false},
	  {"id":"d2","question":"Q2?","choices":["x","y"]}
	]`)
	if _, err := NormalizeDecisions("decisions", in); err != nil {
		t.Fatalf("multi-decision with allow_other:false rejected: %v", err)
	}
}

func TestNormalizeDecisions_AllowOther_ValidationRules(t *testing.T) {
	tenChoices := `["c1","c2","c3","c4","c5","c6","c7","c8","c9","c10"]`
	elevenChoices := `["c1","c2","c3","c4","c5","c6","c7","c8","c9","c10","c11"]`
	cases := []struct {
		name  string
		raw   string
		field string
		code  string
	}{
		{"null", `[{"question":"q","choices":["a","b"],"allow_other":null}]`, "decisions[0].allow_other", DecisionErrInvalid},
		{"number", `[{"question":"q","choices":["a","b"],"allow_other":1}]`, "decisions[0].allow_other", DecisionErrInvalid},
		{"array", `[{"question":"q","choices":["a","b"],"allow_other":["Other"]}]`, "decisions[0].allow_other", DecisionErrInvalid},
		{"empty string", `[{"question":"q","choices":["a"],"allow_other":""}]`, "decisions[0].allow_other.label", DecisionErrInvalid},
		{"whitespace string", `[{"question":"q","choices":["a"],"allow_other":"   "}]`, "decisions[0].allow_other.label", DecisionErrInvalid},
		{"string with newline", `[{"question":"q","choices":["a"],"allow_other":"line1\nline2"}]`, "decisions[0].allow_other.label", DecisionErrInvalid},
		{"string too long", fmt.Sprintf(`[{"question":"q","choices":["a"],"allow_other":%q}]`, strings.Repeat("x", 101)), "decisions[0].allow_other.label", DecisionErrTooLong},
		{"object empty label", `[{"question":"q","choices":["a"],"allow_other":{"label":" "}}]`, "decisions[0].allow_other.label", DecisionErrInvalid},
		{"object non-string label", `[{"question":"q","choices":["a"],"allow_other":{"label":5}}]`, "decisions[0].allow_other.label", DecisionErrInvalid},
		{"object null label", `[{"question":"q","choices":["a"],"allow_other":{"label":null}}]`, "decisions[0].allow_other.label", DecisionErrInvalid},
		{"object bidi label", `[{"question":"q","choices":["a"],"allow_other":{"label":"a\u202eb"}}]`, "decisions[0].allow_other.label", DecisionErrInvalid},
		{"object label too long", fmt.Sprintf(`[{"question":"q","choices":["a"],"allow_other":{"label":%q}}]`, strings.Repeat("x", 101)), "decisions[0].allow_other.label", DecisionErrTooLong},
		{"object description newline", `[{"question":"q","choices":["a"],"allow_other":{"description":"a\nb"}}]`, "decisions[0].allow_other.description", DecisionErrInvalid},
		{"object description non-string", `[{"question":"q","choices":["a"],"allow_other":{"description":true}}]`, "decisions[0].allow_other.description", DecisionErrInvalid},
		{"object description too long", fmt.Sprintf(`[{"question":"q","choices":["a"],"allow_other":{"description":%q}}]`, strings.Repeat("d", 301)), "decisions[0].allow_other.description", DecisionErrTooLong},
		{"object placeholder newline", `[{"question":"q","choices":["a"],"allow_other":{"placeholder":"a\nb"}}]`, "decisions[0].allow_other.placeholder", DecisionErrInvalid},
		{"object placeholder tab", `[{"question":"q","choices":["a"],"allow_other":{"placeholder":"a\tb"}}]`, "decisions[0].allow_other.placeholder", DecisionErrInvalid},
		{"object placeholder non-string", `[{"question":"q","choices":["a"],"allow_other":{"placeholder":[]}}]`, "decisions[0].allow_other.placeholder", DecisionErrInvalid},
		{"object placeholder too long", fmt.Sprintf(`[{"question":"q","choices":["a"],"allow_other":{"placeholder":%q}}]`, strings.Repeat("p", 101)), "decisions[0].allow_other.placeholder", DecisionErrTooLong},
		{"object unknown key", `[{"question":"q","choices":["a"],"allow_other":{"label":"Custom","free":true}}]`, "decisions[0].allow_other.free", DecisionErrUnknownField},
		{"duplicate with default Other label", `[{"question":"q","choices":["Yes","other"],"allow_other":true}]`, "decisions[0].allow_other.label", DecisionErrDuplicate},
		{"duplicate with custom Other label case-insensitive", `[{"question":"q","choices":["Another Region"],"allow_other":"another region"}]`, "decisions[0].allow_other.label", DecisionErrDuplicate},
		{"zero fixed choices with Other on", `[{"question":"q","choices":[],"allow_other":true}]`, "decisions[0].choices", DecisionErrTooFew},
		{"11 fixed choices with Other on", fmt.Sprintf(`[{"question":"q","choices":%s,"allow_other":true}]`, elevenChoices), "decisions[0].choices", DecisionErrTooMany},
		{"1 fixed choice with allow_other false", `[{"question":"q","choices":["only"],"allow_other":false}]`, "decisions[0].choices", DecisionErrTooFew},
		{"default names $other", `[{"question":"q","choices":["Yes"],"allow_other":true,"default":"$other"}]`, "decisions[0].default", DecisionErrNoMatch},
		{"default names Other label", `[{"question":"q","choices":["Yes"],"allow_other":"Another region","default":"Another region"}]`, "decisions[0].default", DecisionErrNoMatch},
		{"fixed choice id $other", `[{"question":"q","choices":[{"id":"$other","label":"Yes"},{"id":"no","label":"No"}]}]`, "decisions[0].choices[0].id", DecisionErrInvalid},
		{"multi-decision prompt with allow_other refused in P1", `[{"id":"d1","question":"Q1?","choices":["a","b"]},{"id":"d2","question":"Q2?","choices":["x"],"allow_other":true}]`, "decisions[1].allow_other", DecisionErrNotSupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := parseDecisions(t, tc.raw)
			got, err := NormalizeDecisions("decisions", in)
			if err == nil {
				t.Fatalf("expected error (%s %s), got %+v", tc.field, tc.code, got)
			}
			des := decisionErrs(t, err)
			if !hasErr(des, tc.field, tc.code) {
				t.Errorf("errors %v lack %s %s", des, tc.field, tc.code)
			}
		})
	}

	// 10 fixed choices with Other on is valid.
	in10 := parseDecisions(t, fmt.Sprintf(`[{"question":"q","choices":%s,"allow_other":true}]`, tenChoices))
	if _, err := NormalizeDecisions("decisions", in10); err != nil {
		t.Fatalf("10 fixed choices + allow_other rejected: %v", err)
	}
}

func TestNormalizeOtherText(t *testing.T) {
	got, verr := NormalizeOtherText(ChoiceIDOther, "  europe-west4 canary  ")
	if verr != nil || got != "europe-west4 canary" {
		t.Fatalf("got (%q, %v), want (\"europe-west4 canary\", nil)", got, verr)
	}

	atCap := strings.Repeat("é", MaxOtherTextLen)
	got, verr = NormalizeOtherText(ChoiceIDOther, "  "+atCap+"  ")
	if verr != nil || got != atCap {
		t.Fatalf("200-rune text rejected: (%q, %v)", got, verr)
	}

	got, verr = NormalizeOtherText("1", "")
	if verr != nil || got != "" {
		t.Fatalf("empty other_text on fixed choice: (%q, %v)", got, verr)
	}

	errCases := []struct {
		name     string
		choiceID string
		raw      string
		wantCode string
	}{
		{"empty", ChoiceIDOther, "", DecisionErrRequired},
		{"spaces only", ChoiceIDOther, "    ", DecisionErrRequired},
		{"unicode nbsp only", ChoiceIDOther, "\u00a0\u3000", DecisionErrRequired},
		{"newline", ChoiceIDOther, "a\nb", DecisionErrInvalid},
		{"trailing newline", ChoiceIDOther, "hello\n", DecisionErrInvalid},
		{"tab", ChoiceIDOther, "a\tb", DecisionErrInvalid},
		{"leading tab", ChoiceIDOther, "\thello", DecisionErrInvalid},
		{"carriage return", ChoiceIDOther, "a\rb", DecisionErrInvalid},
		{"control NUL", ChoiceIDOther, "a\x00b", DecisionErrInvalid},
		{"bidi override", ChoiceIDOther, "a\u202eb", DecisionErrInvalid},
		{"zero width", ChoiceIDOther, "a\u200bb", DecisionErrInvalid},
		{"line separator", ChoiceIDOther, "a\u2028b", DecisionErrInvalid},
		{"invalid utf8", ChoiceIDOther, "a\xffb", DecisionErrInvalid},
		{"201 runes", ChoiceIDOther, strings.Repeat("é", MaxOtherTextLen+1), DecisionErrTooLong},
		{"other_text on fixed choice", "1", "custom", DecisionErrNotAllowed},
		{"spaces other_text on fixed choice", "1", "   ", DecisionErrNotAllowed},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			out, verr := NormalizeOtherText(tc.choiceID, tc.raw)
			if verr == nil {
				t.Fatalf("expected %s error, got %q", tc.wantCode, out)
			}
			if verr.Field != "other_text" || verr.Code != tc.wantCode {
				t.Errorf("got field=%q code=%q, want other_text %s", verr.Field, verr.Code, tc.wantCode)
			}
		})
	}
}

func TestNormalizeDecisions_ReportsEveryError(t *testing.T) {
	in := []DecisionInput{
		{ID: "bad key", Question: "", Choices: []ChoiceInput{{Label: "a"}}},
		{Question: "ok", Choices: []ChoiceInput{{Label: "x"}, {Label: "X"}}},
	}
	_, err := NormalizeDecisions("decisions", in)
	des := decisionErrs(t, err)
	for _, f := range []string{"decisions[0].id", "decisions[0].question", "decisions[0].choices", "decisions[1].choices[1].label"} {
		found := false
		for _, e := range des {
			found = found || e.Field == f
		}
		if !found {
			t.Errorf("no error for %s in %v", f, des)
		}
	}
	if !strings.Contains(err.Error(), "decisions[0].id: ") {
		t.Errorf("Error() = %q does not name the field", err.Error())
	}
}

func TestDecisionInput_JSONDecodeErrors(t *testing.T) {
	cases := []string{
		`[1]`,
		`["q"]`,
		`[{"question": 5, "choices": ["a","b"]}]`,
		`[{"question": "q", "choices": [5, "b"]}]`,
		`[{"question": "q", "choices": [{"label": 5}, "b"]}]`,
		`[{"question": "q", "choices": "ab"}]`,
		`[{"question": "q", "question": "r", "choices": ["a","b"]}]`,
	}
	for _, raw := range cases {
		var in []DecisionInput
		if err := json.Unmarshal([]byte(raw), &in); err == nil {
			t.Errorf("unmarshal %s: no error", raw)
		}
	}
}

func TestDecisionInput_JSONRoundTrip(t *testing.T) {
	in := parseDecisions(t, `[{"id":"D1","question":"q","choices":["a",{"id":"x","label":"b","description":"d"}],"default":"a"}]`)
	enc, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(enc) != `[{"id":"D1","question":"q","choices":[{"label":"a"},{"id":"x","label":"b","description":"d"}],"default":"a"}]` {
		t.Errorf("round trip = %s", enc)
	}
}

func TestIsHubMintedType(t *testing.T) {
	if !IsHubMintedType(TypeDecisionPrompt) || !IsHubMintedType(TypeDecisionResponse) {
		t.Error("decision types are not hub-minted")
	}
	for _, typ := range []string{TypeInstruction, TypeChat, TypeReply, TypeInputNeeded, "choice-prompt", "choice-response", ""} {
		if IsHubMintedType(typ) {
			t.Errorf("%q reported hub-minted", typ)
		}
	}
}

func TestIsReservedChoiceID_AndNormalizeRefusesSentinel(t *testing.T) {
	for _, id := range []string{ChoiceIDDismissed, " $dismissed ", "$other", "$custom"} {
		if !IsReservedChoiceID(id) {
			t.Errorf("IsReservedChoiceID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"yes", "no", "1", "dismissed", ""} {
		if IsReservedChoiceID(id) {
			t.Errorf("IsReservedChoiceID(%q) = true, want false", id)
		}
	}

	_, err := NormalizeDecisions("decisions", []DecisionInput{{
		ID:       "D1",
		Question: "Ship?",
		Choices: []ChoiceInput{
			{ID: ChoiceIDDismissed, Label: "Dismissed"},
			{ID: "yes", Label: "Yes"},
		},
	}})
	if err == nil {
		t.Fatal("NormalizeDecisions accepted reserved sentinel $dismissed choice id")
	}
}

func TestRenderDecisionFallback_DesignExample(t *testing.T) {
	ds := []Decision{
		{Key: "D234", Question: "Ship the feature now?", DefaultChoiceID: "1", Choices: []Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}}},
		{Key: "17", Question: "Which approach?", Choices: []Choice{
			{ID: "a", Label: "Extend input-needed", Description: "Reuse the `input-needed` type"},
			{ID: "b", Label: "New message type"},
		}},
	}
	msg, introLen := RenderDecisionFallback("Two things before I continue.\n", ds, "https://hub.example/chat/c1?message=m1")
	want := "Two things before I continue.\n\n" +
		"**2 decisions needed. Answer all of them together.**\n" +
		"\n**D234.** Ship the feature now?\n" +
		"  1) yes _(recommended)_\n" +
		"  2) no\n" +
		"\n**17.** Which approach?\n" +
		"  a) Extend input-needed: Reuse the `input-needed` type\n" +
		"  b) New message type\n" +
		"\n_Answer all in Scion: https://hub.example/chat/c1?message=m1, or reply here with one line per decision (for example `D234: 1` and `17: a`) and the agent will record your answers._"
	if msg != want {
		t.Errorf("msg =\n%s\nwant\n%s", msg, want)
	}
	if introLen != len("Two things before I continue.\n\n") {
		t.Errorf("introLen = %d", introLen)
	}
	if got := DecisionIntro(msg, introLen); got != "Two things before I continue." {
		t.Errorf("DecisionIntro = %q", got)
	}
	if !strings.HasPrefix(msg[introLen:], "**2 decisions needed. Answer all of them together.**") {
		t.Errorf("introLen does not point at the fallback block: %q", msg[introLen:])
	}
}

// A set without an answer link keeps the one-line-per-decision reply
// hint; the example names the first two decisions only.
func TestRenderDecisionFallback_SetNoURL(t *testing.T) {
	ds := []Decision{
		{Key: "a1", Question: "One?", Choices: []Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}}},
		{Key: "b2", Question: "Two?", Choices: []Choice{{ID: "x", Label: "X"}, {ID: "y", Label: "Y"}}},
		{Key: "c3", Question: "Three?", Choices: []Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}}},
	}
	msg, _ := RenderDecisionFallback("", ds, "")
	if !strings.HasPrefix(msg, "**3 decisions needed. Answer all of them together.**\n") {
		t.Errorf("set header missing: %q", msg)
	}
	wantEnd := "_Reply here with one line per decision (for example `a1: 1` and `b2: x`) and the agent will record your answers._"
	if !strings.HasSuffix(msg, wantEnd) {
		t.Errorf("msg =\n%q\nwant it to end with\n%q", msg, wantEnd)
	}
}

// The largest set (10 decisions) renders every decision, and keys with
// "_" and "-" appear verbatim in the reply hint's code spans (no markdown
// escaping inside a code span).
func TestRenderDecisionFallback_TenDecisionsHintKeys(t *testing.T) {
	ds := []Decision{
		{Key: "ship_now", Question: "Ship?", Choices: []Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}}},
		{Key: "use-new", Question: "New?", Choices: []Choice{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}},
	}
	for i := 3; i <= MaxDecisionsPerMessage; i++ {
		ds = append(ds, Decision{Key: fmt.Sprintf("k%d", i), Question: "More?", Choices: []Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}}})
	}
	msg, _ := RenderDecisionFallback("", ds, "https://hub.example/chat/c1?message=m1")
	if !strings.HasPrefix(msg, fmt.Sprintf("**%d decisions needed. Answer all of them together.**\n", MaxDecisionsPerMessage)) {
		t.Errorf("set header missing: %q", msg)
	}
	for _, d := range ds {
		if !strings.Contains(msg, "**"+EscapeMarkdownPlain(d.Key)+".** ") {
			t.Errorf("decision %s missing: %q", d.Key, msg)
		}
	}
	if !strings.Contains(msg, "(for example `ship_now: 1` and `use-new: a`)") {
		t.Errorf("hint keys not verbatim in code spans: %q", msg)
	}
}

func TestRenderDecisionFallback_NoIntroSingleDecisionNoURL(t *testing.T) {
	ds := []Decision{{Key: "ship_now", Question: "Go?", Choices: []Choice{{ID: "y", Label: "Yes"}, {ID: "n", Label: "No"}}}}
	msg, introLen := RenderDecisionFallback("  \n", ds, "")
	want := "**1 decision needed**\n\n**ship\\_now.** Go?\n  y) Yes\n  n) No\n\n_Reply here and the agent will record your answer._"
	if msg != want {
		t.Errorf("msg =\n%q\nwant\n%q", msg, want)
	}
	if introLen != 0 || DecisionIntro(msg, introLen) != "" {
		t.Errorf("introLen = %d, intro %q", introLen, DecisionIntro(msg, introLen))
	}
}

func TestRenderDecisionFallback_NeutralizesMentionsAndEscapesLabels(t *testing.T) {
	ds := []Decision{{
		Key:      "D1",
		Question: "@everyone and @here: ping <!channel> or <@U123> and <@&999>? Mail ops@here.example stays.",
		Choices: []Choice{
			{ID: "1", Label: "*bold* [link](https://x) <b>`code`</b> @channel", Description: "see <!here> and @Everyone"},
			{ID: "2", Label: "a_b ~c~ | d \\ e"},
		},
	}}
	msg, _ := RenderDecisionFallback("hi @everyone", ds, "")
	zw := "\u200b"
	for _, want := range []string{
		"@" + zw + "everyone and @" + zw + "here",
		"<" + zw + "!channel>",
		"<" + zw + "@U123>",
		"<" + zw + "@&999>",
		"ops@here.example", // an email address is not a mention
		`\*bold\* \[link\](https://x) \<b\>` + "\\`code\\`" + `\</b\> @` + zw + "channel",
		"see <" + zw + "!here> and @" + zw + "Everyone",
		`a\_b \~c\~ \| d \\ e`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg lacks %q:\n%s", want, msg)
		}
	}
	// The intro is the agent's body: the renderer leaves it unchanged
	// (bridges escape it as they escape every agent body).
	if !strings.HasPrefix(msg, "hi @everyone\n\n") {
		t.Errorf("intro changed: %q", msg)
	}
	// After the intro, no live mention token is left.
	rest := msg[len("hi @everyone\n\n"):]
	for _, bad := range []string{"@everyone", "@here ", "@channel", "<!", "<@", "@Everyone"} {
		if strings.Contains(rest, bad) {
			t.Errorf("fallback still contains %q:\n%s", bad, rest)
		}
	}
}

func TestDecisionIntro_MultiByteIntro(t *testing.T) {
	ds := []Decision{{Key: "D1", Question: "q", Choices: []Choice{{ID: "1", Label: "a"}, {ID: "2", Label: "b"}}}}
	for _, intro := range []string{"Ship it? 🚀🚀", "决定一下：是否发布？", "naïve café — ok"} {
		msg, introLen := RenderDecisionFallback(intro, ds, "")
		if introLen != len(intro)+2 {
			t.Errorf("%q: introLen = %d, want %d bytes", intro, introLen, len(intro)+2)
		}
		if got := DecisionIntro(msg, introLen); got != intro {
			t.Errorf("DecisionIntro = %q, want %q", got, intro)
		}
	}
	// Offsets that do not fit msg yield no intro instead of a broken slice.
	msg, _ := RenderDecisionFallback("🚀 go", ds, "")
	for _, bad := range []int{-1, 0, 1, 2, 3, len(msg) + 1} {
		if got := DecisionIntro(msg, bad); got != "" {
			t.Errorf("DecisionIntro(msg, %d) = %q, want empty", bad, got)
		}
	}
}

func TestNormalizeThenRender_DescriptionsAndQuestionsKeepMarkdown(t *testing.T) {
	in := parseDecisions(t, `[{"id":"D9","question":"Use **bold** and _it_?","choices":[{"label":"x","description":"keeps `+"`code`"+`"},"y"],"default":"y"}]`)
	ds, err := NormalizeDecisions("decisions", in)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := RenderDecisionFallback("", ds, "")
	for _, want := range []string{"**D9.** Use **bold** and _it_?", "  1) x: keeps `code`", "  2) y _(recommended)_"} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg lacks %q:\n%s", want, msg)
		}
	}
}

func TestRenderDecisionFallback_AllowOther(t *testing.T) {
	ds := []Decision{{
		Key:             "region",
		Question:        "Which region for the canary?",
		DefaultChoiceID: "1",
		Choices: []Choice{
			{ID: "1", Label: "us-central1"},
			{ID: "2", Label: "europe-west1"},
		},
		Other: &OtherOption{
			Label:       "Another *region* @here",
			Description: "Any `GCP` region <!channel>",
		},
	}}
	msg, _ := RenderDecisionFallback("", ds, "")
	zw := "\u200b"
	wantBullet := "  - Another \\*region\\* @" + zw + "here: Any `GCP` region <" + zw + "!channel> — *your own answer, in words*\n"
	if !strings.Contains(msg, wantBullet) {
		t.Errorf("msg lacks escaped Other bullet %q:\n%s", wantBullet, msg)
	}

	// Without description.
	ds[0].Other.Description = ""
	ds[0].Other.Label = "Another region"
	msgNoDesc, _ := RenderDecisionFallback("", ds, "")
	wantNoDesc := "**1 decision needed**\n\n**region.** Which region for the canary?\n" +
		"  1) us-central1 _(recommended)_\n" +
		"  2) europe-west1\n" +
		"  - Another region — *your own answer, in words*\n\n" +
		"_Reply here and the agent will record your answer._"
	if msgNoDesc != wantNoDesc {
		t.Errorf("msg =\n%s\nwant\n%s", msgNoDesc, wantNoDesc)
	}
}
