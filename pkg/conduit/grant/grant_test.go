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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

type fixture struct {
	ring   KeyRing
	signer *Signer
	keys   KeySet
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	k, err := NewRingKey(t0.Add(-time.Hour), t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ring: KeyRing{Keys: []RingKey{k}}}
	f.signer, err = f.ring.Signer(t0)
	if err != nil {
		t.Fatal(err)
	}
	f.keys, err = NewKeySet(f.ring.PublicKeys(t0)...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func baseTarget() Target {
	return Target{Kind: TargetKindAgent, ID: "agent-1", EndpointIncarnation: "inc-1", SessionID: "sess-1", ConnectionEpoch: 7}
}

func baseClaims() Claims {
	return Claims{
		Issuer:    "scion-hub",
		Subject:   "user-1",
		ProjectID: "proj-1",
		Target:    baseTarget(),
		Stream:    StreamHeader{Kind: StreamKindTCP, Params: map[string]string{ParamHost: "127.0.0.1", ParamPort: "8080"}},
		NotBefore: t0,
		Expiry:    t0.Add(30 * time.Second),
	}
}

func baseExpect() Expectation {
	return Expectation{
		Target:    baseTarget(),
		ProjectID: "proj-1",
		Header:    StreamHeader{Kind: StreamKindTCP, Params: map[string]string{ParamPort: "8080", ParamHost: "127.0.0.1"}},
	}
}

func (f *fixture) mint(t *testing.T, c Claims) []byte {
	t.Helper()
	tok, err := Mint(f.signer, c)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tok
}

func newCache() *MemoryReplayCache {
	return NewMemoryReplayCache(func() time.Time { return t0 }, 0)
}

func TestVerify_Valid(t *testing.T) {
	f := newFixture(t)
	tok := f.mint(t, baseClaims())
	c, err := Verify(context.Background(), tok, f.keys, baseExpect(), newCache(), t0.Add(time.Second))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Audience != Audience || c.KeyID != f.signer.KeyID || c.JTI == "" || c.Subject != "user-1" {
		t.Fatalf("unexpected claims %+v", c)
	}
}

// TestVerify_Refusals covers the C16 unit-level refusals that come from the
// claims or the target's expectation (wrong project / incarnation /
// capability / session / epoch, stream tampering, time window).
func TestVerify_Refusals(t *testing.T) {
	tests := []struct {
		name   string
		claims func(*Claims)
		expect func(*Expectation)
		now    time.Time
		want   error
	}{
		{name: "wrong project", expect: func(e *Expectation) { e.ProjectID = "proj-2" }, want: ErrProject},
		{name: "wrong incarnation", expect: func(e *Expectation) { e.Target.EndpointIncarnation = "inc-2" }, want: ErrTarget},
		{name: "wrong target session", expect: func(e *Expectation) { e.Target.SessionID = "sess-2" }, want: ErrTarget},
		{name: "new epoch refuses old grant", expect: func(e *Expectation) { e.Target.ConnectionEpoch = 8 }, want: ErrTarget},
		{name: "wrong target id", expect: func(e *Expectation) { e.Target.ID = "agent-2" }, want: ErrTarget},
		{name: "wrong target kind", expect: func(e *Expectation) { e.Target.Kind = TargetKindBroker }, want: ErrTarget},
		{name: "wrong capability (kind)", expect: func(e *Expectation) {
			e.Header = StreamHeader{Kind: StreamKindPTY, Params: e.Header.Params}
		}, want: ErrStream},
		{name: "tampered param", expect: func(e *Expectation) { e.Header.Params[ParamPort] = "22" }, want: ErrStream},
		{name: "extra param", expect: func(e *Expectation) { e.Header.Params["extra"] = "x" }, want: ErrStream},
		{name: "missing param", expect: func(e *Expectation) { delete(e.Header.Params, ParamHost) }, want: ErrStream},
		{name: "nil params vs set", expect: func(e *Expectation) { e.Header.Params = nil }, want: ErrStream},
		{name: "wrong issuer", expect: func(e *Expectation) { e.Issuer = "other" }, want: ErrIssuer},
		{name: "nbf in the future", now: t0.Add(-5 * time.Second), want: ErrNotYetValid},
		{name: "expired", now: t0.Add(30 * time.Second), want: ErrExpired},
		{name: "expired long ago", now: t0.Add(time.Hour), want: ErrExpired},
		{name: "skew does not exceed cap", expect: func(e *Expectation) { e.ClockSkew = time.Hour }, now: t0.Add(-11 * time.Second), want: ErrNotYetValid},
		{name: "incomplete expectation", expect: func(e *Expectation) { e.Target.SessionID = "" }, want: ErrExpectation},
		{name: "zero epoch expectation", expect: func(e *Expectation) { e.Target.ConnectionEpoch = 0 }, want: ErrExpectation},
		{name: "missing project expectation", expect: func(e *Expectation) { e.ProjectID = "" }, want: ErrExpectation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			c := baseClaims()
			if tt.claims != nil {
				tt.claims(&c)
			}
			tok := f.mint(t, c)
			e := baseExpect()
			if tt.expect != nil {
				tt.expect(&e)
			}
			now := tt.now
			if now.IsZero() {
				now = t0.Add(time.Second)
			}
			cache := newCache()
			if _, err := Verify(context.Background(), tok, f.keys, e, cache, now); !errors.Is(err, tt.want) {
				t.Fatalf("Verify err = %v, want %v", err, tt.want)
			}
			if cache.Len() != 0 {
				t.Fatalf("a refused grant consumed its jti")
			}
		})
	}
}

func TestVerify_ClockSkewTolerated(t *testing.T) {
	f := newFixture(t)
	tok := f.mint(t, baseClaims())
	e := baseExpect()
	e.ClockSkew = 2 * time.Second
	if _, err := Verify(context.Background(), tok, f.keys, e, newCache(), t0.Add(-time.Second)); err != nil {
		t.Fatalf("Verify with skew: %v", err)
	}
}

func TestVerify_ParamsCompareAsMap(t *testing.T) {
	f := newFixture(t)
	c := baseClaims()
	c.Stream = StreamHeader{Kind: StreamKindPTY}
	tok := f.mint(t, c)
	e := baseExpect()
	e.Header = StreamHeader{Kind: StreamKindPTY, Params: map[string]string{}}
	if _, err := Verify(context.Background(), tok, f.keys, e, newCache(), t0); err != nil {
		t.Fatalf("empty and nil params must compare equal: %v", err)
	}
}

func TestMint_RejectsValidityWindowOver60s(t *testing.T) {
	f := newFixture(t)
	c := baseClaims()
	c.Expiry = c.NotBefore.Add(61 * time.Second)
	if _, err := Mint(f.signer, c); !errors.Is(err, ErrValidityWindow) {
		t.Fatalf("Mint err = %v, want ErrValidityWindow", err)
	}
	c.Expiry = c.NotBefore.Add(MaxValidity)
	if _, err := Mint(f.signer, c); err != nil {
		t.Fatalf("Mint at exactly MaxValidity: %v", err)
	}
}

func TestMint_RejectsInvalidClaims(t *testing.T) {
	f := newFixture(t)
	tests := map[string]func(*Claims){
		"no issuer":       func(c *Claims) { c.Issuer = "" },
		"no subject":      func(c *Claims) { c.Subject = "" },
		"no project":      func(c *Claims) { c.ProjectID = "" },
		"bad kind":        func(c *Claims) { c.Stream.Kind = "shell" },
		"empty kind":      func(c *Claims) { c.Stream.Kind = "" },
		"no session":      func(c *Claims) { c.Target.SessionID = "" },
		"no epoch":        func(c *Claims) { c.Target.ConnectionEpoch = 0 },
		"bad target kind": func(c *Claims) { c.Target.Kind = "user" },
		"exp before nbf":  func(c *Claims) { c.Expiry = c.NotBefore.Add(-time.Second) },
		"wrong audience":  func(c *Claims) { c.Audience = "hub" },
		"kid mismatch":    func(c *Claims) { c.KeyID = "other" },
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			c := baseClaims()
			mut(&c)
			if _, err := Mint(f.signer, c); err == nil {
				t.Fatal("Mint accepted invalid claims")
			}
		})
	}
}

