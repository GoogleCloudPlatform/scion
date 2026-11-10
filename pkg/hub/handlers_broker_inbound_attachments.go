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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Attachments sent through broker plugins (chat integrations that
// authenticate with broker HMAC).
//
// A plugin first asks POST /api/v1/broker/inbound/attachments/check whether
// the sender may send attachments and where they go; it writes nothing.
// When the answer is "artifact" and authorized, the plugin uploads the files
// on POST /api/v1/broker/inbound/attachments, where they are stored as
// unsent uploads owned by the sender user, and then names the returned ids
// in attachment_ids on POST /api/v1/broker/inbound. Who reads an attachment
// once it is sent is the artifact service's decision, as for every other
// attachment stored as an artifact.

// Paths of the broker attachment routes.
const (
	brokerAttachmentsCheckPath  = "/api/v1/broker/inbound/attachments/check"
	brokerAttachmentsUploadPath = "/api/v1/broker/inbound/attachments"
)

// Values of brokerAttachmentCheckResponse.Uploads.
const (
	// brokerUploadsArtifact: upload on the broker attachment route and send
	// the ids.
	brokerUploadsArtifact = "artifact"
	// brokerUploadsLegacy: attachments stored as artifacts are off on this
	// hub while the experiment exists; the plugin keeps its own staging.
	brokerUploadsLegacy = "legacy"
	// brokerUploadsUnavailable: no attachment can be delivered; the plugin
	// sends the text only.
	brokerUploadsUnavailable = "unavailable"
)

// codeAttachmentsUnavailable is the error code of the 409 the upload route
// answers when uploads are not stored as artifacts on this hub.
const codeAttachmentsUnavailable = "attachments_unavailable"

// brokerAttachmentEnvelopeBytes is the allowance for the multipart framing
// and the metadata fields of an upload, above its file bytes.
const brokerAttachmentEnvelopeBytes int64 = 64 << 10

// maxBrokerAttachmentTargets bounds the targets of one check or upload. A
// plugin names every recipient of a message, an @all included, and each
// target costs one agent lookup and one message authorization before
// anything else happens, so the bound keeps that work small. The hub sets
// no per-project agent limit by default to derive it from; 256 is above
// the hub's own per-message recipient bounds (messages.MaxGroupRecipients,
// messages.MaxMentionRecipients), so only an unusually wide message is
// refused, with codeTooManyTargets, which the plugin reports to the sender.
const maxBrokerAttachmentTargets = 256

// codeTooManyTargets is the error code of the 400 for a check or upload
// naming more than maxBrokerAttachmentTargets targets.
const codeTooManyTargets = "too_many_targets"

// maxBrokerAttachmentMetadataBytes bounds the metadata values of one
// upload: the sum of each field's name and value, not the multipart
// framing or a file part's file name. The framing of up to
// maxBrokerAttachmentTargets fields still fits the framing allowance
// (brokerAttachmentEnvelopeBytes); a very long file name can still take
// the body over the cap, and is then refused with 413, nothing stored.
const maxBrokerAttachmentMetadataBytes = 32 << 10

// codeMetadataTooLarge is the error code of the 400 for an upload whose
// metadata exceeds maxBrokerAttachmentMetadataBytes.
const codeMetadataTooLarge = "metadata_too_large"

// maxBrokerAttachmentFieldBytes bounds one metadata field of an upload.
const maxBrokerAttachmentFieldBytes = 4 << 10

// brokerAttachmentBodyCap is the largest request body the broker
// attachment upload route accepts, framing included. It is the one place
// that sets the size limit of broker attachments: the check answers
// delivery_max_bytes from it and the upload enforces it.
//
// A broker request is HMAC-signed over its whole body, and the broker
// authentication middleware reads and hashes that body before any handler
// runs, bounded at apiclient.MaxSignedRequestBodyBytes. So no upload on this
// route can be larger than that, whatever the messaging delivery budget.
func brokerAttachmentBodyCap() int64 {
	return apiclient.MaxSignedRequestBodyBytes
}

// brokerAttachmentDeliveryMaxBytes is the total file size a broker plugin
// may upload for one message: the messaging delivery budget
// (attachment_delivery_max_bytes; 0 means delivery is off), clamped to the
// body cap less the framing allowance.
func (s *Server) brokerAttachmentDeliveryMaxBytes() int64 {
	limit := s.attachmentDeliveryMaxBytes()
	if max := brokerAttachmentBodyCap() - brokerAttachmentEnvelopeBytes; limit > max {
		limit = max
	}
	if limit < 0 {
		limit = 0
	}
	return limit
}

// brokerAttachmentUploads answers where broker plugin attachments go on
// this hub: "artifact" when the experiment is on and the artifact service
// is ready; "legacy" while the experiment is still registered but that is
// false; "unavailable" once the experiment is gone and artifacts are not
// ready. The answer does not depend on the caller.
func (s *Server) brokerAttachmentUploads() string {
	if s.experimentEnabled(experiments.AttachmentArtifacts) && s.attachmentArtifactsReady() {
		return brokerUploadsArtifact
	}
	if _, registered := s.experimentRegistry().Lookup(experiments.AttachmentArtifacts); registered {
		return brokerUploadsLegacy
	}
	return brokerUploadsUnavailable
}

// brokerAttachmentSurfaces are the surfaces a broker attachment request may
// name.
var brokerAttachmentSurfaces = map[string]bool{"telegram": true, "discord": true}

// brokerAttachmentAudiences are the audience attestations an upload may
// carry. The value is recorded in the audit line only: every plugin
// conversation is delivered to its recipients' inboxes, never projected.
var brokerAttachmentAudiences = map[string]bool{"direct": true, "group": true}

// brokerAttachmentRequest is the sender, project and targets of a broker
// attachment check or upload.
type brokerAttachmentRequest struct {
	Sender    string   `json:"sender"`
	ProjectID string   `json:"project_id"`
	Targets   []string `json:"targets,omitempty"`
	Surface   string   `json:"surface"`
	// Audience is set on uploads only.
	Audience string `json:"-"`
}

// tooManyTargets reports whether r names more targets than accepted.
func (r *brokerAttachmentRequest) tooManyTargets() bool {
	return len(r.Targets) > maxBrokerAttachmentTargets
}

// validate returns the validation message for a malformed request, or "".
func (r *brokerAttachmentRequest) validate(upload bool) string {
	switch {
	case !strings.HasPrefix(r.Sender, "user:") || r.Sender == "user:":
		return "sender must be user:<email>"
	case r.ProjectID == "":
		return "project_id is required"
	case !brokerAttachmentSurfaces[r.Surface]:
		return "surface must be telegram or discord"
	case upload && !brokerAttachmentAudiences[r.Audience]:
		return "audience must be direct or group"
	}
	for _, t := range r.Targets {
		if t == "" {
			return "targets must not be empty"
		}
	}
	return ""
}

// brokerAttachmentCheckResponse is the answer of the check route.
type brokerAttachmentCheckResponse struct {
	Authorized bool   `json:"authorized"`
	Uploads    string `json:"uploads"`
	// DeliveryMaxBytes, with "artifact", is the largest file one upload
	// may carry: brokerAttachmentDeliveryMaxBytes, or the per-file
	// artifact limit when that is smaller.
	DeliveryMaxBytes *int64 `json:"delivery_max_bytes,omitempty"`
}

// authorizeBrokerAttachmentRequest resolves and authorizes the sender of
// req. It returns the sender (nil when it could not be resolved) and the
// refusal (403 or 500), nil when the sender is authorized.
func (s *Server) authorizeBrokerAttachmentRequest(ctx context.Context, req *brokerAttachmentRequest) (*AuthenticatedUser, *apiError) {
	sender, refusal := s.resolveBrokerInboundSender(ctx, req.Sender)
	if refusal != nil {
		return nil, refusal
	}
	if refusal := s.authorizeBrokerAttachmentSender(ctx, sender, req.ProjectID, req.Targets); refusal != nil {
		return sender, refusal
	}
	return sender, nil
}

// brokerAttachmentAuditLog returns the audit logger of a broker attachment
// request: broker id and plugin name. File names are never logged.
func (s *Server) brokerAttachmentAuditLog(r *http.Request, broker BrokerIdentity) *slog.Logger {
	return s.messageLog.With(
		"broker_id", broker.ID(),
		"plugin_name", r.Header.Get("X-Scion-Plugin-Name"),
		logging.AttrLogKind, logging.LogKindEvent,
	)
}

// handleBrokerAttachmentsCheck handles POST
// /api/v1/broker/inbound/attachments/check (broker HMAC). It writes
// nothing. A refused sender is a 200 with authorized false, so the plugin
// can tell the user; a malformed request is 400. The route has no 404.
func (s *Server) handleBrokerAttachmentsCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed, "broker HMAC authentication required", nil)
		return
	}
	var req brokerAttachmentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}
	if req.tooManyTargets() {
		writeTooManyTargets(w)
		return
	}
	if msg := req.validate(false); msg != "" {
		ValidationError(w, msg, nil)
		return
	}
	log := s.brokerAttachmentAuditLog(r, broker)
	ctx := withStandingMemo(r.Context())
	answer := brokerAttachmentCheckResponse{Uploads: s.brokerAttachmentUploads()}
	sender, refusal := s.authorizeBrokerAttachmentRequest(ctx, &req)
	if refusal != nil && refusal.status >= http.StatusInternalServerError {
		log.Error("broker attachment check failed", "project_id", req.ProjectID, "refusal", brokerRefusalLogText(refusal))
		refusal.write(w)
		return
	}
	answer.Authorized = refusal == nil
	if answer.Uploads == brokerUploadsArtifact {
		// delivery_max_bytes is the largest single file the hub takes: the
		// smaller of the total for one message and the per-file artifact
		// limit. The upload enforces the total across all its files.
		// (No builtin min here: the package's tests declare their own
		// min(int, int).)
		limit := s.brokerAttachmentDeliveryMaxBytes()
		if perFile := s.attachmentMaxBytes(ctx, true); perFile < limit {
			limit = perFile
		}
		answer.DeliveryMaxBytes = &limit
	}
	decision := "authorized"
	senderID := ""
	if sender != nil {
		senderID = sender.ID()
	}
	if refusal != nil {
		decision = "refused: " + refusal.code
	}
	log.Info("broker attachment check",
		"sender_id", senderID, "project_id", req.ProjectID, "decision", decision,
		"uploads", answer.Uploads, "count", 0, "total_bytes", 0)
	writeJSON(w, http.StatusOK, answer)
}

