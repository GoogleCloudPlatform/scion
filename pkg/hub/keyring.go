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
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
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

// managedSigning reports whether cfg is in managed ring mode: a keyring
// file is configured, or any keyring other than the legacy adapter is set
// (the same rule /readyz applies). In managed mode the signing ring is
// authoritative for the agent and user JWT keys: missing or incomplete
// ring material refuses startup and never selects the legacy signing-key
// resolution (design §4).
func managedSigning(cfg ServerConfig) bool {
	return cfg.KeyringConfigured || (cfg.Keyring != nil && cfg.Keyring.Source() != keyring.SourceLegacy)
}

// validateKeyringConfig checks a managed-mode ServerConfig
// (ptone/scion#1530) before New reads any signing key, in this order: a
// configured keyring file must have produced a keyring; its signing ring
// must have an active generation; the keyring must come from the file (a
// missing or unreadable file, or a legacy adapter in its place, never
// selects legacy mode); the ring holds the "legacy" entry every purpose not
// yet on the signing ring still needs; the keyring passes this release's
// check (keyringReleaseCheck); with emit "legacy" its active signing
// generation is a legacy reader (checkLegacyEmitActiveReader);
// SharedSigningSecret equals its "legacy" signing entry, compared in
// constant time; and explicit agent/user JWT signing keys are refused, so
// no purpose is partly rotated. In legacy mode (no keyring file) explicit
// signing keys take precedence. Errors name no key material, key ID or
// value.
func validateKeyringConfig(cfg ServerConfig) error {
	if !managedSigning(cfg) {
		return nil
	}
	k := cfg.Keyring
	if k == nil {
		return errors.New("keyring mode: server.keyring_file is configured but no keyring file was loaded; refusing to fall back to legacy signing keys")
	}
	// The zero time never reaches a sign_until, so only a ring without a
	// usable active generation errors here.
	if _, err := k.Signing().Active(agentJWTPurpose, time.Time{}); err != nil {
		return errors.New("keyring mode: the signing ring has no active generation")
	}
	if k.Source() != keyring.SourceFile {
		return errors.New("keyring mode: server.keyring_file is configured but the keyring did not come from it; refusing to fall back to legacy signing keys")
	}
	// The purposes that do not use the ring yet still need the "legacy"
	// entry. Checked here, not only by keyringReleaseCheck, so the test
	// seam can never admit a ring without it.
	legacy, ok := k.Signing().LegacySecret()
	if !ok {
		return errors.New("keyring mode: the signing ring has no \"legacy\" entry; every purpose that does not use the ring yet still needs it")
	}
	if err := keyringReleaseCheck(k); err != nil {
		return err
	}
	if err := checkLegacyEmitActiveReader(k.Signing()); err != nil {
		return err
	}
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

// serverValueCipher returns the shared ValueCipher for cfg
// (ptone/scion#1530 §5): cfg.ValueCipher when set (it must wrap the
// keyring file's encryption ring, if there is one), else one built from
// the keyring file, else the legacy cipher over SharedSigningSecret.
func serverValueCipher(cfg ServerConfig) (*secret.ValueCipher, error) {
	file := cfg.Keyring != nil && cfg.Keyring.Source() == keyring.SourceFile
	if cfg.ValueCipher != nil {
		if file && !cfg.ValueCipher.UsesRing(cfg.Keyring.Encryption()) {
			return nil, errors.New("keyring mode: ValueCipher must wrap the keyring file's encryption ring")
		}
		if lb, ok := cfg.SecretBackend.(*secret.LocalBackend); ok && lb.Cipher() != cfg.ValueCipher {
			return nil, errors.New("the local secret backend must share the server's ValueCipher")
		}
		return cfg.ValueCipher, nil
	}
	if file {
		return secret.NewValueCipher(cfg.Keyring)
	}
	return secret.NewLegacyValueCipher(cfg.SharedSigningSecret), nil
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
