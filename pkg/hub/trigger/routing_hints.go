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

package trigger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Security Surface S20 — Routing hints (Stage 2 §7.2, §18 S20, P5 I-4):
//
// Invariant: Routing hints lift event.type, event.subject, and dedup_key from
// a verified webhook body using RFC 6901 JSON pointers without:
//  1. overriding scion_v1 signed headers (or already-populated provider headers),
//  2. accepting pointers > 128 bytes or malformed RFC 6901 escapes,
//  3. accepting non-string/non-number or out-of-bounds/non-conforming values
//     (no lower-casing; invalid values are ignored and recorded in
//     attributes.routing_hint_error without echoing untrusted values), or
//  4. parsing beyond MaxPayloadBytes (64 KiB) on overflow bodies (P5 I-4).
const (
	// MaxRoutingHintPathBytes caps each RFC 6901 JSON pointer in routing_hints.
	MaxRoutingHintPathBytes = 128

	maxRoutingHintPointerDepth = 32
	maxRoutingHintScanDepth    = 64

	// Default routing hint pointers for stripe_v1 endpoints (Stage 2 §7.2).
	StripeDefaultTypePath    = "/type"
	StripeDefaultDedupPath   = "/id"
	StripeDefaultSubjectPath = "/data/object/id"
)

// ParseJSONPointer parses and validates a non-empty RFC 6901 JSON pointer
// bounded to MaxRoutingHintPathBytes (128 bytes) and printable UTF-8.
func ParseJSONPointer(ptr string) ([]string, error) {
	if ptr == "" {
		return nil, errors.New("JSON pointer must not be empty")
	}
	if len(ptr) > MaxRoutingHintPathBytes {
		return nil, fmt.Errorf("JSON pointer exceeds maximum length of %d bytes", MaxRoutingHintPathBytes)
	}
	if !isPrintableString(ptr) {
		return nil, errors.New("JSON pointer must contain only printable UTF-8 characters")
	}
	if ptr[0] != '/' {
		return nil, errors.New("JSON pointer must start with '/'")
	}
	rawParts := strings.Split(ptr[1:], "/")
	if len(rawParts) > maxRoutingHintPointerDepth {
		return nil, fmt.Errorf("JSON pointer exceeds maximum depth of %d segments", maxRoutingHintPointerDepth)
	}
	parts := make([]string, len(rawParts))
	for i, seg := range rawParts {
		unescaped, err := unescapeJSONPointerSegment(seg)
		if err != nil {
			return nil, err
		}
		parts[i] = unescaped
	}
	return parts, nil
}

func unescapeJSONPointerSegment(seg string) (string, error) {
	if !strings.ContainsRune(seg, '~') {
		return seg, nil
	}
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		if seg[i] != '~' {
			b.WriteByte(seg[i])
			continue
		}
		if i+1 >= len(seg) {
			return "", errors.New("invalid RFC 6901 escape: trailing '~'")
		}
		i++
		switch seg[i] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", fmt.Errorf("invalid RFC 6901 escape '~%c' (expected ~0 or ~1)", seg[i])
		}
	}
	return b.String(), nil
}

// ValidateRoutingHints validates all non-empty JSON pointers in hints.
func ValidateRoutingHints(hints *store.WebhookRoutingHints) error {
	if hints == nil || hints.IsEmpty() {
		return nil
	}
	if hints.TypePath != "" {
		if _, err := ParseJSONPointer(hints.TypePath); err != nil {
			return fmt.Errorf("routingHints.typePath: %w", err)
		}
	}
	if hints.SubjectPath != "" {
		if _, err := ParseJSONPointer(hints.SubjectPath); err != nil {
			return fmt.Errorf("routingHints.subjectPath: %w", err)
		}
	}
	if hints.DedupPath != "" {
		if _, err := ParseJSONPointer(hints.DedupPath); err != nil {
			return fmt.Errorf("routingHints.dedupPath: %w", err)
		}
	}
	return nil
}