// handleBrokerAttachmentsUpload handles POST
// /api/v1/broker/inbound/attachments (broker HMAC, multipart/form-data).
//
// The metadata fields come first, in this order: sender, project_id,
// surface, audience, then zero or more targets (one slug each). One or more
// file parts named "file", each with a filename, follow. Order of checks:
//  1. broker identity; the body is capped (brokerAttachmentBodyCap) before
//     any multipart read, and parts are read one at a time;
//  2. the sender is resolved (resolveBrokerInboundSender);
//  3. the sender is authorized for the project and every target
//     (authorizeBrokerAttachmentSender);
//  4. uploads must be stored as artifacts on this hub, else 409;
//  5. each file is checked (checkBrokerAttachmentFile) and
//  6. stored as an unsent upload of the sender user (storeBrokerAttachment).
//
// The broker HMAC middleware buffers and hashes the whole body (bounded at
// apiclient.MaxSignedRequestBodyBytes) before this handler runs. The
// handler then runs the sender and target checks (steps 2-4) before parsing
// any file part, and stores nothing on refusal. This is not end-to-end
// streaming: the ordering is a property of this handler.
func (s *Server) handleBrokerAttachmentsUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed, "broker HMAC authentication required", nil)
		return
	}
	bodyCap := brokerAttachmentBodyCap()
	if r.ContentLength > bodyCap {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large", nil)
		return
	}
	// The reader is capped at the fixed signed-body cap, because the broker
	// HMAC middleware already bounds the body there; the total for one
	// message is enforced separately, per file, in readBrokerAttachmentFile.
	r.Body = http.MaxBytesReader(w, r.Body, bodyCap)
	mr, err := r.MultipartReader()
	if err != nil {
		BadRequest(w, "request must be multipart/form-data")
		return
	}
	log := s.brokerAttachmentAuditLog(r, broker)

	req, first, refusal := readBrokerAttachmentMetadata(mr)
	if refusal != nil {
		refusal.write(w)
		return
	}
	ctx := withStandingMemo(r.Context())
	sender, refusal := s.authorizeBrokerAttachmentRequest(ctx, req)
	if refusal != nil {
		if refusal.status >= http.StatusInternalServerError {
			log.Error("broker attachment upload failed", "project_id", req.ProjectID, "refusal", brokerRefusalLogText(refusal))
		} else {
			senderID := ""
			if sender != nil {
				senderID = sender.ID()
			}
			log.Warn("broker attachment upload",
				"sender_id", senderID, "project_id", req.ProjectID, "decision", "refused: "+refusal.code,
				"audience", req.Audience, "count", 0, "total_bytes", 0)
		}
		refusal.write(w)
		return
	}
	if s.brokerAttachmentUploads() != brokerUploadsArtifact {
		log.Info("broker attachment upload",
			"sender_id", sender.ID(), "project_id", req.ProjectID, "decision", "refused: "+codeAttachmentsUnavailable,
			"audience", req.Audience, "count", 0, "total_bytes", 0)
		writeError(w, http.StatusConflict, codeAttachmentsUnavailable,
			"attachments stored as artifacts are not available", nil)
		return
	}

	// Pass 1: read and check every part before anything is stored. The
	// body is bounded (brokerAttachmentBodyCap), so holding the files in
	// memory is bounded too. A refusal of the whole request (a body over
	// the cap, files over the total, too many files, a part out of order)
	// leaves nothing stored.
	batch := newBrokerAttachmentBatch(s.attachmentMaxBytes(ctx, true), s.brokerAttachmentDeliveryMaxBytes())
	var pending []brokerAttachmentPending
	failures := make([]attachmentUploadFailure, 0)
	// refuse writes a refusal of the whole request after authorization,
	// with its audit line (count and bytes read so far; never file names).
	refuse := func(reason string, write func()) {
		log.Warn("broker attachment upload",
			"sender_id", sender.ID(), "project_id", req.ProjectID, "decision", "refused: "+reason,
			"audience", req.Audience, "count", batch.count, "total_bytes", batch.total)
		write()
	}
	tooLarge := func() {
		refuse("over size limit", func() {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("attachments exceed the total of %d bytes for one message", batch.maxTotal), nil)
		})
	}
	badBody := func() {
		refuse("invalid multipart body", func() { BadRequest(w, "invalid multipart body") })
	}
	part := first
	for part != nil {
		if part.FormName() != "file" || part.FileName() == "" {
			_ = part.Close()
			refuse("part out of order", func() { BadRequest(w, "only file parts may follow the metadata fields") })
			return
		}
		if batch.count == MaxAttachmentsPerMessage {
			_ = part.Close()
			refuse("too many files", func() {
				ValidationError(w, fmt.Sprintf("too many files (max %d)", MaxAttachmentsPerMessage), nil)
			})
			return
		}
		batch.count++
		name := part.FileName()
		f, err := readBrokerAttachmentFile(batch, name, part)
		_ = part.Close()
		switch {
		case isBodyTooLarge(err) || errors.Is(err, errBrokerAttachmentsOverTotal):
			tooLarge()
			return
		case err != nil:
			var rejection attachmentRejection
			if !errors.As(err, &rejection) {
				log.Error("Broker attachment upload failed", "sender_id", sender.ID(), "error", err)
				badBody()
				return
			}
			failures = append(failures, attachmentUploadFailure{Name: name, Error: uploadFailureMessage(err)})
		default:
			pending = append(pending, f)
		}
		part, err = mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if isBodyTooLarge(err) {
				tooLarge()
				return
			}
			badBody()
			return
		}
	}
	if batch.count == 0 {
		refuse("no files", func() { ValidationError(w, "no files uploaded", nil) })
		return
	}

	// Pass 2: the request is acceptable; store each checked file as an
	// unsent upload of the sender. A store fault part way through leaves
	// the files stored before it in place, on purpose: the artifact service
	// has no per-upload delete, the uploads are readable only by the sender and
	// expire after the upload TTL unless a send binds them, and the user
	// upload route behaves the same way. The answer lists them, so the
	// sender can still use or ignore them.
	ownerCtx := brokerSenderContext(ctx, sender)
	results := make([]attachmentUploadResult, 0, len(pending))
	internalError := false
	for _, f := range pending {
		result, err := s.storeBrokerAttachment(ownerCtx, f)
		if err != nil {
			var rejection attachmentRejection
			if !errors.As(err, &rejection) {
				internalError = true
				log.Error("Broker attachment upload failed", "sender_id", sender.ID(), "error", err)
			}
			failures = append(failures, attachmentUploadFailure{Name: f.name, Error: uploadFailureMessage(err)})
			continue
		}
		results = append(results, result)
	}
	status := http.StatusCreated
	decision := "stored"
	if len(results) == 0 {
		status = http.StatusBadRequest
		decision = "refused: files"
		if internalError {
			status = http.StatusInternalServerError
			decision = "failed"
		}
	}
	var storedBytes int64
	for _, r := range results {
		storedBytes += r.Size
	}
	log.Info("broker attachment upload",
		"sender_id", sender.ID(), "project_id", req.ProjectID, "decision", decision,
		"audience", req.Audience, "count", len(results), "failed", len(failures), "total_bytes", storedBytes)
	writeJSON(w, status, map[string]interface{}{
		"attachments": results,
		"failures":    failures,
	})
}

