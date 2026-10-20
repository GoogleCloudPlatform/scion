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

package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Web-chat attachments stored as artifacts (ptone/scion#4595).
//
// An attachment is an artifact with one file in one version of kind
// attachment. While it waits to be sent it is in the upload scope of its
// uploader, readable by the uploader only. Sending binds it to the message
// scope of the message it was sent in, and from then on it is exactly as
// readable as that message (canReadAttachmentScope, the one read rule).
// It is never listed, shared, versioned or changed, and the artifact routes
// answer for it as for an unknown id. It is retired when its message is
// deleted or gone, or, unsent, after UploadTTL.
//
// Every method here takes and returns strings, so the hub reaches the
// service through the same narrow surface as the rest of the Host
// contract.

// UploadTTL is how long an upload may wait to be sent before the
// maintenance pass retires it.
const UploadTTL = 24 * time.Hour

// Errors of the attachment methods.
var (
	// ErrUnavailable: the artifact store or blob storage is not configured.
	ErrUnavailable = errors.New("artifacts: artifact storage is not configured")
	// ErrAttachmentTooLarge: the file exceeds the artifact file size limit.
	ErrAttachmentTooLarge = errors.New("artifacts: the attachment exceeds the file size limit")
	// ErrAttachmentName: the file name cannot name an artifact file.
	ErrAttachmentName = errors.New("artifacts: invalid attachment file name")
	// errNoPrincipal: the request has no caller the service serves.
	errNoPrincipal = errors.New("artifacts: no authenticated caller")
)

// AttachmentInfo describes one attachment: its artifact id, file name,
// media type and size.
type AttachmentInfo struct {
	ID        string
	Name      string
	MediaType string
	Size      int64
	// MessageProjectID is the project recorded at bind time
	// (Artifact.MessageProjectID), set by MessageAttachments.
	MessageProjectID *string
}

// AttachmentState is what UsableAttachment found.
type AttachmentState int

const (
	// AttachmentAbsent: no live attachment has the id; the caller may look
	// in another attachment store.
	AttachmentAbsent AttachmentState = iota
	// AttachmentUnusable: an attachment has the id, but it is not an unsent
	// upload of the sender. The caller refuses, as for an unknown id, and
	// does not look elsewhere.
	AttachmentUnusable
	// AttachmentUsable: an unsent upload of the sender.
	AttachmentUsable
	// AttachmentSentBySender: an attachment the sender uploaded and already
	// sent in a message. It is not usable again; the host may let a retry
	// of the send that bound it through to its idempotency check.
	AttachmentSentBySender
)

// ServeOutcome is what ServeAttachment did.
type ServeOutcome int

const (
	// ServeNotFound: no live attachment has the id. Nothing was written;
	// the caller may look in another attachment store.
	ServeNotFound ServeOutcome = iota
	// ServeRefused: an attachment has the id but the caller may not read
	// it. Nothing was written; the caller answers exactly as for an
	// unknown id and does not look elsewhere.
	ServeRefused
	// ServeWritten: the response was written (the file, a redirect to it,
	// or a server error).
	ServeWritten
)

// CreateAttachment stores body, a file named name of media type mediaType
// (both already validated and classified by the host), as an upload of the
// caller: a new artifact in the caller's upload scope with one ready
// version of kind attachment. The size limit is the artifact file size
// limit.
func (s *Service) CreateAttachment(ctx context.Context, name, mediaType string, body io.Reader) (AttachmentInfo, error) {
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok {
		return AttachmentInfo{}, errNoPrincipal
	}
	clean, err := cleanFilePath(name)
	if err != nil || strings.Contains(clean, "/") || isReservedPath(clean) {
		return AttachmentInfo{}, ErrAttachmentName
	}
	b, ok := s.backend()
	if !ok {
		return AttachmentInfo{}, ErrUnavailable
	}
	spool, err := spoolBody(body, b.maxFileBytes(ctx))
	if errors.Is(err, errTooLarge) {
		return AttachmentInfo{}, ErrAttachmentTooLarge
	}
	if err != nil {
		return AttachmentInfo{}, fmt.Errorf("artifacts: read attachment: %w", err)
	}
	defer spool.Close()
	if err := s.putBlob(ctx, b, spool, mediaType); err != nil {
		return AttachmentInfo{}, fmt.Errorf("artifacts: store attachment: %w", err)
	}
	now := time.Now().UTC()
	a := &Artifact{
		ID: uuid.NewString(), ScopeKind: ScopeKindUpload, ScopeRef: PrincipalRef(kind, ref),
		OwnerKind: kind, OwnerRef: ref, Title: clean, CurrentSeq: 1, CreatedAt: now, UpdatedAt: now,
	}
	v := &Version{
		ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindAttachment, EntryPath: clean,
		TotalBytes: spool.size, FileCount: 1, CreatedByKind: kind, CreatedByRef: ref,
		CreatedAt: now, State: VersionStateReady,
	}
	f := File{VersionID: v.ID, Path: clean, Size: spool.size, SHA256: spool.digest, MediaType: mediaType}
	// A failure here can leave an unreferenced blob behind; the blob sweep
	// reclaims it.
	if err := b.store.CreatePublished(ctx, a, v, []File{f}, nil); err != nil {
		return AttachmentInfo{}, fmt.Errorf("artifacts: record attachment: %w", err)
	}
	return AttachmentInfo{ID: a.ID, Name: clean, MediaType: mediaType, Size: spool.size}, nil
}

// UsableAttachment reports whether id names an unsent upload of the sender
// (senderKind, senderRef), for send validation before the message is
// stored, or an attachment the sender already sent (AttachmentSentBySender).
// It changes nothing; BindAttachment decides under the lock.
func (s *Service) UsableAttachment(ctx context.Context, id, senderKind, senderRef string) (AttachmentInfo, AttachmentState, error) {
	b, ok := s.backend()
	if !ok || !canonicalID(id) {
		return AttachmentInfo{}, AttachmentAbsent, nil
	}
	a, err := b.store.GetAttachment(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return AttachmentInfo{}, AttachmentAbsent, nil
	}
	if err != nil {
		return AttachmentInfo{}, AttachmentAbsent, err
	}
	owned := a.OwnerKind == senderKind && a.OwnerRef == senderRef
	if a.ScopeKind == ScopeKindMessage && owned {
		return AttachmentInfo{}, AttachmentSentBySender, nil
	}
	if a.ScopeKind != ScopeKindUpload || !owned || a.ScopeRef != PrincipalRef(senderKind, senderRef) {
		return AttachmentInfo{}, AttachmentUnusable, nil
	}
	f, err := attachmentFile(ctx, b, a)
	if errors.Is(err, ErrNotFound) {
		return AttachmentInfo{}, AttachmentUnusable, nil
	}
	if err != nil {
		return AttachmentInfo{}, AttachmentAbsent, err
	}
	return AttachmentInfo{ID: a.ID, Name: f.Path, MediaType: f.MediaType, Size: f.Size}, AttachmentUsable, nil
}

// BindAttachment moves the upload artifactID of the sender (senderKind,
// senderRef) to the message scope of messageID, a message that is already
// stored, and records messageProjectID, the project the host derived from
// the stored message ("" for none), on the row in the same write. It
// returns ErrAttachmentNotOwned for an upload of someone else,
// ErrAttachmentBound for one already sent, and ErrNotFound for an unknown
// id.
func (s *Service) BindAttachment(ctx context.Context, artifactID, senderKind, senderRef, messageID, messageProjectID string) error {
	b, ok := s.backend()
	if !ok {
		return ErrUnavailable
	}
	if !canonicalID(artifactID) {
		return ErrNotFound
	}
	return b.store.BindAttachment(ctx, artifactID, senderKind, senderRef, messageID, messageProjectID, time.Now())
}

// ServeAttachment answers a download of attachment id (an artifact id or
// a legacy attachment id the artifact stands for) for the caller of r. A
// readable attachment is delivered through the code path of every artifact
// file read (serveFile): the same safety headers, inline only for safe
// media types, a redirect to a short-lived signed URL on an object store,
// and streamed by the hub with ?stream=1. On ServeNotFound and
// ServeRefused nothing is written, so the caller writes its own single
// not-found answer for both.
func (s *Service) ServeAttachment(w http.ResponseWriter, r *http.Request, id string) ServeOutcome {
	ctx := r.Context()
	b, ok := s.backend()
	if !ok || !canonicalID(id) {
		return ServeNotFound
	}
	a, err := b.store.GetAttachment(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return ServeNotFound
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: get attachment failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the attachment")
		return ServeWritten
	}
	readable, err := s.canReadErr(ctx, b, a)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: attachment read check failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the attachment")
		return ServeWritten
	}
	if !readable {
		return ServeRefused
	}
	f, err := attachmentFile(ctx, b, a)
	if errors.Is(err, ErrNotFound) {
		return ServeRefused
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: read attachment file failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the attachment")
		return ServeWritten
	}
	serveFile(w, r, b, f, deliveryFor(r, b), false)
	return ServeWritten
}

