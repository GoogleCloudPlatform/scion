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

// validateKeyringConfig checks a keyring-mode ServerConfig
// (ptone/scion#1530 Phase 1): the keyring must satisfy this release's
// session pilot limits, SharedSigningSecret must be its "legacy" signing
// entry (every purpose but the web session cookies still uses it), and
// explicit agent/user JWT signing keys are refused, so no purpose is
// partly rotated. Legacy mode (no keyring file) is unchanged: explicit
// signing keys keep their precedence.
func validateKeyringConfig(cfg ServerConfig) error {
	k := cfg.Keyring
	if k == nil || k.Source() != keyring.SourceFile {
		return nil
	}
	if err := k.CheckSessionPilot(); err != nil {
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