// EffectiveRoutingHints returns the active routing hints for an endpoint,
// applying stripe_v1 defaults (/type, /id, /data/object/id) for any unset
// paths on stripe_v1 endpoints (Stage 2 §7.2).
func EffectiveRoutingHints(sigMode string, configured *store.WebhookRoutingHints) *store.WebhookRoutingHints {
	if sigMode != store.WebhookSignatureModeStripeV1 {
		if configured == nil || configured.IsEmpty() {
			return nil
		}
		cp := *configured
		return &cp
	}
	eff := &store.WebhookRoutingHints{
		TypePath:    StripeDefaultTypePath,
		DedupPath:   StripeDefaultDedupPath,
		SubjectPath: StripeDefaultSubjectPath,
	}
	if configured != nil {
		if configured.TypePath != "" {
			eff.TypePath = configured.TypePath
		}
		if configured.DedupPath != "" {
			eff.DedupPath = configured.DedupPath
		}
		if configured.SubjectPath != "" {
			eff.SubjectPath = configured.SubjectPath
		}
	}
	return eff
}

type hintTarget struct {
	field       string
	segments    []string
	found       bool
	value       string
	invalidType bool
}

// ResolveRoutingHints resolves configured routing hints against the first
// MaxPayloadBytes (64 KiB) of body using a bounded streaming JSON decoder
// (never parsing beyond 64 KiB on overflow bodies, P5 I-4 / S20).
//
// Existing non-empty values in current (including scion_v1 signed headers,
// hub_token headers, and X-GitHub-Event / X-GitHub-Delivery) always take
// precedence and are never overwritten. Non-conforming values at matched paths
// are ignored and recorded in hintErr (content-free, never echoing values).
func ResolveRoutingHints(body []byte, hints *store.WebhookRoutingHints, current VerifiedHeaders) (VerifiedHeaders, string) {
	out := current
	if hints == nil || hints.IsEmpty() || len(body) == 0 {
		return out, ""
	}

	var targets []*hintTarget
	var errs []string
	addTarget := func(field, ptr string, alreadySet bool) {
		if alreadySet || ptr == "" {
			return
		}
		segs, err := ParseJSONPointer(ptr)
		if err != nil {
			errs = append(errs, field+": invalid pointer")
			return
		}
		targets = append(targets, &hintTarget{field: field, segments: segs})
	}

	addTarget("typePath", hints.TypePath, current.Event != "" || current.SignedEvent)
	addTarget("subjectPath", hints.SubjectPath, current.Subject != "" || current.SignedSubject)
	addTarget("dedupPath", hints.DedupPath, current.Delivery != "" || current.SignedDelivery)

	if len(targets) > 0 {
		// S20 / P5 I-4: strictly cap the slice passed to the streaming decoder
		// at MaxPayloadBytes (64 KiB) so overflow bodies are never fully parsed.
		prefix := body
		if len(prefix) > MaxPayloadBytes {
			prefix = prefix[:MaxPayloadBytes]
		}
		dec := json.NewDecoder(bytes.NewReader(prefix))
		dec.UseNumber()
		_ = scanJSONForHints(dec, targets, nil, 0)
	}

	for _, t := range targets {
		if !t.found {
			continue
		}
		if t.invalidType {
			errs = append(errs, t.field+": value must be a string or number")
			continue
		}
		switch t.field {
		case "typePath":
			if t.value == "" || len(t.value) > MaxEventBytes || !eventNamePattern.MatchString(t.value) {
				errs = append(errs, "typePath: value does not match event name bounds")
				continue
			}
			out.Event = t.value
		case "subjectPath":
			if t.value == "" || len(t.value) > MaxSubjectBytes || !isPrintableString(t.value) {
				errs = append(errs, "subjectPath: value does not match subject bounds")
				continue
			}
			out.Subject = t.value
		case "dedupPath":
			if t.value == "" || len(t.value) > MaxDeliveryBytes || !isPrintableString(t.value) {
				errs = append(errs, "dedupPath: value does not match delivery bounds")
				continue
			}
			out.Delivery = t.value
		}
	}

	return out, strings.Join(errs, "; ")
}

