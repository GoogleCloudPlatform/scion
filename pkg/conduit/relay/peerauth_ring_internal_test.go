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

package relay

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// Synthetic fixtures only (ptone/scion#1530 Phase 5). None of these is or
// was a real credential.
const (
	ringOldSecret = "p5-test-only-peer-ring-old-generation-0123456789"
	ringNewSecret = "p5-test-only-peer-ring-new-generation-0123456789"
)

var ringT0 = time.Unix(1_800_000_000, 0)

// ringSpec describes a two-generation signing ring: "old" (bounded by
// oldSignUntil/oldVerifyUntil when set) and "new" (no bounds).
type ringSpec struct {
	active       string
	emit         keyring.Emit
	readers      []string
	oldSignUntil time.Time // zero: no bounds on "old"
	oldVerify    time.Time
}

func ringFrom(t *testing.T, s ringSpec) *keyring.SigningRing {
	t.Helper()
	bound := func(at time.Time) any {
		if at.IsZero() {
			return nil
		}
		return at.UTC().Format(time.RFC3339)
	}
	readers := s.readers
	if readers == nil {
		readers = []string{}
	}
	doc := map[string]any{
		"schema":   1,
		"revision": "p5-relay-test",
		"signing": map[string]any{
			"active": s.active,
			"emit":   string(s.emit),
			"keys": []any{
				map[string]any{"id": "old", "secret": ringOldSecret, "sign_until": bound(s.oldSignUntil), "verify_until": bound(s.oldVerify)},
				map[string]any{"id": "new", "secret": ringNewSecret, "sign_until": nil, "verify_until": nil},
			},
			"legacy_read_ids": readers,
		},
		"encryption": map[string]any{
			"active":       "e1",
			"write_format": "v1",
			"keys": []any{
				map[string]any{"id": "e1", "key_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))},
			},
			"legacy_v1_read_ids": []any{"e1"},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keyring.Parse(raw)
	if err != nil {
		t.Fatalf("keyring.Parse: %v", err)
	}
	return k.Signing()
}

func ringAuth(t *testing.T, s ringSpec, self string, now func() time.Time) *HMACPeerAuth {
	t.Helper()
	a, err := NewHMACPeerAuthFromRing(HMACPeerAuthConfig{SelfID: self, Now: now}, ringFrom(t, s))
	if err != nil {
		t.Fatalf("NewHMACPeerAuthFromRing: %v", err)
	}
	return a
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// newPeerRequest is an unsigned relay-peer request with every signed field
// set.
func newPeerRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/conduit/sessions/s1/rpc?x=1", nil)
	req.Header.Set(HeaderBodySHA256, strings.Repeat("ab", 32))
	req.Header.Set(HeaderWant, `{"project_id":"p1"}`)
	SetPeerTarget(req, "relay-b", 7)
	return req
}

// assertNoRingSecret fails when s carries a fixture secret or a derived key.
func assertNoRingSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{ringOldSecret, ringNewSecret} {
		if strings.Contains(s, secret) {
			t.Fatalf("output carries a signing secret: %q", s)
		}
		key, err := deriveHMACPeerKey([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		for _, enc := range []string{base64.RawURLEncoding.EncodeToString(key), base64.StdEncoding.EncodeToString(key), string(key)} {
			if strings.Contains(s, enc) {
				t.Fatalf("output carries a derived key: %q", s)
			}
		}
	}
}

// TestHMACPeerAuthRing_ValidatesOnceSharedNonce (P5 acceptance): a request
// validates once against one replay cache per authenticator, whichever
// generation or format matched. A nonce admitted under one generation is a
// replay under the other, in either order, and a signature that does not
// match (a failed candidate, or no candidate) records nothing.
func TestHMACPeerAuthRing_ValidatesOnceSharedNonce(t *testing.T) {
	clock := fixedClock(ringT0)
	verifierSpec := ringSpec{active: "new", emit: keyring.EmitLegacy, readers: []string{"new", "old"}}
	oldLegacy := ringAuth(t, ringSpec{active: "old", emit: keyring.EmitLegacy, readers: []string{"old"}}, "relay-a", clock)
	newVersioned := ringAuth(t, ringSpec{active: "new", emit: keyring.EmitVersioned, readers: []string{"new"}}, "relay-a", clock)

	t.Run("old ID-less matches the second candidate once", func(t *testing.T) {
		v := ringAuth(t, verifierSpec, "relay-b", clock)
		req := newPeerRequest(t)
		if err := oldLegacy.Sign(req); err != nil {
			t.Fatal(err)
		}
		if peer, err := v.Verify(req); err != nil || peer != "relay-a" {
			t.Fatalf("Verify = %q, %v", peer, err)
		}
		if len(v.nonces) != 1 || len(v.expiry) != 1 {
			t.Fatalf("replay cache holds %d/%d entries, want exactly one", len(v.nonces), len(v.expiry))
		}
		if _, err := v.Verify(req); err == nil || !strings.Contains(err.Error(), "replayed nonce") {
			t.Fatalf("second Verify err = %v, want a replay refusal", err)
		}
	})

	for _, order := range []struct {
		name          string
		first, second *HMACPeerAuth
	}{
		{"old ID-less then new versioned", oldLegacy, newVersioned},
		{"new versioned then old ID-less", newVersioned, oldLegacy},
	} {
		t.Run("same nonce across generations and formats: "+order.name, func(t *testing.T) {
			v := ringAuth(t, verifierSpec, "relay-b", clock)
			first := newPeerRequest(t)
			if err := order.first.Sign(first); err != nil {
				t.Fatal(err)
			}
			if _, err := v.Verify(first); err != nil {
				t.Fatalf("first Verify: %v", err)
			}
			second := resignWithNonce(t, order.second, first.Header.Get(HeaderPeerNonce))
			if _, err := v.Verify(second); err == nil || !strings.Contains(err.Error(), "replayed nonce") {
				t.Fatalf("Verify of the nonce under the other generation err = %v, want a replay refusal", err)
			}
		})
	}

	t.Run("a failed signature consumes no nonce", func(t *testing.T) {
		v := ringAuth(t, verifierSpec, "relay-b", clock)
		req := newPeerRequest(t)
		if err := oldLegacy.Sign(req); err != nil {
			t.Fatal(err)
		}
		bad := req.Clone(req.Context())
		bad.Header.Set(HeaderPeerSignature, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
		if _, err := v.Verify(bad); !errors.Is(err, ErrPeerUnauthenticated) {
			t.Fatalf("bad signature err = %v", err)
		}
		if len(v.nonces) != 0 {
			t.Fatalf("a failed signature recorded %d nonces", len(v.nonces))
		}
		if _, err := v.Verify(req); err != nil {
			t.Fatalf("the valid request with the same nonce: %v", err)
		}
	})
}

// resignWithNonce signs a fresh request with signer and then replaces its
// nonce with nonce, re-signing so that the signature covers it.
func resignWithNonce(t *testing.T, signer *HMACPeerAuth, nonce string) *http.Request {
	t.Helper()
	req := newPeerRequest(t)
	if err := signer.Sign(req); err != nil {
		t.Fatal(err)
	}
	now := signer.cfg.Now()
	key, kid, err := signer.signingKey(now)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(HeaderPeerNonce, nonce)
	ts := req.Header.Get(HeaderPeerTimestamp)
	req.Header.Set(HeaderPeerSignature, base64.RawURLEncoding.EncodeToString(peerMAC(key, kid, canonical(req, signer.cfg.SelfID, ts, nonce))))
	return req
}

// goldenCanonical recomputes the canonical request input independently of
// canonical(): decimal length, colon, bytes, for each signed field.
func goldenCanonical(req *http.Request) []byte {
	var b bytes.Buffer
	for _, part := range []string{
		req.Method, req.URL.RequestURI(), req.Header.Get(HeaderPeerTimestamp), req.Header.Get(HeaderPeerNonce),
		req.Header.Get(HeaderPeerID), req.Header.Get(HeaderBodySHA256), req.Header.Get(HeaderWant),
		req.Header.Get(HeaderPeerTarget), req.Header.Get(HeaderPeerTargetGeneration),
	} {
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
	}
	return b.Bytes()
}

func goldenKey(t *testing.T, secret string) []byte {
	t.Helper()
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, "scion conduit relay-peer hmac v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func goldenMAC(key []byte, prefix string, input []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(prefix))
	m.Write(input)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// TestHMACPeerAuthRing_WireFormats pins both signature formats and the
// strict selection between them.
func TestHMACPeerAuthRing_WireFormats(t *testing.T) {
	clock := fixedClock(ringT0)

	t.Run("ID-less ring signature is byte-identical to the shared-secret signature", func(t *testing.T) {
		ring := ringAuth(t, ringSpec{active: "old", emit: keyring.EmitLegacy, readers: []string{"old"}}, "relay-a", clock)
		req := newPeerRequest(t)
		if err := ring.Sign(req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get(HeaderPeerKeyID) != "" || req.Header.Get(HeaderPeerSignatureVersion) != "" {
			t.Fatal("an ID-less signature carries versioned headers")
		}
		want := goldenMAC(goldenKey(t, ringOldSecret), "", goldenCanonical(req))
		if got := req.Header.Get(HeaderPeerSignature); got != want {
			t.Fatal("ID-less ring signature differs from the golden pre-change signature")
		}
		// The pre-change constructor accepts it.
		legacy, err := NewHMACPeerAuthFromSecret(HMACPeerAuthConfig{Secret: []byte(ringOldSecret), SelfID: "relay-b", Now: clock})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.Verify(req); err != nil {
			t.Fatalf("the shared-secret verifier refuses the ring's ID-less signature: %v", err)
		}
		// And the reverse: a shared-secret signature verifies on the ring.
		req2 := newPeerRequest(t)
		signer, err := NewHMACPeerAuthFromSecret(HMACPeerAuthConfig{Secret: []byte(ringOldSecret), SelfID: "relay-a", Now: clock})
		if err != nil {
			t.Fatal(err)
		}
		if err := signer.Sign(req2); err != nil {
			t.Fatal(err)
		}
		v := ringAuth(t, ringSpec{active: "new", emit: keyring.EmitLegacy, readers: []string{"new", "old"}}, "relay-b", clock)
		if _, err := v.Verify(req2); err != nil {
			t.Fatalf("ring verifier refuses a shared-secret signature of a legacy reader: %v", err)
		}
	})

	t.Run("versioned signature bytes", func(t *testing.T) {
		ring := ringAuth(t, ringSpec{active: "new", emit: keyring.EmitVersioned, readers: []string{"new"}}, "relay-a", clock)
		req := newPeerRequest(t)
		if err := ring.Sign(req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get(HeaderPeerKeyID) != "new" || req.Header.Get(HeaderPeerSignatureVersion) != "2" {
			t.Fatalf("versioned headers = %q/%q", req.Header.Get(HeaderPeerKeyID), req.Header.Get(HeaderPeerSignatureVersion))
		}
		want := goldenMAC(goldenKey(t, ringNewSecret), "scion-conduit-peer-v2\nnew\n", goldenCanonical(req))
		if got := req.Header.Get(HeaderPeerSignature); got != want {
			t.Fatal("versioned signature differs from the golden v2 MAC")
		}
	})

	t.Run("legacy Sign strips stale versioned headers", func(t *testing.T) {
		ring := ringAuth(t, ringSpec{active: "old", emit: keyring.EmitLegacy, readers: []string{"old"}}, "relay-a", clock)
		req := newPeerRequest(t)
		req.Header.Set(HeaderPeerKeyID, "new")
		req.Header.Set(HeaderPeerSignatureVersion, "2")
		if err := ring.Sign(req); err != nil {
			t.Fatal(err)
		}
		if _, ok := req.Header[HeaderPeerKeyID]; ok {
			t.Fatal("key ID header kept")
		}
		if _, ok := req.Header[HeaderPeerSignatureVersion]; ok {
			t.Fatal("version header kept")
		}
	})

	versioned := ringAuth(t, ringSpec{active: "new", emit: keyring.EmitVersioned, readers: []string{"new"}}, "relay-a", clock)
	strict := []struct {
		name   string
		mutate func(h http.Header)
	}{
		{"key ID without version", func(h http.Header) { h.Del(HeaderPeerSignatureVersion) }},
		{"version without key ID", func(h http.Header) { h.Del(HeaderPeerKeyID) }},
		{"duplicated key ID", func(h http.Header) { h.Add(HeaderPeerKeyID, "new") }},
		{"duplicated version", func(h http.Header) { h.Add(HeaderPeerSignatureVersion, "2") }},
		{"version 1", func(h http.Header) { h.Set(HeaderPeerSignatureVersion, "1") }},
		{"version 3", func(h http.Header) { h.Set(HeaderPeerSignatureVersion, "3") }},
		{"malformed key ID", func(h http.Header) { h.Set(HeaderPeerKeyID, "new!") }},
		{"overlong key ID", func(h http.Header) { h.Set(HeaderPeerKeyID, strings.Repeat("a", 33)) }},
		{"unknown key ID", func(h http.Header) { h.Set(HeaderPeerKeyID, "gen9") }},
		{"key ID swapped to the other valid generation", func(h http.Header) { h.Set(HeaderPeerKeyID, "old") }},
		{"a versioned signature with its headers stripped", func(h http.Header) {
			h.Del(HeaderPeerKeyID)
			h.Del(HeaderPeerSignatureVersion)
		}},
	}
	for _, tc := range strict {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			v := ringAuth(t, ringSpec{active: "new", emit: keyring.EmitLegacy, readers: []string{"new", "old"}}, "relay-b", clock)
			req := newPeerRequest(t)
			if err := versioned.Sign(req); err != nil {
				t.Fatal(err)
			}
			tc.mutate(req.Header)
			if _, err := v.Verify(req); !errors.Is(err, ErrPeerUnauthenticated) {
				t.Fatalf("Verify err = %v, want a refusal", err)
			}
			if len(v.nonces) != 0 {
				t.Fatal("a refused request recorded its nonce")
			}
		})
	}

	// Partial or repeated versioned headers on a request whose ID-less
	// signature is valid for the verifier: an ID-less retry would accept
	// it, so these cases fail if the selection ever falls back.
	idless := ringAuth(t, ringSpec{active: "old", emit: keyring.EmitLegacy, readers: []string{"old"}}, "relay-a", clock)
	partial := []struct {
		name   string
		mutate func(h http.Header)
	}{
		{"valid ID-less signature plus only a key ID", func(h http.Header) { h.Set(HeaderPeerKeyID, "old") }},
		{"valid ID-less signature plus only a version", func(h http.Header) { h.Set(HeaderPeerSignatureVersion, "2") }},
		{"valid ID-less signature plus a repeated version", func(h http.Header) {
			h.Add(HeaderPeerSignatureVersion, "2")
			h.Add(HeaderPeerSignatureVersion, "2")
		}},
		{"valid ID-less signature plus a key ID and version 3", func(h http.Header) {
			h.Set(HeaderPeerKeyID, "old")
			h.Set(HeaderPeerSignatureVersion, "3")
		}},
		{"valid ID-less signature plus a malformed key ID and version 2", func(h http.Header) {
			h.Set(HeaderPeerKeyID, "old!")
			h.Set(HeaderPeerSignatureVersion, "2")
		}},
		{"valid ID-less signature plus a repeated key ID", func(h http.Header) {
			h.Add(HeaderPeerKeyID, "old")
			h.Add(HeaderPeerKeyID, "old")
		}},
	}
	for _, tc := range partial {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			v := ringAuth(t, ringSpec{active: "new", emit: keyring.EmitLegacy, readers: []string{"new", "old"}}, "relay-b", clock)
			req := newPeerRequest(t)
			if err := idless.Sign(req); err != nil {
				t.Fatal(err)
			}
			control := req.Clone(req.Context())
			tc.mutate(req.Header)
			if _, err := v.Verify(req); !errors.Is(err, ErrPeerUnauthenticated) {
				t.Fatalf("Verify err = %v, want a refusal", err)
			}
			if len(v.nonces) != 0 {
				t.Fatal("a refused request recorded its nonce")
			}
			// The unmodified request is valid: the refusal came from the
			// header selection, not from the signature.
			if _, err := v.Verify(control); err != nil {
				t.Fatalf("the unmodified ID-less request: %v", err)
			}
		})
	}

	t.Run("a fixed-key verifier refuses a versioned signature of its own key", func(t *testing.T) {
		v, err := NewHMACPeerAuthFromSecret(HMACPeerAuthConfig{Secret: []byte(ringNewSecret), SelfID: "relay-b", Now: clock})
		if err != nil {
			t.Fatal(err)
		}
		req := newPeerRequest(t)
		if err := versioned.Sign(req); err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(req); !errors.Is(err, ErrPeerUnauthenticated) {
			t.Fatalf("Verify err = %v, want a refusal", err)
		}
	})

	t.Run("an ID-less signature of a generation outside the legacy readers is refused", func(t *testing.T) {
		v := ringAuth(t, ringSpec{active: "new", emit: keyring.EmitLegacy, readers: []string{"new"}}, "relay-b", clock)
		req := newPeerRequest(t)
		if err := ringAuth(t, ringSpec{active: "old", emit: keyring.EmitLegacy, readers: []string{"old"}}, "relay-a", clock).Sign(req); err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(req); !errors.Is(err, ErrPeerUnauthenticated) {
			t.Fatalf("Verify err = %v, want a refusal", err)
		}
	})
}

// TestHMACPeerAuthRing_SkewAndReplay: the timestamp window and replay
// refusal are unchanged for both formats.
func TestHMACPeerAuthRing_SkewAndReplay(t *testing.T) {
	for _, emit := range []keyring.Emit{keyring.EmitLegacy, keyring.EmitVersioned} {
		t.Run(string(emit), func(t *testing.T) {
			verifierNow := ringT0
			v := ringAuth(t, ringSpec{active: "new", emit: keyring.EmitLegacy, readers: []string{"new"}}, "relay-b", func() time.Time { return verifierNow })
			for _, tc := range []struct {
				offset time.Duration
				ok     bool
			}{
				{0, true}, {60 * time.Second, true}, {-60 * time.Second, true},
				{61 * time.Second, false}, {-61 * time.Second, false},
			} {
				signedAt := ringT0.Add(tc.offset)
				signer := ringAuth(t, ringSpec{active: "new", emit: emit, readers: []string{"new"}}, "relay-a", fixedClock(signedAt))
				req := newPeerRequest(t)
				if err := signer.Sign(req); err != nil {
					t.Fatal(err)
				}
				_, err := v.Verify(req)
				if tc.ok && err != nil {
					t.Fatalf("offset %v: %v", tc.offset, err)
				}
				if !tc.ok && (err == nil || !strings.Contains(err.Error(), "timestamp outside the allowed skew")) {
					t.Fatalf("offset %v: err = %v, want a skew refusal", tc.offset, err)
				}
				if tc.ok {
					if _, err := v.Verify(req); err == nil || !strings.Contains(err.Error(), "replayed nonce") {
						t.Fatalf("offset %v: replay err = %v", tc.offset, err)
					}
				}
			}
		})
	}
}

// TestHMACPeerAuthRing_Deadlines: an outgoing generation verifies strictly
// before its verify_until (both formats) and not at or after it; the
// active generation signs nothing at or after its sign_until, and no other
// key is used instead.
func TestHMACPeerAuthRing_Deadlines(t *testing.T) {
	signUntil := ringT0.Add(time.Hour)
	verifyUntil := ringT0.Add(2 * time.Hour)
	spec := func(active string, emit keyring.Emit) ringSpec {
		return ringSpec{active: active, emit: emit, readers: []string{"old", "new"}, oldSignUntil: signUntil, oldVerify: verifyUntil}
	}
	for _, emit := range []keyring.Emit{keyring.EmitLegacy, keyring.EmitVersioned} {
		t.Run(string(emit)+" verify_until", func(t *testing.T) {
			for _, tc := range []struct {
				at time.Time
				ok bool
			}{
				{verifyUntil.Add(-time.Second), true},
				{verifyUntil, false},
				{verifyUntil.Add(time.Second), false},
			} {
				// Signed while "old" could still sign (the signature's own
				// timestamp is within skew of the verifier for the check).
				signer := ringAuth(t, spec("old", emit), "relay-a", fixedClock(signUntil.Add(-time.Second)))
				key, kid, err := signer.signingKey(signUntil.Add(-time.Second))
				if err != nil {
					t.Fatal(err)
				}
				v := ringAuth(t, spec("new", keyring.EmitLegacy), "relay-b", fixedClock(tc.at))
				req := newPeerRequest(t)
				req.Header.Set(HeaderPeerID, "relay-a")
				req.Header.Set(HeaderPeerTimestamp, strconv.Itoa(int(tc.at.Unix())))
				req.Header.Set(HeaderPeerNonce, "n-"+strconv.Itoa(int(tc.at.Unix())))
				if kid != "" {
					req.Header.Set(HeaderPeerKeyID, kid)
					req.Header.Set(HeaderPeerSignatureVersion, "2")
				}
				req.Header.Set(HeaderPeerSignature, base64.RawURLEncoding.EncodeToString(peerMAC(key, kid, canonical(req, "relay-a", req.Header.Get(HeaderPeerTimestamp), req.Header.Get(HeaderPeerNonce)))))
				_, err = v.Verify(req)
				if tc.ok != (err == nil) {
					t.Fatalf("at verify_until%+v: err = %v, want ok=%v", tc.at.Sub(verifyUntil), err, tc.ok)
				}
			}
		})
	}

	t.Run("no signature at or after sign_until", func(t *testing.T) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		for _, at := range []time.Time{signUntil, signUntil.Add(time.Second)} {
			signer := ringAuth(t, spec("old", keyring.EmitVersioned), "relay-a", fixedClock(at))
			req := newPeerRequest(t)
			err := signer.Sign(req)
			if err == nil {
				t.Fatalf("Sign at sign_until%+v succeeded", at.Sub(signUntil))
			}
			assertNoRingSecret(t, err.Error())
			for _, h := range []string{HeaderPeerID, HeaderPeerTimestamp, HeaderPeerNonce, HeaderPeerSignature, HeaderPeerKeyID, HeaderPeerSignatureVersion} {
				if req.Header.Get(h) != "" {
					t.Fatalf("a refused Sign wrote %s", h)
				}
			}
		}
		assertNoRingSecret(t, logs.String())
	})
}

// TestNewHMACPeerAuthFromRing_Refusals: construction refuses an unusable
// ring, a generation whose secret is too short to derive from, and a
// secret passed alongside the ring, without naming key material.
func TestNewHMACPeerAuthFromRing_Refusals(t *testing.T) {
	if _, err := NewHMACPeerAuthFromRing(HMACPeerAuthConfig{SelfID: "a"}, nil); err == nil {
		t.Fatal("a nil ring was accepted")
	}
	if _, err := NewHMACPeerAuthFromRing(HMACPeerAuthConfig{SelfID: "a"}, &keyring.SigningRing{}); err == nil {
		t.Fatal("a zero ring was accepted")
	}
	short := keyring.Legacy("short-legacy").Signing() // under 16 bytes
	_, err := NewHMACPeerAuthFromRing(HMACPeerAuthConfig{SelfID: "a"}, short)
	if err == nil {
		t.Fatal("a generation too short to derive from was accepted")
	}
	if strings.Contains(err.Error(), "short-legacy") {
		t.Fatal("the refusal names the secret")
	}
	ring := ringFrom(t, ringSpec{active: "new", emit: keyring.EmitLegacy, readers: []string{"new"}})
	if _, err := NewHMACPeerAuthFromRing(HMACPeerAuthConfig{SelfID: "a", Secret: []byte(ringNewSecret)}, ring); err == nil {
		t.Fatal("a secret alongside the ring was accepted")
	}
	if _, err := NewHMACPeerAuthFromRing(HMACPeerAuthConfig{}, ring); err == nil {
		t.Fatal("a ring authenticator without SelfID was accepted")
	}
}

// TestConduitPeerAuth_RingSelection: NewConduitPeerAuth keys from the
// dedicated peer secret whenever it is set (the ring is ignored), from the
// ring when only the ring is set, and from the shared secret otherwise.
func TestConduitPeerAuth_RingSelection(t *testing.T) {
	ring := ringFrom(t, ringSpec{active: "new", emit: keyring.EmitVersioned, readers: []string{"new"}})
	build := func(o ConduitPeerAuthOptions) *HMACPeerAuth {
		t.Helper()
		o.Mode = "hmac"
		a, _, err := NewConduitPeerAuth(o)
		if err != nil {
			t.Fatal(err)
		}
		return a.(*HMACPeerAuth)
	}
	withPeerSecret := build(ConduitPeerAuthOptions{SelfID: "hub-a", PeerSecret: string(testPeerSecret), SigningRing: ring, SharedSecret: ringOldSecret})
	peerOnly := build(ConduitPeerAuthOptions{SelfID: "hub-a", PeerSecret: string(testPeerSecret)})
	if withPeerSecret.ring != nil || !bytes.Equal(withPeerSecret.key, peerOnly.key) {
		t.Fatal("with a dedicated peer secret the ring was used")
	}
	if a := build(ConduitPeerAuthOptions{SelfID: "hub-a", SigningRing: ring, SharedSecret: ringOldSecret}); a.ring == nil || a.key != nil {
		t.Fatal("ring mode did not key from the ring")
	}
	if a := build(ConduitPeerAuthOptions{SelfID: "hub-a", SharedSecret: ringOldSecret}); a.ring != nil || !bytes.Equal(a.key, goldenKey(t, ringOldSecret)) {
		t.Fatal("legacy mode changed")
	}
	_, _, err := NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: "hmac", SelfID: "hub-a", SigningRing: &keyring.SigningRing{}})
	var se *SettingError
	if !errors.As(err, &se) || se.Key != "server.keyring_file" {
		t.Fatalf("an unusable ring: err = %v, want a server.keyring_file SettingError", err)
	}
}
