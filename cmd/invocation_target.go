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
	// contextProjectID is the hub project UUID resolveInvocationTarget
	// injected as projectPath, or "".
	contextProjectID string
	// contextProjectSlug is the slug of the contextProjectID project, as
	// recorded in the context, or "". It is for messages only.
	contextProjectSlug string
	// hubFlagName is the hub nickname given with --hub, which
	// resolveInvocationTarget replaced by its endpoint in hubEndpoint, or "".
	hubFlagName string
	// contextWarned records that resolveInvocationTarget already warned
	// about an unreadable context.yaml in this invocation.
	contextWarned bool
)

// isContextProject reports whether path is the project that
// resolveInvocationTarget took from the current context.
func isContextProject(path string) bool {
	return projectSource == targetSourceContext && path != "" && path == contextProjectID
}

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

// mergedHubEndpoint returns the hub endpoint in the merged settings of
// projectDir (its own settings over the global ones).
func mergedHubEndpoint(projectDir string) string {
	settings, err := config.LoadSettings(projectDir)
	if err != nil || settings == nil {
		return ""
	}
	return settings.GetHubEndpoint()
}

// contextSkippedSubtrees are top-level commands for which the current
// context is never applied. The server and the runtime broker take their
// hub endpoint from the global settings (it is the endpoint the server
// advertises to its agents), so 'scion use' must never retarget them. A
// --hub nickname is still resolved for them, as for every command: it is
// an explicit choice, not the context.
var contextSkippedSubtrees = map[string]bool{
	"server":         true,
	"runtime-broker": true,
}

// contextProjectCommands are hub-only commands (see hubOnlyCommands), by
// command path, that take the context's project like the project-scoped
// commands do, although they need no local project. expose and unexpose
// act as the calling agent and stay out.
var contextProjectCommands = map[string]bool{
	"scion tunnel":     true,
	"scion ssh":        true,
	"scion ssh-config": true,
}

// importGlobalHubSettings turns on the one-time import of the global
// settings' hub.endpoint into the hub list when the context is read. It is
// always on in the CLI; only the cmd tests' TestMain turns it off (see
// there).
var importGlobalHubSettings = true

