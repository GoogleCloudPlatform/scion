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

// Package apiclient provides shared HTTP client utilities for Scion API clients.
package apiclient

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
)

// Transport provides HTTP transport with standard behaviors.
type Transport struct {
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string

	// Optional retry configuration
	MaxRetries int
	RetryWait  time.Duration

	// Auth is an optional authenticator
	Auth Authenticator
}

// TransportOption configures a Transport.
type TransportOption func(*Transport)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) TransportOption {
	return func(t *Transport) { t.HTTPClient = c }
}

// WithTimeout sets the default request timeout.
func WithTimeout(d time.Duration) TransportOption {
	return func(t *Transport) {
		t.HTTPClient.Timeout = d
	}
}

// WithRetry configures automatic retry behavior for Do: up to maxRetries
// further attempts after a transport error or a 5xx response, waiting wait
// between attempts. Retries are off by default (MaxRetries 0). A retry
// replays the whole request, body included, so only enable this for
// transports whose Do callers are idempotent; DoNoRetry is never retried.
// Pinning non-idempotent callers is tracked in ptone/scion#2955.
func WithRetry(maxRetries int, wait time.Duration) TransportOption {
	return func(t *Transport) {
		t.MaxRetries = maxRetries
		t.RetryWait = wait
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) TransportOption {
	return func(t *Transport) {
		t.UserAgent = ua
	}
}

// WithAuth sets the authenticator.
func WithAuth(auth Authenticator) TransportOption {
	return func(t *Transport) {
		t.Auth = auth
	}
}

// NewTransport creates a new Transport with the given base URL and options.
func NewTransport(baseURL string, opts ...TransportOption) *Transport {
	t := &Transport{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		UserAgent:  "scion-client/1.0",
		MaxRetries: 0,
		RetryWait:  time.Second,
	}

	for _, opt := range opts {
		opt(t)
	}

	return t
}

// Do executes an HTTP request with configured behaviors.
// Handles retries, timeout, and wraps errors.
//
// With MaxRetries > 0 (WithRetry), a transport error or a 5xx response is
// retried. Each retry re-sends the full request body, rewound from
// req.GetBody (which http.NewRequest populates for *bytes.Reader,
// *bytes.Buffer and *strings.Reader bodies). A request whose body cannot be
// rewound (non-empty Body, nil GetBody) is sent exactly once: its first
// response is returned as-is, and its first transport error is returned
// wrapped as "request failed: %w".
//
// A retry replays the request, so Do must only be used with retries enabled
// for idempotent operations. Non-idempotent operations should use DoNoRetry
// (or a *NoRetry helper such as PostNoRetry) instead.
func (t *Transport) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if err := t.prepare(req); err != nil {
		return nil, err
	}

	// Execute with retries
	var resp *http.Response
	var err error

	canRewind := bodyRewindable(req)
	attempts := t.MaxRetries + 1
	for i := 0; i < attempts; i++ {
		attempt := req.WithContext(ctx)
		if i > 0 && req.GetBody != nil {
			// The previous attempt drained (and the client closed) the body;
			// hand this attempt a fresh copy. attempt is a shallow copy, so
			// the caller's req is left untouched.
			body, gerr := req.GetBody()
			if gerr != nil {
				return nil, fmt.Errorf("request failed: rewinding body for retry: %w", gerr)
			}
			attempt.Body = body
		}

		resp, err = t.HTTPClient.Do(attempt)
		retry := i < t.MaxRetries && canRewind
		if err != nil {
			// Check if context was cancelled
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			// Retry on network errors
			if retry {
				time.Sleep(t.RetryWait)
				continue
			}
			return nil, fmt.Errorf("request failed: %w", err)
		}

		// Retry on 5xx errors
		if resp.StatusCode >= 500 && retry {
			_ = resp.Body.Close()
			time.Sleep(t.RetryWait)
			continue
		}

		break
	}

	return resp, nil
}

