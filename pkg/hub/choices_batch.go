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
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Set answers (design choices v5, 4.2 and 4.3): a choice prompt with two or
// more decisions is answered as one set.

const choicePromptsRoutePrefix = "/api/v1/choice-prompts/"

// Error codes of the set answer endpoint and the per-decision guard.
const (
	ErrCodeAnswerSetChanged    = "answer_set_changed"
	ErrCodeIdempotencyMismatch = "idempotency_mismatch"
	ErrCodeBatchRequired       = "batch_required"
)

// ChoiceSetAnswerRequest is the body of POST
// /api/v1/choice-prompts/{messageId}/answers. The client sends choice ids
// only; labels come from the stored rows.
type ChoiceSetAnswerRequest struct {
	IdempotencyKey string                `json:"idempotency_key,omitempty"`
	Answers        []ChoiceSetAnswerItem `json:"answers"`
}

// ChoiceSetAnswerItem is one answer of a set submit.
type ChoiceSetAnswerItem struct {
	DecisionRowID string `json:"decision_row_id"`
	ChoiceID      string `json:"choice_id"`
}

// ChoiceSetAnswerResponse is the 200 body of a set submit: the one
// choice-response message, the batch and every decision of the prompt.
type ChoiceSetAnswerResponse struct {
	ResponseMessageID string         `json:"responseMessageId"`
	BatchID           string         `json:"batchId"`
	Decisions         []DecisionView `json:"decisions"`
	Replayed          bool           `json:"replayed,omitempty"`
}

// handleChoicePromptRoutes serves the choice prompt endpoints, 404 while
// web.chat_choices is off:
//
//	POST /api/v1/choice-prompts/{messageId}/answers  answer every decision as one set
func (s *Server) handleChoicePromptRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.ChatChoices, func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, choicePromptsRoutePrefix)
		msgID, sub, _ := strings.Cut(rest, "/")
		if msgID == "" || sub != "answers" {
			NotFound(w, "Choice prompt")
			return
		}
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleChoiceSetAnswer(w, r, msgID)
	})(w, r)
}

// loadAuthorizedChoicePrompt reads the prompt message and its decisions and
// authorizes the caller to send in its conversation (design v5 4.2 steps 1
// and 2). A missing message, one that is not a choice prompt, a prompt with
// no decisions and a refused caller all answer 404.
func (s *Server) loadAuthorizedChoicePrompt(w http.ResponseWriter, ctx context.Context, user UserIdentity, msgID string) (*store.Message, []*store.Decision, bool) {
	m, err := s.store.GetMessage(ctx, msgID)
	if err != nil || m == nil || m.Type != messages.TypeChoicePrompt || m.ThreadID == "" {
		NotFound(w, "Choice prompt")
		return nil, nil, false
	}
	if _, serr := s.authorizeChatSend(ctx, user, m.ThreadID); serr != nil {
		if serr.Status == http.StatusNotFound || serr.accessRefused {
			NotFound(w, "Choice prompt")
			return nil, nil, false
		}
		serr.write(w)
		return nil, nil, false
	}
	rows, err := s.store.GetDecisionsByMessageIDs(ctx, []string{msgID})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return nil, nil, false
	}
	ds := rows[msgID]
	if len(ds) == 0 {
		NotFound(w, "Choice prompt")
		return nil, nil, false
	}
	return m, ds, true
}

// answerSetReasons is the actionable-set check of design v5 4.2 step 5 on
// the rows read: the reason of every requested row that is not actionable
// for userID, and "missing" for every actionable row not requested.
func answerSetReasons(ds []*store.Decision, userID string, requested map[string]bool) map[string]string {
	reasons := map[string]string{}
	for _, d := range ds {
		reason := store.DecisionActionableReason(d, userID)
		switch {
		case requested[d.ID] && reason != "":
			reasons[d.ID] = reason
		case !requested[d.ID] && reason == "":
			reasons[d.ID] = store.AnswerSetReasonMissing
		}
	}
	return reasons
}

// actionableViews returns the views of the rows of ds that userID may
// answer in a set submit, in position order.
func actionableViews(views []DecisionView, ds []*store.Decision, userID string) []DecisionView {
	ok := make(map[string]bool, len(ds))
	for _, d := range ds {
		ok[d.ID] = store.DecisionActionableReason(d, userID) == ""
	}
	out := []DecisionView{}
	for _, v := range views {
		if ok[v.RowID] {
			out = append(out, v)
		}
	}
	return out
}