// readBrokerAttachmentMetadata reads the leading metadata fields of an
// upload and returns them with the first file part (nil when there is
// none). Only part headers and the small metadata values are read; the
// first file part's content is not. A file part before the metadata is
// complete, a field out of order, an unknown field, or an oversized value
// is a 400.
func readBrokerAttachmentMetadata(mr *multipart.Reader) (*brokerAttachmentRequest, *multipart.Part, *apiError) {
	bad := func(msg string) (*brokerAttachmentRequest, *multipart.Part, *apiError) {
		return nil, nil, &apiError{status: http.StatusBadRequest, code: ErrCodeValidationError, message: msg}
	}
	ordered := []string{"sender", "project_id", "surface", "audience"}
	req := &brokerAttachmentRequest{}
	seen := 0
	metaBytes := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			if seen < len(ordered) {
				return bad("metadata fields are missing")
			}
			if msg := req.validate(true); msg != "" {
				return bad(msg)
			}
			return req, nil, nil
		}
		if err != nil {
			if isBodyTooLarge(err) {
				return nil, nil, &apiError{status: http.StatusRequestEntityTooLarge, code: "payload_too_large", message: "request body too large"}
			}
			return bad("invalid multipart body")
		}
		if part.FileName() != "" || part.FormName() == "file" {
			if seen < len(ordered) {
				_ = part.Close()
				return bad("metadata fields must precede the files")
			}
			if msg := req.validate(true); msg != "" {
				_ = part.Close()
				return bad(msg)
			}
			return req, part, nil
		}
		value, err := readBrokerAttachmentField(part)
		_ = part.Close()
		if err != nil {
			return bad(err.Error())
		}
		name := part.FormName()
		metaBytes += len(name) + len(value)
		if metaBytes > maxBrokerAttachmentMetadataBytes {
			return nil, nil, &apiError{status: http.StatusBadRequest, code: codeMetadataTooLarge,
				message: fmt.Sprintf("upload metadata exceeds %d bytes", maxBrokerAttachmentMetadataBytes)}
		}
		switch {
		case seen < len(ordered):
			if name != ordered[seen] {
				return bad(fmt.Sprintf("expected field %q, got %q", ordered[seen], name))
			}
			switch name {
			case "sender":
				req.Sender = value
			case "project_id":
				req.ProjectID = value
			case "surface":
				req.Surface = value
			case "audience":
				req.Audience = value
			}
			seen++
		case name == "targets":
			req.Targets = append(req.Targets, value)
			if req.tooManyTargets() {
				return nil, nil, tooManyTargetsError()
			}
		default:
			return bad(fmt.Sprintf("unexpected field %q", name))
		}
	}
}

