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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
)

// hubUserLevelCommands are direct "scion hub" subcommands that act on the
// user's hub connection or login rather than on a local project. Outside a
// project they read and write the global settings, as with --global.
var hubUserLevelCommands = map[string]bool{
	"status":  true,
	"enable":  true,
	"disable": true,
}

// brokerMachineLevelCommands are "scion runtime-broker" subcommands that act
// on this machine's broker process or its hub registration (join only sends
// a join token and adds the broker to no project). "provide" and
// "withdraw" are left out: they act on a project.
var brokerMachineLevelCommands = map[string]bool{
	"register":   true,
	"deregister": true,
	"start":      true,
	"stop":       true,
	"restart":    true,
	"status":     true,
	"hubs":       true,
	"join":       true,
}

// commandRequiresProject reports whether cmd needs an active scion project
// (or --global / --project). Commands that create a project, run a server,
// or act on the hub, a login, this machine's broker or the global settings
// do not.
func commandRequiresProject(cmd *cobra.Command) bool {
	cmdName := cmd.Name()
	parentName := ""
	if cmd.Parent() != nil {
		parentName = cmd.Parent().Name()
	}

	switch cmdName {
	case "help", "version", "completion", "doctor", "whoami", "global-flags":
		return false
	case "init":
		// Both top-level init and project init don't require existing project
		return false
	case "shadow", "unshadow":
		// hub shadow/unshadow create/remove the project marker — they must
		// run in a directory with no existing project.
		if parentName == "hub" {
			return false
		}
	case "migrate-names", "migrate":
		// hub secret migrate-names (GCP SM name migration) and hub secret
		// migrate (DB -> GCP SM value migration) operate directly against
		// the Hub DB and GCP Secret Manager; neither reads or resolves
		// the current directory's scion project (ptone/scion#2396).
		if parentName == "secret" && commandInSubtree(cmd, "hub") {
			return false
		}
	case "scion":
		// Root command itself doesn't require project
		return false
	}
	// Hub login and connection commands (ptone/scion#3317).
	if parentName == "auth" && commandInSubtree(cmd, "hub") {
		return false
	}
	if parentName == "hub" && isTopLevel(cmd.Parent()) && hubUserLevelCommands[cmdName] {
		return false
	}
	// Broker process and registration commands (ptone/scion#3317).
	if parentName == "runtime-broker" && brokerMachineLevelCommands[cmdName] {
		return false
	}
	// Server subcommands run the hub server and don't need a local project
	if commandInSubtree(cmd, "server") {
		return false
	}
	// Admin subcommands connect directly to the database and don't need a local project
	if commandInSubtree(cmd, "admin") {
		return false
	}
	// Project subcommands operate on all projects, not just the current one
	if parentName == "project" {
		return false
	}
	// design Amendment A26.2 O1: same reasoning as checkAgentContainerContext
	// — --handoff-template never touches the project or the Hub.
	if isReincarnateHandoffTemplateInvocation(cmd) {
		return false
	}
	return true
}

// isTopLevel reports whether c is a direct child of the root command.
// (Names are compared rather than package variables such as hubCmd, which
// would make rootCmd's initializer refer back to itself.)
func isTopLevel(c *cobra.Command) bool {
	return c != nil && c.Parent() != nil && c.Parent().Parent() == nil
}

// subcommandGlobalFlagSet reports whether cmd is a config subcommand whose
// own --global flag (which shadows the root --global) is set. Cobra parses
// every --global on the command line, before or after the subcommand name,
// into that local flag, so without this the root's global mode is never
// set for these commands.
func subcommandGlobalFlagSet(cmd *cobra.Command) bool {
	if !commandInSubtree(cmd, "config") {
		return false
	}
	f := cmd.Flags().Lookup("global")
	if f == nil || f == cmd.Root().PersistentFlags().Lookup("global") {
		return false
	}
	return f.Changed && f.Value.String() == "true"
}

// isHubDispatchInvocation reports whether agent work for this invocation is
// sent to a hub instead of run locally. It matches how hub dispatch is
// decided (hubsync.EnsureHubReady) and how the endpoint is resolved
// (GetHubEndpoint: --hub, then settings, then environment):
//
//   - inside a hub-connected container (config.IsHubContext);
//   - a hub project reference (slug, name or git URL) for --project, which
//     goes to the hub or fails with a hub-mode error;
//   - hub enabled in settings with an endpoint from any source.
//
// --no-hub turns off the last two.
func isHubDispatchInvocation(projectPath string) bool {
	if config.IsHubContext() {
		return true
	}
	if noHub {
		return false
	}
	if projectPath != "" && hubsync.IsHubProjectRef(projectPath) {
		return true
	}
	settings, err := config.LoadSettings(projectPath)
	if err != nil || settings == nil {
		return false
	}
	return settings.IsHubEnabled() && GetHubEndpoint(settings) != ""
}
