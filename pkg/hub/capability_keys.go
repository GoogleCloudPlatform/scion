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

package hub

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// Capability keys (ptone/scion#1530 Phase 4): the skill file download URLs,
// artifact view capabilities and authorized-list cursors are signed (or
// sealed) with one purpose of a signing ring.
//
// In managed ring mode (a keyring file) each purpose key is derived from
// every generation of the ring with the purpose's existing derivation
// (deriveSharedSigningKey and its key name), so the imported "legacy"
// generation derives exactly the bytes the shared secret did. The active
// generation issues; a value carrying a key ID is checked with that
// generation only (an unknown ID never falls back), and an ID-less value
// with the ring's legacy readers. Ring mode never resolves these keys from
// the secret backend or store: missing ring material refuses startup rather
// than reading a retained single-key record.
//
// In legacy mode (no keyring file) each key is resolved exactly as before
// (ensureSigningKey with its stable-key and ephemeral policy) and used as a
// one-key ring that issues and reads only the legacy (ID-less) forms.

// capabilityKeys binds a signing ring to one capability purpose. It holds
// no key bytes itself; they stay in the ring and are never printed.
type capabilityKeys struct {
	ring    *keyring.SigningRing
	purpose keyring.Purpose
	// now is the clock for the ring's sign_until and verify_until
	// deadlines (nil: time.Now). Capability expiry keeps the wall clock.
	now func() time.Time
}

// errCapabilityIssuanceClosed is returned when no key may sign now: the
// active generation reached its sign_until, or the ring has no active
// generation. It names no key or ID.
var errCapabilityIssuanceClosed = errors.New("no capability signing key may issue now (the active signing generation reached its sign_until)")

// configured reports whether the keys can sign or check anything at all.
func (c capabilityKeys) configured() bool {
	return len(c.ring.IDs()) > 0
}

func (c capabilityKeys) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// issue returns the key new values are signed with and whether they use
// the versioned (key-ID-carrying) form, which the ring's emit setting
// selects. Nothing is signed at or after the active generation's
// sign_until: issue then fails with errCapabilityIssuanceClosed.
func (c capabilityKeys) issue() (keyring.SigningKey, bool, error) {
	key, err := c.ring.Active(c.purpose, c.clock())
	if err != nil {
		return keyring.SigningKey{}, false, errCapabilityIssuanceClosed
	}
	return key, c.ring.Emit() == keyring.EmitVersioned, nil
}

// candidates returns the keys that may have signed a value carrying kid:
// that generation only for a non-empty kid (none when it is unknown or
// retired), the ring's legacy readers for an ID-less value ("").
func (c capabilityKeys) candidates(kid string) []keyring.SigningKey {
	return c.ring.Candidates(c.purpose, kid, c.clock())
}

// viewKeys adapts the keys for the artifact service.
func (c capabilityKeys) viewKeys() artifacts.ViewKeys {
	if !c.configured() {
		return artifacts.ViewKeys{}
	}
	return artifacts.ViewKeys{Ring: c.ring, Purpose: c.purpose, Now: c.now}
}

// fixedCapabilityKeyPurpose uses a generation's secret as the purpose key
// itself, for fixedCapabilityKeys.
var fixedCapabilityKeyPurpose = keyring.NewPurpose("fixed_capability_key", func(secret []byte) []byte {
	return slices.Clone(secret)
})

// fixedCapabilityKeys returns keys over one fixed key, issuing and reading
// only the legacy forms: a legacy-mode key resolved by ensureSigningKey, or
// a test key. An empty key returns unconfigured keys.
func fixedCapabilityKeys(key []byte) capabilityKeys {
	if len(key) == 0 {
		return capabilityKeys{}
	}
	return capabilityKeys{ring: keyring.Legacy(string(key)).Signing(), purpose: fixedCapabilityKeyPurpose}
}

// sharedSecretPurpose is the purpose of a hub signing key derived from the
// shared signing secret: deriveSharedSigningKey with keyName, unchanged.
func sharedSecretPurpose(keyName string) keyring.Purpose {
	return keyring.NewPurpose(keyName, func(secret []byte) []byte {
		return deriveSharedSigningKey(string(secret), keyName)
	})
}

// keyringManaged reports whether capability keys come from the keyring:
// any keyring other than none or the legacy adapter, including one that is
// not usable (which then refuses startup in managedCapabilityKeys rather
// than falling back to legacy key resolution).
func (s *Server) keyringManaged() bool {
	k := s.config.Keyring
	return k != nil && k.Source() != keyring.SourceLegacy
}

// managedCapabilityKeys returns the ring-mode keys of keyName. A ring
// without a usable active generation is a startup error: ring mode never
// reads a retained single-key record or switches to legacy mode. The error
// names no key material or ID.
func (s *Server) managedCapabilityKeys(keyName string) (capabilityKeys, error) {
	ring := s.config.Keyring.Signing()
	c := capabilityKeys{ring: ring, purpose: sharedSecretPurpose(keyName), now: s.keyringNow}
	if !c.configured() {
		return capabilityKeys{}, fmt.Errorf("%s: keyring mode: the signing ring has no usable active generation", keyName)
	}
	s.recordSigningKeySource(keyName, SigningKeySourceKeyring)
	return c, nil
}

// capabilityKeyIDPattern is the keyring's key ID syntax. A versioned value
// whose key ID does not match it is refused before any ring lookup.
var capabilityKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
