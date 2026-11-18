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
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Error codes of the decision prompt send path (design 4.3, 4.12).
const (
	ErrCodeDecisionKeyExists   = "decision_key_exists"
	ErrCodeHubMintedType       = "hub_minted_type"
	ErrCodeTooManyPending      = "too_many_pending_decisions"
	ErrCodeDecisionKeyNotIssue = "decision_key_not_issued"
)

// rejectHubMintedType answers 400 and reports true when a client named a
// message type only the hub may set (decision-prompt, decision-response). Every
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

// decisionPromptSend is a validated decision prompt on its way through the
// outbound-message handler: its decisions, normalized, and, once stored,
// the store's result.
type decisionPromptSend struct {
	decisions []messages.Decision
	update    bool
	intro     string
	result    *store.DecisionPromptResult
}

// decisionStoreError is a store failure of a decision prompt the handler answers
// with its own status and code instead of a 500.
type decisionStoreError struct{ err error }

func (e *decisionStoreError) Error() string { return e.err.Error() }
func (e *decisionStoreError) Unwrap() error { return e.err }

// prepareDecisionPromptSend checks a send that carries decisions, before any
// routing: the experiment (400 experiment_disabled when off, never a silent
// drop), --update without decisions, a recipient that is an agent or a
// broadcast, attachments, and the decisions themselves. It returns nil and
// true for an ordinary send, and false when it wrote the refusal.
func (s *Server) prepareDecisionPromptSend(w http.ResponseWriter, agentID string, req *OutboundMessageRequest) (*decisionPromptSend, bool) {
	decisionsInput := req.Decisions
	decisionsUpdate := req.DecisionsUpdate

	// legacy choices fallback: remove in ptone/scion#8249
	if req.Choices != nil && req.Decisions != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"cannot specify both decisions and legacy choices",
			map[string]interface{}{"field": "decisions"})
		return nil, false
	}
	if (req.Decisions == nil && req.Choices != nil) || (!req.DecisionsUpdate && req.ChoicesUpdate) {
		if decisionsInput == nil {
			decisionsInput = req.Choices
		}
		if !decisionsUpdate {
			decisionsUpdate = req.ChoicesUpdate
		}
		if s.messageLog != nil {
			s.messageLog.Debug("legacy choices request field used", "agent_id", agentID)
		} else {
			slog.Debug("legacy choices request field used", "agent_id", agentID)
		}
	}

	if decisionsInput == nil && !decisionsUpdate {
		return nil, true
	}
	if !s.experimentEnabled(experiments.Decisions) {
		writeError(w, http.StatusBadRequest, ErrCodeExperimentDisabled,
			"experiment_disabled: "+experiments.Decisions+" is off; decisions are not available on this hub",
			map[string]interface{}{"experiment": experiments.Decisions})
		return nil, false
	}
	if decisionsInput == nil {
		ValidationError(w, "decisions_update requires decisions", map[string]interface{}{"field": "decisions_update"})
		return nil, false
	}
	recipient := strings.TrimSpace(req.Recipient)
	if strings.HasPrefix(recipient, "agent:") || isBroadcastRecipient(recipient) {
		ValidationError(w, "decisions can be sent only to a user or a conversation, not to an agent or a broadcast",
			map[string]interface{}{"field": "recipient"})
		return nil, false
	}
	if len(req.Attachments) > 0 || len(req.AttachmentIDs) > 0 {
		ValidationError(w, "a message with decisions cannot carry attachments", map[string]interface{}{"field": "attachments"})
		return nil, false
	}
	decisions, err := messages.NormalizeDecisions("decisions", decisionsInput)
	if err != nil {
		writeDecisionValidationErrors(w, err)
		return nil, false
	}
	// The intro is set from the body once its mentions are rewritten.
	return &decisionPromptSend{decisions: decisions, update: decisionsUpdate}, true
}

// isBroadcastRecipient reports whether a recipient names a broadcast.
func isBroadcastRecipient(recipient string) bool {
	switch strings.ToLower(recipient) {
	case "broadcast", "all", "*", "agent:*", "agents:*":
		return true
	}
	return false
}

// writeDecisionValidationErrors answers 400 with every field error of a NormalizeDecisions
// failure.
func writeDecisionValidationErrors(w http.ResponseWriter, err error) {
	var errs messages.DecisionValidationErrors
	if errors.As(err, &errs) {
		ValidationError(w, "invalid decisions: "+errs.Error(), map[string]interface{}{"errors": errs})
		return
	}
	ValidationError(w, "invalid decisions: "+err.Error(), nil)
}

