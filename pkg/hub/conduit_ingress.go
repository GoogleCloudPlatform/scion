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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
)

// Conduit ingress in external relay mode (design §3.9 "Ingress" and "Relay
// selection"; contracts "Addition (6.2 (a3))").
//
// With server.hub.conduit.ingress = proxy, the hub reverse-proxies each
// client's GET /api/v1/conduit upgrade to the internal listener of a relay
// it picks, over a relay-peer leg. The conduit session runs end to end
// between the client and the relay; the hub is an ingress hop only. The
// client's credential headers, method, path, query and Host pass unchanged,
// so the relay's Admit sees them as sent. The hub strips every relay-peer
// and ingress header the client supplied, sets the ingress instance and
// lifetime cap headers, and signs the leg as relay-peer for the chosen
// relay's instance and generation.
//
// Relay selection samples two eligible relays and picks the one whose
// current generation holds fewer sessions; when a count fails it picks one
// of the two at random. A dial is re-picked, within router.MaxReResolves,
// only when it failed before the relay could have run Admit: a transport
// error before the request was written, 409 relay_target_mismatch, or 503
// relay_not_admitting (which a stopped relay also answers on the ingress
// route). Every other answer, and a transport error after the request was
// written, goes to the client unchanged (502 for the error), since a second
// Admit would replay a broker's request nonce.

// ConduitIngressCauseRequiresHMAC is the cause that keeps the ingress
// proxy off when the resolved relay-peer auth mode is not hmac: the OIDC
// modes carry the peer token in Authorization, which on the proxy leg is
// the client's own credential, until the relay-peer ingress header lands
// (design §3.9, v2.21.52).
const ConduitIngressCauseRequiresHMAC = "ingress_requires_hmac"

// conduitIngressPath is the path a relay serves proxied dials on (its
// internal listener) and the hub's own conduit path.
const conduitIngressPath = "/api/v1/conduit"

// conduitIngressRefusalBodyMax bounds the relay refusal body read to find
// its reason.
const conduitIngressRefusalBodyMax = 4 << 10

// conduitIngressDialTimeout bounds connecting to a relay's internal
// listener; conduitIngressTLSHandshakeTimeout bounds its TLS handshake.
const (
	conduitIngressDialTimeout         = 5 * time.Second
	conduitIngressTLSHandshakeTimeout = 10 * time.Second
)

// conduitIngressResponseHeaderTimeout bounds the wait for a relay's answer
// to a proxied dial: the relay answers after its admission call to the hub,
// which relay.DefaultHubCallTimeout bounds, plus a margin. It bounds only
// the response headers, never an upgraded session.
const conduitIngressResponseHeaderTimeout = relay.DefaultHubCallTimeout + 5*time.Second

// errConduitIngressRepick marks a relay answer the hub may re-pick on.
var errConduitIngressRepick = errors.New("conduit ingress: the relay refused before admission")

// conduitIngressStrippedHeader reports whether a client-supplied header is
// removed before the dial is proxied: every relay-peer header (the
// X-Conduit- prefix covers the peer signature, target, body hash and Want
// headers, and any added later), the relay-peer token header, both ingress
// headers, and the forwarding headers.
func conduitIngressStrippedHeader(k string) bool {
	k = http.CanonicalHeaderKey(k)
	for _, p := range []string{"X-Conduit-", "X-Scion-Conduit-Ingress-", "X-Scion-Peer-", "X-Scion-Relay-Peer-", "X-Forwarded-"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return k == "Forwarded"
}

// serveConduitIngress proxies a conduit dial to a relay picked by
// selection, re-picking only on failures before admission. With no relay
// left it answers 503 conduit_not_serving.
func (s *Server) serveConduitIngress(w http.ResponseWriter, r *http.Request, rt *conduitExternalRuntime) {
	if !isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "WebSocket upgrade required", nil)
		return
	}
	// The dial counts as an open proxied leg from here until it returns,
	// upgraded session included, so the shutdown's drain step can hold it
	// and close it at its deadline (cancelling the leg's context closes
	// both connections). It is counted before terminating is checked, so a
	// leg the drain step does not see always sees terminating.
	legCtx, closeLeg := context.WithCancel(r.Context())
	defer closeLeg()
	defer rt.legs.done(rt.legs.add(closeLeg))
	r = r.WithContext(legCtx)
	excluded := map[string]bool{}
	for attempt := 0; attempt <= router.MaxReResolves; attempt++ {
		if rt.terminating.Load() {
			// The shutdown stopped proxying while this dial was being
			// placed: no further byte goes to a relay (cause
			// ingress_terminating).
			break
		}
		target, ok := s.pickConduitIngressRelay(r.Context(), rt, excluded)
		if !ok {
			break
		}
		if !s.proxyConduitDial(w, r, rt, target) {
			return
		}
		if rt.terminating.Load() {
			break // refused below with cause ingress_terminating, no re-pick
		}
		excluded[target.InstanceID] = true
		slog.Info("Conduit ingress: re-picking a relay", "relay_instance_id", target.InstanceID, "attempt", attempt+1)
	}
	s.refuseConduitExternal(w, r, rt)
}

