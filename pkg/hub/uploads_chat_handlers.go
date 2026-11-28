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
	"crypto/sha256"
	"encoding"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleChatAttachmentUploads dispatches resumable chunked upload requests
// under /api/v1/chat/attachments/uploads (ptone/scion#8654 §3.3, §7 Phase 2).
//
// Routes:
//   - POST   /api/v1/chat/attachments/uploads
//   - PUT    /api/v1/chat/attachments/uploads/{sid}?offset=N
//   - GET    /api/v1/chat/attachments/uploads/{sid}
//   - DELETE /api/v1/chat/attachments/uploads/{sid}
//   - POST   /api/v1/chat/attachments/uploads/{sid}/complete
func (s *Server) handleChatAttachmentUploads(w http.ResponseWriter, r *http.Request, subPath string) {
	ctx := r.Context()

	// Experiment gate (§3.5, §3.12): 404 experiment_disabled when off.
	if !s.experimentEnabled(experiments.LargeUploads) {
		writeError(w, http.StatusNotFound, ErrCodeExperimentDisabled,
			"experiment_disabled: "+experiments.LargeUploads+" is off; resumable large uploads are not enabled on this hub", nil)
		return
	}

	// User principals only in v1 (§6 OQ-5).
	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil || GetAgentIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Resumable uploads require a user principal", nil)
		return
	}

	// Token selector gate matching POST /api/v1/chat/attachments.
	token, ok := s.requireChatSelector(w, r, permInboxWrite)
	if !ok {
		return
	}

	var sid string
	var action string
	if subPath != "" {
		parts := strings.Split(subPath, "/")
		switch len(parts) {
		case 1:
			sid = parts[0]
		case 2:
			if parts[1] != "complete" {
				NotFound(w, "Upload resource")
				return
			}
			sid = parts[0]
			action = "complete"
		default:
			NotFound(w, "Upload resource")
			return
		}
		if sid == "" {
			NotFound(w, "Upload session")
			return
		}
	}

	if sid == "" {
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleChatAttachmentUploadStart(w, r, userIdent, token)
		return
	}

	if action == "complete" && r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	if action == "" && r.Method != http.MethodPut && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		return
	}

	// Bind (owner, target_kind) before exposing any session-scoped error (§3.8).
	sess, err := s.store.GetUploadSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Upload session")
			return
		}
		slog.ErrorContext(ctx, "failed to load chat upload session", "session_id", sid, "error", err)
		InternalError(w)
		return
	}
	if sess.OwnerKind != "user" || sess.OwnerID != userIdent.ID() || sess.TargetKind != store.UploadTargetChatAttachment {
		NotFound(w, "Upload session")
		return
	}

	// Re-run route authorization on every call (§3.5, §3.8).
	if !s.authorizeChatAttachmentUploadScope(w, r, userIdent, token, sess.ScopeProjectID) {
		return
	}

	// Writes (chunk and complete) require artifact attachments and a Composer-capable blob store.
	if r.Method == http.MethodPut || action == "complete" {
		if !s.requireChatAttachmentUploadBackend(w, ctx) {
			return
		}
	}

	scope := uploadTargetScope{
		kind:      store.UploadTargetChatAttachment,
		projectID: sess.ScopeProjectID,
	}
	sink := newObjectUploadSink(s.artifactBackend().Blobs, s.attachmentService())

	switch {
	case action == "complete":
		s.handleUploadComplete(w, r, userIdent, scope, sink, sid)
	case r.Method == http.MethodPut:
		s.handleUploadChunk(w, r, userIdent, scope, sink, sid)
	case r.Method == http.MethodGet:
		s.handleUploadStatus(w, r, userIdent, scope, sid)
	case r.Method == http.MethodDelete:
		s.handleUploadAbort(w, r, userIdent, scope, sink, sid)
	}
}

// authorizeChatAttachmentUploadScope runs the exact same authorization checks
// as POST /api/v1/chat/attachments (handleAttachmentUpload) for either a
// project-scoped or project-less chat attachment upload.
func (s *Server) authorizeChatAttachmentUploadScope(
	w http.ResponseWriter,
	r *http.Request,
	user UserIdentity,
	token *ScopedUserIdentity,
	projectID string,
) bool {
	ctx := r.Context()
	if projectID != "" {
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			NotFound(w, "Project")
			return false
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return false
		}
		if !s.authorizeChatTokenProject(w, r, token, permInboxWrite, project.ID, chatRefuseForbidden) {
			return false
		}
		access, err := s.chatThreadAccess(ctx, user, chatThreadRef{Project: project})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return false
		}
		if !access.Can(ChatActionPost) {
			logAuthzDenial(r, user, projectResource(project), ActionChatPost, access.postDenial.Reason)
			chatSendPostDenied(access.Reason).write(w)
			return false
		}
		return true
	}
	if !requireProfileWriter(w, r) {
		return false
	}
	return s.authorizeChatTokenProject(w, r, token, permInboxWrite, "", chatRefuseForbidden)
}

