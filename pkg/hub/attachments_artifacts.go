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
	"io"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Web-chat attachments stored as message-scoped artifacts
// (ptone/scion#4595, experiment hub.attachment_artifacts).
//
// While the experiment is on, an upload is stored through the artifact
// service as an upload of the user; sending binds it to the message; a
// download resolves the id in the service first and in the AttachmentStore
// second. Who may read a stored attachment is decided by the artifact
// service alone (its read rule asks this hub's artifactHost, through
// MessageScopeHost, whether the caller reads the message), so this file
// holds no read decision of its own.

// attachmentArtifactsReady reports whether attachments stored as artifacts
// can be served: the artifacts settings section is enabled and the
// artifact store and storage are installed. hub.artifacts is not required.
// While it is false, stored attachments answer as unknown ids.
func (s *Server) attachmentArtifactsReady() bool {
	if !s.artifactsConfig().Enabled {
		return false
	}
	b := s.artifactBackend()
	return b.Store != nil && b.Blobs != nil && b.HubID != ""
}

// attachmentUploadsToArtifacts reports, for one upload request, whether its
// files are stored as artifacts: the experiment is on, the artifact service
// is ready, and attachmentWriterKind admits the caller. Otherwise uploads
// go to the AttachmentStore as before, so a federated user keeps uploading,
// and reading what it uploads, as it did with the experiment off.
func (s *Server) attachmentUploadsToArtifacts(ctx context.Context) bool {
	if !s.experimentEnabled(experiments.AttachmentArtifacts) || !s.attachmentArtifactsReady() {
		return false
	}
	return s.attachmentWriterKind(ctx) != ""
}

// attachmentWriterKind answers the write question of attachments stored as
// artifacts (ptone/scion#4596): may the caller of ctx create such
// attachments (upload them, and name them on a send)? It returns the
// principal kind the attachment is owned by, or "" when the caller keeps
// the AttachmentStore path. It is not a read question: who reads an
// attachment is the artifact service's decision alone
// (artifacts.Service.MayReadAttachments and ServeAttachment over
// artifactHost). Admitted: a session of a user or the dev user, a user
// access token (as its holder; the routes keep their token checks,
// ptone/scion#4596 section 9), and an agent that may send (agentMaySend).
func (s *Server) attachmentWriterKind(ctx context.Context) string {
	cred, token := inboxCredentialOf(ctx)
	if cred == inboxCredentialToken {
		if token != nil && token.ID() != "" {
			return artifacts.PrincipalKindUser
		}
		return ""
	}
	if cred == inboxCredentialAgent {
		if _, ok := s.agentMaySend(ctx); ok {
			return artifacts.PrincipalKindAgent
		}
		return ""
	}
	if cred != inboxCredentialSession {
		return ""
	}
	switch id := GetIdentityFromContext(ctx).(type) {
	case *AuthenticatedUser, *DevUser:
		if id.ID() != "" {
			return artifacts.PrincipalKindUser
		}
	}
	return ""
}

// attachmentMaxBytes is the per-file limit of an upload: the artifact file
// size limit when it is stored as an artifact, MaxAttachmentSize otherwise.
func (s *Server) attachmentMaxBytes(ctx context.Context, toArtifacts bool) int64 {
	if !toArtifacts {
		return MaxAttachmentSize
	}
	if n := s.artifactLimits(ctx).MaxFileBytes; n > 0 {
		return n
	}
	return artifacts.DefaultMaxFileBytes
}

// bindChatAttachments binds each artifact attachment of refs to the stored
// message messageID, sent by (senderKind, senderID), under the
// attachment's row lock,
// recording project as the message's project (bindProject; nil when it
// could not be derived, and then no attachment is bound). It returns refs
// without those that could not be bound (one sent meanwhile in another
// message, for example), so the message never names an attachment it does
// not hold. AttachmentStore references are returned as they are. A failure
// is logged with the attachment id only.
func (s *Server) bindChatAttachments(ctx context.Context, senderKind, senderID, messageID string, project *string, refs []AttachmentRef) []AttachmentRef {
	if len(refs) == 0 {
		return refs
	}
	var svc *artifacts.Service
	out := make([]AttachmentRef, 0, len(refs))
	for _, ref := range refs {
		if !ref.artifact {
			out = append(out, ref)
			continue
		}
		if project == nil {
			s.messageLog.Error("No message project for the attachment; dropped from the message",
				"attachment", ref.ID, "message", messageID)
			continue
		}
		if svc == nil {
			svc = s.attachmentService()
		}
		if err := svc.BindAttachment(ctx, ref.ID, senderKind, senderID, messageID, *project); err != nil {
			s.messageLog.Error("Failed to bind attachment to message; dropped from the message",
				"attachment", ref.ID, "message", messageID, "error", err)
			continue
		}
		out = append(out, ref)
	}
	return out
}