// forge re-signs an arbitrary header and payload with priv.
func forge(priv ed25519.PrivateKey, hdr, payload any) []byte {
	h, _ := json.Marshal(hdr)
	p, _ := json.Marshal(payload)
	in := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	return []byte(in + "." + b64.EncodeToString(ed25519.Sign(priv, []byte(in))))
}

func payloadOf(t *testing.T, tok []byte) map[string]any {
	t.Helper()
	parts := strings.Split(string(tok), ".")
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVerify_ForgedAndMalformed(t *testing.T) {
	f := newFixture(t)
	good := f.mint(t, baseClaims())
	parts := strings.Split(string(good), ".")
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	pl := payloadOf(t, good)

	hs256 := func() []byte {
		// Alg confusion: an HS256 token "signed" with the public key bytes.
		pub, _ := f.keys.Lookup(f.signer.KeyID)
		h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": TokenType, "kid": f.signer.KeyID})
		in := b64.EncodeToString(h) + "." + parts[1]
		mac := hmac.New(sha256.New, pub.Key)
		mac.Write([]byte(in))
		return []byte(in + "." + b64.EncodeToString(mac.Sum(nil)))
	}

	tamperedPayload := func() []byte {
		pl2 := payloadOf(t, good)
		pl2["project_id"] = "proj-evil"
		p, _ := json.Marshal(pl2)
		return []byte(parts[0] + "." + b64.EncodeToString(p) + "." + parts[2])
	}

	tests := []struct {
		name string
		tok  []byte
		want error
	}{
		{"forged signature (other key, same kid)", forge(otherPriv, header{Algorithm, TokenType, f.signer.KeyID}, pl), ErrSignature},
		{"tampered payload", tamperedPayload(), ErrSignature},
		{"flipped signature byte", []byte(parts[0] + "." + parts[1] + "." + flip(parts[2])), ErrSignature},
		{"unknown kid", forge(otherPriv, header{Algorithm, TokenType, "cg-unknown"}, pl), ErrUnknownKey},
		{"empty kid", forge(f.signer.Key, header{Algorithm, TokenType, ""}, pl), ErrMalformed},
		{"alg none", forge(f.signer.Key, map[string]string{"alg": "none", "typ": TokenType, "kid": f.signer.KeyID}, pl), ErrAlgorithm},
		{"alg confusion HS256", hs256(), ErrAlgorithm},
		{"alg ES256", forge(f.signer.Key, header{"ES256", TokenType, f.signer.KeyID}, pl), ErrAlgorithm},
		{"wrong typ", forge(f.signer.Key, header{Algorithm, "JWT", f.signer.KeyID}, pl), ErrMalformed},
		{"extra header field", forge(f.signer.Key, map[string]string{"alg": Algorithm, "typ": TokenType, "kid": f.signer.KeyID, "jku": "https://evil"}, pl), ErrMalformed},
		{"payload kid differs from header", forge(f.signer.Key, header{Algorithm, TokenType, f.signer.KeyID}, with(pl, "kid", "other")), ErrMalformed},
		{"wrong audience", forge(f.signer.Key, header{Algorithm, TokenType, f.signer.KeyID}, with(pl, "aud", "hub")), ErrAudience},
		{"unknown payload field", forge(f.signer.Key, header{Algorithm, TokenType, f.signer.KeyID}, with(pl, "admin", true)), ErrMalformed},
		{"validity window over 60s", forge(f.signer.Key, header{Algorithm, TokenType, f.signer.KeyID}, with(pl, "exp", float64(t0.Add(61*time.Second).Unix()))), ErrValidityWindow},
		{"missing jti", forge(f.signer.Key, header{Algorithm, TokenType, f.signer.KeyID}, with(pl, "jti", "")), ErrInvalidClaims},
		{"two parts", []byte(parts[0] + "." + parts[1]), ErrMalformed},
		{"four parts", append(append([]byte{}, good...), ".x"...), ErrMalformed},
		{"padded base64", []byte(parts[0] + "=." + parts[1] + "." + parts[2]), ErrMalformed},
		{"empty", nil, ErrMalformed},
		{"oversized", make([]byte, maxTokenSize+1), ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newCache()
			if _, err := Verify(context.Background(), tt.tok, f.keys, baseExpect(), cache, t0.Add(time.Second)); !errors.Is(err, tt.want) {
				t.Fatalf("Verify err = %v, want %v", err, tt.want)
			}
			if cache.Len() != 0 {
				t.Fatal("a refused grant consumed a jti")
			}
		})
	}
}

