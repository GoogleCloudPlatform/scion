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

// Package grant mints and verifies Conduit stream grants (design §3.5,
// "Grants (trust boundary)").
//
// The hub authorizes a stream, then mints a short-lived grant that travels in
// StreamOpen.grant. The target (sciontool or a broker) verifies it before it
// acts; relays never inspect it. A grant is a compact JWS signed with a
// dedicated Ed25519 key. It is never signed with the hub's HS256 agent/user
// token secret, which has no public half.
//
// This package is a pure library. It has no hub, store or proto imports, and
// it shares only the canonical lowercase strings of the Conduit contract
// (stream kinds, principal kinds) with the other Conduit packages.
//
// Token format: base64url(header) "." base64url(payload) "." base64url(sig),
// unpadded. The header is exactly {"alg":"EdDSA","typ":"conduit-grant+jwt",
// "kid":"..."}. The algorithm is pinned: any other alg value, an unknown
// header field or an unknown kid fails closed.
package grant

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"
)

const (
	// Audience is the only accepted aud value.
	Audience = "conduit-target"
	// Algorithm is the only accepted JWS alg value.
	Algorithm = "EdDSA"
	// TokenType is the only accepted JWS typ value. It keeps a grant from
	// being confused with any other JWT the hub issues.
	TokenType = "conduit-grant+jwt"
	// MaxValidity bounds exp−nbf. Mint and Verify both enforce it.
	MaxValidity = 60 * time.Second
	// MaxClockSkew caps Expectation.ClockSkew.
	MaxClockSkew = 10 * time.Second
	// maxTokenSize bounds the input Verify will parse.
	maxTokenSize = 16 << 10
)

// Canonical stream kinds (contracts.md §2).
const (
	StreamKindTCP    = "tcp"
	StreamKindPTY    = "pty"
	StreamKindSSH    = "ssh"
	StreamKindLogs   = "logs"
	StreamKindEvents = "events"
)

// Canonical target principal kinds (contracts.md §2). Only brokers and agents
// accept streams.
const (
	TargetKindBroker = "broker"
	TargetKindAgent  = "agent"
)

// TCP stream params. A tcp grant names exactly one agent-local target.
const (
	ParamHost = "host"
	ParamPort = "port"
)

// IsStreamKind reports whether kind is one of the canonical stream kinds.
func IsStreamKind(kind string) bool {
	switch kind {
	case StreamKindTCP, StreamKindPTY, StreamKindSSH, StreamKindLogs, StreamKindEvents:
		return true
	}
	return false
}

// Errors returned by Mint and Verify. Verify wraps one of these, so callers
// can map them to close codes with errors.Is. Every failure is a refusal.
var (
	ErrMalformed      = errors.New("grant: malformed token")
	ErrAlgorithm      = errors.New("grant: algorithm not allowed")
	ErrUnknownKey     = errors.New("grant: unknown key id")
	ErrKeyExpired     = errors.New("grant: signing key past not_after")
	ErrSignature      = errors.New("grant: invalid signature")
	ErrAudience       = errors.New("grant: wrong audience")
	ErrIssuer         = errors.New("grant: wrong issuer")
	ErrNotYetValid    = errors.New("grant: not yet valid")
	ErrExpired        = errors.New("grant: expired")
	ErrValidityWindow = errors.New("grant: validity window exceeds maximum")
	ErrProject        = errors.New("grant: wrong project")
	ErrTarget         = errors.New("grant: wrong target")
	ErrStream         = errors.New("grant: stream does not match grant")
	ErrReplay         = errors.New("grant: jti already used")
	ErrExpectation    = errors.New("grant: incomplete verification expectation")
	ErrInvalidClaims  = errors.New("grant: invalid claims")
)

// StreamHeader is the authority-bearing part of a StreamOpen: its kind and
// params. stream_id, initial_window and open_timeout are deliberately not
// part of it: they are assigned after minting and carry no authority.
type StreamHeader struct {
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
}

// Equal reports whether h and o have the same kind and the same params as a
// key→value map. A nil map equals an empty one.
func (h StreamHeader) Equal(o StreamHeader) bool {
	return h.Kind == o.Kind && maps.Equal(h.Params, o.Params)
}

