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
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Starred messages in native chat (web.chat_starred_messages):
//
//	PUT    /api/v1/chat/stars/{messageId}   star a message (body: conversationKey)
//	DELETE /api/v1/chat/stars/{messageId}   remove the caller's star (always 204)
//	GET    /api/v1/chat/stars               one conversation's stars, newest first
//	GET    /api/v1/chat/stars/state         which of the given messages are starred
//	GET    /api/v1/chat/stars/{messageId}   the current detail of one star
//
// The caller is always the authenticated user; there is no user selector.
// Every read and add resolves the conversation's current read access and
// native history selector (chat_read_target.go). A missing, deleted,
// out-of-scope, unstarred or unreadable target is one shared 404. A user
// access token needs inbox:write to add or remove and inbox:read to read,
// for the conversation's project. While the experiment is off every route
// answers 404. Phase 1 serves project threads; a DM key is the shared 404.

const (
	chatStarsDefaultLimit = 30
	chatStarsMaxLimit     = 100
	chatStarsStateMaxIDs  = 200
	chatStarExcerptMax    = 160
	// chatStarsListBinding is the cursor binding endpoint of the list; its
	// version suffix is the schema version of the sealed position.
	chatStarsListBinding = "chat.stars.list.v1"
	// chatStarMaxKeyLen bounds a conversation key in a request.
	chatStarMaxKeyLen = 256
	// chatStarBodyLimit is the request body limit of PUT, as other chat
	// mutations.
	chatStarBodyLimit = 1 << 20
)

// starItemResponse is one starred message as the API returns it. Every
// field is derived from the current message and conversation.
type starItemResponse struct {
	MessageID        string    `json:"messageId"`
	ConversationKey  string    `json:"conversationKey"`
	ConversationKind string    `json:"conversationKind"`
	ProjectID        string    `json:"projectId,omitempty"`
	ProjectName      string    `json:"projectName,omitempty"`
	ConversationName string    `json:"conversationName"`
	SenderName       string    `json:"senderName"`
	MessageCreatedAt time.Time `json:"messageCreatedAt"`
	StarredAt        time.Time `json:"starredAt"`
	Excerpt          string    `json:"excerpt"`
	Edited           bool      `json:"edited"`
	Permalink        string    `json:"permalink"`
}