func with(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m))
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func TestVerify_ReplayRefused(t *testing.T) {
	f := newFixture(t)
	tok := f.mint(t, baseClaims())
	cache := newCache()
	if _, err := Verify(context.Background(), tok, f.keys, baseExpect(), cache, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), tok, f.keys, baseExpect(), cache, t0); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay err = %v, want ErrReplay", err)
	}
}

// TestVerify_ConcurrentReplayExactlyOneWins: C16 "concurrent replay of one
// jti (exactly one stream results)". Run with -race.
func TestVerify_ConcurrentReplayExactlyOneWins(t *testing.T) {
	f := newFixture(t)
	tok := f.mint(t, baseClaims())
	cache := newCache()
	const n = 256
	var wins, replays atomic.Int64
	var start, done sync.WaitGroup
	start.Add(1)
	for range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, err := Verify(context.Background(), tok, f.keys, baseExpect(), cache, t0)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrReplay):
				replays.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	start.Done()
	done.Wait()
	if wins.Load() != 1 || replays.Load() != n-1 {
		t.Fatalf("wins=%d replays=%d, want 1 and %d", wins.Load(), replays.Load(), n-1)
	}
}

// TestKeyRing_RotationOverlap: C16 "key rotation with overlap (old-key grants
// valid until exp, then refused)".
func TestKeyRing_RotationOverlap(t *testing.T) {
	ring := KeyRing{}
	k1, _ := NewRingKey(t0, t0)
	ring.Keys = append(ring.Keys, k1)

	const activateAfter = 5 * time.Minute
	const overlap = 2 * time.Minute
	k2, _ := NewRingKey(t0, t0)
	if err := ring.Rotate(t0, k2, activateAfter, overlap); err != nil {
		t.Fatal(err)
	}
	switchAt := t0.Add(activateAfter)
	retireAt := switchAt.Add(overlap)

	// Before the switch: k1 signs, both keys are published (publish-before-use).
	s, err := ring.Signer(switchAt.Add(-time.Second))
	if err != nil || s.KeyID != k1.KeyID {
		t.Fatalf("signer before switch = %v, %v; want k1", s, err)
	}
	pubs := ring.PublicKeys(t0)
	if len(pubs) != 2 || pubs[0].KeyID != k1.KeyID || !pubs[0].NotAfter.Equal(retireAt) || pubs[1].KeyID != k2.KeyID || !pubs[1].NotAfter.IsZero() {
		t.Fatalf("published keys during rotation = %+v", pubs)
	}

	// A grant k1 signed just before the switch, verified by a target that
	// learned the key set during rotation.
	last := baseClaims()
	last.NotBefore = switchAt.Add(-time.Second)
	last.Expiry = last.NotBefore.Add(MaxValidity)
	oldTok, err := Mint(s, last)
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := NewKeySet(pubs...)

	// After the switch: k2 signs.
	s2, err := ring.Signer(switchAt)
	if err != nil || s2.KeyID != k2.KeyID {
		t.Fatalf("signer after switch = %v, %v; want k2", s2, err)
	}
	// The old-key grant is still valid until its exp.
	if _, err := Verify(context.Background(), oldTok, keys, baseExpect(), newCache(), last.Expiry.Add(-time.Second)); err != nil {
		t.Fatalf("old-key grant refused before exp: %v", err)
	}
	if !last.Expiry.Before(retireAt) {
		t.Fatalf("overlap does not cover max validity")
	}
	// After the key's not_after, an old-key grant is refused even if a
	// target still holds the key.
	late := baseClaims()
	late.NotBefore = retireAt.Add(-10 * time.Second)
	late.Expiry = retireAt.Add(10 * time.Second)
	lateTok, err := Mint(s, late)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), lateTok, keys, baseExpect(), newCache(), retireAt); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("grant after key not_after: err = %v, want ErrKeyExpired", err)
	}
	// Once retired, k1 is no longer published and pruning drops it.
	if pubs := ring.PublicKeys(retireAt); len(pubs) != 1 || pubs[0].KeyID != k2.KeyID {
		t.Fatalf("published keys after retirement = %+v", pubs)
	}
	ring.Prune(retireAt)
	if len(ring.Keys) != 1 || ring.Keys[0].KeyID != k2.KeyID {
		t.Fatalf("prune kept %v", ring)
	}
	// A target whose key set no longer lists k1 refuses with unknown kid.
	keys2, _ := NewKeySet(ring.PublicKeys(retireAt)...)
	if _, err := Verify(context.Background(), oldTok, keys2, baseExpect(), newCache(), last.Expiry.Add(-time.Second)); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestKeyRing_SignerNeverOutlivesKey(t *testing.T) {
	k, _ := NewRingKey(t0, t0)
	k.NotAfter = t0.Add(30 * time.Second)
	ring := KeyRing{Keys: []RingKey{k}}
	if _, err := ring.Signer(t0); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("err = %v, want ErrNoSigningKey", err)
	}
}

