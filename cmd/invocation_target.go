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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
)

const (
	// targetSourceContext marks a hub endpoint or project that
	// resolveInvocationTarget took from the current context ('scion use').
	targetSourceContext = "context"
	// targetSourceDirectory marks a hub endpoint that
	// resolveInvocationTarget took from the link entry of the directory
	// project ('scion hub link').
	targetSourceDirectory = "directory"
)

var (
	// hubSource records where resolveInvocationTarget took hubEndpoint
	// from: targetSourceContext or targetSourceDirectory. It is "" when
	// hubEndpoint is the --hub flag (or empty).
	hubSource string
	// projectSource is targetSourceContext when resolveInvocationTarget set
	// projectPath from the current context, and "" otherwise.
	projectSource string
	// contextProjectSlug is the slug of the hub project
	// resolveInvocationTarget injected as projectPath, as recorded in the
	// context, or "". It is for messages only.
	contextProjectSlug string
	// hubFlagName is the hub nickname given with --hub, which
	// resolveInvocationTarget replaced by its endpoint in hubEndpoint, or "".
	hubFlagName string
	// contextWarned records that resolveInvocationTarget already warned
	// about an unreadable context.yaml in this invocation.
	contextWarned bool
)

// contextDisabled reports whether the current context must be ignored:
// in agent mode, inside an agent container (SCION_HOST_UID), and in a
// hub-managed agent. Agents take their hub and project from their own
// environment and settings, never from a user's context.yaml.
func contextDisabled(mode CLIMode) bool {
	return mode == ModeAgent || os.Getenv("SCION_HOST_UID") != "" || config.IsHubManagedAgent()
}

// envHubEndpoint returns the hub endpoint set in the environment and the
// name of the variable it came from.
func envHubEndpoint() (endpoint, varName string) {
	for _, name := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		if v := os.Getenv(name); v != "" {
			return v, name
		}
	}
	return "", ""
}

// filesystemProject returns the filesystem project of this invocation: the
// one named with --project <path> or --global, or else the one found by
// walking up from the working directory. The implicit fallback to the
// global directory does not count, and neither does a --project value that
// is a hub project reference.
func filesystemProject() (dir string, ok bool) {
	if globalMode || projectPath == "global" || projectPath == "home" {
		g, err := config.GetGlobalDir()
		return g, err == nil
	}
	if projectPath != "" {
		if hubsync.IsHubProjectRef(projectPath) {
			// A hub reference names no directory; its hub settings come
			// from the walked project, as in resolveHubProjectRef.
			return walkedProject()
		}
		p, _, err := config.ResolveProjectPath(projectPath)
		return p, err == nil
	}
	return walkedProject()
}

