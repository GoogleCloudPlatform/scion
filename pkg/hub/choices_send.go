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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Error codes of the choice prompt send path (design 4.3, 4.12).
const (
	ErrCodeChoiceKeyExists     = "choice_key_exists"
	ErrCodeHubMintedType       = "hub_minted_type"
	ErrCodeTooManyPending      = "too_many_pending_decisions"
	ErrCodeDecisionKeyNotIssue = "decision_key_not_issued"
)

// rejectHubMintedType answers 400 and reports true when a client named a
// message type only the hub may set (choice-prompt, choice-response). Every
// client send path that accepts a caller-supplied type calls it before
// anything is stored or routed (design 4.4, acceptance criterion 9).
func rejectHubMintedType(w http.ResponseWriter, msgType string) bool {
	if !messages.IsHubMintedType(msgType) {
		return false
	}
	writeError(w, http.StatusBadRequest, ErrCodeHubMintedType,
		fmt.Sprintf("message type %q is set by the hub and cannot be sent by a client", msgType),
		map[string]interface{}{"field": "type"})
	return true
}

// choicePromptSend is a validated choice prompt on its way through the
// outbound-message handler: its decisions, normalized, and, once stored,
// the store's result.
type choicePromptSend struct {
	decisions []messages.Decision
	update    bool
	intro     string
	result    *store.ChoicePromptResult
}

// choiceStoreError is a store failure of a choice prompt the handler answers
// with its own status and code instead of a 500.
type choiceStoreError struct{ err error }

func (e *choiceStoreError) Error() string { return e.err.Error() }
func (e *choiceStoreError) Unwrap() error { return e.err }

// prepareChoicePromptSend checks a send that carries choices, before any
// routing: the experiment (400 experiment_disabled when off, never a silent
// drop), --update without choices, a recipient that is an agent or a
// broadcast, attachments, and the decisions themselves. It returns nil and
// true for an ordinary send, and false when it wrote the refusal.
func (s *Server) prepareChoicePromptSend(w http.ResponseWriter, req *OutboundMessageRequest) (*choicePromptSend, bool) {
	if req.Choices == nil && !req.ChoicesUpdate {
		return nil, true
	}
	if !s.experimentEnabled(experiments.ChatChoices) {
		writeError(w, http.StatusBadRequest, ErrCodeExperimentDisabled,
			"experiment_disabled: "+experiments.ChatChoices+" is off; choices are not available on this hub",
			map[string]interface{}{"experiment": experiments.ChatChoices})
		return nil, false
	}
	if req.Choices == nil {
		ValidationError(w, "choices_update requires choices", map[string]interface{}{"field": "choices_update"})
		return nil, false
	}
	recipient := strings.TrimSpace(req.Recipient)
	if strings.HasPrefix(recipient, "agent:") || isBroadcastRecipient(recipient) {
		ValidationError(w, "choices can be sent only to a user or a conversation, not to an agent or a broadcast",
			map[string]interface{}{"field": "recipient"})
		return nil, false
	}
	if len(req.Attachments) > 0 || len(req.AttachmentIDs) > 0 {
		ValidationError(w, "a message with choices cannot carry attachments", map[string]interface{}{"field": "attachments"})
		return nil, false
	}
	decisions, err := messages.NormalizeDecisions("choices", req.Choices)
	if err != nil {
		writeChoiceErrors(w, err)
		return nil, false
	}
	return &choicePromptSend{decisions: decisions, update: req.ChoicesUpdate, intro: req.Msg}, true
}

// isBroadcastRecipient reports whether a recipient names a broadcast.
func isBroadcastRecipient(recipient string) bool {
	switch strings.ToLower(recipient) {
	case "broadcast", "all", "*", "agent:*", "agents:*":
		return true
	}
	return false
}

// writeChoiceErrors answers 400 with every field error of a NormalizeDecisions
// failure.
func writeChoiceErrors(w http.ResponseWriter, err error) {
	var errs messages.ChoiceErrors
	if errors.As(err, &errs) {
		ValidationError(w, "invalid choices: "+errs.Error(), map[string]interface{}{"errors": errs})
		return
	}
	ValidationError(w, "invalid choices: "+err.Error(), nil)
}

