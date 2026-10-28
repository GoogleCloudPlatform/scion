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

package keyring

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/securecookie"
)

const cookieName = "scion_session"

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// ringFile parses validDoc after mutate.
func ringFile(t *testing.T, mutate func(d map[string]any)) *SigningRing {
	t.Helper()
	d := validDoc()
	if mutate != nil {
		mutate(d)
	}
	k, err := Parse(marshal(t, d))
	if err != nil {
		t.Fatal(err)
	}
	return k.Signing()
}

// withActive makes id the active signing entry.
func withActive(id string) func(d map[string]any) {
	return func(d map[string]any) { sub(d, "signing")["active"] = id }
}

func mustCodecs(t *testing.T, r *SigningRing, h, b Purpose, maxAge int, now func() time.Time) []securecookie.Codec {
	t.Helper()
	codecs, err := CookieCodecs(r, h, b, maxAge, now)
	if err != nil {
		t.Fatalf("CookieCodecs: %v", err)
	}
	return codecs
}

// TestRingsWithoutActiveFailClosed: a zero, nil or empty ring never yields
// a key derived from an empty secret: Active errors, Generations and
// Candidates are empty, LegacySecret is absent, CookieCodecs refuses, and
// a zero or nil encryption ring has no key bytes and no candidates.
func TestRingsWithoutActiveFailClosed(t *testing.T) {
	var nilRing *SigningRing
	for name, r := range map[string]*SigningRing{
		"nil":   nilRing,
		"zero":  {},
		"empty": {active: "gen1"},
		// A legacy entry and legacy_read_ids make the LegacySecret and
		// ID-less Candidates assertions depend on the usable() guard.
		"dangling active": {active: "nope", legacyReads: []string{"legacy"}, entries: []signingEntry{
			{id: "legacy", secret: newMaterial([]byte(fixtureLegacySecret))},
			{id: "gen1", secret: newMaterial([]byte(fixtureGen1Secret))},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Active(sessionHash, t0); !errors.Is(err, ErrNoActiveGeneration) {
				t.Fatalf("Active = %v, want ErrNoActiveGeneration", err)
			}
			if g := r.Generations(sessionHash); len(g) != 0 {
				t.Fatalf("Generations = %v", g)
			}
			if c := r.Candidates(sessionHash, "", t0); len(c) != 0 {
				t.Fatalf("Candidates = %v", c)
			}
			if _, ok := r.LegacySecret(); ok {
				t.Fatal("LegacySecret present")
			}
			if codecs, err := CookieCodecs(r, sessionHash, sessionBlock, 86400, nil); !errors.Is(err, ErrNoActiveGeneration) || codecs != nil {
				t.Fatalf("CookieCodecs = %v, %v; want refusal", codecs, err)
			}
			for _, kid := range []string{"gen1", "nope", "legacy"} {
				if c := r.Candidates(sessionHash, kid, t0); len(c) != 0 {
					t.Fatalf("Candidates(%q) = %v", kid, c)
				}
			}
			if ids := r.IDs(); ids != nil {
				t.Fatalf("IDs = %v", ids)
			}
		})
	}
	var nilEnc *EncryptionRing
	for name, e := range map[string]*EncryptionRing{
		"nil":             nilEnc,
		"zero":            {},
		"dangling active": {active: "nope", legacyReads: []string{"legacy"}, entries: []encryptionEntry{{id: "legacy", key: newMaterial([]byte(fixtureOtherEncKey))}}},
	} {
		if ids := e.IDs(); ids != nil {
			t.Fatalf("%s encryption ring: IDs = %v", name, ids)
		}
		if b := e.Active().Bytes(); b != nil {
			t.Fatalf("%s encryption ring: Active has bytes", name)
		}
		if c := e.Candidates(""); len(c) != 0 {
			t.Fatalf("%s encryption ring: Candidates = %v", name, c)
		}
		if c := e.Candidates("legacy"); len(c) != 0 {
			t.Fatalf("%s encryption ring: Candidates(legacy) = %v", name, c)
		}
	}
	// Nil receivers on every exported accessor: zero values, no panic.
	var nk *Keyring
	if nk.Source() != "" || nk.Revision() != "" || nk.Signing() != nil || nk.Encryption() != nil {
		t.Fatal("nil Keyring accessors must return zero values")
	}
	if nilRing.ActiveID() != "" || nilRing.Emit() != "" || nilEnc.ActiveID() != "" || nilEnc.WriteFormat() != "" {
		t.Fatal("nil ring accessors must return zero values")
	}
	if _, err := nk.Signing().Active(sessionHash, t0); !errors.Is(err, ErrNoActiveGeneration) {
		t.Fatalf("nil Keyring signing Active = %v", err)
	}
	if nk.Encryption().Active().Bytes() != nil {
		t.Fatal("nil Keyring encryption key has bytes")
	}
}

