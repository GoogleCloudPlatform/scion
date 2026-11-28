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
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GroupBudgetMemberView describes one user's attributed spend inside a pooled
// group budget window. Only returned to hub admins and group owners/admins;
// omitted for regular group members (Tokenomics P3.3).
type GroupBudgetMemberView struct {
	UserID         string `json:"user_id"`
	Email          string `json:"email,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	TokensSummable int64  `json:"tokens_summable"`
	Calls          int64  `json:"calls"`
	Events         int64  `json:"events"`
}

// GroupBudgetItem describes one active pooled-group raw-token budget window
// (system-wide or project-scoped, day or month), matching A2's budget item
// JSON shape plus group-specific non_additive and optional members fields.
type GroupBudgetItem struct {
	ID                 string                  `json:"id,omitempty"`
	LimitDefinitionID  string                  `json:"limit_definition_id"`
	LimitName          string                  `json:"limit_name"`
	ScopeKind          string                  `json:"scope_kind"`
	ScopeID            string                  `json:"scope_id"`
	ProjectScopeID     string                  `json:"project_scope_id,omitempty"`
	WindowStartUTC     time.Time               `json:"window_start_utc"`
	WindowEndUTC       time.Time               `json:"window_end_utc"`
	Cap                *int64                  `json:"cap"`
	TokenAction        string                  `json:"token_action"`
	Consumed           int64                   `json:"consumed"`
	State              string                  `json:"state"`
	ThresholdsNotified int64                   `json:"thresholds_notified"`
	OverrideUntil      *time.Time              `json:"override_until"`
	StateVersion       int64                   `json:"state_version"`
	AsOf               time.Time               `json:"as_of"`
	NonAdditive        bool                    `json:"non_additive"`
	Members            []GroupBudgetMemberView `json:"members,omitempty"`
}

// GroupUsageBudgetsResponse is returned by GET /api/v1/groups/{id}/usage/budgets.
type GroupUsageBudgetsResponse struct {
	Scope       UsageScopeRef     `json:"scope"`
	GroupName   string            `json:"group_name,omitempty"`
	NonAdditive bool              `json:"non_additive"`
	AsOf        time.Time         `json:"as_of"`
	Budgets     []GroupBudgetItem `json:"budgets"`
}

// handleGroupUsageBudgets serves GET /api/v1/groups/{id}/usage/budgets.
//
// Experiment gate: requires both tokenomics.usage_ledger and
// tokenomics.token_budgets (returns 404 when either is disabled).
//
// Authorization (Tokenomics P3.3):
//   - Hub admins (quota.read or group.update on the group) and group
//     owners/admins receive pool totals PLUS per-member breakdown.
//   - Regular effective group members receive pool totals ONLY (members omitted).
//   - Non-members without admin permission receive 403 Forbidden (or 404 when
//     the group is not visible).
func (s *Server) handleGroupUsageBudgets(w http.ResponseWriter, r *http.Request, groupIDOrSlug string) {
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) ||
		!s.experimentEnabled(experiments.TokenomicsTokenBudgets) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	user := GetUserIdentityFromContext(ctx)
	if identity == nil || user == nil {
		Forbidden(w)
		return
	}

	group, err := s.resolveVisibleGroupByIDOrSlug(ctx, identity, groupIDOrSlug)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	includeBreakdown, allowed, err := s.authorizeGroupUsageBudgets(ctx, identity, user, group)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !allowed {
		Forbidden(w)
		return
	}

	projectFilter := strings.TrimSpace(r.URL.Query().Get("project_id"))

	now := time.Now().UTC()
	if eval := s.TokenBudgetEvaluator(); eval != nil {
		now = eval.now()
	}

	// Collect active pooled bindings for this group across day and month limits.
	limitSpecs := []struct {
		name   string
		window string
	}{
		{name: store.LimitLLMTokensPerGroupDay, window: "day"},
		{name: store.LimitLLMTokensPerGroupMonth, window: "month"},
	}

	defsByID := make(map[string]struct {
		def    *store.LimitDefinition
		window string
	}, 2)
	for _, ls := range limitSpecs {
		def, err := s.store.GetLimitDefinitionByName(ctx, ls.name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if def != nil {
			defsByID[def.ID] = struct {
				def    *store.LimitDefinition
				window string
			}{def: def, window: ls.window}
		}
	}

	bindings, err := s.store.ListEntitlementBindingsForSubject(ctx, store.EntitlementSubjectGroup, group.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Group bindings by (limitDefID, projectScopeID), including explicit
	// unlimited (value == 0) bindings so they are emitted with cap: null (O4).
	type poolKey struct {
		limitDefID     string
		projectScopeID string
	}
	seenPools := make(map[poolKey]struct{})
	poolOrder := make([]poolKey, 0)
	for _, b := range bindings {
		if b == nil {
			continue
		}
		if _, ok := defsByID[b.LimitDefinitionID]; !ok {
			continue
		}
		projScope := ""
		if b.ScopeType == store.QuotaScopeProject {
			projScope = b.ScopeID
		}
		if projectFilter != "" && projScope != projectFilter {
			continue
		}
		pk := poolKey{limitDefID: b.LimitDefinitionID, projectScopeID: projScope}
		if _, dup := seenPools[pk]; !dup {
			seenPools[pk] = struct{}{}
			poolOrder = append(poolOrder, pk)
		}
	}

	sort.Slice(poolOrder, func(i, j int) bool {
		di := defsByID[poolOrder[i].limitDefID]
		dj := defsByID[poolOrder[j].limitDefID]
		if di.def.Name != dj.def.Name {
			return di.def.Name < dj.def.Name
		}
		return poolOrder[i].projectScopeID < poolOrder[j].projectScopeID
	})

	userCache := make(map[string]*store.User)
	lookupUser := func(uid string) *store.User {
		if uid == "" {
			return nil
		}
		if u, ok := userCache[uid]; ok {
			return u
		}
		u, err := s.store.GetUser(ctx, uid)
		if err != nil {
			userCache[uid] = nil
			return nil
		}
		userCache[uid] = u
		return u
	}

	items := make([]GroupBudgetItem, 0, len(poolOrder))
	evalCache := newTokenLimitEvalCache(s.quotaService)
	for _, pk := range poolOrder {
		info := defsByID[pk.limitDefID]
		scopeType := store.QuotaScopeSystem
		if pk.projectScopeID != "" {
			scopeType = store.QuotaScopeProject
		}
		capVal, action, err := evalCache.resolveGroupForDef(ctx, info.def, group.ID, scopeType, pk.projectScopeID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		var winStart, winEnd time.Time
		if info.window == "day" {
			winStart, winEnd = utcDayWindow(now)
		} else {
			winStart, winEnd = utcMonthWindow(now)
		}

		consumed, err := s.store.SumUsageGroupRollupTokens(ctx, group.ID, pk.projectScopeID, winStart, winEnd)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		stRow, stErr := s.store.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
			LimitDefinitionID: info.def.ID,
			ScopeKind:         store.UsageScopeKindGroup,
			ScopeID:           group.ID,
			ProjectScopeID:    pk.projectScopeID,
			WindowStartUTC:    winStart,
		})
		if stErr != nil && !errors.Is(stErr, store.ErrNotFound) {
			writeErrorFromErr(w, stErr, "")
			return
		}

		var capPtr *int64
		stateStr := store.UsageBudgetStateOK
		if capVal > 0 {
			capCopy := capVal
			capPtr = &capCopy
			stateStr = computeBudgetState(consumed, capVal)
		}
		item := GroupBudgetItem{
			LimitDefinitionID: info.def.ID,
			LimitName:         info.def.Name,
			ScopeKind:         store.UsageScopeKindGroup,
			ScopeID:           group.ID,
			ProjectScopeID:    pk.projectScopeID,
			WindowStartUTC:    winStart,
			WindowEndUTC:      winEnd,
			Cap:               capPtr,
			TokenAction:       action,
			Consumed:          consumed,
			State:             stateStr,
			AsOf:              now,
			NonAdditive:       true,
		}
		if stRow != nil {
			item.ID = stRow.ID
			// O2: match A2 semantics — while an override is active, report the
			// stored state rather than overwriting it with computed exhausted.
			if stRow.OverrideUntil != nil && stRow.OverrideUntil.After(now) {
				item.State = stRow.State
			} else {
				item.State = maxBudgetState(stRow.State, stateStr)
			}
			item.ThresholdsNotified = stRow.ThresholdsNotified
			item.OverrideUntil = stRow.OverrideUntil
			item.StateVersion = stRow.StateVersion
		}

		if includeBreakdown {
			episodes, epErr := s.store.ListUsageGroupSeedEpisodesOverlapping(ctx, group.ID, winStart, winEnd)
			if epErr != nil {
				writeErrorFromErr(w, epErr, "")
				return
			}
			rawMembers, err := s.store.QueryGroupMemberUsageBreakdown(
				ctx, group.ID, pk.projectScopeID, episodes, winStart, winEnd,
			)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			members := make([]GroupBudgetMemberView, 0, len(rawMembers))
			for _, rm := range rawMembers {
				mv := GroupBudgetMemberView{
					UserID:         rm.UserID,
					TokensSummable: rm.TokensSummable,
					Calls:          rm.Calls,
					Events:         rm.Events,
				}
				if u := lookupUser(rm.UserID); u != nil {
					mv.Email = u.Email
					mv.DisplayName = u.DisplayName
				}
				members = append(members, mv)
			}
			item.Members = members
		}

		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, GroupUsageBudgetsResponse{
		Scope: UsageScopeRef{
			Kind: store.UsageScopeKindGroup,
			ID:   group.ID,
		},
		GroupName:   group.Name,
		NonAdditive: true,
		AsOf:        now,
		Budgets:     items,
	})
}

// authorizeGroupUsageBudgets returns (includeBreakdown, allowed, err):
//   - Hub admins (quota.read on system) or callers with ActionUpdate on the
//     group, or direct group owners/admins: (true, true, nil)
//   - Effective group members (direct or via nested child group): (false, true, nil)
//   - Everyone else: (false, false, nil)
func (s *Server) authorizeGroupUsageBudgets(
	ctx context.Context,
	identity Identity,
	user UserIdentity,
	group *store.Group,
) (includeBreakdown bool, allowed bool, err error) {
	// 1. Hub admins with quota.read on system scope get full breakdown.
	quotaDec := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   hubScopedResource("quota", "hub"),
		Action:     ActionRead,
		Permission: "quota.read",
	})
	if quotaDec.Allowed {
		return true, true, nil
	}

	// 2. Callers with group.update on this group get full breakdown.
	if updDec := s.authzService.CheckAccess(ctx, identity, groupResource(group), ActionUpdate); updDec.Allowed {
		return true, true, nil
	}

	// R4: Membership-based branches below must honor the scoped-UAT ceiling
	// and boundary (mirroring projectCloneTokenDenial / uatListPermitted).
	if scoped, ok := identity.(*ScopedUserIdentity); ok {
		if scoped == nil || !scoped.Ceiling().Allows("quota.read") {
			return false, false, nil
		}
		boundary := scoped.Boundary()
		if !boundary.Valid() || !permissionEligibleForBoundary("quota.read", boundary.Kind) {
			return false, false, nil
		}
	}

	// R3: Apply the admin-kind test-identity decide-time clamp before trusting
	// direct or effective group membership.
	live, err := s.authzService.testAdminPrincipalLive(ctx, user.ID())
	if err != nil {
		return false, false, err
	}
	if !live {
		return false, false, nil
	}

	// 3. Direct group owner/admin membership gets full breakdown.
	membership, mErr := s.store.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, user.ID())
	if mErr != nil && !errors.Is(mErr, store.ErrNotFound) {
		return false, false, mErr
	}
	if membership != nil &&
		(membership.Role == store.GroupMemberRoleOwner || membership.Role == store.GroupMemberRoleAdmin) {
		return true, true, nil
	}

	// 4. Effective group members (direct or nested) see pool totals only.
	effGroups, err := s.store.GetEffectiveGroups(ctx, user.ID())
	if err != nil {
		return false, false, err
	}
	for _, gid := range effGroups {
		if gid == group.ID {
			return false, true, nil
		}
	}

	return false, false, nil
}
