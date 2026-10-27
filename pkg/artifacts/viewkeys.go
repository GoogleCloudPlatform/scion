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

package artifacts

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// ViewKeys signs and checks view capabilities with one purpose of a signing
// ring (ptone/scion#1530 Phase 4). The ring's active generation signs; a
// capability carrying a key ID is checked with that generation only, and an
// ID-less (legacy) capability with the ring's legacy readers. The ring's
// emit setting picks the wire form new capabilities use. A zero ViewKeys
// (no ring) means views are not configured.
//
// ViewKeys holds no key bytes itself: they stay in the ring, and are never
// printed or logged.
type ViewKeys struct {
	// Ring holds the signing generations.
	Ring *keyring.SigningRing
	// Purpose derives the view key from a generation's secret.
	Purpose keyring.Purpose
	// Now is the clock for the ring's sign_until and verify_until
	// deadlines; nil means time.Now. Capability expiry always uses the
	// wall clock.
	Now func() time.Time
}

// fixedViewKeyPurpose uses a generation's secret as the view key itself,
// for LegacyViewKeys.
var fixedViewKeyPurpose = keyring.NewPurpose("artifact_view_fixed_key", func(secret []byte) []byte {
	return slices.Clone(secret)
})

// LegacyViewKeys returns ViewKeys over one fixed key: the key signs every
// capability in the legacy (ID-less) form and is the only legacy reader,
// exactly as a single view key did before signing rings. An empty key
// returns the zero ViewKeys (views not configured).
func LegacyViewKeys(key []byte) ViewKeys {
	if len(key) == 0 {
		return ViewKeys{}
	}
	return ViewKeys{Ring: keyring.Legacy(string(key)).Signing(), Purpose: fixedViewKeyPurpose}
}

// errViewIssuanceClosed is returned when the ring cannot sign now (its
// active generation reached sign_until, or it has no active generation).
var errViewIssuanceClosed = errors.New("artifact views: no signing key may issue now")

// configured reports whether the keys can sign or check anything at all.
func (k ViewKeys) configured() bool {
	return len(k.Ring.IDs()) > 0
}

func (k ViewKeys) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// issue returns the key new capabilities are signed with and whether they
// use the versioned form. It fails once the active generation may no longer
// sign: nothing is signed after sign_until.
func (k ViewKeys) issue() (keyring.SigningKey, bool, error) {
	key, err := k.Ring.Active(k.Purpose, k.now())
	if err != nil {
		return keyring.SigningKey{}, false, errViewIssuanceClosed
	}
	return key, k.Ring.Emit() == keyring.EmitVersioned, nil
}

// candidates returns the keys that may have signed a capability carrying
// kid ("" for the legacy form): that generation only for a kid (none when
// unknown, never another key), the legacy readers otherwise, each only
// before its verify_until.
func (k ViewKeys) candidates(kid string) []keyring.SigningKey {
	return k.Ring.Candidates(k.Purpose, kid, k.now())
}

// viewKeyIDPattern is the keyring's key ID syntax. A versioned capability
// whose key ID does not match it is refused before any ring lookup.
var viewKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// warnViewIssuanceClosed logs, once per service, that a view capability
// was refused because no key may sign now: the instance is already not
// ready, and every refused mint would otherwise log again. It names no key,
// ID or capability.
func (s *Service) warnViewIssuanceClosed(r *http.Request) {
	if s.viewIssuanceClosedWarned.CompareAndSwap(false, true) {
		slog.WarnContext(r.Context(), "artifacts: view capability not issued: the active signing generation may no longer sign (sign_until reached)")
	}
}
