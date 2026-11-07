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

// parseDecisions decodes a --choices JSON array the way a request field of
// type []DecisionInput is decoded.
func parseDecisions(t *testing.T, raw string) []DecisionInput {
	t.Helper()
	var in []DecisionInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return in
}

// choiceErrs returns the ChoiceErrors of err, failing the test otherwise.
func choiceErrs(t *testing.T, err error) ChoiceErrors {
	t.Helper()
	var ces ChoiceErrors
	if !errors.As(err, &ces) {
		t.Fatalf("error %v (%T) is not ChoiceErrors", err, err)
	}
	return ces
}

// hasErr reports whether ces holds an error with field and code.
func hasErr(ces ChoiceErrors, field, code string) bool {
	for _, e := range ces {
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
	got, err := NormalizeDecisions("choices", in)
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
	got, err := NormalizeDecisions("choices", in)
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
		got, err := NormalizeDecisions("choices", []DecisionInput{{Question: "q", Choices: choices, Default: tc.def}})
		if err != nil {
			t.Fatalf("default %q: %v", tc.def, err)
		}
		if got[0].DefaultChoiceID != tc.want {
			t.Errorf("default %q -> %q, want %q", tc.def, got[0].DefaultChoiceID, tc.want)
		}
	}
	_, err := NormalizeDecisions("choices", []DecisionInput{{Question: "q", Choices: choices, Default: "zzz"}})
	if ces := choiceErrs(t, err); !hasErr(ces, "choices[0].default", ChoiceErrNoMatch) {
		t.Errorf("unknown default: %v", ces)
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
		{"no decisions", nil, "choices", ChoiceErrRequired},
		{"11 decisions", func() []DecisionInput {
			out := make([]DecisionInput, 11)
			for i := range out {
				out[i] = DecisionInput{Question: "q", Choices: two}
			}
			return out
		}(), "choices", ChoiceErrTooMany},
		{"invalid key chars", []DecisionInput{{ID: "D 1", Question: "q", Choices: two}}, "choices[0].id", ChoiceErrInvalid},
		{"key too long", []DecisionInput{{ID: strings.Repeat("k", 33), Question: "q", Choices: two}}, "choices[0].id", ChoiceErrInvalid},
		{"all-digit key with leading zero", []DecisionInput{{ID: "07", Question: "q", Choices: two}}, "choices[0].id", ChoiceErrInvalid},
		{"duplicate keys, case-insensitive", []DecisionInput{{ID: "D1", Question: "q", Choices: two}, {ID: "d1", Question: "q", Choices: two}}, "choices[1].id", ChoiceErrDuplicate},
		{"duplicate hub numbers", []DecisionInput{{ID: "4", Question: "q", Choices: two}, {ID: "4", Question: "q", Choices: two}}, "choices[1].id", ChoiceErrDuplicate},
		{"missing question", []DecisionInput{{Choices: two}}, "choices[0].question", ChoiceErrRequired},
		{"blank question", []DecisionInput{{Question: " \n ", Choices: two}}, "choices[0].question", ChoiceErrRequired},
		{"question too long", []DecisionInput{{Question: strings.Repeat("é", 2001), Choices: two}}, "choices[0].question", ChoiceErrTooLong},
		{"question invalid utf8", []DecisionInput{{Question: "a\xffb", Choices: two}}, "choices[0].question", ChoiceErrInvalid},
		{"one choice", []DecisionInput{{Question: "q", Choices: many(1)}}, "choices[0].choices", ChoiceErrTooFew},
		{"no choices", []DecisionInput{{Question: "q"}}, "choices[0].choices", ChoiceErrTooFew},
		{"11 choices", []DecisionInput{{Question: "q", Choices: many(11)}}, "choices[0].choices", ChoiceErrTooMany},
		{"empty label", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: " "}, {Label: "x"}}}}, "choices[0].choices[0].label", ChoiceErrRequired},
		{"label too long", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: strings.Repeat("x", 101)}, {Label: "y"}}}}, "choices[0].choices[0].label", ChoiceErrTooLong},
		{"label with newline", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a\nb"}, {Label: "y"}}}}, "choices[0].choices[0].label", ChoiceErrInvalid},
		{"label with bidi override", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a\u202eb"}, {Label: "y"}}}}, "choices[0].choices[0].label", ChoiceErrInvalid},
		{"duplicate labels, case-insensitive", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "Yes"}, {Label: "yes"}}}}, "choices[0].choices[1].label", ChoiceErrDuplicate},
		{"description too long", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{Label: "a", Description: strings.Repeat("d", 301)}, {Label: "b"}}}}, "choices[0].choices[0].description", ChoiceErrTooLong},
		{"invalid choice id", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{ID: "a b", Label: "a"}, {Label: "b"}}}}, "choices[0].choices[0].id", ChoiceErrInvalid},
		{"choice id too long", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{ID: strings.Repeat("i", 33), Label: "a"}, {Label: "b"}}}}, "choices[0].choices[0].id", ChoiceErrInvalid},
		{"duplicate choice ids", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{ID: "x", Label: "a"}, {ID: "X", Label: "b"}}}}, "choices[0].choices[1].id", ChoiceErrDuplicate},
		{"explicit id colliding with a positional id", []DecisionInput{{Question: "q", Choices: []ChoiceInput{{ID: "2", Label: "a"}, {Label: "b"}}}}, "choices[0].choices[1]", ChoiceErrDuplicate},
		{"unknown default", []DecisionInput{{Question: "q", Choices: two, Default: "maybe"}}, "choices[0].default", ChoiceErrNoMatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDecisions("choices", tc.in)
			if err == nil {
				t.Fatalf("no error; normalized %+v", got)
			}
			if got != nil {
				t.Errorf("returned decisions alongside an error")
			}
			ces := choiceErrs(t, err)
			if !hasErr(ces, tc.field, tc.code) {
				t.Errorf("errors %v lack %s %s", ces, tc.field, tc.code)
			}
		})
	}
}

