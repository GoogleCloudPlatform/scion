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
	"io"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// Drain through the proxy (design §3.9 "Drain through the proxy", v2.21.53;
// contracts "Addition (6.2 (a3))"): on SIGTERM a hub ingress asks each live
// relay to send GoAway to the sessions it proxied. The relay holds each
// session's ingress instance in memory (Principal.IngressInstance), set at
// admission; there is no registry column.
//
// A drain only shortens a session's remaining life. Its deadline is
// clamped to the relay's own drain deadline and then to the session's
// lifetime cap; a session already draining keeps its deadline (GoAway is
// sent once per session) and is not counted; no cap is changed.

// internalDrainPath is the relay internal API route of a drain.
const internalDrainPath = InternalPathPrefix + "drain"

// maxDrainBody bounds a drain request body.
const maxDrainBody = 4 << 10

// maxDrainRequestDeadline saturates a requested drain deadline before the
// clamp (it only prevents overflow; the relay's own deadline is lower).
const maxDrainRequestDeadline = 24 * time.Hour

// DrainRequest is the body of POST /internal/v1/conduit/drain.
type DrainRequest struct {
	// IngressInstance names the hub instance whose proxied sessions are
	// drained. It must equal the calling relay-peer's identity.
	IngressInstance string `json:"ingress_instance"`
	// DrainDeadlineMs is the requested drain deadline in milliseconds (0
	// or absent: the relay's own drain deadline).
	DrainDeadlineMs int64 `json:"drain_deadline_ms,omitempty"`
}

// DrainResponse is the answer to a drain: how many sessions were sent GoAway.
type DrainResponse struct {
	GoAwaySent int `json:"goaway_sent"`
}

// DrainIngress sends GoAway to the live sessions this relay holds whose
// dial was proxied by the ingress instance, and returns how many were
// sent it. deadline is the requested drain deadline: 0 or less, or more
// than the relay's own drain deadline (Config.Session.DrainDeadline, the
// session default when unset), gives the relay's own; it is at least
// 1ms. Each session's deadline is then cut so that it never ends after
// the session's lifetime cap. A session already draining is skipped and
// not counted. The GoAways are sent first; the session rows are marked
// draining afterwards, in the background and bounded, so a slow registry
// never holds the answer.
func (r *Relay) DrainIngress(instance string, deadline time.Duration) int {
	if instance == "" {
		return 0
	}
	own := r.cfg.Session.DrainDeadline
	if own <= 0 {
		own = conduit.DefaultDrainDeadline
	}
	if deadline <= 0 || deadline > own {
		deadline = own
	}
	deadline = max(deadline, minDrainDeadline)
	r.mu.Lock()
	entries := r.snapshotLocked()
	r.mu.Unlock()
	var sent []*entry
	for _, e := range entries {
		if e.principal.IngressInstance != instance || e.sess == nil {
			continue
		}
		select {
		case <-e.sess.Done():
			continue
		default:
		}
		if e.sess.Info().Draining {
			continue // a drain is already under way; its deadline stands
		}
		d := min(deadline, r.drainDeadlineFor(e))
		err := e.sess.GoAway(conduit.GoAwayOptions{
			Reason:         ReasonDraining,
			CloseReason:    ReasonRelayRestart,
			ReconnectAfter: r.reconnectAfter(),
			DrainDeadline:  d,
		})
		if err == nil {
			sent = append(sent, e)
		}
	}
	if len(sent) > 0 {
		r.log.Info("Conduit relay: drained the sessions of a terminating ingress", "ingress_instance", instance, "sessions", len(sent))
		r.startMarkIngressDrained(sent)
	}
	return len(sent)
}

// startMarkIngressDrained starts markIngressDrained in the background,
// tracked by r.wg so Shutdown waits for it, as scheduleTouch does; once the
// relay is stopped or killed the pass is skipped (no store use after
// Shutdown).
func (r *Relay) startMarkIngressDrained(entries []*entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateStopped || r.killed {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.markIngressDrained(entries)
	}()
}

// markIngressDrained marks the rows of drained sessions draining (routing
// stops choosing them), bounded by one deleteTimeout for the whole batch.
func (r *Relay) markIngressDrained(entries []*entry) {
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	for _, e := range entries {
		if err := r.markSessionDraining(ctx, e); err != nil {
			r.log.Warn("Conduit relay: marking a drained session draining failed", "session_id", e.rec.SessionID, "error", err)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// serveDrain is POST /internal/v1/conduit/drain, after AuthenticatePeer.
// The body must match its signed digest (the relay-peer MAC covers
// X-Conduit-Body-SHA256), and its ingress_instance must be the calling
// peer: a hub drains only the sessions it proxied.
func (r *Relay) serveDrain(w http.ResponseWriter, req *http.Request, peer string) {
	if req.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxDrainBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "drain request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(body)
	if req.Header.Get(HeaderBodySHA256) != hex.EncodeToString(sum[:]) {
		http.Error(w, "body digest mismatch", http.StatusBadRequest)
		return
	}
	var dr DrainRequest
	if err := json.Unmarshal(body, &dr); err != nil {
		http.Error(w, "malformed drain request", http.StatusBadRequest)
		return
	}
	if dr.IngressInstance == "" || dr.IngressInstance != peer {
		http.Error(w, "ingress_instance is not the authenticated peer", http.StatusForbidden)
		return
	}
	n := r.DrainIngress(dr.IngressInstance, drainRequestDeadline(dr.DrainDeadlineMs))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(DrainResponse{GoAwaySent: n})
}

// drainRequestDeadline converts a requested drain_deadline_ms (0 or less:
// none) to a duration, saturating instead of overflowing; DrainIngress
// clamps it.
func drainRequestDeadline(ms int64) time.Duration {
	if ms <= 0 {
		return 0
	}
	if ms > int64(maxDrainRequestDeadline/time.Millisecond) {
		return maxDrainRequestDeadline
	}
	return time.Duration(ms) * time.Millisecond
}