// attachmentFile returns the one file of attachment a's ready current
// version, or ErrNotFound.
func attachmentFile(ctx context.Context, b backend, a *Artifact) (*File, error) {
	if a.CurrentSeq == 0 {
		return nil, ErrNotFound
	}
	v, err := b.store.GetVersion(ctx, a.ID, a.CurrentSeq)
	if err != nil {
		return nil, err
	}
	if v.State != VersionStateReady {
		return nil, ErrNotFound
	}
	f, err := b.store.GetFile(ctx, v.ID, v.EntryPath)
	if err != nil {
		return nil, err
	}
	if f.SHA256 == "" || f.Pending {
		return nil, ErrNotFound
	}
	return f, nil
}

// MessageAttachments returns the attachments bound to each of messageIDs,
// keyed by message id. It decides nothing about access: the caller has
// already established that the requester reads those messages.
func (s *Service) MessageAttachments(ctx context.Context, messageIDs []string) (map[string][]AttachmentInfo, error) {
	out := map[string][]AttachmentInfo{}
	b, ok := s.backend()
	if !ok || len(messageIDs) == 0 {
		return out, nil
	}
	rows, err := b.store.ListMessageAttachments(ctx, messageIDs)
	if err != nil {
		return nil, err
	}
	for msgID, list := range rows {
		for _, row := range list {
			if row.File.SHA256 == "" || row.File.Pending {
				continue
			}
			out[msgID] = append(out[msgID], AttachmentInfo{
				ID: row.Artifact.ID, Name: row.File.Path, MediaType: row.File.MediaType, Size: row.File.Size,
				MessageProjectID: row.Artifact.MessageProjectID,
			})
		}
	}
	return out, nil
}

