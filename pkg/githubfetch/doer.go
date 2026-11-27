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

package githubfetch

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
)

// Request describes one HTTP GET made through a Doer.
type Request struct {
	URL           string
	Authorization string
	// Header carries optional allowlisted request headers (Accept, If-None-Match).
	Header http.Header
	// MaxBytes optionally bounds the response body stream for this request.
	MaxBytes int64
}

// Response is an open streaming HTTP response from a Doer.
type Response struct {
	Status int
	// Header holds allowlisted response headers (ETag, Retry-After,
	// X-RateLimit-*, X-GitHub-SSO, Content-Type, Content-Length).
	Header http.Header
	Body   io.ReadCloser
}

// Doer is the transport seam for Client. Production builds a Doer backed by
// remotefetch.Fetcher; tests inject an httptest-backed Doer without relaxing
// remotefetch's unexported seams.
type Doer interface {
	Open(ctx context.Context, r Request) (*Response, error)
}

// RemoteFetchDoer adapts a *remotefetch.Fetcher to Doer.
type RemoteFetchDoer struct {
	Fetcher *remotefetch.Fetcher
}

// Open delegates to Fetcher.Open.
func (d *RemoteFetchDoer) Open(ctx context.Context, r Request) (*Response, error) {
	resp, err := d.Fetcher.Open(ctx, remotefetch.Request{
		URL:           r.URL,
		Authorization: r.Authorization,
		Header:        r.Header,
		MaxBytes:      r.MaxBytes,
	})
	if err != nil {
		return nil, err
	}
	return &Response{
		Status: resp.Status,
		Header: resp.Header,
		Body:   resp.Body,
	}, nil
}

// NewDoer builds a production Doer backed by remotefetch.Fetcher with the
// endpoint's AllowedHosts, the requested network mode, and a redirect cap of 3.
func NewDoer(ep Endpoint, network remotefetch.NetworkMode, maxBytes int64, timeout time.Duration, logger *slog.Logger) Doer {
	f := remotefetch.New(remotefetch.Config{
		MaxBytes:     maxBytes,
		Timeout:      timeout,
		MaxRedirects: 3,
		Network:      network,
		AllowedHosts: ep.AllowedHosts(),
		Logger:       logger,
	})
	return &RemoteFetchDoer{Fetcher: f}
}
