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

// Package messaging implements the conversation resolution and drift-state
// logic for Scion's messaging subsystem.
package messaging

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// AgentLookup is the minimal interface needed for agent resolution.
// It exists so normalize.go does not depend on the full store.Store.
type AgentLookup interface {
	GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error)
}

// slugPattern matches valid agent slugs: lowercase alphanumeric with hyphens,
// starting and ending with an alphanumeric character.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// NormalizeAgentRef resolves an agent slug or UUID to a validated UUID string.
// This function exists in ONE place and is used by two callers:
//  1. The resolution layer (this package, Phase 3)
//  2. The backfill job (Phase 4, a different developer)
//
// If ref is already a valid UUID, it is returned as-is.
// If ref is a slug, it requires a store lookup within the given project.
// Returns ErrNotFound if the slug cannot be resolved.
// Returns ErrInvalidInput if ref is neither a valid UUID nor a valid slug format.
func NormalizeAgentRef(ctx context.Context, agentStore AgentLookup, projectID string, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty agent reference: %w", store.ErrInvalidInput)
	}

	// If ref is a valid UUID, return it directly.
	if _, err := uuid.Parse(ref); err == nil {
		return ref, nil
	}

	// Check if it looks like a valid slug.
	if !slugPattern.MatchString(ref) {
		return "", fmt.Errorf("agent reference %q is neither a valid UUID nor a valid slug: %w", ref, store.ErrInvalidInput)
	}

	// Slug lookup requires a project context.
	if projectID == "" {
		return "", fmt.Errorf("project ID required for slug resolution: %w", store.ErrInvalidInput)
	}

	agent, err := agentStore.GetAgentBySlug(ctx, projectID, ref)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("agent slug %q not found in project %s: %w", ref, projectID, store.ErrNotFound)
		}
		return "", fmt.Errorf("looking up agent slug %q: %w", ref, err)
	}

	return agent.ID, nil
}
