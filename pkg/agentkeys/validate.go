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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ValidationError reports why a request body failed ValidateBody or
// ValidateKeys, carrying the Outcome the caller should surface (always
// OutcomeInvalidRequest or OutcomePayloadTooLarge for these functions).
//
// Error messages are fixed strings, never interpolated with client-supplied
// data (field names, raw JSON syntax-error text, or anything derived from
// the "keys" value itself): the contract forbids request JSON and key
// content from appearing in errors or logs (see the contract's "Concrete
// defaults" audit rules), and a validation error is exactly the kind of
// string that ends up in a log line.
type ValidationError struct {
	Outcome Outcome
	msg     string
}

func (e *ValidationError) Error() string { return e.msg }

func invalid(msg string) error {
	return &ValidationError{Outcome: OutcomeInvalidRequest, msg: msg}
}

func tooLarge(msg string) error {
	return &ValidationError{Outcome: OutcomePayloadTooLarge, msg: msg}
}

// ValidateBody parses and validates a raw /keys request body against the
// frozen contract (.design/agent-keys-contract.md §2.2, "Public request and
// result"):
//
//   - the body must be valid UTF-8
//   - exactly one field, "keys"
//   - the value must be a JSON string (not an array, object, number, bool or
//     null)
//   - unknown top-level fields are rejected
//   - a duplicate "keys" key is rejected (Go's encoding/json would otherwise
//     silently keep the last occurrence; this function reads the raw token
//     stream so it can refuse to be that permissive)
//   - the string must be non-empty
//   - the string must not contain a NUL byte
//   - the string must not exceed MaxBytes UTF-8 bytes
//   - the string must not contain an unpaired UTF-16 surrogate escape
//     (encoding/json would otherwise silently replace it with U+FFFD)
//
// Whitespace-only input is valid: it is returned exactly as received. This
// function never trims, splits, tokenizes or otherwise normalizes the
// string, and — after the checks above — never substitutes any byte the
// request did not contain. That second guarantee is why this function
// avoids plain `encoding/json` string decoding for the value: Token() and
// Unmarshal both silently repair invalid UTF-8 and lone surrogate escapes
// into U+FFFD, which would violate "preserve UTF-8 verbatim" for exactly
// the inputs where it matters most to get right.
//
// ValidateBody is the only decoder any caller may use for a /keys request
// body. Decoding into Request with plain `encoding/json` instead would
// accept case-variant field names and duplicate keys that this function
// correctly rejects, and would not gate the checks above at all.
//
// It does not enforce MaxHTTPBodyBytes; callers should apply that limit to
// the transport-level read (e.g. http.MaxBytesReader) before calling this
// function, returning OutcomePayloadTooLarge (413) directly when the read
// itself is cut off, so that a body clearly larger than any valid request
// is rejected without being parsed.
func ValidateBody(body []byte) (string, error) {
	if !utf8.Valid(body) {
		return "", invalid("invalid JSON: body is not valid UTF-8")
	}

	dec := json.NewDecoder(bytes.NewReader(body))

	tok, err := dec.Token()
	if err != nil {
		return "", invalid("invalid JSON")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", invalid("request body must be a JSON object")
	}

	var keys string
	seenKeys := false

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", invalid("invalid JSON")
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", invalid("invalid JSON: expected a field name")
		}
		if key != "keys" {
			return "", invalid("unknown field")
		}
		if seenKeys {
			return "", invalid("duplicate field")
		}
		seenKeys = true

		// Decode the value as a json.RawMessage rather than through
		// dec.Token(): RawMessage captures the exact input bytes without
		// interpreting escapes, which lets decodeJSONString below apply its
		// own, stricter, rules for what "preserve verbatim" means. Decode
		// and Token may be freely interleaved on the same *json.Decoder.
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", invalid("invalid JSON")
		}
		if len(raw) == 0 || raw[0] != '"' {
			return "", invalid("field must be a string")
		}
		s, err := decodeJSONString(raw)
		if err != nil {
			return "", err
		}
		keys = s
	}

	// Consume and check the closing '}'.
	closeTok, err := dec.Token()
	if err != nil {
		return "", invalid("invalid JSON")
	}
	if d, ok := closeTok.(json.Delim); !ok || d != '}' {
		return "", invalid("request body must be a single JSON object")
	}

	// Reject trailing content after the object close, e.g. a second
	// top-level JSON value concatenated onto the body.
	if _, err := dec.Token(); err != io.EOF {
		return "", invalid("unexpected trailing content after request body")
	}

	if !seenKeys {
		return "", invalid("missing required field \"keys\"")
	}
	if err := validateDecodedKeys(keys); err != nil {
		return "", err
	}

	return keys, nil
}