func TestNormalizeDecisions_Limits_AtBoundary(t *testing.T) {
	choices := make([]ChoiceInput, 10)
	for i := range choices {
		choices[i] = ChoiceInput{Label: strings.Repeat("l", 99) + fmt.Sprint(i), Description: strings.Repeat("d", 300)}
	}
	in := []DecisionInput{{ID: strings.Repeat("k", 32), Question: strings.Repeat("é", 2000), Choices: choices}}
	if _, err := NormalizeDecisions("choices", in); err != nil {
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
	_, err := NormalizeDecisions("choices", ds)
	if ces := choiceErrs(t, err); !hasErr(ces, "choices", ChoiceErrTooLarge) {
		t.Errorf("payload cap: %v", ces)
	}
	// One such decision stays well under it.
	if _, err := NormalizeDecisions("choices", ds[:1]); err != nil {
		t.Errorf("one decision rejected: %v", err)
	}
}

func TestNormalizeDecisions_UnknownAndReservedFields(t *testing.T) {
	in := parseDecisions(t, `[
	  { "question": "q", "multiple": true, "allow_other": true, "expires_in": "1h", "colour": "red",
	    "choices": [ "a", { "label": "b", "icon": "x", "multiple": 1 } ] }
	]`)
	_, err := NormalizeDecisions("choices", in)
	ces := choiceErrs(t, err)
	want := []struct{ field, code string }{
		{"choices[0].multiple", ChoiceErrReservedField},
		{"choices[0].allow_other", ChoiceErrReservedField},
		{"choices[0].expires_in", ChoiceErrReservedField},
		{"choices[0].colour", ChoiceErrUnknownField},
		{"choices[0].choices[1].icon", ChoiceErrUnknownField},
		{"choices[0].choices[1].multiple", ChoiceErrReservedField},
	}
	for _, w := range want {
		if !hasErr(ces, w.field, w.code) {
			t.Errorf("errors %v lack %s %s", ces, w.field, w.code)
		}
	}
	if len(ces) != len(want) {
		t.Errorf("got %d errors, want %d: %v", len(ces), len(want), ces)
	}
}

func TestNormalizeDecisions_ReportsEveryError(t *testing.T) {
	in := []DecisionInput{
		{ID: "bad key", Question: "", Choices: []ChoiceInput{{Label: "a"}}},
		{Question: "ok", Choices: []ChoiceInput{{Label: "x"}, {Label: "X"}}},
	}
	_, err := NormalizeDecisions("choices", in)
	ces := choiceErrs(t, err)
	for _, f := range []string{"choices[0].id", "choices[0].question", "choices[0].choices", "choices[1].choices[1].label"} {
		found := false
		for _, e := range ces {
			found = found || e.Field == f
		}
		if !found {
			t.Errorf("no error for %s in %v", f, ces)
		}
	}
	if !strings.Contains(err.Error(), "choices[0].id: ") {
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
	if !IsHubMintedType(TypeChoicePrompt) || !IsHubMintedType(TypeChoiceResponse) {
		t.Error("choice types are not hub-minted")
	}
	for _, typ := range []string{TypeInstruction, TypeChat, TypeReply, TypeInputNeeded, ""} {
		if IsHubMintedType(typ) {
			t.Errorf("%q reported hub-minted", typ)
		}
	}
}

func TestRenderChoiceFallback_DesignExample(t *testing.T) {
	ds := []Decision{
		{Key: "D234", Question: "Ship the feature now?", DefaultChoiceID: "1", Choices: []Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}}},
		{Key: "17", Question: "Which approach?", Choices: []Choice{
			{ID: "a", Label: "Extend input-needed", Description: "Reuse the `input-needed` type"},
			{ID: "b", Label: "New message type"},
		}},
	}
	msg, introLen := RenderChoiceFallback("Two things before I continue.\n", ds, "https://hub.example/chat/c1?message=m1")
	want := "Two things before I continue.\n\n" +
		"**2 decisions needed**\n" +
		"\n**D234.** Ship the feature now?\n" +
		"  1) yes _(recommended)_\n" +
		"  2) no\n" +
		"\n**17.** Which approach?\n" +
		"  a) Extend input-needed: Reuse the `input-needed` type\n" +
		"  b) New message type\n" +
		"\n_Answer in Scion: https://hub.example/chat/c1?message=m1, or reply here and the agent will record your answer._"
	if msg != want {
		t.Errorf("msg =\n%s\nwant\n%s", msg, want)
	}
	if introLen != len("Two things before I continue.\n\n") {
		t.Errorf("introLen = %d", introLen)
	}
	if got := ChoiceIntro(msg, introLen); got != "Two things before I continue." {
		t.Errorf("ChoiceIntro = %q", got)
	}
	if !strings.HasPrefix(msg[introLen:], "**2 decisions needed**") {
		t.Errorf("introLen does not point at the fallback block: %q", msg[introLen:])
	}
}