// promptDecisionViews returns the views of every decision of the prompt m,
// in position order, read now.
func (s *Server) promptDecisionViews(ctx context.Context, m *store.Message) []DecisionView {
	if v := s.choicePromptViewOf(ctx, m); v != nil {
		return v.Decisions
	}
	return []DecisionView{}
}

// writeAnswerSetChanged answers 409 answer_set_changed with every fresh
// view of the prompt and the reason of every row that differs.
func (s *Server) writeAnswerSetChanged(w http.ResponseWriter, ctx context.Context, m *store.Message, reasons map[string]string) {
	writeError(w, http.StatusConflict, ErrCodeAnswerSetChanged,
		"the decisions to answer changed; refresh and submit again",
		map[string]interface{}{"decisions": s.promptDecisionViews(ctx, m), "reasons": reasons})
}

// handleChoiceSetAnswer serves POST /api/v1/choice-prompts/{messageId}/answers
// (design choices v5, 4.2).
func (s *Server) handleChoiceSetAnswer(w http.ResponseWriter, r *http.Request, msgID string) {
	ctx := r.Context()
	user := decisionCaller(w, r)
	if user == nil {
		return
	}
	var req ChoiceSetAnswerRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if len(req.IdempotencyKey) > store.MaxDecisionIdempotencyKeyLen {
		ValidationError(w, fmt.Sprintf("idempotency_key is at most %d bytes", store.MaxDecisionIdempotencyKeyLen),
			map[string]interface{}{"field": "idempotency_key"})
		return
	}

	// Steps 1 and 2: the prompt, its conversation, the caller's send rights.
	m, ds, ok := s.loadAuthorizedChoicePrompt(w, ctx, user, msgID)
	if !ok {
		return
	}
	byID := make(map[string]*store.Decision, len(ds))
	for _, d := range ds {
		byID[d.ID] = d
	}
	// Per-user answer rate limit: the chat send limiter, once per submit,
	// before the shape and state checks, so malformed and stale probes
	// count too.
	if !s.allowChatSend(w, user.ID(), chatSenderHuman) {
		return
	}

	// Step 4: the shape. Every row is in the prompt once and names one of
	// its choices; the labels are taken from the rows.
	if len(req.Answers) == 0 {
		ValidationError(w, "answers must name at least one decision", map[string]interface{}{"field": "answers"})
		return
	}
	requested := make(map[string]bool, len(req.Answers))
	choices := make(map[string]messages.Choice, len(req.Answers))
	for i, a := range req.Answers {
		field := fmt.Sprintf("answers[%d]", i)
		rowID := strings.TrimSpace(a.DecisionRowID)
		d, ok := byID[rowID]
		if !ok {
			ValidationError(w, fmt.Sprintf("decision row %q is not a decision of this prompt", rowID),
				map[string]interface{}{"field": field + ".decision_row_id"})
			return
		}
		if requested[rowID] {
			ValidationError(w, fmt.Sprintf("decision %s is answered twice", d.Key),
				map[string]interface{}{"field": field + ".decision_row_id"})
			return
		}
		choice, ok := d.Choice(strings.TrimSpace(a.ChoiceID))
		if !ok {
			ValidationError(w, fmt.Sprintf("choice %q is not a choice of decision %s", a.ChoiceID, d.Key),
				map[string]interface{}{"field": field + ".choice_id"})
			return
		}
		requested[rowID] = true
		choices[rowID] = choice
	}

	// Step 3: a replay of the key is answered before any state check; the
	// key, scoped to the user and the prompt, used for other answers is
	// 422. Another user's answers under the same key are not this user's.
	if req.IdempotencyKey != "" {
		if prior := setAnswersWithIdempotencyKey(ds, req.IdempotencyKey, user.ID()); len(prior) > 0 {
			if !sameSetAnswers(prior, user.ID(), choices) {
				writeError(w, http.StatusUnprocessableEntity, ErrCodeIdempotencyMismatch,
					"the idempotency key was used for other answers", nil)
				return
			}
			writeJSON(w, http.StatusOK, ChoiceSetAnswerResponse{
				ResponseMessageID: prior[0].ResponseMessageID, BatchID: prior[0].BatchID,
				Decisions: s.promptDecisionViews(ctx, m), Replayed: true,
			})
			return
		}
	}

	// Step 5 on the rows read (the store checks again under the row
	// locks): the request must equal the actionable set.
	if reasons := answerSetReasons(ds, user.ID(), requested); len(reasons) > 0 {
		s.writeAnswerSetChanged(w, ctx, m, reasons)
		return
	}
	// Outcomes: a row with the user's own interpreted answer is a confirm
	// (same choice) or an override (another choice).
	items := make([]store.AnswerSetItem, 0, len(ds))
	overridden := map[string]*store.DecisionAnswer{}
	allConfirm := true
	for _, d := range ds {
		if !requested[d.ID] {
			continue
		}
		choice := choices[d.ID]
		outcome := store.DecisionOutcomeAnswer
		if eff := d.EffectiveAnswer(); eff != nil && eff.Provenance == store.DecisionProvenanceAgentInterpreted {
			outcome = store.DecisionOutcomeConfirm
			if eff.ChoiceID != choice.ID {
				outcome = store.DecisionOutcomeOverride
				overridden[d.ID] = eff
			}
		}
		if outcome != store.DecisionOutcomeConfirm {
			allConfirm = false
		}
		items = append(items, store.AnswerSetItem{DecisionRowID: d.ID, ChoiceID: choice.ID, Outcome: outcome})
	}

	asker, askerGone, ok := s.choiceAsker(w, ctx, ds[0].AskerAgentID)
	if !ok {
		return
	}
	// The choice-response is a message from the user to the asking agent:
	// the chat send's messaging check, unless nothing is delivered (a gone
	// asker or an all-confirm set).
	if !askerGone && !allConfirm {
		if allowed, reason, _ := s.authorizeAgentMessage(ctx, user, asker, false); !allowed {
			writeError(w, http.StatusForbidden, ErrCodeMessageDenied, "Message delivery denied", map[string]interface{}{
				"reason":        mapReasonToCode(reason),
				"senderMode":    "user",
				"recipientMode": asker.MessageMode,
			})
			return
		}
	}
	responseMsg := choiceSetResponseStoreMessage(user, ds, m, requested, choices, asker, askerGone)

	// Steps 5 to 7 in one transaction.
	res, err := s.store.RecordAnswerSet(ctx, &store.RecordAnswerSetInput{
		MessageID:        m.ID,
		AnsweredByUserID: user.ID(),
		AnsweredByName:   user.DisplayName(),
		AnsweredByEmail:  user.Email(),
		Answers:          items,
		ResponseMessage:  responseMsg,
		IdempotencyKey:   req.IdempotencyKey,
	})
	var changed *store.AnswerSetChangedError
	switch {
	case err == nil:
	case errors.As(err, &changed):
		s.writeAnswerSetChanged(w, ctx, m, changed.Reasons)
		return
	case errors.Is(err, store.ErrIdempotencyMismatch):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeIdempotencyMismatch,
			"the idempotency key was used for other answers", nil)
		return
	case errors.Is(err, store.ErrInvalidInput):
		ValidationError(w, err.Error(), nil)
		return
	case errors.Is(err, store.ErrNotFound):
		NotFound(w, "Choice prompt")
		return
	default:
		writeErrorFromErr(w, err, "")
		return
	}

	views := s.promptDecisionViews(ctx, m)
	if res.Replayed {
		writeJSON(w, http.StatusOK, ChoiceSetAnswerResponse{
			ResponseMessageID: firstResponseID(res), BatchID: res.BatchID, Decisions: views, Replayed: true,
		})
		return
	}

	// Step 8, after commit: the response row's chat events, the delivery
	// to the asker (not for an all-confirm set: the agent already acted on
	// those answers), then choice.updated once per answered row, in
	// position order.
	setView := choiceResponseSetView(m.ID, res.BatchID, ds, res.Answers)
	s.publishChoiceResponseSet(ctx, responseMsg, setView)
	if !askerGone && !allConfirm {
		s.deliverChoiceSetResponse(ctx, asker, responseMsg, m.ID, res, overridden)
	}
	for _, v := range views {
		if requested[v.RowID] {
			s.publishChoiceUpdated(ctx, m, v)
		}
	}
	writeJSON(w, http.StatusOK, ChoiceSetAnswerResponse{
		ResponseMessageID: responseMsg.ID, BatchID: res.BatchID, Decisions: views,
	})
}

