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
	Long: `Run and validate duckflux workflow files.

In Phase 1 (current), all subcommands delegate directly to the local quack
CLI as a subprocess. No Hub or container involvement is required; quack must
be present on PATH.

Hub dispatch (--hub flag) and remote workflow management (list, get, logs,
cancel) will be available from Phase 3 onwards.

Install quack:
  npm install -g @duckflux/runner`,
}

func init() {
	rootCmd.AddCommand(workflowCmd)
}
