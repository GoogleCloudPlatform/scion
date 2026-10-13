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
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

// Renewal grants and the optional deadline claims of stream grants
// (design v2.21.16 §3.5, "Target-side deadlines").

const testOpenJTI = "open-jti-1"

func baseRenewal() RenewalClaims {
	return RenewalClaims{
		Issuer:     "scion-hub",
		Subject:    "user:u1",
		ProjectID:  "proj-1",
		Target:     baseTarget(),
		StreamID:   3,
		OpenJTI:    testOpenJTI,
		AuthzUntil: t0.Add(2 * time.Hour),
		NotBefore:  t0,
		Expiry:     t0.Add(30 * time.Second),
	}
}

func baseRenewalExpect() RenewalExpectation {
	return RenewalExpectation{
		Target:    baseTarget(),
		ProjectID: "proj-1",
		Subject:   "user:u1",
		StreamID:  3,
		OpenJTI:   testOpenJTI,
		After:     t0.Add(time.Hour),
		NotAfter:  t0.Add(time.Hour + time.Hour + 30*time.Second),
	}
}

func (f *fixture) mintRenewal(t *testing.T, c RenewalClaims) []byte {
	t.Helper()
	tok, err := MintRenewal(f.signer, c)
	if err != nil {
		t.Fatalf("MintRenewal: %v", err)
	}
	return tok
}

func TestVerifyRenewal_Valid(t *testing.T) {
	f := newFixture(t)
	c, err := VerifyRenewal(context.Background(), f.mintRenewal(t, baseRenewal()), f.keys, baseRenewalExpect(), newCache(), t0.Add(time.Second))
	if err != nil {
		t.Fatalf("VerifyRenewal: %v", err)
	}
	if !c.AuthzUntil.Equal(t0.Add(2*time.Hour)) || c.StreamID != 3 || c.OpenJTI != testOpenJTI || c.KeyID != f.signer.KeyID || c.JTI == "" {
		t.Fatalf("unexpected claims %+v", c)
	}
}

// TestVerifyRenewal_Refusals: a renewal is accepted only when it is bound
// to the stream (target session, epoch and incarnation, project, subject,
// stream_id and open_jti), within its validity window, and moves the
// deadline later but not past the cap.
func TestVerifyRenewal_Refusals(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	tests := []struct {
		name   string
		claims func(*RenewalClaims)
		expect func(*RenewalExpectation)
		at     time.Time
		want   error
	}{
		{name: "other session", claims: func(c *RenewalClaims) { c.Target.SessionID = "sess-2" }, want: ErrTarget},
		{name: "other epoch", claims: func(c *RenewalClaims) { c.Target.ConnectionEpoch = 8 }, want: ErrTarget},
		{name: "other incarnation", claims: func(c *RenewalClaims) { c.Target.EndpointIncarnation = "inc-2" }, want: ErrTarget},
		{name: "other target id", claims: func(c *RenewalClaims) { c.Target.ID = "agent-2" }, want: ErrTarget},
		{name: "other project", claims: func(c *RenewalClaims) { c.ProjectID = "proj-2" }, want: ErrProject},
		{name: "other subject", claims: func(c *RenewalClaims) { c.Subject = "user:u2" }, want: ErrSubject},
		{name: "other stream id", claims: func(c *RenewalClaims) { c.StreamID = 5 }, want: ErrStreamBinding},
		{name: "other open jti", claims: func(c *RenewalClaims) { c.OpenJTI = "open-jti-2" }, want: ErrStreamBinding},
		{name: "wrong issuer", expect: func(e *RenewalExpectation) { e.Issuer = "other-hub" }, want: ErrIssuer},
		{name: "does not extend", claims: func(c *RenewalClaims) { c.AuthzUntil = t0.Add(time.Hour) }, want: ErrAuthzUntil},
		{name: "earlier than current", claims: func(c *RenewalClaims) { c.AuthzUntil = t0.Add(30 * time.Minute) }, want: ErrAuthzUntil},
		{name: "past the cap", claims: func(c *RenewalClaims) { c.AuthzUntil = t0.Add(3 * time.Hour) }, want: ErrAuthzUntil},
		{name: "expired", at: t0.Add(31 * time.Second), want: ErrExpired},
		{name: "not yet valid", at: t0.Add(-time.Second), want: ErrNotYetValid},
		{name: "incomplete expectation", expect: func(e *RenewalExpectation) { e.OpenJTI = "" }, want: ErrExpectation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseRenewal()
			if tt.claims != nil {
				tt.claims(&c)
			}
			e := baseRenewalExpect()
			if tt.expect != nil {
				tt.expect(&e)
			}
			at := now
			if !tt.at.IsZero() {
				at = tt.at
			}
			cache := newCache()
			tok := f.mintRenewal(t, c)
			if _, err := VerifyRenewal(context.Background(), tok, f.keys, e, cache, at); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			// A refused renewal never consumes its jti.
			if tt.want != ErrExpectation {
				if _, err := VerifyRenewal(context.Background(), f.mintRenewal(t, baseRenewal()), f.keys, baseRenewalExpect(), cache, now); err != nil {
					t.Fatalf("a valid renewal after the refusal: %v", err)
				}
			}
		})
	}
}