// bodyRewindable reports whether req can be re-sent with its full body: it
// has no body at all, or its body can be recreated through GetBody.
func bodyRewindable(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

// prepare applies the headers and authentication every send path (Do and
// DoNoRetry) needs before handing req to an *http.Client.
func (t *Transport) prepare(req *http.Request) error {
	if t.UserAgent != "" {
		req.Header.Set("User-Agent", t.UserAgent)
	}
	if t.Auth != nil {
		if err := t.Auth.ApplyAuth(req); err != nil {
			return fmt.Errorf("failed to apply auth: %w", err)
		}
	}
	return nil
}

// DoNoRetry sends req once at this layer: no MaxRetries (even on a Transport
// built with WithRetry) and no HTTP redirect following — a 3xx is returned
// to the caller unchanged. Use this instead of Do for a non-idempotent
// operation (e.g. agent keys injection) where any replay is unsafe.
//
// This governs this package's own behavior only: net/http can still retry
// internally in the narrow cases where it can prove nothing was written
// (a dead reused HTTP/1 connection, HTTP/2 REFUSED_STREAM/post-GOAWAY) —
// those never reach the server, so they don't reopen a double-injection risk.
func (t *Transport) DoNoRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	if err := t.prepare(req); err != nil {
		return nil, err
	}

	base := t.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	// Shallow-copy the *http.Client so the underlying RoundTripper (and its
	// connection pool) is shared with Do, but this one call's redirect policy
	// is not: CheckRedirect is overridden to stop following at the first hop,
	// regardless of whatever policy (including Go's default) the shared
	// client would otherwise apply.
	once := *base
	once.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := once.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

// AuthenticatedHTTPClient returns an *http.Client that applies this transport's
// authentication to every request. This is useful for code paths that need a
// raw *http.Client (e.g. the transfer client for signed URL uploads/downloads)
// but still need to authenticate with the hub when signed URLs are HTTP proxy
// URLs rather than GCS signed URLs or file:// URLs.
func (t *Transport) AuthenticatedHTTPClient() *http.Client {
	base := t.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	return &http.Client{
		Timeout: base.Timeout,
		Transport: &authRoundTripper{
			base: base.Transport,
			auth: t.Auth,
			ua:   t.UserAgent,
		},
	}
}

// authRoundTripper is an http.RoundTripper that applies authentication headers.
type authRoundTripper struct {
	base http.RoundTripper
	auth Authenticator
	ua   string
}

func (rt *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if rt.ua != "" && clone.Header.Get("User-Agent") == "" {
		clone.Header.Set("User-Agent", rt.ua)
	}
	if rt.auth != nil {
		if err := rt.auth.ApplyAuth(clone); err != nil {
			return nil, fmt.Errorf("failed to apply auth: %w", err)
		}
	}
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// buildURL joins the base URL with a path and optional query parameters.
func (t *Transport) buildURL(path string, query url.Values) string {
	u := t.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// Get performs an HTTP GET request.
func (t *Transport) Get(ctx context.Context, path string, headers http.Header) (*http.Response, error) {
	return t.GetWithQuery(ctx, path, nil, headers)
}

// GetWithQuery performs an HTTP GET request with query parameters.
func (t *Transport) GetWithQuery(ctx context.Context, path string, query url.Values, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.buildURL(path, query), nil)
	if err != nil {
		return nil, err
	}

	for k, v := range headers {
		req.Header[k] = v
	}

	return t.Do(ctx, req)
}

// Post performs an HTTP POST request with JSON body.
func (t *Transport) Post(ctx context.Context, path string, body interface{}, headers http.Header) (*http.Response, error) {
	return t.doJSON(ctx, http.MethodPost, path, body, headers)
}

// PostNoRetry performs an HTTP POST request with a JSON body via DoNoRetry:
// exactly one attempt, no retry on network error or 5xx, no redirect
// following. See DoNoRetry's doc comment for which callers need this instead
// of Post.
func (t *Transport) PostNoRetry(ctx context.Context, path string, body interface{}, headers http.Header) (*http.Response, error) {
	return t.doJSONVia(ctx, t.DoNoRetry, http.MethodPost, path, body, headers)
}

// Put performs an HTTP PUT request with JSON body.
func (t *Transport) Put(ctx context.Context, path string, body interface{}, headers http.Header) (*http.Response, error) {
	return t.doJSON(ctx, http.MethodPut, path, body, headers)
}

// Patch performs an HTTP PATCH request with JSON body.
func (t *Transport) Patch(ctx context.Context, path string, body interface{}, headers http.Header) (*http.Response, error) {
	return t.doJSON(ctx, http.MethodPatch, path, body, headers)
}

// Delete performs an HTTP DELETE request.
func (t *Transport) Delete(ctx context.Context, path string, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.buildURL(path, nil), nil)
	if err != nil {
		return nil, err
	}

	for k, v := range headers {
		req.Header[k] = v
	}

	return t.Do(ctx, req)
}

// doJSON performs an HTTP request with a JSON body.
func (t *Transport) doJSON(ctx context.Context, method, path string, body interface{}, headers http.Header) (*http.Response, error) {
	return t.doJSONVia(ctx, t.Do, method, path, body, headers)
}

// doJSONVia builds a JSON request exactly as doJSON does, then hands it to
// send instead of always using Do — the one difference between Post and
// PostNoRetry (and any future *NoRetry sibling).
func (t *Transport) doJSONVia(ctx context.Context, send func(context.Context, *http.Request) (*http.Response, error), method, path string, body interface{}, headers http.Header) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, t.buildURL(path, nil), bodyReader)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	for k, v := range headers {
		req.Header[k] = v
	}

	return send(ctx, req)
}

// DecodeResponse reads and decodes a JSON response body.
// If the response indicates an error (status >= 400), it returns an APIError.
func DecodeResponse[T any](resp *http.Response) (*T, error) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, ParseErrorResponse(resp)
	}

	// Handle 204 No Content
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	var result T
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// ErrNoContent is returned by DecodeRequired when a successful response has
// no body to decode.
var ErrNoContent = errors.New("server returned no content")

// DecodeRequired is like DecodeResponse, for endpoints that must return a
// body. A 204 No Content or an empty 2xx body returns an error wrapping
// ErrNoContent instead of a nil result, so callers never see (nil, nil).
func DecodeRequired[T any](resp *http.Response) (*T, error) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, ParseErrorResponse(resp)
	}

	if resp.StatusCode == http.StatusNoContent {
		return nil, fmt.Errorf("%w (status: %d)", ErrNoContent, resp.StatusCode)
	}

	var result T
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w (status: %d)", ErrNoContent, resp.StatusCode)
		}
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// CheckResponse checks if a response indicates an error.
// Returns nil on success (2xx status codes), otherwise returns an APIError.
func CheckResponse(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return ParseErrorResponse(resp)
	}

	return nil
}
