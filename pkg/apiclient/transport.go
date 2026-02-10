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

// WithRetry configures automatic retry behavior.
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
func (t *Transport) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	// Set User-Agent
	if t.UserAgent != "" {
		req.Header.Set("User-Agent", t.UserAgent)
	}

	// Apply authentication
	if t.Auth != nil {
		if err := t.Auth.ApplyAuth(req); err != nil {
			return nil, fmt.Errorf("failed to apply auth: %w", err)
		}
	}

	// Execute with retries
	var resp *http.Response
	var err error

	attempts := t.MaxRetries + 1
	for i := 0; i < attempts; i++ {
		resp, err = t.HTTPClient.Do(req.WithContext(ctx))
		if err != nil {
			// Check if context was cancelled
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			// Retry on network errors
			if i < t.MaxRetries {
				time.Sleep(t.RetryWait)
				continue
			}
			return nil, fmt.Errorf("request failed: %w", err)
		}

		// Retry on 5xx errors
		if resp.StatusCode >= 500 && i < t.MaxRetries {
			resp.Body.Close()
			time.Sleep(t.RetryWait)
			continue
		}

		break
	}

	return resp, nil
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

	return t.Do(ctx, req)
}

// DecodeResponse reads and decodes a JSON response body.
// If the response indicates an error (status >= 400), it returns an APIError.
func DecodeResponse[T any](resp *http.Response) (*T, error) {
	defer resp.Body.Close()

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

// CheckResponse checks if a response indicates an error.
// Returns nil on success (2xx status codes), otherwise returns an APIError.
func CheckResponse(resp *http.Response) error {
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return ParseErrorResponse(resp)
	}

	return nil
}
