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

package delegation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// maxRequestBody bounds a proxied request body. The body is buffered so
// that a request the hub refused with 401 can be sent once more with a
// newly exchanged credential.
const maxRequestBody = 4 << 20

// apiPathPrefix is the only path prefix the proxy forwards.
const apiPathPrefix = "/api/v1/"

// forwardedRequestHeaders are the only request headers copied to the hub.
// Credentials the caller sends (Authorization, X-Scion-Agent-Token,
// cookies) are never forwarded: the request carries the delegated
// credential and nothing else.
var forwardedRequestHeaders = []string{"Accept", "Content-Type", "If-Match", "If-None-Match"}

// droppedResponseHeaders are hub response headers not copied back.
var droppedResponseHeaders = map[string]bool{
	"Set-Cookie":        true,
	"Connection":        true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Keep-Alive":        true,
}

// Codes of the errors the helper answers with itself, in the hub's
// structured error body.
const (
	codeGrantRequired     = "delegation_grant_required"
	codeGrantInvalid      = "delegation_grant_invalid"
	codePathNotAllowed    = "delegation_path_not_allowed"
	codeRequestTooLarge   = "request_too_large"
	codeUnavailable       = "delegation_unavailable"
	codeRefreshPending    = "agent_credential_refresh_pending"
	codeHubUnreachable    = "hub_unreachable"
	codeTooManyGrants     = "delegation_too_many_grants"
	codeExchangeMalformed = "delegation_exchange_failed"
)

// ServeHTTP forwards a request received on the delegation socket to the
// hub with the named grant's delegated credential attached.
func (h *Helper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	grantID := r.Header.Get(GrantHeader)
	switch {
	case grantID == "":
		writeError(w, http.StatusBadRequest, codeGrantRequired, "the "+GrantHeader+" header names the delegation grant to use")
		return
	case !grantIDPattern.MatchString(grantID):
		writeError(w, http.StatusBadRequest, codeGrantInvalid, "the "+GrantHeader+" header is not a grant ID")
		return
	}
	path := r.URL.EscapedPath()
	if !allowedPath(path, r.URL.Path) {
		writeError(w, http.StatusBadRequest, codePathNotAllowed, "only hub API paths under "+apiPathPrefix+" are forwarded")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeRequestTooLarge, "the request body could not be read")
		return
	}
	if len(body) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, codeRequestTooLarge, "the request body is too large")
		return
	}

	target := strings.TrimSuffix(h.client.HubURL(), "/") + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	resp, err := h.forward(r.Context(), grantID, r, target, body)
	if err != nil {
		h.writeForwardError(w, grantID, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		if droppedResponseHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// allowedPath reports whether a request path is a hub API path the proxy
// forwards. Both the path as sent (escaped) and its decoded form must be
// under apiPathPrefix, and no decoded segment may be "." or "..", however
// it was encoded ("%2e", ".%2e", "%2e%2e", ...).
func allowedPath(escaped, decoded string) bool {
	if !strings.HasPrefix(escaped, apiPathPrefix) || !strings.HasPrefix(decoded, apiPathPrefix) {
		return false
	}
	for _, seg := range strings.Split(decoded, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// forward sends the request with grant id's credential. When the hub
// refuses the credential with 401 (a later exchange superseded it, or it
// was revoked), the credential is dropped, the grant exchanged again and
// the request sent once more.
func (h *Helper) forward(ctx context.Context, id string, in *http.Request, target string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		tok, err := h.credential(ctx, id)
		if err != nil {
			return nil, err
		}
		resp, err := h.send(ctx, in, target, body, tok)
		if err != nil {
			return nil, &unreachableError{err: err}
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		log.Info("Delegation helper: hub refused the credential of grant %s on a request; exchanging again", id)
		h.invalidate(id, tok)
	}
}

// send makes one request to the hub with tok as the bearer.
func (h *Helper) send(ctx context.Context, in *http.Request, target string, body []byte, tok string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, in.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, k := range forwardedRequestHeaders {
		if v := in.Header.Values(k); len(v) > 0 {
			req.Header[k] = append([]string(nil), v...)
		}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return h.httpClient().Do(req)
}

// httpClient is the hub client's HTTP client, set not to follow
// redirects: a redirect answer is returned to the caller as is, and the
// credential is never sent to a location the hub names.
func (h *Helper) httpClient() *http.Client {
	base := h.client.HTTPClient()
	if base == nil {
		base = &http.Client{Timeout: hub.DefaultTimeout}
	}
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// unreachableError is a proxied request that got no answer from the hub.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return "hub request failed: " + e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

// writeForwardError answers a request the helper could not forward.
func (h *Helper) writeForwardError(w http.ResponseWriter, id string, err error) {
	var (
		xe *hub.ExchangeError
		be *backoffError
		ue *unreachableError
	)
	switch {
	case errors.As(err, &be):
		if wait := be.until.Sub(h.clk.Now()); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		}
		code := codeUnavailable
		if errors.Is(err, errReauthPending) {
			code = codeRefreshPending
		}
		writeError(w, http.StatusServiceUnavailable, code, be.Error())
	case errors.As(err, &xe) && xe.Reauthenticate():
		writeError(w, http.StatusServiceUnavailable, codeRefreshPending, errReauthPending.Error())
	case errors.As(err, &xe) && xe.Retryable():
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "the delegation exchange is unavailable; retry later")
	case errors.As(err, &xe):
		// The hub denied the exchange: pass its status and code through.
		code := xe.Code
		if code == "" {
			code = codeExchangeMalformed
		}
		writeError(w, xe.StatusCode, code, xe.Message)
	case errors.Is(err, errTooManyGrants):
		writeError(w, http.StatusServiceUnavailable, codeTooManyGrants, err.Error())
	case errors.As(err, &ue):
		log.Error("Delegation helper: request with grant %s got no answer from the hub: %v", id, ue.err)
		writeError(w, http.StatusBadGateway, codeHubUnreachable, "the hub did not answer")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "the request was cancelled")
	default:
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "the delegation exchange is unavailable; retry later")
	}
}

// writeError writes the hub's structured error body.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}
