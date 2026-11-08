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

// Choices: an agent asks a user to decide between a few options. A choice
// prompt (TypeChoicePrompt) carries one or more decisions; each decision
// has a per-agent key, a markdown question and 2 to 10 choices. This file
// holds the agent-facing input schema (DecisionInput), the canonical stored
// form (Decision), the normalizer and validator (NormalizeDecisions), and
// the markdown fallback the hub renders into the prompt's msg
// (RenderChoiceFallback) so surfaces without cards still show the
// decisions. The hub is the authority on validation; the CLI may call
// NormalizeDecisions only for early errors.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/util/plaintext"
)

// Limits of a choice prompt.
const (
	// MaxDecisionsPerMessage is the most decisions one choice prompt carries.
	MaxDecisionsPerMessage = 10
	// MinChoicesPerDecision and MaxChoicesPerDecision bound a decision's
	// choices.
	MinChoicesPerDecision = 2
	MaxChoicesPerDecision = 10
	// MaxDecisionQuestionLen is the most characters (runes) in a question.
	MaxDecisionQuestionLen = 2000
	// MaxChoiceLabelLen is the most characters (runes) in a choice label.
	MaxChoiceLabelLen = 100
	// MaxChoiceDescriptionLen is the most characters (runes) in a choice
	// description.
	MaxChoiceDescriptionLen = 300
	// MaxDecisionKeyLen is the most characters in a decision key or a
	// choice id.
	MaxDecisionKeyLen = 32
	// MaxChoicesPayloadBytes caps the JSON encoding of the normalized
	// decisions of one message.
	MaxChoicesPayloadBytes = 32 * 1024
)

// Error codes of a ChoiceError. Handlers map every code to a 400 naming
// the field.
const (
	ChoiceErrRequired      = "required"
	ChoiceErrInvalid       = "invalid"
	ChoiceErrTooLong       = "too_long"
	ChoiceErrTooFew        = "too_few"
	ChoiceErrTooMany       = "too_many"
	ChoiceErrDuplicate     = "duplicate"
	ChoiceErrUnknownField  = "unknown_field"
	ChoiceErrReservedField = "reserved_field"
	ChoiceErrNoMatch       = "no_match"
	ChoiceErrTooLarge      = "too_large"
)

// identRe is the shape of an agent-supplied decision key and of a choice id.
var identRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// hubKeyRe is the shape of a hub-assigned decision key: a positive integer
// without leading zeros.
var hubKeyRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// reservedDecisionFields are names a later schema may use. They are
// rejected with their own code so agents can tell them from typos.
var reservedDecisionFields = map[string]bool{
	"multiple":    true,
	"allow_other": true,
	"expires_in":  true,
}

// ChoiceInput is one choice as an agent writes it: either a JSON string
// (the label) or an object {id?, label, description?}.
type ChoiceInput struct {
	ID          string
	Label       string
	Description string

	// extraFields are the unrecognized object keys, in input order,
	// reported by NormalizeDecisions.
	extraFields []string
}

// UnmarshalJSON accepts a string (the label) or an object. Unknown object
// keys do not fail decoding: they are kept and reported, with the field
// path, by NormalizeDecisions.
func (c *ChoiceInput) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var label string
		if err := json.Unmarshal(data, &label); err != nil {
			return err
		}
		*c = ChoiceInput{Label: label}
		return nil
	}
	if len(data) == 0 || data[0] != '{' {
		return fmt.Errorf("a choice must be a string or an object")
	}
	fields, order, err := decodeObject(data)
	if err != nil {
		return err
	}
	out := ChoiceInput{}
	for _, name := range order {
		raw := fields[name]
		var dst *string
		switch name {
		case "id":
			dst = &out.ID
		case "label":
			dst = &out.Label
		case "description":
			dst = &out.Description
		default:
			out.extraFields = append(out.extraFields, name)
			continue
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("choice %s must be a string", name)
		}
	}
	*c = out
	return nil
}