// walkedProject returns the project found by walking up from the working
// directory, unless that is the global directory. The synthetic path
// FindProjectRoot returns when SCION_HUB_ENDPOINT or SCION_HUB_URL is set
// and no .scion exists is not a project either.
func walkedProject() (string, bool) {
	p, ok := config.FindProjectRoot()
	if !ok {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	if g, err := config.GetGlobalDir(); err == nil && p == g {
		return "", false
	}
	return p, true
}

// cliHub is the hub endpoint of this invocation, where it came from, and
// the display label of that source. resolveCLIHub is the only place that
// pairs an endpoint with its source, so hub status, bare 'scion use' and
// every command that looks up "the hub" agree on both.
type cliHub struct {
	// Endpoint is the resolved hub URL, or "".
	Endpoint string
	// Source is the JSON source code shared with 'scion use': "flag",
	// "env", "directory", "context" or "none".
	Source string
	// Label is the display label of Source: "--hub flag", the name of the
	// environment variable, "directory link", "current context" or "none".
	Label string
}

// resolveCLIHub returns the hub of this invocation: hubEndpoint, which
// resolveInvocationTarget filled from the --hub flag, the link entry of the
// directory project or the current context (source "flag", "directory" or
// "context"), else SCION_HUB_ENDPOINT or SCION_HUB_URL ("env"), else ""
// ("none"). No settings key is read: the hub list is the CLI's only hub
// configuration.
func resolveCLIHub() cliHub {
	switch {
	case hubEndpoint != "" && hubSource == targetSourceContext:
		return cliHub{Endpoint: hubEndpoint, Source: useSourceContext, Label: "current context"}
	case hubEndpoint != "" && hubSource == targetSourceDirectory:
		return cliHub{Endpoint: hubEndpoint, Source: useSourceDirectory, Label: "directory link"}
	case hubEndpoint != "":
		return cliHub{Endpoint: hubEndpoint, Source: useSourceFlag, Label: "--hub flag"}
	}
	if ep, name := envHubEndpoint(); ep != "" {
		return cliHub{Endpoint: ep, Source: useSourceEnv, Label: name}
	}
	return cliHub{Source: useSourceNone, Label: "none"}
}

// invocationHub returns the hub endpoint of this invocation and its source
// code (see resolveCLIHub). Commands look up "the hub" through this
// function or invocationHubEndpoint; the resolver itself and
// hubFlagEndpoint read the package variables directly.
func invocationHub() (endpoint, source string) {
	h := resolveCLIHub()
	return h.Endpoint, h.Source
}

// invocationHubEndpoint returns the hub endpoint of this invocation (see
// invocationHub), or "" when no hub is selected.
func invocationHubEndpoint() string {
	ep, _ := invocationHub()
	return ep
}

// hubConfigured reports whether a hub is selected for this invocation (see
// invocationHub). Local-only mode was removed, so a selected hub is what
// makes the CLI use one. Every cmd "is a hub configured?" check goes
// through this one function.
func hubConfigured() bool {
	return invocationHubEndpoint() != ""
}

// contextSkippedSubtrees are top-level commands for which the current
// context is never applied. The server takes its own endpoints from its
// server settings (the endpoint it advertises to its agents), so 'scion
// use' must never retarget it. A --hub nickname is still resolved for it,
// as for every command: it is an explicit choice, not the context. The
// runtime-broker commands are not here: register, join and the other
// broker administration commands talk to the hub the CLI selected; the
// command that runs the broker process is in contextSkippedCommands.
var contextSkippedSubtrees = map[string]bool{
	"server": true,
}

// contextSkippedCommands are the commands, by command path, that start a
// server or broker process in this process: the process never sees the
// current hub, and no one-time import writes context.yaml. Only a --hub
// nickname is looked up in the hub list, read as it is. 'runtime-broker
// start --foreground' runs 'server start' in-process. (Every 'server'
// command is skipped by contextSkippedSubtrees.)
var contextSkippedCommands = map[string]bool{
	"scion runtime-broker start": true,
}

// contextReadOnlyCommands are the commands, by command path, that only read
// settings. The one-time imports of hub settings apply to them in memory
// only, so a read never creates or rewrites context.yaml or its lock
// (ptone/scion#5483), and they still see the hub the import gives.
var contextReadOnlyCommands = map[string]bool{
	"scion config list":       true,
	"scion config get":        true,
	"scion config validate":   true,
	"scion config schema":     true,
	"scion config dir":        true,
	"scion config cd-config":  true,
	"scion config cd-project": true,
}

// contextProjectCommands are hub-only commands (see hubOnlyCommands), by
// command path, that take the context's project like the project-scoped
// commands do, although they need no local project. expose and unexpose
// act as the calling agent and stay out.
var contextProjectCommands = map[string]bool{
	"scion tunnel":     true,
	"scion ssh":        true,
	"scion ssh-config": true,
	"scion vscode":     true,
}

// importGlobalHubSettings turns on the one-time imports of hub settings
// into the hub list: the global settings' hub.endpoint when the context is
// read, and a linked project's hub.endpoint when its directory is resolved
// (see targetResolution). It is always on in the CLI; only the cmd tests'
// TestMain turns it off (see there).
var importGlobalHubSettings = true

// targetResolution says how resolveInvocationTarget reads the hub list:
// where its warnings go, and whether it runs the one-time imports of hub
// settings (importGlobalHubSettings). Shell completion reads the hub list
// without importing and without printing.
type targetResolution struct {
	stderr  io.Writer
	imports bool
	// noSave applies the one-time imports in memory only, for the
	// read-only commands (contextReadOnlyCommands): they see the imported
	// hub, and nothing is written.
	noSave bool
}

// invocationResolution is the resolution of a command invocation: warnings
// on stderr, imports on (unless a test turned them off).
func invocationResolution(stderr io.Writer) targetResolution {
	return targetResolution{stderr: stderr, imports: importGlobalHubSettings}
}

// loadInvocationContext reads context.yaml for this invocation, after the
// one-time import of the global settings' hub configuration. A failure to
// save the import is a warning on stderr: the returned context holds it
// anyway.
func loadInvocationContext(stderr io.Writer) (*config.CLIContext, error) {
	return invocationResolution(stderr).loadContext()
}

// loadContext reads context.yaml, after the one-time import of the global
// settings' hub configuration when r imports. A failure to save the import
// is a warning: the returned context holds it anyway.
func (r targetResolution) loadContext() (*config.CLIContext, error) {
	if !r.imports {
		return config.LoadCLIContext()
	}
	if r.noSave {
		return config.LoadCLIContextImportedInMemory(useHubBaseName)
	}
	c, err := config.LoadCLIContextImported(useHubBaseName)
	if errors.Is(err, config.ErrCLIContextImport) {
		_, _ = fmt.Fprintf(r.stderr, "Warning: %v\n", err)
		return c, nil
	}
	return c, err
}

// linkEntry returns the link entry of the filesystem project dir and its
// hub entry, or nil and nil. When r imports, a linked project whose hub is
// not in the hub list yet is imported once (see
// config.LinkEntryForProject); a failure to save that import is a warning,
// and the imported link is used anyway.
func (r targetResolution) linkEntry(c *config.CLIContext, dir string) (*config.KnownProject, *config.HubEntry) {
	if !r.imports {
		return c.LinkEntryFor(config.LinkPathForProject(dir))
	}
	if r.noSave {
		return config.LinkEntryForProjectInMemory(c, dir, useHubBaseName)
	}
	p, h, err := config.LinkEntryForProject(c, dir, useHubBaseName)
	if err != nil {
		_, _ = fmt.Fprintf(r.stderr, "Warning: %v\n", err)
	}
	return p, h
}

// isHubNickname reports whether a --hub value names a hub entry rather
// than a URL. A URL always contains "://"; a nickname never contains ':'
// or '/'. Other values are left as they are.
func isHubNickname(v string) bool {
	return !strings.Contains(v, "://") && config.ValidHubName(v)
}

// unknownHubError is the error for a --hub nickname that has no entry.
func unknownHubError(name string, c *config.CLIContext) error {
	names := c.HubNames()
	if len(names) == 0 {
		return fmt.Errorf("unknown hub '%s': no hubs are registered; add one with 'scion hub add <name> <url>', or pass the hub URL", name)
	}
	return fmt.Errorf("unknown hub '%s'; registered hubs: %s", name, strings.Join(names, ", "))
}

// resolveHubNickname replaces a --hub nickname in hubEndpoint with that
// entry's endpoint, and records the nickname in hubFlagName. A URL is left
// as it is: one that is not registered is used for this invocation only.
func resolveHubNickname(c *config.CLIContext) error {
	if !isHubNickname(hubEndpoint) {
		return nil
	}
	h := c.Hubs[hubEndpoint]
	if h == nil {
		return unknownHubError(hubEndpoint, c)
	}
	if h.Endpoint == "" {
		return hubWithoutEndpointError(hubEndpoint)
	}
	hubFlagName = hubEndpoint
	hubEndpoint = h.Endpoint
	return nil
}

// hubWithoutEndpointError is the error for a nickname whose hub entry has
// no endpoint (a hand-edited context.yaml). Using it would silently drop
// the --hub choice.
func hubWithoutEndpointError(name string) error {
	return fmt.Errorf("hub '%s' has no endpoint; remove it with 'scion hub remove %s' and add it again with 'scion hub add %s <url>'", name, name, name)
}

// resolveInvocationTarget fills the hub endpoint and the project of this
// invocation from the hub list and the current context ('scion use') where
// nothing more specific chose them. It only fills empty values and never
// overrides a flag. The hub precedence is: --hub, then
// SCION_HUB_ENDPOINT/SCION_HUB_URL, then the link entry of the directory
// project, then the current hub. No settings key is read for the hub
// (invocationHub applies the environment tier).
//
//   - --hub: a hub nickname is replaced by its entry's endpoint; an unknown
//     nickname is an error that lists the registered names. When the
//     directory project is linked to another hub, a warning says so.
//   - Hub: set from the filesystem project's link entry ('scion hub link',
//     or the one-time import of a linked project's settings) when neither
//     --hub nor SCION_HUB_ENDPOINT/SCION_HUB_URL is set; otherwise from the
//     current hub.
//   - Project: set to the hub UUID of the project remembered for the
//     effective hub, when no --project, no --global and no filesystem
//     project apply and the command needs a project. A UUID is used rather
//     than the slug because it can never be mistaken for a local directory.
//     tunnel, ssh and ssh-config need no project but take this one too
//     (contextProjectCommands).
//
// It records hubSource and projectSource. A context file that cannot be
// read is a warning, and the invocation proceeds without it, unless --hub
// is a nickname, which then cannot be resolved.
//
// For the server commands and 'runtime-broker start' (contextSkippedSubtrees
// and contextSkippedCommands) only a --hub nickname is resolved, from the
// hub list as it is, without the one-time imports; nothing else is filled,
// and without a nickname context.yaml is not read.
func resolveInvocationTarget(cmd *cobra.Command, mode CLIMode) error {
	return resolveInvocationTargetWith(cmd, mode, invocationResolution(cmd.ErrOrStderr()))
}

// resolveCompletionTarget is resolveInvocationTarget for shell completion,
// which skips PersistentPreRunE: it reads the hub list without the
// one-time imports, so completion never writes context.yaml, and it prints
// nothing.
func resolveCompletionTarget(cmd *cobra.Command) error {
	return resolveInvocationTargetWith(cmd, resolveMode(), targetResolution{stderr: io.Discard})
}

// resolveInvocationTargetWith is resolveInvocationTarget, reading the hub
// list as r says.
func resolveInvocationTargetWith(cmd *cobra.Command, mode CLIMode, r targetResolution) error {
	stderr := r.stderr
	hubSource = ""
	projectSource = ""
	contextProjectSlug = ""
	contextWarned = false
	hubFlagName = ""
	if contextDisabled(mode) {
		return nil
	}
	nicknameOnly := contextSkippedSubtrees[topLevelName(cmd)] || contextSkippedCommands[cmd.CommandPath()]
	if nicknameOnly && !isHubNickname(hubEndpoint) {
		return nil
	}
	if nicknameOnly {
		// Read the hub list as it is: no one-time import, so nothing is
		// written, and stderr gets no import warning.
		r = targetResolution{stderr: stderr}
	}
	if contextReadOnlyCommands[cmd.CommandPath()] {
		// The one-time imports apply in memory only: the command sees the
		// hub the next saving command will import, and nothing is written.
		r.noSave = true
	}
	ctx, err := r.loadContext()
	if err != nil {
		if isHubNickname(hubEndpoint) {
			return fmt.Errorf("cannot look up hub '%s': %w", config.RedactURL(hubEndpoint), err)
		}
		_, _ = fmt.Fprintf(stderr, "Warning: ignoring the current context: %v\n", err)
		contextWarned = true
		return nil
	}
	if err := resolveHubNickname(ctx); err != nil {
		return err
	}
	if nicknameOnly {
		return nil
	}
	// config subcommands act on local settings files, never on a hub
	// project, so they never get an injected project (they still see the
	// context hub). commandRequiresProject alone does not exempt them.
	wantsProject := commandRequiresProject(cmd) || contextProjectCommands[cmd.CommandPath()]
	projectScoped := projectPath == "" && !globalMode && wantsProject && !commandInSubtree(cmd, "config")
	if projectScoped && hubFlagEndpoint() != "" {
		warnHubFlagMismatch(r, ctx)
	}
	envHub, _ := envHubEndpoint()
	// The filesystem project is looked up at most once, and only when a
	// tier below it can still fill something.
	var dir string
	var inDir, looked bool
	inFilesystemProject := func() bool {
		if !looked {
			dir, inDir = filesystemProject()
			looked = true
		}
		return inDir
	}

	if hubEndpoint == "" && envHub == "" && inFilesystemProject() {
		if _, h := r.linkEntry(ctx, dir); h != nil {
			hubEndpoint = h.Endpoint
			hubSource = targetSourceDirectory
		}
	}

	if h := ctx.CurrentHubEntry(); h != nil && h.Endpoint != "" && hubEndpoint == "" && envHub == "" {
		hubEndpoint = h.Endpoint
		hubSource = targetSourceContext
	}

	if projectScoped {
		effHub := hubEndpoint
		if effHub == "" {
			effHub = envHub
		}
		if p := ctx.ProjectFor(effHub); p != nil && !inFilesystemProject() {
			projectPath = p.ID
			projectSource = targetSourceContext
			contextProjectSlug = p.Slug
		}
	}
	return nil
}

// hubFlagEndpoint returns the --hub flag value: hubEndpoint, unless
// resolveInvocationTarget filled it from the current context or from a
// link entry. Code that persists "the endpoint given with --hub" must use
// this, so a hub from the hub list is never written into settings.
func hubFlagEndpoint() string {
	if hubSource != "" {
		return ""
	}
	return hubEndpoint
}

// directoryLinkedHub returns the endpoint of the hub the walked directory
// project is linked to, and that hub's nickname: its link entry (after the
// one-time import of a project linked before link entries existed, see
// targetResolution.linkEntry). It returns "" when there is no walked
// project or it is not linked.
func directoryLinkedHub(r targetResolution, c *config.CLIContext) (endpoint, name string) {
	dir, ok := walkedProject()
	if !ok {
		return "", ""
	}
	if p, h := r.linkEntry(c, dir); h != nil {
		return h.Endpoint, p.Hub
	}
	return "", ""
}

// describeHub returns a hub for messages: "'name' (endpoint)", or the
// endpoint alone when the hub has no nickname.
func describeHub(name, endpoint string) string {
	if name == "" {
		return config.RedactURL(endpoint)
	}
	return fmt.Sprintf("'%s' (%s)", name, config.RedactURL(endpoint))
}

// warnHubFlagMismatch warns when --hub selects a hub other than the one
// the walked directory project is linked to: the project's ID is sent to
// the --hub hub, where it most likely does not exist.
func warnHubFlagMismatch(r targetResolution, c *config.CLIContext) {
	dirHub, dirName := directoryLinkedHub(r, c)
	if dirHub == "" || config.NormalizeEndpoint(dirHub) == config.NormalizeEndpoint(hubEndpoint) {
		return
	}
	flagName := hubFlagName
	if flagName == "" {
		flagName, _ = c.HubByEndpoint(hubEndpoint)
	}
	_, _ = fmt.Fprintf(r.stderr, "Warning: this directory is linked to hub %s, but --hub selects %s; the directory's project may not exist there.\n",
		describeHub(dirName, config.NormalizeEndpoint(dirHub)), describeHub(flagName, config.NormalizeEndpoint(hubEndpoint)))
}
