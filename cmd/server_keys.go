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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The `scion server keys` group (ptone/scion#1530 Phase 6): operator
// commands for the Hub server keyring. They run on the server host, read
// the server configuration and keyring file directly, and never start
// services. Human mode only (not in agentAllowed: the whole server tree is
// hidden in agent mode) and not on Windows (not in windowsAllowed).
//
// Exit codes, shared by every keys subcommand (EM ruling on T0 review item 7):
//
//	0  the command did its job; for status, a report was produced, NOT
//	   READY included (readiness is report content, not a failure)
//	1  the operation failed (database open or read, decrypt or audit error)
//	2  invalid configuration or an unmet precondition; nothing was done
const (
	keysExitFailure = 1
	keysExitInvalid = 2
)

// keysExitError carries a keys subcommand's exit status (exitCoder).
type keysExitError struct {
	code int
	err  error
}

func (e *keysExitError) Error() string { return e.err.Error() }

func (e *keysExitError) Unwrap() error { return e.err }

// ExitCode implements exitCoder.
func (e *keysExitError) ExitCode() int { return e.code }

// keysInvalid wraps err as an exit-2 (invalid configuration) failure.
func keysInvalid(err error) error { return &keysExitError{code: keysExitInvalid, err: err} }

// keysFailed wraps err as an exit-1 failure.
func keysFailed(err error) error { return &keysExitError{code: keysExitFailure, err: err} }

var (
	keysConfigPath         string
	keysKeyringFile        string
	keysAllowWorldReadable bool
)

var serverKeysCmd = &cobra.Command{
	Use:   "keys",
	Short: "Inspect the Hub server keyring",
	Long: `Operator commands for the Hub server keyring (server.keyring_file).

They read the server configuration and keyring file on this host and never
start services or print key material. They are available in human mode
only. The read-only commands (status, check) do not require the
server.secret_rotation experiment: it is a hub database setting, and they
change nothing.

Exit codes: 0 the command did its job (status: a report was produced,
including a NOT READY one; check: the configuration is valid); 1 the
operation failed (for example the database could not be opened or read);
2 invalid configuration or an unmet precondition.`,
}

func init() {
	serverCmd.AddCommand(serverKeysCmd)
	addKeysPersistentFlags(serverKeysCmd.PersistentFlags())
}

// addKeysPersistentFlags registers the flags every keys subcommand reads.
func addKeysPersistentFlags(pf *pflag.FlagSet) {
	pf.StringVar(&keysConfigPath, "config", "", "Path to the server configuration file")
	pf.StringVar(&keysKeyringFile, "keyring-file", "", "Keyring file path (overrides server.keyring_file)")
	pf.BoolVar(&keysAllowWorldReadable, "keyring-file-allow-world-readable", false,
		"Admit a world-readable keyring file (overrides server.keyring_file_allow_world_readable)")
}

// keysSettings is the resolved configuration a keys subcommand uses.
type keysSettings struct {
	cfg  *config.GlobalConfig
	path string // "" in legacy mode
}

// loadKeysSettings loads the server configuration and resolves the
// keyring file path: --keyring-file, else server.keyring_file (settings
// merged with SCION_SERVER_KEYRING_FILE).
func loadKeysSettings(cmd *cobra.Command) (keysSettings, error) {
	cfg, err := config.LoadGlobalConfig(keysConfigPath)
	if err != nil {
		return keysSettings{}, keysInvalid(fmt.Errorf("loading server config: %w", err))
	}
	flags := cmd.Flags()
	if f := flags.Lookup("keyring-file"); f != nil && f.Changed {
		cfg.KeyringFile = keysKeyringFile
	}
	if f := flags.Lookup("keyring-file-allow-world-readable"); f != nil && f.Changed {
		cfg.KeyringFileAllowWorldReadable = keysAllowWorldReadable
	}
	return keysSettings{cfg: cfg, path: cfg.KeyringFile}, nil
}

// loadKeysKeyring loads and validates the configured keyring file with
// the same checks a starting Hub applies (loadServerKeyring and
// validateKeyringConfig): an unambiguous path, no legacy session secret
// alongside it, the loader's validation, and hub.CheckKeyringFile. It
// returns nil in legacy mode (no keyring file). Every error is exit 2 and
// carries no key material.
func loadKeysKeyring(s keysSettings) (*keyring.Keyring, error) {
	path := s.path
	if path != strings.TrimSpace(path) {
		return nil, keysInvalid(fmt.Errorf("%s has leading or trailing whitespace (or is only whitespace); set the exact path or unset it", config.KeyringFileKey))
	}
	if path == "" {
		return nil, nil
	}
	if sessionSecretFromEnv() != "" {
		return nil, keysInvalid(fmt.Errorf("%s cannot be combined with a legacy session secret (%s or %s); remove the legacy secret: the keyring's %q entry replaces it",
			config.KeyringFileKey, sessionSecretEnv, legacySessionSecretEnv, keyring.LegacyID))
	}
	k, err := keyring.LoadFileWith(path, keyring.LoadOptions{AllowWorldReadable: s.cfg.KeyringFileAllowWorldReadable})
	if err != nil {
		return nil, keysInvalid(fmt.Errorf("%s: %w", config.KeyringFileKey, err))
	}
	if err := hub.CheckKeyringFile(k); err != nil {
		return nil, keysInvalid(fmt.Errorf("%s: %w", config.KeyringFileKey, err))
	}
	return k, nil
}

// keysValueCipher builds the cipher a Hub with this configuration uses
// (buildServerValueCipher): from the keyring file, else the legacy cipher
// over the session secret in the environment.
func keysValueCipher(k *keyring.Keyring) (*secret.ValueCipher, error) {
	if k != nil {
		c, err := secret.NewValueCipher(k)
		if err != nil {
			return nil, keysInvalid(fmt.Errorf("%s: encryption ring: %w", config.KeyringFileKey, err))
		}
		return c, nil
	}
	return secret.NewLegacyValueCipher(sessionSecretFromEnv()), nil
}

// errKeysNoDatabase is returned when status needs a database and none is
// configured.
var errKeysNoDatabase = errors.New("no database configured: set --db, SCION_SERVER_DATABASE_URL, or database.url in the server config (or pass --no-store)")
