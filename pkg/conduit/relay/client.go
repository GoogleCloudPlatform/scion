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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// ErrStaleRoute means the owning relay no longer holds the session, or the
// session is no longer admissible for the Want (404/409 from the internal
// API, or the session moved). The router re-resolves.
var ErrStaleRoute = errors.New("conduit relay: stale route")

// StaleRouteError carries the owner's reason.
type StaleRouteError struct{ Reason string }

func (e *StaleRouteError) Error() string { return "conduit relay: stale route: " + e.Reason }

// Unwrap makes errors.Is(err, ErrStaleRoute) true.
func (e *StaleRouteError) Unwrap() error { return ErrStaleRoute }

// PeerClient calls other relays' internal APIs. It is the injectable seam
// for tests (two relays over httptest).
type PeerClient struct {
	// HTTP performs internal RPCs and probes. Its Timeout must be zero or
	// above the RPC cap; per-call deadlines come from the context.
	HTTP *http.Client
	// WS dials internal stream WebSockets (nil: gorilla defaults).
	WS *websocket.Dialer
	// Auth signs every request.
	Auth PeerAuth
	// RPCTimeout caps one internal RPC (default 120s).
	RPCTimeout time.Duration
}

func (c *PeerClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *PeerClient) sign(req *http.Request) error {
	if c.Auth == nil {
		return errors.New("conduit relay: no relay-peer auth configured")
	}
	return c.Auth.Sign(req)
}

// Probe is GET {endpoint}/internal/v1/conduit/self, signed for the relay
// targetID at generation targetGen. It returns the answering instance id.
func (c *PeerClient) Probe(ctx context.Context, endpoint, targetID string, targetGen int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+internalSelfPath, nil)
	if err != nil {
		return "", err
	}
	SetPeerTarget(req, targetID, targetGen)
	if err := c.sign(req); err != nil {
		return "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict && resp.Header.Get(HeaderStaleReason) == "relay_target_mismatch" {
		// Another relay, or another generation, answered at endpoint.
		return "", fmt.Errorf("self probe: %w: answered by a relay other than %s generation %d", registry.ErrSelfCheckMismatch, targetID, targetGen)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("self probe: HTTP %d", resp.StatusCode)
	}
	var sr selfResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&sr); err != nil {
		return "", fmt.Errorf("self probe: %w", err)
	}
	return sr.InstanceID, nil
}

// probe is the relay's self-check probe (registry.ProbeFunc). It runs
// before RegisterRelay, so it targets this instance at its current,
// not yet registered, generation.
func (r *Relay) probe(ctx context.Context, endpoint string) (string, error) {
	return r.peerClient().Probe(ctx, endpoint, r.cfg.InstanceID, r.Generation())
}

func (r *Relay) peerClient() *PeerClient {
	return &PeerClient{HTTP: r.cfg.HTTPClient, Auth: r.cfg.PeerAuth, RPCTimeout: r.cfg.RPCTimeout}
}

// RemoteSession is a conduit.Session held by another relay, reached over
// that relay's internal API. It is cheap: it holds no connection, and every
// call re-checks admission on the owner with Want.
type RemoteSession struct {
	client   *PeerClient
	endpoint string
	rec      registry.SessionRecord
	want     registry.Want
}

var _ conduit.Session = (*RemoteSession)(nil)

// NewRemoteSession returns a session handle for rec on the relay at
// endpoint (its registered internal endpoint).
func NewRemoteSession(client *PeerClient, endpoint string, rec registry.SessionRecord, want registry.Want) *RemoteSession {
	return &RemoteSession{client: client, endpoint: strings.TrimRight(endpoint, "/"), rec: rec, want: want}
}

func (s *RemoteSession) sessionURL(op string) string {
	return s.endpoint + internalSessions + url.PathEscape(s.rec.SessionID) + "/" + op
}

