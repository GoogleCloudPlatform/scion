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
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The agent and user JWT purposes of the signing ring (ptone/scion#1530
// Phase 3). They keep the existing logical key names and the existing
// shared-secret derivation, so a ring's "legacy" generation derives exactly
// the keys a legacy-mode hub derives from SharedSigningSecret: JWTs minted
// before the keyring file keep validating.
var (
	agentJWTPurpose = keyring.NewPurpose(SecretKeyAgentSigningKey, func(secret []byte) []byte {
		return deriveSharedSigningKey(string(secret), SecretKeyAgentSigningKey)
	})
	userJWTPurpose = keyring.NewPurpose(SecretKeyUserSigningKey, func(secret []byte) []byte {
		return deriveSharedSigningKey(string(secret), SecretKeyUserSigningKey)
	})
)

// errJWTNoEligibleKey is the verification error of a ring-mode JWT that no
// eligible generation verifies: an unknown or retired key ID, an ID-less
// token no legacy reader verifies, or a bad signature. It names no key ID,
// key or fingerprint (the key ID is attacker-supplied input).
var errJWTNoEligibleKey = errors.New("no eligible signing generation verifies the token")

// ringJWTKeys signs and verifies one purpose's HS256 JWTs from the signing
// ring (design §3, HS256 JWTs row). Exactly one active generation issues;
// verification uses only the generation the token's protected "kid" header
// names, or the bounded legacy readers for an ID-less token, and never
// falls back to other keys. Deadlines are enforced on every call.
type ringJWTKeys struct {
	ring    *keyring.SigningRing
	purpose keyring.Purpose
	// newSigner builds the HS256 signer. Each purpose's constructor wraps
	// jose.NewSigner itself, so the key-material census
	// (TestSigningKeySource_EveryKeySourceClassified) sees one classified
	// call site per signing-key class.
	newSigner func(jose.SigningKey, *jose.SignerOptions) (jose.Signer, error)
}

// sign serializes claims under the generation allowed to issue at now. It
// refuses at or after the active generation's sign_until
// (keyring.ErrIssuanceClosed) and for a ring without an active generation.
// The protected header carries kid = the generation ID only when the ring
// emits versioned formats; with emit legacy it is exactly the pre-keyring
// header (alg HS256, typ JWT).
func (k ringJWTKeys) sign(claims any, now time.Time) (string, error) {
	active, err := k.ring.Active(k.purpose, now)
	if err != nil {
		return "", err
	}
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if k.ring.Emit() == keyring.EmitVersioned {
		// go-jose writes a JWK's KeyID only for asymmetric keys, so an
		// HS256 kid goes in as an explicit protected header.
		opts = opts.WithHeader(jose.HeaderKey("kid"), active.ID)
	}
	if k.newSigner == nil {
		return "", errors.New("no JWT signer configured")
	}
	signer, err := k.newSigner(jose.SigningKey{Algorithm: jose.HS256, Key: active.Bytes()}, opts)
	if err != nil {
		return "", fmt.Errorf("creating signer: %w", err)
	}
	return jwt.Signed(signer).Claims(claims).Serialize()
}

// verify checks tok's signature against the eligible generations at now
// and decodes its claims into dst. Claims validation (issuer, audience,
// expiry) is the caller's, as before.
func (k ringJWTKeys) verify(tok *jwt.JSONWebToken, now time.Time, dst any) error {
	if len(tok.Headers) != 1 {
		return errJWTNoEligibleKey
	}
	// A compact JWS has protected headers only: kid is covered by the MAC.
	kid := tok.Headers[0].KeyID
	for _, c := range k.ring.Candidates(k.purpose, kid, now) {
		if err := tok.Claims(c.Bytes(), dst); err == nil {
			return nil
		}
	}
	return errJWTNoEligibleKey
}

// generationIDs is the non-secret description of the ring for logs.
func (k ringJWTKeys) generationIDs() (active string, ids []string) {
	return k.ring.ActiveID(), k.ring.IDs()
}
