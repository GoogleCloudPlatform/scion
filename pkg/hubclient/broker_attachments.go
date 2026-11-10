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
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Broker attachment routes: a chat plugin that authenticates with broker
// HMAC checks whether a sender may send attachments, uploads them as the
// sender's unsent uploads, and names the returned ids in attachment_ids on
// POST /api/v1/broker/inbound.
//
// Signing: these calls are authenticated by the client's authenticator,
// like every other call. A plugin builds its client with WithHMACAuth (its
// broker id and secret), or WithAuthenticator with its own
// apiclient.Authenticator that signs as the broker does. HMAC signing
// hashes the whole request body, so an upload body is built in memory and
// bounded at apiclient.MaxSignedRequestBodyBytes, the hub's limit for a
// signed body. WithPluginName sets the plugin name the hub records in its
// audit line.
const (
	brokerAttachmentsCheckPath  = "/api/v1/broker/inbound/attachments/check"
	brokerAttachmentsUploadPath = "/api/v1/broker/inbound/attachments"
)

// AttachmentUploadsUnavailable is the BrokerAttachmentCheck.Uploads value
// for a hub that delivers no attachments: the plugin sends the text only.
const AttachmentUploadsUnavailable = "unavailable"

// ErrBrokerAttachmentsUnsupported reports a hub without the broker
// attachment routes: the check answered 404 with a body that is not the
// hub's JSON error envelope. Any other failure, a 404 carrying the
// envelope included, is an ordinary error and must not be read as an old
// hub.
var ErrBrokerAttachmentsUnsupported = errors.New("hub has no broker attachment routes")

// NamedReader is one attachment to upload: its file name and its content.
type NamedReader struct {
	Name   string
	Reader io.Reader
}

// BrokerAttachmentCheck is the hub's answer to a broker attachment check.
type BrokerAttachmentCheck struct {
	// Authorized reports whether the sender may send attachments to the
	// project and targets named in the check.
	Authorized bool `json:"authorized"`
	// Uploads is AttachmentUploadsArtifact, AttachmentUploadsLegacy or
	// AttachmentUploadsUnavailable.
	Uploads string `json:"uploads"`
	// DeliveryMaxBytes, with AttachmentUploadsArtifact, is the largest file
	// one upload may carry: the total for one message, or the hub's
	// per-file limit when that is smaller.
	DeliveryMaxBytes *int64 `json:"delivery_max_bytes,omitempty"`
}

// BrokerAttachmentClient checks and uploads attachments on behalf of a chat
// sender. The hub client implements it; callers type-assert a Client to it.
type BrokerAttachmentClient interface {
	// CheckBrokerAttachments asks whether sender ("user:<email>") may send
	// attachments to projectID and, when targets are named, to each of
	// those agents, and where attachments go. It writes nothing. A hub
	// without the route answers ErrBrokerAttachmentsUnsupported.
	CheckBrokerAttachments(ctx context.Context, sender, projectID string, targets []string, surface string) (*BrokerAttachmentCheck, error)
	// UploadBrokerAttachments uploads files as unsent uploads of sender.
	// audience is "direct" or "group". The result lists the stored uploads
	// and the per-file failures.
	UploadBrokerAttachments(ctx context.Context, sender, projectID string, targets []string, surface, audience string, files []NamedReader) (*AttachmentUploadResult, error)
}

// AttachmentReaderUploader uploads attachments from readers rather than
// paths. The hub client implements it beside AttachmentUploader.
type AttachmentReaderUploader interface {
	// UploadAttachmentReaders uploads files in one request, streamed from
	// their readers, as UploadAttachments does for paths.
	UploadAttachmentReaders(ctx context.Context, files []NamedReader) (*AttachmentUploadResult, error)
}

// WithPluginName sets the plugin name sent as X-Scion-Plugin-Name on the
// broker attachment requests.
func WithPluginName(name string) Option {
	return func(c *client) {
		c.pluginName = name
	}
}

func (c *client) pluginHeaders() http.Header {
	if c.pluginName == "" {
		return nil
	}
	h := http.Header{}
	h.Set("X-Scion-Plugin-Name", c.pluginName)
	return h
}

// CheckBrokerAttachments implements BrokerAttachmentClient.
func (c *client) CheckBrokerAttachments(ctx context.Context, sender, projectID string, targets []string, surface string) (*BrokerAttachmentCheck, error) {
	body := struct {
		Sender    string   `json:"sender"`
		ProjectID string   `json:"project_id"`
		Targets   []string `json:"targets,omitempty"`
		Surface   string   `json:"surface"`
	}{sender, projectID, targets, surface}
	resp, err := c.post(ctx, brokerAttachmentsCheckPath, body, c.pluginHeaders())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr == nil && !isHubErrorEnvelope(raw) {
			return nil, ErrBrokerAttachmentsUnsupported
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
	}
	return apiclient.DecodeRequired[BrokerAttachmentCheck](resp)
}

