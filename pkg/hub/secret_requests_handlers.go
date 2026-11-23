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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Wire type aliases shared with pkg/messages so pkg/hub and pkg/hubclient share
// one canonical JSON contract for secret requests (ptone/scion#8747).
type (
	SecretRequestSendResult        = messages.SecretRequestSendResult
	SecretRequestReplaced          = messages.SecretRequestReplaced
	SecretRequestReplacedBy        = messages.SecretRequestReplacedBy
	SecretRequestFulfilled         = messages.SecretRequestFulfilled
	SecretRequestWithdrawn         = messages.SecretRequestWithdrawn
	SecretRequestDismissed         = messages.SecretRequestDismissed
	SecretRequestExpired           = messages.SecretRequestExpired
	SecretRequestView              = messages.SecretRequestView
	SecretRequestCardResponse      = messages.SecretRequestCardResponse
	FulfillSecretRequestBody       = messages.FulfillSecretRequestBody
	FulfillSecretRequestResponse   = messages.FulfillSecretRequestResponse
	DismissSecretRequestBody       = messages.DismissSecretRequestBody
	DismissSecretRequestResponse   = messages.DismissSecretRequestResponse
	WithdrawSecretRequestsRequest  = messages.WithdrawSecretRequestsRequest
	WithdrawSecretRequestsResponse = messages.WithdrawSecretRequestsResponse
	AgentSecretRequestListResponse = messages.AgentSecretRequestListResponse
)

// Error codes of the secret request endpoints (§4.6).
const (
	ErrCodeSecretRequestKeyPending = msgaction.ConflictSecretRequestKeyPending
	ErrCodeSecretRequestSubmitting = msgaction.ConflictSecretRequestSubmitting
	ErrCodeSecretRequestReplaced   = msgaction.ConflictReplaced
	ErrCodeSecretRequestWithdrawn  = msgaction.ConflictWithdrawn
	ErrCodeSecretRequestDismissed  = msgaction.ConflictDismissed
	ErrCodeSecretRequestExpired    = msgaction.ConflictExpired
	ErrCodeSecretRequestFulfilled  = msgaction.ConflictAlreadyFulfilled
	ErrCodeSecretRequestBusy       = msgaction.ConflictSubmitting
)

const (
	secretRequestsRoutePrefix      = "/api/v1/secret-requests/"
	agentSecretRequestsExactPath   = "/api/v1/agent/secret-requests"
	agentSecretRequestsRoutePrefix = "/api/v1/agent/secret-requests/"

	maxFulfillRequestBodyBytes = 256 * 1024
	maxSecretRequestValueBytes = 64 * 1024
	// secretRequestFulfillTotalTimeout bounds the entire backend.Set loop well
	// below SecretRequestClaimTTL (2m) so a multi-key fulfill cannot outrun the
	// stale-claim sweeper.
	secretRequestFulfillTotalTimeout = 90 * time.Second
	// secretRequestFulfillSetTimeout caps each individual backend.Set call within
	// secretRequestFulfillTotalTimeout.
	secretRequestFulfillSetTimeout   = 30 * time.Second
	secretRequestCompensationTimeout = 15 * time.Second
)

// handleSecretRequestRoutes serves the responder secret-request endpoints,
// 404 while hub.agent_secrets is off:
//
//	GET  /api/v1/secret-requests/{messageId}
//	POST /api/v1/secret-requests/{messageId}/fulfill
//	POST /api/v1/secret-requests/{messageId}/dismiss
func (s *Server) handleSecretRequestRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.AgentSecrets, func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, secretRequestsRoutePrefix)
		msgID, sub, hasSlash := strings.Cut(rest, "/")
		if msgID == "" {
			NotFound(w, "Secret request")
			return
		}
		if !hasSlash || sub == "" {
			if r.Method != http.MethodGet {
				MethodNotAllowed(w, http.MethodGet)
				return
			}
			s.handleSecretRequestGet(w, r, msgID)
			return
		}
		if strings.Contains(sub, "/") {
			NotFound(w, "Secret request")
			return
		}
		switch sub {
		case "fulfill":
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleSecretRequestFulfill(w, r, msgID)
		case "dismiss":
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleSecretRequestDismiss(w, r, msgID)
		default:
			NotFound(w, "Secret request")
		}
	})(w, r)
}

// handleAgentSecretRequestRoutes serves the agent self-service secret-request
// endpoints, 404 while hub.agent_secrets is off:
//
//	GET  /api/v1/agent/secret-requests
//	POST /api/v1/agent/secret-requests/withdraw
func (s *Server) handleAgentSecretRequestRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.AgentSecrets, func(w http.ResponseWriter, r *http.Request) {
		agentIdent := agentDecisionCaller(w, r)
		if agentIdent == nil {
			return
		}
		path := r.URL.Path
		if path == agentSecretRequestsExactPath {
			if r.Method != http.MethodGet {
				MethodNotAllowed(w, http.MethodGet)
				return
			}
			s.handleAgentSecretRequestList(w, r, agentIdent)
			return
		}
		rest := strings.TrimPrefix(path, agentSecretRequestsRoutePrefix)
		if rest == "withdraw" {
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleAgentSecretRequestWithdraw(w, r, agentIdent)
			return
		}
		NotFound(w, "Secret request")
	})(w, r)
}

func normalizeSecretRequestSurface(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case "", msgaction.SecretRequestSurfaceNative:
		return msgaction.SecretRequestSurfaceNative, true
	case msgaction.SecretRequestSurfaceStandalonePage:
		return msgaction.SecretRequestSurfaceStandalonePage, true
	default:
		return "", false
	}
}

func (s *Server) emitSecretRequestRefused(ctx context.Context, ev *MessageActionEvent) {
	if ev == nil {
		return
	}
	ev.Kind = msgaction.KindSecretRequest
	ev.EventType = MessageActionEventRefused
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	s.emitMessageActionEvent(ctx, ev)
}

