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

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// The shared chat read-target resolver. Native history
// (handleConversationHistory) and the starred-message routes both use it,
// so the thread read check and the history selector (channel plus the
// conversation-ID or legacy thread-ID match, chosen by the read switch)
// cannot drift between them. Nothing here writes an HTTP response: each
// caller maps a chatReadError to its own answer.

// chatReadErrorKind classifies a chatReadError.
type chatReadErrorKind int

const (
	// chatReadNotFound: the conversation is missing or the caller may not
	// read it. err is set when a lookup failed for another reason than
	// absence; history still answers 404 for it, other callers 500.
	chatReadNotFound chatReadErrorKind = iota + 1
	// chatReadUnauthenticated: the request has no identity.
	chatReadUnauthenticated
	// chatReadAbsent: the read switch is on and the DM has no
	// conversation yet (a never-used DM); its history is empty.
	chatReadAbsent
	// chatReadFailure: status, code and message describe the answer.
	chatReadFailure
)

// chatReadError is a refusal or failure of the read-target resolver.
type chatReadError struct {
	kind    chatReadErrorKind
	err     error
	status  int
	code    string
	message string
}

func (e *chatReadError) Error() string {
	switch e.kind {
	case chatReadNotFound:
		if e.err != nil {
			return "chat read target: lookup failed: " + e.err.Error()
		}
		return "chat read target: not found"
	case chatReadUnauthenticated:
		return "chat read target: unauthenticated"
	case chatReadAbsent:
		return "chat read target: conversation absent"
	default:
		return "chat read target: " + e.message
	}
}

// writeHistory writes e the way native history always answered it: a
// missing or unreadable thread (including a failed lookup) is "Thread not
// found", and a never-used DM (chatReadAbsent) is an empty page. It
// answers every kind, so a status-less error is a 500, never
// WriteHeader(0).
func (e *chatReadError) writeHistory(w http.ResponseWriter) {
	switch e.kind {
	case chatReadNotFound:
		NotFound(w, "Thread")
	case chatReadUnauthenticated:
		Unauthorized(w)
	case chatReadAbsent:
		writeJSON(w, http.StatusOK, chatHistoryResponse{Messages: []store.Message{}})
	default:
		if e.status == 0 {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to look up conversation", nil)
			return
		}
		writeError(w, e.status, e.code, e.message, nil)
	}
}

// chatReadTarget is the native history scope of one conversation.
type chatReadTarget struct {
	Key  string
	IsDM bool
	// Topic is the thread's topic (nil for a DM).
	Topic *WebChatTopic
	// Project is the thread's project as loaded by the read check (nil
	// when the caller did not set it). History leaves it nil for a DM; the
	// star routes set an agent DM's peer project (starConversation).
	Project *store.Project
	// Filter selects the conversation's history messages: Channel plus
	// ConversationID (read switch on) or ThreadID (off).
	Filter store.MessageFilter
	// ConversationID is the conversation the history is listed by when
	// the read switch resolved one ("" otherwise).
	ConversationID string
}

// starSelector returns the star-store form of the target's selector.
func (t chatReadTarget) starSelector() StarSelector {
	return StarSelector{
		ConversationKey: t.Key,
		Channel:         t.Filter.Channel,
		ConversationID:  t.Filter.ConversationID,
		ThreadID:        t.Filter.ThreadID,
	}
}

// containsMessage reports whether msg is in the target's history scope
// (the same check history applies to an around anchor).
func (t chatReadTarget) containsMessage(msg *store.Message) bool {
	f := t.Filter
	if f.Channel != "" && msg.Channel != f.Channel {
		return false
	}
	if f.ConversationID != "" && msg.ConversationID != f.ConversationID {
		return false
	}
	if f.ThreadID != "" && msg.ThreadID != f.ThreadID {
		return false
	}
	return true
}

// authorizeChatThreadRead returns the topic of the thread key and its
// project when the caller may read that project. A missing topic, a
// failed project lookup and a denial are chatReadNotFound; the denial is
// logged as history always logged it. It never writes a response. Callers
// reuse the returned project rather than loading it again, so a project
// deleted after this check cannot turn into a lookup failure later in the
// same request.
func (s *Server) authorizeChatThreadRead(r *http.Request, wcs WebChatStore, key string) (*WebChatTopic, *store.Project, *chatReadError) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	topic, err := wcs.GetTopic(ctx, key)
	if err != nil {
		return nil, nil, &chatReadError{kind: chatReadNotFound, err: err}
	}
	if topic == nil {
		return nil, nil, &chatReadError{kind: chatReadNotFound}
	}
	project, err := s.store.GetProject(ctx, topic.ProjectID)
	if err != nil {
		logReferenceRefused(ctx, logging.RequestPath(r), "topic project lookup failed: "+err.Error(), identity)
		rerr := &chatReadError{kind: chatReadNotFound}
		if !errors.Is(err, store.ErrNotFound) {
			rerr.err = err
		}
		return nil, nil, rerr
	}
	// A caller who may not read the project gets the same answer as for a
	// missing thread (authorizeRead's rule).
	if identity == nil {
		return nil, nil, &chatReadError{kind: chatReadUnauthenticated}
	}
	if s.authzService == nil {
		return nil, nil, &chatReadError{kind: chatReadNotFound}
	}
	resource := projectResource(project)
	decision := s.authzService.CheckAccess(ctx, identity, resource, ActionRead)
	if !decision.Allowed {
		logAuthzDenial(r, identity, resource, ActionRead, decision.Reason)
		return nil, nil, &chatReadError{kind: chatReadNotFound}
	}
	return topic, project, nil
}