// MarshalJSON writes the object form.
func (c ChoiceInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID          string `json:"id,omitempty"`
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
	}{c.ID, c.Label, c.Description})
}

// DecisionInput is one decision as an agent writes it in --choices.
type DecisionInput struct {
	// ID is the optional decision key. Empty means the hub assigns the
	// next number from the agent's counter.
	ID       string
	Question string
	Choices  []ChoiceInput
	// Default names the recommended choice by id or label.
	Default string

	extraFields []string
}

// UnmarshalJSON decodes the object form. Unknown and reserved keys do not
// fail decoding: they are kept and reported, with the field path, by
// NormalizeDecisions.
func (d *DecisionInput) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return fmt.Errorf("a decision must be an object")
	}
	fields, order, err := decodeObject(data)
	if err != nil {
		return err
	}
	out := DecisionInput{}
	for _, name := range order {
		raw := fields[name]
		switch name {
		case "id", "question", "default":
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("decision %s must be a string", name)
			}
			switch name {
			case "id":
				out.ID = s
			case "question":
				out.Question = s
			default:
				out.Default = s
			}
		case "choices":
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			if err := json.Unmarshal(raw, &out.Choices); err != nil {
				return fmt.Errorf("decision choices: %w", err)
			}
		default:
			out.extraFields = append(out.extraFields, name)
		}
	}
	*d = out
	return nil
}

// MarshalJSON writes the object form with every choice as an object.
func (d DecisionInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID       string        `json:"id,omitempty"`
		Question string        `json:"question"`
		Choices  []ChoiceInput `json:"choices"`
		Default  string        `json:"default,omitempty"`
	}{d.ID, d.Question, d.Choices, d.Default})
}

// decodeObject decodes a JSON object into its raw members, keeping the key
// order so errors are reported in input order. A duplicate key is an error.
func decodeObject(data []byte) (map[string]json.RawMessage, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, nil, fmt.Errorf("expected an object")
	}
	fields := map[string]json.RawMessage{}
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		name, ok := tok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("expected an object key")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		if _, dup := fields[name]; dup {
			return nil, nil, fmt.Errorf("duplicate key %q", name)
		}
		fields[name] = raw
		order = append(order, name)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	return fields, order, nil
}

// Choice is one choice in canonical stored form.
type Choice struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Decision is one decision in canonical stored form. Key is empty when the
// hub is to assign a number.
type Decision struct {
	Key             string   `json:"key,omitempty"`
	Question        string   `json:"question"`
	DefaultChoiceID string   `json:"default_choice_id,omitempty"`
	Choices         []Choice `json:"choices"`
}

// ChoiceError is one field-specific validation failure. Field is a path
// rooted at the request field, for example "choices[1].choices[0].label".
type ChoiceError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ChoiceError) Error() string { return e.Field + ": " + e.Message }

// ChoiceErrors is every validation failure of one choices payload, in input
// order.
type ChoiceErrors []*ChoiceError

