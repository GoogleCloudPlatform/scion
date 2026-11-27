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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
)

const (
	// trajectoryUploadHTTPTimeout bounds a single compressed segment PUT
	// (up to 16 MiB) when the caller's context does not carry a shorter deadline.
	trajectoryUploadHTTPTimeout = 2 * time.Minute

	trajectoryMaxErrorBodyBytes = 64 * 1024
)

// TrajectoryHTTPError represents a non-2xx HTTP response from the Hub
// trajectory segment endpoints (or the signed object PUT target).
type TrajectoryHTTPError struct {
	Operation  string
	StatusCode int
	Code       string
	Message    string
	Body       string
}

func (e *TrajectoryHTTPError) Error() string {
	if e.Code != "" && e.Message != "" {
		return fmt.Sprintf("trajectory %s returned HTTP %d (%s): %s", e.Operation, e.StatusCode, e.Code, e.Message)
	}
	if e.Code != "" {
		return fmt.Sprintf("trajectory %s returned HTTP %d (%s)", e.Operation, e.StatusCode, e.Code)
	}
	return fmt.Sprintf("trajectory %s returned HTTP %d: %s", e.Operation, e.StatusCode, e.Body)
}

func parseTrajectoryHTTPError(op string, statusCode int, respBody []byte) *TrajectoryHTTPError {
	httpErr := &TrajectoryHTTPError{
		Operation:  op,
		StatusCode: statusCode,
		Body:       strings.TrimSpace(string(respBody)),
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if len(respBody) > 0 && json.Unmarshal(respBody, &env) == nil {
		if env.Error.Code != "" {
			httpErr.Code = env.Error.Code
			httpErr.Message = env.Error.Message
		} else if env.Code != "" {
			httpErr.Code = env.Code
			httpErr.Message = env.Message
		}
	}
	return httpErr
}

// PrepareTrajectorySegment calls POST /api/v1/agents/{id}/trajectory/segments:prepare
// using the agent token.
func (c *Client) PrepareTrajectorySegment(
	ctx context.Context,
	payload telemetrycontract.TrajectorySegmentPrepareRequest,
) (*telemetrycontract.TrajectorySegmentPrepareResponse, error) {
	if !c.IsConfigured() {
		return nil, errors.New("hub client not configured")
	}

	endpoint := strings.TrimSuffix(c.hubURL, "/") + telemetrycontract.TrajectorySegmentsPrepareAgentPath(c.agentID)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal trajectory prepare payload: %w", err)
	}

	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create trajectory prepare request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAgentAuth(req.Header, currentToken)

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("send trajectory prepare request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read trajectory prepare response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseTrajectoryHTTPError("prepare", resp.StatusCode, respBody)
	}

	var out telemetrycontract.TrajectorySegmentPrepareResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse trajectory prepare response: %w", err)
	}
	return &out, nil
}