// firstResponseID is the response message of a recorded set.
func firstResponseID(res *store.AnswerSetResult) string {
	if len(res.Answers) == 0 {
		return ""
	}
	return res.Answers[0].ResponseMessageID
}

// setAnswersWithIdempotencyKey returns the set answers userID recorded on ds
// under key, in position order (the key is scoped to the user and the
// prompt).
func setAnswersWithIdempotencyKey(ds []*store.Decision, key, userID string) []*store.DecisionAnswer {
	var out []*store.DecisionAnswer
	for _, d := range ds {
		for _, a := range d.Answers {
			if a.IdempotencyKey == key && a.BatchID != "" && a.AnsweredByUserID == userID {
				out = append(out, a)
			}
		}
	}
	return out
}

// sameSetAnswers reports whether the recorded set is userID's answers with
// exactly the requested choices.
func sameSetAnswers(prior []*store.DecisionAnswer, userID string, choices map[string]messages.Choice) bool {
	if len(prior) != len(choices) {
		return false
	}
	batch := prior[0].BatchID
	for _, a := range prior {
		c, ok := choices[a.DecisionRowID]
		if !ok || c.ID != a.ChoiceID || a.AnsweredByUserID != userID || a.BatchID != batch {
			return false
		}
	}
	return true
}

// choiceSetResponseStoreMessage builds the one choice-response row of a set
// submit (design v5 5.1): sender the answering user, recipient the asking
// agent, in the prompt's conversation, its body one line per answer in
// position order. A gone asker (askerGone; asker is then unused) or one
// that cannot take a message gets the row born failed, as for a one-click
// answer.
func choiceSetResponseStoreMessage(user UserIdentity, ds []*store.Decision, prompt *store.Message, requested map[string]bool, choices map[string]messages.Choice, asker *store.Agent, askerGone bool) *store.Message {
	var parts []string
	for _, d := range ds {
		if requested[d.ID] {
			parts = append(parts, d.Key+" "+plainLine(choices[d.ID].Label))
		}
	}
	noun := "decisions"
	if len(parts) == 1 {
		noun = "decision"
	}
	m := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      prompt.ProjectID,
		Sender:         userSenderAddress(user),
		SenderID:       user.ID(),
		RecipientID:    ds[0].AskerAgentID,
		Msg:            fmt.Sprintf("%s answered %d %s: %s", userDisplay(user), len(parts), noun, strings.Join(parts, " · ")),
		Type:           messages.TypeChoiceResponse,
		AgentID:        ds[0].AskerAgentID,
		Channel:        "web",
		ThreadID:       prompt.ThreadID,
		ConversationID: prompt.ConversationID,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	if askerGone {
		m.Recipient = "agent:" + ds[0].AskerAgentName
		reason := agentGoneReason
		m.DispatchState = store.MessageDispatchFailed
		m.DispatchFailureReason = &reason
	} else {
		m.Recipient = "agent:" + asker.Slug
		if unreachable, why := isAgentUnreachable(asker); unreachable {
			reason := agentUnreachableReason(why)
			m.DispatchState = store.MessageDispatchFailed
			m.DispatchFailureReason = &reason
		}
	}
	return m
}