// secretRequestCaller verifies the caller is a signed-in human user on an
// interactive web session (or dev credential). Agents, visitors, user access
// tokens, and federated credentials are refused and logged as refused audit
// events (§4.6, §4.11).
func (s *Server) secretRequestCaller(w http.ResponseWriter, r *http.Request, msgID string) UserIdentity {
	ctx := r.Context()
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			MessageID: msgID,
			ActorKind: MessageActionActorAgent,
			ActorID:   agentIdent.ID(),
			Reason:    ErrCodeUsersOnly,
		})
		writeError(w, http.StatusForbidden, ErrCodeUsersOnly, "this endpoint is only available to users", nil)
		return nil
	}
	user := GetUserIdentityFromContext(ctx)
	if isNilIdentity(user) {
		Unauthorized(w)
		return nil
	}
	if isVisitorIdentity(user) {
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			MessageID: msgID,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Reason:    "visitor_refused",
		})
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "secret requests require a signed-in user session", nil)
		return nil
	}
	if cls, _ := inboxCredentialOf(ctx); cls != inboxCredentialSession {
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			MessageID: msgID,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Reason:    "session_required",
		})
		writeSessionOnlyDenial(w, ErrCodeForbidden, "secret requests require a web session", authzop.ReasonInteractiveState)
		return nil
	}
	return user
}

// loadSecretRequestCard loads the card's rows and its chat key.
func (s *Server) loadSecretRequestCard(w http.ResponseWriter, ctx context.Context, msgID string) ([]*store.SecretRequest, string, *store.Message, bool) {
	rows, err := s.store.GetSecretRequestsByMessageID(ctx, msgID)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) || errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Secret request")
			return nil, "", nil, false
		}
		writeErrorFromErr(w, err, "")
		return nil, "", nil, false
	}
	if len(rows) == 0 {
		NotFound(w, "Secret request")
		return nil, "", nil, false
	}
	var promptMsg *store.Message
	if m, merr := s.store.GetMessage(ctx, msgID); merr == nil && m != nil {
		promptMsg = m
	}
	chatKey := rows[0].ChatKey
	if chatKey == "" && promptMsg != nil {
		chatKey = promptMsg.ThreadID
	}
	return rows, chatKey, promptMsg, true
}

// isSecretRequestRecipient reports whether user is a recipient of the secret
// request card (the DM target, or a participant of the thread/project).
// answering is false for GET (read access) and true for fulfill/dismiss (N5).
func (s *Server) isSecretRequestRecipient(ctx context.Context, user UserIdentity, chatKey string, promptMsg *store.Message, answering bool) bool {
	if user == nil {
		return false
	}
	if chatKey == "" {
		if promptMsg != nil {
			if promptMsg.RecipientID != "" && promptMsg.RecipientID == user.ID() {
				return true
			}
			if promptMsg.ConversationID != "" && s.store != nil {
				if parts, err := s.store.ListParticipants(ctx, promptMsg.ConversationID); err == nil {
					for _, p := range parts {
						if p.PrincipalKind == "user" && p.PrincipalID == user.ID() {
							return true
						}
					}
				}
			}
		}
		return false
	}
	if strings.HasPrefix(chatKey, "dm:") {
		return authorizeDMKeyParticipant(ctx, user, chatKey, chatSendPath(chatKey)) == nil
	}
	return s.authorizeDecisionChatKey(ctx, user, chatKey, answering) == nil
}

// checkSecretRequestAgentPerm checks whether user holds action (ActionRead or
// ActionAttach) on the requesting agent. If the agent is hard-deleted, it falls
// back to a synthetic resource scoped to the row's project so retained cards
// remain readable by project readers and never attachable.
func (s *Server) checkSecretRequestAgentPerm(ctx context.Context, user UserIdentity, row *store.SecretRequest, action Action) (*store.Agent, bool) {
	a, err := s.store.GetAgent(ctx, row.AgentID)
	if err != nil || a == nil {
		if action == ActionAttach {
			return nil, false
		}
		d := s.authzService.CheckAccess(ctx, user, Resource{
			Type:       "agent",
			ID:         row.AgentID,
			ParentType: "project",
			ParentID:   row.ProjectID,
		}, action)
		return nil, d.Allowed
	}
	d := s.authzService.CheckAccess(ctx, user, agentResource(a), action)
	return a, d.Allowed
}

func (s *Server) secretRequestViewsFor(ctx context.Context, rows []*store.SecretRequest) []SecretRequestView {
	var replIDs []string
	for _, r := range rows {
		if r.ReplacedByID != "" {
			replIDs = append(replIDs, r.ReplacedByID)
		}
	}
	var replMap map[string]*store.SecretRequest
	if len(replIDs) > 0 {
		if m, err := s.store.GetSecretRequestsByIDs(ctx, replIDs); err == nil {
			replMap = m
		}
	}
	views := make([]SecretRequestView, len(rows))
	for i, r := range rows {
		var repl *store.SecretRequest
		if r.ReplacedByID != "" && replMap != nil {
			repl = replMap[r.ReplacedByID]
		}
		views[i] = secretRequestView(r, repl)
	}
	return views
}

func secretRequestView(r *store.SecretRequest, replacedBy *store.SecretRequest) SecretRequestView {
	status := r.Status
	submitting := false
	if status == store.SecretRequestStatusFulfilling {
		status = store.SecretRequestStatusPending
		submitting = true
	}
	v := SecretRequestView{
		RowID:          r.ID,
		MessageID:      r.MessageID,
		ProjectID:      r.ProjectID,
		AgentID:        r.AgentID,
		AgentName:      r.AgentName,
		Key:            r.Key,
		Version:        r.Version,
		Description:    r.Description,
		SecretType:     r.SecretType,
		Target:         r.Target,
		Position:       r.Position,
		Status:         status,
		Submitting:     submitting,
		ConversationID: r.ConversationID,
		CreatedAt:      r.CreatedAt,
		ExpiresAt:      r.ExpiresAt,
		ResolvedAt:     r.ResolvedAt,
		SecretVersion:  r.SecretVersion,
	}
	if replacedBy != nil {
		rb := &SecretRequestReplacedBy{
			RowID:     replacedBy.ID,
			MessageID: replacedBy.MessageID,
			Version:   replacedBy.Version,
		}
		if r.ReplacedAt != nil {
			rb.At = *r.ReplacedAt
		}
		v.ReplacedBy = rb
	}
	switch r.Status {
	case store.SecretRequestStatusFulfilled:
		f := &SecretRequestFulfilled{
			ByUserID:          r.ResolvedByUserID,
			ByName:            r.ResolvedByName,
			ByEmail:           r.ResolvedByEmail,
			Surface:           r.Surface,
			ResponseMessageID: r.ResponseMessageID,
			SecretVersion:     r.SecretVersion,
		}
		if r.ResolvedAt != nil {
			f.At = *r.ResolvedAt
		}
		v.Fulfilled = f
	case store.SecretRequestStatusWithdrawn:
		w := &SecretRequestWithdrawn{
			Reason: r.WithdrawnReason,
			By:     r.WithdrawnBy,
		}
		switch r.WithdrawnBy {
		case store.SecretRequestWithdrawnByAgent:
			w.ByName = r.AgentName
		case store.SecretRequestWithdrawnByHub:
			w.ByName = "hub"
		}
		if r.ResolvedAt != nil {
			w.At = *r.ResolvedAt
		}
		v.Withdrawn = w
	case store.SecretRequestStatusDismissed:
		d := &SecretRequestDismissed{
			ByUserID:          r.ResolvedByUserID,
			ByName:            r.ResolvedByName,
			ByEmail:           r.ResolvedByEmail,
			Reason:            r.DismissReason,
			Surface:           r.Surface,
			ResponseMessageID: r.ResponseMessageID,
		}
		if r.ResolvedAt != nil {
			d.At = *r.ResolvedAt
		}
		v.Dismissed = d
	case store.SecretRequestStatusExpired:
		e := &SecretRequestExpired{}
		if r.ResolvedAt != nil {
			e.At = *r.ResolvedAt
		}
		v.Expired = e
	}
	return v
}

