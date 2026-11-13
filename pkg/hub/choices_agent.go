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

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Error codes of the agent decision endpoints (design choices v4/v5; plan phase 2, 5.1).
const (
	ErrCodeAgentsOnly              = "agents_only"
	ErrCodeDecisionNotFound        = "decision_not_found"
	ErrCodeSourceWrongConversation = "source_wrong_conversation"
	ErrCodeSourceNotUser           = "source_not_user"
	ErrCodeSourceBeforePrompt      = "source_before_prompt"
	ErrCodeSourceAuthorNotAllowed  = "source_author_not_allowed"
	ErrCodeInvalidChoice           = "invalid_choice"
)

const (
	agentDecisionsExactPath   = "/api/v1/agent/decisions"
	agentDecisionsRoutePrefix = "/api/v1/agent/decisions/"
)

// AgentDecisionListItem is one decision version in GET /api/v1/agent/decisions.
type AgentDecisionListItem struct {
	RowID           string      `json:"rowId"`
	Key             string      `json:"key"`
	Ref             string      `json:"ref"`
	Version         int         `json:"version"`
	Status          string      `json:"status"`
	Question        string      `json:"question"`
	ConversationID  string      `json:"conversationId"`
	PromptMessageID string      `json:"promptMessageId"`
	CreatedAt       time.Time   `json:"createdAt"`
	DecidedAt       *time.Time  `json:"decidedAt,omitempty"`
	Answer          *AnswerView `json:"answer,omitempty"`
}

// AgentDecisionListResponse is the 200 envelope of GET /api/v1/agent/decisions.
type AgentDecisionListResponse struct {
	Items      []AgentDecisionListItem `json:"items"`
	NextCursor string                  `json:"nextCursor,omitempty"`
}

// AgentDecisionVersionView is one version in GET /api/v1/agent/decisions/{key}.
// Answers contains every recorded answer row on this version (interpreted and
// direct), oldest first.
type AgentDecisionVersionView struct {
	RowID           string              `json:"rowId"`
	Key             string              `json:"key"`
	Ref             string              `json:"ref"`
	Version         int                 `json:"version"`
	Status          string              `json:"status"`
	Question        string              `json:"question"`
	ConversationID  string              `json:"conversationId"`
	PromptMessageID string              `json:"promptMessageId"`
	Choices         []messages.Choice   `json:"choices"`
	DefaultChoiceID string              `json:"defaultChoiceId,omitempty"`
	ReplacedBy      *DecisionReplacedBy `json:"replacedBy,omitempty"`
	CreatedAt       time.Time           `json:"createdAt"`
	DecidedAt       *time.Time          `json:"decidedAt,omitempty"`
	Answers         []AnswerView        `json:"answers"`
}

// AgentDecisionShowResponse is the 200 envelope of GET /api/v1/agent/decisions/{key}.
type AgentDecisionShowResponse struct {
	Key      string                     `json:"key"`
	Ref      string                     `json:"ref"`
	Versions []AgentDecisionVersionView `json:"versions"`
}

// InterpretedAnswerInput is one key=choice_id pair in POST /api/v1/agent/decisions/interpreted.
type InterpretedAnswerInput struct {
	Key      string `json:"key"`
	ChoiceID string `json:"choice_id"`
}

// RecordInterpretedRequest is the body of POST /api/v1/agent/decisions/interpreted.
type RecordInterpretedRequest struct {
	SourceMessageID string                   `json:"source_message_id"`
	IdempotencyKey  string                   `json:"idempotency_key,omitempty"`
	Answers         []InterpretedAnswerInput `json:"answers"`
}

// RecordInterpretedResponse is the 200 response of POST /api/v1/agent/decisions/interpreted.
type RecordInterpretedResponse struct {
	ResponseMessageID string         `json:"response_message_id"`
	BatchID           string         `json:"batch_id,omitempty"`
	Decisions         []DecisionView `json:"decisions"`
	Replayed          bool           `json:"replayed,omitempty"`
}

