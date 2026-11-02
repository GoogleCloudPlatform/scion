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
	"container/heap"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
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

// ErrNoInternalRelayAPI is returned by NoPeerAuth.Sign: this node serves no
// internal relay API, so it holds no relay-peer credential to sign with.
var ErrNoInternalRelayAPI = errors.New("conduit relay: internal relay API not served on this node (server.hub.conduit.internal_listen is unset), so requests to other relays are refused")

// NoPeerAuth is the relay-peer authenticator of a node that serves no
// internal relay API (no internal listener): it refuses in both directions.
// Verify accepts no peer and Sign signs nothing, so the node never answers
// a relay-peer request and never sends an unsigned one.
type NoPeerAuth struct{}

// Sign implements PeerAuth: it always fails.
func (NoPeerAuth) Sign(*http.Request) error { return ErrNoInternalRelayAPI }

// Verify implements PeerAuth: it always refuses.
func (NoPeerAuth) Verify(*http.Request) (string, error) { return "", ErrPeerUnauthenticated }

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
	// HeaderPeerTarget and HeaderPeerTargetGeneration name the relay the
	// request is meant for: its instance id and registered generation
	// (SetPeerTarget). Both are covered by the HMAC signature, and the
	// serving relay refuses a request that names another instance or
	// generation than its own current ones.
	HeaderPeerTarget           = "X-Conduit-Peer-Target"
	HeaderPeerTargetGeneration = "X-Conduit-Peer-Target-Generation"
	// HeaderPeerKeyID and HeaderPeerSignatureVersion mark a versioned
	// (key-ID-carrying) signature of a signing-ring authenticator
	// (ptone/scion#1530 Phase 5): the signing generation's key ID and
	// peerSignatureVersion2. A request carries both exactly once or
	// neither; neither selects the ID-less signature format.
	HeaderPeerKeyID            = "X-Conduit-Peer-Key-Id"
	HeaderPeerSignatureVersion = "X-Conduit-Peer-Signature-Version"
)

// peerSignatureVersion2 is the HeaderPeerSignatureVersion value of a
// versioned signature.
const peerSignatureVersion2 = "2"

// peerSignatureV2Domain starts the MAC input of a versioned signature:
// domain, key ID and a newline, then the unchanged canonical request
// input. Both the version and the key ID are covered by the MAC.
const peerSignatureV2Domain = "scion-conduit-peer-v2\n"

// peerKeyIDPattern is the keyring's key ID syntax. A versioned signature
// whose key ID does not match it is refused before any ring lookup.
var peerKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// SetPeerTarget names the relay a relay-peer request is meant for. Call it
// before PeerAuth.Sign.
func SetPeerTarget(req *http.Request, instanceID string, generation int64) {
	req.Header.Set(HeaderPeerTarget, instanceID)
	req.Header.Set(HeaderPeerTargetGeneration, strconv.FormatInt(generation, 10))
}

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

// PeerHMACPurpose is the signing-ring purpose of the relay-peer HMAC key
// (ptone/scion#1530 Phase 5): deriveHMACPeerKey, unchanged, applied to a
// generation's secret, so the imported "legacy" generation derives exactly
// the key NewHMACPeerAuthFromSecret derives from the shared secret. A
// secret too short to derive from yields no key, which
// NewHMACPeerAuthFromRing refuses.
var PeerHMACPurpose = keyring.NewPurpose("conduit_relay_peer_hmac", func(secret []byte) []byte {
	key, err := deriveHMACPeerKey(secret)
	if err != nil {
		return nil
	}
	return key
})

// DefaultPeerMaxSkew is the default relay-peer timestamp window
// (HMACPeerAuthConfig.MaxSkew).
const DefaultPeerMaxSkew = 60 * time.Second

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
// body digest, Want header and target relay (instance id and generation)
// with HMAC-SHA256. Verify refuses a stale
// timestamp or a nonce it has already seen within the skew window. It is
// the non-GCP relay-peer mechanism (on GCP the hub uses OIDC ID tokens).
//
// It is keyed either with one fixed derived key (key: the legacy shared
// secret or the dedicated peer secret), which signs and accepts the
// ID-less format only, or with a signing ring (ring), whose active
// generation signs and whose generations verify (NewHMACPeerAuthFromRing).
// Either way a request is validated once: the signature is checked against
// the candidate keys, then the timestamp and the one replay cache, whatever
// key or format matched.
type HMACPeerAuth struct {
	cfg  HMACPeerAuthConfig
	key  []byte               // derived MAC key (fixed-key mode)
	ring *keyring.SigningRing // signing ring (ring mode; key is nil)

	mu     sync.Mutex
	nonces map[string]time.Time // peer+nonce -> expiry
	expiry nonceQueue           // the same entries, ordered by expiry
}

