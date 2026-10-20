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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// AttachmentDeliveryFetcher redeems attachment delivery capabilities
// (ptone/scion#4596). The broker's hub client implements it.
type AttachmentDeliveryFetcher interface {
	// FetchAttachmentDelivery redeems the capability at path (a path on
	// the hub, /api/v1/attachment-deliveries/<token>) with the client's own
	// credential and returns the file's bytes. An object-store redirect is
	// followed with a plain client that carries no hub credential.
	FetchAttachmentDelivery(ctx context.Context, path string) (io.ReadCloser, error)
}

// attachmentDeliveriesPath is the redemption route's prefix.
const attachmentDeliveriesPath = "/api/v1/attachment-deliveries/"

// errDeliveryRefused is returned for any refusal of a delivery capability.
var errDeliveryRefused = errors.New("attachment delivery refused")

// FetchAttachmentDelivery implements AttachmentDeliveryFetcher.
func (c *client) FetchAttachmentDelivery(ctx context.Context, path string) (io.ReadCloser, error) {
	if !strings.HasPrefix(path, attachmentDeliveriesPath) || strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("attachment delivery: unexpected capability path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.transport.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	// No redirect is followed with the hub credential attached.
	resp, err := c.transport.DoNoRetry(ctx, req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Body, nil
	case resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusTemporaryRedirect:
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		return fetchRedirect(ctx, loc, strings.HasPrefix(c.transport.BaseURL, "http://"))
	default:
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusNotFound {
			return nil, errDeliveryRefused
		}
		return nil, apiclient.CheckResponse(resp)
	}
}

// fetchRedirect downloads loc, an object-store URL, with a client that
// carries no credential and follows no further redirect. Only https is
// followed, unless the hub itself is reached over plain http (allowHTTP,
// a local development hub).
func fetchRedirect(ctx context.Context, loc string, allowHTTP bool) (io.ReadCloser, error) {
	u, err := url.Parse(loc)
	if err != nil || (u.Scheme != "https" && (u.Scheme != "http" || !allowHTTP)) || u.Host == "" {
		return nil, fmt.Errorf("attachment delivery: invalid redirect")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := plain.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("attachment delivery: object store answered %d", resp.StatusCode)
	}
	return resp.Body, nil
}
