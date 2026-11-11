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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	// HTTP performs the calls (nil: a hop client with the system roots,
	// NewHopHTTPClientWithTimeout). Whatever it is, it is used without
	// redirects (hopClientWithoutRedirects).
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
	// The relay internal hop rule, before anything is sent: the hub
	// internal API carries user and agent credentials. Permanent, so it
	// is not marked ErrHubUnavailable.
	if err := config.CheckRelayTransport(c.BaseURL); err != nil {
		return nil, fmt.Errorf("conduit relay: refusing the hub internal API: %w", err)
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
	hc := defaultHopHTTPClient
	if c.HTTP != nil {
		hc = hopClientWithoutRedirects(c.HTTP)
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

// Ingress headers (design §3.9 "Ingress"; contracts §3). A hub in external
// relay mode sets both, and only on the relay-peer leg it proxies a conduit
// dial over, after stripping any copy the client sent. A relay trusts them
// only on that leg.
const (
	// HeaderIngressInstance names the hub instance the dial entered
	// through: the hub's instance id, at most MaxIngressInstanceLen bytes.
	HeaderIngressInstance = "X-Scion-Conduit-Ingress-Instance"
	// HeaderIngressLifetimeCap carries the hub's conduit lifetime cap in
	// whole seconds (decimal).
	HeaderIngressLifetimeCap = "X-Scion-Conduit-Ingress-Lifetime-Cap-S"
	// MaxIngressInstanceLen bounds HeaderIngressInstance and
	// HubAdmitRequest.IngressInstance.
	MaxIngressInstanceLen = 128
)

// How a dial reached the relay (HubAdmitRequest.Arrival).
const (
	// ArrivalDirect: the client dialed the relay's public listener.
	ArrivalDirect = "direct"
	// ArrivalIngress: a hub ingress proxied the dial over a relay-peer
	// leg that named the ingress instance.
	ArrivalIngress = "ingress"
)

// ReasonRelayNotAdmitting is the reason of the 503 a relay answers a conduit
// upgrade with while it is draining or not ready (design §3.9 "Relay
// selection", v2.21.51). It is sent only before Admit runs, so a hub
// ingress may re-pick another relay on it.
const ReasonRelayNotAdmitting = "relay_not_admitting"

// HubAdmitRequest asks the hub to admit a dialer the relay received
// (design §3.9 "Admission"). Headers are the dialer's credential headers
// (ForwardCredentialHeaders), passed unchanged, and Host and RawQuery are
// the ones the relay received, so a signed request (broker HMAC) verifies
// as sent. SessionID is the id the relay will admit the session under.
// Arrival is how the dial reached the relay (ArrivalDirect or
// ArrivalIngress; "" from an older relay means direct), and
// IngressInstance names the hub instance of an ingress arrival (empty for
// a direct one). The hub binds Host to the arrival.
type HubAdmitRequest struct {
	RelayInstanceID string              `json:"relay_instance_id"`
	RelayGeneration int64               `json:"relay_generation"`
	SessionID       string              `json:"session_id"`
	Host            string              `json:"host"`
	RawQuery        string              `json:"raw_query,omitempty"`
	Headers         map[string][]string `json:"headers"`
	Arrival         string              `json:"arrival,omitempty"`
	IngressInstance string              `json:"ingress_instance,omitempty"`
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

// forwardedCredentialHeaders is the fixed allowlist of dialer headers that
// cross from a relay to the hub's Admit (design §3.9 "Forwarded
// headers"): the credentials the hub's verifiers read and the run id an
// agent presents. Forwarding is default-deny: no other header crosses,
// whatever the request lists. Both the relay and the hub apply this list.
var forwardedCredentialHeaders = []string{
	"Authorization",
	"X-Scion-Agent-Token",
	"X-Scion-Run-Id",
	"X-Api-Key",
	// The IAP assertion is a signed JWT the hub verifies (signature,
	// issuer, audience, expiry); it is the one X-Goog-* header that crosses.
	"X-Goog-Iap-Jwt-Assertion",
	"X-Scion-Broker-Id",
	"X-Scion-Timestamp",
	"X-Scion-Nonce",
	"X-Scion-Signature",
	"X-Scion-Signed-Headers",
	"X-Scion-On-Behalf-Of",
}

// brokerSignedHeaders are the extra headers a broker request may list in
// X-Scion-Signed-Headers and have forwarded, so its signature verifies as
// sent. A request without X-Scion-Broker-Id gets no expansion; a listed
// name outside this set is not forwarded (the signature then fails).
var brokerSignedHeaders = map[string]bool{"Content-Type": true}

// neverForwardedHeader reports a header that never crosses to the hub
// whatever list names it: proxy identity and addressing headers, cookies
// and every X-Goog-* header but the signed IAP assertion.
func neverForwardedHeader(k string) bool {
	switch {
	case k == "X-Goog-Iap-Jwt-Assertion":
		return false
	case strings.HasPrefix(k, "X-Forwarded-"), strings.HasPrefix(k, "X-Goog-"):
		return true
	}
	switch k {
	case "Forwarded", "X-Real-Ip", "Cookie", "Host", "Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// ForwardCredentialHeaders returns the headers of h that cross to the hub's
// Admit: the allowlisted credential headers and, for a broker request
// only, the broker-signable headers its X-Scion-Signed-Headers lists.
func ForwardCredentialHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	add := func(k string) {
		if neverForwardedHeader(k) {
			return
		}
		if v, ok := h[k]; ok && len(v) > 0 {
			out[k] = append([]string(nil), v...)
		}
	}
	for _, k := range forwardedCredentialHeaders {
		add(k)
	}
	if h.Get("X-Scion-Broker-Id") == "" {
		return out
	}
	for _, list := range h.Values("X-Scion-Signed-Headers") {
		for _, name := range strings.Split(list, ";") {
			if k := http.CanonicalHeaderKey(strings.TrimSpace(name)); brokerSignedHeaders[k] {
				add(k)
			}
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
	// ExcludeTargets names target sessions an earlier attempt of the same
	// tunnel request found stale (the owner no longer held or admitted
	// them). The hub resolves the tunnel's target without them, re-checks
	// and mints a new grant for the session it picks.
	ExcludeTargets []string `json:"exclude_targets,omitempty"`
	// OpenTunnels is, on a tunnel request, how many tunnels the relay holds
	// open or is opening on the session, this request excluded. The hub
	// applies its per-session tunnel cap to it.
	OpenTunnels int `json:"open_tunnels,omitempty"`
	// ExcludeRelays names relay instances an earlier attempt of the same
	// tunnel request could not reach. The hub resolves the target without
	// any session they hold, as the router does within one resolution.
	ExcludeRelays []string `json:"exclude_relays,omitempty"`
	// Features lists what the forwarding relay supports. A relay that opens
	// the tunnels the hub authorizes lists HubFeatureTunnels; a relay that
	// does not (an older one) gets the tunnel request refused, never an
	// instruction it would ignore. A relay that also re-checks every tunnel
	// it opens lists HubFeatureTunnelRecheck.
	Features []string `json:"features,omitempty"`
}

// HubRPCResponse is the RPC's answer (the encoded RpcResponse).
type HubRPCResponse struct {
	RPC []byte `json:"rpc"`
	// Tunnel is set only when the hub accepted a tunnel request (POST
	// /v1/tunnels). The relay then opens the tunnel itself: the target
	// stream, the user stream, and the splice between them. RPC is the
	// answer to send once both legs are open, with the user stream's id
	// set under TunnelResponseStreamIDKey in its JSON body.
	Tunnel *HubTunnelOpen `json:"tunnel,omitempty"`
}

// HubFeatureTunnels is the HubRPCRequest feature of a relay that opens
// the tunnels the hub authorizes (HubRPCResponse.Tunnel).
const HubFeatureTunnels = "tunnels"

// HubFeatureTunnelRecheck is the HubRPCRequest feature of a relay that
// hands every tunnel it opens to its stream scheduler (hint and sweep
// re-checks; design v2.21.46 §3.9, "Relay feature tunnel_recheck"). The
// hub stamps a relay-held tunnel's grant for renewal only when the
// forwarding relay lists it.
const HubFeatureTunnelRecheck = "tunnel_recheck"

// TunnelsRPCPath is the RPC path of a tunnel request (POST), on the hub
// and on a relay-held session alike.
const TunnelsRPCPath = "/v1/tunnels"

// TunnelResponseStreamIDKey is the key of the user-side stream id in the
// JSON body of an accepted tunnel request's answer.
const TunnelResponseStreamIDKey = "streamId"

// HubTunnelOpen is the hub's instruction to open an authorized tunnel: the
// target session it resolved, the owner relay holding it, and the open
// grant it minted for exactly that session. The grant is passed to the
// target unchanged.
type HubTunnelOpen struct {
	TargetSessionID string `json:"target_session_id"`
	OwnerInstanceID string `json:"owner_instance_id"`
	OwnerGeneration int64  `json:"owner_generation"`
	OwnerEndpoint   string `json:"owner_endpoint"`
	// The Want the owner re-checks admission for (the stream kind is the
	// capability).
	WantProjectID   string `json:"want_project_id,omitempty"`
	WantExecScope   string `json:"want_exec_scope,omitempty"`
	WantIncarnation string `json:"want_incarnation"`

	Kind   string            `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
	Grant  []byte            `json:"grant"`
	// OpenJTI is the open grant's jti.
	OpenJTI string `json:"open_jti,omitempty"`
	// Unavailable is the encoded RpcResponse to answer with when the
	// tunnel cannot be opened (no target stream, or no user stream).
	Unavailable []byte `json:"unavailable"`
	// TargetRefusals are the encoded RpcResponses to answer with when the
	// target refuses the stream with one of these close codes (the same
	// mapping as a tunnel on a hub-held session: target not found, and
	// forbidden). Any other failure answers with Unavailable.
	TargetRefusals map[uint32][]byte `json:"target_refusals,omitempty"`

	// AuthzUntil (unix seconds) and AuthzIntervalS (seconds) are unsigned
	// scheduling hints (design §3.9 "Scheduling hints"): the hub copies them
	// from the claims of the open grant it just minted, never from a
	// request field, and sets both or neither (neither when the claims
	// carry no deadline). The relay uses them only to time its stream
	// re-checks and never trusts them for authorization; the target
	// enforces the signed values. A missing, partial or out-of-range hint
	// leaves the stream to the re-check sweep.
	AuthzUntil     int64 `json:"authz_until,omitempty"`
	AuthzIntervalS int64 `json:"authz_interval_s,omitempty"`
}

// Owner returns the relay holding the target session.
func (o *HubTunnelOpen) Owner() Owner {
	return Owner{InstanceID: o.OwnerInstanceID, Generation: o.OwnerGeneration, Endpoint: o.OwnerEndpoint}
}

// Want returns the Want the owner re-checks admission for.
func (o *HubTunnelOpen) Want() registry.Want {
	return registry.Want{ProjectID: o.WantProjectID, ExecScope: o.WantExecScope, Incarnation: o.WantIncarnation}
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
