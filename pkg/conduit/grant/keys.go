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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// PublicKey is a verification key as published to targets. It matches
// Welcome.grant_keys[] {kid, public_key, not_after}. A zero NotAfter means
// the key has no scheduled retirement yet.
type PublicKey struct {
	KeyID    string
	Key      ed25519.PublicKey
	NotAfter time.Time
}

// KeySet resolves a kid to a verification key. An unknown kid must return
// false; Verify then fails closed.
type KeySet interface {
	Lookup(kid string) (PublicKey, bool)
}

type staticKeySet map[string]PublicKey

func (s staticKeySet) Lookup(kid string) (PublicKey, bool) {
	k, ok := s[kid]
	return k, ok
}

// NewKeySet returns an immutable KeySet over keys. It rejects duplicate or
// empty kids and keys of the wrong size. A target builds one from the
// grant_keys of each Welcome (or token refresh), replacing the previous set.
func NewKeySet(keys ...PublicKey) (KeySet, error) {
	s := make(staticKeySet, len(keys))
	for _, k := range keys {
		if k.KeyID == "" {
			return nil, errors.New("grant: key with empty kid")
		}
		if len(k.Key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("grant: key %q has invalid size %d", k.KeyID, len(k.Key))
		}
		if _, dup := s[k.KeyID]; dup {
			return nil, fmt.Errorf("grant: duplicate kid %q", k.KeyID)
		}
		k.Key = append(ed25519.PublicKey(nil), k.Key...)
		s[k.KeyID] = k
	}
	return s, nil
}

// Signer is a private grant signing key. It never leaves the hub.
type Signer struct {
	KeyID string
	Key   ed25519.PrivateKey
}

// String redacts the private key.
func (s Signer) String() string { return "grant.Signer{kid:" + s.KeyID + "}" }

// GoString redacts the private key.
func (s Signer) GoString() string { return s.String() }

// LogValue redacts the private key.
func (s Signer) LogValue() slog.Value { return slog.GroupValue(slog.String("kid", s.KeyID)) }