func (s *Server) publishSecretRequestUpdated(ctx context.Context, row *store.SecretRequest, replacedBy *store.SecretRequest) {
	if row == nil {
		return
	}
	chatKey := row.ChatKey
	if chatKey == "" {
		if prompt, err := s.store.GetMessage(ctx, row.MessageID); err == nil && prompt != nil {
			chatKey = prompt.ThreadID
		}
	}
	if chatKey == "" {
		return
	}
	view := secretRequestView(row, replacedBy)
	s.publishActionUpdated(msgaction.KindSecretRequest, row.ProjectID, row.MessageID, chatKey, view)
}

// handleSecretRequestGet serves GET /api/v1/secret-requests/{messageId}.
func (s *Server) handleSecretRequestGet(w http.ResponseWriter, r *http.Request, msgID string) {
	ctx := r.Context()
	user := s.secretRequestCaller(w, r, msgID)
	if user == nil {
		return
	}
	rows, chatKey, promptMsg, ok := s.loadSecretRequestCard(w, ctx, msgID)
	if !ok {
		return
	}

	isRecipient := s.isSecretRequestRecipient(ctx, user, chatKey, promptMsg, false)
	_, canRead := s.checkSecretRequestAgentPerm(ctx, user, rows[0], ActionRead)
	if !isRecipient || !canRead {
		reason := "not_recipient"
		if isRecipient && !canRead {
			reason = "missing_agent_read"
		}
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     rows[0].ID,
			MessageID: msgID,
			ProjectID: rows[0].ProjectID,
			AgentID:   rows[0].AgentID,
			AgentName: rows[0].AgentName,
			Key:       rows[0].Key,
			Version:   rows[0].Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Reason:    reason,
		})
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "you do not have access to this secret request", nil)
		return
	}

	canAnswer := s.isSecretRequestRecipient(ctx, user, chatKey, promptMsg, true)
	_, canAttach := s.checkSecretRequestAgentPerm(ctx, user, rows[0], ActionAttach)
	now := time.Now().UTC()
	hasPending := false
	for _, row := range rows {
		if row.Status == store.SecretRequestStatusPending && row.ExpiresAt.After(now) {
			hasPending = true
			break
		}
	}

	canRespond := true
	reason := ""
	switch {
	case !canAnswer:
		canRespond = false
		reason = "you are not an eligible responder for this secret request"
	case !canAttach:
		canRespond = false
		reason = "you need agent.attach permission on this agent to supply or dismiss secrets"
	case !hasPending:
		canRespond = false
		reason = "all secret requests on this card are closed"
	}

	projectName := ""
	if proj, err := s.store.GetProject(ctx, rows[0].ProjectID); err == nil && proj != nil {
		projectName = proj.Name
		if projectName == "" {
			projectName = proj.Slug
		}
	}
	intro := ""
	if promptMsg != nil && rows[0].IntroLength > 0 && rows[0].IntroLength <= len(promptMsg.Msg) {
		intro = promptMsg.Msg[:rows[0].IntroLength]
	}

	writeJSON(w, http.StatusOK, SecretRequestCardResponse{
		MessageID:      msgID,
		ProjectID:      rows[0].ProjectID,
		ProjectName:    projectName,
		AgentID:        rows[0].AgentID,
		AgentName:      rows[0].AgentName,
		ConversationID: rows[0].ConversationID,
		ThreadID:       chatKey,
		Intro:          intro,
		Rows:           s.secretRequestViewsFor(ctx, rows),
		CanRespond:     canRespond,
		Reason:         reason,
	})
}