// TestVerifyRenewal_TypesNotInterchangeable: a stream grant is never
// accepted as a renewal, and a renewal is never accepted as a stream
// grant, even when both are signed with the same key.
func TestVerifyRenewal_TypesNotInterchangeable(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	open := f.mint(t, baseClaims())
	if _, err := VerifyRenewal(context.Background(), open, f.keys, baseRenewalExpect(), newCache(), now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("stream grant as renewal: err = %v, want ErrMalformed", err)
	}
	ren := f.mintRenewal(t, baseRenewal())
	if _, err := Verify(context.Background(), ren, f.keys, baseExpect(), newCache(), now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("renewal as stream grant: err = %v, want ErrMalformed", err)
	}
}

func TestVerifyRenewal_ReplayRefused(t *testing.T) {
	f := newFixture(t)
	cache := newCache()
	tok := f.mintRenewal(t, baseRenewal())
	now := t0.Add(time.Second)
	if _, err := VerifyRenewal(context.Background(), tok, f.keys, baseRenewalExpect(), cache, now); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRenewal(context.Background(), tok, f.keys, baseRenewalExpect(), cache, now); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay: err = %v, want ErrReplay", err)
	}
}

// TestVerifyRenewal_Keys: an unknown kid, a key past its not_after and a
// signature by another key are refused.
func TestVerifyRenewal_Keys(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other := &Signer{KeyID: "cg-unknown", Key: otherPriv}
	tok, err := MintRenewal(other, baseRenewal())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRenewal(context.Background(), tok, f.keys, baseRenewalExpect(), newCache(), now); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown kid: err = %v, want ErrUnknownKey", err)
	}
	sameKid := &Signer{KeyID: f.signer.KeyID, Key: otherPriv}
	tok, err = MintRenewal(sameKid, baseRenewal())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRenewal(context.Background(), tok, f.keys, baseRenewalExpect(), newCache(), now); !errors.Is(err, ErrSignature) {
		t.Fatalf("other key under a known kid: err = %v, want ErrSignature", err)
	}
	pub := f.ring.PublicKeys(t0)[0]
	pub.NotAfter = now
	retired, err := NewKeySet(pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRenewal(context.Background(), f.mintRenewal(t, baseRenewal()), retired, baseRenewalExpect(), newCache(), now); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("retired key: err = %v, want ErrKeyExpired", err)
	}
}

func TestMintRenewal_RejectsInvalidClaims(t *testing.T) {
	f := newFixture(t)
	tests := map[string]func(*RenewalClaims){
		"missing stream id":      func(c *RenewalClaims) { c.StreamID = 0 },
		"missing open jti":       func(c *RenewalClaims) { c.OpenJTI = "" },
		"missing authz_until":    func(c *RenewalClaims) { c.AuthzUntil = time.Time{} },
		"authz_until before nbf": func(c *RenewalClaims) { c.AuthzUntil = t0.Add(-time.Second) },
		"validity over 60s":      func(c *RenewalClaims) { c.Expiry = t0.Add(61 * time.Second) },
		"incomplete target":      func(c *RenewalClaims) { c.Target.SessionID = "" },
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			c := baseRenewal()
			mut(&c)
			if _, err := MintRenewal(f.signer, c); err == nil {
				t.Fatal("MintRenewal accepted invalid claims")
			}
		})
	}
}

