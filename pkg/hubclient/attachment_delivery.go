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

// FetchAttachmentDelivery implements AttachmentDeliveryFetcher. Its errors
// never carry a URL: the hub path holds the delivery capability and the
// object-store URL is signed, so a caller may log any error it returns
// (deliveryError).
func (c *client) FetchAttachmentDelivery(ctx context.Context, path string) (io.ReadCloser, error) {
	body, err := c.fetchAttachmentDelivery(ctx, path)
	if err != nil {
		return nil, deliveryError(err)
	}
	return body, nil
}

// deliveryError returns err without any URL: a refusal, an API error from
// the hub (built from the response, never the request) and a status from
// the object store pass as they are; a transport error (*url.Error, which
// formats with the full URL) keeps only its operation and cause; anything
// else becomes a fixed message.
func deliveryError(err error) error {
	var ue *url.Error
	var apiErr *apiclient.APIError
	switch {
	case errors.Is(err, errDeliveryRefused), errors.Is(err, errDeliveryObjectStore), errors.Is(err, errDeliveryPath),
		errors.Is(err, errDeliveryRedirect), errors.As(err, &apiErr):
		return err
	case errors.As(err, &ue):
		return fmt.Errorf("attachment delivery: %s request failed: %w", ue.Op, ue.Err)
	default:
		return errors.New("attachment delivery: request failed")
	}
}

// Fixed errors of a delivery fetch; none carries a URL.
var (
	errDeliveryObjectStore = errors.New("attachment delivery: object store refused the download")
	errDeliveryPath        = errors.New("attachment delivery: unexpected capability path")
	errDeliveryRedirect    = errors.New("attachment delivery: invalid redirect")
)

func (c *client) fetchAttachmentDelivery(ctx context.Context, path string) (io.ReadCloser, error) {
	if !strings.HasPrefix(path, attachmentDeliveriesPath) || strings.ContainsAny(path, "?#") {
		return nil, errDeliveryPath
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
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		return fetchRedirect(ctx, loc, strings.HasPrefix(c.transport.BaseURL, "http://"))
	default:
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusNotFound {
			return nil, errDeliveryRefused
		}
		if err := apiclient.CheckResponse(resp); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w (status %d)", errDeliveryRefused, resp.StatusCode)
	}
}

// fetchRedirect downloads loc, an object-store URL, with a client that
// carries no credential and follows no further redirect. Only https is
// followed, unless the hub itself is reached over plain http (allowHTTP,
// a local development hub).
func fetchRedirect(ctx context.Context, loc string, allowHTTP bool) (io.ReadCloser, error) {
	u, err := url.Parse(loc)
	if err != nil || (u.Scheme != "https" && (u.Scheme != "http" || !allowHTTP)) || u.Host == "" {
		return nil, errDeliveryRedirect
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
		return nil, fmt.Errorf("%w (status %d)", errDeliveryObjectStore, resp.StatusCode)
	}
	return resp.Body, nil
}
