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
	"unicode"
	"unicode/utf8"

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
	text := "attach refused by the Hub (" + e.Detail() + ")"
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

// Detail formats the status, code and reason, skipping empty parts:
// "status 503, runtime_attach_unsupported, reason agent_pty_unavailable".
func (e *PTYPreflightError) Detail() string {
	parts := []string{fmt.Sprintf("status %d", e.Status)}
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Reason != "" {
		parts = append(parts, "reason "+e.Reason)
	}
	return strings.Join(parts, ", ")
}

// NoPath reports whether the Hub found no way to attach a terminal to the
// agent at all: its runtime has no attach and the agent has no session
// that serves a terminal. Retrying will not help until that changes.
func (e *PTYPreflightError) NoPath() bool {
	return e.Status == http.StatusServiceUnavailable && e.Code == wsprotocol.ErrCodeRuntimeAttachUnsupported
}

// preflightTransportError wraps a preflight that got no answer from the
// Hub (a network or transport failure, as opposed to a refusal). Before a
// reconnect it is treated as transient and retried under the backoff.
type preflightTransportError struct{ err error }

func (e *preflightTransportError) Error() string { return e.err.Error() }
func (e *preflightTransportError) Unwrap() error { return e.err }

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
		perr.Code = normalizeErrorText(parsed.Error.Code, maxPreflightToken)
		perr.Reason = normalizeErrorText(parsed.Error.Details.Reason, maxPreflightToken)
		perr.Message = normalizeErrorText(parsed.Error.Message, maxPreflightMessage)
	} else {
		perr.Message = normalizeErrorText(string(body), maxPreflightMessage)
	}
	return perr
}

// Bounds on the preflight error text that reaches PTYPreflightError (and
// the CLI's output): the message, and the code and reason tokens.
const (
	maxPreflightMessage = 200
	maxPreflightToken   = 100
)

// normalizeErrorText normalises preflight error text from the response,
// whether a JSON field or a non-JSON body (for example an HTML error page
// from a load balancer or proxy): it keeps the first non-empty line, drops
// control and Unicode format characters, and cuts the result to maxBytes
// on a UTF-8 boundary.
func normalizeErrorText(text string, maxBytes int) string {
	text = strings.ToValidUTF8(text, "")
	line := ""
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, line)
	line = strings.TrimSpace(line)
	if len(line) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut]
	}
	return line
}

// httpClient returns the client used for the preflight. Like the
// WebSocket dialer it uses no proxy from the environment, and it does not
// follow redirects: a redirect (for example an auth proxy sending the
// request to a login page) is returned as a non-200 refusal rather than
// followed to a page that answers 200.
func (c *PTYClient) httpClient() *http.Client {
	// Start from the default transport's settings when it is the standard
	// *http.Transport; if something has replaced it with another
	// RoundTripper, use a plain transport rather than wrapping it.
	var transport *http.Transport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.Proxy = nil // websocket.Dialer{} in dial has no Proxy either
	// One request per client: do not leave an idle connection (and its
	// goroutines) behind after the preflight.
	transport.DisableKeepAlives = true
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