// MayReadAttachments reports whether the caller of ctx may read the
// attachments bound to message messageID, for the paths that list them
// (history, the send echo, delivery planning; ptone/scion#4596). It asks
// the same two host questions as the read rule of a message attachment
// (canReadAttachmentScope), in the same order, without a row:
// MessageScopeHost.PermitsMessageRead, then MayReadMessage.
// messageProjectID is the project the message's attachment rows record
// (all rows of one message are bound by one send, so any row's value
// serves). A host without MessageScopeHost answers false.
func (s *Service) MayReadAttachments(ctx context.Context, messageID string, messageProjectID *string) bool {
	mh, ok := s.host.(MessageScopeHost)
	if !ok || messageID == "" {
		return false
	}
	if !mh.PermitsMessageRead(ctx) {
		return false
	}
	return mh.MayReadMessage(ctx, messageID, messageProjectID)
}

// OpenBoundAttachment opens the bytes of attachment artifactID, which must
// be bound to message messageID, for the host to deliver to the message's
// addressees. It checks no caller: the host calls it only from its own
// send path, after the send was authorized and the message stored, never
// on behalf of a request that names an attachment.
func (s *Service) OpenBoundAttachment(ctx context.Context, artifactID, messageID string) (io.ReadCloser, AttachmentInfo, error) {
	b, ok := s.backend()
	if !ok {
		return nil, AttachmentInfo{}, ErrUnavailable
	}
	if !canonicalID(artifactID) || messageID == "" {
		return nil, AttachmentInfo{}, ErrNotFound
	}
	a, err := b.store.GetAttachment(ctx, artifactID)
	if err != nil {
		return nil, AttachmentInfo{}, err
	}
	if a.ID != artifactID || a.ScopeKind != ScopeKindMessage || a.ScopeRef != messageID {
		return nil, AttachmentInfo{}, ErrNotFound
	}
	f, err := attachmentFile(ctx, b, a)
	if err != nil {
		return nil, AttachmentInfo{}, err
	}
	rc, _, err := b.blobs.Download(ctx, BlobPath(b.hubID, f.SHA256))
	if err != nil {
		return nil, AttachmentInfo{}, fmt.Errorf("artifacts: open attachment: %w", err)
	}
	return rc, AttachmentInfo{ID: a.ID, Name: f.Path, MediaType: f.MediaType, Size: f.Size}, nil
}