// tooManyTargetsError is the refusal of a request naming more than
// maxBrokerAttachmentTargets targets.
func tooManyTargetsError() *apiError {
	return &apiError{status: http.StatusBadRequest, code: codeTooManyTargets,
		message: fmt.Sprintf("too many targets (max %d)", maxBrokerAttachmentTargets)}
}

// writeTooManyTargets writes tooManyTargetsError.
func writeTooManyTargets(w http.ResponseWriter) {
	tooManyTargetsError().write(w)
}

// readBrokerAttachmentField reads one metadata value, at most
// maxBrokerAttachmentFieldBytes.
func readBrokerAttachmentField(part *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, maxBrokerAttachmentFieldBytes+1))
	if err != nil {
		return "", errors.New("invalid multipart body")
	}
	if len(b) > maxBrokerAttachmentFieldBytes {
		return "", fmt.Errorf("field %q is too long", part.FormName())
	}
	return string(b), nil
}

// isBodyTooLarge reports whether err is the request body cap being hit.
func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// brokerAttachmentBatch is the running state of one broker upload: the
// per-file limit, the remaining total budget and the names taken so far.
// It holds no request framing, so the checks can serve any upload route.
type brokerAttachmentBatch struct {
	maxFile  int64
	maxTotal int64
	total    int64
	count    int
	names    map[string]bool
	reduced  map[string]string
}

