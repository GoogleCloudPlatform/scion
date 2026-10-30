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
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// The hub admission token (design §3.9 "Hub admission token"). Admit
// returns it; the relay presents it unchanged on every later relay-to-hub
// operation for that session (RPC forward, credential refresh, session
// re-check). It binds the operation to the admitted session: the hub takes
// the principal, scope and credential id only from its claims, never from
// anything else the relay sends, and re-checks the credential's current
// status and the principal's current permissions on every operation. It
// proves who was admitted, never what they may do now.
//
// The token is sealed with AES-256-GCM under a hub-only key derived by
// HKDF from a signing generation's secret with its own label (independent
// of the relay-peer secret, which relays hold). The claims are
// confidential to the hub, and opening a token authenticates it: a token
// that does not open, or was sealed under another key, is refused. The
// token is never logged or put in an error.
//
// Keys and formats (ptone/scion#1530 Phase 5). The key is a purpose of the
// signing ring (conduitAdmissionPurpose, the unchanged derivation): in
// managed ring mode (a keyring file) the active generation seals and
// every eligible generation opens; in legacy mode the ring is the legacy
// adapter over the shared session secret, so the key is exactly the one
// that secret always gave. Two wire formats:
//
//   - legacy (emit "legacy"): base64url(nonce || ciphertext), AAD
//     conduitAdmissionAAD; it opens with the ring's legacy readers.
//   - versioned (emit "versioned"): "ca3." + kid + "." +
//     base64url(nonce || ciphertext), AAD conduitAdmissionAADv3Prefix +
//     kid; it opens with that generation only (an unknown kid opens
//     nothing, never another key).
//
// No token is sealed at or after the active generation's sign_until, and
// none opens at or after its generation's verify_until, whatever the
// claims' own expiry and whether the operation accepts expired claims.

// conduitAdmissionKeyInfo is the HKDF label of the admission token key. It
// differs from every other derivation of the session secret and from the
// relay-peer secret's.
const conduitAdmissionKeyInfo = "scion conduit relay admission token aead v2"

// conduitAdmissionAAD binds a sealed legacy-format token to its use.
const conduitAdmissionAAD = "scion conduit relay admission token"

// conduitAdmissionV3Prefix starts a versioned token; the key ID and a dot
// follow. A legacy token never contains a dot (unpadded base64url).
const conduitAdmissionV3Prefix = "ca3."

// conduitAdmissionAADv3Prefix, followed by the key ID, binds a sealed
// versioned token to its use and to its generation.
const conduitAdmissionAADv3Prefix = "scion conduit relay admission token:ca3:"

// conduitAdmissionTokenVersion is the claims format version (unchanged by
// the versioned wire format).
const conduitAdmissionTokenVersion = 2

// conduitAdmissionPurpose is the admission token key's signing-ring
// purpose: HKDF-SHA256 of the generation's secret with
// conduitAdmissionKeyInfo, 32 bytes, as the shared session secret always
// gave.
var conduitAdmissionPurpose = keyring.NewPurpose("conduit_admission_token", func(secret []byte) []byte {
	key, err := hkdf.Key(sha256.New, secret, nil, conduitAdmissionKeyInfo, 32)
	if err != nil {
		return nil
	}
	return key
})

// errConduitAdmissionNoKey refuses admission tokens without a shared
// session secret: tokens from one hub replica must open on every other.
var errConduitAdmissionNoKey = errors.New("conduit admission tokens need the shared session secret (--session-secret or SCION_SERVER_SESSION_SECRET)")

// errConduitAdmissionIssuanceClosed refuses to seal a token when no key
// may issue now: the signing ring's active generation reached its
// sign_until, or the ring has no usable active generation. It names no key
// or ID.
var errConduitAdmissionIssuanceClosed = errors.New("conduit admission tokens: no signing key may issue now (the active signing generation reached its sign_until, or the signing ring has none)")

// conduitAdmissionMaxAge caps a token's lifetime after admission (or
// re-issue): exp <= min(credential_expiry, issued + 1h).
const conduitAdmissionMaxAge = time.Hour