// loadInvocationContext reads context.yaml for this invocation, after the
// one-time import of the global settings' hub configuration. A failure to
// save the import is a warning on stderr: the returned context holds it
// anyway.
func loadInvocationContext(stderr io.Writer) (*config.CLIContext, error) {
	if !importGlobalHubSettings {
		return config.LoadCLIContext()
	}
	c, err := config.LoadCLIContextImported(useHubBaseName)
	if errors.Is(err, config.ErrCLIContextImport) {
		_, _ = fmt.Fprintf(stderr, "Warning: %v\n", err)
		return c, nil
	}
	return c, err
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
// overrides a flag. The precedence is: flags, then the environment, then
// the directory project (its link entry, then its settings), then the
// current context, then the global settings.
//
//   - --hub: a hub nickname is replaced by its entry's endpoint; an unknown
//     nickname is an error that lists the registered names. When the
//     directory project is linked to another hub, a warning says so.
//   - Hub: set from the filesystem project's link entry ('scion hub link')
//     when neither --hub nor SCION_HUB_ENDPOINT/SCION_HUB_URL is set.
//     Otherwise set from the current hub when there is no filesystem
//     project, or its merged settings have no endpoint.
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
// For the server and runtime-broker subtrees (contextSkippedSubtrees) only
// a --hub nickname is resolved; nothing else is filled, and without a
// nickname context.yaml is not read.
func resolveInvocationTarget(cmd *cobra.Command, mode CLIMode) error {
	hubSource = ""
	projectSource = ""
	contextProjectID = ""
	contextProjectSlug = ""
	contextWarned = false
	hubFlagName = ""
	if contextDisabled(mode) {
		return nil
	}
	// --no-hub turns the hub off, and a context project is a hub project.
	// Remove this check together with the --no-hub flag.
	if noHub {
		return nil
	}
	nicknameOnly := contextSkippedSubtrees[topLevelName(cmd)]
	if nicknameOnly && !isHubNickname(hubEndpoint) {
		return nil
	}
	ctx, err := loadInvocationContext(cmd.ErrOrStderr())
	if err != nil {
		if isHubNickname(hubEndpoint) {
			return fmt.Errorf("cannot look up hub '%s': %w", hubEndpoint, err)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: ignoring the current context: %v\n", err)
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
		warnHubFlagMismatch(cmd.ErrOrStderr(), ctx)
	}
	// Without hub entries the context can fill nothing: skip the project
	// walk and settings load below.
	if len(ctx.Hubs) == 0 {
		return nil
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
		if _, h := ctx.LinkEntryFor(config.LinkPathForProject(dir)); h != nil {
			hubEndpoint = h.Endpoint
			hubSource = targetSourceDirectory
		}
	}

	if h := ctx.CurrentHubEntry(); h != nil && h.Endpoint != "" && hubEndpoint == "" && envHub == "" {
		if !inFilesystemProject() || mergedHubEndpoint(dir) == "" {
			hubEndpoint = h.Endpoint
			hubSource = targetSourceContext
		}
	}

	if projectScoped {
		effectiveHub := hubEndpoint
		if effectiveHub == "" {
			effectiveHub = envHub
		}
		if p := ctx.ProjectFor(effectiveHub); p != nil && !inFilesystemProject() {
			projectPath = p.ID
			projectSource = targetSourceContext
			contextProjectID = p.ID
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
// project is linked to, and that hub's nickname ("" when it has none): its
// link entry, or else, for a project linked before link entries existed,
// the hub.endpoint in its own settings. It returns "" when there is no
// walked project or it is not linked.
func directoryLinkedHub(c *config.CLIContext) (endpoint, name string) {
	dir, ok := walkedProject()
	if !ok {
		return "", ""
	}
	if p, h := c.LinkEntryFor(config.LinkPathForProject(dir)); h != nil {
		return h.Endpoint, p.Hub
	}
	if linked := hubsync.ProjectLinkedSetting(dir, false); linked == nil || !*linked {
		return "", ""
	}
	ep, scope := fileHubEndpoint(dir, false)
	if scope != "project" {
		return "", ""
	}
	name, _ = c.HubByEndpoint(ep)
	return ep, name
}

// describeHub returns a hub for messages: "'name' (endpoint)", or the
// endpoint alone when the hub has no nickname.
func describeHub(name, endpoint string) string {
	if name == "" {
		return endpoint
	}
	return fmt.Sprintf("'%s' (%s)", name, endpoint)
}

// warnHubFlagMismatch warns when --hub selects a hub other than the one
// the walked directory project is linked to: the project's ID is sent to
// the --hub hub, where it most likely does not exist.
func warnHubFlagMismatch(stderr io.Writer, c *config.CLIContext) {
	dirHub, dirName := directoryLinkedHub(c)
	if dirHub == "" || config.NormalizeEndpoint(dirHub) == config.NormalizeEndpoint(hubEndpoint) {
		return
	}
	flagName := hubFlagName
	if flagName == "" {
		flagName, _ = c.HubByEndpoint(hubEndpoint)
	}
	_, _ = fmt.Fprintf(stderr, "Warning: this directory is linked to hub %s, but --hub selects %s; the directory's project may not exist there.\n",
		describeHub(dirName, config.NormalizeEndpoint(dirHub)), describeHub(flagName, config.NormalizeEndpoint(hubEndpoint)))
}

// persistHubFlagEndpoint saves the --hub flag value as hub.endpoint in the
// settings of resolvedPath (global settings when isGlobal). It does nothing
// without the flag, including when the hub came from the current context.
func persistHubFlagEndpoint(resolvedPath string, isGlobal bool) error {
	ep := hubFlagEndpoint()
	if ep == "" {
		return nil
	}
	if err := config.UpdateSetting(resolvedPath, "hub.endpoint", ep, isGlobal); err != nil {
		return fmt.Errorf("failed to save endpoint: %w", err)
	}
	return nil
}
