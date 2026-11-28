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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Protocol constants for resumable chunked uploads (ptone/scion#8654 §3.5,
// §3.10, §3.11). Defined as package variables so tests can shorten them.
var (
	uploadChunkDeadline   = 5 * time.Minute
	uploadLeaseMargin     = 30 * time.Second
	uploadLeaseTTL        = 5*time.Minute + 30*time.Second
	uploadFinalizeBudget  = 5 * time.Minute
	uploadQuotaRetryAfter = 60 * time.Second
	uploadNowFn           = func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }
)

const (
	uploadTransportHubChunked = "hub_chunked"
	uploadHeadCaptureBytes    = 512

	// Upload-specific API error codes (§3.3, §3.5, §3.6, §3.8).
	errCodeUploadQuotaExceeded     = "upload_quota_exceeded"
	errCodeUploadOffsetMismatch    = "offset_mismatch"
	errCodeUploadBusy              = "upload_busy"
	errCodeUploadChunkTooLarge     = "chunk_too_large"
	errCodeUploadDeclaredExceeded  = "declared_size_exceeded"
	errCodeUploadIncomplete               = "upload_incomplete"
	errCodeUploadInvalidState             = "upload_invalid_state"
	errCodeUploadIntegrityMismatch        = "integrity_mismatch"
	errCodeUploadDisallowedMediaType      = "disallowed_media_type"
	errCodeArtifactAttachmentsRequired    = "artifact_attachments_required"
	errCodeLargeUploadsUnsupportedStorage = "large_uploads_unsupported_storage"
)

// UploadStartRequest is the JSON body for POST …/uploads (§3.3).
type UploadStartRequest struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType"`
	Path      string `json:"path,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
	// ProjectID optionally scopes a chat_attachment upload to a project space
	// (matching POST /api/v1/chat/attachments's project_id form field).
	ProjectID string `json:"projectId,omitempty"`
}

// UploadStartResponse is the 201 JSON response for POST …/uploads (§3.3).
type UploadStartResponse struct {
	ID         string    `json:"id"`
	Transport  string    `json:"transport"`
	ChunkBytes int64     `json:"chunkBytes"`
	MaxBytes   int64     `json:"maxBytes"`
	Received   int64     `json:"received"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// UploadChunkResponse is the 200 JSON response for PUT …/uploads/{sid} (§3.3).
