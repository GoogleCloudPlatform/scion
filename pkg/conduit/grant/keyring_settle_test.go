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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Key ring settling, retention for re-check verification, and revocation
// (design §3.5, Target-side deadlines, Re-check).

const (
	testActivateAfter = 5 * time.Minute
	testOverlap       = time.Hour
	testRetain        = 8*time.Hour + 30*time.Second
)

// rotatedRing returns a ring with k1 (active since t0) and k2 rotated in at
// t0 (activating at t0+testActivateAfter; k1 retires testOverlap later).
func rotatedRing(t *testing.T) (KeyRing, RingKey, RingKey) {
	t.Helper()
	k1, err := NewRingKey(t0, t0)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := NewRingKey(t0, t0)
	if err != nil {
		t.Fatal(err)
	}
	r := KeyRing{Keys: []RingKey{k1}}
	if err := r.Rotate(t0, k2, testActivateAfter, testOverlap); err != nil {
		t.Fatal(err)
	}
	return r, r.Keys[0], r.Keys[1]
}

func ringKeyByID(r KeyRing, kid string) (RingKey, bool) {
	for _, k := range r.Keys {
		if k.KeyID == kid {
			return k, true
		}
	}
	return RingKey{}, false
}

func retainedByID(r KeyRing, kid string) (RetainedKey, bool) {
	for _, k := range r.Retained {
		if k.KeyID == kid {
			return k, true
		}
	}
	return RetainedKey{}, false
}

// TestKeyRingSettleDropsSeedWhenSuccessorActivates: the outgoing key keeps
// its seed until the next key activates; from then on it still verifies
// within the overlap, but no code path can sign with it and the stored ring
// no longer contains its seed.
func TestKeyRingSettleDropsSeedWhenSuccessorActivates(t *testing.T) {
	r, k1, k2 := rotatedRing(t)
	activate := t0.Add(testActivateAfter)

	// A grant k1 signs just before the switch.
	s, err := r.Signer(activate.Add(-time.Second))
	if err != nil || s.KeyID != k1.KeyID {
		t.Fatalf("signer before activation = %v, %v; want k1", s, err)
	}
	c := baseClaims()
	c.NotBefore, c.Expiry = activate.Add(-time.Second), activate.Add(-time.Second).Add(30*time.Second)
	tok, err := Mint(s, c)
	if err != nil {
		t.Fatal(err)
	}

	if r.Settle(activate.Add(-time.Second), testRetain) {
		t.Fatal("Settle changed the ring before the successor activated")
	}
	if !r.Settle(activate, testRetain) {
		t.Fatal("Settle did not change the ring when the successor activated")
	}
	got, _ := ringKeyByID(r, k1.KeyID)
	if len(got.Seed) != 0 || len(got.PublicKey) == 0 {
		t.Fatalf("outgoing key after activation: seed %d bytes, public key %d bytes", len(got.Seed), len(got.PublicKey))
	}
	// No code path signs with k1.
	if s, err := r.Signer(activate); err != nil || s.KeyID != k2.KeyID {
		t.Fatalf("signer after activation = %v, %v; want k2", s, err)
	}
	// The stored form holds no k1 seed.
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Keys []map[string]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	for _, k := range stored.Keys {
		if string(k["kid"]) == `"`+k1.KeyID+`"` {
			if _, ok := k["seed"]; ok {
				t.Fatal("stored ring still holds the outgoing key's seed")
			}
		}
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("settled ring does not validate: %v", err)
	}
	// k1 still verifies within the overlap, from the published keys.
	keys, err := NewKeySet(r.PublicKeys(activate)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), tok, keys, baseExpect(), newCache(), activate); err != nil {
		t.Fatalf("grant from the outgoing key refused within the overlap: %v", err)
	}
}

