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

package hub

import (
	"context"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Boundary capabilities (B6)
// ---------------------------------------------------------------------------

// BoundaryCapabilities tells the client what operations are available on a
// boundary resource given the current actor's permissions and active
// boundaries. Computed using the canonical evaluator (B1) and current state.
type BoundaryCapabilities struct {
	CanCreate    bool     `json:"canCreate"`
	CanUpdate    bool     `json:"canUpdate"`
	CanDelete    bool     `json:"canDelete"`
	CanPreview   bool     `json:"canPreview"`
	IsAdmin      bool     `json:"isAdmin"`
	Restrictions []string `json:"restrictions,omitempty"`
}

// CapabilitiesService computes capabilities for boundary resources.
type CapabilitiesService struct {
	store   store.Store
	authz   *AuthzService
	logger  *slog.Logger
	nowFunc func() time.Time
}

// NewCapabilitiesService creates a new CapabilitiesService.
func NewCapabilitiesService(s store.Store, authz *AuthzService, logger *slog.Logger) *CapabilitiesService {
	return &CapabilitiesService{
		store:   s,
		authz:   authz,
		logger:  logger,
		nowFunc: time.Now,
	}
}

// ComputeCapabilities determines what the current actor can do with boundary
// resources at the given scope. Uses the canonical evaluator (B1) to check
// the actor's effective permissions considering all active boundaries.
func (cs *CapabilitiesService) ComputeCapabilities(ctx context.Context, actor PrincipalContext, scopeType, scopeID string) (*BoundaryCapabilities, error) {
	now := cs.nowFunc()

	// Get the actor's effective permissions at the given scope.
	actorPerms, err := cs.authz.getEffectivePermissions(
		ctx,
		NormalizePrincipalType(string(actor.Kind)),
		actor.ID,
		scopeType, scopeID,
	)
	if err != nil {
		cs.logger.Warn("failed to resolve actor permissions for capabilities",
			"actor_id", actor.ID,
			"scope_type", scopeType,
			"scope_id", scopeID,
			"error", err,
		)
		// Return a zero-capability set on error — fail closed.
		return &BoundaryCapabilities{}, nil
	}

	actorPermSet := make(map[string]struct{}, len(actorPerms))
	for _, p := range actorPerms {
		actorPermSet[p] = struct{}{}
	}

	// Check if the actor is a constraint admin.
	_, isAdmin := actorPermSet[PermissionConstraintAdmin]

	caps := &BoundaryCapabilities{
		IsAdmin:    isAdmin,
		CanCreate:  isAdmin,
		CanUpdate:  isAdmin,
		CanDelete:  isAdmin,
		CanPreview: isAdmin,
	}

	// Determine active restrictions that affect this actor.
	restrictions, err := cs.computeRestrictions(ctx, actor, scopeType, scopeID, now)
	if err != nil {
		cs.logger.Warn("failed to compute restrictions for capabilities",
			"actor_id", actor.ID,
			"error", err,
		)
	} else {
		caps.Restrictions = restrictions
	}

	return caps, nil
}

// ComputeResourceCapabilities determines what the current actor can do with
// a specific boundary resource. This is for per-resource _capabilities.
func (cs *CapabilitiesService) ComputeResourceCapabilities(ctx context.Context, actor PrincipalContext, constraintID string) (*BoundaryCapabilities, error) {
	// Load the constraint to determine its scope.
	constraint, err := cs.store.GetAccessConstraint(ctx, constraintID)
	if err != nil {
		return nil, err
	}

	caps, err := cs.ComputeCapabilities(ctx, actor, constraint.ScopeType, constraint.ScopeID)
	if err != nil {
		return nil, err
	}

	// If the constraint is recovery-disabled, mutation is not allowed.
	if constraint.Disabled {
		caps.CanUpdate = false
		caps.CanDelete = false
		caps.Restrictions = append(caps.Restrictions, "recovery_disabled")
	}

	return caps, nil
}

// computeRestrictions returns the list of restriction descriptions that
// apply to the actor at the given scope.
func (cs *CapabilitiesService) computeRestrictions(ctx context.Context, actor PrincipalContext, scopeType, scopeID string, now time.Time) ([]string, error) {
	// Build the principal closure for constraint matching.
	principalType := NormalizePrincipalType(string(actor.Kind))
	typedClosure := map[string]struct{}{
		principalType + ":" + actor.ID: {},
	}

	// Add group memberships to the closure.
	switch principalType {
	case "user":
		groups, err := cs.store.GetEffectiveGroups(ctx, actor.ID)
		if err == nil {
			for _, gid := range groups {
				typedClosure["group:"+gid] = struct{}{}
			}
		}
	case "agent":
		groups, err := cs.store.GetEffectiveGroupsForAgent(ctx, actor.ID)
		if err == nil {
			for _, gid := range groups {
				typedClosure["group:"+gid] = struct{}{}
			}
		}
	}

	// Load constraints at this scope.
	constraints, err := cs.store.ListConstraintsForScope(ctx, scopeType, scopeID)
	if err != nil {
		return nil, err
	}

	// Also load system-scope constraints (they apply to everything).
	if scopeType != ScopeTypeSystem {
		sysConstraints, err := cs.store.ListConstraintsForScope(ctx, ScopeTypeSystem, "")
		if err == nil {
			constraints = append(constraints, sysConstraints...)
		}
	}

	// Filter to applicable and active constraints.
	var restrictions []string
	for _, sc := range constraints {
		if sc.Disabled {
			continue
		}
		hc := storeToHubAccessConstraint(sc)
		if hc == nil || !hc.IsActive(now) {
			continue
		}
		if !hc.Subject.MatchesPrincipalClosure(typedClosure) {
			continue
		}
		restrictions = append(restrictions, hc.Name)
	}

	return restrictions, nil
}