type UploadChunkResponse struct {
	Received  int64     `json:"received"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// UploadStatusResponse is the 200 JSON response for GET …/uploads/{sid} (§3.3).
type UploadStatusResponse struct {
	ID           string                   `json:"id"`
	State        store.UploadSessionState `json:"state"`
	Received     int64                    `json:"received"`
	DeclaredSize int64                    `json:"declaredSize"`
	ExpiresAt    time.Time                `json:"expiresAt"`
}

// uploadTargetScope binds a session to the route it was created on (§3.8).
type uploadTargetScope struct {
	kind      store.UploadTargetKind
	projectID string
	sharedDir string
}

func (sc uploadTargetScope) matches(sess *store.UploadSession) bool {
	return sess.TargetKind == sc.kind &&
		sess.ScopeProjectID == sc.projectID &&
		sess.ScopeSharedDir == sc.sharedDir
}

func (s *Server) uploadsConfig() opsettings.UploadsConfig {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.Uploads()
	}
	return opsettings.DefaultUploadsConfig()
}

func (s *Server) uploadHubID() string {
	if id := s.HubID(); id != "" {
		return id
	}
	return "local"
}

func (s *Server) uploadMaxFileBytes(ctx context.Context, kind store.UploadTargetKind) int64 {
	switch kind {
	case store.UploadTargetChatAttachment:
		return s.attachmentMaxBytes(ctx, true)
	default:
		return s.WorkspaceUploadMaxFileBytes(ctx)
	}
}

func effectiveUploadExpiry(idle, hard time.Time) time.Time {
	if hard.Before(idle) {
		return hard
	}
	return idle
}

func effectiveUploadStateString(sess *store.UploadSession, now time.Time) string {
	if sess.State == store.UploadStateUploading && (!sess.IdleExpiresAt.After(now) || !sess.HardExpiresAt.After(now)) {
		return string(store.UploadStateExpired)
	}
	return string(sess.State)
}

func newUploadRandomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleProjectWorkspaceUploads dispatches resumable chunked upload requests
// under /api/v1/projects/{pid}/workspace/uploads (ptone/scion#8654 §3.3).
//
// Route authorization (handleProjectWorkspaceRoutes) has already run before
// this function is called.
func (s *Server) handleProjectWorkspaceUploads(w http.ResponseWriter, r *http.Request, project *store.Project, subPath string) {
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

	// Parse subpath:
	//   ""                 → POST start
	//   "{sid}"            → PUT chunk, GET status, DELETE abort
	//   "{sid}/complete"   → POST complete
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

	// Resolve workspace and run write gate on every call (§2, §3.5).
	resolved, err := s.resolveProjectWorkspace(ctx, project)
	if err != nil {
		if !writeWorkspaceStorageUnavailable(w, err) {
			Conflict(w, err.Error())
		}
		return
	}
	gate := s.workspaceWriteGate(resolved)
	isWrite := r.Method != http.MethodGet
	if isWrite && gate.refuse(w) {
		return
	}

	createIfMissing := sid == "" && r.Method == http.MethodPost
	root, err := resolved.Open(createIfMissing)
	if err != nil {
		if sharedBaseMissing(resolved) {
			writeWorkspaceStorageUnavailable(w, errWorkspaceStorageUnavailable)
			return
		}
		if errors.Is(err, fs.ErrNotExist) {
			NotFound(w, "Upload session")
			return
		}
		slog.ErrorContext(ctx, "failed to open project workspace for upload",
			"project_id", project.ID, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = root.Close() }()

	scope := uploadTargetScope{
		kind:      store.UploadTargetWorkspace,
		projectID: project.ID,
	}
	sink := newFileUploadSink(root)

	switch {
	case sid == "" && r.Method == http.MethodPost:
		s.handleWorkspaceUploadStart(w, r, userIdent, scope, sink)
	case sid == "":
		MethodNotAllowed(w, http.MethodPost)
	case action == "complete" && r.Method == http.MethodPost:
		s.handleUploadComplete(w, r, userIdent, scope, sink, sid)
	case action == "complete":
		MethodNotAllowed(w, http.MethodPost)
	case r.Method == http.MethodPut:
		s.handleUploadChunk(w, r, userIdent, scope, sink, sid)
	case r.Method == http.MethodGet:
		s.handleUploadStatus(w, r, userIdent, scope, sid)
	case r.Method == http.MethodDelete:
		s.handleUploadAbort(w, r, userIdent, scope, sink, sid)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

// handleProjectSharedDirUploads dispatches resumable chunked upload requests
// under /api/v1/projects/{pid}/shared-dirs/{name}/uploads (ptone/scion#8654 §3.3).
//
// Route authorization (handleProjectSharedDirRoutes) has already run before
// this function is called.
func (s *Server) handleProjectSharedDirUploads(w http.ResponseWriter, r *http.Request, project *store.Project, dirName, subPath string) {
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

	// Parse subpath:
	//   ""                 → POST start
	//   "{sid}"            → PUT chunk, GET status, DELETE abort
	//   "{sid}/complete"   → POST complete
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

	// Verify the shared dir is declared on this project.
	found := false
	for _, d := range project.SharedDirs {
		if d.Name == dirName {
			found = true
			break
		}
	}
	if !found {
		NotFound(w, "Shared directory")
		return
	}

	// Resolve shared dir and run write gate on every call (§3.5, §7 P3).
	resolution, resolveErr := s.resolveSharedDirPath(ctx, project, dirName)
	if resolveErr != nil {
		var nfsErr *sharedDirStorageResolveError
		if errors.As(resolveErr, &nfsErr) {
			slog.ErrorContext(ctx, "failed to resolve shared directory path for upload",
				"project_id", project.ID, "dir", dirName, "error", resolveErr)
			Conflict(w, sharedDirStorageUnavailableMessage)
			return
		}
		Conflict(w, resolveErr.Error())
		return
	}
	gate := s.sharedDirWriteGate(resolution)
	isWrite := r.Method != http.MethodGet
	if isWrite && gate.refuse(w) {
		return
	}

	createIfMissing := sid == "" && r.Method == http.MethodPost
	root, err := s.openSharedDirRoot(resolution, createIfMissing)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			NotFound(w, "Upload session")
			return
		}
		slog.ErrorContext(ctx, "failed to open shared directory for upload",
			"project_id", project.ID, "dir", dirName, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = root.Close() }()

	scope := uploadTargetScope{
		kind:      store.UploadTargetSharedDir,
		projectID: project.ID,
		sharedDir: dirName,
	}
	sink := newFileUploadSink(root)

	switch {
	case sid == "" && r.Method == http.MethodPost:
		s.handleWorkspaceUploadStart(w, r, userIdent, scope, sink)
	case sid == "":
		MethodNotAllowed(w, http.MethodPost)
	case action == "complete" && r.Method == http.MethodPost:
		s.handleUploadComplete(w, r, userIdent, scope, sink, sid)
	case action == "complete":
		MethodNotAllowed(w, http.MethodPost)
	case r.Method == http.MethodPut:
		s.handleUploadChunk(w, r, userIdent, scope, sink, sid)
	case r.Method == http.MethodGet:
		s.handleUploadStatus(w, r, userIdent, scope, sid)
	case r.Method == http.MethodDelete:
		s.handleUploadAbort(w, r, userIdent, scope, sink, sid)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) handleWorkspaceUploadStart(
	w http.ResponseWriter,
	r *http.Request,
	userIdent UserIdentity,
	scope uploadTargetScope,
	sink *fileUploadSink,
) {
	ctx := r.Context()

	var req UploadStartRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	destPath := strings.TrimSpace(req.Path)
	if destPath == "" {
		destPath = strings.TrimSpace(req.Name)
	}
	if destPath == "" {
		BadRequest(w, "File path or name is required")
		return
	}
	if err := validateWorkspaceWritePath(destPath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", destPath, err.Error()))
		return
	}
	if isUploadPartialName(path.Base(destPath)) {
		BadRequest(w, "Invalid file path: reserved upload partial filename")
		return
	}

	fileName := strings.TrimSpace(req.Name)
	if fileName == "" {
		fileName = path.Base(destPath)
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
	maxBytes := s.WorkspaceUploadMaxFileBytes(ctx)
	if req.Size > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errCodeUploadDeclaredExceeded,
			fmt.Sprintf("File size %d exceeds maximum allowed size of %d bytes", req.Size, maxBytes),
			map[string]interface{}{"maxBytes": maxBytes})
		return
	}

	// Prepare parent directories and check that destPath is not a symlink or
	// directory before inserting the session row.
	if err := sink.Prepare(destPath); err != nil {
		writeWorkspaceFileError(w, "upload", destPath, err)
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

	sess := &store.UploadSession{
		ID:             sid,
		HubID:          s.uploadHubID(),
		OwnerKind:      "user",
		OwnerID:        userIdent.ID(),
		TargetKind:     scope.kind,
		ScopeProjectID: scope.projectID,
		ScopeSharedDir: scope.sharedDir,
		DestPath:       destPath,
		Overwrite:      req.Overwrite,
		FileName:       cleanName,
		MediaType:      mediaType,
		DeclaredSize:   req.Size,
		HashState:      hashState,
		StagingRef:     uploadStagingRelPath(destPath, sid),
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
		slog.ErrorContext(ctx, "failed to create upload session", "error", err)
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

func (s *Server) loadBoundUploadSession(
	ctx context.Context,
	w http.ResponseWriter,
	userIdent UserIdentity,
	scope uploadTargetScope,
	sid string,
) (*store.UploadSession, bool) {
	sess, err := s.store.GetUploadSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Upload session")
			return nil, false
		}
		slog.ErrorContext(ctx, "failed to load upload session", "session_id", sid, "error", err)
		InternalError(w)
		return nil, false
	}
	if sess.OwnerKind != "user" || sess.OwnerID != userIdent.ID() || !scope.matches(sess) {
		NotFound(w, "Upload session")
		return nil, false
	}
	return sess, true
}

func (s *Server) handleUploadChunk(
	w http.ResponseWriter,
	r *http.Request,
	userIdent UserIdentity,
	scope uploadTargetScope,
	sink uploadSink,
	sid string,
) {
	ctx := r.Context()

	sess, ok := s.loadBoundUploadSession(ctx, w, userIdent, scope, sid)
	if !ok {
		return
	}
	if sess.State != store.UploadStateUploading {
		writeError(w, http.StatusConflict, errCodeUploadInvalidState,
			fmt.Sprintf("Upload session is in state %s", sess.State),
			map[string]interface{}{"state": string(sess.State)})
		return
	}

	// Parse offset query parameter and enforce size checks BEFORE claiming the
	// lease so refused calls never touch the row or extend idle_expires_at.
	offsetRaw := r.URL.Query().Get("offset")
	if offsetRaw == "" {
		BadRequest(w, "Missing required query parameter: offset")
		return
	}
	offset, err := strconv.ParseInt(offsetRaw, 10, 64)
	if err != nil || offset < 0 {
		BadRequest(w, "Invalid offset query parameter")
		return
	}

	cfg := s.uploadsConfig()
	n := r.ContentLength
	if n <= 0 || n > cfg.ChunkBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errCodeUploadChunkTooLarge,
			fmt.Sprintf("Chunk size %d must be between 1 and %d bytes", n, cfg.ChunkBytes),
			map[string]interface{}{"chunkBytes": cfg.ChunkBytes})
		return
	}
	if offset > sess.DeclaredSize || n > sess.DeclaredSize-offset {
		writeError(w, http.StatusRequestEntityTooLarge, errCodeUploadDeclaredExceeded,
			fmt.Sprintf("Chunk range [%d, %d) exceeds declared size %d", offset, offset+n, sess.DeclaredSize),
			map[string]interface{}{"declaredSize": sess.DeclaredSize})
		return
	}

	token, err := newUploadRandomHex(16)
	if err != nil {
		InternalError(w)
		return
	}
	claimed, err := s.store.ClaimUploadLease(ctx, sid, store.UploadStateUploading, offset, token, uploadLeaseTTL)
	if err != nil {
		var om *store.OffsetMismatchError
		switch {
		case errors.As(err, &om):
			writeError(w, http.StatusConflict, errCodeUploadOffsetMismatch,
				fmt.Sprintf("Expected offset %d, got %d", om.Received, om.Expected),
				map[string]interface{}{"received": om.Received})
		case errors.Is(err, store.ErrBusy):
			writeError(w, http.StatusConflict, errCodeUploadBusy,
				"Upload session is busy; retry shortly", nil)
		case errors.Is(err, store.ErrStateChanged):
			cur, gerr := s.store.GetUploadSession(ctx, sid)
			stateStr := ""
			if gerr == nil && cur != nil {
				stateStr = effectiveUploadStateString(cur, uploadNowFn())
			}
			writeError(w, http.StatusConflict, errCodeUploadInvalidState,
				"Upload session is no longer uploading",
				map[string]interface{}{"state": stateStr})
		case errors.Is(err, store.ErrNotFound):
			NotFound(w, "Upload session")
		default:
			slog.ErrorContext(ctx, "failed to claim upload lease", "session_id", sid, "error", err)
			InternalError(w)
		}
		return
	}

	// Extend per-request read and write deadlines without changing global server timeouts (§3.10).
	if rcErr := http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadChunkDeadline)); rcErr != nil {
		slog.DebugContext(ctx, "upload chunk: SetReadDeadline not applied", "error", rcErr)
	}
	extendWriteDeadline(ctx, w, s.config.WriteTimeout, uploadChunkDeadline+uploadLeaseMargin)
	chunkCtx, cancel := context.WithTimeout(ctx, uploadChunkDeadline)
	defer cancel()

	h := sha256.New()
	if len(claimed.HashState) > 0 {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(claimed.HashState); err != nil {
			_ = s.store.ReleaseUploadLease(context.Background(), sid, token)
			slog.ErrorContext(ctx, "failed to unmarshal upload hash state", "session_id", sid, "error", err)
			InternalError(w)
			return
		}
	}

	var (
		headBuf    []byte
		bodyReader io.Reader = io.LimitReader(r.Body, n+1)
		tee        io.Reader
	)
	if offset == 0 && scope.kind == store.UploadTargetChatAttachment {
		// Classify the first <=512 bytes before staging anything (§3.5).
		capLen := n
		if capLen > uploadHeadCaptureBytes {
			capLen = uploadHeadCaptureBytes
		}
		headBuf = make([]byte, capLen)
		if _, rerr := io.ReadFull(bodyReader, headBuf); rerr != nil {
			_ = s.store.ReleaseUploadLease(context.Background(), sid, token)
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				BadRequest(w, "Chunk body shorter than Content-Length")
				return
			}
			slog.WarnContext(ctx, "upload chunk head read failed", "session_id", sid, "error", rerr)
			InternalError(w)
			return
		}
		if _, cerr := ClassifyAttachment(claimed.FileName, headBuf); cerr != nil {
			_ = s.store.ReleaseUploadLease(context.Background(), sid, token)
			writeError(w, http.StatusBadRequest, errCodeUploadDisallowedMediaType, cerr.Error(), nil)
			return
		}
		_, _ = h.Write(headBuf)
		tee = io.MultiReader(bytes.NewReader(headBuf), io.TeeReader(bodyReader, h))
	} else {
		tee = io.TeeReader(bodyReader, h)
		if offset == 0 {
			capLen := n
			if capLen > uploadHeadCaptureBytes {
				capLen = uploadHeadCaptureBytes
			}
			headBuf = make([]byte, 0, capLen)
			tee = io.TeeReader(tee, &headCaptureWriter{buf: &headBuf, limit: int(capLen)})
		}
	}

	partRef, writeErr := sink.WriteChunk(chunkCtx, claimed, offset, token, tee, n)
	if writeErr != nil {
		if errors.Is(writeErr, errUploadIntegrityMismatch) {
			failCtx, failCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.store.TransitionUploadSession(failCtx, sid, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateFailed, token, nil)
			_ = sink.Cleanup(failCtx, claimed)
			failCancel()
			writeError(w, http.StatusConflict, errCodeUploadIntegrityMismatch,
				"Upload staging file is missing or corrupted; restart the upload", nil)
			return
		}
		_ = s.store.ReleaseUploadLease(context.Background(), sid, token)
		switch {
		case errors.Is(writeErr, errUploadChunkTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, errCodeUploadChunkTooLarge,
				"Chunk body exceeds declared Content-Length", nil)
		case errors.Is(writeErr, errUploadShortBody):
			BadRequest(w, "Chunk body shorter than Content-Length")
		case errors.Is(writeErr, errWorkspaceWriteRefused):
			writeWorkspaceFileError(w, "upload", claimed.DestPath, writeErr)
		default:
			slog.WarnContext(ctx, "upload chunk write failed", "session_id", sid, "offset", offset, "error", writeErr)
			InternalError(w)
		}
		return
	}

	newHashState, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		_ = s.store.ReleaseUploadLease(context.Background(), sid, token)
		InternalError(w)
		return
	}

	now := uploadNowFn()
	newIdle := effectiveUploadExpiry(now.Add(cfg.SessionIdleTTL), claimed.HardExpiresAt)
	commit := store.UploadChunkCommit{
		Received:      offset + n,
		HashState:     newHashState,
		IdleExpiresAt: newIdle,
	}
	if offset == 0 && len(headBuf) > 0 {
		commit.Head = headBuf
	}
	if partRef != "" {
		parts := make([]string, len(claimed.Parts)+1)
		copy(parts, claimed.Parts)
		parts[len(claimed.Parts)] = partRef
		commit.Parts = parts
	}

	commitCtx, commitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = s.store.CommitUploadChunk(commitCtx, sid, token, commit)
	commitCancel()
	if err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			writeError(w, http.StatusConflict, errCodeUploadBusy,
				"Upload lease lost during chunk commit; retry", nil)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Upload session")
			return
		}
		relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.store.ReleaseUploadLease(relCtx, sid, token)
		relCancel()
		slog.ErrorContext(ctx, "failed to commit upload chunk", "session_id", sid, "error", err)
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, UploadChunkResponse{
		Received:  commit.Received,
		ExpiresAt: effectiveUploadExpiry(newIdle, claimed.HardExpiresAt),
	})
}

func (s *Server) handleUploadStatus(
	w http.ResponseWriter,
	r *http.Request,
	userIdent UserIdentity,
	scope uploadTargetScope,
	sid string,
) {
	sess, ok := s.loadBoundUploadSession(r.Context(), w, userIdent, scope, sid)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, UploadStatusResponse{
		ID:           sess.ID,
		State:        store.UploadSessionState(effectiveUploadStateString(sess, uploadNowFn())),
		Received:     sess.Received,
		DeclaredSize: sess.DeclaredSize,
		ExpiresAt:    effectiveUploadExpiry(sess.IdleExpiresAt, sess.HardExpiresAt),
	})
}

func (s *Server) handleUploadComplete(
	w http.ResponseWriter,
	r *http.Request,
	userIdent UserIdentity,
	scope uploadTargetScope,
	sink uploadSink,
	sid string,
) {
	ctx := r.Context()

	sess, ok := s.loadBoundUploadSession(ctx, w, userIdent, scope, sid)
	if !ok {
		return
	}

	// Replay stored result if already completed (§3.7).
	if sess.State == store.UploadStateCompleted {
		writeRawJSON(w, http.StatusOK, sess.Result)
		return
	}

	// Re-check effective per-file limit at complete (§3.4).
	maxBytes := s.uploadMaxFileBytes(ctx, scope.kind)
	if sess.DeclaredSize > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errCodeUploadDeclaredExceeded,
			fmt.Sprintf("File size %d exceeds maximum allowed size of %d bytes", sess.DeclaredSize, maxBytes),
			map[string]interface{}{"maxBytes": maxBytes})
		return
	}

	token, err := newUploadRandomHex(16)
	if err != nil {
		InternalError(w)
		return
	}

	var claimed *store.UploadSession
	switch sess.State {
	case store.UploadStateUploading:
		if sess.Received < sess.DeclaredSize {
			writeError(w, http.StatusConflict, errCodeUploadIncomplete,
				fmt.Sprintf("Upload incomplete: received %d of %d bytes", sess.Received, sess.DeclaredSize),
				map[string]interface{}{"received": sess.Received, "declaredSize": sess.DeclaredSize})
			return
		}
		claimed, err = s.store.ClaimUploadLease(ctx, sid, store.UploadStateUploading, sess.DeclaredSize, token, uploadLeaseTTL)
		if err != nil {
			s.writeCompleteClaimError(ctx, w, sid, err)
			return
		}
		if err := s.store.TransitionUploadSession(ctx, sid, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateFinalizing, token, nil); err != nil {
			_ = s.store.ReleaseUploadLease(context.Background(), sid, token)
			s.writeCompleteClaimError(ctx, w, sid, err)
			return
		}

	case store.UploadStateFinalizing:
		// Another replica may hold the finalize lease; if it expired, re-claim
		// and re-run idempotent finalize (§3.7).
		claimed, err = s.store.ClaimUploadLease(ctx, sid, store.UploadStateFinalizing, sess.DeclaredSize, token, uploadLeaseTTL)
		if err != nil {
			if errors.Is(err, store.ErrBusy) {
				writeJSON(w, http.StatusAccepted, map[string]string{"state": string(store.UploadStateFinalizing)})
				return
			}
			s.writeCompleteClaimError(ctx, w, sid, err)
			return
		}

	default:
		writeError(w, http.StatusConflict, errCodeUploadInvalidState,
			fmt.Sprintf("Upload session is in state %s", sess.State),
			map[string]interface{}{"state": string(sess.State)})
		return
	}

	// Extend connection write deadline for finalize (covers rare takeover re-hash, §3.10).
	extendWriteDeadline(ctx, w, s.config.WriteTimeout, uploadFinalizeBudget)
	finCtx, cancel := context.WithTimeout(ctx, uploadFinalizeBudget)
	defer cancel()

	digestHex, err := finalizeUploadDigestHex(claimed.HashState)
	if err != nil {
		_ = s.store.TransitionUploadSession(context.Background(), sid, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateFailed, token, nil)
		_ = sink.Cleanup(context.Background(), claimed)
		slog.ErrorContext(ctx, "failed to compute upload digest", "session_id", sid, "error", err)
		InternalError(w)
		return
	}

	result, finErr := sink.Finalize(finCtx, claimed, digestHex)
	if finErr != nil {
		switch {
		case errors.Is(finErr, errUploadIntegrityMismatch):
			_ = s.store.TransitionUploadSession(context.Background(), sid, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateFailed, token, nil)
			_ = sink.Cleanup(context.Background(), claimed)
			writeError(w, http.StatusConflict, errCodeUploadIntegrityMismatch,
				"Uploaded file failed integrity verification after lease takeover", nil)
		case errors.Is(finErr, errUploadTargetExists):
			_ = s.store.TransitionUploadSession(context.Background(), sid, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateFailed, token, nil)
			_ = sink.Cleanup(context.Background(), claimed)
			Conflict(w, "Target file already exists and overwrite is false")
		case errors.Is(finErr, errWorkspaceWriteRefused):
			_ = s.store.TransitionUploadSession(context.Background(), sid, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateFailed, token, nil)
			_ = sink.Cleanup(context.Background(), claimed)
			writeWorkspaceFileError(w, "upload", claimed.DestPath, finErr)
		case errors.Is(finErr, errUploadDisallowedMediaType):
			_ = s.store.TransitionUploadSession(context.Background(), sid, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateFailed, token, nil)
			_ = sink.Cleanup(context.Background(), claimed)
			writeError(w, http.StatusBadRequest, errCodeUploadDisallowedMediaType, finErr.Error(), nil)
		case errors.Is(finErr, artifacts.ErrAttachmentTooLarge):
			_ = s.store.TransitionUploadSession(context.Background(), sid, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateFailed, token, nil)
			_ = sink.Cleanup(context.Background(), claimed)
			writeError(w, http.StatusRequestEntityTooLarge, errCodeUploadDeclaredExceeded,
				fmt.Sprintf("File size %d exceeds maximum allowed size of %d bytes", claimed.DeclaredSize, maxBytes),
				map[string]interface{}{"maxBytes": maxBytes})
		case errors.Is(finErr, artifacts.ErrAttachmentName):
			_ = s.store.TransitionUploadSession(context.Background(), sid, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateFailed, token, nil)
			_ = sink.Cleanup(context.Background(), claimed)
			BadRequest(w, "Invalid attachment filename")
		case errors.Is(finErr, artifacts.ErrComposerUnsupported):
			_ = s.store.ReleaseUploadLease(context.Background(), sid, token)
			writeError(w, http.StatusNotImplemented, errCodeLargeUploadsUnsupportedStorage,
				"Configured blob storage does not support compose required for large chat attachments", nil)
		default:
			// Transient error: release the finalize lease so a retry can re-claim.
			_ = s.store.ReleaseUploadLease(context.Background(), sid, token)
			slog.ErrorContext(ctx, "upload finalize failed", "session_id", sid, "error", finErr)
			InternalError(w)
		}
		return
	}

	compCtx, compCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = s.store.TransitionUploadSession(compCtx, sid, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateCompleted, token, result)
	compCancel()
	if err != nil {
		if errors.Is(err, store.ErrLeaseLost) || errors.Is(err, store.ErrStateChanged) {
			if cur, gerr := s.store.GetUploadSession(ctx, sid); gerr == nil && cur.State == store.UploadStateCompleted {
				writeRawJSON(w, http.StatusOK, cur.Result)
				return
			}
			writeError(w, http.StatusConflict, errCodeUploadBusy,
				"Upload lease lost during finalize", nil)
			return
		}
		slog.ErrorContext(ctx, "failed to record upload completion", "session_id", sid, "error", err)
		InternalError(w)
		return
	}

	writeRawJSON(w, http.StatusOK, result)
}

func (s *Server) writeCompleteClaimError(ctx context.Context, w http.ResponseWriter, sid string, err error) {
	var om *store.OffsetMismatchError
	switch {
	case errors.As(err, &om):
		writeError(w, http.StatusConflict, errCodeUploadIncomplete,
			fmt.Sprintf("Upload incomplete: received %d bytes", om.Received),
			map[string]interface{}{"received": om.Received})
	case errors.Is(err, store.ErrBusy):
		writeError(w, http.StatusConflict, errCodeUploadBusy,
			"Upload session is busy; retry shortly", nil)
	case errors.Is(err, store.ErrStateChanged):
		cur, gerr := s.store.GetUploadSession(ctx, sid)
		if gerr == nil && cur != nil {
			switch cur.State {
			case store.UploadStateCompleted:
				writeRawJSON(w, http.StatusOK, cur.Result)
				return
			case store.UploadStateFinalizing:
				writeJSON(w, http.StatusAccepted, map[string]string{"state": string(store.UploadStateFinalizing)})
				return
			default:
				stateStr := effectiveUploadStateString(cur, uploadNowFn())
				writeError(w, http.StatusConflict, errCodeUploadInvalidState,
					fmt.Sprintf("Upload session is in state %s", stateStr),
					map[string]interface{}{"state": stateStr})
				return
			}
		}
		writeError(w, http.StatusConflict, errCodeUploadInvalidState,
			"Upload session state changed", nil)
	case errors.Is(err, store.ErrNotFound):
		NotFound(w, "Upload session")
	default:
		slog.ErrorContext(ctx, "failed to claim finalize lease", "session_id", sid, "error", err)
		InternalError(w)
	}
}

func (s *Server) handleUploadAbort(
	w http.ResponseWriter,
	r *http.Request,
	userIdent UserIdentity,
	scope uploadTargetScope,
	sink uploadSink,
	sid string,
) {
	ctx := r.Context()

	sess, ok := s.loadBoundUploadSession(ctx, w, userIdent, scope, sid)
	if !ok {
		return
	}
	if sess.State == store.UploadStateAborted {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if sess.State != store.UploadStateUploading {
		writeError(w, http.StatusConflict, errCodeUploadInvalidState,
			fmt.Sprintf("Upload session is in state %s", sess.State),
			map[string]interface{}{"state": string(sess.State)})
		return
	}

	if err := s.store.TransitionUploadSession(ctx, sid, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateAborted, "", nil); err != nil {
		switch {
		case errors.Is(err, store.ErrBusy):
			writeError(w, http.StatusConflict, errCodeUploadBusy,
				"Upload session has an active chunk write; retry abort shortly", nil)
		case errors.Is(err, store.ErrStateChanged):
			cur, gerr := s.store.GetUploadSession(ctx, sid)
			if gerr == nil && cur != nil && cur.State == store.UploadStateAborted {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeError(w, http.StatusConflict, errCodeUploadInvalidState,
				"Upload session is no longer uploading", nil)
		case errors.Is(err, store.ErrNotFound):
			NotFound(w, "Upload session")
		default:
			slog.ErrorContext(ctx, "failed to abort upload session", "session_id", sid, "error", err)
			InternalError(w)
		}
		return
	}

	if cerr := sink.Cleanup(ctx, sess); cerr != nil {
		slog.WarnContext(ctx, "best-effort upload abort cleanup failed", "session_id", sid, "error", cerr)
	}
	w.WriteHeader(http.StatusNoContent)
}

func finalizeUploadDigestHex(hashState []byte) (string, error) {
	h := sha256.New()
	if len(hashState) > 0 {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(hashState); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeRawJSON(w http.ResponseWriter, status int, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(raw) == 0 {
		_, _ = w.Write([]byte("{}"))
		return
	}
	_, _ = w.Write(raw)
}

type headCaptureWriter struct {
	buf   *[]byte
	limit int
}

func (w *headCaptureWriter) Write(p []byte) (int, error) {
	rem := w.limit - len(*w.buf)
	if rem > 0 {
		take := len(p)
		if take > rem {
			take = rem
		}
		*w.buf = append(*w.buf, p[:take]...)
	}
	return len(p), nil
}