// retireBatch is how many artifacts one retire store call deletes.
const retireBatch = 500

// RetireScope retires every attachment bound to message ref: kind must be
// ScopeKindMessage. The host calls it when it deletes the message; the
// attachments are unreadable from then on, and the blob sweep reclaims
// their bytes after its grace period.
func (s *Service) RetireScope(ctx context.Context, kind, ref string) (int, error) {
	if kind != ScopeKindMessage || ref == "" {
		return 0, fmt.Errorf("artifacts: RetireScope retires message scopes only")
	}
	b, ok := s.backend()
	if !ok {
		return 0, ErrUnavailable
	}
	return retireScope(ctx, b.store, kind, ref)
}

func retireScope(ctx context.Context, st Store, kind, ref string) (int, error) {
	total := 0
	for {
		n, err := st.RetireScope(ctx, kind, ref, &messageGoneActor, retireBatch)
		total += n
		if err != nil || n < retireBatch {
			return total, err
		}
	}
}

// scopePageSize is how many message refs the maintenance pass asks the
// ScopeChecker about at once, and maxScopePages how many such pages one
// pass checks, so a pass costs the same however many messages have
// attachments; the next pass goes on where it stopped. Variables so tests
// can use small pages.
var (
	scopePageSize = 100
	maxScopePages = 10
)

// maxRetireBatches bounds the batches of unsent uploads one pass retires,
// so one pass stays short; the next pass goes on.
const maxRetireBatches = 10

// RetireAttachmentsResult reports one attachment maintenance pass.
type RetireAttachmentsResult struct {
	// Unsent counts uploads retired for being older than UploadTTL.
	Unsent int
	// Orphaned counts message attachments retired because their message
	// is gone.
	Orphaned int
	// Checked counts the message refs asked about.
	Checked int
}

// AttachmentRetirer runs the attachment maintenance pass and keeps, between
// passes, the message ref the last one stopped after. The zero value starts
// from the beginning. It is not safe for concurrent use.
type AttachmentRetirer struct {
	cursor string
}

// Retire runs one attachment maintenance pass at now: it retires uploads
// not sent within UploadTTL, and, when checker is not nil, the attachments
// of messages the checker reports gone (ScopesExist with MessageScopeRef
// refs), checking at most maxScopePages pages of message refs from where
// the last pass stopped and wrapping at the end. The second covers
// messages removed without the delete hook, such as store-level purges. A
// ref the checker does not answer counts as existing. A checker error
// stops the pass, changes nothing more, and the next pass retries the same
// page.
func (r *AttachmentRetirer) Retire(ctx context.Context, st Store, checker ScopeChecker, now time.Time) (RetireAttachmentsResult, error) {
	var res RetireAttachmentsResult
	if st == nil {
		return res, nil
	}
	for range maxRetireBatches {
		n, err := st.RetireStaleUploads(ctx, now.Add(-UploadTTL), retireBatch)
		res.Unsent += n
		if err != nil {
			return res, err
		}
		if n < retireBatch {
			break
		}
	}
	if checker == nil {
		return res, nil
	}
	for range maxScopePages {
		page, err := st.ListLiveScopeRefs(ctx, ScopeKindMessage, r.cursor, scopePageSize)
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			r.cursor = ""
			return res, nil
		}
		refs := make([]string, len(page))
		for i, id := range page {
			refs[i] = MessageScopeRef(id)
		}
		exists, err := checker.ScopesExist(ctx, refs)
		if err != nil {
			return res, err
		}
		res.Checked += len(page)
		for i, id := range page {
			if e, known := exists[refs[i]]; known && !e {
				n, err := retireScope(ctx, st, ScopeKindMessage, id)
				res.Orphaned += n
				if err != nil {
					return res, err
				}
			}
		}
		if len(page) < scopePageSize {
			r.cursor = ""
			return res, nil
		}
		r.cursor = page[len(page)-1]
	}
	return res, nil
}

// CodeAttachmentNotShareable is the error code of the 409 a share, version
// or change of an attachment answers when the store refuses it under the
// artifact's lock (ErrAttachmentNotShareable). The artifact routes never
// look an attachment up, so a request names one only if it became one
// after the route read it.
const CodeAttachmentNotShareable = "attachment_not_shareable"

func writeAttachmentNotShareable(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, CodeAttachmentNotShareable,
		"attachments cannot be shared, versioned or changed")
}
