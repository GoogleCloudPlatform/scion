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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const uploadObjectStagingRoot = "uploads-staging"

var (
	// errUploadDisallowedMediaType is returned when ClassifyAttachment rejects
	// a chat attachment at offset 0 or finalize.
	errUploadDisallowedMediaType = errors.New("disallowed attachment media type")
)

// uploadObjectStagingPrefix returns the top-level staging prefix for an
// ObjectSink session (ptone/scion#8654 §3.6). Top-level "uploads-staging/"
// matches the bucket lifecycle rule (§3.13).
func uploadObjectStagingPrefix(hubID, sid string) string {
	return uploadObjectStagingRoot + "/" + hubID + "/" + sid
}

// uploadObjectPartPath returns the token-fenced part object path for chunk
// index within stagingPrefix (§3.6):
//
//	uploads-staging/{hub_id}/{sid}/p{index:05d}-{token}
//
// Because part names include the lease token, a stale writer whose lease was
// taken over can only write an orphan object and can never overwrite a
// committed part listed in UploadSession.Parts.
func uploadObjectPartPath(stagingPrefix string, index int, token string) string {
	return fmt.Sprintf("%s/p%05d-%s", strings.TrimRight(stagingPrefix, "/"), index, token)
}

// objectUploadSink implements uploadSink for chat_attachment uploads using the
// hub's blob store (ptone/scion#8654 §3.6).
type objectUploadSink struct {
	blobs storage.Storage
	svc   *artifacts.Service
}

var _ uploadSink = (*objectUploadSink)(nil)

func newObjectUploadSink(blobs storage.Storage, svc *artifacts.Service) *objectUploadSink {
	return &objectUploadSink{blobs: blobs, svc: svc}
}

// WriteChunk streams exactly n bytes from r into a token-named staging object
// via blobs.Upload(..., Idempotent: true).
func (s *objectUploadSink) WriteChunk(ctx context.Context, sess *store.UploadSession, _ int64, token string, r io.Reader, n int64) (string, error) {
	if s.blobs == nil {
		return "", artifacts.ErrUnavailable
	}
	partPath := uploadObjectPartPath(sess.StagingRef, len(sess.Parts), token)
	er := &exactLengthReader{ctx: ctx, r: r, want: n}
	obj, err := s.blobs.Upload(ctx, partPath, er, storage.UploadOptions{
		ContentType: "application/octet-stream",
		Idempotent:  true,
	})
	if err != nil {
		return "", err
	}
	if er.total < n {
		return "", errUploadShortBody
	}
	if er.total > n {
		return "", errUploadChunkTooLarge
	}
	if obj != nil && obj.Size != n {
		return "", errUploadShortBody
	}
	return partPath, nil
}

// Finalize classifies the attachment from its captured head bytes, promotes
// the staged parts into the content-addressed artifact store via
// CreateAttachmentFromStaged using the pre-allocated ArtifactID, and returns
// the JSON-encoded attachmentUploadResult matching POST /api/v1/chat/attachments.
func (s *objectUploadSink) Finalize(ctx context.Context, sess *store.UploadSession, digestHex string) (json.RawMessage, error) {
	if s.svc == nil {
		return nil, artifacts.ErrUnavailable
	}
	mediaType, err := ClassifyAttachment(sess.FileName, sess.Head)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errUploadDisallowedMediaType, err.Error())
	}

	info, err := s.svc.CreateAttachmentFromStaged(ctx, artifacts.StagedAttachment{
		ArtifactID:    sess.ArtifactID,
		Name:          sess.FileName,
		MediaType:     mediaType,
		Size:          sess.DeclaredSize,
		SHA256:        digestHex,
		Parts:         sess.Parts,
		StagingPrefix: sess.StagingRef,
	})
	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(attachmentUploadResult{
		ID:       info.ID,
		Name:     info.Name,
		MimeType: info.MediaType,
		Size:     info.Size,
		URL:      "/api/v1/chat/attachments/" + info.ID,
	})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// Cleanup deletes all staged part objects under sess.StagingRef. Idempotent.
func (s *objectUploadSink) Cleanup(ctx context.Context, sess *store.UploadSession) error {
	if s.blobs == nil || sess.StagingRef == "" {
		return nil
	}
	prefix := storage.DirPrefix(sess.StagingRef)
	if prefix == "" {
		return nil
	}
	if err := s.blobs.DeletePrefix(ctx, prefix); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return nil
}

// exactLengthReader wraps r and enforces that it yields exactly want bytes
// while honoring ctx cancellation.
type exactLengthReader struct {
	ctx   context.Context
	r     io.Reader
	want  int64
	total int64
}

func (e *exactLengthReader) Read(p []byte) (int, error) {
	if err := e.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := e.r.Read(p)
	if n > 0 {
		e.total += int64(n)
		if e.total > e.want {
			return 0, errUploadChunkTooLarge
		}
	}
	if err == io.EOF && e.total < e.want {
		return n, errUploadShortBody
	}
	return n, err
}
