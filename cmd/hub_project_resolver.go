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
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/google/uuid"
)

// resolvedHubProject describes the Hub project resolved for a CLI invocation
// following standard CLI precedence:
//  0. --hub-profile (requires explicit --project; reads no local settings)
//  1. --global or --project global/home
//  2. Explicit --project / -g (linked local path or Hub ID/slug/name/git URL)
//  3. Hub/agent container environment (SCION_HUB_PROJECT_ID / SCION_PROJECT_ID)
//  4. Walked directory project (.scion), when linked to a Hub
//  5. 'scion use' context project (outside any filesystem project)
//  6. ~/.scion global settings, when linked to a Hub project
type resolvedHubProject struct {
	ID          string
	Display     string
	UnlinkedDir string
}

// loadHubClientForProjectResolution loads the Hub client without treating a Hub
// project reference (slug, name, UUID, git URL) or a 'scion use' context UUID
// in projectPath as a local filesystem directory, and without reading local
// project settings when a target-hub profile (--hub-profile) is selected.
func loadHubClientForProjectResolution() (hubclient.Client, bool, error) {
	if hubProfileSelected() {
		return getHubClientUAT(nil)
	}

	settingsTarget := ""
	if projectPath != "" && projectSource != targetSourceContext && !hubsync.IsHubProjectRef(projectPath) {
		settingsTarget = projectPath
	}

	resolvedPath, _, err := config.ResolveProjectPath(settingsTarget)
	if err != nil {
		return nil, false, fmt.Errorf("failed to resolve project path: %w", err)
	}

	var settings *config.Settings
	if settingsTarget != "" {
		settings, err = loadSettingsForTarget(resolvedPath)
	} else {
		settings, err = config.LoadSettings(resolvedPath)
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to load settings: %w", err)
	}

	if !hubConfigured() {
		return nil, false, errHubNotConfigured
	}

	return getHubClientUAT(settings)
}