// requireChatAttachmentUploadBackend verifies that hub.attachment_artifacts is
// enabled and ready, the caller is admitted as an artifact attachment writer,
// and the configured blob store implements storage.Composer (§2, §3.6).
func (s *Server) requireChatAttachmentUploadBackend(w http.ResponseWriter, ctx context.Context) bool {
	if !s.experimentEnabled(experiments.AttachmentArtifacts) || !s.attachmentArtifactsReady() {
		writeError(w, http.StatusConflict, errCodeArtifactAttachmentsRequired,
			"Large chat attachments require hub.attachment_artifacts and configured artifact storage", nil)
		return false
	}
	blobs := s.artifactBackend().Blobs
	if _, ok := blobs.(storage.Composer); !ok {
		writeError(w, http.StatusNotImplemented, errCodeLargeUploadsUnsupportedStorage,
			"Configured blob storage does not support compose required for large chat attachments", nil)
		return false
	}
	if s.attachmentWriterKind(ctx) == "" {
		Forbidden(w)
		return false
	}
	return true
}

func (s *Server) handleChatAttachmentUploadStart(
	w http.ResponseWriter,
	r *http.Request,
	userIdent UserIdentity,
	token *ScopedUserIdentity,
) {
	ctx := r.Context()

	var req UploadStartRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	projectID := strings.TrimSpace(req.ProjectID)
	if !s.authorizeChatAttachmentUploadScope(w, r, userIdent, token, projectID) {
		return
	}
	if !s.requireChatAttachmentUploadBackend(w, ctx) {
		return
	}

	fileName := strings.TrimSpace(req.Name)
	if fileName == "" {
		BadRequest(w, "File name is required")
		return
	}
	cleanName, err := SanitizeFilename(fileName)
	if err != nil {
		BadRequest(w, "Invalid file name: "+err.Error())
		return
	}

	mediaType := strings.TrimSpace(req.MediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}

	if req.Size < 0 {
		BadRequest(w, "File size must be non-negative")
		return
	}
	maxBytes := s.attachmentMaxBytes(ctx, true)
	if req.Size > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errCodeUploadDeclaredExceeded,
			fmt.Sprintf("File size %d exceeds maximum allowed size of %d bytes", req.Size, maxBytes),
			map[string]interface{}{"maxBytes": maxBytes})
		return
	}

	randHex, err := newUploadRandomHex(16)
	if err != nil {
		InternalError(w)
		return
	}
	sid := "upl_" + randHex

	h := sha256.New()
	hashState, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		InternalError(w)
		return
	}

	cfg := s.uploadsConfig()
	now := uploadNowFn()
	idleExp := now.Add(cfg.SessionIdleTTL)
	hardExp := now.Add(cfg.SessionMaxLifetime)
	hubID := s.uploadHubID()
	if bHubID := s.artifactBackend().HubID; bHubID != "" {
		hubID = bHubID
	}

	sess := &store.UploadSession{
		ID:             sid,
		HubID:          hubID,
		OwnerKind:      "user",
		OwnerID:        userIdent.ID(),
		TargetKind:     store.UploadTargetChatAttachment,
		ScopeProjectID: projectID,
		FileName:       cleanName,
		MediaType:      mediaType,
		DeclaredSize:   req.Size,
		HashState:      hashState,
		StagingRef:     uploadObjectStagingPrefix(hubID, sid),
		ArtifactID:     uuid.NewString(),
		IdleExpiresAt:  idleExp,
		HardExpiresAt:  hardExp,
	}

	quota := store.UploadQuota{
		MaxSessionsPerUser:     cfg.MaxSessionsPerUser,
		MaxPendingBytesPerUser: cfg.MaxPendingBytesPerUser,
		MaxPendingBytesHub:     cfg.MaxPendingBytesHub,
	}
	if err := s.store.CreateUploadSession(ctx, sess, quota); err != nil {
		if errors.Is(err, store.ErrQuotaExceeded) {
			w.Header().Set("Retry-After", strconv.Itoa(int(uploadQuotaRetryAfter.Seconds())))
			writeError(w, http.StatusTooManyRequests, errCodeUploadQuotaExceeded,
				"Upload quota exceeded; retry later", nil)
			return
		}
		slog.ErrorContext(ctx, "failed to create chat upload session", "error", err)
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusCreated, UploadStartResponse{
		ID:         sess.ID,
		Transport:  uploadTransportHubChunked,
		ChunkBytes: cfg.ChunkBytes,
		MaxBytes:   maxBytes,
		Received:   0,
		ExpiresAt:  effectiveUploadExpiry(sess.IdleExpiresAt, sess.HardExpiresAt),
	})
}
