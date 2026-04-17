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
	"github.com/spf13/cobra"
)

// workflowCmd is the top-level command group for duckflux workflow management.
var workflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Manage duckflux workflow runs",
	Long: `Run and validate duckflux workflow files locally or via the Hub.

Local mode (default): delegates to the quack CLI subprocess. quack must be
present on PATH.

  npm install -g @duckflux/runner

Hub mode (--hub flag on run): dispatches the workflow to the configured Hub.
The source file is uploaded and executed remotely on a Runtime Broker.
Use --wait (default: auto) to stream logs and block until the run completes.

Remote management subcommands (require Hub):

  scion workflow list         List recent runs for the current grove.
  scion workflow get <id>     Show run details (status, timestamps, broker, trace URL).
  scion workflow logs <id>    Replay or stream run log events (-f to follow live).
  scion workflow cancel <id>  Request cancellation of an in-progress run.

All remote subcommands respect the global --hub, --grove, and --format flags.
Use --format json (or --json on list/get) for machine-readable output.`,
}

func init() {
	rootCmd.AddCommand(workflowCmd)
}
