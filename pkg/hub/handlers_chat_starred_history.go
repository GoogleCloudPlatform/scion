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
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// The starred-only variant of conversation history (the filtered center of
// web.chat_starred_messages):
//
//	GET /api/v1/chat/conversations/{key}/messages?starred=true[&limit][&cursor][&around]
//
// It answers the ordinary history response (chatHistoryPage) holding only
// the caller's starred live messages of the conversation, newest first,
// with full bodies, attachments, artifacts, reply previews and extension
// data. totalCount is the number of those starred messages. The
// conversation is resolved as the other star reads resolve it (the shared
// read target, inbox:read for a token), and the continuation is a sealed
// cursor bound to the conversation, the caller and the filter, so it is
// not interchangeable with an ordinary history cursor. While the
// experiment is off the variant is a 404; it never falls back to
// unfiltered history.

const (
	// chatStarredHistoryBinding is the cursor binding endpoint of the
	// variant; its version suffix is the schema version of the position.
	chatStarredHistoryBinding = "chat.history.starred.v1"
	chatStarredHistoryDefault = 50
	chatStarredHistoryMax     = 200
)

// rawQueryHasKey reports whether the raw query string names key, whether
// or not its pair decodes. URL.Query drops a pair it cannot decode
// (starred=%zz) or that holds a ';' (x=1;starred=true), which would serve
// a malformed filter as unfiltered history; the starred variant must see
// it and answer 400. Pairs are split on both '&' and ';'.
func rawQueryHasKey(rawQuery, key string) bool {
	pairs := strings.FieldsFunc(rawQuery, func(r rune) bool { return r == '&' || r == ';' })
	for _, pair := range pairs {
		name, _, _ := strings.Cut(pair, "=")
		if name == key {
			return true
		}
		if unescaped, err := url.QueryUnescape(name); err == nil && unescaped == key {
			return true
		}
	}
	return false
}

