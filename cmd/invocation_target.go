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
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
)

// targetSourceContext marks a hub endpoint or project that
// resolveInvocationTarget took from the current context ('scion use').
const targetSourceContext = "context"

var (
	// hubSource is targetSourceContext when resolveInvocationTarget set
	// hubEndpoint from the current context, and "" otherwise.
	hubSource string
	// projectSource is targetSourceContext when resolveInvocationTarget set
	// projectPath from the current context, and "" otherwise.
	projectSource string
	// contextProjectID is the hub project UUID resolveInvocationTarget
	// injected as projectPath, or "".
	contextProjectID string
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
func envHubEndpoint() (endpoint, name string) {
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
// directory, unless that is the global directory.
func walkedProject() (string, bool) {
	p, ok := config.FindProjectRoot()
	if !ok {
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
// advertises to its agents), so 'scion use' must never retarget them.
var contextSkippedSubtrees = map[string]bool{
	"server":         true,
	"runtime-broker": true,
}

// resolveInvocationTarget fills the hub endpoint and the project of this
// invocation from the current context ('scion use') where nothing more
// specific chose them. It only fills empty values and never overrides a
// flag. The precedence is: flags, then the environment, then the directory
// project, then the current context, then the global settings.
//
//   - Hub: set from the current hub when neither --hub nor
//     SCION_HUB_ENDPOINT/SCION_HUB_URL is set and there is no filesystem
//     project, or its merged settings have no endpoint.
//   - Project: set to the hub UUID of the project remembered for the
//     effective hub, when no --project, no --global and no filesystem
//     project apply and the command needs a project. A UUID is used rather
//     than the slug because it can never be mistaken for a local directory.
//
// It records hubSource and projectSource. A context file that cannot be
// read is a warning, and the invocation proceeds without it.
func resolveInvocationTarget(cmd *cobra.Command, mode CLIMode) {
	hubSource = ""
	projectSource = ""
	contextProjectID = ""
	if contextDisabled(mode) || contextSkippedSubtrees[topLevelName(cmd)] {
		return
	}
	// --no-hub turns the hub off, and a context project is a hub project.
	// Local-mode removal phase 3 removes --no-hub, and this check with it.
	if noHub {
		return
	}
	ctx, err := config.LoadCLIContext()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: ignoring the current context: %v\n", err)
		return
	}
	envHub, _ := envHubEndpoint()
	dir, inDir := filesystemProject()

	if hubEndpoint == "" && envHub == "" && (!inDir || mergedHubEndpoint(dir) == "") {
		if h := ctx.CurrentHubEntry(); h != nil && h.Endpoint != "" {
			hubEndpoint = h.Endpoint
			hubSource = targetSourceContext
		}
	}

	if projectPath == "" && !globalMode && !inDir && commandRequiresProject(cmd) {
		effectiveHub := hubEndpoint
		if effectiveHub == "" {
			effectiveHub = envHub
		}
		if p := ctx.ProjectFor(effectiveHub); p != nil {
			projectPath = p.ID
			projectSource = targetSourceContext
			contextProjectID = p.ID
		}
	}
}