// choiceResponseSetView builds the choiceResponseSet of a set's response
// row from its answers, in the position order of the prompt's rows ds.
func choiceResponseSetView(promptID, batchID string, ds []*store.Decision, answers []*store.DecisionAnswer) *ChoiceResponseSetView {
	byRow := make(map[string]*store.DecisionAnswer, len(answers))
	for _, a := range answers {
		byRow[a.DecisionRowID] = a
	}
	v := &ChoiceResponseSetView{PromptMessageID: promptID, BatchID: batchID, Answers: []ChoiceResponseSetAnswer{}}
	for _, d := range ds {
		a, ok := byRow[d.ID]
		if !ok {
			continue
		}
		v.Answers = append(v.Answers, ChoiceResponseSetAnswer{
			RowID:          d.ID,
			Ref:            d.Ref(),
			Key:            a.Key,
			Version:        a.Version,
			Question:       a.QuestionText,
			ChoiceID:       a.ChoiceID,
			Label:          a.ChoiceLabel,
			Provenance:     a.Provenance,
			Outcome:        a.Outcome,
			MatchedDefault: a.MatchedDefault,
		})
	}
	return v
}

// publishChoiceResponseSet emits the chat events of the stored set response,
// carrying its ChoiceResponseSetView.
func (s *Server) publishChoiceResponseSet(ctx context.Context, m *store.Message, view *ChoiceResponseSetView) {
	pubCtx := context.WithoutCancel(ctx)
	opt := withChoiceEvent(&choiceEventPayload{ResponseSet: view})
	s.touchConversationActivity(pubCtx, m.ThreadID, m.ID)
	s.events.PublishUserMessage(pubCtx, m, nil, nil, opt)
	if !strings.HasPrefix(m.ThreadID, "dm:") {
		s.fanOutThreadMessageToMembersAsync(pubCtx, m, nil, nil, opt)
	}
}