// UploadTrajectorySegment PUTs compressed segment bytes to the URL returned by
// segments:prepare, sending every header in inst.Headers verbatim.
//
// When inst.URL targets this Hub's proxied content endpoint (same origin as
// hubURL and under /api/v1/agents/), agent-token authentication is attached and
// the request goes through c.do. External signed URLs (such as GCS V4 signed
// PUTs) are sent without Hub credentials or transport OIDC headers so agent
// credentials are never leaked off-origin.
//
// HTTP 412 Precondition Failed is treated as success (object already exists
// from a prior attempt whose ACK was lost; caller proceeds to commit).
func (c *Client) UploadTrajectorySegment(
	ctx context.Context,
	inst telemetrycontract.TrajectorySegmentUploadInstruction,
	compressed []byte,
) (int, error) {
	if !c.IsConfigured() {
		return 0, errors.New("hub client not configured")
	}
	if strings.TrimSpace(inst.URL) == "" {
		return 0, errors.New("trajectory upload URL is empty")
	}
	method := strings.TrimSpace(inst.Method)
	if method == "" {
		method = http.MethodPut
	}

	req, err := http.NewRequestWithContext(ctx, method, inst.URL, bytes.NewReader(compressed))
	if err != nil {
		return 0, fmt.Errorf("create trajectory upload request: %w", err)
	}
	req.ContentLength = int64(len(compressed))
	for k, v := range inst.Headers {
		req.Header.Set(k, v)
	}

	var resp *http.Response
	if c.isHubProxiedUploadURL(inst.URL) {
		c.tokenMu.RLock()
		currentToken := c.token
		c.tokenMu.RUnlock()
		c.setAgentAuth(req.Header, currentToken)
		resp, err = c.do(req)
	} else {
		// External signed URL (e.g. GCS): use a clean client with no Hub
		// agent token, run header, or transport OIDC credentials, and refuse
		// redirects so signed PUTs cannot be bounced elsewhere.
		extClient := &http.Client{
			Timeout: trajectoryUploadHTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err = extClient.Do(req)
	}
	if err != nil {
		return 0, fmt.Errorf("send trajectory upload request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, trajectoryMaxErrorBodyBytes))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read trajectory upload response: %w", err)
	}

	// 2xx or 412 Precondition Failed (no-overwrite object already exists):
	// proceed to commit and let the Hub verify integrity.
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusPreconditionFailed {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, parseTrajectoryHTTPError("upload", resp.StatusCode, respBody)
}

func (c *Client) isHubProxiedUploadURL(rawURL string) bool {
	hubU, err := url.Parse(c.hubURL)
	if err != nil || hubU.Scheme == "" || hubU.Host == "" {
		return false
	}
	targetU, err := url.Parse(rawURL)
	if err != nil || targetU.Scheme == "" || targetU.Host == "" {
		return false
	}
	return strings.EqualFold(hubU.Scheme, targetU.Scheme) &&
		strings.EqualFold(hubU.Host, targetU.Host) &&
		strings.HasPrefix(targetU.Path, "/api/v1/agents/")
}

// CommitTrajectorySegment calls POST /api/v1/agents/{id}/trajectory/segments:commit
// using the agent token.
func (c *Client) CommitTrajectorySegment(
	ctx context.Context,
	segmentID string,
) (*telemetrycontract.TrajectorySegmentCommitResponse, error) {
	if !c.IsConfigured() {
		return nil, errors.New("hub client not configured")
	}
	if strings.TrimSpace(segmentID) == "" {
		return nil, errors.New("segment_id is required")
	}

	endpoint := strings.TrimSuffix(c.hubURL, "/") + telemetrycontract.TrajectorySegmentsCommitAgentPath(c.agentID)
	body, err := json.Marshal(telemetrycontract.TrajectorySegmentCommitRequest{
		SegmentID: segmentID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal trajectory commit payload: %w", err)
	}

	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create trajectory commit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAgentAuth(req.Header, currentToken)

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("send trajectory commit request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read trajectory commit response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseTrajectoryHTTPError("commit", resp.StatusCode, respBody)
	}

	var out telemetrycontract.TrajectorySegmentCommitResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse trajectory commit response: %w", err)
	}
	return &out, nil
}

// TrajectoryUploader adapts a sciontool Hub Client to shipper.Uploader.
type TrajectoryUploader struct {
	newClient func() *Client
}

// NewTrajectoryUploader returns a shipper.Uploader backed by client.
func NewTrajectoryUploader(client *Client) *TrajectoryUploader {
	return &TrajectoryUploader{
		newClient: func() *Client { return client },
	}
}

// NewTrajectoryUploaderWithProvider returns a shipper.Uploader that obtains a
// fresh Hub client (picking up refreshed agent tokens from disk) on each Ship
// call.
func NewTrajectoryUploaderWithProvider(newClient func() *Client) *TrajectoryUploader {
	return &TrajectoryUploader{newClient: newClient}
}

// Ship delivers a prepared segment to the Hub (prepare -> PUT -> commit) and
// maps every Hub response onto the shipper.ShipOutcome contract.
func (u *TrajectoryUploader) Ship(ctx context.Context, seg shipper.Segment) (shipper.ShipResult, error) {
	if u == nil || u.newClient == nil {
		return shipper.ShipResult{Outcome: shipper.OutcomeRetryable, Message: "hub client provider not configured"},
			errors.New("trajectory uploader: hub client provider not configured")
	}
	client := u.newClient()
	if client == nil || !client.IsConfigured() {
		return shipper.ShipResult{Outcome: shipper.OutcomeRetryable, Message: "hub client not configured"},
			errors.New("trajectory uploader: hub client not configured")
	}

	prepReq := telemetrycontract.TrajectorySegmentPrepareRequest{
		Harness:            seg.Harness,
		NativeRelativePath: seg.NativeRelativePath,
		ArtifactKind:       string(seg.ArtifactKind),
		Generation:         seg.Generation,
		ByteStart:          seg.ByteStart,
		ByteEnd:            seg.ByteEnd,
		RawSHA256:          seg.RawSHA256,
		CompressedSize:     seg.CompressedSize,
		CompressedSHA256:   seg.CompressedSHA256,
		ContentEncoding:    seg.ContentEncoding,
		IncompleteTail:     seg.IncompleteTail,
		SessionID:          seg.SessionID,
		OriginHubID:        seg.OriginHubID,
	}

	prepResp, err := client.PrepareTrajectorySegment(ctx, prepReq)
	if err != nil {
		return mapTrajectoryShipError(err)
	}

	if prepResp.State == telemetrycontract.TrajectorySegmentStateCommitted {
		return shipper.ShipResult{
			Outcome:   shipper.OutcomeCommitted,
			SegmentID: prepResp.SegmentID,
		}, nil
	}

	if prepResp.Upload == nil || strings.TrimSpace(prepResp.Upload.URL) == "" {
		err := errors.New("trajectory prepare returned upload_required without upload instruction")
		return shipper.ShipResult{
			Outcome: shipper.OutcomeRetryable,
			Message: err.Error(),
		}, err
	}

	if _, err := client.UploadTrajectorySegment(ctx, *prepResp.Upload, seg.Compressed); err != nil {
		return mapTrajectoryShipError(err)
	}

	commitResp, err := client.CommitTrajectorySegment(ctx, prepResp.SegmentID)
	if err != nil {
		return mapTrajectoryShipError(err)
	}
	if commitResp.State != "" && commitResp.State != telemetrycontract.TrajectorySegmentStateCommitted {
		err := fmt.Errorf("trajectory commit returned unexpected state %q", commitResp.State)
		return shipper.ShipResult{
			Outcome: shipper.OutcomeRetryable,
			Message: err.Error(),
		}, err
	}

	return shipper.ShipResult{
		Outcome:   shipper.OutcomeCommitted,
		SegmentID: commitResp.SegmentID,
	}, nil
}

func mapTrajectoryShipError(err error) (shipper.ShipResult, error) {
	var httpErr *TrajectoryHTTPError
	if !errors.As(err, &httpErr) {
		return shipper.ShipResult{
			Outcome: shipper.OutcomeRetryable,
			Message: err.Error(),
		}, err
	}

	msg := httpErr.Code
	if msg == "" {
		msg = httpErr.Message
	}
	if msg == "" {
		msg = httpErr.Error()
	}

	switch httpErr.StatusCode {
	case http.StatusNotFound:
		return shipper.ShipResult{
			Outcome: shipper.OutcomeCaptureUnavailable,
			Message: msg,
		}, nil

	case http.StatusConflict:
		return shipper.ShipResult{
			Outcome: shipper.OutcomeConflict,
			Message: msg,
		}, err

	case http.StatusBadRequest:
		return shipper.ShipResult{
			Outcome: shipper.OutcomeRejected,
			Message: msg,
		}, err

	case http.StatusRequestEntityTooLarge:
		if httpErr.Code == telemetrycontract.TrajectoryErrRunCapExceeded {
			return shipper.ShipResult{
				Outcome: shipper.OutcomeRunCapExceeded,
				Message: msg,
			}, nil
		}
		return shipper.ShipResult{
			Outcome: shipper.OutcomeRejected,
			Message: msg,
		}, err

	case http.StatusForbidden:
		if httpErr.Code == telemetrycontract.TrajectoryErrCaptureDisabled ||
			httpErr.Code == telemetrycontract.TrajectoryErrCaptureMetadataOnly {
			return shipper.ShipResult{
				Outcome: shipper.OutcomePolicyRefused,
				Message: httpErr.Code,
			}, nil
		}
		return shipper.ShipResult{
			Outcome: shipper.OutcomeRetryable,
			Message: msg,
		}, err

	case http.StatusGone, http.StatusUnprocessableEntity:
		return shipper.ShipResult{
			Outcome: shipper.OutcomeRetryable,
			Message: msg,
		}, err

	default:
		return shipper.ShipResult{
			Outcome: shipper.OutcomeRetryable,
			Message: msg,
		}, err
	}
}
