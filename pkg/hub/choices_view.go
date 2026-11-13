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
	"sort"
	"strings"
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
	ChoiceID             string `json:"choiceId"`
	Label                string `json:"label"`
	AnsweredByUserID     string `json:"answeredByUserId,omitempty"`
	AnsweredByName       string `json:"answeredByName,omitempty"`
	AnsweredByEmail      string `json:"answeredByEmail,omitempty"`
	Provenance           string `json:"provenance"`
	Outcome              string `json:"outcome,omitempty"`
	InterpretedByAgentID string `json:"interpretedByAgentId,omitempty"`
	SourceMessageID      string `json:"sourceMessageId,omitempty"`
	Surface              string `json:"surface"`
	ResponseMessageID    string `json:"responseMessageId"`
	// BatchID is set on every answer of one set submit; MatchedDefault when
	// the answer is the decision's recommended choice (design v5 5.2).
	BatchID             string    `json:"batchId,omitempty"`
	MatchedDefault      bool      `json:"matchedDefault,omitempty"`
	PreviousChoiceLabel string    `json:"previousChoiceLabel,omitempty"`
	At                  time.Time `json:"at"`
}

// ChoicePromptView is what a choice-prompt row gains in history and SSE: the
// intro (the agent's body without the markdown fallback, sliced by the hub
// so clients never slice msg by a byte offset) and the decisions.
type ChoicePromptView struct {
	Intro     string         `json:"intro"`
	Decisions []DecisionView `json:"decisions"`
	// AnswerMode is "set" for a prompt with two or more decisions (one
	// submit answers them all) and "single" otherwise, computed by the hub
	// (design v5 5.2).
	AnswerMode string `json:"answerMode"`
}

// Choice prompt answer modes.
const (
	ChoiceAnswerModeSingle = "single"
	ChoiceAnswerModeSet    = "set"
)

// choiceAnswerMode is the answer mode of a prompt with n decisions.
func choiceAnswerMode(n int) string {
	if n >= 2 {
		return ChoiceAnswerModeSet
	}
	return ChoiceAnswerModeSingle
}

// ChoiceResponseSetView is what the choice-response row of a set submit
// gains in history and SSE (design v5 5.1): every answer, in position
// order. PromptMessageID is omitted when an interpreted batch spans multiple
// prompt messages; each answer carries its own PromptMessageID.
type ChoiceResponseSetView struct {
	PromptMessageID string                    `json:"promptMessageId,omitempty"`
	BatchID         string                    `json:"batchId"`
	Answers         []ChoiceResponseSetAnswer `json:"answers"`
}

// ChoiceResponseSetAnswer is one answer of a ChoiceResponseSetView.
type ChoiceResponseSetAnswer struct {
	RowID                string `json:"rowId"`
	Ref                  string `json:"ref"`
	Key                  string `json:"key"`
	Version              int    `json:"version"`
	Question             string `json:"question"`
	ChoiceID             string `json:"choiceId"`
	Label                string `json:"label"`
	Provenance           string `json:"provenance"`
	Outcome              string `json:"outcome"`
	MatchedDefault       bool   `json:"matchedDefault"`
	AnsweredByUserID     string `json:"answeredByUserId,omitempty"`
	AnsweredByName       string `json:"answeredByName,omitempty"`
	InterpretedByAgentID string `json:"interpretedByAgentId,omitempty"`
	SourceMessageID      string `json:"sourceMessageId,omitempty"`
	PromptMessageID      string `json:"promptMessageId,omitempty"`
	PreviousChoiceLabel  string `json:"previousChoiceLabel,omitempty"`
}

// ChoiceResponseView is what a choice-response row gains in history and SSE.
type ChoiceResponseView struct {
	RowID                string `json:"rowId"`
	Ref                  string `json:"ref"`
	Key                  string `json:"key"`
	Version              int    `json:"version"`
	Question             string `json:"question"`
	ChoiceID             string `json:"choiceId"`
	Label                string `json:"label"`
	Provenance           string `json:"provenance"`
	Outcome              string `json:"outcome,omitempty"`
	AnsweredByUserID     string `json:"answeredByUserId,omitempty"`
	AnsweredByName       string `json:"answeredByName,omitempty"`
	InterpretedByAgentID string `json:"interpretedByAgentId,omitempty"`
	PromptMessageID      string `json:"promptMessageId"`
	SourceMessageID      string `json:"sourceMessageId,omitempty"`
	PreviousChoiceLabel  string `json:"previousChoiceLabel,omitempty"`
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
		BatchID:              a.BatchID,
		MatchedDefault:       a.MatchedDefault,
		At:                   a.CreatedAt,
	}
}