// bindProject returns the project to record on the artifact attachments of
// refs when they are bound to the stored message msg
// (messageProjectForBind), or nil when it cannot be derived (logged). It
// derives nothing when refs holds no artifact attachment.
func (s *Server) bindProject(ctx context.Context, msg *store.Message, refs []AttachmentRef) *string {
	needed := false
	for _, ref := range refs {
		if ref.artifact {
			needed = true
			break
		}
	}
	if !needed {
		return nil
	}
	project, err := s.messageProjectForBind(ctx, msg)
	if err != nil {
		s.messageLog.Error("Failed to derive the message project for its attachments", "message", msg.ID, "error", err)
		return nil
	}
	return &project
}

// newArtifactAttachmentRef returns the reference to artifact attachment
// info, marked as stored in the artifact service. It is the only place
// that sets AttachmentRef.artifact (pinned by
// TestArtifactAttachmentRef_OnlyConstructorSetsFlag): every other
// reference, in particular one decoded from message metadata, from JSON or
// built for history, leaves the flag false, so only refs that send
// validation built from a usable upload reach the chat subjects and the
// bind step.
func newArtifactAttachmentRef(info artifacts.AttachmentInfo) AttachmentRef {
	return AttachmentRef{ID: info.ID, Name: info.Name, MimeType: info.MediaType, Size: info.Size, artifact: true, sha256: info.SHA256}
}

// legacyAttachmentRefs returns the AttachmentStore references of refs.
func legacyAttachmentRefs(refs []AttachmentRef) []AttachmentRef {
	var out []AttachmentRef
	for _, ref := range refs {
		if !ref.artifact {
			out = append(out, ref)
		}
	}
	return out
}

// stageArtifactAttachment copies artifact attachment ref, bound to message
// messageID, into the agent staging directory and returns the agent-visible
// path. It is part of the send path only (stageChatAttachments), after the
// send was authorized and the message stored: the bytes are opened through
// OpenBoundAttachment, which checks no caller, so it must never be reached
// from a request that names an attachment.
func (s *Server) stageArtifactAttachment(ctx context.Context, staging *attachmentStaging, messageID string, ref AttachmentRef) (string, error) {
	if staging == nil {
		return "", errNoAttachmentStaging
	}
	svc := s.attachmentService()
	return staging.stageFrom(func() (io.ReadCloser, error) {
		rc, _, err := svc.OpenBoundAttachment(ctx, ref.ID, messageID)
		return rc, err
	}, ref.ID, ref.Name)
}

// errNoAttachmentStaging: the project has no staging directory, and an
// artifact attachment has no hub-local path to send instead.
var errNoAttachmentStaging = errors.New("no attachment staging directory for this project")

// mergeArtifactAttachments adds the artifact attachments of the messages
// in msgIDs to byMessage (the AttachmentStore attachments of history),
// creating it when needed. One batched query finds the attachments of the
// page; then the read decision (MayReadAttachments) is asked once
// per message that has any, so a page costs one query plus one decision
// per message with attachments. A failure is logged and leaves byMessage
// as it is. The caller has already authorized the history read; the
// decision is asked anyway, so every credential class is decided in one
// place.
func (s *Server) mergeArtifactAttachments(ctx context.Context, msgIDs []string, byMessage map[string][]AttachmentRef) map[string][]AttachmentRef {
	if len(msgIDs) == 0 || !s.attachmentArtifactsReady() {
		return byMessage
	}
	svc := s.attachmentService()
	found, err := svc.MessageAttachments(ctx, msgIDs)
	if err != nil {
		s.messageLog.Error("Failed to list message attachments", "error", err)
		return byMessage
	}
	ctx = withStandingMemo(ctx)
	for _, msgID := range msgIDs {
		list := found[msgID]
		if len(list) == 0 || !svc.MayReadAttachments(ctx, msgID, list[0].MessageProjectID) {
			continue
		}
		if byMessage == nil {
			byMessage = make(map[string][]AttachmentRef, len(found))
		}
		for _, a := range list {
			byMessage[msgID] = append(byMessage[msgID], AttachmentRef{
				ID: a.ID, Name: a.Name, MimeType: a.MediaType, Size: a.Size,
			})
		}
	}
	return byMessage
}

