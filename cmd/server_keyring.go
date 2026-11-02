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

package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/spf13/pflag"
)

// serverKeyring is the keyring loaded from server.keyring_file by
// loadServerKeyring, or nil in legacy single-secret mode (no keyring file,
// or a role that does not use one). It holds key material in memory only.
var serverKeyring *keyring.Keyring

// serverValueCipher is the server's one shared encryption object
// (ptone/scion#1530 §5), built by buildServerValueCipher after
// loadServerKeyring. The local secret backend and the hub hold this same
// pointer, so every locally stored ciphertext uses one ring. Nil until
// built (roles without the hub never build it).
var serverValueCipher *secret.ValueCipher

// buildServerValueCipher builds serverValueCipher: from the keyring file
// when there is one (managed mode; an unusable encryption ring fails
// startup, never a fallback to legacy), else the legacy cipher over the
// resolved session secret (an empty secret disables encryption, as
// before). Errors carry no key material.
func buildServerValueCipher() error {
	serverValueCipher = nil
	if k := serverKeyringFromFile(); k != nil {
		c, err := secret.NewValueCipher(k)
		if err != nil {
			return fmt.Errorf("%s: encryption ring: %w", config.KeyringFileKey, err)
		}
		serverValueCipher = c
		return nil
	}
	serverValueCipher = secret.NewLegacyValueCipher(resolveSessionSecret())
	return nil
}

// loadServerKeyringFile is keyring.LoadFileWith; a var for tests.
var loadServerKeyringFile = keyring.LoadFileWith

// applyKeyringFlagOverrides applies the keyring flags over the loaded
// settings (which already merge settings.yaml and the environment), so the
// precedence is flag > env > settings.
func applyKeyringFlagOverrides(flags *pflag.FlagSet, cfg *config.GlobalConfig) {
	if f := flags.Lookup("keyring-file"); f != nil && f.Changed {
		cfg.KeyringFile = serverKeyringFile
	}
	if f := flags.Lookup("keyring-file-allow-world-readable"); f != nil && f.Changed {
		cfg.KeyringFileAllowWorldReadable = serverKeyringAllowWorldReadable
	}
}

// serverKeyringFromFile returns the keyring loaded from server.keyring_file,
// or nil without one.
func serverKeyringFromFile() *keyring.Keyring {
	if serverKeyring == nil || serverKeyring.Source() != keyring.SourceFile {
		return nil
	}
	return serverKeyring
}

// serverSigningRingFromFile returns the keyring file's signing ring, or nil
// (the web server then uses the legacy adapter over the session secret).
func serverSigningRingFromFile() *keyring.SigningRing {
	if k := serverKeyringFromFile(); k != nil {
		return k.Signing()
	}
	return nil
}

// keyringLegacySecret returns the keyring file's "legacy" signing entry,
// which every purpose not yet on the signing ring still uses in this
// release, or false without a keyring file.
func keyringLegacySecret() (string, bool) {
	if k := serverKeyringFromFile(); k != nil {
		return k.Signing().LegacySecret()
	}
	return "", false
}

// legacySessionSecretConfigured reports whether a legacy session secret
// input is present: --session-secret, or either env name (read by
// scrubServerSecretEnv before it unset them). It reads no keyring.
func legacySessionSecretConfigured() bool {
	return webSessionSecret != "" || capturedSessionSecret != ""
}

// loadServerKeyring resolves server.keyring_file (--keyring-file >
// SCION_SERVER_KEYRING_FILE > settings; already merged into
// cfg.KeyringFile) and, when set, reads and validates the keyring file
// once (ptone/scion#1530 §2). It runs in the server process after
// scrubServerSecretEnv and before any child starts or listener opens.
//
// Without a keyring file the legacy adapter applies, unchanged. With one:
// a legacy session secret input as well is an error (no precedence rule);
// so is an unreadable or invalid file (never a fallback to legacy), a
// keyring outside this release's session pilot limits, and a server that
// runs neither the hub nor the web server (the server.secret_rotation gate,
// checked by verifyKeyringExperiment once settings are loaded, needs the
// hub). Errors and notices carry no key material.
func loadServerKeyring(cfg *config.GlobalConfig) error {
	serverKeyring = nil
	path := cfg.KeyringFile
	if path != "" && !enableHub && !enableWeb {
		// A Runtime Broker-only server uses no server secret: like the
		// relay role, it never opens (or judges) the keyring setting.
		slog.Info("server.keyring_file is set but not used: this server runs neither the hub nor the web server")
		return nil
	}
	if path != strings.TrimSpace(path) {
		// Ambiguous configuration never silently selects legacy mode, nor
		// guesses which path was meant.
		return fmt.Errorf("%s has leading or trailing whitespace (or is only whitespace); set the exact path or unset it", config.KeyringFileKey)
	}
	if path == "" {
		if enableHub || enableWeb {
			slog.Info(keyring.StartupNotice(keyring.Legacy("")))
		}
		return nil
	}
	if !enableHub {
		return fmt.Errorf("%s is set but this server does not run the hub; a keyring file needs the hub (its %s experiment gate is read from hub settings)",
			config.KeyringFileKey, experiments.SecretRotation)
	}
	if legacySessionSecretConfigured() {
		return fmt.Errorf("%s cannot be combined with a legacy session secret (--session-secret, %s or %s); remove the legacy secret: the keyring's %q entry replaces it",
			config.KeyringFileKey, sessionSecretEnv, legacySessionSecretEnv, keyring.LegacyID)
	}
	k, err := loadServerKeyringFile(path, keyring.LoadOptions{AllowWorldReadable: cfg.KeyringFileAllowWorldReadable})
	if err != nil {
		return fmt.Errorf("%s: %w", config.KeyringFileKey, err)
	}
	if err := k.CheckSessionPilot(); err != nil {
		return fmt.Errorf("%s: %w", config.KeyringFileKey, err)
	}
	serverKeyring = k
	slog.Info(keyring.StartupNotice(k))
	return nil
}

// errKeyringExperimentOff is returned when a keyring file is configured
// while the server.secret_rotation experiment is off.
var errKeyringExperimentOff = errors.New("experiment is off")

// verifyKeyringExperiment refuses a keyring file while the
// server.secret_rotation experiment is off. It runs once operational
// settings are loaded and before any listener opens; the experiment is not
// re-checked later (restart-based loading).
func verifyKeyringExperiment(hubSrv *hub.Server) error {
	if serverKeyringFromFile() == nil {
		return nil
	}
	if !hubSrv.ExperimentEnabled(experiments.SecretRotation) {
		return fmt.Errorf("%s requires the %s experiment: %w", config.KeyringFileKey, experiments.SecretRotation, errKeyringExperimentOff)
	}
	return nil
}
