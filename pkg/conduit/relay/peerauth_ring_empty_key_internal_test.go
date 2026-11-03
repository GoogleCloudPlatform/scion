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
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// signPeerRequestWithKey signs req as peer with key in the format kid
// selects ("" for the ID-less format), as a caller holding key would.
func signPeerRequestWithKey(req *http.Request, key []byte, kid, peer, nonce string, at time.Time) {
	ts := strconv.FormatInt(at.Unix(), 10)
	req.Header.Set(HeaderPeerID, peer)
	req.Header.Set(HeaderPeerTimestamp, ts)
	req.Header.Set(HeaderPeerNonce, nonce)
	if kid != "" {
		req.Header.Set(HeaderPeerKeyID, kid)
		req.Header.Set(HeaderPeerSignatureVersion, peerSignatureVersion2)
	}
	req.Header.Set(HeaderPeerSignature, base64.RawURLEncoding.EncodeToString(peerMAC(key, kid, canonical(req, peer, ts, nonce))))
}

// TestConduitPeerAuth_KeyringModeRefusesEmptyKeyAndUnsigned: a hub in
// keyring mode builds its relay-peer authenticator with an empty
// SharedSecret and its signing ring. That authenticator refuses a request
// signed with an empty key (nil, zero-length, or the key either HKDF label
// derives from an empty secret), in both signature formats and under every
// key ID the ring holds, and refuses an unsigned request (no peer headers;
// peer headers without a signature; an empty signature). The empty shared
// secret can therefore never verify a peer. Control: the same manual
// signing with a generation's real key is accepted, so the refusals come
// from the key, not from the signing helper.
func TestConduitPeerAuth_KeyringModeRefusesEmptyKeyAndUnsigned(t *testing.T) {
	emptyDerived := func(info string) []byte {
		t.Helper()
		key, err := hkdf.Key(sha256.New, []byte{}, nil, info, 32)
		if err != nil {
			t.Fatalf("hkdf of an empty secret under %q: %v", info, err)
		}
		return key
	}
	emptyKeys := map[string][]byte{
		"nil key":                       nil,
		"zero-length key":               {},
		"shared-secret label, empty":    emptyDerived(hmacPeerKeyInfo),
		"peer-secret label, empty":      emptyDerived(hmacPeerSecretKeyInfo),
		"32 zero bytes (an unset slot)": make([]byte, 32),
	}
	for _, emit := range []keyring.Emit{keyring.EmitLegacy, keyring.EmitVersioned} {
		t.Run(string(emit), func(t *testing.T) {
			ring := ringFrom(t, ringSpec{active: "new", emit: emit, readers: []string{"old", "new"}})
			a, _, err := NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: "hmac", SelfID: "hub-a", SharedSecret: "", SigningRing: ring})
			if err != nil {
				t.Fatalf("keyring-mode options: %v", err)
			}
			verifier := a.(*HMACPeerAuth)
			if verifier.ring == nil || verifier.key != nil {
				t.Fatal("keyring-mode options did not key from the ring alone")
			}
			n := 0
			nonce := func() string { n++; return "empty-key-nonce-" + strconv.Itoa(n) }
			refused := func(t *testing.T, req *http.Request, what string) {
				t.Helper()
				if peer, err := verifier.Verify(req); !errors.Is(err, ErrPeerUnauthenticated) {
					t.Fatalf("%s: Verify = (%q, %v), want ErrPeerUnauthenticated", what, peer, err)
				}
			}

			// Control: the real generation keys verify through the same helper.
			for _, c := range []struct{ kid, secret string }{{"", ringNewSecret}, {"", ringOldSecret}, {"new", ringNewSecret}, {"old", ringOldSecret}} {
				req := newPeerRequest(t)
				signPeerRequestWithKey(req, goldenKey(t, c.secret), c.kid, "hub-b", nonce(), time.Now())
				if peer, err := verifier.Verify(req); err != nil || peer != "hub-b" {
					t.Fatalf("control kid %q: Verify = (%q, %v), want hub-b", c.kid, peer, err)
				}
			}

			// (a) Signed with an empty key, ID-less and under each key ID.
			for name, key := range emptyKeys {
				for _, kid := range []string{"", "new", "old"} {
					req := newPeerRequest(t)
					signPeerRequestWithKey(req, key, kid, "hub-b", nonce(), time.Now())
					refused(t, req, name+", kid "+strconv.Quote(kid))
				}
			}

			// (b) Unsigned.
			refused(t, newPeerRequest(t), "no relay-peer headers")
			noSig := newPeerRequest(t)
			signPeerRequestWithKey(noSig, goldenKey(t, ringNewSecret), "", "hub-b", nonce(), time.Now())
			noSig.Header.Del(HeaderPeerSignature)
			refused(t, noSig, "peer headers without a signature")
			emptySig := newPeerRequest(t)
			signPeerRequestWithKey(emptySig, goldenKey(t, ringNewSecret), "new", "hub-b", nonce(), time.Now())
			emptySig.Header.Set(HeaderPeerSignature, "")
			refused(t, emptySig, "an empty signature")
		})
	}
}
