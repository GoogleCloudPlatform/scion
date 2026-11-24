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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// RuntimeBrokerMoveBundleService is implemented by the RuntimeBrokerService
// returned by Client.RuntimeBrokers(). It is a separate interface so existing
// test mocks of RuntimeBrokerService outside this package are unaffected.
type RuntimeBrokerMoveBundleService interface {
	// MoveBundleUploadURL requests a single-object signed PUT URL for one
	// compressed bundle part (POST
	// /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/move-bundle/{rid}/upload-url).
	// Sent once (no transport retry); caller owns retries. Response bodies and
	// signed URLs never appear in returned errors.
	MoveBundleUploadURL(ctx context.Context, brokerID, agentID, reincarnationID string, req *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error)

	// MoveBundleDownloadURLs requests signed GET URLs for all parts in the
	// hub-persisted bundle manifest (POST
	// /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/move-bundle/{rid}/download-urls).
	// Sent once (no transport retry); caller owns retries. Response bodies and
	// signed URLs never appear in returned errors.
	MoveBundleDownloadURLs(ctx context.Context, brokerID, agentID, reincarnationID string, req *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error)
}

var _ RuntimeBrokerMoveBundleService = (*runtimeBrokerService)(nil)

// maxMoveBundleURLResponseBytes bounds signed-URL endpoint responses.
const maxMoveBundleURLResponseBytes = 8 << 20

// ErrMoveBundleURLDecode is returned for a 200 signed-URL response that does
// not decode. Fixed text: a decode error could quote the body, which holds
// signed URLs.
var ErrMoveBundleURLDecode = errors.New("move bundle signed-URL response did not decode")

// MoveBundleUploadURL implements RuntimeBrokerMoveBundleService.
func (s *runtimeBrokerService) MoveBundleUploadURL(ctx context.Context, brokerID, agentID, reincarnationID string, req *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
	path := "/api/v1/runtime-brokers/" + url.PathEscape(brokerID) +
		"/agents/" + url.PathEscape(agentID) +
		"/move-bundle/" + url.PathEscape(reincarnationID) + "/upload-url"
	resp, err := s.c.postNoRetry(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeMoveBundleURLError(resp, "move bundle upload-url request refused")
	}
	var out api.MoveBundleUploadURLResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMoveBundleURLResponseBytes)).Decode(&out); err != nil {
		return nil, ErrMoveBundleURLDecode
	}
	return &out, nil
}

// MoveBundleDownloadURLs implements RuntimeBrokerMoveBundleService.
func (s *runtimeBrokerService) MoveBundleDownloadURLs(ctx context.Context, brokerID, agentID, reincarnationID string, req *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
	if req == nil {
		req = &api.MoveBundleDownloadURLsRequest{}
	}
	path := "/api/v1/runtime-brokers/" + url.PathEscape(brokerID) +
		"/agents/" + url.PathEscape(agentID) +
		"/move-bundle/" + url.PathEscape(reincarnationID) + "/download-urls"
	resp, err := s.c.postNoRetry(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeMoveBundleURLError(resp, "move bundle download-urls request refused")
	}
	var out api.MoveBundleDownloadURLsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMoveBundleURLResponseBytes)).Decode(&out); err != nil {
		return nil, ErrMoveBundleURLDecode
	}
	return &out, nil
}

// decodeMoveBundleURLError reads only the structured error code from a non-200
// response so no raw response text can reach logs or error strings.
func decodeMoveBundleURLError(resp *http.Response, fixedMsg string) error {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	return &apiclient.APIError{
		StatusCode: resp.StatusCode,
		Code:       body.Error.Code,
		Message:    fixedMsg,
	}
}
