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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const (
	// previewTokenTTL is the lifetime of a preview token.
	previewTokenTTL = 5 * time.Minute

	// syncPreviewThreshold is the max affected-principal count for sync preview.
	// Above this, the preview is processed asynchronously.
	syncPreviewThreshold = 1000

	// defaultPreviewPageSize is the default page size for affected principals.
	defaultPreviewPageSize = 50

	// maxPreviewPageSize caps the page size for affected principals.
	maxPreviewPageSize = 200

	// largeBlastRadiusThreshold triggers a warning when exceeded.
	largeBlastRadiusThreshold = 100
)

// ---------------------------------------------------------------------------
// PreviewService
// ---------------------------------------------------------------------------

// PreviewService computes impact analyses for boundary mutations and issues
// single-use preview tokens that must be presented at commit time.
type PreviewService struct {
	store  store.Store
	authz  *AuthzService
	logger *slog.Logger

	// hmacKey is the server-side secret for signing preview tokens.
	hmacKey []byte

	// usedNonces tracks consumed preview-token nonces for replay prevention.
	// Keys are nonce strings; values are expiry times for TTL cleanup.
	usedNonces sync.Map

	// asyncJobs tracks in-progress async preview jobs.
	asyncJobs sync.Map

	// nowFunc is injectable for testing. Defaults to time.Now.
	nowFunc func() time.Time

	// stopCleanup signals the nonce cleanup goroutine to shut down.
	stopCleanup chan struct{}

	// closeOnce ensures Close() is idempotent.
	closeOnce sync.Once
}

// NewPreviewService creates a new PreviewService.
func NewPreviewService(s store.Store, authz *AuthzService, logger *slog.Logger) *PreviewService {
	// Generate a random HMAC key.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("crypto/rand failure: cannot produce secure HMAC key: " + err.Error())
	}
	ps := &PreviewService{
		store:       s,
		authz:       authz,
		logger:      logger,
		hmacKey:     key,
		nowFunc:     time.Now,
		stopCleanup: make(chan struct{}),
	}
	// Start nonce cleanup goroutine.
	go ps.cleanupNonces()
	return ps
}

// NewPreviewServiceWithKey creates a PreviewService with a specific HMAC key.
// Used for testing.
func NewPreviewServiceWithKey(s store.Store, authz *AuthzService, logger *slog.Logger, key []byte) *PreviewService {
	return &PreviewService{
		store:   s,
		authz:   authz,
		logger:  logger,
		hmacKey: key,
		nowFunc: time.Now,
	}
}

// ---------------------------------------------------------------------------
// GeneratePreview — main entry point
// ---------------------------------------------------------------------------

// GeneratePreview computes a full impact analysis for the proposed mutation
// and returns a time-limited, single-use preview token.
func (ps *PreviewService) GeneratePreview(ctx context.Context, req PreviewRequest) (*PreviewResult, error) {
	now := ps.nowFunc()

	// Validate request.
	if err := ps.validateRequest(req); err != nil {
		return nil, fmt.Errorf("invalid preview request: %w", err)
	}

	// Load current state.
	var existing *AccessConstraint
	if req.ConstraintID != "" {
		sc, err := ps.store.GetAccessConstraint(ctx, req.ConstraintID)
		if err != nil {
			return nil, fmt.Errorf("failed to load constraint %s: %w", req.ConstraintID, err)
		}
		existing = storeToHubAccessConstraint(sc)
		if req.BaseRevision != 0 && existing.Revision != req.BaseRevision {
			return nil, &TokenValidationError{
				Code:    ErrCodePreviewRevisionMismatch,
				Message: fmt.Sprintf("constraint revision %d does not match expected %d", existing.Revision, req.BaseRevision),
			}
		}
	}

	// Build proposed hub constraint from draft.
	var proposed *AccessConstraint
	if req.Draft != nil {
		proposed = storeToHubAccessConstraint(req.Draft)
		if req.ConstraintID != "" {
			proposed.ID = req.ConstraintID
		}
	}

	// Compute draft hash.
	draftHash := ps.computeDraftHash(req.Draft)

	// Compute impact analysis.
	completeness := PreviewCompleteness{
		Complete: true,
		Reasons:  []IncompletenessReason{},
	}

	// Resolve affected principals.
	affected, compIssues := ps.resolveAffectedPrincipals(ctx, req, existing, proposed, now)
	for _, issue := range compIssues {
		completeness.Reasons = append(completeness.Reasons, issue)
		switch issue.Code {
		case IncompletenessCodeSubjectTooLarge:
			completeness.Truncated = true
			completeness.Complete = false
		case IncompletenessCodeMembershipFailed, IncompletenessCodePermissionFailed:
			completeness.Degraded = true
			completeness.Complete = false
		case IncompletenessCodeTimeBudget:
			completeness.Truncated = true
			completeness.Complete = false
		}
	}

	// Compute per-principal impact diffs.
	impactedPrincipals, permDiffs := ps.computeImpact(ctx, req.Operation, existing, proposed, affected, now, &completeness)

	// Compute classification across all temporal states.
	temporalStates := ps.computeTemporalStates(req.Operation, existing, proposed, impactedPrincipals, now)
	classification := ps.classifyMutation(impactedPrincipals, temporalStates)

	// Build aggregate impact.
	impact := ps.buildImpact(impactedPrincipals, permDiffs, &completeness)

	// Compute lockout assessment.
	lockout := ps.assessLockout(ctx, req, existing, proposed, now)

	// Find intersecting boundaries.
	intersecting := ps.findIntersecting(ctx, req, existing, proposed, now)

	// Generate warnings.
	warnings := ps.generateWarnings(classification, impact, temporalStates, completeness, lockout, intersecting)

	// Check if commit would be blocked.
	var commitBlocked *CommitBlockedReason
	if !completeness.Complete {
		commitBlocked = &CommitBlockedReason{
			Code:    ErrCodePreviewIncomplete,
			Message: "preview is incomplete and cannot be committed",
		}
	}
	if lockout.Safe == nil || !*lockout.Safe {
		code := ErrCodeConstraintAdminLockout
		msg := "mutation would lock out all constraint admins"
		if lockout.Safe == nil {
			code = ErrCodePreviewIncomplete
			msg = "lockout assessment could not be determined: " + lockout.UndeterminedReason
		}
		commitBlocked = &CommitBlockedReason{
			Code:    code,
			Message: msg,
		}
	}

	// Build affected principals page.
	page := ps.buildAffectedPage(impactedPrincipals, 0, defaultPreviewPageSize, &completeness)

	// Compute state fingerprint for token binding.
	stateFingerprint := ps.computeStateFingerprint(ctx, req, now)

	// Generate preview ID and token.
	previewID := ps.generatePreviewID()

	token, err := ps.issueToken(previewID, req.ConstraintID, req.Actor, req.Operation, draftHash,
		req.BaseRevision, stateFingerprint, completeness.Complete, now)
	if err != nil {
		return nil, fmt.Errorf("failed to issue preview token: %w", err)
	}

	result := &PreviewResult{
		PreviewID:      previewID,
		PreviewToken:   token,
		GeneratedAt:    now,
		ExpiresAt:      now.Add(previewTokenTTL),
		Operation:      req.Operation,
		ConstraintID:   req.ConstraintID,
		BaseRevision:   req.BaseRevision,
		DraftHash:      draftHash,
		Classification: classification,
		Completeness:   completeness,
		Lockout:        lockout,
		Impact:         impact,
		TemporalStates: temporalStates,
		AffectedPage:   page,
		Intersecting:   intersecting,
		Warnings:       warnings,
		CommitBlocked:  commitBlocked,
		allImpacted:    impactedPrincipals,
	}

	// Store sync preview for pagination support.
	ps.asyncJobs.Store(previewID, &PreviewJob{
		JobID:       previewID,
		Status:      JobStatusSucceeded,
		Operation:   req.Operation,
		Result:      result,
		allImpacted: impactedPrincipals,
	})

	return result, nil
}

// ---------------------------------------------------------------------------
// Request validation
// ---------------------------------------------------------------------------

