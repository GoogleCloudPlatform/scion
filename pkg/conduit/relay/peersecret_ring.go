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
	"crypto/subtle"
	"errors"
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// peerSecretRingCheckPurpose hands each signing generation's raw secret to
// CheckPeerSecretNotInRing, which compares it and drops it at once.
var peerSecretRingCheckPurpose = keyring.NewPurpose("peer_secret_ring_check", func(s []byte) []byte { return slices.Clone(s) })

// The CheckPeerSecretNotInRing refusals. They name the setting, never a
// value or a key ID.
var (
	errPeerSecretInRing         = errors.New("server.hub.conduit.peer_secret must differ from the secret of every signing generation in the keyring file")
	errPeerSecretRingNoUsable   = errors.New("server.hub.conduit.peer_secret: the keyring file's signing ring has no usable generation to compare it against")
	errPeerSecretRingEmptyEntry = errors.New("server.hub.conduit.peer_secret: a signing generation in the keyring file has an empty secret")
)

// CheckPeerSecretNotInRing refuses a relay-peer secret equal to the raw
// secret of any generation of the hub's signing ring: the active one, a
// previous or staged one and the "legacy" entry, whatever their deadlines.
// Callers pass the normalized value (config.NormalizePeerSecret), which is
// what keys the relay-peer HMAC; a value with the trailing newline still
// attached is compared in both forms. Every comparison is constant time,
// against every generation, and the loop never stops early.
//
// It fails closed: a ring with no usable generation, or a generation that
// is present with an empty secret, is refused, so there is no case in
// which nothing is compared. A generation slot the file leaves unset (a
// ring of one or two generations) is not a generation and is not refused.
//
// A nil ring (legacy mode, or the relay role, which never loads the
// keyring) has nothing to compare; the caller keeps its own session secret
// check for that case.
func CheckPeerSecretNotInRing(peerSecret string, ring *keyring.SigningRing) error {
	if ring == nil {
		return nil
	}
	gens := ring.Generations(peerSecretRingCheckPurpose)
	secrets := make([][]byte, len(gens))
	for i, g := range gens {
		secrets[i] = g.Bytes()
	}
	defer func() {
		for _, s := range secrets {
			clear(s)
		}
	}()
	return checkPeerSecretAgainst(peerSecret, secrets)
}

// checkPeerSecretAgainst is CheckPeerSecretNotInRing over the generations'
// raw secrets (one per generation present in the ring).
func checkPeerSecretAgainst(peerSecret string, secrets [][]byte) error {
	if len(secrets) == 0 {
		return errPeerSecretRingNoUsable
	}
	forms := [][]byte{[]byte(peerSecret)}
	if n := config.NormalizePeerSecret(peerSecret); n != peerSecret {
		forms = append(forms, []byte(n))
	}
	match, empty := 0, false
	for _, s := range secrets {
		if len(s) == 0 {
			empty = true
			continue
		}
		for _, f := range forms {
			match |= subtle.ConstantTimeCompare(f, s)
		}
	}
	if empty {
		return errPeerSecretRingEmptyEntry
	}
	if match != 0 {
		return errPeerSecretInRing
	}
	return nil
}
