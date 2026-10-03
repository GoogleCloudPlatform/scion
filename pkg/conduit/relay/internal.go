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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Internal API paths (design §3.5).
const (
	InternalPathPrefix = "/internal/v1/conduit/"
	internalSelfPath   = InternalPathPrefix + "self"
	internalSessions   = InternalPathPrefix + "sessions/"
)

// Response headers of the internal API.
const (
	// HeaderCloseCode carries a conduit close code when the owner could
	// not complete an RPC on the target session (502).
	HeaderCloseCode = "X-Conduit-Close-Code"
	// HeaderCloseReason carries the matching §3.3.1 close reason.
	HeaderCloseReason = "X-Conduit-Close-Reason"
	// HeaderStaleReason names why the owner refused a stale route
	// (404/409): a registry.Reason or "not_local".
	HeaderStaleReason = "X-Conduit-Stale-Reason"
)

// rpcEnvelopeSlack bounds the encoded RpcRequest beyond the body limit
// (method, path, query, headers).
const rpcEnvelopeSlack = 256 << 10

// wireWant is registry.Want on the wire (X-Conduit-Want).
type wireWant struct {
	ProjectID    string `json:"project_id,omitempty"`
	ExecScope    string `json:"exec_scope,omitempty"`
	AnyExecScope bool   `json:"any_exec_scope,omitempty"`
	Incarnation  string `json:"incarnation"`
	Capability   string `json:"capability,omitempty"`
}

func encodeWant(w registry.Want) string {
	b, _ := json.Marshal(wireWant(w))
	return string(b)
}

func decodeWant(s string) (registry.Want, error) {
	var w wireWant
	if s == "" {
		return registry.Want{}, errors.New("missing " + HeaderWant)
	}
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return registry.Want{}, fmt.Errorf("malformed %s: %w", HeaderWant, err)
	}
	return registry.Want(w), nil
}

// selfResponse answers GET /internal/v1/conduit/self.
type selfResponse struct {
	InstanceID string `json:"instance_id"`
	Generation int64  `json:"generation"`
}

// InternalHandler serves the internal relay API. It must be mounted only
// on the internal listener (--internal-listen), never on the public mux.
// Every request is authenticated with PeerAuth first; a request without a
// valid relay-peer identity gets 401 and nothing else is read.
func (r *Relay) InternalHandler() http.Handler {
	return http.HandlerFunc(r.serveInternal)
}

func (r *Relay) serveInternal(w http.ResponseWriter, req *http.Request) {
	if r.cfg.PeerAuth == nil {
		http.Error(w, "relay-peer auth not configured", http.StatusUnauthorized)
		return
	}
	peer, err := r.cfg.PeerAuth.Verify(req)
	if err != nil {
		http.Error(w, "relay-peer identity required", http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	killed := r.killed
	r.mu.Unlock()
	if killed {
		http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
		return
	}
	path := req.URL.Path
	if path == internalSelfPath {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(selfResponse{InstanceID: r.cfg.InstanceID, Generation: r.Generation()})
		return
	}
	rest, ok := strings.CutPrefix(path, internalSessions)
	if !ok {
		http.NotFound(w, req)
		return
	}
	sessionID, op, ok := strings.Cut(rest, "/")
	if !ok || sessionID == "" {
		http.NotFound(w, req)
		return
	}
	switch {
	case op == "rpc" && req.Method == http.MethodPost:
		r.serveRPC(w, req, peer, sessionID)
	case op == "stream" && req.Method == http.MethodGet:
		r.serveStream(w, req, peer, sessionID)
	case op == "rpc" || op == "stream":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, req)
	}
}

// admitInternal re-checks, on the owner, that the session is local and
// admissible for the caller's Want (end-to-end fencing: the caller's
// resolution may be stale). Stream requests must name the stream kind as
// Want.Capability. It writes the refusal and returns false.
func (r *Relay) admitInternal(w http.ResponseWriter, req *http.Request, sessionID string, stream bool) (conduit.LocalSession, registry.Want, bool) {
	want, err := decodeWant(req.Header.Get(HeaderWant))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, want, false
	}
	if stream && want.Capability == "" {
		http.Error(w, "stream requests must name the stream kind as the capability", http.StatusBadRequest)
		return nil, want, false
	}
	if !r.serving() {
		w.Header().Set(HeaderStaleReason, "relay_not_serving")
		http.Error(w, "relay not serving", http.StatusConflict)
		return nil, want, false
	}
	ls, rec, ok := r.Local(req.Context(), sessionID)
	if !ok {
		w.Header().Set(HeaderStaleReason, "not_local")
		http.Error(w, "session not held by this relay", http.StatusNotFound)
		return nil, want, false
	}
	if rec.PrincipalKind == registry.PrincipalUser {
		// User sessions are never a routing target (design §3.10).
		http.Error(w, "user sessions are not routable", http.StatusForbidden)
		return nil, want, false
	}
	d, err := r.cfg.Registry.Admission(req.Context(), sessionID, want)
	switch {
	case err != nil && d.Reason == registry.ReasonReadError:
		// Fail closed.
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return nil, want, false
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, want, false
	case !d.Admissible:
		w.Header().Set(HeaderStaleReason, string(d.Reason))
		http.Error(w, "session not admissible: "+string(d.Reason), http.StatusConflict)
		return nil, want, false
	}
	return ls, want, true
}

