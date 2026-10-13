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

package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The hub internal API (design §3.9 "Authorization: the relay is
// policy-free"): a separate relay calls a hub replica for everything that
// needs policy. It is served on the hub's internal listener only, never on
// its public router, and every call is a relay-peer call: HMAC-signed with
// the key derived from the dedicated peer secret (OIDC added where
// available), addressed to the fixed target HubPeerTarget, with the body
// covered by HeaderBodySHA256. The hub checks each nonce against a store
// shared by its replicas, so a request is accepted once across all of them.
// Everything the hub trusts travels in the signed body, never in a header.
const (
	// HubInternalPathPrefix is the path prefix of the hub internal API.
	HubInternalPathPrefix = "/internal/v1/hub/"
	// HubPeerTarget is the relay-peer target every hub replica answers to
	// (relay-to-hub calls may reach any replica), with generation 0.
	HubPeerTarget = "hub"
)

// Hub internal API operation paths.
const (
	HubAdmitPath          = HubInternalPathPrefix + "admit"
	HubSessionsPrefix     = HubInternalPathPrefix + "sessions/"
	HubSessionRecheckPath = HubSessionsPrefix + "recheck"
	HubStreamRecheckPath  = HubInternalPathPrefix + "streams/recheck"
)

// HubSessionPath returns the path of op ("rpc" or "refresh") on sessionID.
func HubSessionPath(sessionID, op string) string {
	return HubSessionsPrefix + sessionID + "/" + op
}

// MaxHubRequestBody bounds a hub internal API request body.
const MaxHubRequestBody = 2 << 20

// ErrHubUnavailable marks a hub internal API call that could not be
// evaluated: the hub was unreachable, answered 503 or another 5xx, or its
// answer could not be read. Callers treat it as an infrastructure fault:
// never as a refusal (design §3.9: nothing closes, nothing is renewed).
var ErrHubUnavailable = errors.New("conduit relay: hub unavailable")

// HubRefusal is a hub answer that refuses the call (a 4xx). Status and
// Body are the hub's response, passed on unchanged where the caller
// answers a dialer (Admit).
type HubRefusal struct {
	Status int
	Header http.Header
	Body   []byte
}

func (e *HubRefusal) Error() string {
	return fmt.Sprintf("conduit relay: hub refused the call (HTTP %d)", e.Status)
}

// HubClient calls the hub internal API.
type HubClient struct {
	// BaseURL is server.relay.hub_internal_url.
	BaseURL string
	// HTTP performs the calls (nil: http.DefaultClient).
	HTTP *http.Client
	// Auth signs every call. Required.
	Auth PeerAuth
	// Timeout bounds one call (0: 10s).
	Timeout time.Duration
}

// DefaultHubCallTimeout bounds one hub internal API call.
const DefaultHubCallTimeout = 10 * time.Second

// Call POSTs in (as JSON) to path and decodes a 200 answer into out (nil:
// the body is discarded). A 4xx is a *HubRefusal; anything that leaves
// the outcome unknown wraps ErrHubUnavailable.
func (c *HubClient) Call(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.post(ctx, path, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp, out); err != nil {
		return fmt.Errorf("%w: malformed answer: %v", ErrHubUnavailable, err)
	}
	return nil
}

// post sends body to path and returns a 200 answer's body.
func (c *HubClient) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	if c.Auth == nil {
		return nil, errors.New("conduit relay: hub client has no relay-peer authenticator")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultHubCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderBodySHA256, hex.EncodeToString(sum[:]))
	SetPeerTarget(req, HubPeerTarget, 0)
	if err := c.Auth.Sign(req); err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHubUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, MaxHubRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the answer: %v", ErrHubUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return out, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nil, &HubRefusal{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: out}
	default:
		return nil, fmt.Errorf("%w: HTTP %d", ErrHubUnavailable, resp.StatusCode)
	}
}
