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

package fswatcher

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// ProjectDiscovery discovers watch directories from running Docker containers
// that belong to a specific project.
type ProjectDiscovery struct {
	dockerClient *client.Client
	projectID    string
	debug        bool
}

// NewProjectDiscovery creates a ProjectDiscovery for the given project ID.
func NewProjectDiscovery(dockerClient *client.Client, projectID string, debug bool) *ProjectDiscovery {
	return &ProjectDiscovery{
		dockerClient: dockerClient,
		projectID:    projectID,
		debug:        debug,
	}
}

// Discover returns the set of host directories to watch, discovered from
// container bind mounts for all containers in the project.
func (g *ProjectDiscovery) Discover(ctx context.Context) ([]string, error) {
	containers, err := g.dockerClient.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", fmt.Sprintf("scion.grove=%s", g.projectID)),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("listing project containers: %w", err)
	}

	seen := make(map[string]bool)
	var dirs []string

	for _, c := range containers {
		info, err := g.dockerClient.ContainerInspect(ctx, c.ID)
		if err != nil {
			if g.debug {
				log.Printf("[project] failed to inspect container %s: %v", c.ID[:12], err)
			}
			continue
		}

		for _, mount := range info.Mounts {
			if mount.Type != "bind" {
				continue
			}
			// Look for workspace mounts — the destination is typically /workspace,
			// but may also be /repo-root (when the full project is bind-mounted)
			// or a sub-path like /repo-root/.scion/agents/<name>/workspace.
			if !isWorkspaceMount(mount.Destination) {
				continue
			}
			hostPath := mount.Source
			if !seen[hostPath] {
				seen[hostPath] = true
				dirs = append(dirs, hostPath)
				if g.debug {
					agentName := info.Config.Labels["scion.name"]
					log.Printf("[project] discovered watch dir: %s (agent: %s, dest: %s)", hostPath, agentName, mount.Destination)
				}
			}
		}
	}

	if g.debug {
		log.Printf("[project] discovered %d directories for project %q", len(dirs), g.projectID)
	}
	return dirs, nil
}

// DiscoverForContainer returns the workspace host directory for a specific container.
func (g *ProjectDiscovery) DiscoverForContainer(ctx context.Context, containerID string) (string, error) {
	info, err := g.dockerClient.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspecting container: %w", err)
	}

	// Check that the container belongs to our project.
	if projectLabel, ok := info.Config.Labels["scion.grove"]; !ok || projectLabel != g.projectID {
		return "", nil
	}

	for _, mount := range info.Mounts {
		if mount.Type == "bind" && isWorkspaceMount(mount.Destination) {
			return mount.Source, nil
		}
	}
	return "", nil
}

// isWorkspaceMount returns true if the container-side mount destination looks
// like a workspace path. This covers the standard /workspace destination as
// well as /repo-root (used when the full project directory is bind-mounted
// into the container — a mis-detection artifact of git-repo-based projects that
// is not expected to persist long-term).
func isWorkspaceMount(dest string) bool {
	if dest == "/workspace" || dest == "/repo-root" {
		return true
	}
	if strings.HasPrefix(dest, "/repo-root/") && strings.HasSuffix(dest, "/workspace") {
		return true
	}
	return false
}