func (ps *PreviewService) validateRequest(req PreviewRequest) error {
	switch req.Operation {
	case "create":
		if req.Draft == nil {
			return fmt.Errorf("draft is required for create operation")
		}
		if req.ConstraintID != "" {
			return fmt.Errorf("constraintID must be empty for create operation")
		}
	case "update":
		if req.Draft == nil {
			return fmt.Errorf("draft is required for update operation")
		}
		if req.ConstraintID == "" {
			return fmt.Errorf("constraintID is required for update operation")
		}
	case "delete":
		if req.ConstraintID == "" {
			return fmt.Errorf("constraintID is required for delete operation")
		}
	default:
		return fmt.Errorf("invalid operation %q: must be create, update, or delete", req.Operation)
	}
	if req.Actor.ID == "" {
		return fmt.Errorf("actor principal is required")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Draft hashing
// ---------------------------------------------------------------------------

// computeDraftHash computes a SHA-256 hash of the canonicalized draft JSON.
// Keys are sorted and maximumPermissions are sorted for deterministic output.
func (ps *PreviewService) computeDraftHash(draft *store.AccessConstraint) string {
	if draft == nil {
		return "null"
	}

	// Build a canonical representation with sorted keys.
	canonical := map[string]interface{}{
		"disabled":           draft.Disabled,
		"maximumPermissions": sortedCopy(draft.MaximumPermissions),
		"name":               draft.Name,
		"scopeId":            draft.ScopeID,
		"scopeType":          draft.ScopeType,
		"subjectKind":        draft.SubjectKind,
	}
	if draft.SubjectPrincipalType != nil {
		canonical["subjectPrincipalType"] = *draft.SubjectPrincipalType
	}
	if draft.SubjectPrincipalID != nil {
		canonical["subjectPrincipalId"] = *draft.SubjectPrincipalID
	}
	if draft.SubjectGroupID != nil {
		canonical["subjectGroupId"] = *draft.SubjectGroupID
	}
	if draft.NotBefore != nil {
		canonical["notBefore"] = draft.NotBefore.UTC().Format(time.RFC3339Nano)
	}
	if draft.ExpiresAt != nil {
		canonical["expiresAt"] = draft.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if draft.Purpose != "" {
		canonical["purpose"] = draft.Purpose
	}

	data, err := json.Marshal(canonical)
	if err != nil {
		ps.logger.Error("failed to marshal draft for hashing", "error", err)
		return "error"
	}

	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// sortedCopy returns a sorted copy of a string slice.
func sortedCopy(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	sort.Strings(c)
	return c
}

// ---------------------------------------------------------------------------
// Affected principal resolution
// ---------------------------------------------------------------------------

// resolveAffectedPrincipals resolves all principals affected by the mutation.
// For the current and proposed constraint, it finds all principals that fall
// within the subject selector's scope.
func (ps *PreviewService) resolveAffectedPrincipals(
	ctx context.Context,
	req PreviewRequest,
	existing, proposed *AccessConstraint,
	now time.Time,
) ([]resolvedPrincipal, []IncompletenessReason) {
	var issues []IncompletenessReason

	// Determine which subject selectors to consider.
	// For update: both current and proposed subjects may differ.
	// For create: only proposed subject.
	// For delete: only existing subject.
	var selectors []SubjectSelector
	var scopeRefs []ConstraintScopeRef
	switch req.Operation {
	case "create":
		selectors = append(selectors, proposed.Subject)
		scopeRefs = append(scopeRefs, proposed.Scope)
	case "update":
		selectors = append(selectors, existing.Subject)
		scopeRefs = append(scopeRefs, existing.Scope)
		if proposed.Subject != existing.Subject {
			selectors = append(selectors, proposed.Subject)
			scopeRefs = append(scopeRefs, proposed.Scope)
		}
	case "delete":
		selectors = append(selectors, existing.Subject)
		scopeRefs = append(scopeRefs, existing.Scope)
	}

	// Resolve principals for each unique selector.
	seen := make(map[string]bool)
	var result []resolvedPrincipal

	for i, sel := range selectors {
		scope := scopeRefs[i]
		principals, issue := ps.resolvePrincipalsForSelector(ctx, sel, scope)
		if issue != nil {
			issues = append(issues, *issue)
		}
		for _, p := range principals {
			key := p.principalType + ":" + p.principalID
			if !seen[key] {
				seen[key] = true
				result = append(result, p)
			}
		}
	}

	// Check threshold for async.
	if len(result) > syncPreviewThreshold {
		issues = append(issues, IncompletenessReason{
			Code:    IncompletenessCodeSubjectTooLarge,
			Message: fmt.Sprintf("affected principal count (%d) exceeds synchronous threshold (%d)", len(result), syncPreviewThreshold),
			Details: map[string]interface{}{
				"count":     len(result),
				"threshold": syncPreviewThreshold,
			},
		})
	}

	return result, issues
}

// resolvedPrincipal is an internal representation of an affected principal
// with its membership paths.
type resolvedPrincipal struct {
	principalType   string
	principalID     string
	displayName     string
	membershipPaths []MembershipPath
	groupIDs        []string // effective group IDs for constraint filtering
}

// resolvePrincipalsForSelector resolves principals for a single subject selector.
func (ps *PreviewService) resolvePrincipalsForSelector(
	ctx context.Context,
	sel SubjectSelector,
	scope ConstraintScopeRef,
) ([]resolvedPrincipal, *IncompletenessReason) {
	switch sel.Kind {
	case SubjectKindPrincipal:
		return ps.resolveSinglePrincipal(ctx, sel)

	case SubjectKindGroupClosure:
		return ps.resolveGroupClosure(ctx, sel.GroupID)

	case SubjectKindAllPrincipals:
		return ps.resolveAllPrincipals(ctx, scope)

	default:
		return nil, &IncompletenessReason{
			Code:    IncompletenessCodeMembershipFailed,
			Message: fmt.Sprintf("unknown subject kind %q", sel.Kind),
		}
	}
}

// resolveSinglePrincipal resolves a single principal subject.
func (ps *PreviewService) resolveSinglePrincipal(
	ctx context.Context,
	sel SubjectSelector,
) ([]resolvedPrincipal, *IncompletenessReason) {
	p := resolvedPrincipal{
		principalType: sel.PrincipalType,
		principalID:   sel.PrincipalID,
		membershipPaths: []MembershipPath{{
			Hops: []PrincipalRef{{
				Type: sel.PrincipalType,
				ID:   sel.PrincipalID,
			}},
			Direct: true,
		}},
	}

	// Resolve display name.
	p.displayName = ps.resolveDisplayName(ctx, sel.PrincipalType, sel.PrincipalID)

	// Resolve group membership for constraint filtering.
	switch sel.PrincipalType {
	case "user":
		groups, err := ps.store.GetEffectiveGroups(ctx, sel.PrincipalID)
		if err != nil {
			ps.logger.Warn("failed to resolve groups for principal",
				"type", sel.PrincipalType, "id", sel.PrincipalID, "error", err)
		} else {
			p.groupIDs = groups
		}
	case "agent":
		groups, err := ps.store.GetEffectiveGroupsForAgent(ctx, sel.PrincipalID)
		if err != nil {
			ps.logger.Warn("failed to resolve groups for agent",
				"id", sel.PrincipalID, "error", err)
		} else {
			p.groupIDs = groups
		}
	}

	return []resolvedPrincipal{p}, nil
}

// resolveGroupClosure resolves all effective members of a group.
func (ps *PreviewService) resolveGroupClosure(
	ctx context.Context,
	groupID string,
) ([]resolvedPrincipal, *IncompletenessReason) {
	var result []resolvedPrincipal

	members, err := ps.store.GetGroupMembers(ctx, groupID)
	if err != nil {
		return nil, &IncompletenessReason{
			Code:    IncompletenessCodeMembershipFailed,
			Message: fmt.Sprintf("failed to resolve members of group %s: %v", groupID, err),
		}
	}

	seen := make(map[string]bool)
	// Process direct members.
	for _, m := range members {
		key := m.MemberType + ":" + m.MemberID
		if seen[key] {
			continue
		}
		seen[key] = true

		if m.MemberType == store.GroupMemberTypeGroup {
			// Recurse into nested groups.
			nested, issue := ps.resolveGroupClosure(ctx, m.MemberID)
			if issue != nil {
				// Continue with partial results.
				ps.logger.Warn("partial group resolution", "group", m.MemberID, "error", issue.Message)
			}
			for _, np := range nested {
				npKey := np.principalType + ":" + np.principalID
				if !seen[npKey] {
					seen[npKey] = true
					// Prepend the current group hop.
					for i := range np.membershipPaths {
						np.membershipPaths[i].Hops = append(
							[]PrincipalRef{{Type: "group", ID: groupID}},
							np.membershipPaths[i].Hops...,
						)
						np.membershipPaths[i].Direct = false
					}
					result = append(result, np)
				}
			}
			continue
		}

		p := resolvedPrincipal{
			principalType: m.MemberType,
			principalID:   m.MemberID,
			displayName:   ps.resolveDisplayName(ctx, m.MemberType, m.MemberID),
			membershipPaths: []MembershipPath{{
				Hops: []PrincipalRef{
					{Type: m.MemberType, ID: m.MemberID},
					{Type: "group", ID: groupID},
				},
				Direct: true,
			}},
		}

		// Resolve groups for filtering.
		switch m.MemberType {
		case "user":
			groups, _ := ps.store.GetEffectiveGroups(ctx, m.MemberID)
			p.groupIDs = groups
		case "agent":
			groups, _ := ps.store.GetEffectiveGroupsForAgent(ctx, m.MemberID)
			p.groupIDs = groups
		}

		result = append(result, p)
	}

	return result, nil
}

// resolveAllPrincipals resolves "all_principals" — returns resolved
// users and agents in the scope for actual impact computation.
func (ps *PreviewService) resolveAllPrincipals(
	ctx context.Context,
	scope ConstraintScopeRef,
) ([]resolvedPrincipal, *IncompletenessReason) {
	var result []resolvedPrincipal

	// Load users. For system scope, load all users. For project scope,
	// load project members.
	if scope.Type == ScopeTypeProject && scope.ID != "" {
		members, err := ps.store.ListProjectMembers(ctx, scope.ID)
		if err != nil {
			return nil, &IncompletenessReason{
				Code:    IncompletenessCodeMembershipFailed,
				Message: fmt.Sprintf("failed to list project members: %v", err),
			}
		}
		for _, m := range members {
			p := resolvedPrincipal{
				principalType: "user",
				principalID:   m.UserID,
				membershipPaths: []MembershipPath{{
					Hops:   []PrincipalRef{{Type: "all", ID: "*"}},
					Direct: true,
				}},
			}
			groups, _ := ps.store.GetEffectiveGroups(ctx, m.UserID)
			p.groupIDs = groups
			result = append(result, p)
		}

		// Also resolve project agents.
		agents, err := ps.store.ListAgents(ctx, store.AgentFilter{ProjectID: scope.ID}, store.ListOptions{Limit: syncPreviewThreshold + 1})
		if err != nil {
			// Agent resolution failure degrades completeness — same as user failures.
			return result, &IncompletenessReason{
				Code:    IncompletenessCodeMembershipFailed,
				Message: fmt.Sprintf("failed to list project agents: %v", err),
			}
		}
		if agents != nil {
			for _, a := range agents.Items {
				p := resolvedPrincipal{
					principalType: "agent",
					principalID:   a.ID,
					displayName:   a.Name,
					membershipPaths: []MembershipPath{{
						Hops:   []PrincipalRef{{Type: "all", ID: "*"}},
						Direct: true,
					}},
				}
				groups, _ := ps.store.GetEffectiveGroupsForAgent(ctx, a.ID)
				p.groupIDs = groups
				result = append(result, p)
			}
		}
	} else {
		// System scope: list all users.
		users, err := ps.store.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: syncPreviewThreshold + 1})
		if err != nil {
			return nil, &IncompletenessReason{
				Code:    IncompletenessCodeMembershipFailed,
				Message: fmt.Sprintf("failed to list users: %v", err),
			}
		}
		if users != nil {
			for _, u := range users.Items {
				p := resolvedPrincipal{
					principalType: "user",
					principalID:   u.ID,
					displayName:   u.DisplayName,
					membershipPaths: []MembershipPath{{
						Hops:   []PrincipalRef{{Type: "all", ID: "*"}},
						Direct: true,
					}},
				}
				groups, _ := ps.store.GetEffectiveGroups(ctx, u.ID)
				p.groupIDs = groups
				result = append(result, p)
			}
		}

		// System scope: also list all agents.
		agents, err := ps.store.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: syncPreviewThreshold + 1})
		if err != nil {
			// Agent resolution failure degrades completeness — same as user failures.
			return result, &IncompletenessReason{
				Code:    IncompletenessCodeMembershipFailed,
				Message: fmt.Sprintf("failed to list agents: %v", err),
			}
		}
		if agents != nil {
			for _, a := range agents.Items {
				p := resolvedPrincipal{
					principalType: "agent",
					principalID:   a.ID,
					displayName:   a.Name,
					membershipPaths: []MembershipPath{{
						Hops:   []PrincipalRef{{Type: "all", ID: "*"}},
						Direct: true,
					}},
				}
				groups, _ := ps.store.GetEffectiveGroupsForAgent(ctx, a.ID)
				p.groupIDs = groups
				result = append(result, p)
			}
		}
	}

	return result, nil
}

