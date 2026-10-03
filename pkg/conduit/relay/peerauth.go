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
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// PeerAuth authenticates internal relay API calls between relays (the
// relay-peer principal, design §3.2, §3.10). The relay performs no end-user
// authorization; it only checks that the caller is a peer.
//
// Sign is called by the calling relay on every request (RPC, stream
// upgrade, self-check probe) after the request's headers are final. Verify
// is called by the serving relay before anything else; an error makes the
// request fail with 401 and nothing is read or forwarded.
type PeerAuth interface {
	Sign(req *http.Request) error
	Verify(req *http.Request) (peerID string, err error)
}

// ErrPeerUnauthenticated is returned by Verify for a request without a
// valid peer identity.
var ErrPeerUnauthenticated = errors.New("conduit relay: relay-peer identity missing or invalid")

// HMAC peer-auth headers.
const (
	HeaderPeerID        = "X-Conduit-Peer"
	HeaderPeerTimestamp = "X-Conduit-Peer-Timestamp"
	HeaderPeerNonce     = "X-Conduit-Peer-Nonce"
	HeaderPeerSignature = "X-Conduit-Peer-Signature"
	// HeaderBodySHA256 carries the hex SHA-256 of an RPC request body. It
	// is covered by the HMAC signature, and the serving relay checks the
	// body against it whatever the PeerAuth mechanism.
	HeaderBodySHA256 = "X-Conduit-Body-SHA256"
	// HeaderWant carries the caller's registry.Want (JSON) so the owning
	// relay re-checks admission with the same expectation.
	HeaderWant = "X-Conduit-Want"
)

// hmacPeerKeyInfo is the HKDF info label for the relay-peer HMAC key. It
// separates this key from every other use of the input secret.
const hmacPeerKeyInfo = "scion conduit relay-peer hmac v1"

// deriveHMACPeerKey derives the relay-peer HMAC key from a secret every
// hub node shares (the hub's shared signing secret in HA deployments) with
// HKDF-SHA256 under a dedicated info label. The raw secret is never used as
// the MAC key, so a relay-peer signature can never be confused with, or
// help forge, a token signed with the same secret for another purpose.
// It is unexported on purpose: callers hand NewHMACPeerAuthFromSecret the
// shared secret and the derivation cannot be skipped.
func deriveHMACPeerKey(secret []byte) ([]byte, error) {
	if len(secret) < 16 {
		return nil, errors.New("conduit relay: shared secret too short to derive a relay-peer key")
	}
	return hkdf.Key(sha256.New, secret, nil, hmacPeerKeyInfo, 32)
}

// HMACPeerAuthConfig configures HMACPeerAuth.
type HMACPeerAuthConfig struct {
	// Secret is the secret every hub node shares (at least 16 bytes). The
	// MAC key is derived from it with HKDF (deriveHMACPeerKey); the raw
	// secret is never used as the key.
	Secret []byte
	// SelfID is this relay's instance id, sent as the caller identity.
	SelfID string
	// MaxSkew bounds |now - timestamp| (default 60s).
	MaxSkew time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
	// MaxNonces bounds the replay cache (default 1<<16).
	MaxNonces int
}

// HMACPeerAuth signs the method, request URI, timestamp, nonce, caller id,
// body digest and Want header with HMAC-SHA256. Verify refuses a stale
// timestamp or a nonce it has already seen within the skew window. It is
// the non-GCP relay-peer mechanism (on GCP the hub uses OIDC ID tokens).
type HMACPeerAuth struct {
	cfg HMACPeerAuthConfig
	key []byte // derived MAC key

	mu     sync.Mutex
	nonces map[string]time.Time
}

// NewHMACPeerAuthFromSecret returns an HMACPeerAuth keyed with the HKDF
// derivation of cfg.Secret.
func NewHMACPeerAuthFromSecret(cfg HMACPeerAuthConfig) (*HMACPeerAuth, error) {
	key, err := deriveHMACPeerKey(cfg.Secret)
	if err != nil {
		return nil, err
	}
	cfg.Secret = nil // keep only the derived key
	return newHMACPeerAuthWithKey(cfg, key)
}

