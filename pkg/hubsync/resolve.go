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

package hubsync

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/google/uuid"
)

// IsHubProjectRef returns true if the given project path value looks like a hub
// project reference (slug, name, UUID, or git URL) rather than a filesystem path.
// It is used to decide whether to resolve the project via the hub API instead of
// the local filesystem.
func IsHubProjectRef(projectPath string) bool {
	if projectPath == "" {
		return false
	}

	// "global" and "home" are special filesystem-like values
	if projectPath == "global" || projectPath == "home" {
		return false
	}

	// Git URLs are always hub references
	if util.IsGitURL(projectPath) {
		return true
	}

	// Absolute or explicitly relative paths are filesystem references
	if strings.HasPrefix(projectPath, "/") || strings.HasPrefix(projectPath, "./") || strings.HasPrefix(projectPath, "../") {
		return false
	}

	// Contains path separators → filesystem path
	if strings.Contains(projectPath, string(os.PathSeparator)) {
		return false
	}

	// Could be a slug or a relative directory name. Check the filesystem:
	// if the path exists as a directory or contains a .scion subdirectory,
	// treat it as a local path.
	if info, err := os.Stat(projectPath); err == nil && info.IsDir() {
		return false
	}
	if info, err := os.Stat(projectPath + "/.scion"); err == nil && info.IsDir() {
		return false
	}

	return true
}

// resolveHubProjectRef resolves a hub project reference (slug, name, UUID, or git
// URL) by loading hub settings from a fallback project and querying the hub API.
func resolveHubProjectRef(ref string, opts EnsureHubReadyOptions) (*HubContext, error) {
	debugf("resolveHubProjectRef: ref=%s", ref)

	// Load hub settings from the fallback project (current project or global)
	fallbackPath, isGlobal, err := config.ResolveProjectPath("")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve fallback project for hub settings: %w", err)
	}
	debugf("resolveHubProjectRef: fallbackPath=%s, isGlobal=%v", fallbackPath, isGlobal)

	settings, err := config.LoadSettings(fallbackPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load settings from fallback project: %w", err)
	}

	debugf("resolveHubProjectRef: hub=%v, hubConfigured=%v, hubEnabled=%v, hubExplicitlyDisabled=%v",
		settings.Hub != nil, settings.IsHubConfigured(), settings.IsHubEnabled(), settings.IsHubExplicitlyDisabled())
	if settings.Hub != nil {
		hasToken := settings.Hub.Token != ""
		hasAPIKey := settings.Hub.APIKey != ""
		hasEndpoint := settings.Hub.Endpoint != ""
		enabledPtr := "<nil>"
		if settings.Hub.Enabled != nil {
			enabledPtr = fmt.Sprintf("%v", *settings.Hub.Enabled)
		}
		debugf("resolveHubProjectRef: hub.enabled=%s, hub.endpoint=%v, hub.hasToken=%v, hub.hasAPIKey=%v",
			enabledPtr, hasEndpoint, hasToken, hasAPIKey)
	}

	if !settings.IsHubEnabled() {
		return nil, fmt.Errorf("hub project references (slugs, names, git URLs) require hub mode to be enabled\n\n" +
			"Enable with: scion config set hub.enabled true")
	}

	endpoint := opts.EndpointOverride
	if endpoint == "" {
		endpoint = getEndpoint(settings)
	}
	if endpoint == "" {
		return nil, fmt.Errorf("hub is enabled but no endpoint configured\n\nConfigure via: scion config set hub.endpoint <url>")
	}

	client, err := createHubClient(settings, endpoint)
	if err != nil {
		return nil, wrapHubError(fmt.Errorf("failed to create hub client: %w", err))
	}

	// Health check
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Health(ctx); err != nil {
		return nil, wrapHubError(fmt.Errorf("hub at %s is not responding: %w", endpoint, err))
	}

	// Resolve the project on the hub
	project, err := resolveProjectOnHub(ctx, client, ref)
	if err != nil {
		return nil, err
	}

	brokerID := ""
	if settings.Hub != nil {
		brokerID = settings.Hub.BrokerID
	}

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: endpoint,
		Settings: settings,
		ProjectID:  project.ID,
		BrokerID: brokerID,
		// Use the fallback project path for settings access, not the target project
		ProjectPath: fallbackPath,
		IsGlobal:  isGlobal,
	}

	debugf("resolveHubProjectRef: resolved project %s (ID: %s) via hub", project.Name, project.ID)
	return hubCtx, nil
}

// resolveProjectOnHub resolves a project reference on the hub, trying multiple
// strategies in order: UUID, git URL, slug, name.
func resolveProjectOnHub(ctx context.Context, client hubclient.Client, ref string) (*hubclient.Project, error) {
	// 1. Try as UUID
	if _, err := uuid.Parse(ref); err == nil {
		project, err := client.Projects().Get(ctx, ref)
		if err == nil {
			return project, nil
		}
		if !apiclient.IsNotFoundError(err) {
			return nil, fmt.Errorf("failed to get project by ID: %w", err)
		}
		// UUID format but not found — fall through to other strategies
	}

	// 2. Try as git URL
	if util.IsGitURL(ref) {
		resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
			GitRemote: util.NormalizeGitRemote(ref),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to search for project by git URL: %w", err)
		}
		switch len(resp.Projects) {
		case 0:
			return nil, fmt.Errorf("no project found for git URL '%s'", ref)
		case 1:
			return &resp.Projects[0], nil
		default:
			return nil, fmt.Errorf("multiple projects found for git URL '%s' — please use a project ID or slug instead", ref)
		}
	}

	// 3. Try as slug
	resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
		Slug: ref,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for project by slug: %w", err)
	}
	if len(resp.Projects) == 1 {
		return &resp.Projects[0], nil
	}

	// 4. Try as name
	resp, err = client.Projects().List(ctx, &hubclient.ListProjectsOptions{
		Name: ref,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for project by name: %w", err)
	}
	switch len(resp.Projects) {
	case 0:
		return nil, fmt.Errorf("project '%s' not found on hub", ref)
	case 1:
		return &resp.Projects[0], nil
	default:
		return nil, fmt.Errorf("multiple projects found with name '%s' — please use a project ID or slug instead", ref)
	}
}