// handleAgentDecisionRoutes serves the agent decision endpoints, 404 while
// web.chat_choices is off:
//
//	GET  /api/v1/agent/decisions              list caller agent's decisions
//	GET  /api/v1/agent/decisions/{key}        show all versions of {key}
//	POST /api/v1/agent/decisions/interpreted  record interpreted answers
func (s *Server) handleAgentDecisionRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.ChatChoices, func(w http.ResponseWriter, r *http.Request) {
		agentIdent := agentDecisionCaller(w, r)
		if agentIdent == nil {
			return
		}
		path := r.URL.Path
		if path == agentDecisionsExactPath {
			if r.Method != http.MethodGet {
				MethodNotAllowed(w, http.MethodGet)
				return
			}
			s.handleAgentDecisionList(w, r, agentIdent)
			return
		}
		rest := strings.TrimPrefix(path, agentDecisionsRoutePrefix)
		seg, extra, hasSlash := strings.Cut(rest, "/")
		if seg == "" || hasSlash || extra != "" {
			NotFound(w, "Decision")
			return
		}
		if seg == "interpreted" && r.Method == http.MethodPost {
			s.handleAgentDecisionInterpreted(w, r, agentIdent)
			return
		}
		if seg == "interpreted" && r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
			return
		}
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleAgentDecisionShow(w, r, agentIdent, seg)
	})(w, r)
}

// agentDecisionCaller verifies the request carries an AgentIdentity (never a
// UserIdentity, session, visitor, delegated user credential, or broker).
func agentDecisionCaller(w http.ResponseWriter, r *http.Request) AgentIdentity {
	ctx := r.Context()
	agentIdent := GetAgentIdentityFromContext(ctx)
	if isNilIdentity(agentIdent) || GetUserIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeAgentsOnly, "this endpoint is only available to agents", nil)
		return nil
	}
	return agentIdent
}