// handleSecretRequestFulfill serves POST /api/v1/secret-requests/{messageId}/fulfill.
func (s *Server) handleSecretRequestFulfill(w http.ResponseWriter, r *http.Request, msgID string) {
	ctx := r.Context()
	if pendingSecretBrowserRequestRefused(w, r) {
		return
	}
	user := s.secretRequestCaller(w, r, msgID)
	if user == nil {
		return
	}
	rows, chatKey, promptMsg, ok := s.loadSecretRequestCard(w, ctx, msgID)
	if !ok {
		return
	}

	// Step 1: Authorize recipient + agent.attach.
	isRecipient := s.isSecretRequestRecipient(ctx, user, chatKey, promptMsg, true)
	asker, canAttach := s.checkSecretRequestAgentPerm(ctx, user, rows[0], ActionAttach)
	if !isRecipient || !canAttach {
		reason := "not_recipient"
		msg := "only recipients of this request can fulfill it"
		if isRecipient && !canAttach {
			reason = "missing_agent_attach"
			msg = "fulfilling a secret request requires agent.attach permission on the requesting agent"
		}
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     rows[0].ID,
			MessageID: msgID,
			ProjectID: rows[0].ProjectID,
			AgentID:   rows[0].AgentID,
			AgentName: rows[0].AgentName,
			Key:       rows[0].Key,
			Version:   rows[0].Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Reason:    reason,
		})
		writeError(w, http.StatusForbidden, ErrCodeForbidden, msg, nil)
		return
	}

	// Step 2: Decode and validate body (never log Values).
	r.Body = http.MaxBytesReader(w, r.Body, maxFulfillRequestBodyBytes)
	var body FulfillSecretRequestBody
	if err := readJSON(r, &body); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if len(body.IdempotencyKey) > msgaction.MaxIdempotencyKeyLen {
		ValidationError(w, fmt.Sprintf("idempotencyKey is at most %d bytes", msgaction.MaxIdempotencyKeyLen),
			map[string]interface{}{"field": "idempotencyKey"})
		return
	}
	surface, surfOK := normalizeSecretRequestSurface(body.Surface)
	if !surfOK {
		ValidationError(w, fmt.Sprintf("surface %q is not supported; expected %q or %q",
			body.Surface, msgaction.SecretRequestSurfaceNative, msgaction.SecretRequestSurfaceStandalonePage),
			map[string]interface{}{"field": "surface"})
		return
	}

	// Check idempotent replay before key-set validation so an already-fulfilled
	// card replays cleanly with the same idempotencyKey.
	now := time.Now().UTC()
	if body.IdempotencyKey != "" {
		for _, row := range rows {
			if row.Status == store.SecretRequestStatusFulfilled &&
				row.IdempotencyKey == body.IdempotencyKey &&
				row.ResolvedByUserID == user.ID() {
				writeJSON(w, http.StatusOK, FulfillSecretRequestResponse{
					Rows:              s.secretRequestViewsFor(ctx, rows),
					ResponseMessageID: row.ResponseMessageID,
					Replayed:          true,
				})
				return
			}
		}
	}

	// Identify currently pending rows on the card.
	var pendingRows []*store.SecretRequest
	for _, row := range rows {
		if row.Status == store.SecretRequestStatusPending && row.ExpiresAt.After(now) {
			pendingRows = append(pendingRows, row)
		}
	}
	if len(pendingRows) == 0 {
		// No pending rows left: report the conflict state of the first row.
		conflict := rows[0]
		statusForCode := conflict.Status
		if statusForCode == store.SecretRequestStatusPending && !conflict.ExpiresAt.After(now) {
			statusForCode = store.SecretRequestStatusExpired
		}
		code := msgaction.ConflictCode(msgaction.SecretRequestKindSpec, statusForCode)
		if code == "" {
			code = msgaction.ConflictAlreadyFulfilled
		}
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     conflict.ID,
			MessageID: msgID,
			ProjectID: conflict.ProjectID,
			AgentID:   conflict.AgentID,
			AgentName: conflict.AgentName,
			Key:       conflict.Key,
			Version:   conflict.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Surface:   surface,
			Reason:    code,
		})
		s.writeSecretRequestConflict(w, ctx, code, conflict)
		return
	}

	// Validate values map has EXACTLY the card's currently pending keys.
	if len(body.Values) != len(pendingRows) {
		ValidationError(w, fmt.Sprintf("values must contain exactly the %d pending key(s) on this card", len(pendingRows)),
			map[string]interface{}{"field": "values"})
		return
	}
	valByNorm := make(map[string]string, len(body.Values))
	for k, v := range body.Values {
		norm := messages.SecretRequestKeyNorm(k)
		if v == "" {
			ValidationError(w, fmt.Sprintf("value for key %q must be non-empty", k),
				map[string]interface{}{"field": "values"})
			return
		}
		if len(v) > maxSecretRequestValueBytes {
			ValidationError(w, fmt.Sprintf("secret value for key %q exceeds 64KB limit", k),
				map[string]interface{}{"field": "values"})
			return
		}
		valByNorm[norm] = v
	}
	claimRowIDs := make([]string, len(pendingRows))
	for i, row := range pendingRows {
		if _, exists := valByNorm[row.KeyNorm]; !exists {
			ValidationError(w, fmt.Sprintf("missing value for pending key %q", row.Key),
				map[string]interface{}{"field": "values"})
			return
		}
		if !s.validateAgentSecretWriteConstraints(w, ctx, row.AgentID, row.Key, row.SecretType, row.Target) {
			return
		}
		claimRowIDs[i] = row.ID
	}

	if s.secretBackend == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "secret backend is not configured", nil)
		return
	}

	// Step 3: Claim the pending rows (pending -> fulfilling).
	userName := user.DisplayName()
	if userName == "" {
		userName = user.Email()
	}
	claimRes, err := s.store.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID:      msgID,
		RowIDs:         claimRowIDs,
		UserID:         user.ID(),
		UserName:       userName,
		UserEmail:      user.Email(),
		Surface:        surface,
		IdempotencyKey: body.IdempotencyKey,
		Now:            now,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if claimRes.Replayed {
		respMsgID := ""
		for _, r := range claimRes.Rows {
			if r.ResponseMessageID != "" {
				respMsgID = r.ResponseMessageID
				break
			}
		}
		writeJSON(w, http.StatusOK, FulfillSecretRequestResponse{
			Rows:              s.secretRequestViewsFor(ctx, claimRes.Rows),
			ResponseMessageID: respMsgID,
			Replayed:          true,
		})
		return
	}
	if claimRes.Conflict != nil {
		code := msgaction.ConflictCode(msgaction.SecretRequestKindSpec, claimRes.Conflict.Status)
		if code == "" {
			code = msgaction.ConflictSubmitting
		}
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     claimRes.Conflict.ID,
			MessageID: msgID,
			ProjectID: claimRes.Conflict.ProjectID,
			AgentID:   claimRes.Conflict.AgentID,
			AgentName: claimRes.Conflict.AgentName,
			Key:       claimRes.Conflict.Key,
			Version:   claimRes.Conflict.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Surface:   surface,
			Reason:    code,
		})
		s.writeSecretRequestConflict(w, ctx, code, claimRes.Conflict)
		return
	}

	// Publish live "submitting" state for each claimed row (N4).
	for _, row := range claimRes.Claimed {
		s.publishSecretRequestUpdated(ctx, row, nil)
	}

	// compensate deletes keys already written in this call and rolls back the
	// claim using a detached context so client disconnects cannot leak partial
	// writes or stuck claims (N2), then publishes the restored pending state (N4).
	compensate := func(writtenKeys []string, rollbackClaim bool) {
		compCtx, compCancel := context.WithTimeout(context.WithoutCancel(ctx), secretRequestCompensationTimeout)
		defer compCancel()
		for _, k := range writtenKeys {
			_ = s.secretBackend.Delete(compCtx, k, store.ScopeAgent, rows[0].AgentID)
		}
		if rollbackClaim {
			if n, rerr := s.store.RollbackSecretRequestClaim(compCtx, &store.RollbackSecretRequestClaimInput{
				MessageID: msgID,
				RowIDs:    claimRowIDs,
				UserID:    user.ID(),
				ClaimedAt: claimRes.ClaimedAt,
			}); rerr == nil && n > 0 {
				if restored, gerr := s.store.GetSecretRequestsByIDs(compCtx, claimRowIDs); gerr == nil {
					for _, id := range claimRowIDs {
						if rr, ok := restored[id]; ok {
							s.publishSecretRequestUpdated(compCtx, rr, nil)
						}
					}
				}
			}
		}
	}

	// Step 4: Store each key in the secret backend at agent scope, bounded
	// as a whole by secretRequestFulfillTotalTimeout (< SecretRequestClaimTTL)
	// with each individual Set capped at secretRequestFulfillSetTimeout.
	// Track both all writtenKeys (for AC7 hub-withdrawn cleanup when the agent
	// is hard-deleted mid-fulfill) and newly createdKeys (so a 503 rollback
	// never deletes an already-existing secret during a failed rotation).
	loopCtx, loopCancel := context.WithTimeout(ctx, secretRequestFulfillTotalTimeout)
	defer loopCancel()
	var writtenKeys []string
	var createdKeys []string
	secretVersions := make(map[string]int, len(claimRes.Claimed))
	for _, row := range claimRes.Claimed {
		val := valByNorm[row.KeyNorm]
		target := row.Target
		if target == "" {
			target = row.Key
		}
		setCtx, setCancel := context.WithTimeout(loopCtx, secretRequestFulfillSetTimeout)
		_, metaErr := s.secretBackend.GetMeta(setCtx, row.Key, store.ScopeAgent, row.AgentID)
		if metaErr != nil && !errors.Is(metaErr, store.ErrNotFound) {
			setCancel()
			slog.Warn("secret request fulfill: backend.GetMeta failed",
				"message_id", msgID,
				"row_id", row.ID,
				"key", row.Key,
				"error", metaErr,
			)
			compensate(createdKeys, true)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "failed to check secret in backend; please retry", nil)
			return
		}
		existedBefore := metaErr == nil
		created, meta, setErr := s.secretBackend.Set(setCtx, &secret.SetSecretInput{
			Name:          row.Key,
			Value:         val,
			SecretType:    row.SecretType,
			Target:        target,
			Scope:         store.ScopeAgent,
			ScopeID:       row.AgentID,
			Description:   row.Description,
			InjectionMode: store.InjectionModeAsNeeded,
			CreatedBy:     user.ID(),
			UpdatedBy:     user.ID(),
			ProjectID:     row.ProjectID,
		})
		setCancel()
		if setErr != nil {
			slog.Warn("secret request fulfill: backend.Set failed",
				"message_id", msgID,
				"row_id", row.ID,
				"key", row.Key,
				"error", setErr,
			)
			compensate(createdKeys, true)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "failed to store secret in backend; please retry", nil)
			return
		}
		writtenKeys = append(writtenKeys, row.Key)
		if !existedBefore && created {
			createdKeys = append(createdKeys, row.Key)
		}
		if meta != nil {
			secretVersions[row.ID] = meta.Version
		}
	}

	// Step 5: Finish (fulfilling -> fulfilled) and insert secret-response message.
	respText := formatSecretFulfilledResponse(claimRes.Claimed, userName, user.Email())
	respMsg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      rows[0].ProjectID,
		ConversationID: rows[0].ConversationID,
		ThreadID:       chatKey,
		Sender:         userSenderAddress(user),
		SenderID:       user.ID(),
		Recipient:      "agent:" + rows[0].AgentName,
		RecipientID:    rows[0].AgentID,
		Msg:            respText,
		Type:           messages.TypeSecretResponse,
		DispatchState:  store.MessageDispatchDispatched,
	}
	finishNow := time.Now().UTC()
	finRes, err := s.store.FinishSecretRequestFulfill(ctx, &store.FinishSecretRequestFulfillInput{
		MessageID:       msgID,
		RowIDs:          claimRowIDs,
		UserID:          user.ID(),
		ClaimedAt:       claimRes.ClaimedAt,
		Surface:         surface,
		IdempotencyKey:  body.IdempotencyKey,
		SecretVersions:  secretVersions,
		ResponseMessage: respMsg,
		Now:             finishNow,
	})
	if err != nil {
		compensate(createdKeys, true)
		writeErrorFromErr(w, err, "")
		return
	}
	if finRes.Conflict != nil {
		// On finish compare-and-set conflict, delete EVERY key written in this
		// call when the conflict row was withdrawn by the hub (agent hard-deleted
		// or purged — AC7). Otherwise (e.g., stale-claim recovery followed by
		// another responder's fulfill), leave the backend alone and log per §4.6
		// ("a value that was already written stays in place").
		if finRes.Conflict.Status == store.SecretRequestStatusWithdrawn &&
			finRes.Conflict.WithdrawnBy == store.SecretRequestWithdrawnByHub {
			compensate(writtenKeys, false)
		} else if len(writtenKeys) > 0 {
			slog.Warn("secret request fulfill finish conflict; leaving written backend keys in place",
				"message_id", msgID,
				"conflict_row_id", finRes.Conflict.ID,
				"conflict_status", finRes.Conflict.Status,
				"written_key_count", len(writtenKeys),
			)
		}
		code := msgaction.ConflictCode(msgaction.SecretRequestKindSpec, finRes.Conflict.Status)
		if code == "" {
			code = msgaction.ConflictSubmitting
		}
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     finRes.Conflict.ID,
			MessageID: msgID,
			ProjectID: finRes.Conflict.ProjectID,
			AgentID:   finRes.Conflict.AgentID,
			AgentName: finRes.Conflict.AgentName,
			Key:       finRes.Conflict.Key,
			Version:   finRes.Conflict.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Surface:   surface,
			Reason:    code,
		})
		s.writeSecretRequestConflict(w, ctx, code, finRes.Conflict)
		return
	}

	// Step 6: Deliver response to asking agent (auto-wake), publish live updates, emit audit.
	if finRes.ResponseMessage != nil && asker != nil && asker.DeletedAt.IsZero() {
		sm := &messages.StructuredMessage{
			Version:        messages.Version,
			Timestamp:      finishNow.Format(time.RFC3339),
			Sender:         finRes.ResponseMessage.Sender,
			SenderID:       finRes.ResponseMessage.SenderID,
			Recipient:      finRes.ResponseMessage.Recipient,
			RecipientID:    finRes.ResponseMessage.RecipientID,
			Msg:            respText,
			Type:           messages.TypeSecretResponse,
			ThreadID:       chatKey,
			ConversationID: finRes.ResponseMessage.ConversationID,
		}
		_, _ = s.dispatchActionResponse(ctx, actionResponse{
			Asker:      asker,
			Message:    finRes.ResponseMessage,
			Text:       respText,
			Structured: sm,
			PopulateMeta: func(meta map[string]string) {
				meta["secret_request_message_id"] = msgID
				meta["secret_request_outcome"] = msgaction.EventFulfilled
			},
			LogPrefix: "secret_request",
			Label:     "secret-response",
		})
	}

	claimedSet := make(map[string]bool, len(claimRowIDs))
	for _, id := range claimRowIDs {
		claimedSet[id] = true
	}
	for _, row := range finRes.Rows {
		if !claimedSet[row.ID] {
			continue
		}
		s.publishSecretRequestUpdated(ctx, row, nil)
		details := map[string]string{
			"secret_type": row.SecretType,
		}
		if row.Target != "" {
			details["target"] = row.Target
		}
		if row.SecretVersion > 0 {
			details["secret_version"] = strconv.Itoa(row.SecretVersion)
		}
		s.emitMessageActionEvent(ctx, &MessageActionEvent{
			Kind:      msgaction.KindSecretRequest,
			EventType: msgaction.EventFulfilled,
			RowID:     row.ID,
			MessageID: row.MessageID,
			ProjectID: row.ProjectID,
			AgentID:   row.AgentID,
			AgentName: row.AgentName,
			Key:       row.Key,
			Version:   row.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Surface:   surface,
			Details:   details,
			Timestamp: finishNow,
		})
	}

	respMsgID := ""
	if finRes.ResponseMessage != nil {
		respMsgID = finRes.ResponseMessage.ID
	}
	writeJSON(w, http.StatusOK, FulfillSecretRequestResponse{
		Rows:              s.secretRequestViewsFor(ctx, finRes.Rows),
		ResponseMessageID: respMsgID,
	})
}