func newBrokerAttachmentBatch(maxFile, maxTotal int64) *brokerAttachmentBatch {
	return &brokerAttachmentBatch{
		maxFile: maxFile, maxTotal: maxTotal,
		names: make(map[string]bool), reduced: make(map[string]string),
	}
}

// errBrokerAttachmentsOverTotal reports an upload whose files exceed the
// total for one message (brokerAttachmentDeliveryMaxBytes). The whole
// request is refused with 413, like a body over the cap.
var errBrokerAttachmentsOverTotal = errors.New("attachments exceed the total size for one message")

// errBrokerAttachmentOverBudget is returned while reading a file that
// would take the batch over its per-file limit or total budget.
var errBrokerAttachmentOverBudget = errors.New("attachment over the size limit")

// checkBrokerAttachmentFile checks one file of a broker upload by its name
// and first bytes (head, at most contentSniffLen): the sanitized name, no
// two files under one name or one delivered (reduced) name, no HTML or SVG,
// and a classifiable type. It returns the sanitized name and the MIME type,
// or an attachmentRejection. It records the name in batch.
func checkBrokerAttachmentFile(batch *brokerAttachmentBatch, name string, head []byte) (string, string, error) {
	safeName, err := SanitizeFilename(name)
	if err != nil {
		return "", "", attachmentRejection{msg: err.Error()}
	}
	key := strings.ToLower(safeName)
	if batch.names[key] {
		return "", "", rejectAttachment("two attachments are named %q; rename one", safeName)
	}
	reduced := strings.ToLower(reduceAttachmentName(safeName))
	if first, ok := batch.reduced[reduced]; ok {
		return "", "", rejectAttachment("two attachments, %q and %q, are delivered under one file name; rename one", first, safeName)
	}
	if messaging.IsHTMLOrSVG(safeName, head) {
		return "", "", rejectAttachment("HTML and SVG files are not accepted as attachments")
	}
	mimeType, err := ClassifyAttachment(safeName, head)
	if err != nil {
		return "", "", attachmentRejection{msg: err.Error()}
	}
	batch.names[key] = true
	batch.reduced[reduced] = safeName
	return safeName, mimeType, nil
}

