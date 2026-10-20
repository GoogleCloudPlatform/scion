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

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Agent uploads of attachments stored as artifacts (ptone/scion#4596,
// design sections 6.1 and 7.1).
//
// An agent uploads on POST /api/v1/chat/attachments and names the returned
// ids when it sends. An upload is readable only by its uploader until a send
// binds it, so uploading carries the same authority as sending: the gate is
// agentMaySend, the target-independent part of "may this agent send". An
// agent has no attachment read route; it receives attachments by delivery.

// codeAttachmentArtifactsUnavailable is the error code of the 404 an agent
// upload answers while attachments stored as artifacts are off (the
// experiment is off or the artifact service is not ready). The CLI reads it
// as "use today's path".
const codeAttachmentArtifactsUnavailable = "attachment_artifacts_unavailable"

// agentMaySend reports whether the caller of ctx is an agent that may send
// at all, before any target is known (design D1): its credential classifies
// as an agent token (a delegated agent credential does not), and the agent
// is in good standing (agentStanding, ptone/scion#3433: it and every agent
// on its chain are live and not held, and its root user is active and
// admitted). A lookup fault refuses. It returns the agent's row.
func (s *Server) agentMaySend(ctx context.Context) (*store.Agent, bool) {
	if cred, _ := inboxCredentialOf(ctx); cred != inboxCredentialAgent {
		return nil, false
	}
	identity := GetAgentIdentityFromContext(ctx)
	if identity == nil || identity.ID() == "" {
		return nil, false
	}
	if _, delegated := GetIdentityFromContext(ctx).(*DelegatedAgentIdentity); delegated {
		return nil, false
	}
	if err := s.agentStanding(ctx, identity.ID()); err != nil {
		return nil, false
	}
	agent, err := s.store.GetAgent(ctx, identity.ID())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.messageLog.Warn("Agent lookup failed for an attachment upload", "agent", identity.ID(), "error", err)
		}
		return nil, false
	}
	return agent, true
}

// chatAttachmentRoute wraps the two chat attachment routes. It admits an
// agent credential for POST and GET on the collection,
// /api/v1/chat/attachments, only (the upload and the capability probe), and
// hands every other credential and method, and every request on
// /api/v1/chat/attachments/{id}, to chatRoute. So the by-id download stays
// session and token only, and chatRoute's visitor gate applies unchanged.
// The admitted agent request still meets agentMaySend in the handler.
func (s *Server) chatAttachmentRoute(next http.HandlerFunc) http.HandlerFunc {
	guarded := s.chatRoute(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == chatAttachmentsPath && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
			if cred, _ := inboxCredentialOf(r.Context()); cred == inboxCredentialAgent {
				next(w, r)
				return
			}
		}
		guarded(w, r)
	}
}

// chatAttachmentsPath is the chat attachment collection route.
const chatAttachmentsPath = "/api/v1/chat/attachments"

// handleAgentAttachmentUpload is the agent branch of the upload handler. It
// runs before every check of the user branch: an agent upload never reaches
// the AttachmentStore, the token selector or the project_id authorization,
// which belong to user callers. project_id is ignored: an upload has no
// project until a send binds it.
func (s *Server) handleAgentAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	ctx := withStandingMemo(r.Context())
	r = r.WithContext(ctx)
	if _, ok := s.agentMaySend(ctx); !ok {
		Forbidden(w)
		return
	}
	if !s.attachmentUploadsToArtifacts(ctx) {
		writeError(w, http.StatusNotFound, codeAttachmentArtifactsUnavailable,
			"attachments stored as artifacts are not available", nil)
		return
	}
	maxFile := s.attachmentMaxBytes(ctx, true)
	r.Body = http.MaxBytesReader(w, r.Body, maxFile*MaxAttachmentsPerMessage+1024*1024)
	if err := r.ParseMultipartForm(uploadFormMemory); err != nil {
		BadRequest(w, "invalid multipart form or request too large")
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		ValidationError(w, "no files uploaded", nil)
		return
	}
	if len(files) > MaxAttachmentsPerMessage {
		ValidationError(w, fmt.Sprintf("too many files: %d (max %d)", len(files), MaxAttachmentsPerMessage), nil)
		return
	}
	results := make([]attachmentUploadResult, 0, len(files))
	failures := make([]attachmentUploadFailure, 0)
	internalError := false
	for _, fh := range files {
		result, err := s.storeUploadedArtifact(ctx, maxFile, fh)
		if err != nil {
			var rejection attachmentRejection
			if !errors.As(err, &rejection) {
				internalError = true
				s.messageLog.Error("Agent attachment upload failed", "file", fh.Filename, "error", err)
			}
			failures = append(failures, attachmentUploadFailure{Name: fh.Filename, Error: uploadFailureMessage(err)})
			continue
		}
		results = append(results, result)
	}
	status := http.StatusCreated
	if len(results) == 0 {
		status = http.StatusBadRequest
		if internalError {
			status = http.StatusInternalServerError
		}
	}
	writeJSON(w, status, map[string]interface{}{
		"attachments": results,
		"failures":    failures,
	})
}

// attachmentUploadProbe is the answer of GET /api/v1/chat/attachments: where
// the calling credential's uploads go. "artifact" means the experiment is on,
// the artifact service is ready and attachmentWriterKind admits the caller;
// "legacy" means today's path (staged files, or the AttachmentStore).
type attachmentUploadProbe struct {
	Uploads string `json:"uploads"`
}

// handleAttachmentProbe answers GET /api/v1/chat/attachments. It uploads and
// reads nothing.
func (s *Server) handleAttachmentProbe(w http.ResponseWriter, r *http.Request) {
	ctx := withStandingMemo(r.Context())
	answer := attachmentUploadProbe{Uploads: "legacy"}
	if s.experimentEnabled(experiments.AttachmentArtifacts) && s.attachmentArtifactsReady() && s.attachmentWriterKind(ctx) != "" {
		answer.Uploads = "artifact"
	}
	writeJSON(w, http.StatusOK, answer)
}