// chatHistoryChannel is the channel history is limited to for identity:
// the web chat's channel, except for a project visitor (ptone/scion#4603),
// who reads its own DM with an agent through the hub API, never the web
// chat, and is admitted only to that DM (admitVisitorSubRoute), so its
// page lists every message of the conversation whatever its channel.
func chatHistoryChannel(identity Identity) string {
	if isVisitorIdentity(identity) {
		return ""
	}
	return "web"
}

// resolveChatReadTarget returns the history selector of an authorized
// conversation key. topic is the thread's topic (nil for a DM key).
//
// Phase 8 read-switch: when ConversationReadSwitch is ON, the
// conversation is resolved and history is queried by ConversationID
// instead of Channel+ThreadID. G3: the fallback to channel+thread is
// REMOVED; an unresolved thread conversation is a typed 409 so failures
// are observable, not silent.
func (s *Server) resolveChatReadTarget(ctx context.Context, wcs WebChatStore, key string, topic *WebChatTopic, identity Identity) (chatReadTarget, *chatReadError) {
	isDM := strings.HasPrefix(key, "dm:")
	target := chatReadTarget{Key: key, IsDM: isDM, Topic: topic}
	historyChannel := chatHistoryChannel(identity)

	ops := s.GetOperationalSettings()
	if ops == nil || !ops.ConversationEnvelopeSwitch() {
		target.Filter = store.MessageFilter{Channel: historyChannel, ThreadID: key}
		return target, nil
	}

	var convResult *messaging.ConversationResult
	if isDM {
		// DM key format: dm:<kind>:<id>:<kind>:<id> — exactly 5 parts.
		// Strict parse (B-3): never tolerate or normalise a DM key on the
		// derivation path. A 7-part key silently deriving from the first 5
		// would be an access path error after the S4 read-switch.
		parts := strings.Split(key, ":")
		if len(parts) != 5 {
			// G3 / AC-G3-4: a DM key with a part count other than 5 is
			// a parse failure, not a cache miss. Return a distinct error.
			slog.Warn("read-switch: DM key has invalid part count",
				"key", key, "parts", len(parts))
			return target, &chatReadError{kind: chatReadFailure, status: http.StatusConflict, code: ErrCodeInvalidDMKey,
				message: fmt.Sprintf("DM key must have exactly 5 colon-separated parts, got %d", len(parts))}
		}
		var resolveErr error
		convResult, resolveErr = messaging.ResolveDMConversationForRead(ctx, s.store, s.messageLog, parts[1], parts[2], parts[3], parts[4])
		if resolveErr != nil {
			// DEF-127a: infrastructure error — return 500, not 409.
			slog.Error("read-switch: DM conversation lookup failed",
				"key", key, "error", resolveErr)
			return target, &chatReadError{kind: chatReadFailure, status: http.StatusInternalServerError, code: ErrCodeInternalError,
				message: "Failed to look up conversation", err: resolveErr}
		}
	} else if wcs != nil && topic != nil {
		// Thread key — the topic gives the projectID for the external_ref.
		convResult = messaging.ResolveThreadConversationForRead(ctx, s.store, s.messageLog, key, topic.ProjectID,
			messaging.WithReadTopicLookup(wcs))
	}

	switch {
	case convResult != nil:
		// G3-d: preserve Channel:"web" — this scope serves the web chat
		// UI. Dropping the channel constraint would widen visibility to
		// messages from Discord, telegram, etc. that share the same
		// conversation_id. Widening is not recoverable; narrowing is.
		target.Filter = store.MessageFilter{Channel: historyChannel, ConversationID: convResult.ConversationID}
		target.ConversationID = convResult.ConversationID
		return target, nil
	case isDM:
		// DEF-127: a never-used DM is a normal first-use state, not a
		// defect. Authorization already passed (key-based), so its history
		// is empty. Emit counter + WARN for observability.
		messaging.DMAbsentMetrics.Inc()
		slog.Warn("read-switch: DM conversation absent, returning empty",
			"key", key)
		return target, &chatReadError{kind: chatReadAbsent}
	default:
		// G3 / AC-G3-2,5: no fallback — return typed error. Thread
		// conversations are created by the topic system, so absence is
		// genuine drift.
		slog.Warn("read-switch: conversation not resolved, returning error",
			"key", key, "is_dm", isDM)
		return target, &chatReadError{kind: chatReadFailure, status: http.StatusConflict, code: ErrCodeConversationNotResolved,
			message: "Conversation could not be resolved for this key; the read-switch is ON but no matching conversation record exists"}
	}
}