// requireHubProjectClient loads a Hub client and resolves the required Hub
// project ID for project-scoped commands (such as 'scion project service-accounts',
// 'scion project settings', and 'scion project identity default').
func requireHubProjectClient() (hubclient.Client, string, error) {
	if hubProfileSelected() {
		if globalMode {
			return nil, "", errors.New("--global does not apply with a hub profile: name the project on the target hub with --project <name or id>")
		}
		if strings.TrimSpace(projectPath) == "" || projectSource == targetSourceContext {
			return nil, "", errProfileNeedsProject
		}
	}

	client, uat, err := loadHubClientForProjectResolution()
	if err != nil {
		return nil, "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := resolveHubProject(ctx, client, uat, true)
	if err != nil {
		return nil, "", err
	}
	if res.UnlinkedDir != "" {
		return nil, "", unlinkedScopeProjectError(res.UnlinkedDir)
	}
	if res.ID == "" {
		return nil, "", errCannotInferProject
	}
	return client, res.ID, nil
}

// resolveHubProject resolves the active Hub project across all CLI tiers.
// When required is true, a missing project under --hub-profile or an unlinked
// explicit --global target looks up or fails appropriately; an unlinked walked
// directory is reported in UnlinkedDir so optional-filter callers (such as
// 'scion notifications subscriptions') can emit a note while required callers
// return unlinkedScopeProjectError.
func resolveHubProject(ctx context.Context, client hubclient.Client, uat, required bool) (resolvedHubProject, error) {
	if hubProfileSelected() {
		if globalMode {
			if required {
				return resolvedHubProject{}, errors.New("--global does not apply with a hub profile: name the project on the target hub with --project <name or id>")
			}
			return resolvedHubProject{}, nil
		}
		ref := strings.TrimSpace(projectPath)
		if ref == "" || projectSource == targetSourceContext {
			if required {
				return resolvedHubProject{}, errProfileNeedsProject
			}
			return resolvedHubProject{}, nil
		}
		id, disp, err := resolveHubProjectByRef(ctx, client, ref, uat)
		if err != nil {
			return resolvedHubProject{}, err
		}
		return resolvedHubProject{ID: id, Display: disp}, nil
	}

	// 1. --global (without a separate --project) or --project global/home.
	if (globalMode && (projectPath == "" || projectPath == "global")) || (!globalMode && (projectPath == "global" || projectPath == "home")) {
		if globalDir, gerr := config.GetGlobalDir(); gerr == nil {
			if id := linkedGlobalHubProjectID(globalDir); id != "" {
				return resolvedHubProject{ID: id, Display: formatResolvedProjectDisplay("global", id)}, nil
			}
		}
		if required {
			resp, lerr := client.Projects().List(ctx, &hubclient.ListProjectsOptions{Slug: "global"})
			if lerr == nil && resp != nil && len(resp.Projects) == 1 {
				p := resp.Projects[0]
				return resolvedHubProject{ID: p.ID, Display: formatResolvedProjectDisplay(projectLabel(&p), p.ID)}, nil
			}
			return resolvedHubProject{}, unlinkedScopeProjectError("global")
		}
		return resolvedHubProject{}, nil
	}

	// 2. Explicit --project / -g flag.
	if projectPath != "" && projectSource != targetSourceContext {
		if !hubsync.IsHubProjectRef(projectPath) {
			dir, isGlobal, rerr := config.ResolveProjectPath(projectPath)
			if rerr != nil {
				return resolvedHubProject{}, fmt.Errorf("failed to resolve project path: %w", rerr)
			}
			var id string
			if isGlobal {
				id = linkedGlobalHubProjectID(dir)
			} else {
				id = linkedHubProjectID(dir)
			}
			if id == "" {
				return resolvedHubProject{}, unlinkedScopeProjectError(config.GetProjectName(dir))
			}
			return resolvedHubProject{ID: id, Display: formatResolvedProjectDisplay(config.GetProjectName(dir), id)}, nil
		}
		id, disp, rerr := resolveHubProjectByRef(ctx, client, projectPath, uat)
		if rerr != nil {
			return resolvedHubProject{}, rerr
		}
		return resolvedHubProject{ID: id, Display: disp}, nil
	}

	// 3. Hub/agent container environment (SCION_HUB_PROJECT_ID / SCION_PROJECT_ID)
	// wins over a cloned checkout's .scion/project-id, matching hubsync.EnsureHubReady.
	if id := config.ContainerProjectIDFromEnv(); id != "" {
		return resolvedHubProject{ID: id, Display: formatResolvedProjectDisplay(os.Getenv("SCION_PROJECT"), id)}, nil
	}

	// 4. Walked directory project (.scion): use its linked Hub project ID only.
	// Never send an unlinked local .scion/project-id to the Hub.
	if dir, ok := walkedProject(); ok {
		name := config.GetProjectName(dir)
		if id := linkedHubProjectID(dir); id != "" {
			return resolvedHubProject{ID: id, Display: formatResolvedProjectDisplay(name, id)}, nil
		}
		return resolvedHubProject{UnlinkedDir: name}, nil
	}

	// 5. 'scion use' context project (outside any filesystem project).
	if projectSource == targetSourceContext && projectPath != "" {
		return resolvedHubProject{ID: projectPath, Display: formatResolvedProjectDisplay(contextProjectSlug, projectPath)}, nil
	}

	// 6. Fallback: ~/.scion global settings if explicitly linked to a Hub project.
	if globalDir, gerr := config.GetGlobalDir(); gerr == nil {
		if id := linkedGlobalHubProjectID(globalDir); id != "" {
			return resolvedHubProject{ID: id, Display: formatResolvedProjectDisplay("global", id)}, nil
		}
	}

	return resolvedHubProject{}, nil
}

// linkedGlobalHubProjectID returns the Hub project ID linked in globalDir
// (~/.scion), reading it via LoadSettingsFromDir (which allows symlinked global
// settings files) and ProjectLinkedSetting(globalDir, true).
func linkedGlobalHubProjectID(globalDir string) string {
	versioned := isVersionedSettingsDir(globalDir)
	if !versioned {
		if s, err := config.LoadSettingsFromDir(globalDir); err == nil && s != nil {
			if s.Hub != nil && s.Hub.ProjectID != "" {
				return s.Hub.ProjectID
			}
			if linked := hubsync.ProjectLinkedSetting(globalDir, true); linked != nil && *linked {
				return s.ProjectID
			}
		}
		return ""
	}
	if linked := hubsync.ProjectLinkedSetting(globalDir, true); linked != nil && *linked {
		if vs, err := config.LoadSingleFileVersioned(globalDir); err == nil && vs != nil && vs.Hub != nil {
			return vs.Hub.ProjectID
		}
	}
	return ""
}

// resolveHubProjectByRef resolves a Hub project reference (UUID, git URL, slug,
// or name) against the Hub API. When ref is already a valid UUID and the caller
// lacks project:read (403 Forbidden), the UUID itself is used directly so
// project-scoped sub-resource routes (such as service-account status or
// subscriptions) can authorize on their own scope.
func resolveHubProjectByRef(ctx context.Context, client hubclient.Client, ref string, uat bool) (projectID, display string, err error) {
	var project *hubclient.Project
	if util.IsGitURL(ref) {
		project, err = hubsync.ResolveProjectOnHub(ctx, client, ref)
		if err != nil {
			var notFound *hubsync.ProjectNotFoundError
			if errors.As(err, &notFound) && uat {
				return "", "", hubsync.ScopedTokenProjectLookupEmptyError(ref)
			}
			return "", "", err
		}
	} else {
		project, err = resolveProjectByNameOrID(ctx, client, ref, uat)
		if err != nil {
			if _, uerr := uuid.Parse(ref); uerr == nil && apiclient.IsForbiddenError(err) {
				return ref, ref, nil
			}
			return "", "", err
		}
	}
	if project == nil || project.ID == "" {
		return "", "", fmt.Errorf("the Hub returned no ID for project %q", ref)
	}
	return project.ID, formatResolvedProjectDisplay(projectLabel(project), project.ID), nil
}

func formatResolvedProjectDisplay(label, id string) string {
	if label == "" || label == id {
		return id
	}
	if strings.Contains(label, id) {
		return label
	}
	return fmt.Sprintf("%s (%s)", label, id)
}