func formatSecretFulfilledResponse(rows []*store.SecretRequest, userName, userEmail string) string {
	who := userEmail
	if who == "" {
		who = userName
	}
	if len(rows) == 1 {
		r := rows[0]
		return fmt.Sprintf("`%s` v%d is now available (stored by %s). Read it when needed with `scion secret get --value %s`.",
			r.Key, r.Version, who, r.Key)
	}
	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, fmt.Sprintf("Secrets stored by %s:", who))
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("- `%s` v%d — read with `scion secret get --value %s`", r.Key, r.Version, r.Key))
	}
	return strings.Join(lines, "\n")
}

// handleSecretRequestDismiss serves POST /api/v1/secret-requests/{messageId}/dismiss.
func (s *Server) handleSecretRequestDismiss(w http.ResponseWriter, r *http.Request, msgID string) {
	ctx := r.Context()
	if pendingSecretBrowserRequestRefused(w, r) {
		return
	}
	user := s.secretRequestCaller(w, r, msgID)
	if user == nil {
		return
	}
	rows, chatKey, promptMsg, ok := s.loadSecretRequestCard(w, ctx, msgID)
	if !ok {
		return
	}

	isRecipient := s.isSecretRequestRecipient(ctx, user, chatKey, promptMsg, true)
	asker, canAttach := s.checkSecretRequestAgentPerm(ctx, user, rows[0], ActionAttach)
	if !isRecipient || !canAttach {
		reason := "not_recipient"
		msg := "only recipients of this request can dismiss it"
		if isRecipient && !canAttach {
			reason = "missing_agent_attach"
			msg = "dismissing a secret request requires agent.attach permission on the requesting agent"
		}
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     rows[0].ID,
			MessageID: msgID,
			ProjectID: rows[0].ProjectID,
			AgentID:   rows[0].AgentID,
			AgentName: rows[0].AgentName,
			Key:       rows[0].Key,
			Version:   rows[0].Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Reason:    reason,
		})
		writeError(w, http.StatusForbidden, ErrCodeForbidden, msg, nil)
		return
	}

	var body DismissSecretRequestBody
	if r.Body != nil && r.ContentLength != 0 {
		raw, err := io.ReadAll(io.LimitReader(r.Body, msgaction.MaxRequestBodyBytes+1))
		if err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if int64(len(raw)) > msgaction.MaxRequestBodyBytes {
			BadRequest(w, fmt.Sprintf("Invalid request body: request body exceeds %d bytes", msgaction.MaxRequestBodyBytes))
			return
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				BadRequest(w, "Invalid request body: "+err.Error())
				return
			}
		}
	}

	parsed := &msgaction.DismissRequest{
		RowIDs:         body.RowIDs,
		Reason:         body.Reason,
		Surface:        body.Surface,
		IdempotencyKey: body.IdempotencyKey,
	}
	if ferr := msgaction.ValidateDismissRequest(parsed, msgaction.DismissParseOptions{
		RowIDsField:      "rowIds",
		MaxRowIDs:        messages.MaxSecretRequestsPerMessage,
		NormalizeSurface: normalizeSecretRequestSurface,
		SurfaceError: func(raw string) string {
			return fmt.Sprintf("surface %q is not supported; expected %q or %q",
				raw, msgaction.SecretRequestSurfaceNative, msgaction.SecretRequestSurfaceStandalonePage)
		},
	}); ferr != nil {
		ValidationError(w, ferr.Message, map[string]interface{}{"field": ferr.Field})
		return
	}

	userName := user.DisplayName()
	if userName == "" {
		userName = user.Email()
	}
	now := time.Now().UTC()

	// Build response message text from the actual dismissed rows inside the tx (N6).
	respMsg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      rows[0].ProjectID,
		ConversationID: rows[0].ConversationID,
		ThreadID:       chatKey,
		Sender:         userSenderAddress(user),
		SenderID:       user.ID(),
		Recipient:      "agent:" + rows[0].AgentName,
		RecipientID:    rows[0].AgentID,
		Type:           messages.TypeSecretResponse,
		DispatchState:  store.MessageDispatchDispatched,
	}

	res, err := s.store.DismissSecretRequests(ctx, &store.DismissSecretRequestsInput{
		MessageID:       msgID,
		RowIDs:          parsed.RowIDs,
		UserID:          user.ID(),
		UserName:        userName,
		UserEmail:       user.Email(),
		Reason:          parsed.Reason,
		Surface:         parsed.Surface,
		IdempotencyKey:  parsed.IdempotencyKey,
		ResponseMessage: respMsg,
		FormatResponseMsg: func(dismissed []*store.SecretRequest) string {
			return formatSecretDismissedResponse(dismissed, userName, parsed.Reason)
		},
		Now: now,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, err.Error(), nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if res.Submitting != nil {
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     res.Submitting.ID,
			MessageID: msgID,
			ProjectID: res.Submitting.ProjectID,
			AgentID:   res.Submitting.AgentID,
			AgentName: res.Submitting.AgentName,
			Key:       res.Submitting.Key,
			Version:   res.Submitting.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Surface:   parsed.Surface,
			Reason:    msgaction.ConflictSubmitting,
		})
		s.writeSecretRequestConflict(w, ctx, msgaction.ConflictSubmitting, res.Submitting)
		return
	}
	if res.Replayed {
		respMsgID := ""
		for _, r := range res.AlreadyDismissed {
			if r.ResponseMessageID != "" {
				respMsgID = r.ResponseMessageID
				break
			}
		}
		writeJSON(w, http.StatusOK, DismissSecretRequestResponse{
			Dismissed:         []SecretRequestView{},
			AlreadyDismissed:  s.secretRequestViewsFor(ctx, res.AlreadyDismissed),
			ResponseMessageID: respMsgID,
			Replayed:          true,
		})
		return
	}
	if res.Replaced != nil {
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     res.Replaced.ID,
			MessageID: msgID,
			ProjectID: res.Replaced.ProjectID,
			AgentID:   res.Replaced.AgentID,
			AgentName: res.Replaced.AgentName,
			Key:       res.Replaced.Key,
			Version:   res.Replaced.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Surface:   parsed.Surface,
			Reason:    msgaction.ConflictReplaced,
		})
		s.writeSecretRequestConflict(w, ctx, msgaction.ConflictReplaced, res.Replaced)
		return
	}
	if len(res.Dismissed) == 0 {
		s.emitSecretRequestRefused(ctx, &MessageActionEvent{
			RowID:     rows[0].ID,
			MessageID: msgID,
			ProjectID: rows[0].ProjectID,
			AgentID:   rows[0].AgentID,
			AgentName: rows[0].AgentName,
			Key:       rows[0].Key,
			Version:   rows[0].Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Surface:   parsed.Surface,
			Reason:    msgaction.ConflictDismissChanged,
		})
		writeError(w, http.StatusConflict, msgaction.ConflictDismissChanged,
			"none of the targeted secret requests is still pending", nil)
		return
	}

	// Deliver response to asking agent (auto-wake).
	if res.ResponseMessage != nil && asker != nil && asker.DeletedAt.IsZero() {
		sm := &messages.StructuredMessage{
			Version:        messages.Version,
			Timestamp:      now.Format(time.RFC3339),
			Sender:         res.ResponseMessage.Sender,
			SenderID:       res.ResponseMessage.SenderID,
			Recipient:      res.ResponseMessage.Recipient,
			RecipientID:    res.ResponseMessage.RecipientID,
			Msg:            res.ResponseMessage.Msg,
			Type:           messages.TypeSecretResponse,
			ThreadID:       chatKey,
			ConversationID: res.ResponseMessage.ConversationID,
		}
		_, _ = s.dispatchActionResponse(ctx, actionResponse{
			Asker:      asker,
			Message:    res.ResponseMessage,
			Text:       res.ResponseMessage.Msg,
			Structured: sm,
			PopulateMeta: func(meta map[string]string) {
				meta["secret_request_message_id"] = msgID
				meta["secret_request_outcome"] = MessageActionEventDismissed
			},
			LogPrefix: "secret_request",
			Label:     "secret-response",
		})
	}

	for _, row := range res.Dismissed {
		s.publishSecretRequestUpdated(ctx, row, nil)
		details := map[string]string{"secret_type": row.SecretType}
		if row.Target != "" {
			details["target"] = row.Target
		}
		s.emitMessageActionEvent(ctx, &MessageActionEvent{
			Kind:      msgaction.KindSecretRequest,
			EventType: MessageActionEventDismissed,
			RowID:     row.ID,
			MessageID: row.MessageID,
			ProjectID: row.ProjectID,
			AgentID:   row.AgentID,
			AgentName: row.AgentName,
			Key:       row.Key,
			Version:   row.Version,
			ActorKind: MessageActionActorUser,
			ActorID:   user.ID(),
			Surface:   parsed.Surface,
			Reason:    parsed.Reason,
			Details:   details,
			Timestamp: now,
		})
	}

	respMsgID := ""
	if res.ResponseMessage != nil {
		respMsgID = res.ResponseMessage.ID
	}
	writeJSON(w, http.StatusOK, DismissSecretRequestResponse{
		Dismissed:         s.secretRequestViewsFor(ctx, res.Dismissed),
		AlreadyDismissed:  s.secretRequestViewsFor(ctx, res.AlreadyDismissed),
		ResponseMessageID: respMsgID,
	})
}

