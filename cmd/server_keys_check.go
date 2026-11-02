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
	"fmt"

	"github.com/spf13/cobra"
)

var serverKeysCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate the server keyring configuration without starting services",
	Long: `Validate the server keyring configuration as a starting Hub would,
without starting services or opening the database: the keyring file path,
its permissions and schema, and this release's keyring limits.

Without a keyring file (legacy single-secret mode) there is nothing to
validate and the command succeeds.

Exit codes: 0 valid; 2 invalid.`,
	Args: cobra.NoArgs,
	RunE: runServerKeysCheck,
}

func init() {
	serverKeysCmd.AddCommand(serverKeysCheckCmd)
}

func runServerKeysCheck(cmd *cobra.Command, _ []string) error {
	s, err := loadKeysSettings(cmd)
	if err != nil {
		return err
	}
	k, err := loadKeysKeyring(s)
	if err != nil {
		return err
	}
	if _, err := keysValueCipher(k); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if k == nil {
		_, _ = fmt.Fprintln(out, "No keyring file configured: legacy single-secret mode; nothing to validate.")
		return nil
	}
	_, _ = fmt.Fprintf(out, "Keyring file is valid: revision %q, signing active %q, encryption active %q.\n",
		k.Revision(), k.Signing().ActiveID(), k.Encryption().ActiveID())
	return nil
}
