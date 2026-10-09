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
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// This file holds the checks for references carried inside chat content: a
// reply target, a read watermark, an attachment, a thread or a conversation
// ID. A reference must belong to the conversation (or the conversation's
// project) it is used in; anything else is answered exactly as if it did not
// exist, and the reason is written to the server log only.
//
// Every helper here that reads a store or asks the authorization service
// answers "no" when that call fails. None of them defaults to allow.

// logReferenceRefused records why a reference was refused. The response the
// caller receives is the route's answer for a missing reference; the reason
// is only ever written here.
func logReferenceRefused(ctx context.Context, route, reason string, caller Identity) {
	var callerType, callerID string
	if caller != nil {
		callerType, callerID = caller.Type(), caller.ID()
	}
	slog.InfoContext(ctx, "reference refused",
		"route", route,
		"reason", reason,
		"caller_type", callerType,
		"caller", callerID,
	)
}

// reasonExternalRefOfOtherProject is the logged reason when an external
// chat reference already names a conversation of another project.
const reasonExternalRefOfOtherProject = "external reference names a conversation of another project"

// externalRefOfOtherProject reports whether a conversation resolution error
// is the store keeping an existing conversation in the project it was created
// with. Such a reference is never reused: the request is refused whatever the
// write-deny setting, because continuing would deliver the message without
// the conversation the reference names.
func externalRefOfOtherProject(err error) bool {
	return errors.Is(err, store.ErrConversationProjectMismatch)
}

// sameConversation reports whether msg belongs to the current conversation.
// A match on either field is enough:
//
//   - convID != "" && msg.ConversationID == convID (history lists by
//     conversation when the conversation envelope switch is on, and a
//     visible row's ThreadID may then differ from the key)
//   - msg.ThreadID == threadKey (rows listed by thread key)
//
// A message of another conversation matches neither. Pure; no store access.
func sameConversation(msg *store.Message, threadKey, convID string) bool {
	if msg == nil {
		return false
	}
	if threadKey != "" && msg.ThreadID == threadKey {
		return true
	}
	return convID != "" && msg.ConversationID == convID
}

// attachmentUsableIn reports whether an attachment may be sent in a
// conversation. The rule follows the conversation kind, not a resolved
// project (an agent DM resolves to the agent's project, but files uploaded
// in a direct message carry no project):
//
//   - direct message: a file the sender uploaded in a direct message
//     (no project, uploaded by userID)
//   - topic: a file of the topic's project
//
// Pure; no store access.
func attachmentUsableIn(meta *AttachmentMeta, isDM bool, topicProjectID, userID string) bool {
	if meta == nil {
		return false
	}
	if isDM {
		return meta.ProjectID == "" && userID != "" && meta.UploadedBy == userID
	}
	return topicProjectID != "" && meta.ProjectID == topicProjectID
}

// conversationIDForKey returns the conversation ID of a chat key using the
// read-only resolvers history uses, or "" when none exists. A topic key is
// resolved within the topic's own project. A store error from the DM lookup
// is returned; the topic resolver reports a failed lookup as "no
// conversation", which callers treat as no match.
func (s *Server) conversationIDForKey(ctx context.Context, wcs WebChatStore, key string) (string, error) {
	if strings.HasPrefix(key, "dm:") {
		parts := strings.Split(key, ":")
		if len(parts) != 5 {
			return "", nil
		}
		res, err := messaging.ResolveDMConversationForRead(ctx, s.store, s.messageLog, parts[1], parts[2], parts[3], parts[4])
		if err != nil {
			return "", err
		}
		if res == nil {
			return "", nil
		}
		return res.ConversationID, nil
	}
	if wcs == nil {
		return "", nil
	}
	topic, err := wcs.GetTopic(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	if topic == nil {
		return "", nil
	}
	res := messaging.ResolveThreadConversationForRead(ctx, s.store, s.messageLog, key, topic.ProjectID,
		messaging.WithReadTopicLookup(wcs))
	if res == nil {
		return "", nil
	}
	return res.ConversationID, nil
}

// messageInChatConversation reports whether messageID names a stored message
// of the conversation key. A missing message is (false, nil); a store error
// is (false, err). The conversation ID is resolved only when the thread key
// does not already match and the message carries a conversation ID.
func (s *Server) messageInChatConversation(ctx context.Context, wcs WebChatStore, key, messageID string) (bool, error) {
	msg, err := s.store.GetMessage(ctx, messageID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if msg == nil {
		return false, nil
	}
	if sameConversation(msg, key, "") {
		return true, nil
	}
	if msg.ConversationID == "" {
		return false, nil
	}
	convID, err := s.conversationIDForKey(ctx, wcs, key)
	if err != nil {
		return false, err
	}
	return sameConversation(msg, key, convID), nil
}

// attachmentLinkScanLimit bounds how many linked messages the download check
// looks at for a file uploaded in a direct message.
const attachmentLinkScanLimit = 20

// canReadAttachment decides whether user may download an attachment. The
// first rule that matches wins:
//
//  1. user uploaded it
//  2. it is a project file and user can read the project
//  3. user can read at least one message it is attached to (first
//     attachmentLinkScanLimit links), whatever the file's project: a reader
//     of the message already sees what the message carries
//
// Rule 3 depends on how files get attached to messages: only through send
// validation (attachmentUsableIn) or hub ingest from the sending agent's own
// project (see LinkAttachmentToMessage). Any store or authorization error
// answers false.
func (s *Server) canReadAttachment(ctx context.Context, user UserIdentity, meta *AttachmentMeta) bool {
	if user == nil || meta == nil {
		return false
	}
	if meta.UploadedBy != "" && meta.UploadedBy == user.ID() {
		return true
	}
	if meta.ProjectID != "" && s.canReadProject(ctx, user, meta.ProjectID) {
		return true
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return false
	}
	ids, err := wcs.ListMessageIDsForAttachment(ctx, meta.ID, attachmentLinkScanLimit)
	if err != nil {
		slog.WarnContext(ctx, "canReadAttachment: listing linked messages failed", "attachment", meta.ID, "error", err)
		return false
	}
	for _, id := range ids {
		msg, err := s.store.GetMessage(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			slog.WarnContext(ctx, "canReadAttachment: message lookup failed", "attachment", meta.ID, "message", id, "error", err)
			return false
		}
		if s.canReadNativeMessage(ctx, user, msg) {
			return true
		}
	}
	return false
}

// canReadNativeMessage reports whether user may read a native chat message:
// through its conversation when it has one (canUserReadMessage), otherwise
// by its thread key — a DM key by its user slot, a topic by read access to
// the topic's project. Any store or authorization error answers false.
func (s *Server) canReadNativeMessage(ctx context.Context, user UserIdentity, msg *store.Message) bool {
	if user == nil || msg == nil {
		return false
	}
	if msg.ConversationID != "" {
		return s.canUserReadMessage(ctx, user, msg)
	}
	if msg.ThreadID == "" {
		return false
	}
	if strings.HasPrefix(msg.ThreadID, "dm:") {
		return isDMParticipant(msg.ThreadID, user.ID())
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return false
	}
	topic, err := wcs.GetTopic(ctx, msg.ThreadID)
	if err != nil || topic == nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "canReadNativeMessage: topic lookup failed", "message", msg.ID, "error", err)
		}
		return false
	}
	return s.canReadProject(ctx, user, topic.ProjectID)
}

