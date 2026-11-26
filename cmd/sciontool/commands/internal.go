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

import "github.com/spf13/cobra"

// internalCmd groups hidden internal plumbing subcommands invoked by harness
// bundles (e.g. normalize.py) and offline tooling.
var internalCmd = &cobra.Command{
	Use:    "internal",
	Short:  "Internal plumbing subcommands",
	Hidden: true,
}

func init() {
	rootCmd.AddCommand(internalCmd)
}