// TestKeyRingSettleRetainsPublicHalf: a key past its not_after leaves the
// published keys and joins the retained set with its public half only; the
// re-check key set verifies it until not_after + retain, then not at all.
func TestKeyRingSettleRetainsPublicHalf(t *testing.T) {
	r, k1, _ := rotatedRing(t)
	notAfter := t0.Add(testActivateAfter + testOverlap)
	r.Settle(notAfter, testRetain)

	if _, ok := ringKeyByID(r, k1.KeyID); ok {
		t.Fatal("retired key still in Keys")
	}
	ret, ok := retainedByID(r, k1.KeyID)
	if !ok || !ret.RetainUntil.Equal(notAfter.Add(testRetain)) || len(ret.PublicKey) != 32 {
		t.Fatalf("retained entry = %+v (ok=%v)", ret, ok)
	}
	for _, pk := range r.PublicKeys(notAfter) {
		if pk.KeyID == k1.KeyID {
			t.Fatal("retired key published")
		}
	}
	if _, ok := r.RecheckKeys(notAfter, testRetain).Lookup(k1.KeyID); !ok {
		t.Fatal("re-check keys lack the retained key")
	}
	end := notAfter.Add(testRetain)
	if _, ok := r.RecheckKeys(end, testRetain).Lookup(k1.KeyID); ok {
		t.Fatal("re-check keys hold the key past its retention")
	}
	r.Settle(end, testRetain)
	if _, ok := retainedByID(r, k1.KeyID); ok {
		t.Fatal("retained key not dropped past its retention")
	}
}

// TestKeyRingRetentionOnlyGrows: a longer retention extends RetainUntil on
// the next Settle; a shorter one never shortens it.
func TestKeyRingRetentionOnlyGrows(t *testing.T) {
	r, k1, _ := rotatedRing(t)
	notAfter := t0.Add(testActivateAfter + testOverlap)
	r.Settle(notAfter, time.Hour)
	r.Settle(notAfter.Add(time.Minute), 2*time.Hour)
	if ret, _ := retainedByID(r, k1.KeyID); !ret.RetainUntil.Equal(notAfter.Add(2 * time.Hour)) {
		t.Fatalf("retain_until %v, want extended to %v", ret.RetainUntil, notAfter.Add(2*time.Hour))
	}
	r.Settle(notAfter.Add(2*time.Minute), 30*time.Minute)
	if ret, _ := retainedByID(r, k1.KeyID); !ret.RetainUntil.Equal(notAfter.Add(2 * time.Hour)) {
		t.Fatalf("retain_until %v shortened", ret.RetainUntil)
	}
}