func TestRenderChoiceFallback_NoIntroSingleDecisionNoURL(t *testing.T) {
	ds := []Decision{{Key: "ship_now", Question: "Go?", Choices: []Choice{{ID: "y", Label: "Yes"}, {ID: "n", Label: "No"}}}}
	msg, introLen := RenderChoiceFallback("  \n", ds, "")
	want := "**1 decision needed**\n\n**ship\\_now.** Go?\n  y) Yes\n  n) No\n\n_Reply here and the agent will record your answer._"
	if msg != want {
		t.Errorf("msg =\n%q\nwant\n%q", msg, want)
	}
	if introLen != 0 || ChoiceIntro(msg, introLen) != "" {
		t.Errorf("introLen = %d, intro %q", introLen, ChoiceIntro(msg, introLen))
	}
}

func TestRenderChoiceFallback_NeutralizesMentionsAndEscapesLabels(t *testing.T) {
	ds := []Decision{{
		Key:      "D1",
		Question: "@everyone and @here: ping <!channel> or <@U123> and <@&999>? Mail ops@here.example stays.",
		Choices: []Choice{
			{ID: "1", Label: "*bold* [link](https://x) <b>`code`</b> @channel", Description: "see <!here> and @Everyone"},
			{ID: "2", Label: "a_b ~c~ | d \\ e"},
		},
	}}
	msg, _ := RenderChoiceFallback("hi @everyone", ds, "")
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

func TestChoiceIntro_MultiByteIntro(t *testing.T) {
	ds := []Decision{{Key: "D1", Question: "q", Choices: []Choice{{ID: "1", Label: "a"}, {ID: "2", Label: "b"}}}}
	for _, intro := range []string{"Ship it? 🚀🚀", "决定一下：是否发布？", "naïve café — ok"} {
		msg, introLen := RenderChoiceFallback(intro, ds, "")
		if introLen != len(intro)+2 {
			t.Errorf("%q: introLen = %d, want %d bytes", intro, introLen, len(intro)+2)
		}
		if got := ChoiceIntro(msg, introLen); got != intro {
			t.Errorf("ChoiceIntro = %q, want %q", got, intro)
		}
	}
	// Offsets that do not fit msg yield no intro instead of a broken slice.
	msg, _ := RenderChoiceFallback("🚀 go", ds, "")
	for _, bad := range []int{-1, 0, 1, 2, 3, len(msg) + 1} {
		if got := ChoiceIntro(msg, bad); got != "" {
			t.Errorf("ChoiceIntro(msg, %d) = %q, want empty", bad, got)
		}
	}
}

func TestNormalizeThenRender_DescriptionsAndQuestionsKeepMarkdown(t *testing.T) {
	in := parseDecisions(t, `[{"id":"D9","question":"Use **bold** and _it_?","choices":[{"label":"x","description":"keeps `+"`code`"+`"},"y"],"default":"y"}]`)
	ds, err := NormalizeDecisions("choices", in)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := RenderChoiceFallback("", ds, "")
	for _, want := range []string{"**D9.** Use **bold** and _it_?", "  1) x: keeps `code`", "  2) y _(recommended)_"} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg lacks %q:\n%s", want, msg)
		}
	}
}
