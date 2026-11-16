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

package triggertemplate

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// MapVars is a simple map-backed Vars implementation for fixed variables,
// event attributes, and unit tests.
type MapVars map[string]string

// Lookup returns m[path] when present.
func (m MapVars) Lookup(path string) (string, bool, error) {
	if m == nil {
		return "", false, nil
	}
	v, ok := m[path]
	return v, ok, nil
}

type chainVars struct {
	sources []Vars
}

// ChainVars combines multiple Vars sources in order, returning the first error
// or found value encountered.
func ChainVars(sources ...Vars) Vars {
	return chainVars{sources: sources}
}

func (c chainVars) Lookup(path string) (string, bool, error) {
	for _, src := range c.sources {
		if src == nil {
			continue
		}
		val, found, err := src.Lookup(path)
		if err != nil || found {
			return val, found, err
		}
	}
	return "", false, nil
}

// PayloadVars serves event.payload.<path> lookups over a parsed JSON document.
// Strings are returned verbatim, numbers and booleans as JSON text, and null,
// objects, and arrays are rejected with *PayloadValueError (§5.2).
type PayloadVars struct {
	doc any
}

// NewPayloadVars parses rawJSON once using json.Decoder.UseNumber (preserving
// exact numeric text without float64 precision loss) and returns a PayloadVars.
// Errors never echo rawJSON.
func NewPayloadVars(rawJSON []byte) (*PayloadVars, error) {
	dec := json.NewDecoder(bytes.NewReader(rawJSON))
	dec.UseNumber()

	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, &PayloadValueError{Reason: "invalid JSON payload"}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, &PayloadValueError{Reason: "invalid JSON payload: trailing data"}
	}
	return &PayloadVars{doc: doc}, nil
}

// NewPayloadVarsFromValue wraps an already-decoded JSON document (for example
// decoded with json.Decoder.UseNumber).
func NewPayloadVarsFromValue(doc any) *PayloadVars {
	return &PayloadVars{doc: doc}
}

// Lookup resolves event.payload.<path> against p's JSON document. Non-payload
// paths return ("", false, nil) so PayloadVars composes cleanly in ChainVars.
func (p *PayloadVars) Lookup(path string) (string, bool, error) {
	if p == nil {
		return "", false, nil
	}
	return LookupPayload(p.doc, path)
}

// LookupPayload resolves an event.payload.<path> expression over doc.
func LookupPayload(doc any, path string) (string, bool, error) {
	if !strings.HasPrefix(path, "event.payload.") && !strings.HasPrefix(path, "event.payload[") {
		return "", false, nil
	}
	steps, err := parsePayloadPath(path)
	if err != nil {
		return "", false, &PayloadValueError{Reason: "invalid payload path"}
	}

	cur := doc
	for _, step := range steps {
		if cur == nil {
			return "", false, nil
		}
		if step.isIndex {
			arr, ok := cur.([]any)
			if !ok {
				return "", false, nil
			}
			if step.index < 0 || step.index >= len(arr) {
				return "", false, nil
			}
			cur = arr[step.index]
		} else {
			obj, ok := cur.(map[string]any)
			if !ok {
				return "", false, nil
			}
			next, exists := obj[step.key]
			if !exists {
				return "", false, nil
			}
			cur = next
		}
	}

	switch v := cur.(type) {
	case nil:
		return "", true, &PayloadValueError{Reason: "payload value is null"}
	case string:
		return v, true, nil
	case json.Number:
		return v.String(), true, nil
	case bool:
		return strconv.FormatBool(v), true, nil
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), true, nil
	case []any:
		return "", true, &PayloadValueError{Reason: "payload value is an array"}
	case map[string]any:
		return "", true, &PayloadValueError{Reason: "payload value is an object"}
	default:
		return "", true, &PayloadValueError{Reason: "payload value is not a JSON scalar"}
	}
}
