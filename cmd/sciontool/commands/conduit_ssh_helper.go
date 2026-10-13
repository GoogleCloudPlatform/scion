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
	"os"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/sshd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// conduitSSHHelperCmd runs the embedded SSH server's helpers (the sftp
// server and the streamlocal connector) as the container user. sciontool
// starts it itself with the container user's credentials; it is not meant
// to be run by hand.
var conduitSSHHelperCmd = &cobra.Command{
	Use:                sshd.HelperCommand,
	Short:              "Internal helper for the conduit SSH server",
	Hidden:             true,
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		log.SetQuiet(true)
		os.Exit(sshd.RunHelper(args, os.Stdin, os.Stdout))
	},
}

func init() {
	rootCmd.AddCommand(conduitSSHHelperCmd)
}