// TestKeyRingRevoke: revoking a kid removes it from the published keys and
// from the re-check key set at once, so a real grant it signed no longer
// verifies there, while a rotated (not revoked) kid's grant still does. The
// key that signs now, and an unknown kid, cannot be revoked.
func TestKeyRingRevoke(t *testing.T) {
	r, k1, k2 := rotatedRing(t)
	activate := t0.Add(testActivateAfter)
	signer := func(k RingKey) *Signer {
		s, err := (&KeyRing{Keys: []RingKey{k}}).Signer(k.ActivateAt)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	mintAt := func(s *Signer, at time.Time) []byte {
		c := baseClaims()
		c.NotBefore, c.Expiry = at, at.Add(30*time.Second)
		tok, err := Mint(s, c)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	s1 := signer(k1)
	tokOld := mintAt(s1, activate.Add(-time.Second))

	if err := r.Revoke(t0, k1.KeyID); !errors.Is(err, ErrRevokeActiveSigner) {
		t.Fatalf("revoking the active signer: %v", err)
	}
	if err := r.Revoke(t0, "cg-unknown"); !errors.Is(err, ErrRevokeUnknown) {
		t.Fatalf("revoking an unknown kid: %v", err)
	}
	r.Settle(activate, testRetain)
	at := activate.Add(time.Second)
	if _, err := Verify(context.Background(), tokOld, r.RecheckKeys(at, testRetain), baseExpect(), newCache(), activate.Add(-time.Second)); err != nil {
		t.Fatalf("grant from the rotated key refused by the re-check keys before revocation: %v", err)
	}
	if err := r.Revoke(at, k1.KeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := Verify(context.Background(), tokOld, r.RecheckKeys(at, testRetain), baseExpect(), newCache(), activate.Add(-time.Second)); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("grant from the revoked key: err = %v, want ErrUnknownKey", err)
	}
	for _, pk := range r.PublicKeys(at) {
		if pk.KeyID == k1.KeyID {
			t.Fatal("revoked key still published")
		}
	}
	// k2, rotated in and not revoked, still verifies.
	s2, err := r.Signer(at)
	if err != nil || s2.KeyID != k2.KeyID {
		t.Fatalf("signer = %v, %v", s2, err)
	}
	if _, err := Verify(context.Background(), mintAt(s2, at), r.RecheckKeys(at, testRetain), baseExpect(), newCache(), at); err != nil {
		t.Fatalf("grant from the unrevoked key: %v", err)
	}
	if _, ok := ringKeyByID(r, k1.KeyID); ok {
		t.Fatal("revoked kid still in Keys")
	}
	if _, ok := retainedByID(r, k1.KeyID); ok {
		t.Fatal("revoked kid still retained")
	}
	if !strings.Contains(r.String(), "revoked:["+k1.KeyID+"]") {
		t.Fatalf("revoked kid not remembered: %s", r.String())
	}
}

// TestKeyRingKIDsNeverReused: a key whose kid equals a live, retained or
// revoked kid cannot be rotated in; a revoked kid is remembered until its
// revocation plus the retention, then forgotten.
func TestKeyRingKIDsNeverReused(t *testing.T) {
	r, k1, k2 := rotatedRing(t)
	activate := t0.Add(testActivateAfter)
	notAfter := activate.Add(testOverlap)
	r.Settle(notAfter, testRetain) // k1 is now retained
	k3, err := NewRingKey(notAfter, notAfter)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Rotate(notAfter, k3, testActivateAfter, testOverlap); err != nil {
		t.Fatal(err)
	}
	r.Settle(notAfter.Add(testActivateAfter), testRetain)
	revokedAt := notAfter.Add(testActivateAfter)
	if err := r.Revoke(revokedAt, k2.KeyID); err != nil { // k3 signs now; k2 is revocable
		t.Fatal(err)
	}
	for name, kid := range map[string]string{"live": k3.KeyID, "retained": k1.KeyID, "revoked": k2.KeyID} {
		t.Run(name, func(t *testing.T) {
			dup, err := NewRingKey(revokedAt, revokedAt)
			if err != nil {
				t.Fatal(err)
			}
			dup.KeyID = kid
			rr := r
			rr.Keys = append([]RingKey(nil), r.Keys...)
			if err := rr.Rotate(revokedAt.Add(time.Hour), dup, testActivateAfter, testOverlap); err == nil {
				t.Fatalf("a %s kid was reused", name)
			}
		})
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("ring with a revoked entry does not validate: %v", err)
	}
	r.Settle(revokedAt.Add(testRetain), testRetain)
	if strings.Contains(r.String(), "revoked:["+k2.KeyID+"]") {
		t.Fatal("revoked kid remembered past the retention")
	}
}

// TestKeyRingValidateSeedless: a key without a seed must carry a 32-byte
// public key; retained keys hold public halves only.
func TestKeyRingValidateSeedless(t *testing.T) {
	r, _, _ := rotatedRing(t)
	r.Settle(t0.Add(testActivateAfter), testRetain)
	if err := r.Validate(); err != nil {
		t.Fatalf("valid settled ring refused: %v", err)
	}
	bad := r
	bad.Keys = append([]RingKey(nil), r.Keys...)
	bad.Keys[0].PublicKey = nil
	bad.Keys[0].Seed = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("a key with neither seed nor public key validated")
	}
}