// handleAgentDecisionList serves GET /api/v1/agent/decisions.
func (s *Server) handleAgentDecisionList(w http.ResponseWriter, r *http.Request, agentIdent AgentIdentity) {
	ctx := r.Context()
	q := r.URL.Query()

	var state store.DecisionListState
	switch s := q.Get("state"); s {
	case "", string(store.DecisionListStatePending):
		state = store.DecisionListStatePending
	case string(store.DecisionListStateDecided):
		state = store.DecisionListStateDecided
	case string(store.DecisionListStateAll):
		state = store.DecisionListStateAll
	default:
		ValidationError(w, fmt.Sprintf("invalid state %q: must be pending, decided, or all", s), nil)
		return
	}

	limit := store.DefaultDecisionListLimit
	if limitRaw := q.Get("limit"); limitRaw != "" {
		n, err := strconv.Atoi(limitRaw)
		if err != nil || n < 1 || n > store.MaxDecisionListLimit {
			ValidationError(w, fmt.Sprintf("limit must be an integer between 1 and %d", store.MaxDecisionListLimit), nil)
			return
		}
		limit = n
	}

	cursor := q.Get("cursor")
	rows, nextCursor, err := s.store.ListAgentDecisions(ctx, agentIdent.ID(), state, cursor, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, err.Error(), nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	convByPrompt := s.fallbackConversationsForDecisions(ctx, rows)
	items := make([]AgentDecisionListItem, len(rows))
	for i, d := range rows {
		convID := d.ConversationID
		if convID == "" {
			convID = convByPrompt[d.MessageID]
		}
		items[i] = AgentDecisionListItem{
			RowID:           d.ID,
			Key:             d.Key,
			Ref:             d.Ref(),
			Version:         d.Version,
			Status:          d.Status,
			Question:        d.Question,
			ConversationID:  convID,
			PromptMessageID: d.MessageID,
			CreatedAt:       d.CreatedAt,
			DecidedAt:       d.DecidedAt,
			Answer:          answerView(d.EffectiveAnswer()),
		}
	}

	writeJSON(w, http.StatusOK, AgentDecisionListResponse{
		Items:      items,
		NextCursor: nextCursor,
	})
}

// handleAgentDecisionShow serves GET /api/v1/agent/decisions/{key}.
func (s *Server) handleAgentDecisionShow(w http.ResponseWriter, r *http.Request, agentIdent AgentIdentity, key string) {
	ctx := r.Context()
	versions, err := s.store.GetAgentDecisionVersions(ctx, agentIdent.ID(), key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeDecisionNotFound,
				fmt.Sprintf("decision %q was not found", key),
				map[string]interface{}{"key": key})
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	var replacedByIDs []string
	for _, d := range versions {
		if d.ReplacedByID != "" {
			replacedByIDs = append(replacedByIDs, d.ReplacedByID)
		}
	}
	replacedBy := s.decisionRows(ctx, replacedByIDs)
	convByPrompt := s.fallbackConversationsForDecisions(ctx, versions)

	views := make([]AgentDecisionVersionView, len(versions))
	for i, d := range versions {
		convID := d.ConversationID
		if convID == "" {
			convID = convByPrompt[d.MessageID]
		}
		choices := d.Choices
		if choices == nil {
			choices = []messages.Choice{}
		}
		ansViews := make([]AnswerView, 0, len(d.Answers))
		for _, a := range d.Answers {
			if av := answerView(a); av != nil {
				ansViews = append(ansViews, *av)
			}
		}
		var rep *DecisionReplacedBy
		if r := replacedBy[d.ReplacedByID]; r != nil {
			rep = &DecisionReplacedBy{RowID: r.ID, MessageID: r.MessageID, Version: r.Version}
			if d.ReplacedAt != nil {
				rep.At = *d.ReplacedAt
			}
		}
		views[i] = AgentDecisionVersionView{
			RowID:           d.ID,
			Key:             d.Key,
			Ref:             d.Ref(),
			Version:         d.Version,
			Status:          d.Status,
			Question:        d.Question,
			ConversationID:  convID,
			PromptMessageID: d.MessageID,
			Choices:         choices,
			DefaultChoiceID: d.DefaultChoiceID,
			ReplacedBy:      rep,
			CreatedAt:       d.CreatedAt,
			DecidedAt:       d.DecidedAt,
			Answers:         ansViews,
		}
	}

	canonicalKey := versions[0].Key
	writeJSON(w, http.StatusOK, AgentDecisionShowResponse{
		Key:      canonicalKey,
		Ref:      versions[0].Ref(),
		Versions: views,
	})
}

func (s *Server) fallbackConversationsForDecisions(ctx context.Context, ds []*store.Decision) map[string]string {
	var missing []string
	seen := map[string]bool{}
	for _, d := range ds {
		if d.ConversationID == "" && d.MessageID != "" && !seen[d.MessageID] {
			seen[d.MessageID] = true
			missing = append(missing, d.MessageID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	msgs, err := s.store.GetMessagesByIDs(ctx, missing)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(msgs))
	for id, m := range msgs {
		if m == nil {
			continue
		}
		if m.ConversationID != "" {
			out[id] = m.ConversationID
		} else {
			out[id] = m.ThreadID
		}
	}
	return out
}

// decisionSurfaceFromChannel maps a source message's Channel to a store
// DecisionSurface value (plan phase 2, A6).
func decisionSurfaceFromChannel(ch string) string {
	switch strings.ToLower(strings.TrimSpace(ch)) {
	case "", "web", "native":
		return store.DecisionSurfaceNative
	case "gchat", "google_chat", "chat-app":
		return store.DecisionSurfaceGoogleChat
	case "slack":
		return store.DecisionSurfaceSlack
	case "discord":
		return store.DecisionSurfaceDiscord
	case "telegram":
		return store.DecisionSurfaceTelegram
	case "teams":
		return store.DecisionSurfaceTeams
	default:
		return store.DecisionSurfaceOther
	}
}

// handleAgentDecisionInterpreted serves POST /api/v1/agent/decisions/interpreted.
func (s *Server) handleAgentDecisionInterpreted(w http.ResponseWriter, r *http.Request, agentIdent AgentIdentity) {
	ctx := r.Context()
	agentID := agentIdent.ID()

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req RecordInterpretedRequest
	if err := readJSON(r, &req); err != nil {
		ValidationError(w, "invalid JSON body: "+err.Error(), nil)
		return
	}

	// Step 1: shape validation (1..10 pairs, no duplicate key, non-empty IDs).
	req.SourceMessageID = strings.TrimSpace(req.SourceMessageID)
	if req.SourceMessageID == "" {
		ValidationError(w, "source_message_id is required", nil)
		return
	}
	if len(req.IdempotencyKey) > store.MaxDecisionIdempotencyKeyLen {
		ValidationError(w, fmt.Sprintf("idempotency_key is at most %d bytes", store.MaxDecisionIdempotencyKeyLen), nil)
		return
	}
	if len(req.Answers) < 1 || len(req.Answers) > messages.MaxDecisionsPerMessage {
		ValidationError(w, fmt.Sprintf("answers must contain 1 to %d items", messages.MaxDecisionsPerMessage), nil)
		return
	}
	seenNorms := make(map[string]bool, len(req.Answers))
	for i := range req.Answers {
		req.Answers[i].Key = strings.TrimSpace(req.Answers[i].Key)
		req.Answers[i].ChoiceID = strings.TrimSpace(req.Answers[i].ChoiceID)
		if req.Answers[i].Key == "" || req.Answers[i].ChoiceID == "" {
			ValidationError(w, "each answer requires non-empty key and choice_id", nil)
			return
		}
		norm := messages.DecisionKeyNorm(req.Answers[i].Key)
		if seenNorms[norm] {
			ValidationError(w, fmt.Sprintf("duplicate decision key %q in request", req.Answers[i].Key), nil)
			return
		}
		seenNorms[norm] = true
	}

	// Step 2: resolve each key to the caller's active version (or, on an
	// idempotency retry after --update, the version carrying that key).
	resolved := make([]*store.Decision, 0, len(req.Answers))
	var missingKeys []string
	isReplayCandidate := false
	for _, item := range req.Answers {
		versions, err := s.store.GetAgentDecisionVersions(ctx, agentID, item.Key)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				missingKeys = append(missingKeys, item.Key)
				continue
			}
			writeErrorFromErr(w, err, "")
			return
		}
		var target *store.Decision
		if req.IdempotencyKey != "" {
			for _, v := range versions {
				for _, a := range v.Answers {
					if a.InterpretedByAgentID == agentID && a.IdempotencyKey == req.IdempotencyKey {
						target = v
						isReplayCandidate = true
						break
					}
				}
			}
		}
		if target == nil {
			for _, v := range versions {
				if v.Status != store.DecisionStatusReplaced && v.ReplacedByID == "" {
					target = v
				}
			}
		}
		if target == nil {
			// Every version was replaced without an active successor.
			target = versions[len(versions)-1]
		}
		resolved = append(resolved, target)
	}
	if len(missingKeys) > 0 {
		// If an idempotency key was supplied, check whether it was already used
		// on another decision of this agent (422 idempotency_mismatch takes
		// precedence over 404 when reusing a key for a nonexistent target).
		if req.IdempotencyKey != "" && isValidUUID(req.SourceMessageID) {
			if s.checkIdempotencyMismatchOnly(w, ctx, agentID, req) {
				return
			}
		}
		writeError(w, http.StatusNotFound, ErrCodeDecisionNotFound,
			fmt.Sprintf("decision %s was not found", strings.Join(missingKeys, ", ")),
			map[string]interface{}{"keys": missingKeys})
		return
	}

	// Step 3: source message validation (exists & same conversation ->
	// 400 source_wrong_conversation; user sender -> 400 source_not_user;
	// strictly after every involved prompt -> 400 source_before_prompt).
	if req.IdempotencyKey != "" && isValidUUID(req.SourceMessageID) && !isReplayCandidate {
		if s.checkIdempotencyMismatchOnly(w, ctx, agentID, req) {
			return
		}
	}

	sourceMsg, err := s.store.GetMessage(ctx, req.SourceMessageID)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalidInput), sourceMsg == nil && err == nil:
		writeError(w, http.StatusBadRequest, ErrCodeSourceWrongConversation,
			"source message was not found in the decision's conversation", nil)
		return
	case err != nil:
		writeErrorFromErr(w, err, "")
		return
	}

	promptIDs := make([]string, 0, len(resolved))
	seenPrompts := map[string]bool{}
	for _, d := range resolved {
		if !seenPrompts[d.MessageID] {
			seenPrompts[d.MessageID] = true
			promptIDs = append(promptIDs, d.MessageID)
		}
	}
	promptsByID, err := s.store.GetMessagesByIDs(ctx, promptIDs)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var commonThreadID string
	var firstPrompt *store.Message
	for _, d := range resolved {
		prompt := promptsByID[d.MessageID]
		if prompt == nil || prompt.ThreadID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeSourceWrongConversation,
				fmt.Sprintf("%s: prompt message has no conversation", d.Key),
				map[string]interface{}{"key": d.Key})
			return
		}
		if firstPrompt == nil {
			firstPrompt = prompt
			commonThreadID = prompt.ThreadID
		} else if prompt.ThreadID != commonThreadID {
			writeError(w, http.StatusBadRequest, ErrCodeSourceWrongConversation,
				fmt.Sprintf("%s: decisions span different conversations", d.Key),
				map[string]interface{}{"key": d.Key})
			return
		}
		if sourceMsg.ThreadID == "" || sourceMsg.ThreadID != prompt.ThreadID {
			writeError(w, http.StatusBadRequest, ErrCodeSourceWrongConversation,
				fmt.Sprintf("%s: source message is not in the decision's conversation", d.Key),
				map[string]interface{}{"key": d.Key})
			return
		}
		if d.ConversationID != "" && sourceMsg.ConversationID != "" && d.ConversationID != sourceMsg.ConversationID {
			writeError(w, http.StatusBadRequest, ErrCodeSourceWrongConversation,
				fmt.Sprintf("%s: source message is not in the decision's conversation", d.Key),
				map[string]interface{}{"key": d.Key})
			return
		}
	}

	if !strings.HasPrefix(sourceMsg.Sender, "user:") || sourceMsg.SenderID == "" || strings.HasPrefix(sourceMsg.Sender, "agent:") {
		writeError(w, http.StatusBadRequest, ErrCodeSourceNotUser,
			"source message must be authored by a user, not an agent", nil)
		return
	}

	for _, d := range resolved {
		prompt := promptsByID[d.MessageID]
		if !sourceMsg.CreatedAt.After(prompt.CreatedAt) {
			writeError(w, http.StatusBadRequest, ErrCodeSourceBeforePrompt,
				fmt.Sprintf("%s: source message was not created after the prompt message", d.Key),
				map[string]interface{}{"key": d.Key})
			return
		}
	}

	// Step 4: source author permission — authorizeChatSend(author, threadKey)
	// and, for a user-addressed DM row, author == addressed user.
	authorIdent, ok := s.loadSourceAuthorIdentity(w, ctx, sourceMsg.SenderID)
	if !ok {
		return
	}
	if strings.HasPrefix(commonThreadID, "dm:") && !isDMParticipant(commonThreadID, authorIdent.ID()) {
		writeError(w, http.StatusForbidden, ErrCodeSourceAuthorNotAllowed,
			"source message author is not the addressed user of this decision", nil)
		return
	}
	if _, serr := s.authorizeChatSend(ctx, authorIdent, commonThreadID); serr != nil {
		if serr.Status >= 500 {
			serr.write(w)
			return
		}
		writeError(w, http.StatusForbidden, ErrCodeSourceAuthorNotAllowed,
			"source message author is not allowed to answer in this conversation", nil)
		return
	}

	callerAgent, err := s.store.GetAgent(ctx, agentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErrorFromErr(w, err, "")
		return
	}
	agentSlug := resolved[0].AskerAgentName
	if callerAgent != nil && callerAgent.Slug != "" {
		agentSlug = callerAgent.Slug
	} else if callerAgent != nil && callerAgent.Name != "" {
		agentSlug = callerAgent.Name
	}
	if agentSlug == "" {
		agentSlug = agentID
	}

	authorName := userDisplay(authorIdent)
	convID := firstPrompt.ConversationID
	if convID == "" {
		convID = resolved[0].ConversationID
	}
	responseMsg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      firstPrompt.ProjectID,
		Sender:         "agent:" + agentSlug,
		SenderID:       agentID,
		Recipient:      userSenderAddress(authorIdent),
		RecipientID:    authorIdent.ID(),
		Type:           messages.TypeChoiceResponse,
		AgentID:        agentID,
		Channel:        "web",
		ThreadID:       commonThreadID,
		ConversationID: convID,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}

	pairs := make([]store.InterpretedAnswerPair, len(req.Answers))
	for i, a := range req.Answers {
		rowID := resolved[i].ID
		if isReplayCandidate {
			rowID = ""
		}
		pairs[i] = store.InterpretedAnswerPair{
			DecisionRowID: rowID,
			Key:           a.Key,
			ChoiceID:      a.ChoiceID,
		}
	}

	// Steps 5-8: atomic transaction (idempotency, row locks, pending + choice
	// validation, insert response message + answers, mark decided).
	res, err := s.store.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: agentID,
		SourceMessageID:      req.SourceMessageID,
		AnsweredByUserID:     authorIdent.ID(),
		AnsweredByName:       authorIdent.DisplayName(),
		AnsweredByEmail:      authorIdent.Email(),
		Surface:              decisionSurfaceFromChannel(sourceMsg.Channel),
		Answers:              pairs,
		ResponseMessage:      responseMsg,
		FormatMessage: func(items []store.InterpretedFormattedItem) string {
			return formatInterpretedResponseLine(agentSlug, authorName, items)
		},
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		s.writeInterpretedRecordError(w, ctx, err)
		return
	}

	updatedRows := make([]*store.Decision, len(res.Answers))
	views := make([]DecisionView, len(res.Answers))
	for i, a := range res.Answers {
		cur, gerr := s.store.GetDecision(ctx, a.DecisionRowID)
		if gerr != nil {
			writeErrorFromErr(w, gerr, "")
			return
		}
		updatedRows[i] = cur
		views[i] = s.decisionViewFor(ctx, cur)
	}

	if res.Replayed {
		writeJSON(w, http.StatusOK, RecordInterpretedResponse{
			ResponseMessageID: res.Answers[0].ResponseMessageID,
			BatchID:           res.BatchID,
			Decisions:         views,
			Replayed:          true,
		})
		return
	}

	// Publish the single choice-response to web SSE + member fan-out, and
	// choice.updated per row. Never deliver back to the asking agent.
	if len(res.Answers) == 1 {
		s.publishChoiceResponse(ctx, responseMsg, updatedRows[0], res.Answers[0])
	} else {
		setView := interpretedChoiceResponseSetView(res.BatchID, updatedRows, res.Answers)
		s.publishChoiceResponseSet(ctx, responseMsg, setView)
	}
	for i, v := range views {
		if prompt := promptsByID[updatedRows[i].MessageID]; prompt != nil {
			s.publishChoiceUpdated(ctx, prompt, v)
		}
	}

	writeJSON(w, http.StatusOK, RecordInterpretedResponse{
		ResponseMessageID: responseMsg.ID,
		BatchID:           res.BatchID,
		Decisions:         views,
	})
}

func isValidUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// checkIdempotencyMismatchOnly probes RecordInterpretedAnswers inside a
// read/rollback path when req.IdempotencyKey is set so a key reused with a
// different source_message_id or nonexistent decision key still answers 422
// idempotency_mismatch instead of 400/404.
func (s *Server) checkIdempotencyMismatchOnly(w http.ResponseWriter, ctx context.Context, agentID string, req RecordInterpretedRequest) bool {
	probePairs := make([]store.InterpretedAnswerPair, len(req.Answers))
	for i, a := range req.Answers {
		probePairs[i] = store.InterpretedAnswerPair{Key: a.Key, ChoiceID: a.ChoiceID}
	}
	var probeErr error
	sentinelRollback := errors.New("probe rollback")
	_ = s.store.WithTx(ctx, func(tx store.Store) error {
		_, probeErr = tx.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
			InterpretedByAgentID: agentID,
			SourceMessageID:      req.SourceMessageID,
			AnsweredByUserID:     "probe-user",
			Surface:              store.DecisionSurfaceNative,
			Answers:              probePairs,
			ResponseMessage: &store.Message{
				ID:            api.NewUUID(),
				ProjectID:     api.NewUUID(),
				DispatchState: store.MessageDispatchDispatched,
			},
			IdempotencyKey: req.IdempotencyKey,
		})
		return sentinelRollback
	})
	if errors.Is(probeErr, store.ErrIdempotencyMismatch) {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeIdempotencyMismatch, probeErr.Error(), nil)
		return true
	}
	return false
}

