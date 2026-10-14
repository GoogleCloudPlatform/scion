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

package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// movedCommand returns a hidden stub for a command that moved to the scion
// CLI. It accepts any arguments and flags, and fails with a message naming
// the new command (sciontool exits 1 on the returned error).
func movedCommand(name string) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Short:              fmt.Sprintf("Moved to 'scion %s'", name),
		Hidden:             true,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("sciontool %s has moved to 'scion %s'", name, name)
		},
	}
}

func init() {
	rootCmd.AddCommand(movedCommand("expose"))
	rootCmd.AddCommand(movedCommand("unexpose"))
}
