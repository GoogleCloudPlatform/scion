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
	"errors"
	"fmt"
	"strings"
	"time"
)

// RecheckAllowance is how far past a grant's authz_until a re-check still
// accepts it, and how far in the future its nbf may be: the targets' clock
// allowance (design §3.5, Re-check).
const RecheckAllowance = 30 * time.Second

// ErrRecheckWindow refuses a grant presented to a re-check after its
// authz_until plus RecheckAllowance.
var ErrRecheckWindow = errors.New("grant: past its renewal window")

// RecheckClaims is what a re-check takes from the presented grant: the
// stream's principal, project, target, kind and params, always from the
// grant's signed claims.
type RecheckClaims struct {
	// Renewal is true when the presented grant is a renewal grant, false
	// for a stream (open) grant.
	Renewal   bool
	KeyID     string
	Issuer    string
	Subject   string
	ProjectID string
	Target    Target
	Stream    StreamHeader
	// OpenJTI is the jti of the stream's open grant: the presented grant's
	// own jti for an open grant, its open_jti claim for a renewal.
	OpenJTI string
	// StreamID is the renewal's stream_id; zero for an open grant.
	StreamID   uint32
	AuthzUntil time.Time
	NotBefore  time.Time
	Expiry     time.Time
}

// RecheckExpectation is what the hub knows about the stream independently
// of the presented grant.
type RecheckExpectation struct {
	// Issuer must equal the grant's iss. Required.
	Issuer string
	// Target is the stream's target session as the hub's registry records
	// it (kind, id, admitted incarnation, session id, epoch). Required.
	Target Target
	// OpenJTI is the stream's open-grant jti. Required.
	OpenJTI string
	// StreamID is the target-side stream id. Required; a renewal's
	// stream_id must equal it (an open grant carries none).
	StreamID uint32
}

// VerifyRecheck verifies token for a re-check, a mode distinct from
// admission (design §3.5, Re-check). It accepts a stream grant or a renewal
// grant, checks the signature against keys (the hub's re-check key set:
// published and retained keys), iss and aud, and the binding: target
// session, epoch and incarnation, open_jti, and stream_id for a renewal. A
// grant whose exp has passed is accepted while now is at most its
// authz_until plus RecheckAllowance; nbf may be at most RecheckAllowance in
// the future. No jti is consumed: a re-check is not an admission.
func VerifyRecheck(token []byte, keys KeySet, expect RecheckExpectation, now time.Time) (*RecheckClaims, error) {
	if keys == nil || expect.Issuer == "" || !expect.Target.complete() || expect.OpenJTI == "" || expect.StreamID == 0 {
		return nil, ErrExpectation
	}
	typ, err := peekTokenType(token)
	if err != nil {
		return nil, err
	}
	payload, kid, err := verifySigned(token, keys, typ, now)
	if err != nil {
		return nil, err
	}
	var c *RecheckClaims
	switch typ {
	case TokenType:
		var w wireClaims
		if err := decodeStrict(payload, &w); err != nil || !canonical(payload, w) {
			return nil, ErrMalformed
		}
		oc := Claims{
			Issuer: w.Issuer, Audience: w.Audience, KeyID: w.KeyID, JTI: w.JTI,
			Subject: w.Subject, ProjectID: w.ProjectID, Target: w.Target, Stream: w.Stream,
			NotBefore: time.Unix(w.NotBefore, 0), Expiry: time.Unix(w.Expiry, 0),
			AuthzUntil: timeOrZero(w.AuthzUntil), AuthzInterval: time.Duration(w.AuthzInterval) * time.Second,
		}
		if oc.Audience != Audience {
			return nil, ErrAudience
		}
		if err := oc.validate(); err != nil {
			return nil, err
		}
		if oc.AuthzUntil.IsZero() {
			return nil, fmt.Errorf("%w: a stream grant without authz_until cannot be renewed", ErrAuthzUntil)
		}
		c = &RecheckClaims{
			KeyID: w.KeyID, Issuer: oc.Issuer, Subject: oc.Subject, ProjectID: oc.ProjectID,
			Target: oc.Target, Stream: oc.Stream, OpenJTI: oc.JTI,
			AuthzUntil: oc.AuthzUntil, NotBefore: oc.NotBefore, Expiry: oc.Expiry,
		}
	case RenewalTokenType:
		var w wireRenewal
		if err := decodeStrict(payload, &w); err != nil || !canonical(payload, w) {
			return nil, ErrMalformed
		}
		rc := RenewalClaims{
			Issuer: w.Issuer, Audience: w.Audience, KeyID: w.KeyID, JTI: w.JTI,
			Subject: w.Subject, ProjectID: w.ProjectID, Target: w.Target, Stream: w.Stream,
			StreamID: w.StreamID, OpenJTI: w.OpenJTI, AuthzUntil: timeOrZero(w.AuthzUntil),
			NotBefore: time.Unix(w.NotBefore, 0), Expiry: time.Unix(w.Expiry, 0),
		}
		if rc.Audience != Audience {
			return nil, ErrAudience
		}
		if err := rc.validate(); err != nil {
			return nil, err
		}
		if rc.StreamID != expect.StreamID {
			return nil, ErrStreamBinding
		}
		c = &RecheckClaims{
			Renewal: true, KeyID: w.KeyID, Issuer: rc.Issuer, Subject: rc.Subject, ProjectID: rc.ProjectID,
			Target: rc.Target, Stream: rc.Stream, OpenJTI: rc.OpenJTI, StreamID: rc.StreamID,
			AuthzUntil: rc.AuthzUntil, NotBefore: rc.NotBefore, Expiry: rc.Expiry,
		}
	default:
		return nil, ErrMalformed
	}
	if c.KeyID != kid {
		return nil, fmt.Errorf("%w: payload kid does not match header", ErrMalformed)
	}
	if c.Issuer != expect.Issuer {
		return nil, ErrIssuer
	}
	if now.Add(RecheckAllowance).Before(c.NotBefore) {
		return nil, ErrNotYetValid
	}
	if now.After(c.AuthzUntil.Add(RecheckAllowance)) {
		return nil, ErrRecheckWindow
	}
	if c.Target != expect.Target {
		return nil, ErrTarget
	}
	if c.OpenJTI != expect.OpenJTI {
		return nil, ErrStreamBinding
	}
	return c, nil
}

// peekTokenType returns the typ of token's header, one of TokenType and
// RenewalTokenType; anything else is malformed. verifySigned then checks
// the header in full (canonical encoding, alg, kid) and the signature.
func peekTokenType(token []byte) (string, error) {
	if len(token) == 0 || len(token) > maxTokenSize || !tokenCharsetOK(token) {
		return "", ErrMalformed
	}
	head, _, ok := strings.Cut(string(token), ".")
	if !ok {
		return "", ErrMalformed
	}
	raw, err := b64.DecodeString(head)
	if err != nil {
		return "", ErrMalformed
	}
	var hdr header
	if err := decodeStrict(raw, &hdr); err != nil || !canonical(raw, hdr) {
		return "", ErrMalformed
	}
	switch hdr.Typ {
	case TokenType, RenewalTokenType:
		return hdr.Typ, nil
	}
	return "", ErrMalformed
}