// brokerAttachmentPending is one file of a broker upload that passed every
// check and is held in memory until the whole request is accepted.
type brokerAttachmentPending struct {
	name, mimeType string
	data           []byte
}

// readBrokerAttachmentFile reads one file of a broker upload into memory
// and checks it (checkBrokerAttachmentFile, then its size against the
// per-file limit and the batch total). A refusal of this file alone is an
// attachmentRejection; errBrokerAttachmentsOverTotal and a body over the
// cap refuse the whole request. Nothing is stored here.
func readBrokerAttachmentFile(batch *brokerAttachmentBatch, name string, body io.Reader) (brokerAttachmentPending, error) {
	head := make([]byte, contentSniffLen)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return brokerAttachmentPending{}, fmt.Errorf("read attachment: %w", err)
	}
	head = head[:n]
	safeName, mimeType, err := checkBrokerAttachmentFile(batch, name, head)
	if err != nil {
		return brokerAttachmentPending{}, err
	}
	limit := batch.maxFile
	if remaining := batch.maxTotal - batch.total; remaining < limit {
		limit = remaining
	}
	data, err := io.ReadAll(&budgetReader{r: io.MultiReader(bytes.NewReader(head), body), limit: limit})
	switch {
	case isBodyTooLarge(err):
		return brokerAttachmentPending{}, err
	case errors.Is(err, errBrokerAttachmentOverBudget):
		if limit < batch.maxFile {
			return brokerAttachmentPending{}, errBrokerAttachmentsOverTotal
		}
		return brokerAttachmentPending{}, rejectAttachment("file exceeds the maximum size of %d bytes", batch.maxFile)
	case err != nil:
		return brokerAttachmentPending{}, fmt.Errorf("read attachment: %w", err)
	}
	batch.total += int64(len(data))
	return brokerAttachmentPending{name: safeName, mimeType: mimeType, data: data}, nil
}