// nonceEntry is one replay-cache entry.
type nonceEntry struct {
	key string
	exp time.Time
}

// nonceQueue is a min-heap of replay-cache entries by expiry
// (container/heap).
type nonceQueue []nonceEntry

func (q nonceQueue) Len() int           { return len(q) }
func (q nonceQueue) Less(i, j int) bool { return q[i].exp.Before(q[j].exp) }
func (q nonceQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *nonceQueue) Push(x any)        { *q = append(*q, x.(nonceEntry)) }
func (q *nonceQueue) Pop() any {
	old := *q
	e := old[len(old)-1]
	old[len(old)-1] = nonceEntry{}
	*q = old[:len(old)-1]
	return e
}

// evictExpiredLocked removes the entries whose expiry has passed, in
// expiry order. Verify calls it only when the cache is at its cap; an
// entry is removed only once it has expired, never to make room. a.mu
// must be held.
func (a *HMACPeerAuth) evictExpiredLocked(now time.Time) {
	for len(a.expiry) > 0 && now.After(a.expiry[0].exp) {
		e := heap.Pop(&a.expiry).(nonceEntry)
		delete(a.nonces, e.key)
	}
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

// hmacPeerSecretKeyInfo is the HKDF info label for the relay-peer HMAC key
// derived from the dedicated relay-peer secret
// (server.hub.conduit.peer_secret, design §3.9). It differs from
// hmacPeerKeyInfo and from every
// label used with the session secret, and the secret itself is independent
// of the session secret: neither key can be derived from the other.
const hmacPeerSecretKeyInfo = "scion conduit relay-peer secret hmac v1"

// NewHMACPeerAuthFromPeerSecret returns an HMACPeerAuth keyed with the HKDF
// derivation, under its own label, of the dedicated relay-peer secret
// cfg.Secret. Separate relays and hubs in external-relay mode use it; they
// never hold the session secret. The secret's strength rules (normalization,
// minimum length, distinct octets) are config.ValidatePeerSecret's, which
// every caller applies first; here an empty secret is refused.
func NewHMACPeerAuthFromPeerSecret(cfg HMACPeerAuthConfig) (*HMACPeerAuth, error) {
	if len(cfg.Secret) == 0 {
		return nil, errors.New("conduit relay: empty relay-peer secret")
	}
	key, err := hkdf.Key(sha256.New, cfg.Secret, nil, hmacPeerSecretKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	cfg.Secret = nil // keep only the derived key
	return newHMACPeerAuthWithKey(cfg, key)
}

// Ring-mode construction refusals. Fixed text: no key ID, secret or key.
var (
	errPeerRingNoActive = errors.New("conduit relay: the signing ring has no usable active generation for relay-peer signatures")
	errPeerRingShortKey = errors.New("conduit relay: a signing ring generation's secret is too short to derive a relay-peer key")
)

// NewHMACPeerAuthFromRing returns an HMACPeerAuth keyed with the signing
// ring (ptone/scion#1530 Phase 5, managed ring mode of the shared-secret
// fallback). The active generation signs, with PeerHMACPurpose; it writes
// the versioned format when the ring's emit is "versioned" and the ID-less
// format otherwise, and signs nothing at or after its sign_until. Verify
// accepts a versioned signature with its key ID's generation only, and an
// ID-less one with the ring's legacy readers, each strictly before its
// verify_until; cfg.Now is the clock for these deadlines too. cfg.Secret
// must be empty. A ring without a usable active generation, or with a
// generation whose secret is too short to derive from, is refused.
func NewHMACPeerAuthFromRing(cfg HMACPeerAuthConfig, ring *keyring.SigningRing) (*HMACPeerAuth, error) {
	if len(cfg.Secret) != 0 {
		return nil, errors.New("conduit relay: a ring-keyed relay-peer authenticator takes no secret")
	}
	gens := ring.Generations(PeerHMACPurpose)
	if len(gens) == 0 {
		return nil, errPeerRingNoActive
	}
	for _, g := range gens {
		if len(g.Bytes()) < 32 {
			return nil, errPeerRingShortKey
		}
	}
	if cfg.SelfID == "" {
		return nil, errors.New("conduit relay: relay-peer HMAC needs SelfID")
	}
	a := newHMACPeerAuthDefaults(cfg)
	a.ring = ring
	return a, nil
}

// newHMACPeerAuthWithKey uses key as the MAC key as is (internal and
// tests only; production goes through NewHMACPeerAuthFromSecret or
// NewHMACPeerAuthFromPeerSecret).
func newHMACPeerAuthWithKey(cfg HMACPeerAuthConfig, key []byte) (*HMACPeerAuth, error) {
	if len(key) < 32 {
		return nil, errors.New("conduit relay: relay-peer HMAC key must be at least 32 bytes")
	}
	if cfg.SelfID == "" {
		return nil, errors.New("conduit relay: relay-peer HMAC needs SelfID")
	}
	a := newHMACPeerAuthDefaults(cfg)
	a.key = key
	return a, nil
}

// newHMACPeerAuthDefaults applies the config defaults and returns an
// authenticator without keys.
func newHMACPeerAuthDefaults(cfg HMACPeerAuthConfig) *HMACPeerAuth {
	if cfg.MaxSkew <= 0 {
		cfg.MaxSkew = DefaultPeerMaxSkew
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxNonces <= 0 {
		cfg.MaxNonces = 1 << 16
	}
	return &HMACPeerAuth{cfg: cfg, nonces: make(map[string]time.Time)}
}

// canonical is the request MAC input of the ID-less format: each signed
// field as its decimal length, a colon and its bytes. A versioned
// signature MACs peerSignatureV2Domain, the key ID and a newline before
// these exact bytes.
func canonical(req *http.Request, peer, ts, nonce string) []byte {
	var b []byte
	for _, part := range []string{
		req.Method, req.URL.RequestURI(), ts, nonce, peer,
		req.Header.Get(HeaderBodySHA256), req.Header.Get(HeaderWant),
		req.Header.Get(HeaderPeerTarget), req.Header.Get(HeaderPeerTargetGeneration),
	} {
		b = strconv.AppendInt(b, int64(len(part)), 10)
		b = append(b, ':')
		b = append(b, part...)
	}
	return b
}

// peerMAC is the signature of input under key: the ID-less format for an
// empty kid, the versioned format for kid.
func peerMAC(key []byte, kid string, input []byte) []byte {
	m := hmac.New(sha256.New, key)
	if kid != "" {
		m.Write([]byte(peerSignatureV2Domain))
		m.Write([]byte(kid))
		m.Write([]byte{'\n'})
	}
	m.Write(input)
	return m.Sum(nil)
}

// errPeerIssuanceClosed is returned by Sign of a ring-keyed authenticator
// whose active generation may not sign now (at or after its sign_until).
// It names no key or ID.
var errPeerIssuanceClosed = errors.New("conduit relay: no relay-peer signing key may sign now (the active signing generation reached its sign_until)")

// signingKey returns the key a request is signed with now and the key ID
// for a versioned signature ("" for the ID-less format).
func (a *HMACPeerAuth) signingKey(now time.Time) (key []byte, kid string, err error) {
	if a.ring == nil {
		return a.key, "", nil
	}
	k, err := a.ring.Active(PeerHMACPurpose, now)
	if err != nil {
		return nil, "", errPeerIssuanceClosed
	}
	key = k.Bytes()
	if len(key) < 32 {
		return nil, "", errPeerIssuanceClosed
	}
	if a.ring.Emit() == keyring.EmitVersioned {
		return key, k.ID, nil
	}
	return key, "", nil
}

// verifyKeys returns the keys a signature in the format kid selects may be
// checked with: a fixed key accepts only the ID-less format; a ring gives
// the key ID's generation (none when unknown or retired, never another) or,
// for "", its legacy readers.
func (a *HMACPeerAuth) verifyKeys(kid string, now time.Time) [][]byte {
	if a.ring == nil {
		if kid != "" {
			return nil
		}
		return [][]byte{a.key}
	}
	var out [][]byte
	for _, k := range a.ring.Candidates(PeerHMACPurpose, kid, now) {
		if b := k.Bytes(); len(b) >= 32 {
			out = append(out, b)
		}
	}
	return out
}

// signatureKeyID returns the key ID a request's signature format selects:
// "" when it carries neither versioned header, the key ID when it carries
// both exactly once with version peerSignatureVersion2 and a well-formed
// key ID. Anything else (one header, a repeated one, another version, a
// malformed key ID) is not ok: the request is refused, never retried as
// the ID-less format.
func signatureKeyID(h http.Header) (kid string, ok bool) {
	ids := h.Values(HeaderPeerKeyID)
	versions := h.Values(HeaderPeerSignatureVersion)
	switch {
	case len(ids) == 0 && len(versions) == 0:
		return "", true
	case len(ids) != 1 || len(versions) != 1:
		return "", false
	case versions[0] != peerSignatureVersion2 || !peerKeyIDPattern.MatchString(ids[0]):
		return "", false
	}
	return ids[0], true
}

// Sign implements PeerAuth.
func (a *HMACPeerAuth) Sign(req *http.Request) error {
	now := a.cfg.Now()
	key, kid, err := a.signingKey(now)
	if err != nil {
		return err
	}
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return err
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := hex.EncodeToString(nb[:])
	req.Header.Set(HeaderPeerID, a.cfg.SelfID)
	req.Header.Set(HeaderPeerTimestamp, ts)
	req.Header.Set(HeaderPeerNonce, nonce)
	if kid != "" {
		req.Header.Set(HeaderPeerKeyID, kid)
		req.Header.Set(HeaderPeerSignatureVersion, peerSignatureVersion2)
	} else {
		// A request signed again must not keep a previous versioned mark.
		req.Header.Del(HeaderPeerKeyID)
		req.Header.Del(HeaderPeerSignatureVersion)
	}
	req.Header.Set(HeaderPeerSignature, base64.RawURLEncoding.EncodeToString(peerMAC(key, kid, canonical(req, a.cfg.SelfID, ts, nonce))))
	return nil
}

// Verify implements PeerAuth. The signature is checked against the
// candidate keys of its format (a key that does not match changes
// nothing); then, once, the timestamp and the replay cache, keyed by peer
// and nonce whatever key or format matched.
func (a *HMACPeerAuth) Verify(req *http.Request) (string, error) {
	peer := req.Header.Get(HeaderPeerID)
	ts := req.Header.Get(HeaderPeerTimestamp)
	nonce := req.Header.Get(HeaderPeerNonce)
	sig := req.Header.Get(HeaderPeerSignature)
	if peer == "" || ts == "" || nonce == "" || sig == "" || len(nonce) > 64 {
		return "", ErrPeerUnauthenticated
	}
	kid, ok := signatureKeyID(req.Header)
	if !ok {
		return "", ErrPeerUnauthenticated
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return "", ErrPeerUnauthenticated
	}
	now := a.cfg.Now()
	input := canonical(req, peer, ts, nonce)
	matched := false
	for _, key := range a.verifyKeys(kid, now) {
		if hmac.Equal(got, peerMAC(key, kid, input)) {
			matched = true
			break
		}
	}
	if !matched {
		return "", ErrPeerUnauthenticated
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", ErrPeerUnauthenticated
	}
	at := time.Unix(sec, 0)
	if d := now.Sub(at); d > a.cfg.MaxSkew || d < -a.cfg.MaxSkew {
		return "", fmt.Errorf("%w: timestamp outside the allowed skew", ErrPeerUnauthenticated)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.nonces) >= a.cfg.MaxNonces {
		a.evictExpiredLocked(now)
	}
	key := peer + "\x00" + nonce
	if _, seen := a.nonces[key]; seen {
		return "", fmt.Errorf("%w: replayed nonce", ErrPeerUnauthenticated)
	}
	if len(a.nonces) >= a.cfg.MaxNonces {
		// Fail closed rather than forget nonces still inside the window.
		return "", fmt.Errorf("%w: replay cache full", ErrPeerUnauthenticated)
	}
	exp := at.Add(2 * a.cfg.MaxSkew)
	a.nonces[key] = exp
	heap.Push(&a.expiry, nonceEntry{key: key, exp: exp})
	return peer, nil
}