// conduitAdmissionClaims are the claims of an admission token.
type conduitAdmissionClaims struct {
	V         int    `json:"v"`
	SessionID string `json:"sid"`
	Nonce     string `json:"n"`
	Relay     string `json:"rel"`
	RelayGen  int64  `json:"gen"`

	Kind            string `json:"pk"`
	ID              string `json:"pid"`
	ProjectID       string `json:"proj,omitempty"`
	ExecScope       string `json:"xs,omitempty"`
	AgentLaunchID   string `json:"aln,omitempty"`
	AgentGeneration int64  `json:"agen,omitempty"`
	TokenRunID      string `json:"trun,omitempty"`
	TokenRunEnforce bool   `json:"tenf,omitempty"`

	AuthType     string   `json:"at,omitempty"`
	CredentialID string   `json:"cid,omitempty"`
	ScopeProject string   `json:"sp,omitempty"`
	Scopes       []string `json:"sc,omitempty"`

	// CredentialExpiry is when the admitting credential expires (unix
	// seconds; 0: no expiry).
	CredentialExpiry int64 `json:"cexp,omitempty"`
	// AdmittedAt is when the session was admitted (unix seconds); kept
	// across re-issues.
	AdmittedAt int64 `json:"adm"`
	// IssuedAt and Expiry bound this token (unix seconds).
	IssuedAt int64 `json:"iat"`
	Expiry   int64 `json:"exp"`
}

// Admission token verification failures. Each maps to one status.
var (
	// errAdmissionTokenInvalid: missing, malformed, wrong version, or it
	// does not open under the hub's key (401).
	errAdmissionTokenInvalid = errors.New("admission token invalid")
	// errAdmissionTokenExpired: past exp where expiry applies (401).
	errAdmissionTokenExpired = errors.New("admission token expired")
	// errAdmissionTokenSession: issued for another session (401).
	errAdmissionTokenSession = errors.New("admission token is for another session")
	// errAdmissionTokenRelay: issued to another relay instance or
	// generation (409 relay_target_mismatch).
	errAdmissionTokenRelay = errors.New("admission token is for another relay")
)

// conduitAdmissionRing returns the signing ring admission token keys come
// from: the keyring's signing ring in managed ring mode (any keyring but
// the legacy adapter; a ring without a usable active generation then
// fails every seal and open, never falling back to the shared secret), or
// the legacy adapter over the shared session secret.
func (s *Server) conduitAdmissionRing() (*keyring.SigningRing, error) {
	if k := s.config.Keyring; k != nil && k.Source() != keyring.SourceLegacy {
		return k.Signing(), nil
	}
	secret := s.config.SharedSigningSecret
	if secret == "" {
		return nil, errConduitAdmissionNoKey
	}
	return keyring.Legacy(secret).Signing(), nil
}

// errConduitAdmissionNoActive refuses admission tokens in managed ring
// mode when the signing ring has no usable active generation.
var errConduitAdmissionNoActive = errors.New("conduit admission tokens: keyring mode: the signing ring has no usable active generation")

// checkConduitAdmissionKeys is the startup check of the admission token
// keys: a shared session secret in legacy mode, a signing ring with an
// active generation in managed ring mode. An active generation past its
// sign_until is not a startup refusal; minting then fails and /readyz
// reports it.
func (s *Server) checkConduitAdmissionKeys() error {
	ring, err := s.conduitAdmissionRing()
	if err != nil {
		return err
	}
	if len(ring.IDs()) == 0 {
		return errConduitAdmissionNoActive
	}
	return nil
}

// newConduitAdmissionAEAD returns the admission token cipher of one
// derived key.
func newConduitAdmissionAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// setAdmissionLifetime sets IssuedAt and Expiry from now:
// exp = min(credential_expiry, now + conduitAdmissionMaxAge).
func setAdmissionLifetime(c *conduitAdmissionClaims, now time.Time) {
	c.IssuedAt = now.Unix()
	exp := now.Add(conduitAdmissionMaxAge).Unix()
	if c.CredentialExpiry > 0 && c.CredentialExpiry < exp {
		exp = c.CredentialExpiry
	}
	c.Expiry = exp
}

