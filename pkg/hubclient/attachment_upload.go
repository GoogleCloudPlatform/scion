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

package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// chatAttachmentsPath is the attachment upload collection and its
// capability probe.
const chatAttachmentsPath = "/api/v1/chat/attachments"

// AttachmentUploads values of AttachmentProbe.Uploads.
const (
	// AttachmentUploadsArtifact: uploads are stored as artifacts and sent
	// by id (attachment_ids).
	AttachmentUploadsArtifact = "artifact"
	// AttachmentUploadsLegacy: today's path (staged files).
	AttachmentUploadsLegacy = "legacy"
)

// AttachmentProbe is the hub's answer to the attachment capability probe
// (GET /api/v1/chat/attachments) for the calling credential
// (ptone/scion#4596).
type AttachmentProbe struct {
	// Uploads is AttachmentUploadsArtifact or AttachmentUploadsLegacy.
	Uploads string `json:"uploads"`
	// DeliveryMaxBytes, with AttachmentUploadsArtifact, is the total size
	// of the attachments of one message with an agent recipient; 0 means
	// the hub delivers no attachment to agents.
	DeliveryMaxBytes *int64 `json:"delivery_max_bytes,omitempty"`
}

// UploadedAttachment is one stored upload.
type UploadedAttachment struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime"`
	Size     int64  `json:"size"`
}

// AttachmentUploadFailure is one file the hub did not store.
type AttachmentUploadFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// AttachmentUploadResult is the hub's answer to an upload.
type AttachmentUploadResult struct {
	Attachments []UploadedAttachment      `json:"attachments"`
	Failures    []AttachmentUploadFailure `json:"failures"`
}

// AttachmentUploader probes and uploads message attachments stored as
// artifacts (ptone/scion#4596). The hub client implements it; callers
// type-assert a Client to it.
type AttachmentUploader interface {
	// ProbeAttachmentUploads asks where the calling credential's uploads
	// go. It uploads nothing. A hub from before the probe answers an
	// error with status 403, 404 or 405.
	ProbeAttachmentUploads(ctx context.Context) (*AttachmentProbe, error)
	// UploadAttachments uploads the files at paths in one request. The
	// result lists the stored uploads and the per-file failures; a request
	// in which no file was stored still returns the failures when the hub
	// reports them.
	UploadAttachments(ctx context.Context, paths []string) (*AttachmentUploadResult, error)
}

// ProbeAttachmentUploads implements AttachmentUploader.
func (c *client) ProbeAttachmentUploads(ctx context.Context) (*AttachmentProbe, error) {
	resp, err := c.get(ctx, chatAttachmentsPath, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[AttachmentProbe](resp)
}

// UploadAttachments implements AttachmentUploader. The multipart body is
// streamed from the files, never held in memory whole. The request is not
// retried: an upload is not idempotent, and a streamed body cannot be
// replayed.
func (c *client) UploadAttachments(ctx context.Context, paths []string) (*AttachmentUploadResult, error) {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", p, err)
		}
		_ = f.Close()
	}
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		for _, p := range paths {
			if err := appendAttachmentPart(writer, p); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		if err := writer.Close(); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("finalize attachment upload: %w", err))
			return
		}
		_ = pw.Close()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.transport.BaseURL+chatAttachmentsPath, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.transport.DoNoRetry(ctx, req)
	// Unblock the writer if the request stopped reading the body.
	_ = pr.CloseWithError(io.ErrClosedPipe)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusCreated {
		return apiclient.DecodeRequired[AttachmentUploadResult](resp)
	}
	// A refusal of every file carries the per-file failures.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read attachment upload response (status %d): %w", resp.StatusCode, readErr)
	}
	var result AttachmentUploadResult
	if json.Unmarshal(raw, &result) == nil && len(result.Failures) > 0 {
		return &result, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err := apiclient.CheckResponse(resp); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("unexpected attachment upload response status: %d", resp.StatusCode)
}

// appendAttachmentPart adds the file at path to writer as a "files" part.
func appendAttachmentPart(writer *multipart.Writer, path string) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("attachment %q: %w", path, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	part, err := writer.CreateFormFile("files", filepath.Base(path))
	if err != nil {
		return fmt.Errorf("attachment %q: %w", path, err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("attachment %q: %w", path, err)
	}
	return nil
}