func formatSecretDismissedResponse(rows []*store.SecretRequest, userName, reason string) string {
	suffix := ""
	if reason != "" {
		suffix = ": " + reason
	}
	if len(rows) == 0 {
		return fmt.Sprintf("Secret request dismissed by %s%s.", userName, suffix)
	}
	if len(rows) == 1 {
		r := rows[0]
		return fmt.Sprintf("`%s` v%d dismissed by %s%s.", r.Key, r.Version, userName, suffix)
	}
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = fmt.Sprintf("`%s` v%d", r.Key, r.Version)
	}
	return fmt.Sprintf("Secret requests %s dismissed by %s%s.", strings.Join(keys, ", "), userName, suffix)
}

func (s *Server) writeSecretRequestConflict(w http.ResponseWriter, ctx context.Context, code string, row *store.SecretRequest) {
	var repl *store.SecretRequest
	if row.ReplacedByID != "" {
		if r, err := s.store.GetSecretRequest(ctx, row.ReplacedByID); err == nil {
			repl = r
		}
	}
	view := secretRequestView(row, repl)
	details := map[string]interface{}{
		"rowId":         row.ID,
		"key":           row.Key,
		"version":       row.Version,
		"status":        view.Status,
		"secretRequest": view,
	}
	if view.ReplacedBy != nil {
		details["replacedBy"] = view.ReplacedBy
	}
	writeError(w, http.StatusConflict, code, fmt.Sprintf("secret request %s v%d is %s", row.Key, row.Version, code), details)
}