// handleStarredHistory implements the starred=true variant of
// handleConversationHistory.
func (s *Server) handleStarredHistory(w http.ResponseWriter, r *http.Request, key string) {
	// The experiment is checked before anything else: off, the filter is a
	// 404, never ignored.
	if !s.experimentEnabled(experiments.ChatStarredMessages) {
		http.NotFound(w, r)
		return
	}
	q, ok := starQuery(w, r, []string{"starred", "limit", "cursor", "around"}, "")
	if !ok {
		return
	}
	if q.Get("starred") != "true" {
		BadRequest(w, "starred must be true")
		return
	}
	call, ok := s.beginStarCall(w, r, permInboxRead)
	if !ok {
		return
	}
	ctx := r.Context()

	limit := chatStarredHistoryDefault
	if raw, set := q["limit"]; set {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > chatStarredHistoryMax {
			BadRequest(w, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	_, hasCursor := q["cursor"]
	_, hasAround := q["around"]
	if hasCursor && hasAround {
		BadRequest(w, "cursor and around cannot be combined")
		return
	}
	var aroundID string
	if hasAround {
		id, ok := parseStarMessageID(q.Get("around"))
		if !ok {
			BadRequest(w, "invalid around message ID")
			return
		}
		aroundID = id
	}
	if !validStarConversationKey(key) {
		BadRequest(w, "invalid conversation key")
		return
	}

	sc, refusal, rerr := s.resolveStarScope(r, call, permInboxRead, key)
	switch {
	case rerr != nil && rerr.kind == chatReadAbsent:
		// A never-used DM has no messages, so no starred ones; an around
		// target there cannot be a starred message (the shared 404).
		if aroundID != "" {
			writeStarNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, chatHistoryResponse{Messages: []store.Message{}})
		return
	case rerr != nil:
		writeStarReadError(w, rerr)
		return
	case refusal != "":
		logReferenceRefused(ctx, logging.RequestPath(r), refusal, GetIdentityFromContext(ctx))
		writeStarNotFound(w)
		return
	}

	binding := scopedCursorBinding(chatStarredHistoryBinding, struct {
		ConversationKey string `json:"conversationKey"`
		Starred         bool   `json:"starred"`
	}{key, true}, GetIdentityFromContext(ctx))
	var cursor *MessagePosition
	if hasCursor {
		pos, err := s.openStarredHistoryCursor(q.Get("cursor"), binding)
		if err != nil {
			writeErrorFromErr(w, errInvalidCursor, "")
			return
		}
		cursor = &pos
	}

	base := StarHistoryQuery{UserID: call.user.ID(), Selector: sc.starSelector()}
	var positions []MessagePosition
	var next *MessagePosition
	var total int
	if aroundID != "" {
		var found bool
		positions, next, total, found, ok = s.starredHistoryAround(w, r, call, sc, base, aroundID, limit)
		if !ok {
			return
		}
		if !found {
			writeStarNotFound(w)
			return
		}
	} else {
		pageQuery := base
		pageQuery.Cursor = cursor
		pageQuery.Limit = limit
		page, err := call.stars.ListStarredHistory(ctx, pageQuery)
		if err != nil {
			slog.Error("chat stars: starred history failed", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to fetch messages", nil)
			return
		}
		positions, next, total = page.Items, page.Next, page.Total
	}

	items, err := s.loadStarredHistoryMessages(r, sc, positions)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to fetch messages", nil)
		return
	}
	result := &store.ListResult[store.Message]{Items: items, TotalCount: total}
	if next != nil {
		sealed, err := s.listCursorSealer.Seal(authorizedListCursor(next.Created, next.ID, binding), binding)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to fetch messages", nil)
			return
		}
		result.NextCursor = sealed
	}
	// No canPost: the filtered center has no composer.
	writeJSON(w, http.StatusOK, s.chatHistoryPage(ctx, call.wcs, key, sc.ConversationID, result, nil))
}

// starredHistoryAround returns the page around a starred message: up to
// limit/2 older starred messages, the anchor, and the newer ones filling
// the rest, newest first, as ordinary history builds an around page. The
// anchor must be one of the caller's live starred messages in the
// conversation (found false otherwise). next resumes older than the page.
// It writes the response and returns ok false on a failure.
func (s *Server) starredHistoryAround(w http.ResponseWriter, r *http.Request, call starCall, sc starConversation, base StarHistoryQuery, aroundID string, limit int) (positions []MessagePosition, next *MessagePosition, total int, found, ok bool) {
	ctx := r.Context()
	fail := func(err error) ([]MessagePosition, *MessagePosition, int, bool, bool) {
		slog.Error("chat stars: starred history around failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to fetch messages", nil)
		return nil, nil, 0, false, false
	}
	refs, err := call.stars.ListStarredMessageIDs(ctx, call.user.ID(), []string{aroundID})
	if err != nil {
		return fail(err)
	}
	if len(refs) != 1 || !refs[0].matches(sc.starSelector()) {
		return nil, nil, 0, false, true
	}
	anchor, err := s.store.GetMessage(ctx, aroundID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, 0, false, true
	}
	if err != nil {
		return fail(err)
	}
	if !sc.containsMessage(anchor) {
		return nil, nil, 0, false, true
	}

	olderLimit := limit / 2
	newerLimit := limit - olderLimit - 1
	// Both halves start at the anchor's (created, id) position, so a
	// starred message sharing the anchor's instant falls in one of them
	// by its ID. The older half is always queried (one row when the page
	// has no room for it) for the total and to know whether older
	// messages remain.
	anchorPos := MessagePosition{Created: anchor.CreatedAt.UTC(), ID: aroundID}
	olderQuery := base
	olderQuery.Cursor = &anchorPos
	olderQuery.Limit = max(olderLimit, 1)
	older, err := call.stars.ListStarredHistory(ctx, olderQuery)
	if err != nil {
		return fail(err)
	}
	newer := StarHistoryPage{}
	if newerLimit > 0 {
		newerQuery := base
		newerQuery.Cursor = &anchorPos
		newerQuery.Ascending = true
		newerQuery.Limit = newerLimit
		if newer, err = call.stars.ListStarredHistory(ctx, newerQuery); err != nil {
			return fail(err)
		}
	}

	positions = make([]MessagePosition, 0, len(newer.Items)+1+len(older.Items))
	for i := len(newer.Items) - 1; i >= 0; i-- {
		positions = append(positions, newer.Items[i])
	}
	positions = append(positions, anchorPos)
	switch {
	case olderLimit > 0:
		positions = append(positions, older.Items...)
		next = older.Next
	case len(older.Items) > 0:
		// No room for older messages, but some remain: resume past the
		// anchor.
		next = &anchorPos
	}
	return positions, next, older.Total, true, true
}

// loadStarredHistoryMessages loads the messages at positions, in order. A
// message that disappeared since the star query, or is no longer in the
// conversation's history scope, is left out.
func (s *Server) loadStarredHistoryMessages(r *http.Request, sc starConversation, positions []MessagePosition) ([]store.Message, error) {
	items := make([]store.Message, 0, len(positions))
	if len(positions) == 0 {
		return items, nil
	}
	ids := make([]string, len(positions))
	for i, p := range positions {
		ids[i] = p.ID
	}
	msgs, err := s.store.GetMessagesByIDs(r.Context(), ids)
	if err != nil {
		slog.Error("chat stars: load starred history messages failed", "error", err)
		return nil, err
	}
	for _, id := range ids {
		msg := msgs[id]
		if msg == nil || !sc.containsMessage(msg) {
			continue
		}
		items = append(items, *msg)
	}
	return items, nil
}

// openStarredHistoryCursor opens a sealed starred-history cursor and
// validates its position: a UTC instant and a canonical message UUID.
func (s *Server) openStarredHistoryCursor(sealed, binding string) (MessagePosition, error) {
	inner, err := openAndValidateListCursor(s.listCursorSealer, sealed, binding)
	if err != nil {
		return MessagePosition{}, err
	}
	raw, err := base64.URLEncoding.DecodeString(inner)
	if err != nil {
		return MessagePosition{}, errInvalidCursor
	}
	parts := strings.SplitN(string(raw), ",", 3)
	if len(parts) != 3 || parts[2] != binding {
		return MessagePosition{}, errInvalidCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return MessagePosition{}, errInvalidCursor
	}
	id, ok := parseStarMessageID(parts[1])
	if !ok || id != parts[1] {
		return MessagePosition{}, errInvalidCursor
	}
	return MessagePosition{Created: at.UTC(), ID: id}, nil
}