func values() map[interface{}]interface{} {
	return map[interface{}]interface{}{"user_id": "u-1", "n": 7}
}

func decode(codecs []securecookie.Codec, v string) (map[interface{}]interface{}, error) {
	var dst map[interface{}]interface{}
	err := securecookie.DecodeMulti(cookieName, v, &dst, codecs...)
	return dst, err
}

func TestCookieCodecsLegacyMatchesPreKeyringStore(t *testing.T) {
	// The pre-keyring store: sessions.NewCookieStore(deriveSessionKey(s,
	// "scion-session-hash"), deriveSessionKey(s, "scion-session-block")),
	// deriveSessionKey = sha256(label + ":" + secret).
	derive := func(label string) []byte {
		sum := sha256.Sum256([]byte(label + ":" + fixtureLegacySecret))
		return sum[:]
	}
	old := securecookie.New(derive("scion-session-hash"), derive("scion-session-block"))
	old.MaxAge(86400)
	c := &clock{t0}
	codecs := mustCodecs(t, Legacy(fixtureLegacySecret).Signing(), sessionHash, sessionBlock, 86400, c.now)
	if len(codecs) != 1 {
		t.Fatalf("legacy codecs = %d", len(codecs))
	}
	fromOld, err := old.Encode(cookieName, values())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decode(codecs, fromOld); err != nil || got["user_id"] != "u-1" {
		t.Fatalf("keyring codec cannot read a pre-keyring cookie: %v", err)
	}
	fromNew, err := securecookie.EncodeMulti(cookieName, values(), codecs...)
	if err != nil {
		t.Fatal(err)
	}
	var dst map[interface{}]interface{}
	if err := old.Decode(cookieName, fromNew, &dst); err != nil {
		t.Fatalf("pre-keyring codec cannot read a keyring cookie: %v", err)
	}
}

