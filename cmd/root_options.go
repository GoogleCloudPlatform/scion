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
	"github.com/GoogleCloudPlatform/scion/cmd/internal/cliutil"
	"github.com/spf13/cobra"
)

// The root flags are moving from package globals to cliutil.RootOptions one
// command group at a time (ptone/scion#4285). Until the last group moves, the
// flags stay bound to the globals in root.go; root's PersistentPreRunE puts a
// snapshot of them on the command's context, and converted commands read it
// with rootOptions instead of touching the globals.

// snapshotRootOptions copies the current root flag globals into a
// RootOptions.
func snapshotRootOptions() cliutil.RootOptions {
	return cliutil.RootOptions{
		ProjectPath:    projectPath,
		GlobalMode:     globalMode,
		Profile:        profile,
		OutputFormat:   outputFormat,
		HubEndpoint:    hubEndpoint,
		NoHub:          noHub,
		AutoConfirm:    autoConfirm,
		NonInteractive: nonInteractive,
		Debug:          debugMode,
		DisplayTZ:      displayTZ,
		DisplayUTC:     displayUTC,
	}
}

// rootOptions returns the root flag values for cmd's invocation: the
// snapshot root's PersistentPreRunE stored on cmd's context, or, when there
// is none (root's hook did not run for cmd), a snapshot of the globals.
func rootOptions(cmd *cobra.Command) cliutil.RootOptions {
	if opts, ok := cliutil.RootOptionsFrom(cmd.Context()); ok {
		return opts
	}
	return snapshotRootOptions()
}
