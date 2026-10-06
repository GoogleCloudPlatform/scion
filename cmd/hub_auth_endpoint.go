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
	"io"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// hubAuthURLPrecedence documents the hub URL order 'scion hub auth login'
// (and logout, without --hub-url) uses; resolveHubAuthURL implements it.
const hubAuthURLPrecedence = `The hub URL is taken from, in order:
  1. --hub-url
  2. the root --hub flag
  3. the SCION_HUB_ENDPOINT environment variable
  4. hub.endpoint in settings (the current project's, else global)`

// resolveHubAuthURL resolves the hub URL for hub auth login and logout:
// --hub-url, then the root --hub flag, then SCION_HUB_ENDPOINT, then the
// settings endpoint (ptone/scion#3537). It returns the URL and its source
// ("--hub-url", "--hub", "env", "settings" or "" when none is set).
func resolveHubAuthURL(hubURLFlag, rootHubFlag string, getenv func(string) string, settingsEndpoint func() string) (string, string) {
	if hubURLFlag != "" {
		return hubURLFlag, "--hub-url"
	}
	if rootHubFlag != "" {
		return rootHubFlag, "--hub"
	}
	if env := getenv("SCION_HUB_ENDPOINT"); env != "" {
		return env, "env"
	}
	if ep := settingsEndpoint(); ep != "" {
		return ep, "settings"
	}
	return "", ""
}

// settingsHubEndpoint returns hub.endpoint from the settings of the current
// project, falling back to global settings.
func settingsHubEndpoint() string {
	projectPath, _, err := config.ResolveProjectPath("")
	if err != nil {
		return ""
	}
	settings, err := config.LoadSettings(projectPath)
	if err != nil {
		return ""
	}
	return settings.GetHubEndpoint()
}

// sameHubURL reports whether a and b name the same hub, ignoring a trailing
// slash and letter case in the scheme and host.
func sameHubURL(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/")) }
	return norm(a) == norm(b)
}

// loginEndpointOptions carries what persistLoginEndpoint needs, so tests can
// drive it without a terminal.
type loginEndpointOptions struct {
	// HubURL is the hub the login authenticated against.
	HubURL string
	// ProjectPath and IsGlobal name the settings scope of the invocation.
	ProjectPath string
	IsGlobal    bool
	// Interactive reports whether the user can answer a prompt.
	Interactive bool
	// Confirm asks a yes/no question (default yes).
	Confirm func(prompt string) bool
}

// persistLoginEndpoint makes a successful 'hub auth login' usable by the
// rest of the CLI (ptone/scion#3532). When no hub.endpoint is set in the
// settings of the invocation's scope (the project's own, else global), it
// saves the hub URL there, so 'hub status' and every other hub command find
// the hub the stored credentials belong to. An endpoint that is already set
// is never overwritten; a note says how to use the other hub. When hub mode
// is off for that endpoint it then offers, interactively only, to enable it;
// otherwise it prints the 'scion hub enable' hint.
func persistLoginEndpoint(out io.Writer, opts loginEndpointOptions) error {
	scope := "global"
	if !opts.IsGlobal {
		scope = "project"
	}
	configured, configuredScope := fileHubEndpoint(opts.ProjectPath, opts.IsGlobal)
	switch {
	case configured == "":
		if err := config.UpdateSetting(opts.ProjectPath, "hub.endpoint", opts.HubURL, opts.IsGlobal); err != nil {
			return fmt.Errorf("failed to save hub endpoint: %w", err)
		}
		_, _ = fmt.Fprintf(out, "Saved hub endpoint %s to %s settings.\n", opts.HubURL, scope)
	case !sameHubURL(configured, opts.HubURL):
		_, _ = fmt.Fprintf(out, "Note: hub.endpoint is %s (%s settings) and was left unchanged.\n", configured, configuredScope)
		_, _ = fmt.Fprintf(out, "To use %s, pass --hub %s or run 'scion config set hub.endpoint %s'.\n", opts.HubURL, opts.HubURL, opts.HubURL)
		return nil
	}

	settings, err := config.LoadSettings(opts.ProjectPath)
	if err != nil || settings.IsHubEnabled() {
		return nil
	}
	if opts.Interactive && opts.Confirm != nil && opts.Confirm("Hub mode is not enabled. Enable it now (scion hub enable)?") {
		if err := config.UpdateSetting(opts.ProjectPath, "hub.enabled", "true", opts.IsGlobal); err != nil {
			return fmt.Errorf("failed to enable hub mode: %w", err)
		}
		_, _ = fmt.Fprintf(out, "Hub mode enabled (%s scope).\n", scope)
		return nil
	}
	_, _ = fmt.Fprintln(out, "Hub mode is not enabled. Run 'scion hub enable' to route agent operations through this hub.")
	return nil
}

// fileHubEndpoint returns the hub.endpoint written in settings files for
// the invocation's scope: the project's own settings (unless global), else
// global settings. Environment overrides are not included: they are not
// persisted, so they don't count as configured.
func fileHubEndpoint(projectPath string, isGlobal bool) (endpoint, scope string) {
	if !isGlobal {
		if s, err := config.LoadSettingsFromDir(projectPath); err == nil && s.Hub != nil && s.Hub.Endpoint != "" {
			return s.Hub.Endpoint, "project"
		}
	}
	if globalDir, err := config.GetGlobalDir(); err == nil {
		if s, err := config.LoadSettingsFromDir(globalDir); err == nil && s.Hub != nil && s.Hub.Endpoint != "" {
			return s.Hub.Endpoint, "global"
		}
	}
	return "", ""
}

// persistLoginEndpointForInvocation runs persistLoginEndpoint for the scope
// of the current command and the real terminal.
func persistLoginEndpointForInvocation(hubURL string) {
	resolvedPath, isGlobal, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		fmt.Printf("Warning: could not resolve settings scope to save the hub endpoint: %v\n", err)
		return
	}
	err = persistLoginEndpoint(os.Stdout, loginEndpointOptions{
		HubURL:      hubURL,
		ProjectPath: resolvedPath,
		IsGlobal:    isGlobal,
		Interactive: util.IsTerminal() && !nonInteractive,
		Confirm: func(prompt string) bool {
			return hubsync.ConfirmAction(prompt, true, autoConfirm)
		},
	})
	if err != nil {
		fmt.Printf("Warning: %v\n", err)
	}
}