// pickConduitIngressRelay picks a relay for a new conduit session: two
// eligible relays not in excluded, sampled at random, then the one whose
// current generation holds fewer sessions (a tie is random). When a count
// fails, one of the two is picked at random and the failure is logged and
// counted. It reports false when no eligible relay is left.
func (s *Server) pickConduitIngressRelay(ctx context.Context, rt *conduitExternalRuntime, excluded map[string]bool) (registry.RelayInstance, bool) {
	all, err := rt.registry.EligibleRelays(ctx, registry.TransportWS, rt.now())
	if err != nil {
		slog.Warn("Conduit ingress: listing eligible relays failed", "error", err)
		return registry.RelayInstance{}, false
	}
	var candidates []registry.RelayInstance
	for _, ri := range all {
		if !excluded[ri.InstanceID] {
			candidates = append(candidates, ri)
		}
	}
	if len(candidates) == 0 {
		return registry.RelayInstance{}, false
	}
	sample := sampleConduitIngressRelays(candidates)
	if hook := s.conduitIngressSampleTestHook; hook != nil {
		sample = hook(candidates)
	}
	if len(sample) == 1 {
		return sample[0], true
	}
	a, b := sample[0], sample[1]
	na, errA := rt.store.CountRelaySessions(ctx, a.InstanceID, a.Generation)
	nb, errB := rt.store.CountRelaySessions(ctx, b.InstanceID, b.Generation)
	if err := errors.Join(errA, errB); err != nil {
		n := rt.ingressCountFailures.Add(1)
		slog.Warn("Conduit ingress: counting relay sessions failed; picking one of the two at random",
			"relays", []string{a.InstanceID, b.InstanceID}, "failures", n, "error", err)
		return sample[rand.IntN(2)], true
	}
	switch {
	case na < nb:
		return a, true
	case nb < na:
		return b, true
	}
	return sample[rand.IntN(2)], true
}

// sampleConduitIngressRelays returns two distinct candidates at random, or
// the only one.
func sampleConduitIngressRelays(candidates []registry.RelayInstance) []registry.RelayInstance {
	if len(candidates) == 1 {
		return candidates[:1]
	}
	p := rand.Perm(len(candidates))
	return []registry.RelayInstance{candidates[p[0]], candidates[p[1]]}
}

// newConduitIngressTransport returns the transport for proxied dials: a
// clone of base (nil: http.DefaultTransport) with a dial timeout, a TLS
// handshake timeout when base has none, headerTimeout as the response
// header timeout, and keep-alives off, so the transport never sends a dial
// again on its own over a reused connection. A base that is not an
// *http.Transport is returned unchanged and keeps its own bounds.
func newConduitIngressTransport(base http.RoundTripper, headerTimeout time.Duration) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	t, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	t = t.Clone()
	dial := t.DialContext
	if dial == nil {
		dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, conduitIngressDialTimeout)
		defer cancel()
		return dial(ctx, network, addr)
	}
	if t.TLSHandshakeTimeout <= 0 {
		t.TLSHandshakeTimeout = conduitIngressTLSHandshakeTimeout
	}
	t.ResponseHeaderTimeout = headerTimeout
	t.DisableKeepAlives = true
	return t
}

// conduitIngressRepickResponse reports whether a relay's answer to a
// proxied dial was given before admission: 409 relay_target_mismatch (the
// relay-peer check) or 503 relay_not_admitting (draining, not ready, or
// stopped). It reads at most conduitIngressRefusalBodyMax
// bytes of a 503 body; the body of an answer that is not re-picked is
// restored for the client.
func conduitIngressRepickResponse(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusConflict:
		return resp.Header.Get(relay.HeaderStaleReason) == "relay_target_mismatch"
	case http.StatusServiceUnavailable:
		body, err := io.ReadAll(io.LimitReader(resp.Body, conduitIngressRefusalBodyMax))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(strings.NewReader(string(body)), resp.Body), resp.Body}
		if err != nil {
			return false
		}
		var refusal struct {
			Error struct {
				Details struct {
					Reason string `json:"reason"`
				} `json:"details"`
			} `json:"error"`
		}
		return json.Unmarshal(body, &refusal) == nil && refusal.Error.Details.Reason == relay.ReasonRelayNotAdmitting
	}
	return false
}