func TestCookieCodecsCrossGenerationAndActiveReencode(t *testing.T) {
	c := &clock{t0}
	// Replica A: active legacy. Replica B: active gen1. Same file otherwise.
	a := mustCodecs(t, ringFile(t, withActive("legacy")), sessionHash, sessionBlock, 86400, c.now)
	b := mustCodecs(t, ringFile(t, nil), sessionHash, sessionBlock, 86400, c.now)

	fromA, err := securecookie.EncodeMulti(cookieName, values(), a...)
	if err != nil {
		t.Fatal(err)
	}
	if DecodesWithActive(cookieName, fromA, b) {
		t.Fatal("A's cookie must not decode under B's active generation")
	}
	got, err := decode(b, fromA)
	if err != nil || got["user_id"] != "u-1" {
		t.Fatalf("B cannot read A's cookie: %v", err)
	}
	// Re-save on B uses B's active pair.
	resaved, err := securecookie.EncodeMulti(cookieName, got, b...)
	if err != nil {
		t.Fatal(err)
	}
	if !DecodesWithActive(cookieName, resaved, b) {
		t.Fatal("re-saved cookie is not under B's active pair")
	}
	if DecodesWithActive(cookieName, resaved, a) {
		t.Fatal("re-saved cookie still under the old pair")
	}
	// And A reads B's cookie (mixed active generations read each other).
	if _, err := decode(a, resaved); err != nil {
		t.Fatalf("A cannot read B's cookie: %v", err)
	}
	// A staged generation (gen2) cookie is readable before activation
	// elsewhere is complete.
	staged := mustCodecs(t, ringFile(t, withActive("gen2")), sessionHash, sessionBlock, 86400, c.now)
	fromStaged, err := securecookie.EncodeMulti(cookieName, values(), staged...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(b, fromStaged); err != nil {
		t.Fatalf("B cannot read a gen2 cookie: %v", err)
	}
}

func TestCookieCodecsDeadlines(t *testing.T) {
	c := &clock{t0}
	// legacy: sign_until 13:00, verify_until next day 13:00 (validDoc).
	a := mustCodecs(t, ringFile(t, withActive("legacy")), sessionHash, sessionBlock, 86400*3, c.now)
	b := mustCodecs(t, ringFile(t, nil), sessionHash, sessionBlock, 86400*3, c.now)
	fromA, err := securecookie.EncodeMulti(cookieName, values(), a...)
	if err != nil {
		t.Fatal(err)
	}

	// At sign_until, A (active legacy) stops issuing, and does not fall
	// through to gen1/gen2 (non-active codecs never encode).
	c.t = time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)
	_, err = securecookie.EncodeMulti(cookieName, values(), a...)
	if err == nil {
		t.Fatal("A encoded at sign_until")
	}
	var multi securecookie.MultiError
	if !errors.As(err, &multi) || !errors.Is(multi[0], ErrCookieIssuanceClosed) {
		t.Fatalf("encode error = %v, want issuance closed first", err)
	}
	for _, e := range multi[1:] {
		if !errors.Is(e, ErrCookieNotActive) {
			t.Fatalf("non-active codec error = %v", e)
		}
	}
	// B still verifies the legacy cookie before verify_until.
	if _, err := decode(b, fromA); err != nil {
		t.Fatalf("legacy cookie refused before verify_until: %v", err)
	}
	c.t = time.Date(2026, 10, 22, 12, 59, 59, 0, time.UTC)
	if _, err := decode(b, fromA); err != nil {
		t.Fatalf("legacy cookie refused 1s before verify_until: %v", err)
	}
	// At verify_until, the previous codec refuses even though the key is
	// still in the ring and the cookie timestamp is in its window.
	c.t = time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)
	_, err = decode(b, fromA)
	if err == nil {
		t.Fatal("expired previous codec decoded")
	}
	if !errors.As(err, &multi) || len(multi) != 3 || !errors.Is(multi[1], ErrCookieVerificationClosed) {
		t.Fatalf("decode error = %#v, want the legacy codec to refuse with verification closed", err)
	}
}

func TestCookieCodecsLimits(t *testing.T) {
	c := &clock{t0}
	b := mustCodecs(t, ringFile(t, nil), sessionHash, sessionBlock, 86400, c.now)
	// 4096-byte securecookie length limit stays in force.
	big := map[interface{}]interface{}{"blob": strings.Repeat("x", 5000)}
	if _, err := securecookie.EncodeMulti(cookieName, big, b...); err == nil {
		t.Fatal("oversize cookie encoded")
	}
	// MaxAge reaches the inner codecs. securecookie stamps and checks with
	// the wall clock, so instead of waiting out a short max age this uses
	// max age -1: a codec refuses a value when its timestamp is older than
	// now - maxAge, which with -1 is every value, while the 24h codec
	// accepts the same fresh cookie.
	v, err := securecookie.EncodeMulti(cookieName, values(), b...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(b, v); err != nil {
		t.Fatalf("24h codec refused a fresh cookie: %v", err)
	}
	expired := mustCodecs(t, ringFile(t, nil), sessionHash, sessionBlock, -1, c.now)
	if _, err := decode(expired, v); err == nil {
		t.Fatal("max age not applied to inner codecs")
	}
}