// handleAgentSecretRequestWithdraw serves POST /api/v1/agent/secret-requests/withdraw.
func (s *Server) handleAgentSecretRequestWithdraw(w http.ResponseWriter, r *http.Request, agentIdent AgentIdentity) {
	ctx := r.Context()
	agentID := agentIdent.ID()

	r.Body = http.MaxBytesReader(w, r.Body, msgaction.MaxRequestBodyBytes)
	var req WithdrawSecretRequestsRequest
	if err := readJSON(r, &req); err != nil {
		ValidationError(w, "invalid JSON body: "+err.Error(), nil)
		return
	}

	parsed := &msgaction.WithdrawRequest{
		Keys:     req.Keys,
		WholeSet: req.WholeSet,
		Reason:   req.Reason,
	}
	if ferr := msgaction.ValidateWithdrawRequest(parsed, msgaction.WithdrawParseOptions{
		MaxKeys:      messages.MaxSecretRequestsPerMessage,
		NormalizeKey: messages.SecretRequestKeyNorm,
		KeyLabel:     "secret request key",
	}); ferr != nil {
		ValidationError(w, ferr.Message, map[string]interface{}{"field": ferr.Field})
		return
	}

	now := time.Now().UTC()
	res, err := s.store.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID:  agentID,
		Keys:     parsed.Keys,
		WholeSet: parsed.WholeSet,
		Reason:   parsed.Reason,
		Now:      now,
	})
	if err != nil {
		s.writeWithdrawSecretRequestsError(w, ctx, agentID, err)
		return
	}

	for _, row := range res.Withdrawn {
		s.publishSecretRequestUpdated(ctx, row, nil)
		details := map[string]string{"secret_type": row.SecretType}
		if row.Target != "" {
			details["target"] = row.Target
		}
		s.emitMessageActionEvent(ctx, &MessageActionEvent{
			Kind:      msgaction.KindSecretRequest,
			EventType: MessageActionEventWithdrawn,
			RowID:     row.ID,
			MessageID: row.MessageID,
			ProjectID: row.ProjectID,
			AgentID:   row.AgentID,
			AgentName: row.AgentName,
			Key:       row.Key,
			Version:   row.Version,
			ActorKind: MessageActionActorAgent,
			ActorID:   agentID,
			Reason:    parsed.Reason,
			Details:   details,
			Timestamp: now,
		})
	}

	writeJSON(w, http.StatusOK, WithdrawSecretRequestsResponse{
		Withdrawn:        s.secretRequestViewsFor(ctx, res.Withdrawn),
		AlreadyWithdrawn: s.secretRequestViewsFor(ctx, res.AlreadyWithdrawn),
	})
}