func (s *Server) loadSourceAuthorIdentity(w http.ResponseWriter, ctx context.Context, userID string) (UserIdentity, bool) {
	u, err := s.store.GetUser(ctx, userID)
	switch {
	case errors.Is(err, store.ErrNotFound), u == nil && err == nil:
		writeError(w, http.StatusForbidden, ErrCodeSourceAuthorNotAllowed,
			"source message author is not allowed to answer in this conversation", nil)
		return nil, false
	case err != nil:
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	if (u.Status != "" && u.Status != store.UserStatusActive) || (u.ExpiresAt != nil && !u.ExpiresAt.After(time.Now())) {
		writeError(w, http.StatusForbidden, ErrCodeSourceAuthorNotAllowed,
			"source message author is not allowed to answer in this conversation", nil)
		return nil, false
	}
	if u.IsFederated() {
		email := u.Email
		if u.FederatedEmail != nil && *u.FederatedEmail != "" {
			email = *u.FederatedEmail
		}
		issuer := ""
		if u.HomeIssuer != nil {
			issuer = *u.HomeIssuer
		}
		return NewVisitorIdentity(u.ID, issuer, u.ID, email, u.DisplayName), true
	}
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)), true
}

func formatInterpretedResponseLine(agentSlug, authorName string, items []store.InterpretedFormattedItem) string {
	if len(items) == 1 {
		return fmt.Sprintf("%s recorded %s's answer %s to %s, interpreted from their reply",
			agentSlug, authorName, plainLine(items[0].ChoiceLabel), items[0].Key)
	}
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = it.Key + " " + plainLine(it.ChoiceLabel)
	}
	return fmt.Sprintf("%s recorded %s's answers to %d decisions: %s, interpreted from their reply",
		agentSlug, authorName, len(items), strings.Join(parts, " · "))
}