// starPageResponse is one page of the star list.
type starPageResponse struct {
	Items      []starItemResponse `json:"items"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// starPutRequest is the body of PUT /api/v1/chat/stars/{messageId}.
type starPutRequest struct {
	ConversationKey string `json:"conversationKey"`
}

// starPutResponse is the answer to a successful PUT.
type starPutResponse struct {
	Starred   bool      `json:"starred"`
	StarredAt time.Time `json:"starredAt"`
}

// starStateResponse is the answer to GET /api/v1/chat/stars/state.
type starStateResponse struct {
	StarredMessageIDs []string `json:"starredMessageIds"`
}

// starCall is the resolved context of one star request.
type starCall struct {
	user  UserIdentity
	token *ScopedUserIdentity
	wcs   WebChatStore
	stars MessageStarStore
}

// writeStarNotFound writes the shared 404 of the star routes.
func writeStarNotFound(w http.ResponseWriter) {
	NotFound(w, "Message")
}

// handleChatStars serves /api/v1/chat/stars (the list).
func (s *Server) handleChatStars(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(experiments.ChatStarredMessages) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	call, ok := s.beginStarCall(w, r, permInboxRead)
	if !ok {
		return
	}
	s.handleStarList(w, r, call)
}

// handleChatStarRoutes serves /api/v1/chat/stars/state and
// /api/v1/chat/stars/{messageId}. The literal state route is matched
// before the message ID.
func (s *Server) handleChatStarRoutes(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(experiments.ChatStarredMessages) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/chat/stars/")
	if rest == "state" {
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		call, ok := s.beginStarCall(w, r, permInboxRead)
		if !ok {
			return
		}
		s.handleStarState(w, r, call)
		return
	}
	if rest == "" || strings.Contains(rest, "/") {
		http.NotFound(w, r)
		return
	}
	var perm string
	switch r.Method {
	case http.MethodPut, http.MethodDelete:
		perm = permInboxWrite
	case http.MethodGet:
		perm = permInboxRead
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		return
	}
	call, ok := s.beginStarCall(w, r, perm)
	if !ok {
		return
	}
	messageID, ok := parseStarMessageID(rest)
	if !ok {
		BadRequest(w, "invalid message ID")
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handleStarPut(w, r, call, messageID)
	case http.MethodDelete:
		s.handleStarDelete(w, r, call, messageID)
	default:
		s.handleStarDetail(w, r, call, messageID)
	}
}

// beginStarCall resolves the caller and the star store, or writes the
// refusal: 403 without a user, 503 without a capable chat store, 403 for
// a token lacking permissionID.
func (s *Server) beginStarCall(w http.ResponseWriter, r *http.Request, permissionID string) (starCall, bool) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.ID() == "" {
		Forbidden(w)
		return starCall{}, false
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	var stars MessageStarStore
	if wcs != nil {
		stars = messageStarStoreFrom(wcs)
	}
	if stars == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Starred messages are not available", nil)
		return starCall{}, false
	}
	token, ok := s.requireChatSelector(w, r, permissionID)
	if !ok {
		return starCall{}, false
	}
	return starCall{user: user, token: token, wcs: wcs, stars: stars}, true
}

// parseStarMessageID returns the canonical (lowercase) form of a message
// UUID.
func parseStarMessageID(raw string) (string, bool) {
	id, err := uuid.Parse(raw)
	if err != nil || len(raw) != 36 {
		return "", false
	}
	return id.String(), true
}

// validStarConversationKey reports whether key is well-formed enough to
// look up.
func validStarConversationKey(key string) bool {
	return key != "" && len(key) <= chatStarMaxKeyLen && !strings.ContainsAny(key, "/?#")
}

// starQuery validates the query string: only allowed names, each at most
// once except repeatable. It writes 400 and returns false on violation.
func starQuery(w http.ResponseWriter, r *http.Request, allowed []string, repeatable string) (url.Values, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		BadRequest(w, "invalid query string")
		return nil, false
	}
	for name, vals := range q {
		known := false
		for _, a := range allowed {
			if a == name {
				known = true
				break
			}
		}
		if !known {
			BadRequest(w, "unknown query parameter: "+name)
			return nil, false
		}
		if len(vals) > 1 && name != repeatable {
			BadRequest(w, "duplicate query parameter: "+name)
			return nil, false
		}
	}
	return q, true
}

// resolveStarConversation returns the read target of a conversation the
// caller may read, or writes the refusal: the shared 404 for a missing or
// unreadable conversation (and, in phase 1, any DM), 500 when a lookup
// failed, and the read switch's own errors otherwise.
func (s *Server) resolveStarConversation(w http.ResponseWriter, r *http.Request, call starCall, permissionID, key string) (chatReadTarget, bool) {
	ctx := r.Context()
	if strings.HasPrefix(key, "dm:") {
		writeStarNotFound(w)
		return chatReadTarget{}, false
	}
	topic, project, rerr := s.authorizeChatThreadRead(r, call.wcs, key)
	if rerr != nil {
		writeStarReadError(w, rerr)
		return chatReadTarget{}, false
	}
	if call.token != nil {
		ok, err := s.chatTokenMayUse(ctx, call.token, permissionID, key, topic.ProjectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to check access", nil)
			return chatReadTarget{}, false
		}
		if !ok {
			logReferenceRefused(ctx, logging.RequestPath(r), chatTokenRefusalReason, call.token)
			writeStarNotFound(w)
			return chatReadTarget{}, false
		}
	}
	target, rerr := s.resolveChatReadTarget(ctx, call.wcs, key, topic, GetIdentityFromContext(ctx))
	if rerr != nil {
		writeStarReadError(w, rerr)
		return chatReadTarget{}, false
	}
	target.Project = project
	return target, true
}

// writeStarReadError maps a resolver error to the star routes' answer: a
// failed lookup is never turned into a 404.
func writeStarReadError(w http.ResponseWriter, rerr *chatReadError) {
	switch rerr.kind {
	case chatReadNotFound:
		if rerr.err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to look up conversation", nil)
			return
		}
		writeStarNotFound(w)
	case chatReadUnauthenticated:
		Unauthorized(w)
	case chatReadAbsent:
		writeStarNotFound(w)
	default:
		writeError(w, rerr.status, rerr.code, rerr.message, nil)
	}
}

// handleStarPut implements PUT /api/v1/chat/stars/{messageId}.
func (s *Server) handleStarPut(w http.ResponseWriter, r *http.Request, call starCall, messageID string) {
	ctx := r.Context()
	if _, ok := starQuery(w, r, nil, ""); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, chatStarBodyLimit)
	var body starPutRequest
	if err := readJSONStrict(r, &body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}
	if !validStarConversationKey(body.ConversationKey) {
		BadRequest(w, "conversationKey is required")
		return
	}
	target, ok := s.resolveStarConversation(w, r, call, permInboxWrite, body.ConversationKey)
	if !ok {
		return
	}
	projectID := ""
	if target.Topic != nil {
		projectID = target.Topic.ProjectID
	}
	rec, changed, err := call.stars.AddMessageStar(ctx, call.user.ID(), StarTarget{
		MessageID: messageID,
		ProjectID: projectID,
		Selector:  target.starSelector(),
	})
	if errors.Is(err, store.ErrNotFound) {
		writeStarNotFound(w)
		return
	}
	if err != nil {
		slog.Error("chat stars: add failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to star message", nil)
		return
	}
	if changed {
		s.events.PublishChatStarsChanged(ctx, call.user.ID())
	}
	writeJSON(w, http.StatusOK, starPutResponse{Starred: true, StarredAt: rec.StarredAt.UTC()})
}

// handleStarDelete implements DELETE /api/v1/chat/stars/{messageId}. It
// removes only the caller's own association, whatever the message's
// state or the caller's current access, and always answers 204. A
// project-boundary token removes a row only when the row's project is
// inside its boundary; otherwise the answer is the same empty 204.
func (s *Server) handleStarDelete(w http.ResponseWriter, r *http.Request, call starCall, messageID string) {
	ctx := r.Context()
	if _, ok := starQuery(w, r, nil, ""); !ok {
		return
	}
	if call.token != nil {
		rec, err := call.stars.GetMessageStar(ctx, call.user.ID(), messageID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to remove star", nil)
			return
		}
		if rec == nil || !s.newSelfScopeCheck(ctx, call.token, permInboxWrite).allows(rec.ProjectID) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	removed, err := call.stars.RemoveMessageStar(ctx, call.user.ID(), messageID)
	if err != nil {
		slog.Error("chat stars: remove failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to remove star", nil)
		return
	}
	if removed {
		s.events.PublishChatStarsChanged(ctx, call.user.ID())
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStarDetail implements GET /api/v1/chat/stars/{messageId}: the
// current detail of the caller's star, used before a jump.
func (s *Server) handleStarDetail(w http.ResponseWriter, r *http.Request, call starCall, messageID string) {
	ctx := r.Context()
	if _, ok := starQuery(w, r, nil, ""); !ok {
		return
	}
	rec, err := call.stars.GetMessageStar(ctx, call.user.ID(), messageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to read star", nil)
		return
	}
	if rec == nil {
		writeStarNotFound(w)
		return
	}
	target, ok := s.resolveStarConversation(w, r, call, permInboxRead, rec.ConversationKey)
	if !ok {
		return
	}
	items, err := s.hydrateStarItems(ctx, call.wcs, target, []StarRecord{*rec})
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to read star", nil)
		return
	}
	if len(items) == 0 {
		writeStarNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, items[0])
}

// handleStarList implements GET /api/v1/chat/stars?conversationKey=…
func (s *Server) handleStarList(w http.ResponseWriter, r *http.Request, call starCall) {
	ctx := r.Context()
	q, ok := starQuery(w, r, []string{"conversationKey", "limit", "cursor"}, "")
	if !ok {
		return
	}
	key := q.Get("conversationKey")
	if !validStarConversationKey(key) {
		BadRequest(w, "conversationKey is required")
		return
	}
	limit := chatStarsDefaultLimit
	if raw, set := q["limit"]; set {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > chatStarsMaxLimit {
			BadRequest(w, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	target, ok := s.resolveStarConversation(w, r, call, permInboxRead, key)
	if !ok {
		return
	}

	binding := scopedCursorBinding(chatStarsListBinding, struct {
		ConversationKey string `json:"conversationKey"`
	}{key}, GetIdentityFromContext(ctx))
	var after *StarPosition
	if cursor := q.Get("cursor"); cursor != "" {
		pos, err := s.openStarCursor(cursor, binding)
		if err != nil {
			writeErrorFromErr(w, errInvalidCursor, "")
			return
		}
		after = &pos
	}

	page, err := call.stars.ListMessageStarCandidates(ctx, StarFilter{
		UserID:   call.user.ID(),
		Selector: target.starSelector(),
		After:    after,
		Limit:    limit,
	})
	if err != nil {
		slog.Error("chat stars: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to list stars", nil)
		return
	}
	items, err := s.hydrateStarItems(ctx, call.wcs, target, page.Items)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to list stars", nil)
		return
	}
	resp := starPageResponse{Items: items}
	if page.Next != nil {
		sealed, err := s.listCursorSealer.Seal(authorizedListCursor(page.Next.StarredAt, page.Next.MessageID, binding), binding)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to list stars", nil)
			return
		}
		resp.NextCursor = sealed
	}
	writeJSON(w, http.StatusOK, resp)
}

// openStarCursor opens a sealed list cursor and validates its position:
// a UTC instant at the stored precision and a canonical message UUID.
func (s *Server) openStarCursor(sealed, binding string) (StarPosition, error) {
	inner, err := openAndValidateListCursor(s.listCursorSealer, sealed, binding)
	if err != nil {
		return StarPosition{}, err
	}
	raw, err := base64.URLEncoding.DecodeString(inner)
	if err != nil {
		return StarPosition{}, errInvalidCursor
	}
	parts := strings.SplitN(string(raw), ",", 3)
	if len(parts) != 3 {
		return StarPosition{}, errInvalidCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil || !at.Equal(at.Truncate(starTimePrecision)) {
		return StarPosition{}, errInvalidCursor
	}
	id, ok := parseStarMessageID(parts[1])
	if !ok || id != parts[1] {
		return StarPosition{}, errInvalidCursor
	}
	return StarPosition{StarredAt: at.UTC(), MessageID: id}, nil
}

// handleStarState implements GET /api/v1/chat/stars/state?messageId=…:
// which of up to 200 messages the caller starred, limited to live
// messages in conversations the caller can currently read.
func (s *Server) handleStarState(w http.ResponseWriter, r *http.Request, call starCall) {
	ctx := r.Context()
	q, ok := starQuery(w, r, []string{"messageId"}, "messageId")
	if !ok {
		return
	}
	raw := q["messageId"]
	if len(raw) < 1 || len(raw) > chatStarsStateMaxIDs {
		BadRequest(w, "messageId must be given 1 to 200 times")
		return
	}
	ids := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, v := range raw {
		id, ok := parseStarMessageID(v)
		if !ok {
			BadRequest(w, "invalid message ID")
			return
		}
		if seen[id] {
			BadRequest(w, "duplicate message ID")
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}

	refs, err := call.stars.ListStarredMessageIDs(ctx, call.user.ID(), ids)
	if err != nil {
		slog.Error("chat stars: state failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to read star state", nil)
		return
	}

	// Request-local decisions, one per conversation; never kept beyond
	// this request.
	decisions := map[string]starStateDecision{}
	starred := map[string]bool{}
	for _, ref := range refs {
		d, seen := decisions[ref.ConversationKey]
		if !seen {
			var rerr *chatReadError
			d, rerr = s.decideStarState(r, call, ref.ConversationKey)
			if rerr != nil {
				writeStarReadError(w, rerr)
				return
			}
			decisions[ref.ConversationKey] = d
		}
		if d.allowed && ref.matches(d.target.starSelector()) {
			starred[ref.MessageID] = true
		}
	}
	out := make([]string, 0, len(starred))
	for _, id := range ids {
		if starred[id] {
			out = append(out, id)
		}
	}
	writeJSON(w, http.StatusOK, starStateResponse{StarredMessageIDs: out})
}

// starStateDecision is the state route's read decision for one
// conversation.
type starStateDecision struct {
	target  chatReadTarget
	allowed bool
}

// decideStarState decides read access to one conversation for the state
// route without writing a response: a refusal is allowed=false, a failed
// lookup is an error.
//
// The state route answers for many conversations at once, so one
// conversation whose read target cannot be resolved (read-switch drift: a
// thread with no conversation record, the resolver's 409) is treated as
// not readable and logged, rather than failing the whole batch; only a
// real lookup failure (a 500) fails the request. The single-conversation
// routes (list, add, detail) keep answering the resolver's error.
func (s *Server) decideStarState(r *http.Request, call starCall, key string) (starStateDecision, *chatReadError) {
	type decision = starStateDecision
	ctx := r.Context()
	if strings.HasPrefix(key, "dm:") {
		return decision{}, nil // phase 1: threads only
	}
	topic, _, rerr := s.authorizeChatThreadRead(r, call.wcs, key)
	if rerr != nil {
		if rerr.kind == chatReadNotFound && rerr.err == nil {
			return decision{}, nil
		}
		return decision{}, rerr
	}
	if call.token != nil {
		ok, err := s.chatTokenMayUse(ctx, call.token, permInboxRead, key, topic.ProjectID)
		if err != nil {
			return decision{}, &chatReadError{kind: chatReadNotFound, err: err}
		}
		if !ok {
			return decision{}, nil
		}
	}
	target, rerr := s.resolveChatReadTarget(ctx, call.wcs, key, topic, GetIdentityFromContext(ctx))
	if rerr != nil {
		if rerr.kind == chatReadFailure && rerr.status != http.StatusInternalServerError {
			slog.Warn("chat stars: state treats a conversation as not readable: read target unresolved",
				"conversation_key", key, "code", rerr.code)
			return decision{}, nil
		}
		return decision{}, rerr
	}
	return decision{target: target, allowed: true}, nil
}

// hydrateStarItems builds the API form of recs, which are stars in
// target's conversation, from the current message, extension, sender and
// conversation records, in batch. A record whose message is gone, deleted
// or outside the target's history scope is omitted.
func (s *Server) hydrateStarItems(ctx context.Context, wcs WebChatStore, target chatReadTarget, recs []StarRecord) ([]starItemResponse, error) {
	out := make([]starItemResponse, 0, len(recs))
	if len(recs) == 0 {
		return out, nil
	}
	ids := make([]string, len(recs))
	for i, rec := range recs {
		ids[i] = rec.MessageID
	}
	msgs, err := s.store.GetMessagesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	exts, err := wcs.GetMessageExts(ctx, ids)
	if err != nil {
		return nil, err
	}

	var userIDs []string
	for _, msg := range msgs {
		if strings.HasPrefix(msg.Sender, "user:") && msg.SenderID != "" {
			userIDs = append(userIDs, msg.SenderID)
		}
	}
	users := map[string]*store.User{}
	if len(userIDs) > 0 {
		if users, err = s.store.GetUsersByIDs(ctx, userIDs); err != nil {
			return nil, err
		}
	}

	var projectID, projectName, conversationName string
	if target.Topic != nil {
		projectID = target.Topic.ProjectID
		conversationName = target.Topic.Name
		// The project the read check loaded (resolveStarConversation): no
		// second lookup a concurrent delete could fail.
		if target.Project == nil {
			return nil, errors.New("chat stars: read target has no project")
		}
		projectName = target.Project.Name
	}

	for _, rec := range recs {
		msg, ok := msgs[rec.MessageID]
		if !ok || msg == nil || !target.containsMessage(msg) {
			continue
		}
		ext := exts[rec.MessageID]
		if ext != nil && ext.DeletedAt != nil {
			continue
		}
		item := starItemResponse{
			MessageID:        rec.MessageID,
			ConversationKey:  target.Key,
			ConversationKind: "thread",
			ProjectID:        projectID,
			ProjectName:      projectName,
			ConversationName: conversationName,
			SenderName:       starSenderName(msg, users),
			MessageCreatedAt: msg.CreatedAt.UTC(),
			StarredAt:        rec.StarredAt.UTC(),
			Excerpt:          starExcerpt(msg.Msg),
			Edited:           ext != nil && ext.EditedAt != nil,
			Permalink:        starPermalink(projectID, target.Key, rec.MessageID),
		}
		if target.IsDM {
			item.ConversationKind = "dm"
		}
		out = append(out, item)
	}
	return out, nil
}

// starSenderName is the display label of a message's sender: a user's
// display name (else the address part of the sender), an agent's slug.
func starSenderName(msg *store.Message, users map[string]*store.User) string {
	switch {
	case strings.HasPrefix(msg.Sender, "user:"):
		if u, ok := users[msg.SenderID]; ok && u != nil && strings.TrimSpace(u.DisplayName) != "" {
			return u.DisplayName
		}
		return strings.TrimPrefix(msg.Sender, "user:")
	case strings.HasPrefix(msg.Sender, "agent:"):
		return strings.TrimPrefix(msg.Sender, "agent:")
	case msg.Sender == "":
		return "Someone"
	default:
		return msg.Sender
	}
}

// starExcerpt is at most chatStarExcerptMax code points of the message's
// whitespace-collapsed source text; an empty text (an attachment-only
// message) is "Attachment". It is plain text, never markup.
func starExcerpt(content string) string {
	collapsed := strings.Join(strings.Fields(content), " ")
	if collapsed == "" {
		return "Attachment"
	}
	if utf8.RuneCountInString(collapsed) <= chatStarExcerptMax {
		return collapsed
	}
	runes := []rune(collapsed)
	return strings.TrimRight(string(runes[:chatStarExcerptMax-1]), " ") + "…"
}

// starPermalink is the relative, same-origin link to a thread message
// (the web chat's thread route plus #msg-<id>).
func starPermalink(projectID, key, messageID string) string {
	return "/chat/space/" + url.PathEscape(projectID) + "/thread/" + url.PathEscape(key) + "#msg-" + url.PathEscape(messageID)
}

// messageStarStoreProblem returns why starred messages cannot be served
// while the experiment is on, or "" when they can (or the experiment is
// off, or native chat is disabled).
func (s *Server) messageStarStoreProblem() string {
	if !s.nativeChatEnabled() || !s.experimentEnabled(experiments.ChatStarredMessages) {
		return ""
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return "no web chat store is configured"
	}
	if messageStarStoreFrom(wcs) == nil {
		return "the web chat store does not support starred messages"
	}
	return ""
}

// validateMessageStarStore logs, at startup, an experiment that is on
// without a capable chat store; its routes answer 503 until fixed.
func (s *Server) validateMessageStarStore() {
	if problem := s.messageStarStoreProblem(); problem != "" {
		slog.Error("chat stars: "+experiments.ChatStarredMessages+" is on but starred messages are unavailable", "reason", problem)
	}
}