// Target is the session the grant is aimed at. A target accepts the grant
// only if every field equals its own values (ID and incarnation from its
// identity, session_id and connection_epoch from its Welcome).
type Target struct {
	Kind                string `json:"kind"`
	ID                  string `json:"id"`
	EndpointIncarnation string `json:"endpoint_incarnation"`
	SessionID           string `json:"session_id"`
	ConnectionEpoch     int64  `json:"connection_epoch"`
}

func (t Target) complete() bool {
	return (t.Kind == TargetKindAgent || t.Kind == TargetKindBroker) &&
		t.ID != "" && t.EndpointIncarnation != "" && t.SessionID != "" && t.ConnectionEpoch > 0
}

// Claims is the grant payload (design v2.1 §3.5).
type Claims struct {
	Issuer    string
	Audience  string
	KeyID     string
	JTI       string
	Subject   string
	ProjectID string
	Target    Target
	Stream    StreamHeader
	NotBefore time.Time
	Expiry    time.Time
}

// wireClaims is the JSON form of Claims. Times are Unix seconds.
type wireClaims struct {
	Issuer    string       `json:"iss"`
	Audience  string       `json:"aud"`
	KeyID     string       `json:"kid"`
	JTI       string       `json:"jti"`
	Subject   string       `json:"sub"`
	ProjectID string       `json:"project_id"`
	Target    Target       `json:"target"`
	Stream    StreamHeader `json:"stream"`
	NotBefore int64        `json:"nbf"`
	Expiry    int64        `json:"exp"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

var b64 = base64.RawURLEncoding.Strict()

// validate checks the claim invariants shared by Mint and Verify.
func (c *Claims) validate() error {
	switch {
	case c.Issuer == "":
		return fmt.Errorf("%w: missing iss", ErrInvalidClaims)
	case c.JTI == "":
		return fmt.Errorf("%w: missing jti", ErrInvalidClaims)
	case c.Subject == "":
		return fmt.Errorf("%w: missing sub", ErrInvalidClaims)
	case c.ProjectID == "":
		return fmt.Errorf("%w: missing project_id", ErrInvalidClaims)
	case !c.Target.complete():
		return fmt.Errorf("%w: incomplete target", ErrInvalidClaims)
	case !IsStreamKind(c.Stream.Kind):
		return fmt.Errorf("%w: unknown stream kind %q", ErrInvalidClaims, c.Stream.Kind)
	case c.NotBefore.IsZero() || c.Expiry.IsZero():
		return fmt.Errorf("%w: missing nbf or exp", ErrInvalidClaims)
	case !c.Expiry.After(c.NotBefore):
		return fmt.Errorf("%w: exp must be after nbf", ErrInvalidClaims)
	case c.Expiry.Sub(c.NotBefore) > MaxValidity:
		return fmt.Errorf("%w: exp-nbf %s > %s", ErrValidityWindow, c.Expiry.Sub(c.NotBefore), MaxValidity)
	}
	return nil
}

// NewJTI returns a random 128-bit grant ID.
func NewJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Mint signs claims with signer. It fills Audience (when empty) and KeyID
// from the signer, a random JTI when empty, and rejects any claims Verify
// would refuse on shape alone, including exp−nbf > MaxValidity. Times are
// truncated to whole seconds before the window is checked.
func Mint(signer *Signer, c Claims) ([]byte, error) {
	if signer == nil || signer.KeyID == "" || len(signer.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("grant: invalid signer")
	}
	if c.Audience == "" {
		c.Audience = Audience
	}
	if c.Audience != Audience {
		return nil, fmt.Errorf("%w: aud must be %q", ErrAudience, Audience)
	}
	if c.KeyID != "" && c.KeyID != signer.KeyID {
		return nil, fmt.Errorf("%w: kid does not match signer", ErrInvalidClaims)
	}
	c.KeyID = signer.KeyID
	if c.JTI == "" {
		jti, err := NewJTI()
		if err != nil {
			return nil, fmt.Errorf("grant: generate jti: %w", err)
		}
		c.JTI = jti
	}
	c.NotBefore = c.NotBefore.Truncate(time.Second)
	c.Expiry = c.Expiry.Truncate(time.Second)
	if err := c.validate(); err != nil {
		return nil, err
	}

	hdr, err := json.Marshal(header{Alg: Algorithm, Typ: TokenType, Kid: signer.KeyID})
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(wireClaims{
		Issuer: c.Issuer, Audience: c.Audience, KeyID: c.KeyID, JTI: c.JTI,
		Subject: c.Subject, ProjectID: c.ProjectID, Target: c.Target, Stream: c.Stream,
		NotBefore: c.NotBefore.Unix(), Expiry: c.Expiry.Unix(),
	})
	if err != nil {
		return nil, err
	}
	signingInput := b64.EncodeToString(hdr) + "." + b64.EncodeToString(payload)
	sig := ed25519.Sign(signer.Key, []byte(signingInput))
	return []byte(signingInput + "." + b64.EncodeToString(sig)), nil
}

// Expectation is what the target knows independently of the grant.
type Expectation struct {
	// Target holds the target's own identity, incarnation, and the
	// session_id and connection_epoch from its current Welcome. Every field
	// is required.
	Target Target
	// Header is the kind and params of the StreamOpen being admitted.
	Header StreamHeader
	// ProjectID is the target's project. Required.
	ProjectID string
	// Issuer, when non-empty, must equal the grant's iss.
	Issuer string
	// ClockSkew tolerates that much clock difference on nbf and exp. It is
	// capped at MaxClockSkew; zero means none.
	ClockSkew time.Duration
}

// Verify checks token against keys and expect, and on success consumes its
// jti in replay. It returns the verified claims. The checks run in a fixed
// order, and the jti is consumed only after every other check has passed, so
// a refused grant never burns its jti. The caller must perform the side
// effect (spawning a pty, dialing a port) only after Verify returns nil.
func Verify(ctx context.Context, token []byte, keys KeySet, expect Expectation, replay ReplayCache, now time.Time) (*Claims, error) {
	if keys == nil || replay == nil {
		return nil, fmt.Errorf("%w: nil key set or replay cache", ErrExpectation)
	}
	if !expect.Target.complete() || expect.ProjectID == "" || !IsStreamKind(expect.Header.Kind) {
		return nil, ErrExpectation
	}
	skew := min(max(expect.ClockSkew, 0), MaxClockSkew)

	if len(token) == 0 || len(token) > maxTokenSize {
		return nil, ErrMalformed
	}
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	hdrBytes, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var hdr header
	if err := decodeStrict(hdrBytes, &hdr); err != nil {
		return nil, ErrMalformed
	}
	if hdr.Alg != Algorithm {
		return nil, ErrAlgorithm
	}
	if hdr.Typ != TokenType || hdr.Kid == "" {
		return nil, ErrMalformed
	}
	pub, ok := keys.Lookup(hdr.Kid)
	if !ok || len(pub.Key) != ed25519.PublicKeySize {
		return nil, ErrUnknownKey
	}
	if !pub.NotAfter.IsZero() && !now.Before(pub.NotAfter) {
		return nil, ErrKeyExpired
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrSignature
	}
	if !ed25519.Verify(pub.Key, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, ErrSignature
	}

	// Only signed bytes are decoded past this point.
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var w wireClaims
	if err := decodeStrict(payload, &w); err != nil {
		return nil, ErrMalformed
	}
	c := &Claims{
		Issuer: w.Issuer, Audience: w.Audience, KeyID: w.KeyID, JTI: w.JTI,
		Subject: w.Subject, ProjectID: w.ProjectID, Target: w.Target, Stream: w.Stream,
		NotBefore: time.Unix(w.NotBefore, 0), Expiry: time.Unix(w.Expiry, 0),
	}
	if c.KeyID != hdr.Kid {
		return nil, fmt.Errorf("%w: payload kid does not match header", ErrMalformed)
	}
	if c.Audience != Audience {
		return nil, ErrAudience
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if expect.Issuer != "" && c.Issuer != expect.Issuer {
		return nil, ErrIssuer
	}
	if now.Add(skew).Before(c.NotBefore) {
		return nil, ErrNotYetValid
	}
	if !now.Add(-skew).Before(c.Expiry) {
		return nil, ErrExpired
	}
	if c.ProjectID != expect.ProjectID {
		return nil, ErrProject
	}
	if c.Target != expect.Target {
		return nil, ErrTarget
	}
	if !c.Stream.Equal(expect.Header) {
		return nil, ErrStream
	}

	// Final step: consume the jti. Exactly one concurrent caller wins.
	fresh, err := replay.Consume(ctx, c.JTI, c.Expiry.Add(skew))
	if err != nil {
		return nil, fmt.Errorf("%w: replay cache: %v", ErrReplay, err)
	}
	if !fresh {
		return nil, ErrReplay
	}
	return c, nil
}

// decodeStrict decodes exactly one JSON object with no unknown fields and no
// trailing data.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}
