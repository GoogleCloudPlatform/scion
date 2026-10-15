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
	"time"
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
// The token is the base64url encoding of a random 12-byte nonce and the
// AES-256-GCM sealing of the JSON claims under a hub-only key derived by
// HKDF from the session secret with its own label (independent of the
// relay-peer secret, which relays hold). The claims are confidential to
// the hub, and opening a token authenticates it: a token that does not
// open, or was sealed under another key, is refused. A session-secret
// rotation invalidates every outstanding token: there is no overlap key.
// The token is never logged or put in an error.

// conduitAdmissionKeyInfo is the HKDF label of the admission token key. It
// differs from every other derivation of the session secret and from the
// relay-peer secret's.
const conduitAdmissionKeyInfo = "scion conduit relay admission token aead v2"

// conduitAdmissionAAD binds a sealed token to its use.
const conduitAdmissionAAD = "scion conduit relay admission token"

// conduitAdmissionTokenVersion is the claims format version.
const conduitAdmissionTokenVersion = 2

// errConduitAdmissionNoKey refuses admission tokens without a shared
// session secret: tokens from one hub replica must open on every other.
var errConduitAdmissionNoKey = errors.New("conduit admission tokens need the shared session secret (--session-secret or SCION_SERVER_SESSION_SECRET)")

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

// conduitAdmissionAEAD returns the admission token cipher, derived on
// first use from the shared session secret.
func (s *Server) conduitAdmissionAEAD() (cipher.AEAD, error) {
	s.conduitAdmissionKeyOnce.Do(func() {
		secret := s.config.SharedSigningSecret
		if secret == "" {
			s.conduitAdmissionKeyErr = errConduitAdmissionNoKey
			return
		}
		key, err := hkdf.Key(sha256.New, []byte(secret), nil, conduitAdmissionKeyInfo, 32)
		if err != nil {
			s.conduitAdmissionKeyErr = err
			return
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			s.conduitAdmissionKeyErr = err
			return
		}
		s.conduitAdmissionAEADv, s.conduitAdmissionKeyErr = cipher.NewGCM(block)
	})
	return s.conduitAdmissionAEADv, s.conduitAdmissionKeyErr
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

// mintAdmissionToken seals c.
func (s *Server) mintAdmissionToken(c conduitAdmissionClaims) (string, error) {
	aead, err := s.conduitAdmissionAEAD()
	if err != nil {
		return "", err
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
	sealed := aead.Seal(nonce, nonce, payload, []byte(conduitAdmissionAAD))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openAdmissionToken authenticates and decrypts token (nil, false on any
// failure).
func (s *Server) openAdmissionToken(token string) ([]byte, bool) {
	aead, err := s.conduitAdmissionAEAD()
	if err != nil {
		return nil, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, false
	}
	payload, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(conduitAdmissionAAD))
	if err != nil {
		return nil, false
	}
	return payload, true
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
