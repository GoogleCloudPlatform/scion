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

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
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

// HubAdmitRequest asks the hub to admit a dialer the relay received
// (design §3.9 "Admission"). Headers are the dialer's credential headers
// (ForwardCredentialHeaders), passed unchanged, and Host and RawQuery are
// the ones the relay received, so a signed request (broker HMAC) verifies
// as sent. SessionID is the id the relay will admit the session under.
type HubAdmitRequest struct {
	RelayInstanceID string              `json:"relay_instance_id"`
	RelayGeneration int64               `json:"relay_generation"`
	SessionID       string              `json:"session_id"`
	Host            string              `json:"host"`
	RawQuery        string              `json:"raw_query,omitempty"`
	Headers         map[string][]string `json:"headers"`
}

// HubAdmitResponse is an admitted dialer: the principal to serve the
// session as, and the admission token to present, unchanged and unparsed,
// on every later hub call for the session. The principal fields are for
// serving the session and logging only: the hub never reads them back.
type HubAdmitResponse struct {
	AdmissionToken   string    `json:"admission_token"`
	PrincipalKind    string    `json:"principal_kind"`
	PrincipalID      string    `json:"principal_id"`
	ProjectID        string    `json:"project_id,omitempty"`
	ExecScope        string    `json:"exec_scope,omitempty"`
	AgentLaunchID    string    `json:"agent_launch_id,omitempty"`
	AgentGeneration  int64     `json:"agent_generation,omitempty"`
	TokenRunID       string    `json:"token_run_id,omitempty"`
	TokenRunEnforce  bool      `json:"token_run_enforce,omitempty"`
	CredentialExpiry time.Time `json:"credential_expiry,omitzero"`
	// GrantKeys are the hub's grant verification public keys, sent to the
	// dialer in Welcome.
	GrantKeys []HubGrantKey `json:"grant_keys"`
}

// HubGrantKey is one grant verification public key (Welcome.grant_keys).
type HubGrantKey struct {
	KID          string `json:"kid"`
	PublicKey    []byte `json:"public_key"`
	NotAfterUnix int64  `json:"not_after_unix"`
}

// WelcomeGrantKeys converts keys to Welcome.grant_keys.
func WelcomeGrantKeys(keys []HubGrantKey) []*conduitv1.GrantKey {
	out := make([]*conduitv1.GrantKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, &conduitv1.GrantKey{Kid: k.KID, PublicKey: append([]byte(nil), k.PublicKey...), NotAfterUnix: k.NotAfterUnix})
	}
	return out
}

// forwardedCredentialHeaders are the dialer headers a relay passes to Admit
// (and the hub accepts from it): the credentials the hub's verifiers read,
// and the run id an agent presents. Trusted-proxy identity headers are
// never forwarded: they are trusted only from a proxy in front of the hub.
var forwardedCredentialHeaders = []string{
	"Authorization",
	"X-Scion-Agent-Token",
	"X-Scion-Run-Id",
	"X-Goog-IAP-JWT-Assertion",
	"X-Api-Key",
	"X-Scion-Broker-Id",
	"X-Scion-Timestamp",
	"X-Scion-Nonce",
	"X-Scion-Signature",
	"X-Scion-Signed-Headers",
	"X-Scion-On-Behalf-Of",
}

// hopByHopHeaders are never forwarded (RFC 9110 §7.6.1), nor is Host,
// which travels as HubAdmitRequest.Host.
var hopByHopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Proxy-Connection": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Host": true,
}

// ForwardCredentialHeaders returns the headers of h a relay forwards to
// Admit: the credential headers, plus any header a broker signature lists
// in X-Scion-Signed-Headers, so the signature verifies as sent. Hop-by-hop
// headers are dropped.
func ForwardCredentialHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	add := func(name string) {
		k := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if k == "" || hopByHopHeaders[k] {
			return
		}
		if v, ok := h[k]; ok && len(v) > 0 {
			out[k] = append([]string(nil), v...)
		}
	}
	for _, name := range forwardedCredentialHeaders {
		add(name)
	}
	for _, list := range h.Values("X-Scion-Signed-Headers") {
		for _, name := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
			add(name)
		}
	}
	return out
}