func TestKeyRing_RotateValidation(t *testing.T) {
	k1, _ := NewRingKey(t0, t0)
	ring := KeyRing{Keys: []RingKey{k1}}
	k2, _ := NewRingKey(t0, t0)
	if err := ring.Rotate(t0, k2, 0, MaxValidity-time.Second); err == nil {
		t.Fatal("overlap shorter than MaxValidity accepted")
	}
	if err := ring.Rotate(t0, k1, 0, time.Hour); err == nil {
		t.Fatal("duplicate kid accepted")
	}
	if err := ring.Rotate(t0, RingKey{KeyID: "x", Seed: []byte{1}}, 0, time.Hour); err == nil {
		t.Fatal("bad seed accepted")
	}
	if err := ring.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := KeyRing{Keys: []RingKey{k1, k1}}
	if err := bad.Validate(); err == nil {
		t.Fatal("duplicate kid validated")
	}
}

func TestNewKeySet_Validation(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	if _, err := NewKeySet(PublicKey{KeyID: "a", Key: pub}, PublicKey{KeyID: "a", Key: pub}); err == nil {
		t.Fatal("duplicate kid accepted")
	}
	if _, err := NewKeySet(PublicKey{KeyID: "", Key: pub}); err == nil {
		t.Fatal("empty kid accepted")
	}
	if _, err := NewKeySet(PublicKey{KeyID: "a", Key: pub[:5]}); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestSecretsRedacted(t *testing.T) {
	k, _ := NewRingKey(t0, t0)
	ring := KeyRing{Keys: []RingKey{k}}
	s, _ := ring.Signer(t0)
	seedHex := fmt.Sprintf("%x", k.Seed)
	for _, out := range []string{
		fmt.Sprintf("%v %+v %#v %s", k, k, k, k),
		fmt.Sprintf("%v %+v %#v", ring, ring, ring),
		fmt.Sprintf("%v %+v %#v", *s, *s, *s),
	} {
		if strings.Contains(out, seedHex) || strings.Contains(out, fmt.Sprint(k.Seed)) || strings.Contains(out, fmt.Sprintf("%x", []byte(s.Key))) {
			t.Fatalf("secret material in formatted output: %s", out)
		}
	}
}

func TestMemoryReplayCache_Eviction(t *testing.T) {
	now := t0
	c := NewMemoryReplayCache(func() time.Time { return now }, 2)
	ctx := context.Background()
	for _, j := range []string{"a", "b"} {
		if fresh, err := c.Consume(ctx, j, t0.Add(10*time.Second)); err != nil || !fresh {
			t.Fatalf("consume %s: %v %v", j, fresh, err)
		}
	}
	if _, err := c.Consume(ctx, "c", t0.Add(10*time.Second)); !errors.Is(err, ErrReplayCacheFull) {
		t.Fatalf("full cache err = %v", err)
	}
	if fresh, _ := c.Consume(ctx, "a", t0.Add(10*time.Second)); fresh {
		t.Fatal("replay reported fresh")
	}
	now = t0.Add(11 * time.Second)
	if fresh, err := c.Consume(ctx, "c", now.Add(10*time.Second)); err != nil || !fresh {
		t.Fatalf("consume after expiry: %v %v", fresh, err)
	}
	if c.Len() != 1 {
		t.Fatalf("expired entries not evicted: len=%d", c.Len())
	}
	if _, err := c.Consume(ctx, "", now); err == nil {
		t.Fatal("empty jti accepted")
	}
}

type failingCache struct{}

func (failingCache) Consume(context.Context, string, time.Time) (bool, error) {
	return false, errors.New("boom")
}

func TestVerify_ReplayCacheErrorFailsClosed(t *testing.T) {
	f := newFixture(t)
	tok := f.mint(t, baseClaims())
	if _, err := Verify(context.Background(), tok, f.keys, baseExpect(), failingCache{}, t0); !errors.Is(err, ErrReplay) {
		t.Fatalf("err = %v, want ErrReplay", err)
	}
}