// resolveDisplayName resolves a human-readable name for a principal.
func (ps *PreviewService) resolveDisplayName(ctx context.Context, principalType, principalID string) string {
	switch principalType {
	case "user":
		u, err := ps.store.GetUser(ctx, principalID)
		if err == nil && u != nil {
			return u.DisplayName
		}
	case "agent":
		a, err := ps.store.GetAgent(ctx, principalID)
		if err == nil && a != nil {
			return a.Name
		}
	case "group":
		g, err := ps.store.GetGroup(ctx, principalID)
		if err == nil && g != nil {
			return g.Name
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Impact computation
// ---------------------------------------------------------------------------

// computeImpact computes per-principal impact diffs between current and
// proposed states.
func (ps *PreviewService) computeImpact(
	ctx context.Context,
	operation string,
	existing, proposed *AccessConstraint,
	affected []resolvedPrincipal,
	now time.Time,
	completeness *PreviewCompleteness,
) ([]ImpactedPrincipal, map[string]*PermissionImpact) {
	var results []ImpactedPrincipal
	permDiffs := make(map[string]*PermissionImpact)

	for _, ap := range affected {
		ip, err := ps.computePrincipalImpact(ctx, operation, existing, proposed, ap, now)
		if err != nil {
			ps.logger.Warn("failed to compute impact for principal",
				"type", ap.principalType, "id", ap.principalID, "error", err)
			completeness.Degraded = true
			completeness.Complete = false
			completeness.Reasons = append(completeness.Reasons, IncompletenessReason{
				Code:    IncompletenessCodePermissionFailed,
				Message: fmt.Sprintf("failed to resolve permissions for %s:%s: %v", ap.principalType, ap.principalID, err),
			})
			continue
		}

		// Accumulate per-permission diffs.
		for _, permID := range ip.RemovedPermissions {
			if pd, ok := permDiffs[permID]; ok {
				pd.LosingCount++
			} else {
				permDiffs[permID] = &PermissionImpact{
					PermissionID: permID,
					LosingCount:  1,
				}
			}
		}
		for _, permID := range ip.RegainedPermissions {
			if pd, ok := permDiffs[permID]; ok {
				pd.RegainingCount++
			} else {
				permDiffs[permID] = &PermissionImpact{
					PermissionID:   permID,
					RegainingCount: 1,
				}
			}
		}

		results = append(results, ip)
	}

	return results, permDiffs
}

// computePrincipalImpact computes the impact for a single principal.
func (ps *PreviewService) computePrincipalImpact(
	ctx context.Context,
	operation string,
	existing, proposed *AccessConstraint,
	principal resolvedPrincipal,
	now time.Time,
) (ImpactedPrincipal, error) {
	scopeType := ScopeTypeSystem
	scopeID := ""
	if existing != nil {
		scopeType = existing.Scope.Type
		scopeID = existing.Scope.ID
	} else if proposed != nil {
		scopeType = proposed.Scope.Type
		scopeID = proposed.Scope.ID
	}

	// Get current effective permissions (with all existing constraints applied).
	currentPerms, err := ps.authz.getEffectivePermissions(
		ctx, principal.principalType, principal.principalID,
		scopeType, scopeID,
	)
	if err != nil {
		return ImpactedPrincipal{}, fmt.Errorf("current permissions: %w", err)
	}

	// Get proposed effective permissions.
	// For this, we need to simulate the constraint set with the proposed change.
	proposedPerms, err := ps.computeProposedPermissions(
		ctx, operation, existing, proposed, principal, scopeType, scopeID, now,
	)
	if err != nil {
		return ImpactedPrincipal{}, fmt.Errorf("proposed permissions: %w", err)
	}

	// Compute diffs.
	currentSet := toSet(currentPerms)
	proposedSet := toSet(proposedPerms)

	var removed, regained []string
	for p := range currentSet {
		if _, ok := proposedSet[p]; !ok {
			removed = append(removed, p)
		}
	}
	for p := range proposedSet {
		if _, ok := currentSet[p]; !ok {
			regained = append(regained, p)
		}
	}
	sort.Strings(removed)
	sort.Strings(regained)

	changeKind := "no_effect"
	if len(removed) > 0 && len(regained) > 0 {
		changeKind = "mixed"
	} else if len(removed) > 0 {
		changeKind = "loses"
	} else if len(regained) > 0 {
		changeKind = "regains"
	}

	ip := ImpactedPrincipal{
		PrincipalType:           principal.principalType,
		PrincipalID:             principal.principalID,
		DisplayName:             principal.displayName,
		ChangeKind:              changeKind,
		MembershipPaths:         principal.membershipPaths,
		CurrentPermissionCount:  len(currentPerms),
		ProposedPermissionCount: len(proposedPerms),
		RemovedPermissions:      removed,
		RegainedPermissions:     regained,
		RemovedPermissionCount:  len(removed),
	}

	return ip, nil
}

// computeProposedPermissions computes what permissions a principal would
// have after the proposed mutation. It simulates the constraint set change
// in-memory rather than persisting.
func (ps *PreviewService) computeProposedPermissions(
	ctx context.Context,
	operation string,
	existing, proposed *AccessConstraint,
	principal resolvedPrincipal,
	scopeType, scopeID string,
	now time.Time,
) ([]string, error) {
	// Get the base permissions before any constraints (from role bindings only).
	basePerms, err := ps.getBasePermissions(ctx, principal, scopeType, scopeID, now)
	if err != nil {
		return nil, err
	}

	// Load all constraints.
	allStoreConstraints, err := ps.authz.loadAllAccessConstraints(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load constraints: %w", err)
	}

	// Convert to hub constraints and simulate the mutation.
	var hubConstraints []*AccessConstraint
	for _, sc := range allStoreConstraints {
		hc := storeToHubAccessConstraint(sc)
		if hc == nil {
			continue
		}
		// For update/delete: skip the existing constraint (we'll replace/remove it).
		if existing != nil && hc.ID == existing.ID {
			continue
		}
		hubConstraints = append(hubConstraints, hc)
	}

	// For create/update: add the proposed constraint.
	if operation != "delete" && proposed != nil {
		hubConstraints = append(hubConstraints, proposed)
	}

	// Build typed principal closure.
	closure := make(map[string]struct{})
	normalizedType := NormalizePrincipalType(principal.principalType)
	closure[normalizedType+":"+principal.principalID] = struct{}{}
	for _, gid := range principal.groupIDs {
		closure["group:"+gid] = struct{}{}
	}

	// Filter to applicable constraints.
	applicable := FilterApplicableConstraints(hubConstraints, closure, scopeType, scopeID)

	// Apply constraints as restrictions.
	restrictions := ConstraintsToRestrictions(applicable, now)

	// Filter base permissions through restrictions.
	if len(restrictions) == 0 {
		return basePerms, nil
	}

	var filtered []string
	for _, permID := range basePerms {
		blocked := false
		for _, r := range restrictions {
			if r.Check == nil || !r.Check(permID) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, permID)
		}
	}

	return filtered, nil
}

// getBasePermissions gets permissions from role bindings without any constraint
// restrictions applied.
func (ps *PreviewService) getBasePermissions(
	ctx context.Context,
	principal resolvedPrincipal,
	scopeType, scopeID string,
	now time.Time,
) ([]string, error) {
	normalizedType := NormalizePrincipalType(principal.principalType)

	// Build principals: direct principal + group-expanded.
	principals := []store.PrincipalRef{{Type: normalizedType, ID: principal.principalID}}
	for _, gid := range principal.groupIDs {
		principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
	}

	// Load role bindings.
	bindings, err := ps.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		return nil, err
	}

	// Resolve permissions from bindings.
	seen := make(map[string]bool)
	var result []string
	for _, b := range bindings {
		// Filter by scope.
		if b.ScopeType == store.RoleScopeProject {
			if scopeType != store.RoleScopeProject || b.ScopeID != scopeID {
				continue
			}
		}
		// Check activation.
		cb := &CandidateBinding{BindingID: b.ID}
		if b.NotBefore != nil {
			cb.NotBefore = *b.NotBefore
		}
		if b.ExpiresAt != nil {
			cb.ExpiresAt = *b.ExpiresAt
		}
		activation := evaluateActivation(cb, now)
		if !activation.Active {
			continue
		}

		rd, err := ps.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			ps.logger.Warn("failed to resolve role definition",
				"binding_id", b.ID, "role_definition_id", b.RoleDefinitionID, "error", err)
			continue
		}
		for _, permID := range rd.Permissions {
			if !seen[permID] {
				seen[permID] = true
				result = append(result, permID)
			}
		}
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Classification
// ---------------------------------------------------------------------------

// classifyMutation determines the overall classification from affected
// principals and temporal states.
func (ps *PreviewService) classifyMutation(
	impacted []ImpactedPrincipal,
	temporalStates []TemporalImpact,
) string {
	hasLosing := false
	hasRegaining := false

	for _, ip := range impacted {
		switch ip.ChangeKind {
		case "loses":
			hasLosing = true
		case "regains":
			hasRegaining = true
		case "mixed":
			hasLosing = true
			hasRegaining = true
		}
	}

	// Classification spans all temporal states.
	for _, ts := range temporalStates {
		switch ts.Classification {
		case ClassificationTighten:
			hasLosing = true
		case ClassificationRelax:
			hasRegaining = true
		case ClassificationMixed:
			hasLosing = true
			hasRegaining = true
		}
	}

	if hasLosing && hasRegaining {
		return ClassificationMixed
	}
	if hasLosing {
		return ClassificationTighten
	}
	if hasRegaining {
		return ClassificationRelax
	}
	// No effect is a display-only subtype. On the wire it's "relax".
	return ClassificationRelax
}

// ---------------------------------------------------------------------------
// Temporal impact
// ---------------------------------------------------------------------------

// computeTemporalStates computes the temporal impact for time-bounded constraints.
func (ps *PreviewService) computeTemporalStates(
	operation string,
	existing, proposed *AccessConstraint,
	impacted []ImpactedPrincipal,
	now time.Time,
) []TemporalImpact {
	var states []TemporalImpact

	// Determine the relevant constraint for time analysis.
	var constraint *AccessConstraint
	switch operation {
	case "create", "update":
		constraint = proposed
	case "delete":
		constraint = existing
	}
	if constraint == nil {
		return states
	}

	notBefore := constraint.Condition.NotBefore
	expiresAt := constraint.Condition.ExpiresAt

	// If no time bounds, single "now → indefinite" state.
	if notBefore.IsZero() && expiresAt.IsZero() {
		// The impacted principals already reflect the mutation effect
		// (current vs proposed state), so classifyFromImpacted already
		// gives the correct classification. No inversion needed.
		classification := classifyFromImpacted(impacted)
		states = append(states, TemporalImpact{
			From:                   now,
			Label:                  "effective immediately",
			Classification:         classification,
			AffectedPrincipalCount: len(impacted),
			RemovedPermissionCount: countRemovedPermissions(impacted),
		})
		return states
	}

	// Before activation (if notBefore is in the future).
	if !notBefore.IsZero() && now.Before(notBefore) {
		states = append(states, TemporalImpact{
			From:                   now,
			Until:                  &notBefore,
			Label:                  "before activation",
			Classification:         ClassificationRelax, // no change yet
			AffectedPrincipalCount: 0,
			RemovedPermissionCount: 0,
			Note:                   "constraint not yet active",
		})
	}

	// Active period.
	activeFrom := now
	if !notBefore.IsZero() && now.Before(notBefore) {
		activeFrom = notBefore
	}
	var activeUntil *time.Time
	if !expiresAt.IsZero() {
		activeUntil = &expiresAt
	}

	// The impacted principals already reflect the mutation effect
	// (current vs proposed state), so the active-period classification
	// is directly from the impacted set.
	activeClassification := classifyFromImpacted(impacted)
	states = append(states, TemporalImpact{
		From:                   activeFrom,
		Until:                  activeUntil,
		Label:                  "active period",
		Classification:         activeClassification,
		AffectedPrincipalCount: len(impacted),
		RemovedPermissionCount: countRemovedPermissions(impacted),
	})

	// After expiry: the temporal transition reverses the active-period effect.
	if !expiresAt.IsZero() {
		expiryClassification := invertClassification(activeClassification)
		states = append(states, TemporalImpact{
			From:                   expiresAt,
			Label:                  "after expiry",
			Classification:         expiryClassification,
			AffectedPrincipalCount: len(impacted),
			RemovedPermissionCount: 0,
			Note:                   "constraint expired, permissions restored",
		})
	}

	return states
}

func classifyFromImpacted(impacted []ImpactedPrincipal) string {
	hasLosing := false
	hasRegaining := false
	for _, ip := range impacted {
		switch ip.ChangeKind {
		case "loses":
			hasLosing = true
		case "regains":
			hasRegaining = true
		case "mixed":
			return ClassificationMixed
		}
	}
	if hasLosing && hasRegaining {
		return ClassificationMixed
	}
	if hasLosing {
		return ClassificationTighten
	}
	if hasRegaining {
		return ClassificationRelax
	}
	return ClassificationRelax
}

func invertClassification(c string) string {
	switch c {
	case ClassificationTighten:
		return ClassificationRelax
	case ClassificationRelax:
		return ClassificationTighten
	default:
		return c
	}
}

func countRemovedPermissions(impacted []ImpactedPrincipal) int {
	total := 0
	for _, ip := range impacted {
		total += ip.RemovedPermissionCount
	}
	return total
}

// ---------------------------------------------------------------------------
// Impact aggregation
// ---------------------------------------------------------------------------

// buildImpact aggregates per-principal impacts into the BoundaryImpact summary.
func (ps *PreviewService) buildImpact(
	impacted []ImpactedPrincipal,
	permDiffs map[string]*PermissionImpact,
	completeness *PreviewCompleteness,
) BoundaryImpact {
	losing := 0
	regaining := 0
	noEffect := 0
	totalCurrentPerms := 0
	totalProposedPerms := 0

	for _, ip := range impacted {
		switch ip.ChangeKind {
		case "loses":
			losing++
		case "regains":
			regaining++
		case "mixed":
			losing++
			regaining++
		case "no_effect":
			noEffect++
		}
		totalCurrentPerms += ip.CurrentPermissionCount
		totalProposedPerms += ip.ProposedPermissionCount
	}

	// Build sorted permission diffs.
	var diffs []PermissionImpact
	for _, d := range permDiffs {
		diffs = append(diffs, *d)
	}
	sort.Slice(diffs, func(i, j int) bool {
		return diffs[i].PermissionID < diffs[j].PermissionID
	})

	return BoundaryImpact{
		AffectedPrincipalCount:      len(impacted),
		AffectedPrincipalCountExact: completeness.Complete && !completeness.Truncated,
		LosingPrincipalCount:        losing,
		RegainingPrincipalCount:     regaining,
		NoEffectPrincipalCount:      noEffect,
		PermissionDiffs:             diffs,
		PermissionDiffsTruncated:    completeness.Truncated,
		Current: PermissionSummary{
			EffectivePermissionCount: totalCurrentPerms,
		},
		Proposed: PermissionSummary{
			EffectivePermissionCount: totalProposedPerms,
		},
	}
}

// ---------------------------------------------------------------------------
// Lockout assessment
// ---------------------------------------------------------------------------

// assessLockout evaluates whether the proposed mutation would lock out all
// constraint admins. Zero resolved admins = UNSAFE (never degraded pass).
func (ps *PreviewService) assessLockout(
	ctx context.Context,
	req PreviewRequest,
	existing, proposed *AccessConstraint,
	now time.Time,
) LockoutAssessment {
	checkedPerms := []string{PermissionConstraintAdmin}

	// For delete: removing a constraint can only relax. Safe.
	if req.Operation == "delete" {
		safe := true
		return LockoutAssessment{
			Safe:                          &safe,
			CheckedPermissionIDs:          checkedPerms,
			ActorRetainsMutationAuthority: &safe,
		}
	}

	scopeType := ScopeTypeSystem
	scopeID := ""
	if proposed != nil {
		scopeType = proposed.Scope.Type
		scopeID = proposed.Scope.ID
	}

	// Build a proposed store constraint for the lockout check.
	proposedStore := ps.hubToStoreConstraint(proposed)

	// Resolve admin users.
	adminUsers, err := ps.resolveAdminUsers(ctx, scopeType, scopeID)
	if err != nil {
		ps.logger.Warn("lockout check: failed to resolve admin users", "error", err)
		return LockoutAssessment{
			Safe:                 nil,
			CheckedPermissionIDs: checkedPerms,
			UndeterminedReason:   fmt.Sprintf("failed to resolve admin users: %v", err),
		}
	}

	// Zero resolved admins = UNSAFE, not degraded pass.
	if len(adminUsers) == 0 {
		safe := false
		zero := 0
		return LockoutAssessment{
			Safe:                          &safe,
			RemainingActiveDirectAdmins:   &zero,
			CheckedPermissionIDs:          checkedPerms,
			ActorRetainsMutationAuthority: previewBoolPtr(false),
		}
	}

	// Load all constraints at scope and merge proposed.
	constraints, err := ps.store.ListConstraintsForScope(ctx, scopeType, scopeID)
	if err != nil {
		return LockoutAssessment{
			Safe:                 nil,
			CheckedPermissionIDs: checkedPerms,
			UndeterminedReason:   fmt.Sprintf("failed to load constraints: %v", err),
		}
	}

	// Add or replace proposed constraint.
	found := false
	for i, c := range constraints {
		if c.ID == proposedStore.ID {
			constraints[i] = proposedStore
			found = true
			break
		}
	}
	if !found {
		constraints = append(constraints, proposedStore)
	}

	// Filter to constraints that restrict admin in most-restrictive state.
	var restricting []*store.AccessConstraint
	for _, c := range constraints {
		if c.Disabled {
			continue
		}
		condition := ConstraintCondition{}
		if c.NotBefore != nil {
			condition.NotBefore = *c.NotBefore
		}
		if c.ExpiresAt != nil {
			condition.ExpiresAt = *c.ExpiresAt
		}
		if !condition.IsActiveInMostRestrictiveState(now) {
			continue
		}
		if constraintAllowsPermissionStore(c, PermissionConstraintAdmin) {
			continue
		}
		restricting = append(restricting, c)
	}

	if len(restricting) == 0 {
		safe := true
		count := len(adminUsers)
		actorRetains := true
		return LockoutAssessment{
			Safe:                          &safe,
			RemainingActiveDirectAdmins:   &count,
			CheckedPermissionIDs:          checkedPerms,
			ActorRetainsMutationAuthority: &actorRetains,
		}
	}

	// Check each admin user against restricting constraints.
	surviving := 0
	actorSurvives := false

	for _, au := range adminUsers {
		blocked := false
		for _, c := range restricting {
			if ps.constraintMatchesUser(ctx, c, au) {
				blocked = true
				break
			}
		}
		if !blocked {
			surviving++
			if au.userID == req.Actor.ID {
				actorSurvives = true
			}
		}
	}

	safe := surviving > 0
	return LockoutAssessment{
		Safe:                          &safe,
		RemainingActiveDirectAdmins:   &surviving,
		CheckedPermissionIDs:          checkedPerms,
		ActorRetainsMutationAuthority: &actorSurvives,
	}
}

// resolveAdminUsers finds users with constraint-admin permission at a scope.
func (ps *PreviewService) resolveAdminUsers(ctx context.Context, scopeType, scopeID string) ([]adminUserInfo, error) {
	bindings, err := ps.store.ListRoleBindingsForScope(ctx, scopeType, scopeID)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var admins []adminUserInfo

	for _, b := range bindings {
		rd, err := ps.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		hasAdmin := false
		for _, p := range rd.Permissions {
			if p == PermissionConstraintAdmin {
				hasAdmin = true
				break
			}
		}
		if !hasAdmin {
			continue
		}

		// Resolve to individual users.
		switch b.PrincipalType {
		case "user":
			if !seen[b.PrincipalID] {
				seen[b.PrincipalID] = true
				groups, _ := ps.store.GetEffectiveGroups(ctx, b.PrincipalID)
				admins = append(admins, adminUserInfo{
					userID:   b.PrincipalID,
					groupIDs: groups,
				})
			}
		case "group":
			// Expand group members.
			members, err := ps.store.GetGroupMembers(ctx, b.PrincipalID)
			if err != nil {
				continue
			}
			for _, m := range members {
				if m.MemberType == "user" && !seen[m.MemberID] {
					seen[m.MemberID] = true
					groups, _ := ps.store.GetEffectiveGroups(ctx, m.MemberID)
					admins = append(admins, adminUserInfo{
						userID:   m.MemberID,
						groupIDs: groups,
					})
				}
			}
		}
	}

	return admins, nil
}

// constraintMatchesUser checks if a store constraint matches a user.
func (ps *PreviewService) constraintMatchesUser(ctx context.Context, c *store.AccessConstraint, user adminUserInfo) bool {
	switch c.SubjectKind {
	case store.ConstraintSubjectAllPrincipals:
		return true
	case store.ConstraintSubjectPrincipal:
		if c.SubjectPrincipalType != nil && c.SubjectPrincipalID != nil {
			if *c.SubjectPrincipalType == "user" && *c.SubjectPrincipalID == user.userID {
				return true
			}
			if *c.SubjectPrincipalType == "group" {
				for _, gid := range user.groupIDs {
					if gid == *c.SubjectPrincipalID {
						return true
					}
				}
			}
		}
	case store.ConstraintSubjectGroupClosure:
		if c.SubjectGroupID != nil {
			for _, gid := range user.groupIDs {
				if gid == *c.SubjectGroupID {
					return true
				}
			}
		}
	}
	return false
}

// constraintAllowsPermissionStore checks if a store constraint allows a permission.
func constraintAllowsPermissionStore(c *store.AccessConstraint, permID string) bool {
	for _, p := range c.MaximumPermissions {
		if p == permID {
			return true
		}
	}
	return false
}

// hubToStoreConstraint converts a hub AccessConstraint to a store AccessConstraint.
func (ps *PreviewService) hubToStoreConstraint(hc *AccessConstraint) *store.AccessConstraint {
	if hc == nil {
		return nil
	}
	sc := &store.AccessConstraint{
		ID:                 hc.ID,
		Name:               hc.Name,
		SubjectKind:        string(hc.Subject.Kind),
		ScopeType:          hc.Scope.Type,
		ScopeID:            hc.Scope.ID,
		MaximumPermissions: hc.MaximumPermissions,
		Disabled:           hc.Disabled,
		Revision:           hc.Revision,
		Purpose:            hc.Purpose,
		UpdatedBy:          hc.UpdatedBy,
		CreatedBy:          hc.CreatedBy,
		CreatedAt:          hc.CreatedAt,
		UpdatedAt:          hc.UpdatedAt,
	}
	if hc.Subject.PrincipalType != "" {
		sc.SubjectPrincipalType = &hc.Subject.PrincipalType
	}
	if hc.Subject.PrincipalID != "" {
		sc.SubjectPrincipalID = &hc.Subject.PrincipalID
	}
	if hc.Subject.GroupID != "" {
		sc.SubjectGroupID = &hc.Subject.GroupID
	}
	if !hc.Condition.NotBefore.IsZero() {
		nb := hc.Condition.NotBefore
		sc.NotBefore = &nb
	}
	if !hc.Condition.ExpiresAt.IsZero() {
		ea := hc.Condition.ExpiresAt
		sc.ExpiresAt = &ea
	}
	return sc
}

// ---------------------------------------------------------------------------
// Intersecting boundaries
// ---------------------------------------------------------------------------

// findIntersecting finds other boundaries that overlap with the proposed mutation.
func (ps *PreviewService) findIntersecting(
	ctx context.Context,
	req PreviewRequest,
	existing, proposed *AccessConstraint,
	now time.Time,
) []IntersectingBoundary {
	var results []IntersectingBoundary

	constraint := proposed
	if constraint == nil {
		constraint = existing
	}
	if constraint == nil {
		return results
	}

	// Load all constraints at the same scope.
	constraints, err := ps.store.ListConstraintsForScope(ctx, constraint.Scope.Type, constraint.Scope.ID)
	if err != nil {
		ps.logger.Warn("failed to load constraints for intersection check", "error", err)
		return results
	}

	// Convert proposed permissions to a set.
	proposedPerms := constraint.MaximumPermissionSet()

	for _, sc := range constraints {
		// Skip self.
		if sc.ID == constraint.ID || sc.ID == req.ConstraintID {
			continue
		}

		hc := storeToHubAccessConstraint(sc)
		if hc == nil || !hc.IsActive(now) {
			continue
		}

		// Check for permission overlap.
		otherPerms := hc.MaximumPermissionSet()
		overlapCount := 0
		for p := range proposedPerms {
			if _, ok := otherPerms[p]; ok {
				overlapCount++
			}
		}

		if overlapCount == 0 {
			continue
		}

		// Determine relationship.
		relationship := RelationshipOverlaps
		if len(otherPerms) < len(proposedPerms) && isSubset(otherPerms, proposedPerms) {
			relationship = RelationshipNarrows
		} else if len(proposedPerms) < len(otherPerms) && isSubset(proposedPerms, otherPerms) {
			// Proposed is narrower: the other boundary limits relaxation.
			relationship = RelationshipLimitsRelaxation
		}

		// Check subject overlap for blocks_relaxation.
		if ps.subjectsOverlap(constraint.Subject, hc.Subject) {
			if relationship == RelationshipLimitsRelaxation {
				relationship = RelationshipBlocksRelaxation
			}
		}

		note := fmt.Sprintf("%d shared permissions", overlapCount)
		if relationship == RelationshipBlocksRelaxation {
			note += "; relaxing this boundary will not widen authority while the other boundary is active"
		}

		results = append(results, IntersectingBoundary{
			ID:                         sc.ID,
			Name:                       sc.Name,
			ScopeType:                  sc.ScopeType,
			ScopeID:                    sc.ScopeID,
			Relationship:               relationship,
			OverlappingPermissionCount: overlapCount,
			NetEffectNote:              note,
		})
	}

	return results
}

// isSubset returns true if a is a subset of b.
func isSubset(a, b map[string]struct{}) bool {
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// subjectsOverlap checks if two subject selectors can affect the same principals.
func (ps *PreviewService) subjectsOverlap(a, b SubjectSelector) bool {
	// all_principals overlaps with everything.
	if a.Kind == SubjectKindAllPrincipals || b.Kind == SubjectKindAllPrincipals {
		return true
	}
	// Same principal.
	if a.Kind == SubjectKindPrincipal && b.Kind == SubjectKindPrincipal {
		return a.PrincipalType == b.PrincipalType && a.PrincipalID == b.PrincipalID
	}
	// Same group closure.
	if a.Kind == SubjectKindGroupClosure && b.Kind == SubjectKindGroupClosure {
		return a.GroupID == b.GroupID
	}
	// Cross-type: a group_closure could contain a principal.
	// Conservative: assume overlap.
	return true
}

// ---------------------------------------------------------------------------
// Warnings
// ---------------------------------------------------------------------------

func (ps *PreviewService) generateWarnings(
	classification string,
	impact BoundaryImpact,
	temporal []TemporalImpact,
	completeness PreviewCompleteness,
	lockout LockoutAssessment,
	intersecting []IntersectingBoundary,
) []PreviewWarning {
	warnings := []PreviewWarning{}

	// Large blast radius warning.
	if impact.AffectedPrincipalCount > largeBlastRadiusThreshold {
		warnings = append(warnings, PreviewWarning{
			Code:     WarningCodeLargeBlastRadius,
			Severity: "warning",
			Message:  fmt.Sprintf("this change affects %d principals", impact.AffectedPrincipalCount),
			Details: map[string]interface{}{
				"count":     impact.AffectedPrincipalCount,
				"threshold": largeBlastRadiusThreshold,
			},
		})
	}

	// Relaxation included warning (when tightening also has some regaining).
	if classification == ClassificationMixed && impact.RegainingPrincipalCount > 0 {
		warnings = append(warnings, PreviewWarning{
			Code:     WarningCodeRelaxationIncluded,
			Severity: "info",
			Message:  fmt.Sprintf("%d principals regain permissions while %d lose permissions", impact.RegainingPrincipalCount, impact.LosingPrincipalCount),
		})
	}

	// Relaxation masked by intersection.
	for _, ib := range intersecting {
		if ib.Relationship == RelationshipBlocksRelaxation {
			warnings = append(warnings, PreviewWarning{
				Code:     WarningCodeRelaxationMasked,
				Severity: "info",
				Message:  fmt.Sprintf("relaxation is masked by intersecting boundary %q", ib.Name),
				Details: map[string]interface{}{
					"intersectingBoundaryId": ib.ID,
				},
			})
		}
	}

	// Degraded preview warning.
	if completeness.Degraded {
		warnings = append(warnings, PreviewWarning{
			Code:     WarningCodePreviewDegraded,
			Severity: "warning",
			Message:  "preview is degraded; counts are lower bounds",
		})
	}

	// Scheduled activation.
	for _, ts := range temporal {
		if ts.Label == "before activation" {
			warnings = append(warnings, PreviewWarning{
				Code:     WarningCodeScheduledActivation,
				Severity: "info",
				Message:  fmt.Sprintf("constraint will activate at %s", ts.Until.Format(time.RFC3339)),
			})
			break
		}
	}

	return warnings
}

// ---------------------------------------------------------------------------
// Affected principals pagination
// ---------------------------------------------------------------------------

// buildAffectedPage builds a page of affected principals from the full list.
func (ps *PreviewService) buildAffectedPage(
	impacted []ImpactedPrincipal,
	offset, pageSize int,
	completeness *PreviewCompleteness,
) AffectedPrincipalsPage {
	if pageSize <= 0 {
		pageSize = defaultPreviewPageSize
	}
	if pageSize > maxPreviewPageSize {
		pageSize = maxPreviewPageSize
	}

	total := len(impacted)
	end := offset + pageSize
	if end > total {
		end = total
	}

	var items []ImpactedPrincipal
	if offset < total {
		items = impacted[offset:end]
	}

	var nextPageToken string
	if end < total {
		nextPageToken = fmt.Sprintf("%d", end)
	}

	return AffectedPrincipalsPage{
		Items:           items,
		NextPageToken:   nextPageToken,
		TotalCount:      total,
		TotalCountExact: completeness.Complete && !completeness.Truncated,
	}
}

// ListAffectedPrincipals retrieves a page of affected principals for a
// completed preview. This allows pagination beyond the first page included
// in the PreviewResult.
func (ps *PreviewService) ListAffectedPrincipals(
	ctx context.Context,
	previewID string,
	pageToken string,
	pageSize int,
) (*AffectedPrincipalsPage, error) {
	jobVal, ok := ps.asyncJobs.Load(previewID)
	if !ok {
		return nil, fmt.Errorf("preview %s not found or expired", previewID)
	}
	job, ok := jobVal.(*PreviewJob)
	if !ok {
		return nil, fmt.Errorf("preview %s not yet completed", previewID)
	}

	// Hold the mutex while reading job state to avoid data races with
	// the async goroutine that writes Result and allImpacted.
	job.mu.Lock()
	if job.Result == nil {
		job.mu.Unlock()
		return nil, fmt.Errorf("preview %s not yet completed", previewID)
	}
	allImpacted := job.allImpacted
	completeness := job.Result.Completeness
	job.mu.Unlock()

	// Parse page token as offset.
	offset := 0
	if pageToken != "" {
		if _, err := fmt.Sscanf(pageToken, "%d", &offset); err != nil {
			return nil, fmt.Errorf("invalid page token: %w", err)
		}
	}

	// Paginate over the full impacted principals set, not just the first page.
	page := ps.buildAffectedPage(
		allImpacted,
		offset,
		pageSize,
		&completeness,
	)
	return &page, nil
}

// ---------------------------------------------------------------------------
// Async preview
// ---------------------------------------------------------------------------

// GeneratePreviewAsync starts an asynchronous preview computation for large
// subject sets and returns a job ID for polling.
func (ps *PreviewService) GeneratePreviewAsync(ctx context.Context, req PreviewRequest) (*PreviewJob, error) {
	if err := ps.validateRequest(req); err != nil {
		return nil, fmt.Errorf("invalid preview request: %w", err)
	}

	jobID := ps.generatePreviewID()
	jobCtx, jobCancel := context.WithCancel(context.Background())

	job := &PreviewJob{
		JobID:     jobID,
		Status:    JobStatusAccepted,
		Operation: req.Operation,
		cancel:    jobCancel,
	}

	ps.asyncJobs.Store(jobID, job)

	// Run in background.
	go func() {
		defer jobCancel()

		job.mu.Lock()
		if job.Status == JobStatusCancelled {
			job.mu.Unlock()
			return
		}
		job.Status = JobStatusRunning
		job.mu.Unlock()

		result, err := ps.GeneratePreview(jobCtx, req)

		job.mu.Lock()
		defer job.mu.Unlock()

		// If cancelled while running, do not overwrite status.
		if job.Status == JobStatusCancelled {
			return
		}

		if err != nil {
			job.Status = JobStatusFailed
			job.Error = err.Error()
		} else {
			job.Status = JobStatusSucceeded
			job.Result = result
			job.allImpacted = result.allImpacted
		}

		// Also store under the preview ID for pagination.
		if result != nil {
			ps.asyncJobs.Store(result.PreviewID, job)
		}
	}()

	return job, nil
}

// GetPreviewJob retrieves the current status of an async preview job.
func (ps *PreviewService) GetPreviewJob(ctx context.Context, jobID string) (*PreviewJob, error) {
	val, ok := ps.asyncJobs.Load(jobID)
	if !ok {
		return nil, fmt.Errorf("preview job %s not found", jobID)
	}
	job, ok := val.(*PreviewJob)
	if !ok {
		return nil, fmt.Errorf("invalid job data for %s", jobID)
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	// Return a snapshot to avoid races on the caller side.
	// Copy fields individually — never copy sync.Mutex.
	// Deep-copy Progress so the caller cannot observe mutations after unlock.
	var progressCopy *JobProgress
	if job.Progress != nil {
		p := *job.Progress
		progressCopy = &p
	}
	snapshot := &PreviewJob{
		JobID:       job.JobID,
		Status:      job.Status,
		Operation:   job.Operation,
		Progress:    progressCopy,
		Result:      job.Result,
		Error:       job.Error,
		allImpacted: job.allImpacted,
	}
	return snapshot, nil
}

// CancelPreviewJob cancels an in-progress async preview job.
func (ps *PreviewService) CancelPreviewJob(ctx context.Context, jobID string) error {
	val, ok := ps.asyncJobs.Load(jobID)
	if !ok {
		return fmt.Errorf("preview job %s not found", jobID)
	}
	job, ok := val.(*PreviewJob)
	if !ok {
		return fmt.Errorf("invalid job data for %s", jobID)
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.Status != JobStatusAccepted && job.Status != JobStatusRunning {
		return fmt.Errorf("job %s is not cancellable (status: %s)", jobID, job.Status)
	}
	job.Status = JobStatusCancelled
	if job.cancel != nil {
		job.cancel()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Token issuance
// ---------------------------------------------------------------------------

// tokenPayload is the binding vector for a preview token.
type tokenPayload struct {
	PreviewID        string `json:"pid"`
	ConstraintID     string `json:"cid"`
	ActorKind        string `json:"ak"`
	ActorID          string `json:"ai"`
	Operation        string `json:"op"`
	DraftHash        string `json:"dh"`
	ObjectRevision   int64  `json:"or"`
	StateFingerprint string `json:"sf"`
	ExpiresAt        int64  `json:"ea"`
	Nonce            string `json:"n"`
	Complete         bool   `json:"cp"`
}

// issueToken creates a server-signed preview token.
func (ps *PreviewService) issueToken(
	previewID string,
	constraintID string,
	actor PrincipalContext,
	operation string,
	draftHash string,
	objectRevision int64,
	stateFingerprint string,
	complete bool,
	now time.Time,
) (string, error) {
	// Generate nonce.
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)

	payload := tokenPayload{
		PreviewID:        previewID,
		ConstraintID:     constraintID,
		ActorKind:        string(actor.Kind),
		ActorID:          actor.ID,
		Operation:        operation,
		DraftHash:        draftHash,
		ObjectRevision:   objectRevision,
		StateFingerprint: stateFingerprint,
		ExpiresAt:        now.Add(previewTokenTTL).Unix(),
		Nonce:            nonce,
		Complete:         complete,
	}

	// Serialize payload.
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal token payload: %w", err)
	}

	// Compute HMAC.
	mac := hmac.New(sha256.New, ps.hmacKey)
	mac.Write(payloadBytes)
	sig := mac.Sum(nil)

	// Token = base64(payload) + "." + base64(sig)
	token := base64.RawURLEncoding.EncodeToString(payloadBytes) +
		"." +
		base64.RawURLEncoding.EncodeToString(sig)

	return token, nil
}

// ---------------------------------------------------------------------------
// Token validation
// ---------------------------------------------------------------------------

// ValidateToken validates a preview token for commit.
// Returns nil on success, or a TokenValidationError with a specific code.
func (ps *PreviewService) ValidateToken(
	ctx context.Context,
	token string,
	actor PrincipalContext,
	operation string,
	draft *store.AccessConstraint,
	objectRevision int64,
) error {
	now := ps.nowFunc()

	// Parse token.
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return &TokenValidationError{
			Code:    ErrCodePreviewTokenInvalid,
			Message: "malformed preview token",
		}
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return &TokenValidationError{
			Code:    ErrCodePreviewTokenInvalid,
			Message: "invalid token encoding",
		}
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return &TokenValidationError{
			Code:    ErrCodePreviewTokenInvalid,
			Message: "invalid token signature encoding",
		}
	}

	// Verify HMAC.
	mac := hmac.New(sha256.New, ps.hmacKey)
	mac.Write(payloadBytes)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expectedSig) {
		return &TokenValidationError{
			Code:    ErrCodePreviewTokenInvalid,
			Message: "invalid token signature",
		}
	}

	// Decode payload.
	var payload tokenPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return &TokenValidationError{
			Code:    ErrCodePreviewTokenInvalid,
			Message: "invalid token payload",
		}
	}

	// Check expiry.
	if now.Unix() > payload.ExpiresAt {
		return &TokenValidationError{
			Code:    ErrCodePreviewTokenExpired,
			Message: "preview token has expired",
		}
	}

	// Check actor.
	if string(actor.Kind) != payload.ActorKind || actor.ID != payload.ActorID {
		return &TokenValidationError{
			Code:    ErrCodePreviewActorMismatch,
			Message: "actor does not match the preview token",
		}
	}

	// Check operation.
	if operation != payload.Operation {
		return &TokenValidationError{
			Code:    ErrCodePreviewOperationMismatch,
			Message: fmt.Sprintf("operation %q does not match token operation %q", operation, payload.Operation),
		}
	}

	// Check draft hash.
	draftHash := ps.computeDraftHash(draft)
	if draftHash != payload.DraftHash {
		return &TokenValidationError{
			Code:    ErrCodePreviewDraftModified,
			Message: "draft was modified after preview was generated",
		}
	}

	// Check object revision.
	if objectRevision != payload.ObjectRevision {
		return &TokenValidationError{
			Code:    ErrCodePreviewRevisionMismatch,
			Message: fmt.Sprintf("object revision %d does not match preview revision %d", objectRevision, payload.ObjectRevision),
		}
	}

	// Check completeness (R2: server-side enforcement — incomplete previews cannot commit).
	if !payload.Complete {
		return &TokenValidationError{
			Code:    ErrCodePreviewIncomplete,
			Message: "preview was incomplete and cannot be used for commit",
		}
	}

	// Check state fingerprint (related state may have changed).
	currentFingerprint := ps.computeStateFingerprint(ctx, PreviewRequest{
		Operation:    operation,
		Draft:        draft,
		ConstraintID: payload.ConstraintID,
		BaseRevision: objectRevision,
		Actor:        actor,
	}, now)
	if currentFingerprint != payload.StateFingerprint {
		return &TokenValidationError{
			Code:    ErrCodePreviewStateMismatch,
			Message: "related state has changed since preview was generated (group membership, role bindings, etc.)",
		}
	}

	// All binding checks passed — now consume the nonce (R3: only after all checks pass).
	if _, loaded := ps.usedNonces.LoadOrStore(payload.Nonce, now.Add(previewTokenTTL)); loaded {
		return &TokenValidationError{
			Code:    ErrCodePreviewTokenReplay,
			Message: "preview token has already been used",
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// State fingerprint
// ---------------------------------------------------------------------------

// computeStateFingerprint computes a hash over relevant state timestamps
// and counts to detect changes since the preview was generated.
func (ps *PreviewService) computeStateFingerprint(
	ctx context.Context,
	req PreviewRequest,
	now time.Time,
) string {
	h := sha256.New()

	// Include constraint count.
	count, err := ps.store.CountAccessConstraints(ctx)
	if err == nil {
		_, _ = fmt.Fprintf(h, "cc:%d;", count)
	}

	// Include draft scope's constraint list hash.
	scopeType := ScopeTypeSystem
	scopeID := ""
	if req.Draft != nil {
		scopeType = req.Draft.ScopeType
		scopeID = req.Draft.ScopeID
	} else if req.ConstraintID != "" {
		sc, err := ps.store.GetAccessConstraint(ctx, req.ConstraintID)
		if err == nil {
			scopeType = sc.ScopeType
			scopeID = sc.ScopeID
		}
	}

	constraints, err := ps.store.ListConstraintsForScope(ctx, scopeType, scopeID)
	if err == nil {
		for _, c := range constraints {
			_, _ = fmt.Fprintf(h, "c:%s:%d:%s;", c.ID, c.Revision, c.UpdatedAt.UTC().Format(time.RFC3339Nano))
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Preview ID generation
// ---------------------------------------------------------------------------

func (ps *PreviewService) generatePreviewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		ps.logger.Error("failed to generate preview ID", "error", err)
		return fmt.Sprintf("preview-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Nonce cleanup
// ---------------------------------------------------------------------------

// cleanupNonces periodically evicts expired nonces from the used-nonce store.
// It shuts down when stopCleanup is closed.
func (ps *PreviewService) cleanupNonces() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ps.stopCleanup:
			return
		case <-ticker.C:
			now := time.Now()
			ps.usedNonces.Range(func(key, value interface{}) bool {
				if expiry, ok := value.(time.Time); ok {
					if now.After(expiry) {
						ps.usedNonces.Delete(key)
					}
				}
				return true
			})
		}
	}
}

// Close shuts down the PreviewService, stopping the nonce cleanup goroutine.
// Safe to call multiple times.
func (ps *PreviewService) Close() {
	ps.closeOnce.Do(func() {
		if ps.stopCleanup != nil {
			close(ps.stopCleanup)
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toSet(s []string) map[string]struct{} {
	m := make(map[string]struct{}, len(s))
	for _, v := range s {
		m[v] = struct{}{}
	}
	return m
}

func previewBoolPtr(b bool) *bool {
	return &b
}