// conduitIngressTransport records whether the round trip itself failed
// (no response at all), as opposed to a failure after a response arrived,
// and whether the request may have reached the relay. wrote is set at the
// first request header field the transport writes, which precedes the end
// of the request headers on the wire, so a relay cannot have run Admit for
// a request that leaves it unset.
type conduitIngressTransport struct {
	base   http.RoundTripper
	failed bool
	wrote  atomic.Bool
}

func (t *conduitIngressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	trace := &httptrace.ClientTrace{WroteHeaderField: func(string, []string) { t.wrote.Store(true) }}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.failed = true
	}
	return resp, err
}

// proxyConduitDial proxies r to target's internal listener over a
// relay-peer leg. It returns true, with nothing written to the client, when
// the dial failed before the relay could have run Admit, so another relay
// may be picked; otherwise the relay's answer (or the session, or a 502
// when the leg failed after the request was written) went to the client.
func (s *Server) proxyConduitDial(w http.ResponseWriter, r *http.Request, rt *conduitExternalRuntime, target registry.RelayInstance) (repick bool) {
	endpoint, err := url.Parse(target.InternalEndpoint)
	if err != nil || endpoint.Host == "" {
		slog.Warn("Conduit ingress: the relay's internal endpoint is unusable", "relay_instance_id", target.InstanceID, "error", err)
		return true
	}
	// The proxied session outlives the request: lift the server's read and
	// write deadlines, which a hijacked connection keeps.
	rc := http.NewResponseController(w)
	if err := errors.Join(rc.SetReadDeadline(time.Time{}), rc.SetWriteDeadline(time.Time{})); err != nil {
		slog.Debug("Conduit ingress: cannot lift the connection deadlines", "error", err)
	}
	transport := &conduitIngressTransport{base: rt.ingressTransport}
	var signErr error
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = endpoint.Scheme
			pr.Out.URL.Host = endpoint.Host
			pr.Out.URL.Path = conduitIngressPath
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = pr.In.Host
			for k := range pr.Out.Header {
				if conduitIngressStrippedHeader(k) {
					delete(pr.Out.Header, k)
				}
			}
			pr.Out.Header.Set(relay.HeaderIngressInstance, rt.instanceID)
			pr.Out.Header.Set(relay.HeaderIngressLifetimeCap, strconv.FormatInt(int64(rt.lifetimeCap/time.Second), 10))
			relay.SetPeerTarget(pr.Out, target.InstanceID, target.Generation)
			if rt.terminating.Load() {
				// Proxying stopped after the relay was picked: the leg
				// is never sent (the empty host fails the round trip before
				// any byte, and the caller refuses the dial).
				pr.Out.URL.Host = ""
				return
			}
			if err := rt.peerAuth.Sign(pr.Out); err != nil {
				signErr = err
				// An unsigned leg is never sent: the empty host fails the
				// round trip.
				pr.Out.URL.Host = ""
			}
		},
		Transport: transport,
		ModifyResponse: func(resp *http.Response) error {
			if conduitIngressRepickResponse(resp) {
				return errConduitIngressRepick
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, errConduitIngressRepick) || (transport.failed && !transport.wrote.Load()) {
				repick = true
				if signErr != nil {
					slog.Error("Conduit ingress: signing the relay-peer leg failed", "error", signErr)
				}
				return
			}
			if rt.terminating.Load() {
				// The drain step closed the leg (or the shutdown cut it
				// short): expected, not a relay fault.
				slog.Debug("Conduit ingress: proxied dial ended while shutting down", "relay_instance_id", target.InstanceID, "cause", conduitIngressCauseTerminating, "error", err)
			} else {
				slog.Warn("Conduit ingress: proxying the dial failed", "relay_instance_id", target.InstanceID, "error", err)
			}
			writeError(w, http.StatusBadGateway, ErrCodeRuntimeError, "Conduit relay connection failed", nil)
		},
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
	rp.ServeHTTP(w, r)
	return repick
}
