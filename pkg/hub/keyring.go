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
	"crypto/subtle"
	"errors"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// The /readyz reasons of a keyring file whose signing ring cannot issue.
// They name no key material, ID or deadline.
const (
	// readyReasonSigningIssuanceClosed: the active generation is at or
	// past its sign_until.
	readyReasonSigningIssuanceClosed = "signing_issuance_closed"
	// readyReasonSigningNoActiveGeneration: the ring has no usable active
	// generation (unreachable through the loader; fail closed anyway).
	readyReasonSigningNoActiveGeneration = "signing_no_active_generation"
)

// SigningKeySourceKeyring is the signing-key source label of the web
// session cookie keys when a keyring file (server.keyring_file) supplies
// them (ServerConfig.WebSessionKeySource). It is deployment-wide, so not
// hub-local (signingKeySourceIsHubLocal): the test admin tier refuses it.
const SigningKeySourceKeyring = "keyring"

// keyringReleaseCheck refuses keyring files this release does not support
// (keyring.Keyring.CheckSessionPilot). A variable so tests can build
// servers on versioned rings; production never changes it.
var keyringReleaseCheck = (*keyring.Keyring).CheckSessionPilot

// legacyReaderCheckPurpose derives nothing; checkLegacyEmitActiveReader
// uses it only to list the legacy readers' IDs.
var legacyReaderCheckPurpose = keyring.NewPurpose("legacy_reader_check", func([]byte) []byte { return nil })

// checkLegacyEmitActiveReader refuses a signing ring that would issue
// ID-less values its own readers cannot accept (ptone/scion#1530 Phase 4,
// design addendum §3): with emit "legacy" the active generation signs the
// ID-less skill URLs, artifact views, list cursors and attachment delivery
// capabilities, and ID-less values verify only against legacy_read_ids, so
// the active generation must be one of them. It is a membership check, independent of deadlines (the
// zero time is before every deadline). Session cookies are unaffected:
// they try every generation. The error names no key or ID.
func checkLegacyEmitActiveReader(r *keyring.SigningRing) error {
	if r.Emit() != keyring.EmitLegacy {
		return nil
	}
	for _, k := range r.Candidates(legacyReaderCheckPurpose, "", time.Time{}) {
		if k.ID == r.ActiveID() {
			return nil
		}
	}
	return errors.New(`keyring file: with signing.emit "legacy" the active signing key must be listed in signing.legacy_read_ids, or the ID-less values it issues could not be verified`)
}

// validateKeyringConfig checks a keyring-mode ServerConfig
// (ptone/scion#1530), in this order: the keyring passes this release's
// check (keyringReleaseCheck); with emit "legacy" its active signing
// generation is a legacy reader (checkLegacyEmitActiveReader);
// SharedSigningSecret equals its "legacy" signing entry, compared in
// constant time (every purpose not yet on the signing ring still uses
// it); and explicit agent/user JWT signing keys are refused, so no purpose
// is partly rotated. In legacy mode (no keyring file) explicit signing keys
// take precedence.
func validateKeyringConfig(cfg ServerConfig) error {
	k := cfg.Keyring
	if k == nil || k.Source() != keyring.SourceFile {
		return nil
	}
	if err := keyringReleaseCheck(k); err != nil {
		return err
	}
	if err := checkLegacyEmitActiveReader(k.Signing()); err != nil {
		return err
	}
	legacy, _ := k.Signing().LegacySecret()
	if subtle.ConstantTimeCompare([]byte(cfg.SharedSigningSecret), []byte(legacy)) != 1 {
		return errors.New("keyring mode: SharedSigningSecret must be the keyring's \"legacy\" signing entry")
	}
	if len(cfg.AgentTokenConfig.SigningKey) > 0 {
		return errors.New("keyring mode cannot be combined with an explicit agent token signing key (AgentTokenConfig.SigningKey)")
	}
	if len(cfg.UserTokenConfig.SigningKey) > 0 {
		return errors.New("keyring mode cannot be combined with an explicit user token signing key (UserTokenConfig.SigningKey)")
	}
	return nil
}

// keyringNow is the keyring deadline clock.
func (s *Server) keyringNow() time.Time {
	if s.config.KeyringNow != nil {
		return s.config.KeyringNow()
	}
	return time.Now()
}

// signingNotReadyReason returns the /readyz reason when a keyring file's
// signing ring cannot issue: signing_issuance_closed at or after the active
// generation's sign_until, signing_no_active_generation for a ring without
// a usable active generation (any other Active error is treated the same
// way). It returns "" when issuance works, and always without a keyring
// file or with the legacy adapter. The first not-ready result is logged once at WARN, without key
// material.
func (s *Server) signingNotReadyReason() string {
	k := s.config.Keyring
	if k == nil || k.Source() == keyring.SourceLegacy {
		// No keyring file (or the legacy adapter): nothing to check. Any
		// other keyring, including a zero one, must be able to issue.
		return ""
	}
	_, err := k.Signing().Active(webSessionHashPurpose, s.keyringNow())
	if err == nil {
		return ""
	}
	reason := readyReasonSigningNoActiveGeneration
	if errors.Is(err, keyring.ErrIssuanceClosed) {
		reason = readyReasonSigningIssuanceClosed
	}
	if s.signingNotReadyLogged.CompareAndSwap(false, true) {
		slog.Warn("Not ready: the keyring's signing ring cannot issue",
			"reason", reason, "active_id", k.Signing().ActiveID())
	}
	return reason
}

// ExperimentEnabled reports whether the named experiment is enabled now.
// It is for startup checks outside this package (the server.keyring_file
// gate in cmd); hub code uses experimentEnabled.
func (s *Server) ExperimentEnabled(name string) bool {
	return s.experimentEnabled(name)
}