func (es ChoiceErrors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// DecisionKeyNorm returns the normalized form of a decision key, used for
// uniqueness: keys compare case-insensitively.
func DecisionKeyNorm(key string) string { return strings.ToLower(key) }

// IsHubDecisionKey reports whether key has the shape of a hub-assigned
// number. Agent-chosen keys always contain a non-digit, so the two
// namespaces cannot collide.
func IsHubDecisionKey(key string) bool { return hubKeyRe.MatchString(key) }

// ValidDecisionKey reports whether key is acceptable from an agent: an
// agent key ([A-Za-z0-9_-]{1,32} with at least one non-digit) or the shape
// of a hub number the agent was given earlier. Whether a hub number was
// actually issued to the agent is the store's check.
func ValidDecisionKey(key string) bool {
	if !identRe.MatchString(key) {
		return false
	}
	if strings.Trim(key, "0123456789") != "" {
		return true
	}
	if !hubKeyRe.MatchString(key) {
		return false
	}
	_, err := strconv.ParseInt(key, 10, 64)
	return err == nil
}

// NormalizeDecisions validates the agent's decisions and returns them in
// canonical form. Every rule of design 4.2 and 4.3 is checked, and every
// failure is reported (a ChoiceErrors) with its field path under root
// (the request field name, normally "choices"). Surrounding whitespace of
// every string is trimmed; choices without an id get their 1-based
// position; a default naming a label becomes that choice's id.
func NormalizeDecisions(root string, in []DecisionInput) ([]Decision, error) {
	var errs ChoiceErrors
	add := func(field, code, format string, args ...any) {
		errs = append(errs, &ChoiceError{Field: field, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	switch {
	case len(in) == 0:
		add(root, ChoiceErrRequired, "at least one decision is required")
		return nil, errs
	case len(in) > MaxDecisionsPerMessage:
		add(root, ChoiceErrTooMany, "at most %d decisions per message, got %d", MaxDecisionsPerMessage, len(in))
		return nil, errs
	}

	out := make([]Decision, 0, len(in))
	keys := map[string]int{}
	for i, d := range in {
		p := fmt.Sprintf("%s[%d]", root, i)
		for _, name := range d.extraFields {
			if reservedDecisionFields[name] {
				add(p+"."+name, ChoiceErrReservedField, "%q is reserved and not supported", name)
			} else {
				add(p+"."+name, ChoiceErrUnknownField, "unknown field %q", name)
			}
		}

		nd := Decision{Key: strings.TrimSpace(d.ID)}
		if nd.Key != "" {
			if !ValidDecisionKey(nd.Key) {
				add(p+".id", ChoiceErrInvalid, "decision id must match [A-Za-z0-9_-]{1,%d} and contain a non-digit, or be a number the hub assigned to you", MaxDecisionKeyLen)
			} else if prev, dup := keys[DecisionKeyNorm(nd.Key)]; dup {
				add(p+".id", ChoiceErrDuplicate, "decision id %q is also used by %s[%d] (ids compare case-insensitively)", nd.Key, root, prev)
			} else {
				keys[DecisionKeyNorm(nd.Key)] = i
			}
		}

		nd.Question = strings.TrimSpace(d.Question)
		switch {
		case nd.Question == "":
			add(p+".question", ChoiceErrRequired, "question is required")
		case !validQuestionText(nd.Question):
			add(p+".question", ChoiceErrInvalid, "question must be valid UTF-8 text without control characters (line breaks and tabs are allowed)")
		case utf8.RuneCountInString(nd.Question) > MaxDecisionQuestionLen:
			add(p+".question", ChoiceErrTooLong, "question is at most %d characters", MaxDecisionQuestionLen)
		}

		nd.Choices = normalizeChoices(p+".choices", d.Choices, add)
		if def := strings.TrimSpace(d.Default); def != "" && nd.Choices != nil {
			id, ok := matchDefault(nd.Choices, def)
			if !ok {
				add(p+".default", ChoiceErrNoMatch, "default %q matches no choice id or label", def)
			}
			nd.DefaultChoiceID = id
		}
		out = append(out, nd)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(enc) > MaxChoicesPayloadBytes {
		add(root, ChoiceErrTooLarge, "decisions are %d bytes after normalization; the limit is %d", len(enc), MaxChoicesPayloadBytes)
		return nil, errs
	}
	return out, nil
}

// normalizeChoices validates one decision's choices. It returns nil when
// the count is out of range (the per-choice checks are then skipped).
func normalizeChoices(p string, in []ChoiceInput, add func(field, code, format string, args ...any)) []Choice {
	switch {
	case len(in) < MinChoicesPerDecision:
		add(p, ChoiceErrTooFew, "at least %d choices are required, got %d", MinChoicesPerDecision, len(in))
		return nil
	case len(in) > MaxChoicesPerDecision:
		add(p, ChoiceErrTooMany, "at most %d choices are allowed, got %d", MaxChoicesPerDecision, len(in))
		return nil
	}
	out := make([]Choice, 0, len(in))
	ids := map[string]int{}
	labels := map[string]int{}
	for j, c := range in {
		cp := fmt.Sprintf("%s[%d]", p, j)
		for _, name := range c.extraFields {
			if reservedDecisionFields[name] {
				add(cp+"."+name, ChoiceErrReservedField, "%q is reserved and not supported", name)
			} else {
				add(cp+"."+name, ChoiceErrUnknownField, "unknown field %q", name)
			}
		}
		nc := Choice{
			ID:          strings.TrimSpace(c.ID),
			Label:       strings.TrimSpace(c.Label),
			Description: strings.TrimSpace(c.Description),
		}
		idField := cp + ".id"
		if nc.ID == "" {
			nc.ID = strconv.Itoa(j + 1)
			idField = cp
		} else if !identRe.MatchString(nc.ID) {
			add(idField, ChoiceErrInvalid, "choice id must match [A-Za-z0-9_-]{1,%d}", MaxDecisionKeyLen)
		}
		if prev, dup := ids[strings.ToLower(nc.ID)]; dup {
			add(idField, ChoiceErrDuplicate, "choice id %q is also used by %s[%d] (a choice without an id gets its position)", nc.ID, p, prev)
		} else {
			ids[strings.ToLower(nc.ID)] = j
		}

		switch {
		case nc.Label == "":
			add(cp+".label", ChoiceErrRequired, "label is required")
		case !validPlainText(nc.Label):
			add(cp+".label", ChoiceErrInvalid, "label must be one line of plain text without control characters")
		case utf8.RuneCountInString(nc.Label) > MaxChoiceLabelLen:
			add(cp+".label", ChoiceErrTooLong, "label is at most %d characters", MaxChoiceLabelLen)
		default:
			if prev, dup := labels[strings.ToLower(nc.Label)]; dup {
				add(cp+".label", ChoiceErrDuplicate, "label %q is also used by %s[%d] (labels compare case-insensitively)", nc.Label, p, prev)
			} else {
				labels[strings.ToLower(nc.Label)] = j
			}
		}

		switch {
		case nc.Description == "":
		case !validPlainText(nc.Description):
			add(cp+".description", ChoiceErrInvalid, "description must be one line of text without control characters")
		case utf8.RuneCountInString(nc.Description) > MaxChoiceDescriptionLen:
			add(cp+".description", ChoiceErrTooLong, "description is at most %d characters", MaxChoiceDescriptionLen)
		}
		out = append(out, nc)
	}
	return out
}

// matchDefault resolves a default to a choice id: an id match wins, then a
// label match, both case-insensitive.
func matchDefault(choices []Choice, def string) (string, bool) {
	for _, c := range choices {
		if strings.EqualFold(c.ID, def) {
			return c.ID, true
		}
	}
	for _, c := range choices {
		if strings.EqualFold(c.Label, def) {
			return c.ID, true
		}
	}
	return "", false
}

// validPlainText reports whether s is valid UTF-8 with no control, format,
// line or paragraph separator characters (so no newlines).
// validQuestionText reports whether s is valid UTF-8 without control
// characters other than line feed and tab: a question may span lines, but
// no other control character (a carriage return, an escape sequence) may
// change how it, or the choices after it, are shown.
func validQuestionText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && unicode.Is(unicode.Cc, r) {
			return false
		}
	}
	return true
}

func validPlainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if plaintext.IsUnsafe(r) {
			return false
		}
	}
	return true
}

// Markdown fallback (design 4.6).

// zeroWidthSpace breaks a mention token without changing how it reads.
const zeroWidthSpace = "\u200b"

var (
	// broadcastMentionRe matches @everyone, @here and @channel as a word
	// (not inside an email address or another word).
	broadcastMentionRe = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.@])@(everyone|here|channel)\b`)
	// bracketMentionRe matches the start of Slack <!channel>/<!here> and
	// Slack/Discord <@id> and <@&role> mentions.
	bracketMentionRe = regexp.MustCompile(`<([!@])`)
	// markdownSpecialRe matches the characters that start inline markdown
	// (emphasis, code, links, autolinks and HTML, strikethrough, tables).
	// Escaped text is always placed mid-line, after a marker such as "1) ",
	// so block-level markers (#, +, -, digits with ".") need no escape.
	markdownSpecialRe = regexp.MustCompile("([\\\\`*_\\[\\]<>~|])")
)

