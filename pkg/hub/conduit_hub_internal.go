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
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// The hub internal API (external relay mode, design §3.9): the operations
// separate relays call on any hub replica. It is mounted only on the
// internal listener (ConduitInternalHandler), never on the public mux.
//
// Every request passes, in order:
//
//  1. relay-peer authentication (the PeerAuth keyed from peer_secret):
//     401 on failure, including a timestamp outside the window or a nonce
//     this replica has already seen;
//  2. the target: HubPeerTarget, generation 0 (409 relay_target_mismatch
//     otherwise, as relays answer a request meant for another relay);
//  3. the shared replay store: the nonce is recorded once across every hub
//     replica (401 when it was already recorded, 503 when the store cannot
//     be reached: never accepted without the check);
//  4. the body digest (400 on a mismatch).
//
// Only then is the operation dispatched. Operations register themselves in
// hubInternalOps.

// relayPeerReplayTTL is how long a relay-peer nonce stays recorded in the
// shared store: twice the verifier's timestamp window, as the in-memory
// cache keeps it (a request outside the window is refused anyway).
const relayPeerReplayTTL = 2 * relay.DefaultPeerMaxSkew

// relayPeerReplayStore records a relay-peer nonce once across hub replicas
// (NonceCacheStore in production).
type relayPeerReplayStore interface {
	CheckAndStore(ctx context.Context, nonce string, ttl time.Duration) (bool, error)
}

// hubInternalOp serves one hub internal API operation. body is the
// verified request body; peer is the authenticated relay.
type hubInternalOp func(s *Server, w http.ResponseWriter, r *http.Request, peer string, body []byte)

// hubInternalOps maps an exact path to its operation. Session-scoped
// operations (rpc, refresh) are matched by hubInternalSessionOp.
var hubInternalOps = map[string]hubInternalOp{}

// hubInternalSessionOps maps the op of a sessions/{id}/{op} path.
var hubInternalSessionOps = map[string]func(s *Server, w http.ResponseWriter, r *http.Request, peer, sessionID string, body []byte){}

// relayPeerReplayKey is the shared-store key of a relay-peer nonce:
// "relay-peer:" and the hex SHA-256 of the peer id and nonce. It is
// printable (a text column on every database accepts it), of fixed length,
// and the prefix keeps relay-peer nonces apart from the broker HMAC nonces
// in the same table.
func relayPeerReplayKey(peer, nonce string) string {
	sum := sha256.Sum256([]byte(peer + "\x00" + nonce))
	return "relay-peer:" + hex.EncodeToString(sum[:])
}

// replayStoreFor returns the shared replay store (nil when none can be
// built: no database).
func (s *Server) relayPeerReplayStoreFor(rt *conduitExternalRuntime) relayPeerReplayStore {
	if rt.replay != nil {
		return rt.replay
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.nonceCacheStore != nil {
		return s.nonceCacheStore
	}
	if s.entClient != nil {
		return NewNonceCacheStore(s.entClient)
	}
	return nil
}

// serveHubInternal serves the hub internal API in external relay mode.
func (s *Server) serveHubInternal(w http.ResponseWriter, r *http.Request, rt *conduitExternalRuntime) {
	peer, err := rt.peerAuth.Verify(r)
	if err != nil {
		http.Error(w, "relay-peer identity required", http.StatusUnauthorized)
		return
	}
	if r.Header.Get(relay.HeaderPeerTarget) != relay.HubPeerTarget || r.Header.Get(relay.HeaderPeerTargetGeneration) != "0" {
		w.Header().Set(relay.HeaderStaleReason, "relay_target_mismatch")
		http.Error(w, "request is not for the hub", http.StatusConflict)
		return
	}
	store := s.relayPeerReplayStoreFor(rt)
	if store == nil {
		http.Error(w, "relay-peer replay store unavailable", http.StatusServiceUnavailable)
		return
	}
	fresh, err := store.CheckAndStore(r.Context(), relayPeerReplayKey(peer, r.Header.Get(relay.HeaderPeerNonce)), relayPeerReplayTTL)
	if err != nil {
		slog.Warn("Conduit hub internal API: relay-peer replay store unavailable", "peer", peer, "error", err)
		http.Error(w, "relay-peer replay store unavailable", http.StatusServiceUnavailable)
		return
	}
	if !fresh {
		http.Error(w, "relay-peer request replayed", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, relay.MaxHubRequestBody+1))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if len(body) > relay.MaxHubRequestBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	sum := sha256.Sum256(body)
	if r.Header.Get(relay.HeaderBodySHA256) != hex.EncodeToString(sum[:]) {
		http.Error(w, "body digest mismatch", http.StatusBadRequest)
		return
	}
	path := r.URL.Path
	if op, ok := hubInternalOps[path]; ok {
		op(s, w, r, peer, body)
		return
	}
	if rest, ok := cutHubSessionPath(path); ok {
		if op, ok := hubInternalSessionOps[rest.op]; ok {
			op(s, w, r, peer, rest.sessionID, body)
			return
		}
	}
	http.NotFound(w, r)
}

// hubSessionPath is a parsed sessions/{id}/{op} path.
type hubSessionPath struct{ sessionID, op string }

// cutHubSessionPath parses a sessions/{id}/{op} path.
func cutHubSessionPath(path string) (hubSessionPath, bool) {
	if len(path) <= len(relay.HubSessionsPrefix) || path[:len(relay.HubSessionsPrefix)] != relay.HubSessionsPrefix {
		return hubSessionPath{}, false
	}
	rest := path[len(relay.HubSessionsPrefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			id, op := rest[:i], rest[i+1:]
			if id == "" || op == "" {
				return hubSessionPath{}, false
			}
			return hubSessionPath{sessionID: id, op: op}, true
		}
	}
	return hubSessionPath{}, false
}