// TestGrantDeadlineClaims: authz_until and authz_interval round-trip, are
// required together, and a grant without them encodes exactly as before
// (no new keys in the payload).
func TestGrantDeadlineClaims(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)

	plain := f.mint(t, baseClaims())
	if pl := payloadOf(t, plain); pl["authz_until"] != nil || pl["authz_interval"] != nil {
		t.Fatalf("grant without a deadline carries deadline keys: %v", pl)
	}
	c, err := Verify(context.Background(), plain, f.keys, baseExpect(), newCache(), now)
	if err != nil || !c.AuthzUntil.IsZero() || c.AuthzInterval != 0 {
		t.Fatalf("plain grant: %+v, %v", c, err)
	}

	withDeadline := baseClaims()
	withDeadline.AuthzUntil = t0.Add(8 * time.Hour)
	withDeadline.AuthzInterval = 8 * time.Hour
	c, err = Verify(context.Background(), f.mint(t, withDeadline), f.keys, baseExpect(), newCache(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !c.AuthzUntil.Equal(withDeadline.AuthzUntil) || c.AuthzInterval != 8*time.Hour {
		t.Fatalf("deadline claims = %v, %v", c.AuthzUntil, c.AuthzInterval)
	}

	for name, mut := range map[string]func(*Claims){
		"until without interval": func(c *Claims) { c.AuthzUntil = t0.Add(time.Hour) },
		"interval without until": func(c *Claims) { c.AuthzInterval = time.Hour },
		"until not after nbf":    func(c *Claims) { c.AuthzUntil, c.AuthzInterval = t0, time.Hour },
		"fractional interval":    func(c *Claims) { c.AuthzUntil, c.AuthzInterval = t0.Add(time.Hour), 1500*time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			bad := baseClaims()
			mut(&bad)
			if _, err := Mint(f.signer, bad); err == nil {
				t.Fatal("Mint accepted invalid deadline claims")
			}
		})
	}

	// An explicit zero is not the canonical encoding and is refused.
	pl := payloadOf(t, plain)
	pl["authz_until"] = 0
	tok := forge(f.signer.Key, header{Algorithm, TokenType, f.signer.KeyID}, pl)
	if _, err := Verify(context.Background(), tok, f.keys, baseExpect(), newCache(), now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("explicit zero authz_until: err = %v, want ErrMalformed", err)
	}
}

// TestGrantEncodingWithoutDeadlineUnchanged pins the exact header and
// payload bytes of a stream grant without deadline claims, so adding the
// optional claims leaves such grants byte-identical to before.
func TestGrantEncodingWithoutDeadlineUnchanged(t *testing.T) {
	signer := &Signer{KeyID: "cg-golden", Key: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))}
	c := baseClaims()
	c.JTI = "jti-golden"
	tok, err := Mint(signer, c)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(tok), ".")
	hdr, err := b64.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	const wantHdr = `{"alg":"EdDSA","typ":"conduit-grant+jwt","kid":"cg-golden"}`
	const wantPayload = `{"iss":"scion-hub","aud":"conduit-target","kid":"cg-golden","jti":"jti-golden","sub":"user-1","project_id":"proj-1",` +
		`"target":{"kind":"agent","id":"agent-1","endpoint_incarnation":"inc-1","session_id":"sess-1","connection_epoch":7},` +
		`"stream":{"kind":"tcp","params":{"host":"127.0.0.1","port":"8080"}},"nbf":1791028800,"exp":1791028830}`
	if string(hdr) != wantHdr {
		t.Fatalf("header\n got %s\nwant %s", hdr, wantHdr)
	}
	if string(payload) != wantPayload {
		t.Fatalf("payload\n got %s\nwant %s", payload, wantPayload)
	}
}