// writeDecisionStoreError answers a CreateDecisionPrompt failure. It reports
// false for an error that is not a decision prompt refusal (the caller answers
// 500).
func writeDecisionStoreError(w http.ResponseWriter, err error) bool {
	var exists *store.DecisionKeyExistsError
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
		writeError(w, http.StatusConflict, ErrCodeDecisionKeyExists, exists.Error(),
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

// decisionAnswerURL is the absolute link to the prompt in web chat, or "" when
// the hub has no public URL or the message has no thread to link to.
func (s *Server) decisionAnswerURL(m *store.Message) string {
	base := strings.TrimRight(s.HubEndpoints().Public, "/")
	if base == "" || m.ThreadID == "" {
		return ""
	}
	if strings.HasPrefix(m.ThreadID, "dm:") {
		return base + starDMPermalink(m.ThreadID, m.ID)
	}
	return base + starPermalink(m.ProjectID, m.ThreadID, m.ID)
}

// storeDecisionPrompt stores the prompt message and its decisions in one
// transaction (CreateDecisionPrompt renders msg and sets the type), then
// copies the stored body and type onto the structured message that is
// published. A refusal comes back as a decisionStoreError.
func (s *Server) storeDecisionPrompt(ctx context.Context, dp *decisionPromptSend, agent *store.Agent, storeMsg *store.Message, structuredMsg *messages.StructuredMessage) error {
	res, err := s.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message:        storeMsg,
		Intro:          dp.intro,
		AnswerURL:      s.decisionAnswerURL(storeMsg),
		AskerAgentID:   agent.ID,
		AskerAgentName: agentDisplayName(agent),
		Decisions:      dp.decisions,
		Update:         dp.update,
	})
	if err != nil {
		return &decisionStoreError{err: err}
	}
	dp.result = res
	structuredMsg.Msg = storeMsg.Msg
	structuredMsg.Type = storeMsg.Type
	return nil
}

// publishReplacedDecisions tells the open cards of every version a decision
// send replaced (--update) that it is now replaced: chat.decision.updated for
// the old row, on its own prompt's subjects (it may be in another thread or
// DM than the new prompt), with a view that carries replacedBy. Best
// effort: the send has committed, and history carries the state anyway.
func (s *Server) publishReplacedDecisions(ctx context.Context, res *store.DecisionPromptResult) {
	if res == nil {
		return
	}
	for _, d := range res.Decisions {
		if d.Replaced == nil || d.Replaced.RowID == "" {
			continue
		}
		old, err := s.store.GetDecision(ctx, d.Replaced.RowID)
		if err != nil {
			s.messageLog.Warn("decisions: replaced decision not found for decision.updated", "row_id", d.Replaced.RowID, "error", err)
			continue
		}
		chatKey := old.ChatKey
		if chatKey == "" {
			prompt, err := s.store.GetMessage(ctx, old.MessageID)
			if err != nil {
				s.messageLog.Warn("decisions: replaced decision's prompt not found for decision.updated", "row_id", old.ID, "message_id", old.MessageID, "error", err)
				continue
			}
			chatKey = prompt.ThreadID
		}
		if chatKey == "" {
			s.messageLog.Warn("decisions: replaced decision has no chat_key for decision.updated", "row_id", old.ID, "message_id", old.MessageID)
			continue
		}
		s.publishDecisionUpdatedAt(ctx, old.ProjectID, old.MessageID, chatKey, s.decisionViewFor(ctx, old))
	}
}

// DecisionSendResult is one decision in a decision prompt send response.
type DecisionSendResult struct {
	Key      string            `json:"key"`
	Version  int               `json:"version"`
	Ref      string            `json:"ref"`
	RowID    string            `json:"row_id"`
	Replaced *DecisionReplaced `json:"replaced"`
}

// DecisionReplaced is the version a send replaced, as it was. For a
// decided version, Answer is the effective answer's label and AnsweredBy
// who gave it (the name, else the email).
type DecisionReplaced struct {
	Version    int    `json:"version"`
	Status     string `json:"status"`
	Answer     string `json:"answer,omitempty"`
	AnsweredBy string `json:"answered_by,omitempty"`
}

// decisionSendResults is the decisions field of the send response.
func decisionSendResults(agentID string, res *store.DecisionPromptResult) []DecisionSendResult {
	if res == nil {
		return nil
	}
	out := make([]DecisionSendResult, len(res.Decisions))
	for i, d := range res.Decisions {
		out[i] = DecisionSendResult{Key: d.Key, Version: d.Version, Ref: d.Ref(agentID), RowID: d.RowID}
		if d.Replaced != nil {
			r := &DecisionReplaced{Version: d.Replaced.Version, Status: d.Replaced.Status}
			if a := d.Replaced.Answer; a != nil {
				r.Answer = a.ChoiceLabel
				r.AnsweredBy = a.AnsweredByName
				if r.AnsweredBy == "" {
					r.AnsweredBy = a.AnsweredByEmail
				}
			}
			out[i].Replaced = r
		}
	}
	return out
}