// mintAdmissionToken seals c under the signing ring's active generation,
// in the format the ring's emit selects. Nothing is sealed at or after the
// active generation's sign_until.
func (s *Server) mintAdmissionToken(c conduitAdmissionClaims) (string, error) {
	ring, err := s.conduitAdmissionRing()
	if err != nil {
		return "", err
	}
	key, err := ring.Active(conduitAdmissionPurpose, s.keyringNow())
	if err != nil {
		return "", errConduitAdmissionIssuanceClosed
	}
	aead, err := newConduitAdmissionAEAD(key.Bytes())
	if err != nil {
		return "", errConduitAdmissionIssuanceClosed
	}
	c.V = conduitAdmissionTokenVersion
	c.Nonce = newAdmissionNonce()
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	if ring.Emit() == keyring.EmitVersioned {
		sealed := aead.Seal(nonce, nonce, payload, []byte(conduitAdmissionAADv3Prefix+key.ID))
		return conduitAdmissionV3Prefix + key.ID + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
	}
	sealed := aead.Seal(nonce, nonce, payload, []byte(conduitAdmissionAAD))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// conduitAdmissionKeyIDPattern is the keyring's key ID syntax. A versioned
// token whose key ID does not match it is refused before any ring lookup.
var conduitAdmissionKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// openAdmissionToken authenticates and decrypts token (nil, false on any
// failure). A versioned token opens with its key ID's generation only, a
// legacy token with the ring's legacy readers, each strictly before its
// verify_until.
func (s *Server) openAdmissionToken(token string) ([]byte, bool) {
	ring, err := s.conduitAdmissionRing()
	if err != nil {
		return nil, false
	}
	kid, body, aad := "", token, conduitAdmissionAAD
	if rest, ok := strings.CutPrefix(token, conduitAdmissionV3Prefix); ok {
		var found bool
		kid, body, found = strings.Cut(rest, ".")
		if !found || !conduitAdmissionKeyIDPattern.MatchString(kid) || strings.Contains(body, ".") {
			return nil, false
		}
		aad = conduitAdmissionAADv3Prefix + kid
	}
	sealed, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, false
	}
	for _, key := range ring.Candidates(conduitAdmissionPurpose, kid, s.keyringNow()) {
		aead, err := newConduitAdmissionAEAD(key.Bytes())
		if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
			continue
		}
		if payload, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(aad)); err == nil {
			return payload, true
		}
	}
	return nil, false
}

// newAdmissionNonce returns a random claims nonce.
func newAdmissionNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// admissionCheck is what an operation requires of a token beyond opening.
type admissionCheck struct {
	// sessionID is the session the operation is for.
	sessionID string
	// relay and relayGen are the calling relay's instance id (its
	// authenticated relay-peer id) and its current registered generation.
	relay    string
	relayGen int64
	// allowExpired accepts a token past exp (re-check and refresh, which
	// re-evaluate current status anyway).
	allowExpired bool
}

// verifyAdmissionToken opens token (authenticating it), checks its
// version, session and relay binding and (unless allowed) expiry, and
// returns its claims. The error is one of the errAdmissionToken* values
// and never carries the token.
func (s *Server) verifyAdmissionToken(token string, want admissionCheck, now time.Time) (conduitAdmissionClaims, error) {
	var c conduitAdmissionClaims
	payload, ok := s.openAdmissionToken(token)
	if !ok {
		return c, errAdmissionTokenInvalid
	}
	if err := json.Unmarshal(payload, &c); err != nil || c.V != conduitAdmissionTokenVersion || c.SessionID == "" || c.Kind == "" || c.ID == "" {
		return conduitAdmissionClaims{}, errAdmissionTokenInvalid
	}
	if c.Relay != want.relay || c.RelayGen != want.relayGen {
		return conduitAdmissionClaims{}, errAdmissionTokenRelay
	}
	if c.SessionID != want.sessionID {
		return conduitAdmissionClaims{}, errAdmissionTokenSession
	}
	if !want.allowExpired && now.Unix() >= c.Expiry {
		return conduitAdmissionClaims{}, errAdmissionTokenExpired
	}
	return c, nil
}

// reissueAdmissionToken mints a fresh token for the same admission (same
// session, principal, relay binding and admitted_at), with a new lifetime
// from now. credentialExpiry replaces the credential expiry when non-zero
// (a refresh); otherwise it is kept.
func (s *Server) reissueAdmissionToken(c conduitAdmissionClaims, credentialExpiry time.Time, now time.Time) (string, error) {
	if !credentialExpiry.IsZero() {
		c.CredentialExpiry = credentialExpiry.Unix()
	}
	setAdmissionLifetime(&c, now)
	return s.mintAdmissionToken(c)
}

// admissionHalfLifeLeft reports whether less than half of c's lifetime is
// left at now (a healthy re-check then re-issues the token).
func admissionHalfLifeLeft(c conduitAdmissionClaims, now time.Time) bool {
	return now.Unix() >= c.IssuedAt+(c.Expiry-c.IssuedAt)/2
}
