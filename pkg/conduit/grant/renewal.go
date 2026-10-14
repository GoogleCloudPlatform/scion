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

package grant

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"
)

// RenewalTokenType is the only accepted JWS typ of a renewal grant. It
// differs from TokenType, so a renewal grant is never accepted as a stream
// grant and a stream grant is never accepted as a renewal.
const RenewalTokenType = "conduit-renewal+jwt"

// RenewalClaims is the payload of a renewal grant: the hub's renewal of one
// open stream's authorization deadline (design §3.5, "Enforcement point").
// It is signed with the same key ring as stream grants and is bound to the
// stream it renews twice over: by StreamID, the id the target knows the
// stream by, and by OpenJTI, the jti of the grant the stream was opened
// with, together with the target's session, epoch and incarnation.
type RenewalClaims struct {
	Issuer    string
	Audience  string
	KeyID     string
	JTI       string
	Subject   string
	ProjectID string
	Target    Target
	StreamID  uint32
	OpenJTI   string
	// AuthzUntil is the stream's new authorization deadline.
	AuthzUntil time.Time
	NotBefore  time.Time
	Expiry     time.Time
}

// wireRenewal is the JSON form of RenewalClaims. Times are Unix seconds.
type wireRenewal struct {
	Issuer     string `json:"iss"`
	Audience   string `json:"aud"`
	KeyID      string `json:"kid"`
	JTI        string `json:"jti"`
	Subject    string `json:"sub"`
	ProjectID  string `json:"project_id"`
	Target     Target `json:"target"`
	StreamID   uint32 `json:"stream_id"`
	OpenJTI    string `json:"open_jti"`
	AuthzUntil int64  `json:"authz_until"`
	NotBefore  int64  `json:"nbf"`
	Expiry     int64  `json:"exp"`
}

func (c *RenewalClaims) validate() error {
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
	case c.StreamID == 0:
		return fmt.Errorf("%w: missing stream_id", ErrInvalidClaims)
	case c.OpenJTI == "":
		return fmt.Errorf("%w: missing open_jti", ErrInvalidClaims)
	case c.NotBefore.IsZero() || c.Expiry.IsZero() || c.AuthzUntil.IsZero():
		return fmt.Errorf("%w: missing nbf, exp or authz_until", ErrInvalidClaims)
	case !c.Expiry.After(c.NotBefore):
		return fmt.Errorf("%w: exp must be after nbf", ErrInvalidClaims)
	case c.Expiry.Sub(c.NotBefore) > MaxValidity:
		return fmt.Errorf("%w: exp-nbf %s > %s", ErrValidityWindow, c.Expiry.Sub(c.NotBefore), MaxValidity)
	case !c.AuthzUntil.After(c.NotBefore):
		return fmt.Errorf("%w: authz_until must be after nbf", ErrInvalidClaims)
	}
	return nil
}

// MintRenewal signs a renewal grant with signer. Like Mint it fills
// Audience (when empty), KeyID and a random JTI (when empty), truncates
// times to whole seconds and refuses claims VerifyRenewal would refuse on
// shape alone.
func MintRenewal(signer *Signer, c RenewalClaims) ([]byte, error) {
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
	c.AuthzUntil = c.AuthzUntil.Truncate(time.Second)
	if err := c.validate(); err != nil {
		return nil, err
	}
	return sign(signer, RenewalTokenType, wireRenewal{
		Issuer: c.Issuer, Audience: c.Audience, KeyID: c.KeyID, JTI: c.JTI,
		Subject: c.Subject, ProjectID: c.ProjectID, Target: c.Target,
		StreamID: c.StreamID, OpenJTI: c.OpenJTI, AuthzUntil: c.AuthzUntil.Unix(),
		NotBefore: c.NotBefore.Unix(), Expiry: c.Expiry.Unix(),
	})
}

// RenewalExpectation is what the target knows about the stream a renewal
// names, independently of the renewal: the verified claims of the grant it
// was opened with, its stream id, and its current deadline.
type RenewalExpectation struct {
	// Target, ProjectID and Subject must equal the renewal's. A target
	// takes them from the stream's verified open grant (whose target
	// equalled its own identity and Welcome binding). Required.
	Target    Target
	ProjectID string
	Subject   string
	// StreamID and OpenJTI name the stream: the id the renewal arrived
	// for and the jti of its open grant. Required.
	StreamID uint32
	OpenJTI  string
	// Issuer, when non-empty, must equal the renewal's iss.
	Issuer string
	// After is the stream's current deadline: authz_until must be later.
	// Required.
	After time.Time
	// NotAfter caps authz_until. Required.
	NotAfter time.Time
	// ClockSkew tolerates that much clock difference on nbf and exp. It
	// is capped at MaxClockSkew; zero means none.
	ClockSkew time.Duration
}

// VerifyRenewal checks token as a renewal grant against keys and expect and,
// on success, consumes its jti in replay. As with Verify, the jti is
// consumed only after every other check has passed, and an unknown kid
// fails closed. The caller moves the stream's deadline to the returned
// AuthzUntil only after a nil error.
func VerifyRenewal(ctx context.Context, token []byte, keys KeySet, expect RenewalExpectation, replay ReplayCache, now time.Time) (*RenewalClaims, error) {
	if keys == nil || replay == nil {
		return nil, fmt.Errorf("%w: nil key set or replay cache", ErrExpectation)
	}
	if !expect.Target.complete() || expect.ProjectID == "" || expect.Subject == "" ||
		expect.StreamID == 0 || expect.OpenJTI == "" || expect.After.IsZero() || expect.NotAfter.IsZero() {
		return nil, ErrExpectation
	}
	skew := min(max(expect.ClockSkew, 0), MaxClockSkew)

	payload, kid, err := verifySigned(token, keys, RenewalTokenType, now)
	if err != nil {
		return nil, err
	}
	var w wireRenewal
	if err := decodeStrict(payload, &w); err != nil {
		return nil, ErrMalformed
	}
	if !canonical(payload, w) {
		return nil, ErrMalformed
	}
	c := &RenewalClaims{
		Issuer: w.Issuer, Audience: w.Audience, KeyID: w.KeyID, JTI: w.JTI,
		Subject: w.Subject, ProjectID: w.ProjectID, Target: w.Target,
		StreamID: w.StreamID, OpenJTI: w.OpenJTI, AuthzUntil: timeOrZero(w.AuthzUntil),
		NotBefore: time.Unix(w.NotBefore, 0), Expiry: time.Unix(w.Expiry, 0),
	}
	if c.KeyID != kid {
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
	if c.Subject != expect.Subject {
		return nil, ErrSubject
	}
	if c.StreamID != expect.StreamID || c.OpenJTI != expect.OpenJTI {
		return nil, ErrStreamBinding
	}
	if !c.AuthzUntil.After(expect.After) || c.AuthzUntil.After(expect.NotAfter) {
		return nil, ErrAuthzUntil
	}

	fresh, err := replay.Consume(ctx, c.JTI, c.Expiry.Add(skew))
	if err != nil {
		return nil, fmt.Errorf("%w: replay cache: %v", ErrReplay, err)
	}
	if !fresh {
		return nil, ErrReplay
	}
	return c, nil
}