// validateDecodedKeys applies the field-level rules (non-empty, no NUL
// byte, size ceiling) shared by ValidateBody and ValidateKeys to an
// already-decoded string known to be valid UTF-8.
func validateDecodedKeys(keys string) error {
	if keys == "" {
		return invalid("keys must not be empty")
	}
	if strings.IndexByte(keys, 0) >= 0 {
		return invalid("keys must not contain a NUL byte")
	}
	if len(keys) > MaxBytes {
		return tooLarge("keys exceeds the byte limit")
	}
	return nil
}

// ValidateKeys applies ValidateBody's field-level rules (non-empty, valid
// UTF-8, no NUL byte, size ceiling) directly to an already-decoded string,
// for callers that assemble a Request from a source other than a raw HTTP
// body — e.g. a local-mode CLI path or the runtime broker's keys handler.
// It never trims, splits or otherwise normalizes s.
//
// Unlike ValidateBody, this function cannot detect a lone surrogate escape
// that some upstream `encoding/json` decode already silently replaced with
// U+FFFD before s reached here — there is no raw JSON left to inspect. It
// can and does still reject s if it is not valid UTF-8 at all (e.g. built
// programmatically rather than JSON-decoded). A caller that still has the
// raw JSON request body should use ValidateBody instead, which does not have
// this gap.
func ValidateKeys(s string) error {
	if !utf8.ValidString(s) {
		return invalid("keys must be valid UTF-8")
	}
	return validateDecodedKeys(s)
}

// AsValidationError extracts the Outcome from err if it (or something it
// wraps) is a *ValidationError, returning false otherwise.
func AsValidationError(err error) (*ValidationError, bool) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}

// decodeJSONString decodes raw — a JSON string literal exactly as it
// appears in the request body, including the surrounding quotes — into a Go
// string. raw is assumed already proven valid UTF-8 by the caller.
//
// It rejects two things encoding/json's own string decoding would silently
// repair into U+FFFD instead: an unpaired UTF-16 surrogate escape (a lone
// \uD800-\uDFFF not part of a valid high/low surrogate pair), and a
// malformed \u escape. Every other escape (\", \\, \/, \b, \f, \n, \r, \t,
// and a valid \uXXXX or surrogate pair) decodes exactly as
// RFC 8259 §7 specifies.
func decodeJSONString(raw []byte) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", invalid("invalid JSON: malformed string")
	}
	body := raw[1 : len(raw)-1]

	var buf bytes.Buffer
	buf.Grow(len(body))
	for i := 0; i < len(body); {
		c := body[i]
		if c != '\\' {
			buf.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(body) {
			return "", invalid("invalid JSON: truncated escape sequence")
		}
		switch body[i+1] {
		case '"':
			buf.WriteByte('"')
			i += 2
		case '\\':
			buf.WriteByte('\\')
			i += 2
		case '/':
			buf.WriteByte('/')
			i += 2
		case 'b':
			buf.WriteByte('\b')
			i += 2
		case 'f':
			buf.WriteByte('\f')
			i += 2
		case 'n':
			buf.WriteByte('\n')
			i += 2
		case 'r':
			buf.WriteByte('\r')
			i += 2
		case 't':
			buf.WriteByte('\t')
			i += 2
		case 'u':
			r, consumed, err := decodeUnicodeEscape(body[i:])
			if err != nil {
				return "", err
			}
			buf.WriteRune(r)
			i += consumed
		default:
			return "", invalid("invalid JSON: invalid escape sequence")
		}
	}
	return buf.String(), nil
}

// decodeUnicodeEscape decodes a \uXXXX escape (and, if it is a UTF-16 high
// surrogate, the \uYYYY low surrogate that must immediately follow it) at
// the start of s. It returns the decoded rune and the number of input bytes
// consumed (6 for a standalone escape, 12 for a surrogate pair).
//
// A high surrogate not immediately followed by a valid low surrogate, or a
// bare low surrogate, is rejected rather than replaced with
// unicode.ReplacementChar — this is the behavior that differs from
// encoding/json and is the entire point of this function.
func decodeUnicodeEscape(s []byte) (rune, int, error) {
	if len(s) < 6 {
		return 0, 0, invalid("invalid JSON: truncated unicode escape")
	}
	v, err := strconv.ParseUint(string(s[2:6]), 16, 32)
	if err != nil {
		return 0, 0, invalid("invalid JSON: invalid unicode escape")
	}
	r := rune(v)

	if !utf16.IsSurrogate(r) {
		return r, 6, nil
	}

	// r is a surrogate half. It is only valid as the first element of a
	// \uXXXX\uYYYY pair.
	if len(s) >= 12 && s[6] == '\\' && s[7] == 'u' {
		v2, err2 := strconv.ParseUint(string(s[8:12]), 16, 32)
		if err2 == nil {
			combined := utf16.DecodeRune(r, rune(v2))
			if combined != unicode.ReplacementChar {
				return combined, 12, nil
			}
		}
	}
	return 0, 0, invalid("invalid JSON: unpaired UTF-16 surrogate escape")
}
