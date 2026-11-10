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
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DecisionView is one version of a decision as clients see it (design
// choices v4, 4.4): the web card, the answer endpoint's responses and the
// choice.updated event.
type DecisionView struct {
	RowID           string              `json:"rowId"`
	MessageID       string              `json:"messageId"`
	Key             string              `json:"key"`
	Ref             string              `json:"ref"`
	Version         int                 `json:"version"`
	Status          string              `json:"status"`
	AskerAgentID    string              `json:"askerAgentId"`
	AskerAgentName  string              `json:"askerAgentName"`
	Question        string              `json:"question"`
	DefaultChoiceID string              `json:"defaultChoiceId,omitempty"`
	Choices         []messages.Choice   `json:"choices"`
	ReplacedBy      *DecisionReplacedBy `json:"replacedBy,omitempty"`
	// Decision is the effective answer; nil while pending.
	Decision *AnswerView `json:"decision,omitempty"`
}

// DecisionReplacedBy names the version that replaced a decision.
type DecisionReplacedBy struct {
	RowID     string    `json:"rowId"`
	MessageID string    `json:"messageId"`
	Version   int       `json:"version"`
	At        time.Time `json:"at"`
}

// AnswerView is an answer as clients see it.
type AnswerView struct {
	ChoiceID             string    `json:"choiceId"`
	Label                string    `json:"label"`
	AnsweredByUserID     string    `json:"answeredByUserId,omitempty"`
	AnsweredByName       string    `json:"answeredByName,omitempty"`
	AnsweredByEmail      string    `json:"answeredByEmail,omitempty"`
	Provenance           string    `json:"provenance"`
	Outcome              string    `json:"outcome,omitempty"`
	InterpretedByAgentID string    `json:"interpretedByAgentId,omitempty"`
	SourceMessageID      string    `json:"sourceMessageId,omitempty"`
	Surface              string    `json:"surface"`
	ResponseMessageID    string    `json:"responseMessageId"`
	At                   time.Time `json:"at"`
}

// ChoicePromptView is what a choice-prompt row gains in history and SSE: the
// intro (the agent's body without the markdown fallback, sliced by the hub
// so clients never slice msg by a byte offset) and the decisions.
type ChoicePromptView struct {
	Intro     string         `json:"intro"`
	Decisions []DecisionView `json:"decisions"`
}

// ChoiceResponseView is what a choice-response row gains in history and SSE.
type ChoiceResponseView struct {
	RowID           string `json:"rowId"`
	Ref             string `json:"ref"`
	Key             string `json:"key"`
	Version         int    `json:"version"`
	Question        string `json:"question"`
	ChoiceID        string `json:"choiceId"`
	Label           string `json:"label"`
	Provenance      string `json:"provenance"`
	PromptMessageID string `json:"promptMessageId"`
	SourceMessageID string `json:"sourceMessageId,omitempty"`
}

// answerView converts a stored answer.
func answerView(a *store.DecisionAnswer) *AnswerView {
	if a == nil {
		return nil
	}
	return &AnswerView{
		ChoiceID:             a.ChoiceID,
		Label:                a.ChoiceLabel,
		AnsweredByUserID:     a.AnsweredByUserID,
		AnsweredByName:       a.AnsweredByName,
		AnsweredByEmail:      a.AnsweredByEmail,
		Provenance:           a.Provenance,
		Outcome:              a.Outcome,
		InterpretedByAgentID: a.InterpretedByAgentID,
		SourceMessageID:      a.SourceMessageID,
		Surface:              a.Surface,
		ResponseMessageID:    a.ResponseMessageID,
		At:                   a.CreatedAt,
	}
}

// decisionView converts a stored decision. replacedBy, when the decision is
// replaced, is the row that replaced it (nil when it could not be read).
func decisionView(d *store.Decision, replacedBy *store.Decision) DecisionView {
	v := DecisionView{
		RowID:           d.ID,
		MessageID:       d.MessageID,
		Key:             d.Key,
		Ref:             d.Ref(),
		Version:         d.Version,
		Status:          d.Status,
		AskerAgentID:    d.AskerAgentID,
		AskerAgentName:  d.AskerAgentName,
		Question:        d.Question,
		DefaultChoiceID: d.DefaultChoiceID,
		Choices:         d.Choices,
		Decision:        answerView(d.EffectiveAnswer()),
	}
	if v.Choices == nil {
		v.Choices = []messages.Choice{}
	}
	if replacedBy != nil {
		r := &DecisionReplacedBy{RowID: replacedBy.ID, MessageID: replacedBy.MessageID, Version: replacedBy.Version}
		if d.ReplacedAt != nil {
			r.At = *d.ReplacedAt
		}
		v.ReplacedBy = r
	}
	return v
}

// decisionViewFor builds the view of d, reading the row that replaced it.
func (s *Server) decisionViewFor(ctx context.Context, d *store.Decision) DecisionView {
	var replacedBy *store.Decision
	if d.ReplacedByID != "" {
		r, err := s.store.GetDecision(ctx, d.ReplacedByID)
		if err != nil {
			slog.WarnContext(ctx, "choices: replacing decision unreadable", "row_id", d.ReplacedByID, "error", err)
		} else {
			replacedBy = r
		}
	}
	return decisionView(d, replacedBy)
}

// choicePromptViews returns the ChoicePromptView of every choice-prompt
// message among msgs, keyed by message id. The decisions are read in one
// batch. A read failure is logged and leaves the rows without views (the
// clients then show the markdown body).
func (s *Server) choicePromptViews(ctx context.Context, msgs []*store.Message) map[string]ChoicePromptView {
	var ids []string
	byID := make(map[string]*store.Message)
	for _, m := range msgs {
		if m != nil && m.Type == messages.TypeChoicePrompt {
			ids = append(ids, m.ID)
			byID[m.ID] = m
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.store.GetDecisionsByMessageIDs(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "choices: decisions unreadable for history", "error", err)
		return nil
	}
	// The versions that replaced these, in one batch.
	var replacedByIDs []string
	for _, ds := range rows {
		for _, d := range ds {
			if d.ReplacedByID != "" {
				replacedByIDs = append(replacedByIDs, d.ReplacedByID)
			}
		}
	}
	replacedBy := s.decisionRows(ctx, replacedByIDs)
	out := make(map[string]ChoicePromptView, len(rows))
	for id, ds := range rows {
		if len(ds) == 0 {
			continue
		}
		view := ChoicePromptView{Decisions: make([]DecisionView, 0, len(ds))}
		for _, d := range ds {
			if d.Position == 0 {
				view.Intro = messages.ChoiceIntro(byID[id].Msg, d.IntroLength)
			}
			view.Decisions = append(view.Decisions, decisionView(d, replacedBy[d.ReplacedByID]))
		}
		out[id] = view
	}
	return out
}

// decisionRows reads the given decision versions in one batch. A read
// failure is logged and returns none (the views then omit what needs
// them).
func (s *Server) decisionRows(ctx context.Context, rowIDs []string) map[string]*store.Decision {
	if len(rowIDs) == 0 {
		return nil
	}
	rows, err := s.store.GetDecisionRowsByIDs(ctx, rowIDs)
	if err != nil {
		slog.WarnContext(ctx, "choices: decision rows unreadable", "error", err)
		return nil
	}
	return rows
}

// choicePromptViewOf returns the view of one stored choice prompt, or nil.
func (s *Server) choicePromptViewOf(ctx context.Context, m *store.Message) *ChoicePromptView {
	views := s.choicePromptViews(ctx, []*store.Message{m})
	if v, ok := views[m.ID]; ok {
		return &v
	}
	return nil
}

// choiceResponseViews returns the ChoiceResponseView of every
// choice-response message among msgs, keyed by message id.
func (s *Server) choiceResponseViews(ctx context.Context, msgs []*store.Message) map[string]ChoiceResponseView {
	var ids []string
	for _, m := range msgs {
		if m != nil && m.Type == messages.TypeChoiceResponse {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	answers, err := s.store.GetDecisionAnswersByResponseMessageIDs(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "choices: answers unreadable for history", "error", err)
		return nil
	}
	rowIDs := make([]string, 0, len(answers))
	for _, a := range answers {
		rowIDs = append(rowIDs, a.DecisionRowID)
	}
	prompts := s.decisionRows(ctx, rowIDs)
	out := make(map[string]ChoiceResponseView, len(answers))
	for id, a := range answers {
		v := ChoiceResponseView{
			RowID:           a.DecisionRowID,
			Ref:             a.AskerAgentID + "/" + a.Key,
			Key:             a.Key,
			Version:         a.Version,
			Question:        a.QuestionText,
			ChoiceID:        a.ChoiceID,
			Label:           a.ChoiceLabel,
			Provenance:      a.Provenance,
			SourceMessageID: a.SourceMessageID,
		}
		if d := prompts[a.DecisionRowID]; d != nil {
			v.PromptMessageID = d.MessageID
		}
		out[id] = v
	}
	return out
}

// addChoiceHistoryViews fills the choice fields of a history page. They are
// omitted while web.chat_choices is off, so clients show the markdown body.
func (s *Server) addChoiceHistoryViews(ctx context.Context, resp *chatHistoryResponse) {
	if len(resp.Messages) == 0 || !s.experimentEnabled(experiments.ChatChoices) {
		return
	}
	msgs := make([]*store.Message, len(resp.Messages))
	for i := range resp.Messages {
		msgs[i] = &resp.Messages[i]
	}
	if prompts := s.choicePromptViews(ctx, msgs); len(prompts) > 0 {
		resp.MessageDecisions = make(map[string][]DecisionView, len(prompts))
		for id, v := range prompts {
			resp.MessageDecisions[id] = v.Decisions
			if v.Intro != "" {
				if resp.MessageChoiceIntros == nil {
					resp.MessageChoiceIntros = map[string]string{}
				}
				resp.MessageChoiceIntros[id] = v.Intro
			}
		}
	}
	if responses := s.choiceResponseViews(ctx, msgs); len(responses) > 0 {
		resp.MessageChoiceResponses = responses
	}
}

// choiceEventPayload is what a chat message event carries for a choice
// prompt or response. The handler that stores the message passes it to
// PublishUserMessage and the member fan-out (withChoiceEvent), so the
// event publisher needs no store.
type choiceEventPayload struct {
	Prompt   *ChoicePromptView
	Response *ChoiceResponseView
}

// withChoiceEvent makes the chat events of a published message carry p.
func withChoiceEvent(p *choiceEventPayload) ChatEventOption {
	return func(o *chatEventOptions) { o.choice = p }
}