// choiceSetAgentLines is the hub-formatted text delivered to the asking agent
// for a set (design v5 5.1): a header, then one line per answer in position
// order. The questions are the agent's own text, truncated.
func choiceSetAgentLines(promptID string, answers []*store.DecisionAnswer, overridden map[string]*store.DecisionAnswer) string {
	who := ""
	if len(answers) > 0 {
		who = answers[0].AnsweredByEmail
		if who == "" {
			who = answers[0].AnsweredByName
		}
	}
	noun := "decisions"
	if len(answers) == 1 {
		noun = "decision"
	}
	lines := []string{fmt.Sprintf("[choice-response] %d %s answered by %s (prompt %s)", len(answers), noun, who, promptID)}
	for _, a := range answers {
		notes := []string{"choice " + a.ChoiceID}
		if a.MatchedDefault {
			notes = append(notes, "recommended")
		}
		switch a.Outcome {
		case store.DecisionOutcomeConfirm:
			notes = append(notes, "confirmed")
		case store.DecisionOutcomeOverride:
			if o := overridden[a.DecisionRowID]; o != nil {
				notes = append(notes, "overriding interpreted answer "+o.ChoiceLabel)
			} else {
				notes = append(notes, "overriding interpreted answer")
			}
		}
		lines = append(lines, fmt.Sprintf("%s v%d (%q) answer=%s (%s)",
			a.Key, a.Version, truncateRunes(plainLine(a.QuestionText), choiceResponseQuestionMax), a.ChoiceLabel, strings.Join(notes, ", ")))
	}
	return strings.Join(lines, "\n")
}

// choiceAnswersMetadata is the compact choice_answers value of a set
// response: key@version=choiceId, comma-separated, in position order (at
// most 10 entries, one per decision).
func choiceAnswersMetadata(answers []*store.DecisionAnswer) string {
	parts := make([]string, len(answers))
	for i, a := range answers {
		parts[i] = a.Key + "@" + strconv.Itoa(a.Version) + "=" + a.ChoiceID
	}
	return strings.Join(parts, ",")
}

// deliverChoiceSetResponse dispatches a set's response to the asking agent:
// one message with a line per answer and the hub-set choice_batch and
// choice_answers metadata. A stopped or deleted asker leaves the row failed;
// the answers stand either way.
func (s *Server) deliverChoiceSetResponse(ctx context.Context, asker *store.Agent, m *store.Message, promptID string, res *store.AnswerSetResult, overridden map[string]*store.DecisionAnswer) {
	if asker == nil || m.DispatchState != store.MessageDispatchDispatched {
		return
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}
	text := choiceSetAgentLines(promptID, res.Answers, overridden)
	sm := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      m.CreatedAt.UTC().Format(time.RFC3339),
		Sender:         m.Sender,
		SenderID:       m.SenderID,
		Recipient:      m.Recipient,
		RecipientID:    m.RecipientID,
		Msg:            text,
		Type:           messages.TypeChoiceResponse,
		Channel:        m.Channel,
		ThreadID:       m.ThreadID,
		ConversationID: m.ConversationID,
	}
	// Strip the reserved keys first, as every dispatch site does; the set
	// keys are reserved and only the hub sets them, after the strip.
	sm.Metadata = messaging.StripReservedMetadata(sm.Metadata)
	if sm.Metadata == nil {
		sm.Metadata = make(map[string]string, 2)
	}
	sm.Metadata[messages.MetadataChoiceBatch] = res.BatchID
	sm.Metadata[messages.MetadataChoiceAnswers] = choiceAnswersMetadata(res.Answers)
	dctx, cancel := context.WithTimeout(withDispatchMessageID(context.WithoutCancel(ctx), m.ID), 30*time.Second)
	defer cancel()
	if err := dispatchWithBrokerRetry(dctx, s.recordLog(), dispatcher, asker, text, false, sm); err != nil {
		s.messageLog.Error("choices: failed to deliver set choice-response", "agent", asker.Slug, "message_id", m.ID, "error", err)
		_ = s.markFailed(context.WithoutCancel(ctx), m.ID, err.Error())
	}
}

// writeBatchRequired answers 409 batch_required for a per-decision answer to
// a row of a set (design v5 4.3): the prompt id and the rows the caller may
// answer in a set submit.
func (s *Server) writeBatchRequired(w http.ResponseWriter, ctx context.Context, user UserIdentity, prompt *store.Message, ds []*store.Decision) {
	writeError(w, http.StatusConflict, ErrCodeBatchRequired,
		fmt.Sprintf("this prompt has %d decisions; answer them together with POST %s%s/answers", len(ds), choicePromptsRoutePrefix, prompt.ID),
		map[string]interface{}{
			"promptMessageId": prompt.ID,
			"actionable":      actionableViews(s.promptDecisionViews(ctx, prompt), ds, user.ID()),
		})
}