func (r *Relay) serveRPC(w http.ResponseWriter, req *http.Request, peer, sessionID string) {
	ls, _, ok := r.admitInternal(w, req, sessionID, false)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, conduit.MaxRPCBody+rpcEnvelopeSlack+1))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if len(body) > conduit.MaxRPCBody+rpcEnvelopeSlack {
		http.Error(w, "rpc envelope too large", http.StatusRequestEntityTooLarge)
		return
	}
	sum := sha256.Sum256(body)
	if req.Header.Get(HeaderBodySHA256) != hex.EncodeToString(sum[:]) {
		http.Error(w, "body digest mismatch", http.StatusBadRequest)
		return
	}
	rpc := &conduitv1.RpcRequest{}
	if err := proto.Unmarshal(body, rpc); err != nil {
		http.Error(w, "malformed rpc request", http.StatusBadRequest)
		return
	}
	// The caller's disconnect cancels req.Context(), which sends RpcCancel
	// to the target (design §3.5: ctx cancellation propagates).
	ctx, cancel := context.WithTimeout(req.Context(), r.cfg.RPCTimeout)
	defer cancel()
	resp, err := ls.Call(ctx, rpc)
	if err != nil {
		code, why := codeAndReason(err, "target session call failed")
		if errors.Is(err, context.DeadlineExceeded) {
			code, why = conduit.CloseRelayTimeout, reason(ReasonOpenTimeout, "rpc timeout")
		}
		r.log.Debug("Conduit internal RPC failed", "peer", peer, "session_id", sessionID, "error", err)
		w.Header().Set(HeaderCloseCode, strconv.FormatUint(uint64(code), 10))
		w.Header().Set(HeaderCloseReason, why)
		http.Error(w, "rpc failed", http.StatusBadGateway)
		return
	}
	out, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "encoding response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// serveStream bridges one stream: the first frame on the internal WS must
// be StreamOpen; the owner opens the stream on the local target session
// and splices the two framed legs. Cancellation during opening (the caller
// sends StreamClose or drops the WS) cancels the local OpenStream, which
// sends 4499 to the target; an accept that loses the race is closed 4499.
//
// The Want must name the stream kind as its Capability (RemoteSession sets
// it from the kind), and the opening frame's kind must equal it: the owner
// opens only what it re-checked admission for. Otherwise the hop is closed
// 4400 bad_frame.
func (r *Relay) serveStream(w http.ResponseWriter, req *http.Request, peer, sessionID string) {
	ls, want, ok := r.admitInternal(w, req, sessionID, true)
	if !ok {
		return
	}
	conn, err := ws.Upgrade(w, req, nil, ws.Options{})
	if err != nil {
		return // Upgrade wrote the error
	}
	r.bridges.Add(1)
	r.activeBridges.Add(1)
	defer func() {
		r.activeBridges.Add(-1)
		r.bridges.Done()
	}()
	open, err := readStreamOpen(r.clk, conn, r.cfg.Session.HandshakeTimeout)
	if err != nil {
		r.log.Debug("Conduit internal stream: bad opening frame", "peer", peer, "error", err)
		_ = conn.Close()
		return
	}
	callerWin := open.GetInitialWindow()
	if callerWin == 0 {
		callerWin = conduit.DefaultStreamWindow
	}
	hop := newWSStream(conn, callerWin, 0)
	if kind, err := conduit.StreamKindFromProto(open.GetKind()); err != nil || string(kind) != want.Capability {
		r.log.Debug("Conduit internal stream: kind does not match the admitted capability", "peer", peer, "kind", open.GetKind().String(), "capability", want.Capability)
		_ = hop.CloseWithCode(conduit.CloseProtocolError, reason(ReasonBadFrame, "stream kind does not match the admitted capability"))
		return
	}

	octx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-hop.Done(): // caller cancelled or link lost while opening
			cancel()
		case <-octx.Done():
		}
	}()
	fwd := &conduitv1.StreamOpen{
		Kind:          open.GetKind(),
		Params:        open.GetParams(),
		Grant:         open.GetGrant(),
		OpenTimeoutMs: open.GetOpenTimeoutMs(),
	}
	st, err := ls.OpenStream(octx, fwd)
	if err != nil {
		code, why := codeAndReason(err, "opening the target stream failed")
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, conduit.ErrStreamCancelled):
			code, why = conduit.CloseCancelled, ReasonCancelled
		case errors.Is(err, conduit.ErrDraining):
			code, why = conduit.CloseRelayRestart, ReasonDraining
		}
		_ = hop.CloseWithCode(code, why)
		return
	}
	if h := r.testHookAfterOpen; h != nil {
		h(hop)
	}
	if ended, _ := hop.endedErr(); ended {
		// Late accept: the caller is gone. Do not leak the target leg.
		_ = st.CloseWithCode(conduit.CloseCancelled, ReasonCancelled)
		return
	}
	if err := hop.grant(conduit.DefaultStreamWindow); err != nil {
		_ = st.CloseWithCode(conduit.CloseCancelled, ReasonCancelled)
		hop.end(errLinkLost, false)
		return
	}
	cancel() // stop the opening watcher; the stream is established
	splice(st, hop)
}

// readStreamOpen reads the first frame of an internal stream WS, bounded by
// timeout on clk (0 = conduit default handshake timeout).
func readStreamOpen(clk clock.Clock, conn interface {
	ReadFrame() ([]byte, error)
	Close() error
}, timeout time.Duration) (*conduitv1.StreamOpen, error) {
	if timeout <= 0 {
		timeout = conduit.DefaultHandshakeTimeout
	}
	t := clk.AfterFunc(timeout, func() { _ = conn.Close() })
	b, err := conn.ReadFrame()
	if !t.Stop() {
		return nil, errors.New("timed out waiting for StreamOpen")
	}
	if err != nil {
		return nil, err
	}
	f := &conduitv1.Frame{}
	if err := proto.Unmarshal(b, f); err != nil {
		return nil, err
	}
	open := f.GetStreamOpen()
	if open == nil {
		return nil, fmt.Errorf("first frame is %s, want stream_open", conduit.FrameType(f))
	}
	return open, nil
}