// The requests below identify the session only by its admission token
// (and the session id in the path): the hub takes the principal from the
// token, never from the relay. The relay stores each token unchanged and
// replaces it when the hub returns a new one.

// HubRPCRequest forwards one RPC the principal of a relay-held session
// sent (POST sessions/{id}/rpc). RPC is the encoded RpcRequest.
type HubRPCRequest struct {
	AdmissionToken string `json:"admission_token"`
	RPC            []byte `json:"rpc"`
}

// HubRPCResponse is the RPC's answer (the encoded RpcResponse).
type HubRPCResponse struct {
	RPC []byte `json:"rpc"`
}

// HubRefreshRequest carries an in-band AuthRefresh credential of a
// relay-held session (POST sessions/{id}/refresh).
type HubRefreshRequest struct {
	AdmissionToken string `json:"admission_token"`
	Credential     []byte `json:"credential"`
}

// HubRefreshResponse accepts the refreshed credential: the session's new
// admission token and credential expiry.
type HubRefreshResponse struct {
	AdmissionToken   string    `json:"admission_token"`
	CredentialExpiry time.Time `json:"credential_expiry,omitzero"`
}

// HubSessionRecheckRequest asks the hub to re-check relay-held sessions
// (POST sessions/recheck).
type HubSessionRecheckRequest struct {
	Items []HubSessionRecheckItem `json:"items"`
}

// HubSessionRecheckItem is one session to re-check.
type HubSessionRecheckItem struct {
	SessionID      string `json:"session_id"`
	AdmissionToken string `json:"admission_token"`
}

// HubSessionRecheckResponse has one result per item, in item order.
type HubSessionRecheckResponse struct {
	Results []HubSessionRecheckResult `json:"results"`
}

// HubSessionRecheckResult is one session's re-check: OK (keep it, and
// replace its token when AdmissionToken is set), Close (close it with
// CloseCode), or neither (the hub could not evaluate it: keep it, the next
// sweep tries again).
type HubSessionRecheckResult struct {
	SessionID      string `json:"session_id"`
	OK             bool   `json:"ok,omitempty"`
	AdmissionToken string `json:"admission_token,omitempty"`
	Close          bool   `json:"close,omitempty"`
	CloseCode      uint32 `json:"close_code,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// MaxHubRecheckBatch bounds the items of one re-check call.
const MaxHubRecheckBatch = 64

// HubStreamRecheckRequest asks the hub to re-check relay-held user streams
// and renew their authorization (POST streams/recheck; design §3.9
// "Active authorization").
type HubStreamRecheckRequest struct {
	Items []HubStreamRecheckItem `json:"items"`
}

// HubStreamRecheckItem is one stream: the user session holding its leaf
// (with that session's admission token), the stream as the target sees it
// (TargetStream), the open grant's jti, and the stream's latest hub-signed
// grant (the open grant or the latest renewal), stored and passed back
// unchanged and unparsed. Trigger names what scheduled the re-check, for
// the hub's log and metrics.
type HubStreamRecheckItem struct {
	UserSessionID   string `json:"user_session_id"`
	AdmissionToken  string `json:"admission_token"`
	TargetSessionID string `json:"target_session_id"`
	TargetStreamID  uint32 `json:"target_stream_id"`
	OpenJTI         string `json:"open_jti"`
	LatestGrant     []byte `json:"latest_grant"`
	Trigger         string `json:"trigger,omitempty"`
}

// HubStreamRecheckResponse has one result per item, in item order.
type HubStreamRecheckResponse struct {
	Results []HubStreamRecheckResult `json:"results"`
}

// HubStreamRecheckResult is one stream's re-check: Renew (send
// RenewalGrant to the target unchanged as AuthRefresh{target stream id};
// AuthzUntil is the new deadline), Close (close the stream with CloseCode),
// or neither (not evaluated: nothing is renewed and the stream ends at its
// deadline unless a later re-check renews it).
type HubStreamRecheckResult struct {
	Renew        bool      `json:"renew,omitempty"`
	RenewalGrant []byte    `json:"renewal_grant,omitempty"`
	AuthzUntil   time.Time `json:"authz_until,omitzero"`
	Close        bool      `json:"close,omitempty"`
	CloseCode    uint32    `json:"close_code,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}