func (s *Server) writeWithdrawSecretRequestsError(w http.ResponseWriter, ctx context.Context, agentID string, err error) {
	if errors.Is(err, store.ErrInvalidInput) {
		ValidationError(w, err.Error(), nil)
		return
	}
	var wdErr *store.WithdrawSecretRequestsError
	if !errors.As(err, &wdErr) || len(wdErr.Failures) == 0 {
		writeErrorFromErr(w, err, "")
		return
	}
	failures := make([]map[string]interface{}, 0, len(wdErr.Failures))
	msgs := make([]string, 0, len(wdErr.Failures))
	for _, f := range wdErr.Failures {
		msg := fmt.Sprintf("%s: %s", f.Key, f.Reason)
		msgs = append(msgs, msg)
		entry := map[string]interface{}{
			"key":     f.Key,
			"reason":  f.Reason,
			"message": msg,
		}
		ev := &MessageActionEvent{
			AgentID:   agentID,
			Key:       f.Key,
			ActorKind: MessageActionActorAgent,
			ActorID:   agentID,
			Reason:    f.Reason,
		}
		if f.Request != nil {
			entry["secretRequest"] = secretRequestView(f.Request, nil)
			ev.RowID = f.Request.ID
			ev.MessageID = f.Request.MessageID
			ev.ProjectID = f.Request.ProjectID
			ev.AgentName = f.Request.AgentName
			ev.Version = f.Request.Version
		}
		s.emitSecretRequestRefused(ctx, ev)
		failures = append(failures, entry)
	}
	writeError(w, http.StatusConflict, ErrCodeWithdrawFailed, strings.Join(msgs, "; "),
		map[string]interface{}{"failures": failures})
}

// handleAgentSecretRequestList serves GET /api/v1/agent/secret-requests.
func (s *Server) handleAgentSecretRequestList(w http.ResponseWriter, r *http.Request, agentIdent AgentIdentity) {
	ctx := r.Context()
	q := r.URL.Query()

	var state store.SecretRequestListState
	switch raw := q.Get("state"); raw {
	case "", string(store.SecretRequestListStatePending):
		state = store.SecretRequestListStatePending
	case string(store.SecretRequestListStateFulfilled):
		state = store.SecretRequestListStateFulfilled
	case string(store.SecretRequestListStateWithdrawn):
		state = store.SecretRequestListStateWithdrawn
	case string(store.SecretRequestListStateDismissed):
		state = store.SecretRequestListStateDismissed
	case string(store.SecretRequestListStateExpired):
		state = store.SecretRequestListStateExpired
	case string(store.SecretRequestListStateAll):
		state = store.SecretRequestListStateAll
	default:
		ValidationError(w, fmt.Sprintf("invalid state %q: must be pending, fulfilled, withdrawn, dismissed, expired, or all", raw), nil)
		return
	}

	limit := store.DefaultSecretRequestListLimit
	if limitRaw := q.Get("limit"); limitRaw != "" {
		n, err := strconv.Atoi(limitRaw)
		if err != nil || n < 1 || n > store.MaxSecretRequestListLimit {
			ValidationError(w, fmt.Sprintf("limit must be an integer between 1 and %d", store.MaxSecretRequestListLimit), nil)
			return
		}
		limit = n
	}

	res, err := s.store.ListAgentSecretRequests(ctx, &store.ListAgentSecretRequestsOptions{
		AgentID: agentIdent.ID(),
		State:   state,
		Cursor:  q.Get("cursor"),
		Limit:   limit,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, err.Error(), nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, AgentSecretRequestListResponse{
		Items:      s.secretRequestViewsFor(ctx, res.Items),
		NextCursor: res.NextCursor,
	})
}
