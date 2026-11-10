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
	"encoding/base64"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var hubSecretPendingRevealCmd = &cobra.Command{
	Use:   "reveal ID",
	Short: "Print the value of a secret waiting for your approval",
	Long: `Print the value of a pending secret, by the ID 'scion hub secret pending list'
shows, so you can check it before you approve it.

An agent chose the value, so on a terminal every byte the terminal could act on
(escape sequences, carriage returns and other control characters) is shown as
a visible escape such as \x1b, and a backslash as \\. Redirected to a file or
a pipe, or with --raw, the value is written exactly as stored. No trailing
newline is added. The Hub records every reveal in its audit log. Nothing else
changes: the secret stays pending.

Examples:
  scion hub secret pending reveal 0b6f...
  scion hub secret pending reveal 0b6f... > /tmp/check.json
  scion hub secret pending reveal 0b6f... --raw`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretPendingReveal,
}

var pendingSecretRevealRaw bool

func init() {
	hubSecretPendingCmd.AddCommand(hubSecretPendingRevealCmd)
	hubSecretPendingRevealCmd.Flags().BoolVar(&pendingSecretRevealRaw, "raw", false,
		"Write the value exactly as stored, even to a terminal")
}

func runSecretPendingReveal(_ *cobra.Command, args []string) error {
	id := args[0]
	if id == "" {
		return newUsageError("ID cannot be empty")
	}
	client, ctx, cancel, err := pendingSecretClient()
	if err != nil {
		return err
	}
	defer cancel()

	resp, err := client.Secrets().RevealPending(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to reveal pending secret: %w", err)
	}
	value, err := base64.StdEncoding.DecodeString(resp.Value)
	if err != nil {
		return fmt.Errorf("failed to decode the revealed value: %w", err)
	}
	if !pendingSecretRevealRaw && stdoutIsTerminal() {
		_, err = os.Stdout.WriteString(escapeForTerminal(string(value), true))
		return err
	}
	_, err = os.Stdout.Write(value)
	return err
}
