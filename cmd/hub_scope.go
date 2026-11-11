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
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

type hubScopeResolver func(*cobra.Command, *config.Settings) (hubScopeTarget, error)

// hubScopeTarget is the scope a hub env or hub secret command acts on: the
// scope level, its ID (a name, slug or ID to resolve) and, for a project
// scope, where the project came from (a scopeSource constant), so the
// write notice names the source of the ID actually used.
type hubScopeTarget struct {
	scope  string
	id     string
	source string
}

// fromSuffix is " (from <source>)" for the notice, or "" without a source.
func (t hubScopeTarget) fromSuffix() string {
	if t.source == "" {
		return ""
	}
	return " (from " + t.source + ")"
}

// loadHubClient returns the resolved settings alongside the client for callers
// that need project or endpoint metadata after client construction.
func loadHubClient() (*config.Settings, hubclient.Client, error) {
	settings, client, _, err := loadHubClientUAT()
	return settings, client, err
}

// loadHubClientUAT is loadHubClient that also reports whether the client
// sends a user access token (see authSourceIsUserAccessToken).
func loadHubClientUAT() (*config.Settings, hubclient.Client, bool, error) {
	// A hub profile names the hub: no local project settings are read.
	if hubProfileSelected() {
		client, uat, err := getHubClientUAT(nil)
		if err != nil {
			return nil, nil, false, err
		}
		return &config.Settings{}, client, uat, nil
	}
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return nil, nil, false, fmt.Errorf("failed to resolve project path: %w", err)
	}

	// An explicit --project or --global wins over SCION_PROJECT_ID in the
	// environment (ptone/scion#3251).
	settings, err := loadSettingsForTarget(resolvedPath)
	if err != nil {
		return nil, nil, false, fmt.Errorf("failed to load settings: %w", err)
	}

	client, uat, err := getHubClientUAT(settings)
	if err != nil {
		return nil, nil, false, err
	}

	return settings, client, uat, nil
}

// errCannotInferProject is the error of a bare --project (hub env, hub
// secret) when no project applies.
var errCannotInferProject = errors.New("cannot infer project ID: no project selected. " +
	"Select one with 'scion use', link this project with 'scion hub link', or provide an explicit project ID")

// Where the project of a project scope came from, as the write notice of
// hub env set and hub secret set names it.
const (
	scopeSourceFlag        = "--project"
	scopeSourceContext     = "the scion use context"
	scopeSourceDirectory   = "the directory project"
	scopeSourceHubSettings = "the hub settings"
)

// inferScopeProject returns the hub project a bare --project names (hub
// env, hub secret) and where it came from. It takes the project the way
// resolveHubOnlyProject does for every other hub-only command: the directory
// project's hub id (linkedHubProjectID), else the project of the current
// context ('scion use'). A directory project that is not linked to a hub
// is an error, so a local project id is never used as a hub project. With
// no directory project and no context project, the hub project id of the
// loaded settings applies.
//
// With a hub profile selected, the profile names the hub and no local
// project state is read (see loadHubClientUAT), so no project is inferred.
//
// The project is not checked here: the write path (resolveHubScopeForWrite)
// looks it up on the selected hub before any write or clear.
//
// An explicit --project=<value> is handled by the caller before this.
func inferScopeProject(settings *config.Settings) (projectID, source string, err error) {
	if hubProfileSelected() {
		return "", "", errCannotInferProject
	}
	p := resolveHubOnlyProject()
	if p.unlinkedDir != "" {
		return "", "", unlinkedScopeProjectError(p.unlinkedDir)
	}
	if p.ref != "" {
		switch p.source {
		case hubOnlySourceContext:
			return p.ref, scopeSourceContext, nil
		case hubOnlySourceDirectory:
			return p.ref, scopeSourceDirectory, nil
		case hubOnlySourceFlag:
			return p.ref, scopeSourceFlag, nil
		default:
			return "", "", fmt.Errorf("cannot infer project ID: unknown project source %q", p.source)
		}
	}
	if settings != nil && settings.Hub != nil && settings.Hub.ProjectID != "" {
		return settings.Hub.ProjectID, scopeSourceHubSettings, nil
	}
	return "", "", errCannotInferProject
}

// unlinkedScopeProjectError is the error of a bare --project in a directory
// project that is not linked to a hub. It words the remedy of
// hubOnlyProject.unlinkedError for hub env and hub secret: a plain error,
// without the tunnelclient exit code of tunnel, ssh and ssh-config.
func unlinkedScopeProjectError(dir string) error {
	return fmt.Errorf("cannot infer project ID: directory project %s is not linked to a hub. "+
		"Link it with 'scion hub link', or pass --project=<hub project id>", dir)
}

// projectLabel names a project in a notice: its name, followed by its slug
// when that differs, else its slug, else its ID.
func projectLabel(p *hubclient.Project) string {
	switch {
	case p.Name != "" && p.Slug != "" && p.Slug != p.Name:
		return p.Name + " (" + p.Slug + ")"
	case p.Name != "":
		return p.Name
	case p.Slug != "":
		return p.Slug
	}
	return p.ID
}

func resolveHubScope(
	cmd *cobra.Command,
	resolve hubScopeResolver,
) (hubclient.Client, string, string, context.Context, context.CancelFunc, error) {
	return resolveHubScopeWith(cmd, resolve, nil)
}

// resolveHubScopeForWrite is resolveHubScope for a command that writes or
// clears at the resolved scope (hub env set and clear, hub secret set,
// update and clear). For a project scope it looks the project up on the
// selected hub, refuses when the hub does not find it, and first writes
// "Using project <name> (<slug>) (from <source>)" to stderr, so a write or
// clear never lands silently in a project the user did not mean, such as a
// stale 'scion use' project. The command acts on the project's ID as the
// hub returns it. stdout is left to the command's own result.
func resolveHubScopeForWrite(
	cmd *cobra.Command,
	resolve hubScopeResolver,
) (hubclient.Client, string, string, context.Context, context.CancelFunc, error) {
	return resolveHubScopeWith(cmd, resolve, cmd.ErrOrStderr())
}

// resolveHubScopeWith is resolveHubScope, writing the project notice to
// notice when it is not nil.
func resolveHubScopeWith(
	cmd *cobra.Command,
	resolve hubScopeResolver,
	notice io.Writer,
) (hubclient.Client, string, string, context.Context, context.CancelFunc, error) {
	settings, client, uat, err := loadHubClientUAT()
	if err != nil {
		return nil, "", "", nil, nil, err
	}

	target, err := resolve(cmd, settings)
	if err != nil {
		return nil, "", "", nil, nil, err
	}
	scope, scopeID := target.scope, target.id

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if notice != nil && scope == "project" {
		// Look the project up once, for its ID and its label. A project
		// the hub does not resolve is an error and nothing is written: a
		// stale 'scion use' project, or one deleted since, is never
		// written to by its bare ID.
		project, perr := resolveProjectByNameOrID(ctx, client, scopeID, uat)
		if perr != nil {
			cancel()
			return nil, "", "", nil, nil, fmt.Errorf("project %s%s was not found on this hub, so nothing was written: %w",
				scopeID, target.fromSuffix(), perr)
		}
		_, _ = fmt.Fprintf(notice, "Using project %s%s\n", projectLabel(project), target.fromSuffix())
		return client, scope, project.ID, ctx, cancel, nil
	}
	scopeID, err = resolveScopeID(ctx, client, scope, scopeID, uat)
	if err != nil {
		cancel()
		return nil, "", "", nil, nil, err
	}

	return client, scope, scopeID, ctx, cancel, nil
}
