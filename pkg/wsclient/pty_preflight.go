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

package wsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// PTYPreflightError reports that the Hub's attach preflight (a plain,
// non-upgrade GET of the agent's /pty endpoint) refused the attach. The Hub
// answers the preflight with the same path decision it makes for the
// WebSocket open, so a refusal here means the WebSocket would be refused
// too.
type PTYPreflightError struct {
	// Status is the HTTP status of the preflight response.
	Status int
	// Code is the machine-readable error code (error.code), if any.
	Code string
	// Reason is error.details.reason, if any (for example
	// agent_pty_unavailable or broker_not_connected).
	Reason string
	// Message is the Hub's human-readable error.message, if any.
	Message string
}

func (e *PTYPreflightError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "attach refused by the Hub with status %d", e.Status)
	switch {
	case e.Code != "" && e.Reason != "":
		fmt.Fprintf(&b, " (%s, reason %s)", e.Code, e.Reason)
	case e.Code != "":
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// NoPath reports whether the Hub found no way to attach a terminal to the
// agent at all: its runtime has no attach and the agent has no session
// that serves a terminal. Retrying will not help until that changes.
func (e *PTYPreflightError) NoPath() bool {
	return e.Status == http.StatusServiceUnavailable && e.Code == wsprotocol.ErrCodeRuntimeAttachUnsupported
}

// preflightErrorBody is the Hub's JSON error envelope.
type preflightErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

// Preflight asks the Hub whether an attach can proceed, without upgrading
// to a WebSocket: a 200 means the Hub has a path to the agent's terminal.
// Any other status is returned as a *PTYPreflightError. It sends the same
// credentials the WebSocket dial sends.
func (c *PTYClient) Preflight(ctx context.Context) error {
	wsURL, err := c.buildWebSocketURL()
	if err != nil {
		return fmt.Errorf("failed to build URL: %w", err)
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return fmt.Errorf("failed to build URL: %w", err)
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	default:
		u.Scheme = "http"
	}

	reqCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to build preflight request: %w", err)
	}
	if c.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.Token)
	}
	if c.config.TransportSource != nil {
		if err := transportauth.ApplyHeaders(req.Header, c.config.TransportSource, c.config.TransportMode); err != nil {
			slog.Debug("Transport auth header failed, proceeding without", "error", err)
		}
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		if reqCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("attach preflight timed out after %v", connectTimeout)
		}
		return fmt.Errorf("attach preflight failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	perr := &PTYPreflightError{Status: resp.StatusCode}
	var parsed preflightErrorBody
	if json.Unmarshal(body, &parsed) == nil && (parsed.Error.Code != "" || parsed.Error.Message != "") {
		perr.Code = parsed.Error.Code
		perr.Reason = parsed.Error.Details.Reason
		perr.Message = parsed.Error.Message
	} else {
		perr.Message = strings.TrimSpace(string(body))
	}
	return perr
}

// httpClient returns the client used for the preflight.
func (c *PTYClient) httpClient() *http.Client {
	if c.preflightClient != nil {
		return c.preflightClient
	}
	return http.DefaultClient
}