// storeBrokerAttachment stores one checked file through the artifact
// service as an unsent upload of the caller of ctx, which must be the
// sender user (brokerSenderContext): the same store call a user's own
// upload makes.
func (s *Server) storeBrokerAttachment(ctx context.Context, f brokerAttachmentPending) (attachmentUploadResult, error) {
	info, err := s.attachmentService().CreateAttachment(ctx, f.name, f.mimeType, bytes.NewReader(f.data))
	switch {
	case errors.Is(err, artifacts.ErrAttachmentTooLarge):
		return attachmentUploadResult{}, rejectAttachment("file exceeds the maximum size")
	case errors.Is(err, artifacts.ErrAttachmentName):
		return attachmentUploadResult{}, rejectAttachment("invalid filename")
	case err != nil:
		return attachmentUploadResult{}, fmt.Errorf("save attachment: %w", err)
	}
	return attachmentUploadResult{
		ID:       info.ID,
		Name:     info.Name,
		MimeType: info.MediaType,
		Size:     info.Size,
		URL:      "/api/v1/chat/attachments/" + info.ID,
	}, nil
}

// budgetReader reads r and fails with errBrokerAttachmentOverBudget once
// more than limit bytes were read.
type budgetReader struct {
	r     io.Reader
	limit int64
	n     int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.limit {
		return n, errBrokerAttachmentOverBudget
	}
	return n, err
}
