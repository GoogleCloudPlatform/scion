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
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// hubUserLevelCommands are direct "scion hub" subcommands that act on the
// user's hub connection or login rather than on a local project. Outside a
// project they read and write the global settings, as with --global. The
// hub list commands (add, list, rename, remove) act on the user's hub list
// in ~/.scion/context.yaml.
var hubUserLevelCommands = map[string]bool{
	"status":  true,
	"enable":  true,
	"disable": true,
	"add":     true,
	"list":    true,
	"rename":  true,
	"remove":  true,
}

// hubOnlyCommands are top-level commands that need only the hub endpoint,
// the credential and an agent reference: they never resolve, create,
// register or sync a local project, even when --project names one. One
// command per line. (expose and unexpose act as the calling agent, with
// its own hub identity and no agent reference.)
var hubOnlyCommands = map[string]bool{
	"inbox":      true,
	"tunnel":     true,
	"ssh":        true,
	"ssh-config": true,
	"vscode":     true,
	"expose":     true,
	"unexpose":   true,
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
	"convert":    true,
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
	case "use":
		// Top-level 'use' (alias of 'project use') acts on user state.
		if isTopLevel(cmd) {
			return false
		}
	case "login":
		// Top-level 'login' (alias of 'hub auth login') acts on user credentials and hub list.
		if isTopLevel(cmd) {
			return false
		}
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
	if hubOnlyCommands[topLevelName(cmd)] {
		return false
	}
	// Project subcommands operate on all projects, not just the current one
	if parentName == "project" {
		return false
	}
	// 'templates update', 'templates delete', 'templates sync' and 'templates
	// push' (and their singular 'template' aliases) with --scope global or
	// --scope user act on global or user templates and read no project, so
	// they run outside a project (ptone/scion#1713, ptone/scion#8318,
	// ptone/scion#8643).
	if isProjectlessScopedTemplatesCommand(cmd) {
		return false
	}
	// 'harness-config sync' and 'harness-config push' with --scope global
	// upload to the global Hub scope and read no project, so they run outside
	// a project (ptone/scion#2903).
	if isProjectlessScopedHarnessConfigCommand(cmd) {
		return false
	}
	// 'decisions list --all-projects' queries GET /api/v1/users/me/decisions
	// across the user's Hub projects and needs no local project (ptone/scion#7995).
	if isAllProjectsDecisionsList(cmd) {
		return false
	}
	// 'notifications subscriptions' reads Hub subscriptions (optionally across
	// all projects or for a --project Hub reference) and needs no local project
	// (ptone/scion#8127).
	if isNotificationsSubscriptionsCommand(cmd) {
		return false
	}
	// design Amendment A26.2 O1: same reasoning as checkAgentContainerContext
	// — --handoff-template never touches the project or the Hub.
	if isReincarnateHandoffTemplateInvocation(cmd) {
		return false
	}
	return true
}

// isAllProjectsDecisionsList reports whether cmd is 'decisions list' with
// --all-projects set.
func isAllProjectsDecisionsList(cmd *cobra.Command) bool {
	if !isDecisionsListCommand(cmd) {
		return false
	}
	allProjects, _ := cmd.Flags().GetBool("all-projects")
	return allProjects
}

// isAllProjectsDecisionsListArgs reports whether target is 'decisions list'
// with --all-projects requested, before or after cobra parses flags (mirrors
// hubProfileRequested for Execute's early autoHelp settings load).
func isAllProjectsDecisionsListArgs(target *cobra.Command, args []string) bool {
	if !isDecisionsListCommand(target) {
		return false
	}
	if isAllProjectsDecisionsList(target) {
		return true
	}
	requested := false
	for _, a := range args {
		if a == "--" {
			break
		}
		switch {
		case a == "--all-projects":
			requested = true
		case strings.HasPrefix(a, "--all-projects="):
			if v, err := strconv.ParseBool(strings.TrimPrefix(a, "--all-projects=")); err == nil {
				requested = v
			}
		}
	}
	return requested
}

func isDecisionsListCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "list" && cmd.Parent() != nil && cmd.Parent().Name() == "decisions" && isTopLevel(cmd.Parent())
}

func isNotificationsSubscriptionsCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "subscriptions" && cmd.Parent() != nil && cmd.Parent().Name() == "notifications" && isTopLevel(cmd.Parent())
}

// isProjectlessScopedTemplatesCommand reports whether cmd is a scoped
// 'templates' (or singular 'template') subcommand ('update', 'delete', 'sync',
// or 'push') targeting 'global' or 'user' scope, neither of which reads a
// project.
func isProjectlessScopedTemplatesCommand(cmd *cobra.Command) bool {
	if cmd == nil || cmd.Parent() == nil || !isTopLevel(cmd.Parent()) {
		return false
	}
	parent := cmd.Parent().Name()
	if parent != "templates" && parent != "template" {
		return false
	}
	switch cmd.Name() {
	case "update", "delete", "sync", "push":
	default:
		return false
	}
	scope, _ := cmd.Flags().GetString("scope")
	return scope == "global" || scope == "user"
}

// isProjectlessScopedHarnessConfigCommand reports whether cmd is
// 'harness-config sync' or 'harness-config push' targeting 'global' scope,
// which reads no project.
func isProjectlessScopedHarnessConfigCommand(cmd *cobra.Command) bool {
	if cmd == nil || cmd.Parent() == nil || !isTopLevel(cmd.Parent()) {
		return false
	}
	if cmd.Parent().Name() != "harness-config" {
		return false
	}
	switch cmd.Name() {
	case "sync", "push":
	default:
		return false
	}
	scope, _ := cmd.Flags().GetString("scope")
	return scope == "global"
}

// isTopLevel reports whether c is a direct child of the root command.
// (Names are compared rather than package variables such as hubCmd, which
// would make rootCmd's initializer refer back to itself.)
func isTopLevel(c *cobra.Command) bool {
	return c != nil && c.Parent() != nil && c.Parent().Parent() == nil
}

// topLevelName returns the name of the top-level command cmd belongs to
// ("" for the root).
func topLevelName(cmd *cobra.Command) string {
	for c := cmd; c != nil; c = c.Parent() {
		if isTopLevel(c) {
			return c.Name()
		}
	}
	return ""
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
