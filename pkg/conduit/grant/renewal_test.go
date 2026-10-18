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
// (design v2.21.17 §3.5, "Target-side deadlines").

const testOpenJTI = "open-jti-1"

func baseRenewal() RenewalClaims {
	return RenewalClaims{
		Issuer:     "scion-hub",
		Subject:    "user:u1",
		ProjectID:  "proj-1",
		Target:     baseTarget(),
		Stream:     baseClaims().Stream,
		Via:        ViaProxy,
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
		Stream:    baseClaims().Stream,
		Via:       ViaProxy,
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
		// The tunnel origin carries pty, so the renewal differs from the
		// expectation in its stream first (Stream is compared before Via).
		{name: "other stream kind", claims: func(c *RenewalClaims) { c.Stream, c.Via = StreamHeader{Kind: StreamKindPTY}, ViaTunnel }, want: ErrStream},
		{name: "other via", claims: func(c *RenewalClaims) { c.Via = ViaTunnel }, want: ErrVia},
		{name: "other stream params", claims: func(c *RenewalClaims) {
			c.Stream = StreamHeader{Kind: StreamKindTCP, Params: map[string]string{ParamHost: "127.0.0.1", ParamPort: "9090"}}
		}, want: ErrStream},
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
			// The refused token and the valid one that follows share a jti.
			const jti = "renewal-jti-fixed"
			c.JTI = jti
			cache := newCache()
			tok := f.mintRenewal(t, c)
			if _, err := VerifyRenewal(context.Background(), tok, f.keys, e, cache, at); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			// A refused renewal never consumes its jti.
			valid := baseRenewal()
			valid.JTI = jti
			if _, err := VerifyRenewal(context.Background(), f.mintRenewal(t, valid), f.keys, baseRenewalExpect(), cache, now); err != nil {
				t.Fatalf("a valid renewal with the refused one's jti: %v", err)
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
// signature by another key are refused, and none of them consumes the
// jti: a valid renewal with the same jti is accepted afterwards.
func TestVerifyRenewal_Keys(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := f.ring.PublicKeys(t0)[0]
	pub.NotAfter = now
	retired, err := NewKeySet(pub)
	if err != nil {
		t.Fatal(err)
	}
	const jti = "renewal-jti-fixed"
	withJTI := baseRenewal()
	withJTI.JTI = jti
	tests := []struct {
		name   string
		signer *Signer
		keys   KeySet
		want   error
	}{
		{"unknown kid", &Signer{KeyID: "cg-unknown", Key: otherPriv}, f.keys, ErrUnknownKey},
		{"other key under a known kid", &Signer{KeyID: f.signer.KeyID, Key: otherPriv}, f.keys, ErrSignature},
		{"retired key", f.signer, retired, ErrKeyExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newCache()
			tok, err := MintRenewal(tt.signer, withJTI)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyRenewal(context.Background(), tok, tt.keys, baseRenewalExpect(), cache, now); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if _, err := VerifyRenewal(context.Background(), f.mintRenewal(t, withJTI), f.keys, baseRenewalExpect(), cache, now); err != nil {
				t.Fatalf("a valid renewal with the refused one's jti: %v", err)
			}
		})
	}
}

func TestMintRenewal_RejectsInvalidClaims(t *testing.T) {
	f := newFixture(t)
	// Each row names the error and the validation message it must fail
	// with, so a row cannot pass on an earlier, unrelated check.
	tests := map[string]struct {
		mut  func(*RenewalClaims)
		want error
		msg  string
	}{
		"missing stream id":      {func(c *RenewalClaims) { c.StreamID = 0 }, ErrInvalidClaims, "missing stream_id"},
		"missing open jti":       {func(c *RenewalClaims) { c.OpenJTI = "" }, ErrInvalidClaims, "missing open_jti"},
		"missing stream":         {func(c *RenewalClaims) { c.Stream = StreamHeader{} }, ErrInvalidClaims, "missing or unknown stream kind"},
		"missing via":            {func(c *RenewalClaims) { c.Via = "" }, ErrInvalidClaims, "missing or unknown via"},
		"unknown via":            {func(c *RenewalClaims) { c.Via = "elsewhere" }, ErrInvalidClaims, "missing or unknown via"},
		"proxy via, pty stream":  {func(c *RenewalClaims) { c.Stream = StreamHeader{Kind: StreamKindPTY} }, ErrVia, "does not carry stream kind"},
		"missing authz_until":    {func(c *RenewalClaims) { c.AuthzUntil = time.Time{} }, ErrInvalidClaims, "missing nbf, exp or authz_until"},
		"authz_until before nbf": {func(c *RenewalClaims) { c.AuthzUntil = t0.Add(-time.Second) }, ErrInvalidClaims, "authz_until must be after nbf"},
		"validity over 60s":      {func(c *RenewalClaims) { c.Expiry = t0.Add(61 * time.Second) }, ErrValidityWindow, "exp-nbf"},
		"incomplete target":      {func(c *RenewalClaims) { c.Target.SessionID = "" }, ErrInvalidClaims, "incomplete target"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := baseRenewal()
			tt.mut(&c)
			_, err := MintRenewal(f.signer, c)
			if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("MintRenewal: %v, want %v (%q)", err, tt.want, tt.msg)
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

// TestVerify_RequireDeadlineFor: with RequireDeadlineFor set, a grant whose
// sub has that prefix and no authz_until is refused with ErrAuthzUntil
// without consuming its jti (the same token verifies once the requirement
// is dropped); a grant with another subject is not affected; and an empty
// RequireDeadlineFor leaves verification unchanged.
func TestVerify_RequireDeadlineFor(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	required := baseExpect()
	required.RequireDeadlineFor = SubjectUserPrefix

	t.Run("user grant refused, jti kept", func(t *testing.T) {
		cache := newCache()
		c := baseClaims()
		c.Subject = SubjectUserPrefix + "u1"
		tok := f.mint(t, c)
		if _, err := Verify(context.Background(), tok, f.keys, required, cache, now); !errors.Is(err, ErrAuthzUntil) {
			t.Fatalf("err = %v, want ErrAuthzUntil", err)
		}
		if _, err := Verify(context.Background(), tok, f.keys, baseExpect(), cache, now); err != nil {
			t.Fatalf("the same grant without the requirement: %v", err)
		}
	})
	t.Run("user grant with authz_until and via accepted", func(t *testing.T) {
		c := baseClaims()
		c.Subject = SubjectUserPrefix + "u1"
		c.AuthzUntil, c.AuthzInterval, c.Via = t0.Add(time.Hour), time.Hour, ViaProxy
		if _, err := Verify(context.Background(), f.mint(t, c), f.keys, required, newCache(), now); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("user grant with authz_until but no via refused, jti kept", func(t *testing.T) {
		cache := newCache()
		c := baseClaims()
		c.Subject, c.JTI = SubjectUserPrefix+"u1", "jti-novia"
		c.AuthzUntil, c.AuthzInterval = t0.Add(time.Hour), time.Hour
		if _, err := Verify(context.Background(), f.mint(t, c), f.keys, required, cache, now); !errors.Is(err, ErrVia) {
			t.Fatalf("err = %v, want ErrVia", err)
		}
		c.Via = ViaTunnel
		if _, err := Verify(context.Background(), f.mint(t, c), f.keys, required, cache, now); err != nil {
			t.Fatalf("the same jti with a via: %v", err)
		}
	})
	t.Run("other subject not affected", func(t *testing.T) {
		c := baseClaims()
		c.Subject = "agent:a1"
		if _, err := Verify(context.Background(), f.mint(t, c), f.keys, required, newCache(), now); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("empty requirement unchanged", func(t *testing.T) {
		c := baseClaims()
		c.Subject = SubjectUserPrefix + "u1"
		if _, err := Verify(context.Background(), f.mint(t, c), f.keys, baseExpect(), newCache(), now); err != nil {
			t.Fatal(err)
		}
	})
}

// TestVerifyRenewal_StreamClaimRequired: a renewal token without the stream
// claim (signed with a valid key) is refused and does not consume its jti
// (design v2.21.26 §3.5).
func TestVerifyRenewal_StreamClaimRequired(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	c := baseRenewal()
	c.JTI = "renewal-jti-fixed"
	tok := f.mintRenewal(t, c)
	pl := payloadOf(t, tok)
	delete(pl, "stream")
	forged := forge(f.signer.Key, header{Algorithm, RenewalTokenType, f.signer.KeyID}, pl)
	cache := newCache()
	if _, err := VerifyRenewal(context.Background(), forged, f.keys, baseRenewalExpect(), cache, now); err == nil {
		t.Fatal("renewal without a stream claim accepted")
	}
	if _, err := VerifyRenewal(context.Background(), tok, f.keys, baseRenewalExpect(), cache, now); err != nil {
		t.Fatalf("the same jti with the stream claim: %v", err)
	}
}

// TestGrantViaClaim: via round-trips on a stream grant, is omitted when
// empty, and an unknown value cannot be minted (design v2.21.29 §3.5).
func TestGrantViaClaim(t *testing.T) {
	f := newFixture(t)
	c := baseClaims()
	c.Via = ViaTunnel
	got, err := Verify(context.Background(), f.mint(t, c), f.keys, baseExpect(), newCache(), t0.Add(time.Second))
	if err != nil || got.Via != ViaTunnel {
		t.Fatalf("via round trip: %+v, %v", got, err)
	}
	if pl := payloadOf(t, f.mint(t, baseClaims())); pl["via"] != nil {
		t.Fatal("a grant without via carries the key")
	}
	c.Via = "elsewhere"
	if _, err := Mint(f.signer, c); err == nil {
		t.Fatal("an unknown via was minted")
	}
}

// resignReplacing returns tok with old replaced by repl in its payload,
// signed again with priv under the same header: a grant Mint would refuse
// to sign, with canonical encoding.
func resignReplacing(t *testing.T, priv ed25519.PrivateKey, tok []byte, old, repl string) []byte {
	t.Helper()
	parts := strings.Split(string(tok), ".")
	payload, err := b64.DecodeString(parts[1])
	if err != nil || strings.Count(string(payload), old) != 1 {
		t.Fatalf("payload %s does not hold %q once", payload, old)
	}
	in := parts[0] + "." + b64.EncodeToString([]byte(strings.Replace(string(payload), old, repl, 1)))
	return []byte(in + "." + b64.EncodeToString(ed25519.Sign(priv, []byte(in))))
}

// TestViaAllows is the origin and kind table (design v2.21.29 §3.5).
func TestViaAllows(t *testing.T) {
	want := map[string][]string{
		ViaTunnel: {StreamKindTCP, StreamKindSSH, StreamKindPTY},
		ViaProxy:  {StreamKindTCP},
		ViaPTY:    {StreamKindPTY},
	}
	for _, via := range []string{ViaTunnel, ViaProxy, ViaPTY, "", "elsewhere"} {
		for _, kind := range []string{StreamKindTCP, StreamKindSSH, StreamKindPTY, StreamKindLogs, StreamKindEvents, "", "other"} {
			allowed := false
			for _, k := range want[via] {
				allowed = allowed || k == kind
			}
			if got := ViaAllows(via, kind); got != allowed {
				t.Errorf("ViaAllows(%q, %q) = %v, want %v", via, kind, got, allowed)
			}
		}
	}
}

// TestViaOutsideTableRefused: a grant or renewal whose via does not carry
// its stream kind cannot be minted, and one signed anyway is refused by
// Verify, VerifyRenewal and VerifyRecheck with ErrVia, without consuming
// its jti.
func TestViaOutsideTableRefused(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)

	open := baseClaims()
	open.Via = ViaPTY // tcp stream
	if _, err := Mint(f.signer, open); !errors.Is(err, ErrVia) {
		t.Fatalf("Mint: %v, want ErrVia", err)
	}
	ren := baseRenewal()
	ren.Via = ViaPTY
	if _, err := MintRenewal(f.signer, ren); !errors.Is(err, ErrVia) {
		t.Fatalf("MintRenewal: %v, want ErrVia", err)
	}

	open.Via = ViaTunnel
	open.JTI = "open-table"
	forgedOpen := resignReplacing(t, f.signer.Key, f.mint(t, open), `"via":"tunnel"`, `"via":"pty"`)
	cache := newCache()
	if _, err := Verify(context.Background(), forgedOpen, f.keys, baseExpect(), cache, now); !errors.Is(err, ErrVia) {
		t.Fatalf("Verify: %v, want ErrVia", err)
	}
	if cache.Len() != 0 {
		t.Fatal("Verify consumed the jti of a refused grant")
	}

	ren.Via = ViaTunnel
	ren.JTI = "renewal-table"
	forgedRen := resignReplacing(t, f.signer.Key, f.mintRenewal(t, ren), `"via":"tunnel"`, `"via":"pty"`)
	expect := baseRenewalExpect()
	expect.Via = ViaPTY
	if _, err := VerifyRenewal(context.Background(), forgedRen, f.keys, expect, cache, now); !errors.Is(err, ErrVia) {
		t.Fatalf("VerifyRenewal: %v, want ErrVia", err)
	}
	if cache.Len() != 0 {
		t.Fatal("VerifyRenewal consumed the jti of a refused renewal")
	}

	rx := RecheckExpectation{Issuer: "scion-hub", Target: baseTarget(), OpenJTI: testOpenJTI, StreamID: 3}
	if _, err := VerifyRecheck(forgedRen, f.keys, rx, now); !errors.Is(err, ErrVia) {
		t.Fatalf("VerifyRecheck renewal: %v, want ErrVia", err)
	}
	rc := recheckOpenClaims()
	rc.Via = ViaTunnel
	forgedRecheckOpen := resignReplacing(t, f.signer.Key, f.mint(t, rc), `"via":"tunnel"`, `"via":"pty"`)
	if _, err := VerifyRecheck(forgedRecheckOpen, f.keys, rx, now); !errors.Is(err, ErrVia) {
		t.Fatalf("VerifyRecheck open grant: %v, want ErrVia", err)
	}
}