func interpretedChoiceResponseSetView(batchID string, ds []*store.Decision, answers []*store.DecisionAnswer) *ChoiceResponseSetView {
	commonPromptID := ""
	multiPrompt := false
	for _, d := range ds {
		if d.MessageID == "" {
			continue
		}
		switch {
		case commonPromptID == "" && !multiPrompt:
			commonPromptID = d.MessageID
		case commonPromptID != d.MessageID:
			commonPromptID = ""
			multiPrompt = true
		}
	}
	v := &ChoiceResponseSetView{
		PromptMessageID: commonPromptID,
		BatchID:         batchID,
		Answers:         make([]ChoiceResponseSetAnswer, 0, len(answers)),
	}
	for i, a := range answers {
		d := ds[i]
		v.Answers = append(v.Answers, ChoiceResponseSetAnswer{
			RowID:           d.ID,
			Ref:             d.Ref(),
			Key:             a.Key,
			Version:         a.Version,
			Question:        a.QuestionText,
			ChoiceID:        a.ChoiceID,
			Label:           a.ChoiceLabel,
			Provenance:      a.Provenance,
			Outcome:         a.Outcome,
			MatchedDefault:  a.MatchedDefault,
			PromptMessageID: d.MessageID,
		})
	}
	return v
}

func (s *Server) writeInterpretedRecordError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	case errors.Is(err, store.ErrIdempotencyMismatch):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeIdempotencyMismatch, err.Error(), nil)
		return
	case errors.Is(err, store.ErrInvalidInput):
		ValidationError(w, err.Error(), nil)
		return
	}

	var iaErr *store.InterpretedAnswersError
	if !errors.As(err, &iaErr) || len(iaErr.Failures) == 0 {
		writeErrorFromErr(w, err, "")
		return
	}

	status := http.StatusBadRequest
	code := ErrCodeInvalidChoice
	hasAnswered, hasReplaced, hasNotFound := false, false, false
	for _, f := range iaErr.Failures {
		switch f.Reason {
		case store.InterpretedReasonAlreadyAnswered:
			hasAnswered = true
		case store.InterpretedReasonReplaced:
			hasReplaced = true
		case store.InterpretedReasonNotFound:
			hasNotFound = true
		}
	}
	switch {
	case hasAnswered:
		status = http.StatusConflict
		code = ErrCodeDecisionAnswered
	case hasReplaced:
		status = http.StatusConflict
		code = ErrCodeDecisionReplaced
	case hasNotFound:
		status = http.StatusNotFound
		code = ErrCodeDecisionNotFound
	}

	perKey := make([]map[string]interface{}, 0, len(iaErr.Failures))
	msgs := make([]string, 0, len(iaErr.Failures))
	var firstDecision *DecisionView
	var firstReplacedBy *DecisionView

	for _, f := range iaErr.Failures {
		msg := formatInterpretedFailureMessage(f)
		msgs = append(msgs, msg)
		entry := map[string]interface{}{
			"key":     f.Key,
			"code":    f.Reason,
			"message": msg,
		}
		if f.Decision != nil {
			dv := s.decisionViewFor(ctx, f.Decision)
			entry["decision"] = dv
			if firstDecision == nil && (f.Reason == store.InterpretedReasonAlreadyAnswered || f.Reason == store.InterpretedReasonReplaced) {
				dvCopy := dv
				firstDecision = &dvCopy
			}
			if f.Reason == store.InterpretedReasonReplaced && f.Decision.ReplacedByID != "" {
				if cur, gerr := s.store.GetDecision(ctx, f.Decision.ReplacedByID); gerr == nil && cur != nil {
					rv := s.decisionViewFor(ctx, cur)
					entry["replacedBy"] = rv
					if firstReplacedBy == nil {
						rvCopy := rv
						firstReplacedBy = &rvCopy
					}
				}
			}
		}
		perKey = append(perKey, entry)
	}

	details := map[string]interface{}{
		"errors": perKey,
	}
	if firstDecision != nil {
		details["decision"] = *firstDecision
	}
	if firstReplacedBy != nil {
		details["replacedBy"] = *firstReplacedBy
	}
	writeError(w, status, code, strings.Join(msgs, "; "), details)
}

