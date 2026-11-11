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
	"bytes"
	"encoding/json"
	"strings"
)

// Raw keystroke delivery through message requests has been removed
// (.design/agent-keys-contract.md §6, AK-35/AK-36). StructuredMessage no
// longer has a Raw field, so encoding/json would silently drop a "raw" member
// and turn an old raw request into an ordinary message. Every message ingress
// that used to accept raw therefore probes the request body with
// HasRetiredRawField before decoding it and rejects a match with
// RawInputRemovedCode. This is a rejection adapter only: nothing here
// delivers keystrokes.

// RawInputRemovedCode is the machine-readable error code for a message
// request that still carries the retired raw field. It matches
// agentkeys.OutcomeRawInputRemoved.
const RawInputRemovedCode = "raw_input_removed"

// RawInputRemovedMessage is the fixed, content-free guidance returned with
// RawInputRemovedCode. It names the working replacement.
const RawInputRemovedMessage = "raw keystroke delivery through message requests has been removed; " +
	"send keystrokes with 'scion keys <agent> <keys>' or POST /api/v1/agents/{id}/keys"

// rawFieldName is the retired member name. Matching uses strings.EqualFold,
// the same case-insensitive rule encoding/json applies to struct field
// names, so "RAW" and "Raw" are caught exactly as they used to be accepted.
const rawFieldName = "raw"

// HasRetiredRawField reports whether the leading JSON value in body is an
// object that carries a "raw" member at the top level, or inside an
// object-valued member whose name matches one of nested (for example
// "structured_message" or "message").
//
// Any value counts: true, false, null, a string, a number, an array, an
// object, or a value that is not valid JSON at all. The member name is read
// before its value, so a body such as {"raw":tru} is still reported. Every
// occurrence of a duplicated member is inspected, unlike a map or struct
// decode that keeps only one of them.
//
// Only the leading JSON value is inspected, matching readJSON's
// json.Decoder.Decode semantics, so trailing bytes neither hide nor create
// a match. A body that is not an object, or that is malformed before any
// raw member appears, is reported as false; the caller's normal decode then
// rejects it as an invalid request.
func HasRetiredRawField(body []byte, nested ...string) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return false
	}
	return scanMembersForRaw(dec, nested)
}

// scanMembersForRaw walks the members of an object whose opening brace has
// already been consumed. It returns true as soon as a raw member name is
// read. Members named in nested are descended into one level when their
// value is an object; nested is not passed down, so only the documented
// locations are inspected.
func scanMembersForRaw(dec *json.Decoder, nested []string) bool {
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		key, ok := tok.(string)
		if !ok {
			return false
		}
		if strings.EqualFold(key, rawFieldName) {
			return true
		}
		if !matchesAnyFold(key, nested) {
			if !skipJSONValue(dec) {
				return false
			}
			continue
		}
		vt, err := dec.Token()
		if err != nil {
			return false
		}
		switch vt {
		case json.Delim('{'):
			if scanMembersForRaw(dec, nil) {
				return true
			}
			if _, err := dec.Token(); err != nil { // closing '}'
				return false
			}
		case json.Delim('['):
			for dec.More() {
				if !skipJSONValue(dec) {
					return false
				}
			}
			if _, err := dec.Token(); err != nil { // closing ']'
				return false
			}
		}
		// Scalars were fully consumed by the Token call above.
	}
	return false
}

// skipJSONValue consumes the next JSON value from dec and reports whether it
// was well formed. It decodes into discardJSON rather than json.RawMessage:
// both go through the same Decoder.Decode path, so validation, truncation
// and error behaviour are unchanged, but discardJSON keeps no copy of the
// value. A token-by-token skip is deliberately not used here: Token decodes
// every scalar into an interface value, which rejects numbers outside the
// float64 range (so {"a":1e400,"raw":true} would stop being reported) and
// allocates per element.
func skipJSONValue(dec *json.Decoder) bool {
	var skip discardJSON
	return dec.Decode(&skip) == nil
}

// discardJSON is a json.Unmarshaler that ignores its input. The decoder has
// already validated the value before UnmarshalJSON is called.
type discardJSON struct{}

func (*discardJSON) UnmarshalJSON([]byte) error { return nil }

func matchesAnyFold(key string, names []string) bool {
	for _, n := range names {
		if strings.EqualFold(key, n) {
			return true
		}
	}
	return false
}