// writeChoiceStoreError answers a CreateChoicePrompt failure. It reports
// false for an error that is not a choice prompt refusal (the caller answers
// 500).
func writeChoiceStoreError(w http.ResponseWriter, err error) bool {
	var exists *store.ChoiceKeyExistsError
	var notIssued *store.DecisionKeyNotIssuedError
	switch {
	case errors.As(err, &exists):
		keys := make([]map[string]interface{}, len(exists.Existing))
		for i, k := range exists.Existing {
			entry := map[string]interface{}{"key": k.Key, "version": k.Version, "status": k.Status}
			if k.Answer != nil {
				entry["answer"] = k.Answer.ChoiceLabel
			}
			keys[i] = entry
		}
		writeError(w, http.StatusConflict, ErrCodeChoiceKeyExists, exists.Error(),
			map[string]interface{}{"existing": keys})
		return true
	case errors.As(err, &notIssued):
		writeError(w, http.StatusBadRequest, ErrCodeDecisionKeyNotIssue, notIssued.Error(),
			map[string]interface{}{"keys": notIssued.Keys})
		return true
	case errors.Is(err, store.ErrTooManyPendingDecisions):
		writeError(w, http.StatusConflict, ErrCodeTooManyPending,
			fmt.Sprintf("at most %d pending decisions per agent; wait for answers or replace existing ones with --update", store.MaxPendingDecisionsPerAgent), nil)
		return true
	case errors.Is(err, store.ErrInvalidInput):
		ValidationError(w, err.Error(), nil)
		return true
	}
	return false
}

// choiceAnswerURL is the absolute link to the prompt in web chat, or "" when
// the hub has no public URL or the message has no thread to link to.
func (s *Server) choiceAnswerURL(m *store.Message) string {
	base := strings.TrimRight(s.HubEndpoints().Public, "/")
	if base == "" || m.ThreadID == "" {
		return ""
	}
	if strings.HasPrefix(m.ThreadID, "dm:") {
		return base + starDMPermalink(m.ThreadID, m.ID)
	}
	return base + starPermalink(m.ProjectID, m.ThreadID, m.ID)
}

// storeChoicePrompt stores the prompt message and its decisions in one
// transaction (CreateChoicePrompt renders msg and sets the type), then
// copies the stored body and type onto the structured message that is
// published. A refusal comes back as a choiceStoreError.
func (s *Server) storeChoicePrompt(ctx context.Context, cp *choicePromptSend, agent *store.Agent, storeMsg *store.Message, structuredMsg *messages.StructuredMessage) error {
	res, err := s.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message:        storeMsg,
		Intro:          cp.intro,
		AnswerURL:      s.choiceAnswerURL(storeMsg),
		AskerAgentID:   agent.ID,
		AskerAgentName: agentDisplayName(agent),
		Decisions:      cp.decisions,
		Update:         cp.update,
	})
	if err != nil {
		return &choiceStoreError{err: err}
	}
	cp.result = res
	structuredMsg.Msg = storeMsg.Msg
	structuredMsg.Type = storeMsg.Type
	return nil
}

// ChoiceDecisionResult is one decision in a choice prompt send response.
type ChoiceDecisionResult struct {
	Key      string                  `json:"key"`
	Version  int                     `json:"version"`
	Ref      string                  `json:"ref"`
	RowID    string                  `json:"row_id"`
	Replaced *ChoiceReplacedDecision `json:"replaced"`
}

// ChoiceReplacedDecision is the version a send replaced, as it was.
type ChoiceReplacedDecision struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
	Answer  string `json:"answer,omitempty"`
}

// choiceDecisionResults is the decisions field of the send response.
func choiceDecisionResults(agentID string, res *store.ChoicePromptResult) []ChoiceDecisionResult {
	if res == nil {
		return nil
	}
	out := make([]ChoiceDecisionResult, len(res.Decisions))
	for i, d := range res.Decisions {
		out[i] = ChoiceDecisionResult{Key: d.Key, Version: d.Version, Ref: d.Ref(agentID), RowID: d.RowID}
		if d.Replaced != nil {
			r := &ChoiceReplacedDecision{Version: d.Replaced.Version, Status: d.Replaced.Status}
			if d.Replaced.Answer != nil {
				r.Answer = d.Replaced.Answer.ChoiceLabel
			}
			out[i].Replaced = r
		}
	}
	return out
}