func formatInterpretedFailureMessage(f store.InterpretedKeyFailure) string {
	switch f.Reason {
	case store.InterpretedReasonAlreadyAnswered:
		if f.Decision != nil {
			if eff := f.Decision.EffectiveAnswer(); eff != nil {
				who := eff.AnsweredByName
				if who == "" {
					who = eff.AnsweredByEmail
				}
				if who == "" {
					who = "a user"
				}
				if eff.Provenance == store.DecisionProvenanceAgentInterpreted {
					return fmt.Sprintf("%s: already answered (interpreted by you from %s's reply, %s)", f.Key, who, eff.ChoiceLabel)
				}
				switch eff.Outcome {
				case store.DecisionOutcomeConfirm:
					return fmt.Sprintf("%s: already answered (confirmed by %s, %s)", f.Key, who, eff.ChoiceLabel)
				case store.DecisionOutcomeOverride:
					return fmt.Sprintf("%s: already answered (overridden by %s, %s)", f.Key, who, eff.ChoiceLabel)
				default:
					return fmt.Sprintf("%s: already answered (by %s, %s)", f.Key, who, eff.ChoiceLabel)
				}
			}
		}
		return fmt.Sprintf("%s: already answered", f.Key)
	case store.InterpretedReasonReplaced:
		return fmt.Sprintf("%s: replaced by a newer version", f.Key)
	case store.InterpretedReasonInvalidChoice:
		return fmt.Sprintf("%s: choice %q is not a choice of this decision", f.Key, f.ChoiceID)
	case store.InterpretedReasonNotFound:
		return fmt.Sprintf("%s: decision not found", f.Key)
	default:
		return fmt.Sprintf("%s: %s", f.Key, f.Reason)
	}
}
