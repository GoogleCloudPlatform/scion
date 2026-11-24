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

package messagepolicy

import (
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// SenderFacts carries mechanical sender attributes exposed under `sender` in CEL.
type SenderFacts struct {
	Kind      string
	ID        string
	Slug      string
	ProjectID string
}

// RecipientFacts carries mechanical recipient attributes exposed under `recipient` in CEL.
type RecipientFacts struct {
	Kind string // "agent" | "user" | "conversation"
	ID   string
	Slug string
}

// MessageFacts carries mechanical outbound message attributes exposed under `message` in CEL.
type MessageFacts struct {
	Type            string
	Body            string
	HasChoices      bool
	Urgent          bool
	AttachmentCount int
}

// ConversationFacts carries conversation participant metadata exposed under `conversation` in CEL.
type ConversationFacts struct {
	Kind                  string   `json:"kind,omitempty"`
	AgentParticipantIDs   []string `json:"agent_participant_ids,omitempty"`
	UserParticipantIDs    []string `json:"user_participant_ids,omitempty"`
	UnmentionedAgentSlugs []string `json:"unmentioned_agent_slugs,omitempty"`
}

// Judgments carries optional typed judgment maps (empty in M1; reading an
// unevaluated judgment key in CEL yields missing_input -> ClassFailed, never false).
type Judgments struct {
	Flags  map[string]bool
	Labels map[string]string
	Scores map[string]float64
}

// Facts aggregates all mechanical and judgment inputs for building a
// message_policy CEL Activation.
type Facts struct {
	Surface           string
	Sender            SenderFacts
	Recipient         RecipientFacts
	Message           MessageFacts
	MentionedAgentIDs []string
	MentionedUserIDs  []string
	Conversation      ConversationFacts
	Judgments         Judgments
}

// Activation holds a pre-built CEL variable map for evaluating one or more
// policies against a single outbound send.
type Activation struct {
	vars map[string]any
}

// TruncateBodyUTF8 truncates body to at most maxBytes bytes without splitting a
// multi-byte UTF-8 rune at the cut boundary, returning the truncated string,
// the original byte count, and whether truncation occurred.
func TruncateBodyUTF8(body string, maxBytes int) (string, int, bool) {
	origBytes := len(body)
	if maxBytes < 0 {
		maxBytes = 0
	}
	if origBytes <= maxBytes {
		return body, origBytes, false
	}
	end := maxBytes
	// Back up past UTF-8 continuation bytes if maxBytes landed mid-rune, then
	// check whether the leading rune fits within maxBytes.
	start := end
	for start > 0 && end-start < utf8.UTFMax && !utf8.RuneStart(body[start]) {
		start--
	}
	if start < end {
		r, size := utf8.DecodeRuneInString(body[start:])
		if r != utf8.RuneError && start+size > maxBytes {
			end = start
		}
	}
	return body[:end], origBytes, true
}

func cloneStringsNonNil(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	return slices.Clone(in)
}

// NewActivation constructs a message_policy v1 CEL Activation from f.
// Unevaluated judgment maps are populated as empty maps so reading any key
// produces EvalError{Reason: "missing_input"} -> ClassFailed, never false.
func NewActivation(f Facts) Activation {
	truncBody, origBytes, wasTruncated := TruncateBodyUTF8(f.Message.Body, MaxBodyBytes)

	flags := make(map[string]bool, len(f.Judgments.Flags))
	maps.Copy(flags, f.Judgments.Flags)

	labels := make(map[string]string, len(f.Judgments.Labels))
	maps.Copy(labels, f.Judgments.Labels)

	scores := make(map[string]float64, len(f.Judgments.Scores))
	maps.Copy(scores, f.Judgments.Scores)

	vars := map[string]any{
		"surface": f.Surface,
		"sender": map[string]string{
			"kind":       f.Sender.Kind,
			"id":         f.Sender.ID,
			"slug":       f.Sender.Slug,
			"project_id": f.Sender.ProjectID,
		},
		"recipient": map[string]string{
			"kind": f.Recipient.Kind,
			"id":   f.Recipient.ID,
			"slug": f.Recipient.Slug,
		},
		"message": map[string]any{
			"type":             f.Message.Type,
			"body":             truncBody,
			"body_truncated":   wasTruncated,
			"bytes":            int64(origBytes),
			"has_choices":      f.Message.HasChoices,
			"urgent":           f.Message.Urgent,
			"attachment_count": int64(f.Message.AttachmentCount),
		},
		"mentioned_agent_ids": cloneStringsNonNil(f.MentionedAgentIDs),
		"mentioned_user_ids":  cloneStringsNonNil(f.MentionedUserIDs),
		"conversation": map[string]any{
			"kind":                  f.Conversation.Kind,
			"agent_participant_ids": cloneStringsNonNil(f.Conversation.AgentParticipantIDs),
			"user_participant_ids":  cloneStringsNonNil(f.Conversation.UserParticipantIDs),
		},
		"judgment_flags":  flags,
		"judgment_labels": labels,
		"judgment_scores": scores,
	}
	return Activation{vars: vars}
}

// Map returns the underlying CEL activation variable bindings.
func (a Activation) Map() map[string]any {
	if a.vars == nil {
		return map[string]any{}
	}
	return a.vars
}

// BuildSystemOneInput constructs a bounded systemone.Input from typed outbound
// message Facts matching the message_policy v1 ContentEnvironment (consumer
// design §5.4). The System One runtime selects only fields referenced by the
// compiled rule's inputs list and enforces per-field MaxBytes / on_oversize.
func BuildSystemOneInput(f Facts) systemone.Input {
	fields := []provider.InputField{
		{
			Name:  FieldMessageBody,
			Kind:  spec.FieldKindText,
			Value: f.Message.Body,
			Trust: spec.TrustUntrusted,
		},
		{
			Name:  FieldMessageType,
			Kind:  spec.FieldKindText,
			Value: f.Message.Type,
			Trust: spec.TrustUntrusted,
		},
		{
			Name:  FieldSenderSlug,
			Kind:  spec.FieldKindText,
			Value: f.Sender.Slug,
			Trust: spec.TrustTrusted,
		},
		{
			Name:  FieldRecipientSlug,
			Kind:  spec.FieldKindText,
			Value: f.Recipient.Slug,
			Trust: spec.TrustTrusted,
		},
		{
			Name:  FieldRecipientKind,
			Kind:  spec.FieldKindText,
			Value: f.Recipient.Kind,
			Trust: spec.TrustTrusted,
		},
		{
			Name:  FieldConversationUnmentionedAgentSlug,
			Kind:  spec.FieldKindText,
			Value: strings.Join(f.Conversation.UnmentionedAgentSlugs, "\n"),
			Trust: spec.TrustTrusted,
		},
	}
	return systemone.Input{
		Fields: fields,
	}
}
