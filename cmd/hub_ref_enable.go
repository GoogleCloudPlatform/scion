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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// hubRefEnableOptions carries what ensureHubModeForHubProjectRef needs, so
// tests can drive it without a terminal or stored credentials.
type hubRefEnableOptions struct {
	// Interactive reports whether the user can answer a prompt.
	Interactive bool
	// Confirm asks a yes/no question (default yes).
	Confirm func(prompt string) bool
	// LoggedIn reports whether the CLI holds credentials for endpoint.
	LoggedIn func(settings *config.Settings, endpoint string) bool
}

// ensureHubModeForHubProjectRef runs before a command resolves a hub project
// reference (scion start -g <hub-project>, ptone/scion#3533). Hub project
// references need hub mode, which is read from the same settings the hub
// resolution uses (the current project, else global). When it is off and
// the user is logged in to the configured hub, an interactive run offers to
// enable it once, as 'scion hub enable' would. It never enables hub mode
// without asking. Otherwise it returns an error naming 'scion hub enable'
// (and, with no endpoint, 'scion hub auth login').
func ensureHubModeForHubProjectRef(out io.Writer, ref string, opts hubRefEnableOptions) error {
	fallbackPath, isGlobal, err := config.ResolveProjectPath("")
	if err != nil {
		return nil // hub resolution reports this itself
	}
	settings, err := config.LoadSettings(fallbackPath)
	if err != nil || settings.IsHubEnabled() {
		return nil
	}
	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return errors.New("hub project references (slugs, names, git URLs) require hub mode to be enabled\n\n" +
			"Log in to a hub with: scion hub auth login --hub-url <url>\n" +
			"Then enable hub mode with: scion hub enable")
	}
	if opts.Interactive && opts.Confirm != nil && opts.LoggedIn != nil && opts.LoggedIn(settings, endpoint) {
		_, _ = fmt.Fprintf(out, "'%s' is a hub project reference, but hub mode is not enabled.\n", ref)
		if opts.Confirm(fmt.Sprintf("Enable hub mode for %s now (scion hub enable)?", endpoint)) {
			if err := config.UpdateSetting(fallbackPath, "hub.enabled", "true", isGlobal); err != nil {
				return fmt.Errorf("failed to enable hub mode: %w", err)
			}
			scope := "global"
			if !isGlobal {
				scope = "project"
			}
			_, _ = fmt.Fprintf(out, "Hub mode enabled (%s scope).\n", scope)
			return nil
		}
	}
	return errors.New("hub project references (slugs, names, git URLs) require hub mode to be enabled\n\n" +
		"Enable with: scion hub enable")
}

// ensureHubModeForHubProjectRefForInvocation runs
// ensureHubModeForHubProjectRef with the real terminal and credentials.
func ensureHubModeForHubProjectRefForInvocation(ref string) error {
	return ensureHubModeForHubProjectRef(os.Stdout, ref, hubRefEnableOptions{
		Interactive: util.IsTerminal() && !nonInteractive,
		Confirm: func(prompt string) bool {
			return hubsync.ConfirmAction(prompt, true, autoConfirm)
		},
		LoggedIn: func(settings *config.Settings, endpoint string) bool {
			return getAuthInfo(settings, endpoint).MethodType != "none"
		},
	})
}
