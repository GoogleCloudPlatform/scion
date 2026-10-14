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
	"testing"
	"time"
)

// Re-check verification (design v2.21.27 §3.5, Re-check).

// recheckOpenClaims is a stream grant with a deadline, jti testOpenJTI.
func recheckOpenClaims() Claims {
	c := baseClaims()
	c.JTI = testOpenJTI
	c.AuthzUntil, c.AuthzInterval = t0.Add(time.Hour), time.Hour
	return c
}

func recheckExpect() RecheckExpectation {
	return RecheckExpectation{Issuer: "scion-hub", Target: baseTarget(), OpenJTI: testOpenJTI, StreamID: 3}
}

// TestVerifyRecheck_AcceptsChainGrants: both an open grant and a renewal
// for the same open_jti verify, and the claims come from the token.
func TestVerifyRecheck_AcceptsChainGrants(t *testing.T) {
	f := newFixture(t)
	open := f.mint(t, recheckOpenClaims())
	c, err := VerifyRecheck(open, f.keys, recheckExpect(), t0.Add(time.Second))
	if err != nil {
		t.Fatalf("open grant: %v", err)
	}
	if c.Renewal || c.OpenJTI != testOpenJTI || !c.Stream.Equal(baseClaims().Stream) || c.Subject != baseClaims().Subject {
		t.Fatalf("open grant claims %+v", c)
	}
	ren := f.mintRenewal(t, baseRenewal())
	c, err = VerifyRecheck(ren, f.keys, recheckExpect(), t0.Add(time.Second))
	if err != nil {
		t.Fatalf("renewal: %v", err)
	}
	if !c.Renewal || c.StreamID != 3 || c.OpenJTI != testOpenJTI || !c.Stream.Equal(baseRenewal().Stream) {
		t.Fatalf("renewal claims %+v", c)
	}
}

// TestVerifyRecheck_Window: a grant whose exp passed is accepted until its
// authz_until + 30s and refused after; an nbf more than 30s ahead is
// refused; no jti is consumed (the same token verifies again).
func TestVerifyRecheck_Window(t *testing.T) {
	f := newFixture(t)
	open := f.mint(t, recheckOpenClaims())
	until := recheckOpenClaims().AuthzUntil
	for _, at := range []time.Time{t0.Add(time.Minute), until, until.Add(RecheckAllowance)} {
		if _, err := VerifyRecheck(open, f.keys, recheckExpect(), at); err != nil {
			t.Fatalf("at %v (exp passed, within the window): %v", at, err)
		}
	}
	if _, err := VerifyRecheck(open, f.keys, recheckExpect(), until.Add(RecheckAllowance+time.Second)); !errors.Is(err, ErrRecheckWindow) {
		t.Fatalf("past the window: err = %v, want ErrRecheckWindow", err)
	}
	if _, err := VerifyRecheck(open, f.keys, recheckExpect(), t0.Add(-RecheckAllowance-time.Second)); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("nbf more than 30s ahead: err = %v, want ErrNotYetValid", err)
	}
}

// TestVerifyRecheck_Refusals: binding, type and claim refusals.
func TestVerifyRecheck_Refusals(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	noDeadline := baseClaims()
	noDeadline.JTI = testOpenJTI
	tests := []struct {
		name   string
		token  []byte
		expect func(*RecheckExpectation)
		want   error
	}{
		{"other session", f.mint(t, recheckOpenClaims()), func(e *RecheckExpectation) { e.Target.SessionID = "sess-2" }, ErrTarget},
		{"other epoch", f.mint(t, recheckOpenClaims()), func(e *RecheckExpectation) { e.Target.ConnectionEpoch = 8 }, ErrTarget},
		{"other incarnation", f.mint(t, recheckOpenClaims()), func(e *RecheckExpectation) { e.Target.EndpointIncarnation = "inc-2" }, ErrTarget},
		{"other open jti (open grant)", f.mint(t, recheckOpenClaims()), func(e *RecheckExpectation) { e.OpenJTI = "open-jti-2" }, ErrStreamBinding},
		{"other open jti (renewal)", f.mintRenewal(t, baseRenewal()), func(e *RecheckExpectation) { e.OpenJTI = "open-jti-2" }, ErrStreamBinding},
		{"other stream id (renewal)", f.mintRenewal(t, baseRenewal()), func(e *RecheckExpectation) { e.StreamID = 5 }, ErrStreamBinding},
		{"wrong issuer", f.mint(t, recheckOpenClaims()), func(e *RecheckExpectation) { e.Issuer = "other-hub" }, ErrIssuer},
		{"open grant without authz_until", f.mint(t, noDeadline), nil, ErrAuthzUntil},
		{"incomplete expectation", f.mint(t, recheckOpenClaims()), func(e *RecheckExpectation) { e.StreamID = 0 }, ErrExpectation},
		{"not a grant", []byte("abc.def.ghi"), nil, ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := recheckExpect()
			if tt.expect != nil {
				tt.expect(&e)
			}
			if _, err := VerifyRecheck(tt.token, f.keys, e, now); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestVerifyRecheck_Keys: a real grant signed by a revoked kid is refused by
// the re-check key set; a rotated, unrevoked kid's grant is accepted within
// its retention, after its not_after; an unknown kid is refused.
func TestVerifyRecheck_Keys(t *testing.T) {
	r, k1, k2 := rotatedRing(t)
	activate := t0.Add(testActivateAfter)
	notAfter := activate.Add(testOverlap)
	s1, err := r.Signer(activate.Add(-time.Second))
	if err != nil || s1.KeyID != k1.KeyID {
		t.Fatalf("signer = %v, %v", s1, err)
	}
	c := recheckOpenClaims()
	c.NotBefore, c.Expiry = activate.Add(-time.Second), activate.Add(29*time.Second)
	c.AuthzUntil = notAfter.Add(time.Hour)
	tok, err := Mint(s1, c)
	if err != nil {
		t.Fatal(err)
	}
	// After k1's not_after, the retained set still verifies it.
	at := notAfter.Add(time.Minute)
	r.Settle(at, testRetain)
	if _, err := VerifyRecheck(tok, r.RecheckKeys(at, testRetain), recheckExpect(), at); err != nil {
		t.Fatalf("rotated key within its retention: %v", err)
	}
	if _, err := VerifyRecheck(tok, mustKeySet(t, r.PublicKeys(at)), recheckExpect(), at); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("published keys after not_after: err = %v, want ErrUnknownKey", err)
	}
	if err := r.Revoke(at, k1.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRecheck(tok, r.RecheckKeys(at, testRetain), recheckExpect(), at); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("revoked key: err = %v, want ErrUnknownKey", err)
	}
	_ = k2
}

func mustKeySet(t *testing.T, keys []PublicKey) KeySet {
	t.Helper()
	ks, err := NewKeySet(keys...)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}
