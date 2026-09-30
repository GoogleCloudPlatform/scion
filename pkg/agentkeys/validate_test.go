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

package agentkeys

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateBody_Valid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"literal text", `{"keys":"hello"}`, "hello"},
		{"named key", `{"keys":"Enter"}`, "Enter"},
		{"control key", `{"keys":"C-c"}`, "C-c"},
		{"whitespace preserved, leading/trailing", `{"keys":"  spaced out  "}`, "  spaced out  "},
		{"whitespace-only is valid input", `{"keys":"   "}`, "   "},
		{"unicode preserved verbatim", `{"keys":"héllo 世界"}`, "héllo 世界"},
		{"literal @name is text, not mention syntax", `{"keys":"@builder do the thing"}`, "@builder do the thing"},
		{"internal spaces are not tokenized", `{"keys":"Up Up Enter"}`, "Up Up Enter"},
		// The field name is "keys" spelled entirely as \uXXXX escapes.
		// RFC 8259 requires decoders to unescape field names before
		// comparison, exactly like string values; encoding/json's
		// Token() already does this, so this proves the "key != \"keys\""
		// check in ValidateBody is comparing decoded text, not raw bytes.
		{"escaped field name still means \"keys\"", `{"\u006b\u0065\u0079\u0073":"a"}`, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateBody([]byte(tc.body))
			if err != nil {
				t.Fatalf("ValidateBody(%q) unexpected error: %v", tc.body, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestValidateBody_UnknownField(t *testing.T) {
	_, err := ValidateBody([]byte(`{"keys":"Enter","sender":"user:me"}`))
	_ = assertInvalid(t, err)
}

// TestValidateBody_CaseVariantFieldNameRejected proves field-name matching
// is exact and case-sensitive: "KEYS" is not "keys", and must be rejected as
// an unknown field rather than accepted case-insensitively.
func TestValidateBody_CaseVariantFieldNameRejected(t *testing.T) {
	_, err := ValidateBody([]byte(`{"KEYS":"a"}`))
	_ = assertInvalid(t, err)
}

func TestValidateBody_DuplicateKeysField(t *testing.T) {
	_, err := ValidateBody([]byte(`{"keys":"a","keys":"b"}`))
	_ = assertInvalid(t, err)
}

func TestValidateBody_MissingField(t *testing.T) {
	_, err := ValidateBody([]byte(`{}`))
	_ = assertInvalid(t, err)
}

func TestValidateBody_NonStringValue(t *testing.T) {
	cases := []string{
		`{"keys":123}`,
		`{"keys":true}`,
		`{"keys":null}`,
		`{"keys":["a","b"]}`,
		`{"keys":{"nested":"object"}}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			_, err := ValidateBody([]byte(body))
			_ = assertInvalid(t, err)
		})
	}
}

func TestValidateBody_Empty(t *testing.T) {
	_, err := ValidateBody([]byte(`{"keys":""}`))
	_ = assertInvalid(t, err)
}

// TestValidateBody_RawNULByteIsAJSONSyntaxError documents that a literal
// (unescaped) 0x00 byte inside a JSON string is invalid JSON syntax — it
// never reaches the NUL rule in validateDecodedKeys at all, because
// encoding/json's tokenizer rejects it first. This is intentionally a
// separate test from TestValidateBody_EscapedNULIsRejected below: deleting
// the strings.IndexByte NUL check in validateDecodedKeys would leave this
// test green on its own.
func TestValidateBody_RawNULByteIsAJSONSyntaxError(t *testing.T) {
	_, err := ValidateBody([]byte("{\"keys\":\"a\x00b\"}"))
	_ = assertInvalid(t, err)
}

// TestValidateBody_EscapedNULIsRejected exercises the NUL rule itself: a
// JSON-escaped \u0000, which is syntactically valid JSON and decodes
// successfully to a Go string containing a NUL byte, must still be rejected
// by validateDecodedKeys's explicit NUL check. Without this test, deleting
// that check would leave the suite green because nothing else exercises the
// escape path.
func TestValidateBody_EscapedNULIsRejected(t *testing.T) {
	_, err := ValidateBody([]byte(`{"keys":"a\u0000b"}`))
	ve := assertInvalid(t, err)
	if !strings.Contains(ve.Error(), "NUL") {
		t.Fatalf("error %q does not mention the NUL rule; want confirmation this came from the NUL check, not some other rejection", ve.Error())
	}
}

// TestValidateKeys_NULIsRejected is ValidateKeys's equivalent direct check
// (no JSON escaping involved: the NUL byte is already a literal byte in the
// Go string).
func TestValidateKeys_NULIsRejected(t *testing.T) {
	err := ValidateKeys("a\x00b")
	ve := assertInvalid(t, err)
	if !strings.Contains(ve.Error(), "NUL") {
		t.Fatalf("error %q does not mention the NUL rule", ve.Error())
	}
}

func TestValidateBody_SizeCeiling(t *testing.T) {
	within := strings.Repeat("a", MaxBytes)
	if _, err := ValidateBody([]byte(`{"keys":"` + within + `"}`)); err != nil {
		t.Fatalf("expected %d bytes to be within the ceiling, got error: %v", MaxBytes, err)
	}

	tooBig := strings.Repeat("a", MaxBytes+1)
	_, err := ValidateBody([]byte(`{"keys":"` + tooBig + `"}`))
	if err == nil {
		t.Fatalf("expected an error for %d bytes", MaxBytes+1)
	}
	ve, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
	}
	if ve.Outcome != OutcomePayloadTooLarge {
		t.Fatalf("Outcome = %v, want %v", ve.Outcome, OutcomePayloadTooLarge)
	}
}

// TestValidateBody_MultibyteSizeCeiling proves the byte ceiling is counted
// in UTF-8 bytes, not runes: "é" is one rune but two bytes, so 2048 of them
// is exactly MaxBytes and 2049 is one over.
func TestValidateBody_MultibyteSizeCeiling(t *testing.T) {
	const char = "é" // 2 UTF-8 bytes
	if len(char) != 2 {
		t.Fatalf("test assumption broken: %q is %d bytes, want 2", char, len(char))
	}

	within := strings.Repeat(char, MaxBytes/2)
	got, err := ValidateBody([]byte(`{"keys":"` + within + `"}`))
	if err != nil {
		t.Fatalf("expected %d bytes (%d runes) to be within the ceiling, got error: %v", len(within), MaxBytes/2, err)
	}
	if got != within {
		t.Fatalf("round-tripped value changed")
	}

	tooBig := strings.Repeat(char, MaxBytes/2+1)
	_, err = ValidateBody([]byte(`{"keys":"` + tooBig + `"}`))
	ve, ok := AsValidationError(err)
	if !ok || ve.Outcome != OutcomePayloadTooLarge {
		t.Fatalf("expected OutcomePayloadTooLarge for %d bytes, got %v", len(tooBig), err)
	}
}

func TestValidateBody_NotAnObject(t *testing.T) {
	cases := []string{`"hello"`, `42`, `["keys"]`, `null`, ``}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			_, err := ValidateBody([]byte(body))
			_ = assertInvalid(t, err)
		})
	}
}

// TestValidateBody_InvalidUTF8Rejected: a raw invalid UTF-8 byte inside the
// request body must be rejected, not silently replaced with U+FFFD the way
// encoding/json's own string decoding would (both Token() and Unmarshal do
// this). Without this check, {"keys":"a\xffb"} would be accepted as "a�b"
// — 3 input bytes silently becoming a different 3-byte sequence.
func TestValidateBody_InvalidUTF8Rejected(t *testing.T) {
	body := []byte{'{', '"', 'k', 'e', 'y', 's', '"', ':', '"', 'a', 0xff, 'b', '"', '}'}
	_, err := ValidateBody(body)
	_ = assertInvalid(t, err)
}

// TestValidateBody_UnpairedSurrogateEscapeRejected: a lone (unpaired) UTF-16
// surrogate escape is syntactically valid JSON (the raw bytes are plain
// ASCII, so utf8.Valid(body) alone would not catch it) but has no valid
// Unicode meaning on its own. encoding/json would decode it to U+FFFD; this
// function must reject it instead.
func TestValidateBody_UnpairedSurrogateEscapeRejected(t *testing.T) {
	cases := map[string]string{
		"lone high surrogate":                                   `{"keys":"\ud800"}`,
		"lone low surrogate":                                    `{"keys":"\udc00"}`,
		"high surrogate followed by literal text":               `{"keys":"\ud800x"}`,
		"high surrogate followed by a non-surrogate \\u escape": "{\"keys\":\"\\ud800\\u0041\"}",
		"two high surrogates in a row":                          `{"keys":"\ud800\ud800"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateBody([]byte(body))
			_ = assertInvalid(t, err)
		})
	}
}

// TestValidateBody_ValidSurrogatePairAccepted exercises the pairing branch
// of decodeUnicodeEscape directly: a real 😀 escape sequence
// (encoding U+1F600 as a UTF-16 surrogate pair via two \u escapes, not as
// literal UTF-8 bytes) must decode to the same string a correct
// implementation of encoding/json would produce. Both lowercase and
// uppercase hex digits are covered, since decodeUnicodeEscape parses them
// with strconv.ParseUint, which accepts both.
func TestValidateBody_ValidSurrogatePairAccepted(t *testing.T) {
	want := "\U0001F600"
	cases := map[string]string{
		"lowercase hex": `{"keys":"\ud83d\ude00"}`,
		"uppercase hex": `{"keys":"\uD83D\uDE00"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ValidateBody([]byte(body))
			if err != nil {
				t.Fatalf("ValidateBody(%s) unexpected error: %v", body, err)
			}
			if got != want {
				t.Fatalf("got %q (% x), want %q (% x)", got, []byte(got), want, []byte(want))
			}
			// Cross-check against the standard library on the exact same
			// escape sequence, so this test would fail if
			// decodeUnicodeEscape's pairing arithmetic ever silently
			// diverged from RFC 8259 semantics.
			var viaStdlib string
			quoted := body[len(`{"keys":`) : len(body)-len("}")]
			if err := json.Unmarshal([]byte(quoted), &viaStdlib); err != nil {
				t.Fatalf("encoding/json could not decode the same escape %s: %v", quoted, err)
			}
			if viaStdlib != want {
				t.Fatalf("test itself is wrong: encoding/json decoded %s to %q, not %q", quoted, viaStdlib, want)
			}
		})
	}
}

// TestValidateBody_LiteralAstralCharacterAccepted covers the companion case
// to the surrogate-*escape* test above: an astral character given as literal
// UTF-8 bytes (not a \u escape at all) must also round-trip unchanged.
func TestValidateBody_LiteralAstralCharacterAccepted(t *testing.T) {
	want := "😀"
	got, err := ValidateBody([]byte(`{"keys":"` + want + `"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("got %q (% x), want %q (% x)", got, []byte(got), want, []byte(want))
	}
}

// TestValidateBody_SimpleEscapes is a table test over every simple escape
// RFC 8259 §7 defines, plus a BMP \uXXXX escape, so a regression that swaps
// or drops one of the switch cases in decodeJSONString is caught even though
// it would not affect the surrogate-pairing tests above.
func TestValidateBody_SimpleEscapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{`\"`, `{"keys":"a\"b"}`, `a"b`},
		{`\\`, `{"keys":"a\\b"}`, `a\b`},
		{`\/`, `{"keys":"a\/b"}`, `a/b`},
		{`\b`, `{"keys":"a\bb"}`, "a\bb"},
		{`\f`, `{"keys":"a\fb"}`, "a\fb"},
		{`\n`, `{"keys":"a\nb"}`, "a\nb"},
		{`\r`, `{"keys":"a\rb"}`, "a\rb"},
		{`\t`, `{"keys":"a\tb"}`, "a\tb"},
		{`\u0041 (BMP, ASCII, digits only)`, `{"keys":"a\u0041b"}`, "aAb"},
		{`\u00E9 (BMP, uppercase hex, non-ASCII)`, `{"keys":"a\u00E9b"}`, "a\u00e9b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateBody([]byte(tc.body))
			if err != nil {
				t.Fatalf("ValidateBody(%q) unexpected error: %v", tc.body, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestValidateBody_InvalidEscapeRejected covers an escape character JSON
// does not define at all (\x is not a recognized escape — only \", \\, \/,
// \b, \f, \n, \r, \t and \u are).
func TestValidateBody_InvalidEscapeRejected(t *testing.T) {
	_, err := ValidateBody([]byte(`{"keys":"a\x41b"}`))
	_ = assertInvalid(t, err)
}

// TestValidateBody_TrailingNonJSONBytesRejected covers trailing bytes that
// are not even a second JSON value (as opposed to
// TestValidateBody_TrailingContent below, which covers a second, otherwise
// well-formed, JSON value concatenated onto the body).
func TestValidateBody_TrailingNonJSONBytesRejected(t *testing.T) {
	_, err := ValidateBody([]byte(`{"keys":"a"}x`))
	_ = assertInvalid(t, err)
}

func TestValidateKeys_InvalidUTF8Rejected(t *testing.T) {
	err := ValidateKeys(string([]byte{'a', 0xff, 'b'}))
	_ = assertInvalid(t, err)
}

func TestValidateBody_TrailingContent(t *testing.T) {
	_, err := ValidateBody([]byte(`{"keys":"a"}{"keys":"b"}`))
	_ = assertInvalid(t, err)
}

// TestValidateBody_DifferentialAgainstEncodingJSON generates random valid
// Go strings (including astral-plane runes, so json.Marshal emits real
// surrogate pairs for some of them), JSON-encodes each with the standard
// library, and asserts ValidateBody decodes the resulting document back to
// exactly the same string encoding/json would produce. This is a floor
// under decodeJSONString/decodeUnicodeEscape: any future edit that changes
// their behavior on valid input, even in a case no other test in this file
// happens to name, fails here.
func TestValidateBody_DifferentialAgainstEncodingJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const iterations = 5000

	randRune := func() rune {
		switch rng.Intn(4) {
		case 0:
			return rune(rng.Intn(0x80)) // ASCII, including control chars and '"'/'\\'
		case 1:
			return rune(0x80 + rng.Intn(0x800-0x80)) // 2-byte range
		case 2:
			return rune(0x800 + rng.Intn(0x10000-0x800)) // 3-byte range (BMP, excluding surrogates handled below)
		default:
			return rune(0x10000 + rng.Intn(0x110000-0x10000)) // astral plane, requires a surrogate pair when escaped
		}
	}

	for i := 0; i < iterations; i++ {
		n := 1 + rng.Intn(12) // at least one rune: ValidateBody rejects "" by design, which is a separate, already-tested rule (TestValidateBody_Empty), not this test's concern
		var b strings.Builder
		for j := 0; j < n; j++ {
			r := randRune()
			if r == 0 || !utf8.ValidRune(r) {
				continue // NUL and surrogate code points are rejected/impossible by design, not this test's concern
			}
			b.WriteRune(r)
		}
		want := b.String()
		if want == "" {
			continue // every candidate rune this iteration was filtered out above
		}

		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("json.Marshal(%q) failed: %v", want, err)
		}
		body := []byte(`{"keys":`)
		body = append(body, encoded...)
		body = append(body, '}')

		got, err := ValidateBody(body)
		if err != nil {
			t.Fatalf("iteration %d: ValidateBody(%s) unexpected error: %v (input %q)", i, body, err, want)
		}
		if got != want {
			t.Fatalf("iteration %d: ValidateBody(%s) = %q, want %q", i, body, got, want)
		}
	}
}

func TestValidateKeys(t *testing.T) {
	if err := ValidateKeys(""); err == nil {
		t.Fatal("expected empty string to be rejected")
	}
	if err := ValidateKeys("a\x00b"); err == nil {
		t.Fatal("expected NUL byte to be rejected")
	}
	if err := ValidateKeys(strings.Repeat("a", MaxBytes+1)); err == nil {
		t.Fatal("expected oversized input to be rejected")
	} else if ve, ok := AsValidationError(err); !ok || ve.Outcome != OutcomePayloadTooLarge {
		t.Fatalf("expected OutcomePayloadTooLarge, got %v", err)
	}
	if err := ValidateKeys("   "); err != nil {
		t.Fatalf("expected whitespace-only input to be valid, got %v", err)
	}
	if err := ValidateKeys("Enter"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertInvalid(t *testing.T, err error) *ValidationError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	ve, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
	}
	if ve.Outcome != OutcomeInvalidRequest {
		t.Fatalf("Outcome = %v, want %v", ve.Outcome, OutcomeInvalidRequest)
	}
	return ve
}

// TestValidateBody_ErrorsNeverContainInputData pins the property validate.go
// claims by construction (ValidationError's doc comment: "fixed strings,
// never interpolated with client-supplied data"): every ValidateBody failure
// path is exercised with a distinctive secret placed exactly where a
// careless interpolation (an unknown field name, an echoed json.Decoder
// syntax-error message, the rejected value itself, or the "keys" content
// surrounding an invalid byte/escape) would leak it into err.Error(). Each
// case also asserts the exact expected Outcome and fixed message, not just
// "an error occurred": asserting only non-nil-and-secret-free would still
// pass if a case failed on an earlier, unrelated check than the one it
// names, silently losing coverage of the intended path. Without the message
// assertion, a later refactor that moved the intended check behind a
// different, interpolating one could pass this test by accidentally hitting
// a different fixed-string rejection first.
func TestValidateBody_ErrorsNeverContainInputData(t *testing.T) {
	const secret = "SECRET_CANARY_9f3a1b2c"

	cases := []struct {
		name    string
		body    []byte
		outcome Outcome
		message string
	}{
		{"unknown field name", []byte(`{"` + secret + `":"a"}`), OutcomeInvalidRequest, "unknown field"},
		{"non-string value", []byte(`{"keys":["` + secret + `"]}`), OutcomeInvalidRequest, "field must be a string"},
		// decodeJSONString's `default:` "invalid escape sequence" branch is
		// defensive and unreachable via ValidateBody: the stdlib scanner
		// already rejects an unrecognized escape character while capturing
		// the raw value (json.Decoder.Decode(&json.RawMessage{}) must
		// validate escape syntax to find the string's closing quote), so the
		// observed message here is the generic one — confirmed empirically,
		// not assumed.
		{"invalid escape", []byte(`{"keys":"` + secret + `\x41b"}`), OutcomeInvalidRequest, "invalid JSON"},
		{"trailing garbage", []byte(`{"keys":"` + secret + `"}garbage`), OutcomeInvalidRequest, "unexpected trailing content after request body"},
		{"NUL (escaped)", []byte(`{"keys":"` + secret + `\u0000"}`), OutcomeInvalidRequest, "keys must not contain a NUL byte"},
		{"unpaired surrogate", []byte(`{"keys":"` + secret + `\ud800"}`), OutcomeInvalidRequest, "invalid JSON: unpaired UTF-16 surrogate escape"},
		{"over-size", []byte(`{"keys":"` + secret + strings.Repeat("a", MaxBytes) + `"}`), OutcomePayloadTooLarge, "keys exceeds the byte limit"},
		// "invalid UTF-8" needs raw bytes, not a Go string literal (which
		// cannot itself contain invalid UTF-8), so it is built with append
		// rather than written as a `[]byte(...)` conversion of a literal.
		{"invalid UTF-8", append(append([]byte(`{"keys":"`+secret), 0xff), []byte(`"}`)...), OutcomeInvalidRequest, "invalid JSON: body is not valid UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateBody(tc.body)
			ve, ok := AsValidationError(err)
			if !ok {
				t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
			}
			if ve.Outcome != tc.outcome {
				t.Fatalf("Outcome = %v, want %v", ve.Outcome, tc.outcome)
			}
			if ve.Error() != tc.message {
				t.Fatalf("message = %q, want %q", ve.Error(), tc.message)
			}
			if strings.Contains(ve.Error(), secret) {
				t.Fatalf("error %q leaks the input secret", ve.Error())
			}
		})
	}
}

// TestValidateKeys_ErrorsNeverContainInputData is
// TestValidateBody_ErrorsNeverContainInputData's counterpart for
// ValidateKeys — the bridge's validator (§6.1 step 4 of the contract),
// applied directly to an already-decoded Go string rather than a raw JSON
// body. "empty" carries no secret (there is no input to leak from an empty
// string) and is included only to pin its exact Outcome/message alongside
// the others, for the same "asserts the intended path, not just an error"
// reason as above.
func TestValidateKeys_ErrorsNeverContainInputData(t *testing.T) {
	const secret = "SECRET_CANARY_9f3a1b2c"

	cases := []struct {
		name    string
		input   string
		outcome Outcome
		message string
	}{
		{"empty", "", OutcomeInvalidRequest, "keys must not be empty"},
		{"NUL", secret + "\x00", OutcomeInvalidRequest, "keys must not contain a NUL byte"},
		{"over-size", secret + strings.Repeat("a", MaxBytes), OutcomePayloadTooLarge, "keys exceeds the byte limit"},
		{"invalid UTF-8", secret + string([]byte{0xff}), OutcomeInvalidRequest, "keys must be valid UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKeys(tc.input)
			ve, ok := AsValidationError(err)
			if !ok {
				t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
			}
			if ve.Outcome != tc.outcome {
				t.Fatalf("Outcome = %v, want %v", ve.Outcome, tc.outcome)
			}
			if ve.Error() != tc.message {
				t.Fatalf("message = %q, want %q", ve.Error(), tc.message)
			}
			if strings.Contains(ve.Error(), secret) {
				t.Fatalf("error %q leaks the input secret", ve.Error())
			}
		})
	}
}

// TestValidateKeysJSON_ErrorsNeverContainInputData is
// TestValidateBody_ErrorsNeverContainInputData's counterpart for
// ValidateKeysJSON — the bridge's re-extraction validator (§6.1 "Keys-content
// parity"), applied to a json.RawMessage captured by a second Decoder.Decode
// of an already-buffered legacy request body.
func TestValidateKeysJSON_ErrorsNeverContainInputData(t *testing.T) {
	const secret = "SECRET_CANARY_9f3a1b2c"

	cases := []struct {
		name    string
		raw     json.RawMessage
		outcome Outcome
		message string
	}{
		{"non-string value", json.RawMessage(`["` + secret + `"]`), OutcomeInvalidRequest, "field must be a string"},
		// json.Valid rejects an unrecognized escape character before
		// decodeJSONString's own `default:` branch would ever see it, so
		// this produces the same generic message ValidateBody produces for
		// the equivalent input, not decodeJSONString's more specific one.
		{"invalid escape", json.RawMessage(`"` + secret + `\x41b"`), OutcomeInvalidRequest, "invalid JSON"},
		{"NUL (escaped)", json.RawMessage(`"` + secret + `\u0000"`), OutcomeInvalidRequest, "keys must not contain a NUL byte"},
		{"unpaired surrogate", json.RawMessage(`"` + secret + `\ud800"`), OutcomeInvalidRequest, "invalid JSON: unpaired UTF-16 surrogate escape"},
		{"over-size", json.RawMessage(`"` + secret + strings.Repeat("a", MaxBytes) + `"`), OutcomePayloadTooLarge, "keys exceeds the byte limit"},
		{"invalid UTF-8", append(append(json.RawMessage(`"`+secret), 0xff), []byte(`"`)...), OutcomeInvalidRequest, "invalid JSON: body is not valid UTF-8"},
		// Not valid JSON at all (an unescaped control byte, an unescaped
		// interior quote) — json.Valid must catch both before
		// decodeJSONString, which checks only quotes and escapes, would
		// otherwise silently accept them.
		{"unescaped control byte", append(append(json.RawMessage(`"`+secret), '\n'), []byte(`b"`)...), OutcomeInvalidRequest, "invalid JSON"},
		{"unescaped interior quote", json.RawMessage(`"` + secret + `"b"`), OutcomeInvalidRequest, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateKeysJSON(tc.raw)
			ve, ok := AsValidationError(err)
			if !ok {
				t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
			}
			if ve.Outcome != tc.outcome {
				t.Fatalf("Outcome = %v, want %v", ve.Outcome, tc.outcome)
			}
			if ve.Error() != tc.message {
				t.Fatalf("message = %q, want %q", ve.Error(), tc.message)
			}
			if strings.Contains(ve.Error(), secret) {
				t.Fatalf("error %q leaks the input secret", ve.Error())
			}
		})
	}
}

// TestValidateKeysJSON_Valid pins the accept path: a well-formed JSON string
// decodes to exactly the same value ValidateBody produces for the
// equivalent `{"keys":...}` body — compared directly against ValidateBody's
// own output for each case, not just against a hand-picked `want` string —
// including a real, escaped (non-literal-UTF-8) surrogate pair, so
// ValidateKeysJSON is provably not a looser sibling of ValidateBody.
func TestValidateKeysJSON_Valid(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{"plain ASCII", json.RawMessage(`"C-c"`)},
		{"escaped surrogate pair", json.RawMessage("\"\\uD83D\\uDE00\"")},
		{"literal UTF-8", json.RawMessage(`"héllo"`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateKeysJSON(tc.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			wantBody := append(append([]byte(`{"keys":`), tc.raw...), '}')
			want, wantErr := ValidateBody(wantBody)
			if wantErr != nil {
				t.Fatalf("ValidateBody(%s) unexpected error: %v", wantBody, wantErr)
			}
			if got != want {
				t.Fatalf("got %q, want %q (ValidateBody's value for the equivalent body)", got, want)
			}
		})
	}
}
