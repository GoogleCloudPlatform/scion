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

var machineInit bool
var machineInitForce bool

// initCmd represents the init command
var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new project",
	Long: `Initialize a new project by creating the .scion directory structure
and seeding the default template.

This is an alias for 'scion project init'.

By default, it initializes in:
- The root of the current git repo if run inside a repo
- The current directory

With --global or --machine, it performs full machine-level setup
(seeds harness-configs, templates, settings) in the user's home folder.`,
	RunE: projectInitCmd.RunE,
}

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().BoolVar(&globalInit, "global", false, "Initialize the global project in the home directory")
	initCmd.Flags().BoolVar(&machineInit, "machine", false, "Perform full machine-level setup (seeds harness-configs, templates, settings)")
	initCmd.Flags().BoolVar(&machineInitForce, "force", false, "Force overwrite existing templates and harness-configs with embedded defaults")
	initCmd.Flags().StringVar(&initImageRegistry, "image-registry", "", "Container image registry path (e.g., ghcr.io/myorg)")
}