// Call implements conduit.Session: POST …/sessions/{id}/rpc, capped at the
// RPC timeout; cancelling ctx aborts the HTTP request, which cancels the
// owner's call (RpcCancel to the target).
func (s *RemoteSession) Call(ctx context.Context, rpc *conduitv1.RpcRequest) (*conduitv1.RpcResponse, error) {
	if len(rpc.GetBody()) > conduit.MaxRPCBody {
		// Same contract as a local session: answered locally, the caller
		// falls back to direct HTTP.
		return &conduitv1.RpcResponse{RequestId: rpc.GetRequestId(), Status: http.StatusRequestEntityTooLarge}, nil
	}
	timeout := s.client.RPCTimeout
	if timeout <= 0 {
		timeout = DefaultRPCTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := proto.Marshal(rpc)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.sessionURL("rpc"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set(HeaderBodySHA256, hex.EncodeToString(sum[:]))
	req.Header.Set(HeaderWant, encodeWant(s.want))
	SetPeerTarget(req, s.rec.RelayInstanceID, s.rec.RelayGeneration)
	if err := s.client.sign(req); err != nil {
		return nil, err
	}
	resp, err := s.client.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The owner is unreachable: treat like a lost link.
		return nil, closeErr(conduit.CloseRelayTimeout, ReasonUpstreamUnreachable, "owner relay unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusErr(resp); err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, conduit.MaxRPCBody+rpcEnvelopeSlack+1))
	if err != nil {
		return nil, closeErr(conduit.CloseRelayTimeout, ReasonUpstreamUnreachable, "owner relay response truncated")
	}
	rr := &conduitv1.RpcResponse{}
	if err := proto.Unmarshal(out, rr); err != nil {
		return nil, fmt.Errorf("conduit relay: malformed rpc response: %w", err)
	}
	return rr, nil
}

// statusErr maps an internal API status to an error (nil for 200).
func statusErr(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusSwitchingProtocols:
		return nil
	case http.StatusNotFound, http.StatusConflict:
		return &StaleRouteError{Reason: resp.Header.Get(HeaderStaleReason)}
	case http.StatusUnauthorized:
		return errors.New("conduit relay: owner refused relay-peer identity (401)")
	case http.StatusServiceUnavailable:
		return closeErr(conduit.CloseRelayTimeout, ReasonUpstreamUnreachable, "owner relay unavailable")
	case http.StatusBadGateway:
		code, why := conduit.CloseRelayTimeout, reason(ReasonUpstreamUnreachable, "target session call failed")
		if v, err := strconv.ParseUint(resp.Header.Get(HeaderCloseCode), 10, 32); err == nil && v != 0 {
			code, why = uint32(v), ""
			if hr := resp.Header.Get(HeaderCloseReason); hr != "" {
				why = reason(hr, "")
			}
		}
		return &conduit.CloseError{Code: code, Reason: why}
	default:
		return fmt.Errorf("conduit relay: internal API: HTTP %d", resp.StatusCode)
	}
}

// OpenStream implements conduit.Session: one internal WebSocket per
// stream. The first frame is StreamOpen; the owner answers StreamAccept or
// StreamClose. Cancelling ctx or reaching open_timeout_ms while opening
// sends StreamClose{4499} and drops the WS, so the owner cancels its leg
// (4499 to the target) — both legs see 4499.
func (s *RemoteSession) OpenStream(ctx context.Context, open *conduitv1.StreamOpen) (conduit.Stream, error) {
	kind, err := conduit.StreamKindFromProto(open.GetKind())
	if err != nil {
		return nil, closeErr(conduit.CloseProtocolError, ReasonBadFrame, err.Error())
	}
	// The owner opens only the kind it re-checked admission for.
	want := s.want
	want.Capability = string(kind)
	u := s.sessionURL("stream")
	wsURL := "ws" + strings.TrimPrefix(u, "http")
	sign := func(ctx context.Context) (http.Header, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set(HeaderWant, encodeWant(want))
		SetPeerTarget(req, s.rec.RelayInstanceID, s.rec.RelayGeneration)
		if err := s.client.sign(req); err != nil {
			return nil, err
		}
		return req.Header, nil
	}
	timeout := conduit.DefaultOpenTimeout
	if ms := open.GetOpenTimeoutMs(); ms > 0 {
		timeout = min(time.Duration(ms)*time.Millisecond, conduit.MaxOpenTimeout)
	}
	octx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	d := &ws.Dialer{URL: wsURL, Header: sign, WS: s.client.WS}
	conn, err := d.Dial(octx)
	if err != nil {
		var de *ws.DialError
		if errors.As(err, &de) {
			switch de.StatusCode {
			case http.StatusNotFound, http.StatusConflict:
				return nil, &StaleRouteError{Reason: fmt.Sprintf("HTTP %d", de.StatusCode)}
			case http.StatusServiceUnavailable:
				return nil, closeErr(conduit.CloseRelayTimeout, ReasonUpstreamUnreachable, "owner relay unavailable")
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("conduit relay: dialing owner relay: %w", err)
	}
	win := open.GetInitialWindow()
	if win == 0 {
		win = conduit.DefaultStreamWindow
	}
	first := &conduitv1.StreamOpen{
		StreamId:      hopStreamID,
		Kind:          open.GetKind(),
		Params:        open.GetParams(),
		Grant:         open.GetGrant(),
		InitialWindow: win,
		OpenTimeoutMs: uint32(timeout / time.Millisecond),
	}
	b, err := proto.Marshal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: first}})
	if err == nil {
		err = conn.WriteFrame(b)
	}
	if err != nil {
		_ = conn.Close()
		return nil, closeErr(conduit.CloseRelayTimeout, ReasonUpstreamUnreachable, "owner relay link failed")
	}

	type result struct {
		f   *conduitv1.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := conn.ReadFrame()
		if err != nil {
			ch <- result{err: err}
			return
		}
		f := &conduitv1.Frame{}
		ch <- result{f: f, err: proto.Unmarshal(b, f)}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			_ = conn.Close()
			return nil, errLinkLost
		}
		switch body := res.f.GetBody().(type) {
		case *conduitv1.Frame_StreamAccept:
			return newWSStream(conn, body.StreamAccept.GetInitialWindow(), win), nil
		case *conduitv1.Frame_StreamClose:
			_ = conn.Close()
			return nil, &conduit.CloseError{Code: body.StreamClose.GetCode(), Reason: body.StreamClose.GetReason()}
		default:
			_ = conn.Close()
			return nil, closeErr(conduit.CloseProtocolError, ReasonBadFrame, "unexpected frame while opening")
		}
	case <-octx.Done():
		// Cancel during opening: tell the owner, then drop the link. The
		// reader goroutine ends when the conn closes.
		if b, err := proto.Marshal(closeFrame(conduit.CloseCancelled, ReasonCancelled)); err == nil {
			_ = conn.WriteFrame(b)
		}
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("conduit: remote stream: open timeout after %v: %w", timeout, context.DeadlineExceeded)
	}
}