// newHMACPeerAuthWithKey uses key as the MAC key as is (internal and
// tests only; production goes through NewHMACPeerAuthFromSecret).
func newHMACPeerAuthWithKey(cfg HMACPeerAuthConfig, key []byte) (*HMACPeerAuth, error) {
	if len(key) < 32 {
		return nil, errors.New("conduit relay: relay-peer HMAC key must be at least 32 bytes")
	}
	if cfg.SelfID == "" {
		return nil, errors.New("conduit relay: relay-peer HMAC needs SelfID")
	}
	if cfg.MaxSkew <= 0 {
		cfg.MaxSkew = 60 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxNonces <= 0 {
		cfg.MaxNonces = 1 << 16
	}
	return &HMACPeerAuth{cfg: cfg, key: key, nonces: make(map[string]time.Time)}, nil
}

func (a *HMACPeerAuth) mac(req *http.Request, peer, ts, nonce string) []byte {
	m := hmac.New(sha256.New, a.key)
	for _, part := range []string{
		req.Method, req.URL.RequestURI(), ts, nonce, peer,
		req.Header.Get(HeaderBodySHA256), req.Header.Get(HeaderWant),
	} {
		m.Write([]byte(strconv.Itoa(len(part))))
		m.Write([]byte{':'})
		m.Write([]byte(part))
	}
	return m.Sum(nil)
}

// Sign implements PeerAuth.
func (a *HMACPeerAuth) Sign(req *http.Request) error {
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return err
	}
	ts := strconv.FormatInt(a.cfg.Now().Unix(), 10)
	nonce := hex.EncodeToString(nb[:])
	req.Header.Set(HeaderPeerID, a.cfg.SelfID)
	req.Header.Set(HeaderPeerTimestamp, ts)
	req.Header.Set(HeaderPeerNonce, nonce)
	req.Header.Set(HeaderPeerSignature, base64.RawURLEncoding.EncodeToString(a.mac(req, a.cfg.SelfID, ts, nonce)))
	return nil
}

// Verify implements PeerAuth.
func (a *HMACPeerAuth) Verify(req *http.Request) (string, error) {
	peer := req.Header.Get(HeaderPeerID)
	ts := req.Header.Get(HeaderPeerTimestamp)
	nonce := req.Header.Get(HeaderPeerNonce)
	sig := req.Header.Get(HeaderPeerSignature)
	if peer == "" || ts == "" || nonce == "" || sig == "" || len(nonce) > 64 {
		return "", ErrPeerUnauthenticated
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, a.mac(req, peer, ts, nonce)) {
		return "", ErrPeerUnauthenticated
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", ErrPeerUnauthenticated
	}
	now := a.cfg.Now()
	at := time.Unix(sec, 0)
	if d := now.Sub(at); d > a.cfg.MaxSkew || d < -a.cfg.MaxSkew {
		return "", fmt.Errorf("%w: timestamp outside the allowed skew", ErrPeerUnauthenticated)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for n, exp := range a.nonces {
		if len(a.nonces) < a.cfg.MaxNonces {
			break
		}
		if now.After(exp) {
			delete(a.nonces, n)
		}
	}
	key := peer + "\x00" + nonce
	if _, seen := a.nonces[key]; seen {
		return "", fmt.Errorf("%w: replayed nonce", ErrPeerUnauthenticated)
	}
	if len(a.nonces) >= a.cfg.MaxNonces {
		// Fail closed rather than forget nonces still inside the window.
		return "", fmt.Errorf("%w: replay cache full", ErrPeerUnauthenticated)
	}
	a.nonces[key] = at.Add(2 * a.cfg.MaxSkew)
	return peer, nil
}