func scanJSONForHints(dec *json.Decoder, targets []*hintTarget, path []string, depth int) error {
	if depth > maxRoutingHintScanDepth {
		return skipJSONValue(dec)
	}
	if allHintsResolved(targets) {
		return nil
	}

	tok, err := dec.Token()
	if err != nil {
		return err
	}

	// Check if any unresolved target matches this exact path.
	var exactMatches []*hintTarget
	hasDescendant := false
	for _, t := range targets {
		if t.found {
			continue
		}
		if pathEqual(t.segments, path) {
			exactMatches = append(exactMatches, t)
		} else if pathHasPrefix(t.segments, path) {
			hasDescendant = true
		}
	}

	if len(exactMatches) > 0 {
		switch v := tok.(type) {
		case string:
			for _, t := range exactMatches {
				t.found = true
				t.value = v
			}
			return nil
		case json.Number:
			s := v.String()
			for _, t := range exactMatches {
				t.found = true
				t.value = s
			}
			return nil
		case json.Delim:
			for _, t := range exactMatches {
				t.found = true
				t.invalidType = true
			}
			// If another target points deeper inside this container, continue
			// descending; otherwise skip the container cleanly.
			if !hasDescendant {
				return skipJSONContainer(dec, v)
			}
		default:
			// bool or nil
			for _, t := range exactMatches {
				t.found = true
				t.invalidType = true
			}
			return nil
		}
	}

	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return nil
	}
	if !hasDescendant {
		return skipJSONContainer(dec, delim)
	}

	switch delim {
	case '{':
		for dec.More() {
			if allHintsResolved(targets) {
				return nil
			}
			keyTok, kErr := dec.Token()
			if kErr != nil {
				return kErr
			}
			key, ok := keyTok.(string)
			if !ok {
				return errors.New("expected JSON object key")
			}
			childPath := append(path, key)
			if anyTargetNeedsPath(targets, childPath) {
				if err := scanJSONForHints(dec, targets, childPath, depth+1); err != nil {
					return err
				}
			} else {
				if err := skipJSONValue(dec); err != nil {
					return err
				}
			}
		}
		// Consume closing '}'.
		_, err := dec.Token()
		return err

	case '[':
		idx := 0
		for dec.More() {
			if allHintsResolved(targets) {
				return nil
			}
			childPath := append(path, strconv.Itoa(idx))
			idx++
			if anyTargetNeedsPath(targets, childPath) {
				if err := scanJSONForHints(dec, targets, childPath, depth+1); err != nil {
					return err
				}
			} else {
				if err := skipJSONValue(dec); err != nil {
					return err
				}
			}
		}
		// Consume closing ']'.
		_, err := dec.Token()
		return err
	}
	return nil
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); ok {
		return skipJSONContainer(dec, delim)
	}
	return nil
}

func skipJSONContainer(dec *json.Decoder, open json.Delim) error {
	if open != '{' && open != '[' {
		return nil
	}
	nesting := 1
	for nesting > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				nesting++
			case '}', ']':
				nesting--
			}
		}
	}
	return nil
}

func allHintsResolved(targets []*hintTarget) bool {
	for _, t := range targets {
		if !t.found {
			return false
		}
	}
	return true
}

func pathEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func pathHasPrefix(full, prefix []string) bool {
	if len(full) <= len(prefix) {
		return false
	}
	for i := range prefix {
		if full[i] != prefix[i] {
			return false
		}
	}
	return true
}

func anyTargetNeedsPath(targets []*hintTarget, path []string) bool {
	for _, t := range targets {
		if t.found {
			continue
		}
		if pathEqual(t.segments, path) || pathHasPrefix(t.segments, path) {
			return true
		}
	}
	return false
}