// NeutralizeMentions breaks broadcast and bracket mention syntax (@everyone,
// @here, @channel, <!channel>, <@id>) with a zero-width space so no surface
// resolves it, and leaves the rest of s unchanged.
func NeutralizeMentions(s string) string {
	s = broadcastMentionRe.ReplaceAllString(s, "${1}@"+zeroWidthSpace+"${2}")
	return bracketMentionRe.ReplaceAllString(s, "<"+zeroWidthSpace+"${1}")
}

// EscapeMarkdownPlain makes s display as literal text in markdown: every
// markdown punctuation character is backslash-escaped and mentions are
// neutralized.
func EscapeMarkdownPlain(s string) string {
	return NeutralizeMentions(markdownSpecialRe.ReplaceAllString(s, `\$1`))
}

// RenderChoiceFallback renders the choice prompt's msg: the intro (the
// agent's body, unchanged) followed by the decisions as markdown, so every
// surface that shows msg shows the decisions. Each decision must already
// have its key. Questions and descriptions pass through as markdown with
// mentions neutralized; keys, choice ids and labels are escaped as plain
// text. answerURL, when set, is the link to answer in Scion.
//
// introLen is the byte offset in msg where the fallback block starts (0
// with no intro). The hub stores it on the message's first decision row and
// recovers the intro with ChoiceIntro.
func RenderChoiceFallback(intro string, decisions []Decision, answerURL string) (msg string, introLen int) {
	var b strings.Builder
	intro = strings.TrimRight(intro, " \t\r\n")
	if intro != "" {
		b.WriteString(intro)
		b.WriteString("\n\n")
		introLen = b.Len()
	}
	if len(decisions) == 1 {
		b.WriteString("**1 decision needed**\n")
	} else {
		fmt.Fprintf(&b, "**%d decisions needed**\n", len(decisions))
	}
	for _, d := range decisions {
		b.WriteString("\n**")
		b.WriteString(EscapeMarkdownPlain(d.Key))
		b.WriteString(".** ")
		b.WriteString(NeutralizeMentions(d.Question))
		b.WriteString("\n")
		for _, c := range d.Choices {
			b.WriteString("  ")
			b.WriteString(EscapeMarkdownPlain(c.ID))
			b.WriteString(") ")
			b.WriteString(EscapeMarkdownPlain(c.Label))
			if c.Description != "" {
				b.WriteString(": ")
				b.WriteString(NeutralizeMentions(c.Description))
			}
			if c.ID == d.DefaultChoiceID && d.DefaultChoiceID != "" {
				b.WriteString(" _(recommended)_")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	if answerURL != "" {
		fmt.Fprintf(&b, "_Answer in Scion: %s, or reply here and the agent will record your answer._", answerURL)
	} else {
		b.WriteString("_Reply here and the agent will record your answer._")
	}
	return b.String(), introLen
}

// ChoiceIntro returns the intro of a choice prompt's msg, given the
// introLen RenderChoiceFallback returned: the agent's body without the
// fallback block and without the separating blank line. It returns "" when
// introLen is 0 or does not fit msg (out of range or not on a character
// boundary). The web never slices msg itself (JavaScript strings are
// UTF-16); the hub serves this value.
func ChoiceIntro(msg string, introLen int) string {
	if introLen <= 0 || introLen > len(msg) {
		return ""
	}
	if introLen < len(msg) && !utf8.RuneStart(msg[introLen]) {
		return ""
	}
	return strings.TrimSuffix(msg[:introLen], "\n\n")
}