// Info implements conduit.Session from the registry record.
func (s *RemoteSession) Info() conduit.SessionInfo {
	c := s.rec.Capabilities
	return conduit.SessionInfo{
		SessionID:           s.rec.SessionID,
		RelayInstanceID:     s.rec.RelayInstanceID,
		Transport:           s.rec.Transport,
		PrincipalKind:       s.rec.PrincipalKind,
		PrincipalID:         s.rec.PrincipalID,
		EndpointIncarnation: s.rec.EndpointIncarnation,
		ExecScope:           s.rec.ExecScope,
		ConnectionEpoch:     s.rec.ConnectionEpoch,
		ConnectedAt:         s.rec.ConnectedAt,
		Draining:            s.rec.Draining,
		Capabilities: &conduitv1.Capabilities{
			StreamKinds:         append([]string(nil), c.StreamKinds...),
			Rpc:                 append([]string(nil), c.RPC...),
			EndpointIncarnation: c.EndpointIncarnation,
			ExecScope:           c.ExecScope,
			TransportLimits: &conduitv1.TransportLimits{
				MaxFrame:     uint32(c.TransportLimits.MaxFrame),
				IdleTimeoutS: uint32(c.TransportLimits.IdleTimeoutS),
			},
		},
	}
}

// Close implements conduit.Session. A remote handle owns no connection.
func (s *RemoteSession) Close() error { return nil }

// Record returns the registry record the handle was resolved from.
func (s *RemoteSession) Record() registry.SessionRecord { return s.rec }