// decisionView converts a stored decision. replacedBy, when the decision is
// replaced, is the row that replaced it (nil when it could not be read).
func decisionView(d *store.Decision, replacedBy *store.Decision) DecisionView {
	eff := d.EffectiveAnswer()
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
		Decision:        answerView(eff),
	}
	if v.Decision != nil {
		v.Decision.PreviousChoiceLabel = previousChoiceLabel(eff, d)
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
		view := ChoicePromptView{Decisions: make([]DecisionView, 0, len(ds)), AnswerMode: choiceAnswerMode(len(ds))}
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

// choiceResponseViews returns, keyed by message id, the ChoiceResponseView
// of every one-click choice-response message among msgs and the
// ChoiceResponseSetView of every set response (its answers share a batch).
func (s *Server) choiceResponseViews(ctx context.Context, msgs []*store.Message) (map[string]ChoiceResponseView, map[string]ChoiceResponseSetView) {
	var ids []string
	for _, m := range msgs {
		if m != nil && m.Type == messages.TypeChoiceResponse {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	byResponse, err := s.store.GetDecisionAnswersByResponseMessageIDs(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "choices: answers unreadable for history", "error", err)
		return nil, nil
	}
	var rowIDs []string
	for _, as := range byResponse {
		for _, a := range as {
			rowIDs = append(rowIDs, a.DecisionRowID)
		}
	}
	prompts := s.decisionRows(ctx, rowIDs)
	// When any response row overrides an interpreted answer, load its
	// prompt's decisions with answers so PreviousChoiceLabel is populated
	// on history reload just as on live SSE.
	var overrideMsgIDs []string
	seenMsgID := map[string]bool{}
	for _, as := range byResponse {
		for _, a := range as {
			if a.Outcome != store.DecisionOutcomeOverride {
				continue
			}
			if d := prompts[a.DecisionRowID]; d != nil && d.MessageID != "" && !seenMsgID[d.MessageID] {
				seenMsgID[d.MessageID] = true
				overrideMsgIDs = append(overrideMsgIDs, d.MessageID)
			}
		}
	}
	if len(overrideMsgIDs) > 0 {
		if withAnswers, err := s.store.GetDecisionsByMessageIDs(ctx, overrideMsgIDs); err != nil {
			slog.WarnContext(ctx, "choices: override decisions unreadable for history", "error", err)
		} else {
			for _, ds := range withAnswers {
				for _, d := range ds {
					prompts[d.ID] = d
				}
			}
		}
	}
	out := map[string]ChoiceResponseView{}
	var sets map[string]ChoiceResponseSetView
	for id, as := range byResponse {
		if len(as) == 0 {
			continue
		}
		if as[0].BatchID != "" {
			if sets == nil {
				sets = map[string]ChoiceResponseSetView{}
			}
			sets[id] = choiceResponseSetViewOf(as, prompts)
			continue
		}
		a := as[0]
		out[id] = choiceResponseViewOf(a, prompts[a.DecisionRowID])
	}
	if len(out) == 0 {
		out = nil
	}
	return out, sets
}

// answererDisplayName returns the stored display name of an answerer, falling
// back to their email snapshot when no display name was recorded.
func answererDisplayName(a *store.DecisionAnswer) string {
	if a == nil {
		return ""
	}
	if name := strings.TrimSpace(a.AnsweredByName); name != "" {
		return name
	}
	return a.AnsweredByEmail
}

// previousChoiceLabel returns the label of d's interpreted answer when a
// overrides it, or "".
func previousChoiceLabel(a *store.DecisionAnswer, d *store.Decision) string {
	if a == nil || a.Outcome != store.DecisionOutcomeOverride || d == nil {
		return ""
	}
	for _, prev := range d.Answers {
		if prev.Provenance == store.DecisionProvenanceAgentInterpreted {
			return prev.ChoiceLabel
		}
	}
	return ""
}

// choiceResponseViewOf builds the ChoiceResponseView of one answer and its
// decision row (d may be nil if unreadable).
func choiceResponseViewOf(a *store.DecisionAnswer, d *store.Decision) ChoiceResponseView {
	v := ChoiceResponseView{
		RowID:                a.DecisionRowID,
		Ref:                  a.AskerAgentID + "/" + a.Key,
		Key:                  a.Key,
		Version:              a.Version,
		Question:             a.QuestionText,
		ChoiceID:             a.ChoiceID,
		Label:                a.ChoiceLabel,
		Provenance:           a.Provenance,
		Outcome:              a.Outcome,
		AnsweredByUserID:     a.AnsweredByUserID,
		AnsweredByName:       answererDisplayName(a),
		InterpretedByAgentID: a.InterpretedByAgentID,
		SourceMessageID:      a.SourceMessageID,
		PreviousChoiceLabel:  previousChoiceLabel(a, d),
	}
	if d != nil {
		v.RowID = d.ID
		v.Ref = d.Ref()
		v.PromptMessageID = d.MessageID
	}
	return v
}

// choiceResponseSetAnswerOf builds one item of a ChoiceResponseSetView from a
// stored answer and its decision row (d may be nil if unreadable).
func choiceResponseSetAnswerOf(a *store.DecisionAnswer, d *store.Decision) ChoiceResponseSetAnswer {
	item := ChoiceResponseSetAnswer{
		RowID:                a.DecisionRowID,
		Ref:                  a.AskerAgentID + "/" + a.Key,
		Key:                  a.Key,
		Version:              a.Version,
		Question:             a.QuestionText,
		ChoiceID:             a.ChoiceID,
		Label:                a.ChoiceLabel,
		Provenance:           a.Provenance,
		Outcome:              a.Outcome,
		MatchedDefault:       a.MatchedDefault,
		AnsweredByUserID:     a.AnsweredByUserID,
		AnsweredByName:       answererDisplayName(a),
		InterpretedByAgentID: a.InterpretedByAgentID,
		SourceMessageID:      a.SourceMessageID,
		PreviousChoiceLabel:  previousChoiceLabel(a, d),
	}
	if d != nil {
		item.RowID = d.ID
		item.Ref = d.Ref()
		item.PromptMessageID = d.MessageID
	}
	return item
}

// choiceResponseSetViewOf builds a set response's view from its answers,
// ordered by prompt creation time and position (read in prompts). When the
// batch spans multiple prompt messages, PromptMessageID on the envelope is
// left empty and each answer carries its own PromptMessageID.
func choiceResponseSetViewOf(as []*store.DecisionAnswer, prompts map[string]*store.Decision) ChoiceResponseSetView {
	sorted := append([]*store.DecisionAnswer(nil), as...)
	sort.SliceStable(sorted, func(i, j int) bool {
		di, dj := prompts[sorted[i].DecisionRowID], prompts[sorted[j].DecisionRowID]
		if di != nil && dj != nil && di.MessageID != dj.MessageID {
			if !di.CreatedAt.Equal(dj.CreatedAt) {
				return di.CreatedAt.Before(dj.CreatedAt)
			}
			return di.MessageID < dj.MessageID
		}
		pi, pj := 0, 0
		if di != nil {
			pi = di.Position
		}
		if dj != nil {
			pj = dj.Position
		}
		return pi < pj
	})
	v := ChoiceResponseSetView{BatchID: sorted[0].BatchID, Answers: make([]ChoiceResponseSetAnswer, 0, len(sorted))}
	multiPrompt := false
	for _, a := range sorted {
		item := choiceResponseSetAnswerOf(a, prompts[a.DecisionRowID])
		if item.PromptMessageID != "" {
			if v.PromptMessageID == "" && !multiPrompt {
				v.PromptMessageID = item.PromptMessageID
			} else if v.PromptMessageID != item.PromptMessageID {
				v.PromptMessageID = ""
				multiPrompt = true
			}
		}
		v.Answers = append(v.Answers, item)
	}
	return v
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
			if resp.MessageChoiceAnswerModes == nil {
				resp.MessageChoiceAnswerModes = map[string]string{}
			}
			resp.MessageChoiceAnswerModes[id] = v.AnswerMode
		}
	}
	responses, sets := s.choiceResponseViews(ctx, msgs)
	if len(responses) > 0 {
		resp.MessageChoiceResponses = responses
	}
	if len(sets) > 0 {
		resp.MessageChoiceResponseSets = sets
	}
}

// choiceEventPayload is what a chat message event carries for a choice
// prompt or response. The handler that stores the message passes it to
// PublishUserMessage and the member fan-out (withChoiceEvent), so the
// event publisher needs no store.
type choiceEventPayload struct {
	Prompt      *ChoicePromptView
	Response    *ChoiceResponseView
	ResponseSet *ChoiceResponseSetView
}

// withChoiceEvent makes the chat events of a published message carry p.
func withChoiceEvent(p *choiceEventPayload) ChatEventOption {
	return func(o *chatEventOptions) { o.choice = p }
}