// isHubErrorEnvelope reports whether body is the hub's JSON error
// envelope, {"error":{"code":...}} with a non-empty code.
func isHubErrorEnvelope(body []byte) bool {
	var env struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &env) == nil && env.Error != nil && env.Error.Code != ""
}

// errSignedBodyTooLarge is returned before sending an upload whose body
// would exceed the hub's signed-body limit.
var errSignedBodyTooLarge = fmt.Errorf("broker attachment upload exceeds %d bytes", apiclient.MaxSignedRequestBodyBytes)

// UploadBrokerAttachments implements BrokerAttachmentClient. The metadata
// fields go first, in the order the hub requires (sender, project_id,
// surface, audience, then one targets field per slug), followed by one
// "file" part per file. The body is built in memory because the request is
// signed over it; it is refused before sending when it would exceed
// apiclient.MaxSignedRequestBodyBytes. The request is not retried.
func (c *client) UploadBrokerAttachments(ctx context.Context, sender, projectID string, targets []string, surface, audience string, files []NamedReader) (*AttachmentUploadResult, error) {
	if len(files) == 0 {
		return nil, errors.New("no attachments to upload")
	}
	var buf bytes.Buffer
	limited := &limitedWriter{w: &buf, remaining: apiclient.MaxSignedRequestBodyBytes}
	writer := multipart.NewWriter(limited)
	fields := [][2]string{{"sender", sender}, {"project_id", projectID}, {"surface", surface}, {"audience", audience}}
	for _, t := range targets {
		fields = append(fields, [2]string{"targets", t})
	}
	for _, f := range fields {
		if err := writer.WriteField(f[0], f[1]); err != nil {
			return nil, uploadBuildError(err)
		}
	}
	for _, f := range files {
		if err := appendReaderPart(writer, "file", f); err != nil {
			return nil, uploadBuildError(err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, uploadBuildError(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.transport.BaseURL+brokerAttachmentsUploadPath, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if c.pluginName != "" {
		req.Header.Set("X-Scion-Plugin-Name", c.pluginName)
	}
	resp, err := c.transport.DoNoRetry(ctx, req)
	if err != nil {
		return nil, err
	}
	return decodeAttachmentUploadResponse(resp, true)
}

func uploadBuildError(err error) error {
	if errors.Is(err, errSignedBodyTooLarge) {
		return err
	}
	return fmt.Errorf("build broker attachment upload: %w", err)
}

// limitedWriter writes to w and fails with errSignedBodyTooLarge once more
// than remaining bytes were written.
type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.remaining {
		return 0, errSignedBodyTooLarge
	}
	l.remaining -= int64(len(p))
	return l.w.Write(p)
}

// UploadAttachmentReaders implements AttachmentReaderUploader. The
// multipart body is streamed from the readers, never held in memory whole,
// with the "files" parts of UploadAttachments. The request is not retried.
func (c *client) UploadAttachmentReaders(ctx context.Context, files []NamedReader) (*AttachmentUploadResult, error) {
	if len(files) == 0 {
		return nil, errors.New("no attachments to upload")
	}
	for _, f := range files {
		if f.Name == "" || f.Reader == nil {
			return nil, errors.New("attachment needs a name and a reader")
		}
	}
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		for _, f := range files {
			if err := appendReaderPart(writer, "files", f); err != nil {
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
	return decodeAttachmentUploadResponse(resp, false)
}

// appendReaderPart adds f to writer as a file part named field.
func appendReaderPart(writer *multipart.Writer, field string, f NamedReader) error {
	if f.Name == "" || f.Reader == nil {
		return errors.New("attachment needs a name and a reader")
	}
	part, err := writer.CreateFormFile(field, f.Name)
	if err != nil {
		return fmt.Errorf("attachment %q: %w", f.Name, err)
	}
	if _, err := io.Copy(part, f.Reader); err != nil {
		return fmt.Errorf("attachment %q: %w", f.Name, err)
	}
	return nil
}

// decodeAttachmentUploadResponse reads the answer to an attachment upload:
// the result on 201 (or 200 when okToo); on a refusal of every file, the
// per-file failures when the hub reports them; otherwise the hub's error.
func decodeAttachmentUploadResponse(resp *http.Response, okToo bool) (*AttachmentUploadResult, error) {
	if resp.StatusCode == http.StatusCreated || (okToo && resp.StatusCode == http.StatusOK) {
		return apiclient.DecodeRequired[AttachmentUploadResult](resp)
	}
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

var (
	_ BrokerAttachmentClient   = (*client)(nil)
	_ AttachmentReaderUploader = (*client)(nil)
)