// stripClientAttachmentRefs returns md without the hub's attachments
// metadata key. An agent names its files in the request's attachments
// field; the hub sets the metadata key itself, only for files it ingested
// for the message, so a caller-supplied value is never kept. md itself is
// never changed: a copy is returned when the key is present.
//
// This is separate from messaging.StripReservedMetadata, which removes a
// different set of keys; chat sends set the attachments key before that
// strip runs, so the two are not merged.
func stripClientAttachmentRefs(md map[string]string) map[string]string {
	if _, ok := md[attachmentsMetadataKey]; !ok {
		return md
	}
	out := make(map[string]string, len(md)-1)
	for k, v := range md {
		if k != attachmentsMetadataKey {
			out[k] = v
		}
	}
	return out
}

// writeConversationIDNotFound answers a caller-supplied conversation_id the
// caller may not use exactly as an unknown one (400 "caller-supplied
// conversation_id does not exist") and logs the reason.
func writeConversationIDNotFound(ctx context.Context, w http.ResponseWriter, route, reason, callerKind, callerID string) {
	slog.InfoContext(ctx, "reference refused",
		"route", route,
		"reason", reason,
		"caller_type", callerKind,
		"caller", callerID,
	)
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
		"caller-supplied conversation_id does not exist", nil)
}

// senderCanReadGroup reports whether the authenticated sender may read a
// group conversation of projectID: a user with read access to the project,
// an agent only within its own project (the strict rule for agents). The
// sender is resolved in the same order as authenticatedSender, so the check
// and the participant row describe the same principal. Any other caller,
// and any lookup error, answers false.
func (s *Server) senderCanReadGroup(ctx context.Context, projectID string) bool {
	if user := GetUserIdentityFromContext(ctx); user != nil {
		return s.canReadProject(ctx, user, projectID)
	}
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		return agentIdent.ProjectID() != "" && agentIdent.ProjectID() == projectID
	}
	return false
}

// groupReadMemo answers "may identity read this group conversation now" for
// one request. The answer for a group with a project is the project read
// rule (canReadGroupConversation), looked up once per project; a group with
// no project uses the participant rule and is checked on its own. A lookup
// error answers no and is never cached.
type groupReadMemo struct {
	s         *Server
	identity  Identity
	byProject map[string]bool
}

func newGroupReadMemo(s *Server, identity Identity) *groupReadMemo {
	return &groupReadMemo{s: s, identity: identity, byProject: map[string]bool{}}
}

func (m *groupReadMemo) canRead(ctx context.Context, conv *store.Conversation) bool {
	projectID := ""
	if conv.ProjectID != nil {
		projectID = *conv.ProjectID
	}
	if projectID != "" {
		if allowed, ok := m.byProject[projectID]; ok {
			return allowed
		}
	}
	allowed, err := m.s.canReadGroupConversation(ctx, m.identity, conv)
	if err != nil {
		slog.DebugContext(ctx, "group conversation read check failed; row omitted",
			"conversation_id", conv.ID, "project_id", projectID, "error", err)
		return false
	}
	if projectID != "" {
		m.byProject[projectID] = allowed
	}
	return allowed
}

// authorizeGroupConversationReadAsNotFound reports whether the caller may
// read conv (canReadGroupConversation). A caller who may not gets the same
// answer as for an unknown conversation; the reason is logged. A store error
// is written as that error and refuses.
func (s *Server) authorizeGroupConversationReadAsNotFound(w http.ResponseWriter, r *http.Request, conv *store.Conversation) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	allowed, err := s.canReadGroupConversation(ctx, identity, conv)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	if !allowed {
		logReferenceRefused(ctx, logging.RequestPath(r), "caller cannot read the group conversation", identity)
		NotFound(w, "Conversation")
		return false
	}
	return true
}