// filterReadableAttachments returns refs without the artifact attachments
// of message messageID when the caller of ctx does not read them (the
// send echo: one decision per send, since every ref belongs to the one
// message). AttachmentStore references are returned as they are.
func (s *Server) filterReadableAttachments(ctx context.Context, messageID string, refs []AttachmentRef) []AttachmentRef {
	hasArtifact := false
	for _, ref := range refs {
		if ref.artifact {
			hasArtifact = true
			break
		}
	}
	if !hasArtifact {
		return refs
	}
	if s.attachmentArtifactsReady() {
		svc := s.attachmentService()
		found, err := svc.MessageAttachments(ctx, []string{messageID})
		if err == nil && len(found[messageID]) > 0 &&
			svc.MayReadAttachments(withStandingMemo(ctx), messageID, found[messageID][0].MessageProjectID) {
			return refs
		}
		if err != nil {
			s.messageLog.Error("Failed to list message attachments for the send echo", "message", messageID, "error", err)
		}
	}
	return legacyAttachmentRefs(refs)
}

// retireMessageAttachments retires the artifact attachments of message
// messageID, which was just deleted, so they are unreadable at once. A
// failure is logged; the maintenance pass retires them later because the
// message is gone (artifacts.AttachmentRetirer).
func (s *Server) retireMessageAttachments(ctx context.Context, messageID string) {
	if s.ArtifactStore() == nil {
		return
	}
	if _, err := s.attachmentService().RetireScope(ctx, artifacts.ScopeKindMessage, messageID); err != nil &&
		!errors.Is(err, artifacts.ErrUnavailable) {
		s.messageLog.Error("Failed to retire the attachments of a deleted message", "message", messageID, "error", err)
	}
}

// messageProjectForBind returns the project recorded on the attachments
// bound to msg (ptone/scion#4596, design section 1): derived from the
// stored message row and its conversation, never from a request field.
//
//   - group conversation: its project, or "" when it has none;
//   - direct conversation: the project of its one agent party (""
//     when that agent row is gone), or "" when it has no agent party or
//     two;
//   - no conversation: the topic's project for a topic thread key, ""
//     for a DM key.
//
// Unlike conversationProject it makes no caller-relative peer choice, so
// the value does not depend on who sends. A lookup failure is returned
// and the caller binds nothing.
func (s *Server) messageProjectForBind(ctx context.Context, msg *store.Message) (string, error) {
	if msg == nil {
		return "", errors.New("no message")
	}
	if msg.ConversationID != "" {
		conv, err := s.store.GetConversation(ctx, msg.ConversationID)
		if err != nil {
			return "", fmt.Errorf("conversation lookup: %w", err)
		}
		if conv.Kind != "direct" {
			if conv.ProjectID == nil {
				return "", nil
			}
			return *conv.ProjectID, nil
		}
		parties, err := dmAgentParties(conv)
		if err != nil {
			return "", err
		}
		if len(parties) != 1 {
			return "", nil
		}
		agent, err := s.store.GetAgent(ctx, parties[0])
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("agent lookup: %w", err)
		}
		return agent.ProjectID, nil
	}
	if msg.ThreadID == "" || strings.HasPrefix(msg.ThreadID, "dm:") {
		return "", nil
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return "", nil
	}
	topic, err := wcs.GetTopic(ctx, msg.ThreadID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && topic == nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("topic lookup: %w", err)
	}
	return topic.ProjectID, nil
}

// dmAgentParties returns the agent ids named in the key of direct
// conversation conv, in key order: none, one or two. It parses the key
// once and depends on no caller.
func dmAgentParties(conv *store.Conversation) ([]string, error) {
	kindA, idA, kindB, idB, err := messages.ParseDMKey(conv.ExternalRef)
	if err != nil {
		return nil, err
	}
	var out []string
	if kindA == "agent" {
		out = append(out, idA)
	}
	if kindB == "agent" {
		out = append(out, idB)
	}
	return out, nil
}