// RingKey is one key in a KeyRing. Seed is the 32-byte Ed25519 seed and is
// secret; it appears only in the persisted JSON form of the ring. Once a
// later key has activated, the seed is dropped (see KeyRing.Settle) and only
// PublicKey remains: the key still verifies until NotAfter but can no
// longer sign.
type RingKey struct {
	KeyID string `json:"kid"`
	Seed  []byte `json:"seed,omitempty"`
	// PublicKey is the raw 32-byte public key, set when Seed is dropped.
	PublicKey []byte    `json:"public_key,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// ActivateAt is when the hub may start signing with this key. A rotated
	// key is published before it signs, so targets that refresh their key
	// set in the meantime already trust it.
	ActivateAt time.Time `json:"activate_at"`
	// NotAfter is when the key is retired. Zero means no retirement is
	// scheduled. Grants signed with the key are refused from NotAfter on.
	NotAfter time.Time `json:"not_after,omitzero"`
}

// String redacts the seed.
func (k RingKey) String() string { return "grant.RingKey{kid:" + k.KeyID + "}" }

// GoString redacts the seed.
func (k RingKey) GoString() string { return k.String() }

// LogValue redacts the seed.
func (k RingKey) LogValue() slog.Value { return slog.GroupValue(slog.String("kid", k.KeyID)) }

// canSign reports whether k still holds its private seed.
func (k RingKey) canSign() bool { return len(k.Seed) == ed25519.SeedSize }

// publicBytes returns k's public key, from the seed when present.
func (k RingKey) publicBytes() ed25519.PublicKey {
	if k.canSign() {
		return ed25519.NewKeyFromSeed(k.Seed).Public().(ed25519.PublicKey)
	}
	return append(ed25519.PublicKey(nil), k.PublicKey...)
}

func (k RingKey) publicKey() PublicKey {
	return PublicKey{KeyID: k.KeyID, Key: k.publicBytes(), NotAfter: k.NotAfter}
}

func (k RingKey) liveAt(now time.Time) bool {
	return k.NotAfter.IsZero() || now.Before(k.NotAfter)
}

// NewRingKey returns a key with a random seed and kid, active from
// activateAt.
func NewRingKey(now, activateAt time.Time) (RingKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return RingKey{}, fmt.Errorf("grant: generate seed: %w", err)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return RingKey{}, fmt.Errorf("grant: generate kid: %w", err)
	}
	return RingKey{KeyID: "cg-" + hex.EncodeToString(id[:]), Seed: seed, CreatedAt: now.UTC(), ActivateAt: activateAt.UTC()}, nil
}

// KeyRing is the hub's set of grant signing keys, keyed by kid. It is a
// plain value: the hub persists it (as JSON) and serializes rotation.
//
// Retained holds the public halves of keys past their not_after. They are
// kept only for re-check verification (RecheckKeys), are never published
// (PublicKeys) and can never sign.
type KeyRing struct {
	Keys     []RingKey     `json:"keys"`
	Retained []RetainedKey `json:"retained,omitempty"`
}

// RetainedKey is the public half of a key past its not_after, kept until
// RetainUntil for re-check verification only.
type RetainedKey struct {
	KeyID       string    `json:"kid"`
	PublicKey   []byte    `json:"public_key"`
	RetiredAt   time.Time `json:"retired_at"`
	RetainUntil time.Time `json:"retain_until"`
}

// String redacts every seed.
func (r KeyRing) String() string {
	ids := make([]string, len(r.Keys))
	for i, k := range r.Keys {
		ids[i] = k.KeyID
	}
	ret := make([]string, len(r.Retained))
	for i, k := range r.Retained {
		ret[i] = k.KeyID
	}
	return fmt.Sprintf("grant.KeyRing{kids:%v retained:%v}", ids, ret)
}

// GoString redacts every seed.
func (r KeyRing) GoString() string { return r.String() }

// Validate checks that kids are unique and non-empty across Keys and
// Retained, that every key has either a 32-byte seed or (without one) a
// 32-byte public key, and that retained keys hold no seed.
func (r *KeyRing) Validate() error {
	seen := make(map[string]bool, len(r.Keys)+len(r.Retained))
	for _, k := range r.Keys {
		if k.KeyID == "" {
			return errors.New("grant: ring key with empty kid")
		}
		if seen[k.KeyID] {
			return fmt.Errorf("grant: duplicate ring kid %q", k.KeyID)
		}
		seen[k.KeyID] = true
		switch {
		case len(k.Seed) == ed25519.SeedSize:
		case len(k.Seed) == 0 && len(k.PublicKey) == ed25519.PublicKeySize:
		default:
			return fmt.Errorf("grant: ring key %q has neither a valid seed nor a valid public key", k.KeyID)
		}
	}
	for _, k := range r.Retained {
		if k.KeyID == "" {
			return errors.New("grant: retained key with empty kid")
		}
		if seen[k.KeyID] {
			return fmt.Errorf("grant: duplicate ring kid %q", k.KeyID)
		}
		seen[k.KeyID] = true
		if len(k.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("grant: retained key %q has invalid public key size", k.KeyID)
		}
	}
	return nil
}

// ErrNoSigningKey means the ring has no key that may sign at now.
var ErrNoSigningKey = errors.New("grant: no active signing key")

// Signer returns the key to sign with at now: the most recently activated
// key whose ActivateAt has passed and that stays live for at least
// MaxValidity, so no grant it signs can outlive its key.
func (r *KeyRing) Signer(now time.Time) (*Signer, error) {
	var best *RingKey
	for i := range r.Keys {
		k := &r.Keys[i]
		if !k.canSign() || now.Before(k.ActivateAt) || !k.liveAt(now.Add(MaxValidity)) {
			continue
		}
		if best == nil || k.ActivateAt.After(best.ActivateAt) {
			best = k
		}
	}
	if best == nil {
		return nil, ErrNoSigningKey
	}
	return &Signer{KeyID: best.KeyID, Key: ed25519.NewKeyFromSeed(best.Seed)}, nil
}

// PublicKeys returns the verification keys to publish at now: every key
// still within not_after, including rotated-in keys that do not sign yet and
// retiring keys whose grants may still be live. Ordered by ActivateAt.
func (r *KeyRing) PublicKeys(now time.Time) []PublicKey {
	live := make([]RingKey, 0, len(r.Keys))
	for _, k := range r.Keys {
		if len(k.publicBytes()) == ed25519.PublicKeySize && k.liveAt(now) {
			live = append(live, k)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].ActivateAt.Before(live[j].ActivateAt) })
	out := make([]PublicKey, len(live))
	for i, k := range live {
		out[i] = k.publicKey()
	}
	return out
}

// Rotate adds next to the ring. next starts signing at now+activateAfter;
// every key that is live past next's activation plus overlap is scheduled
// to retire at exactly that time, so a grant signed by an old key just before
// the switch stays verifiable until its exp. overlap must be at least
// MaxValidity.
func (r *KeyRing) Rotate(now time.Time, next RingKey, activateAfter, overlap time.Duration) error {
	if overlap < MaxValidity {
		return fmt.Errorf("grant: rotation overlap %s is shorter than max grant validity %s", overlap, MaxValidity)
	}
	if activateAfter < 0 {
		return errors.New("grant: negative activation delay")
	}
	if len(next.Seed) != ed25519.SeedSize || next.KeyID == "" {
		return errors.New("grant: invalid rotation key")
	}
	for _, k := range r.Keys {
		if k.KeyID == next.KeyID {
			return fmt.Errorf("grant: kid %q already in ring", next.KeyID)
		}
	}
	next.ActivateAt = now.Add(activateAfter).UTC()
	retire := next.ActivateAt.Add(overlap)
	for i := range r.Keys {
		if r.Keys[i].liveAt(retire) {
			r.Keys[i].NotAfter = retire
		}
	}
	r.Keys = append(r.Keys, next)
	return nil
}

// Prune drops keys whose not_after has passed. Their grants are already
// refused, so they no longer need publishing or storing. The hub uses Settle
// instead, which keeps their public halves for re-check verification.
func (r *KeyRing) Prune(now time.Time) {
	kept := r.Keys[:0]
	for _, k := range r.Keys {
		if k.liveAt(now) {
			kept = append(kept, k)
		}
	}
	r.Keys = kept
}

// Settle brings the ring up to date at now and reports whether it changed:
//
//   - a key whose successor (a later key that has activated and can sign)
//     exists drops its seed and keeps only its public half: it never signs
//     again, but still verifies until its not_after;
//   - a key past its not_after leaves Keys and joins Retained, public half
//     only, until not_after + retain;
//   - a retained key's RetainUntil only moves later (to RetiredAt + retain
//     when that is later), never earlier;
//   - a retained key past its RetainUntil is dropped.
func (r *KeyRing) Settle(now time.Time, retain time.Duration) bool {
	changed := false
	for i := range r.Keys {
		k := &r.Keys[i]
		if k.canSign() && r.superseded(i, now) {
			k.PublicKey = k.publicBytes()
			k.Seed = nil
			changed = true
		}
	}
	kept := r.Keys[:0]
	for _, k := range r.Keys {
		if k.liveAt(now) {
			kept = append(kept, k)
			continue
		}
		r.Retained = append(r.Retained, RetainedKey{
			KeyID: k.KeyID, PublicKey: k.publicBytes(),
			RetiredAt: k.NotAfter, RetainUntil: k.NotAfter.Add(retain),
		})
		changed = true
	}
	r.Keys = kept
	ret := r.Retained[:0]
	for _, k := range r.Retained {
		if want := k.RetiredAt.Add(retain); want.After(k.RetainUntil) {
			k.RetainUntil = want
			changed = true
		}
		if !now.Before(k.RetainUntil) {
			changed = true
			continue
		}
		ret = append(ret, k)
	}
	r.Retained = ret
	return changed
}

// superseded reports whether a key activated after r.Keys[i] has itself
// activated at now and can sign.
func (r *KeyRing) superseded(i int, now time.Time) bool {
	k := r.Keys[i]
	for j, o := range r.Keys {
		if j != i && o.canSign() && o.ActivateAt.After(k.ActivateAt) && !now.Before(o.ActivateAt) {
			return true
		}
	}
	return false
}

// Errors returned by Revoke.
var (
	ErrRevokeUnknown      = errors.New("grant: no such key in the ring")
	ErrRevokeActiveSigner = errors.New("grant: the active signing key cannot be revoked; rotate first")
)

// Revoke removes kid from the ring at once, from Keys and from Retained:
// it no longer verifies anything, in admission or in re-check. The key that
// signs at now cannot be revoked (rotate first).
func (r *KeyRing) Revoke(now time.Time, kid string) error {
	if s, err := r.Signer(now); err == nil && s.KeyID == kid {
		return ErrRevokeActiveSigner
	}
	found := false
	keys := r.Keys[:0]
	for _, k := range r.Keys {
		if k.KeyID == kid {
			found = true
			continue
		}
		keys = append(keys, k)
	}
	r.Keys = keys
	ret := r.Retained[:0]
	for _, k := range r.Retained {
		if k.KeyID == kid {
			found = true
			continue
		}
		ret = append(ret, k)
	}
	r.Retained = ret
	if !found {
		return ErrRevokeUnknown
	}
	return nil
}

// RecheckKeys returns the key set for re-check verification only: every
// key in Keys and every retained key, each valid until its retention ends
// (not_after + retain for a key in Keys, RetainUntil for a retained one).
// It must never be published or used to verify a stream grant at
// admission.
func (r *KeyRing) RecheckKeys(now time.Time, retain time.Duration) KeySet {
	s := make(staticKeySet, len(r.Keys)+len(r.Retained))
	for _, k := range r.Keys {
		pk := k.publicKey()
		if !k.NotAfter.IsZero() {
			pk.NotAfter = k.NotAfter.Add(retain)
		}
		s[k.KeyID] = pk
	}
	for _, k := range r.Retained {
		if now.Before(k.RetainUntil) {
			s[k.KeyID] = PublicKey{KeyID: k.KeyID, Key: append(ed25519.PublicKey(nil), k.PublicKey...), NotAfter: k.RetainUntil}
		}
	}
	return s
}

// WireKey is the JSON form of a PublicKey, matching the contract's GrantKey
// {kid, public_key, not_after_unix}. public_key is the raw 32-byte Ed25519
// key (standard base64 in JSON); not_after_unix 0 means no scheduled
// retirement.
type WireKey struct {
	KeyID        string `json:"kid"`
	PublicKey    []byte `json:"public_key"`
	NotAfterUnix int64  `json:"not_after_unix"`
}

// ToWire converts keys to their wire form.
func ToWire(keys []PublicKey) []WireKey {
	out := make([]WireKey, len(keys))
	for i, k := range keys {
		out[i] = WireKey{KeyID: k.KeyID, PublicKey: append([]byte(nil), k.Key...)}
		if !k.NotAfter.IsZero() {
			out[i].NotAfterUnix = k.NotAfter.Unix()
		}
	}
	return out
}

// FromWire converts wire keys back, for a target building its KeySet.
func FromWire(keys []WireKey) []PublicKey {
	out := make([]PublicKey, len(keys))
	for i, k := range keys {
		out[i] = PublicKey{KeyID: k.KeyID, Key: append(ed25519.PublicKey(nil), k.PublicKey...)}
		if k.NotAfterUnix != 0 {
			out[i].NotAfter = time.Unix(k.NotAfterUnix, 0)
		}
	}
	return out
}
