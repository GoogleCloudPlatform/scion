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

package telegram

import (
	"context"
	"log/slog"
	"time"
)

// notificationAgentCacheTTL bounds how long a cached agent list is reused
// for notification toggles before it is refreshed as the user.
const notificationAgentCacheTTL = 5 * time.Minute

// notificationEntries is the toggle list offered to one linked user.
type notificationEntries struct {
	// Entries are the per-agent toggles, in group-link order.
	Entries []notificationAgentEntry
	// LinkedProjects counts linked projects the user can read, including
	// ones with no agents.
	LinkedProjects int
	// Readable is the set of project IDs the user can read.
	Readable map[string]bool
}

// has reports whether a toggle for the agent is listed.
func (n *notificationEntries) has(projectID, agentSlug string) bool {
	for _, e := range n.Entries {
		if e.ProjectID == projectID && e.AgentSlug == agentSlug {
			return true
		}
	}
	return false
}

// set updates the Enabled state of a toggle if it is listed.
func (n *notificationEntries) set(projectID, agentSlug string, enabled bool) {
	for i := range n.Entries {
		if n.Entries[i].ProjectID == projectID && n.Entries[i].AgentSlug == agentSlug {
			n.Entries[i].Enabled = enabled
		}
	}
}

// buildNotificationEntries returns the notification toggles for the linked
// user in mapping. Only group-linked projects the user can read (listed as
// that user) are included; their agents come from projectAgentsForUser.
// The error is a hub error (including a stale link) or a store error.
func buildNotificationEntries(ctx context.Context, store Store, hub HubClient, log *slog.Logger, mapping *TelegramUserMapping) (*notificationEntries, error) {
	principal := linkedUserPrincipal(mapping)

	userProjects, err := hub.ListProjectsForUser(ctx, principal)
	if err != nil {
		return nil, err
	}
	result := &notificationEntries{Readable: make(map[string]bool, len(userProjects))}
	for _, p := range userProjects {
		result.Readable[p.ID] = true
	}

	links, err := store.GetAllGroupLinks(ctx)
	if err != nil {
		return nil, err
	}

	prefs, err := store.GetNotificationPrefs(ctx, mapping.TelegramUserID)
	if err != nil {
		return nil, err
	}
	prefMap := make(map[string]bool, len(prefs))
	for _, p := range prefs {
		prefMap[p.ProjectID+":"+p.AgentSlug] = p.Enabled
	}

	seen := make(map[string]bool)
	for _, link := range links {
		if !link.Active || seen[link.ProjectID] || !result.Readable[link.ProjectID] {
			continue
		}
		seen[link.ProjectID] = true
		result.LinkedProjects++

		agents, agentErr := projectAgentsForUser(ctx, store, hub, log, link.ProjectID, principal)
		if agentErr != nil {
			if isStaleLinkError(agentErr) {
				return nil, agentErr
			}
			// A project whose agents the user may not list is left out.
			log.Warn("Failed to list agents for notification prefs", "project_id", link.ProjectID, "error", agentErr)
			continue
		}

		for _, agent := range agents {
			enabled := true
			if val, ok := prefMap[link.ProjectID+":"+agent.Slug]; ok {
				enabled = val
			}
			result.Entries = append(result.Entries, notificationAgentEntry{
				ProjectSlug: link.ProjectSlug,
				ProjectID:   link.ProjectID,
				AgentSlug:   agent.Slug,
				Enabled:     enabled,
			})
		}
	}
	return result, nil
}

// projectAgentsForUser returns agents for a project the user can read,
// using the store cache with a fallback to the hub API, which is called as
// the linked user identified by onBehalfOf. A stale cache is used when the
// hub is unavailable, but not when the hub denies the request.
//
// The cache is kept per user and project, so the user is only served a
// list fetched as themselves.
func projectAgentsForUser(ctx context.Context, store Store, hub HubClient, log *slog.Logger, projectID, onBehalfOf string) ([]AgentInfo, error) {
	cached, err := store.GetProjectAgents(ctx, onBehalfOf, projectID)
	if err != nil {
		log.Warn("Failed to read agent cache", "project_id", projectID, "error", err)
	}
	if cached != nil && time.Since(cached.RefreshedAt) < notificationAgentCacheTTL {
		return cached.Agents, nil
	}

	agents, err := hub.ListAgents(ctx, projectID, onBehalfOf)
	if err != nil {
		if cached != nil && !isForbiddenHubError(err) {
			return cached.Agents, nil
		}
		return nil, err
	}

	if saveErr := store.SaveProjectAgents(ctx, &ProjectAgents{
		User:        onBehalfOf,
		ProjectID:   projectID,
		Agents:      agents,
		RefreshedAt: time.Now(),
	}); saveErr != nil {
		log.Warn("Failed to cache agents", "project_id", projectID, "error", saveErr)
	}
	return agents, nil
}
