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
	"io"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
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
// is ready, and the caller is one the service lets read attachments
// (PermitsMessageRead: a session of a user or the dev user). Otherwise
// uploads go to the AttachmentStore as before, so a user access token or a
// federated user keeps uploading, and reading what it uploads, as it did
// with the experiment off.
func (s *Server) attachmentUploadsToArtifacts(ctx context.Context) bool {
	if !s.experimentEnabled(experiments.AttachmentArtifacts) || !s.attachmentArtifactsReady() {
		return false
	}
	return s.attachmentCallerIsSession(ctx)
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
// message messageID, sent by user, under the attachment's row lock, and
// returns refs without those that could not be bound (one sent meanwhile
// in another message, for example), so the message never names an
// attachment it does not hold. AttachmentStore references are returned as
// they are. A failure is logged with the attachment id only.
func (s *Server) bindChatAttachments(ctx context.Context, user UserIdentity, messageID string, refs []AttachmentRef) []AttachmentRef {
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
		if svc == nil {
			svc = s.attachmentService()
		}
		if err := svc.BindAttachment(ctx, ref.ID, artifacts.PrincipalKindUser, user.ID(), messageID); err != nil {
			s.messageLog.Error("Failed to bind attachment to message; dropped from the message",
				"attachment", ref.ID, "message", messageID, "error", err)
			continue
		}
		out = append(out, ref)
	}
	return out
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
// creating it when needed. A failure is logged and leaves byMessage as it
// is. The caller has already authorized the history read.
func (s *Server) mergeArtifactAttachments(ctx context.Context, msgIDs []string, byMessage map[string][]AttachmentRef) map[string][]AttachmentRef {
	if len(msgIDs) == 0 || !s.attachmentArtifactsReady() {
		return byMessage
	}
	// Only a caller the artifact service lets read attachments (a session)
	// sees them listed; user access tokens do not (ptone/scion#4596).
	if !s.attachmentCallerIsSession(ctx) {
		return byMessage
	}
	found, err := s.attachmentService().MessageAttachments(ctx, msgIDs)
	if err != nil {
		s.messageLog.Error("Failed to list message attachments", "error", err)
		return byMessage
	}
	for msgID, list := range found {
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
