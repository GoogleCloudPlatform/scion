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
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Shared sign-in policy / account-state helper.
//
// The Hub has more than one entry point that can hand back an already-
// existing user record on a successful sign-in: interactive web login
// (provisionUser) and the Google identity resolver shared by the GE
// exchange endpoint and the external-bearer path (GoogleIdentityResolver.
// Resolve). Both need to apply the exact same live access policy and
// account-state handling to that existing record — otherwise the two
// implementations can quietly drift apart over time. applyLiveSignInPolicy
// is the single implementation both call, so there is only one place this
// logic can go wrong.
// ---------------------------------------------------------------------------

// signInPolicyDeps bundles the callbacks applyLiveSignInPolicy needs to
// enforce the same live sign-in policy and account-state handling for an
// existing user, regardless of which sign-in path found that user. Every
// field except authorize is optional: a nil callback degrades to a safe
// no-op/pass-through rather than an error, so a caller that only has a
// subset of the server's configuration available can still use the helper.
type signInPolicyDeps struct {
	// authorize reports whether email currently satisfies the Hub sign-in
	// policy (admin bypass, authorized_domains, user_access_mode). A nil
	// authorize is treated as "deny" (fail closed).
	authorize func(ctx context.Context, email string) bool

	// activationRole computes the role an invited record receives when it
	// activates: evaluated as a brand-new user, except a UI-promoted admin
	// keeps its role. Optional; when nil, activation keeps the record's
	// current (placeholder) role unchanged.
	activationRole func(ctx context.Context, email, currentRole, userID string) string

	// roleFor re-evaluates role for an already-active record on every
	// sign-in (admin_emails additions/removals, safe demotion). Optional;
	// when nil, the current role is kept unchanged.
	roleFor func(ctx context.Context, email, currentRole, userID string) string

	// UpdateUser persists the mutated user record. Optional; when nil the
	// helper still computes the state transition but does not persist it.
	// Capitalized (unlike its sibling fields) so the call site below reads
	// as UpdateUser for the authorization mutation-audit scanner
	// (pkg/hub/authzop), which discovers security-relevant mutations by
	// literal call-site symbol name.
	UpdateUser func(ctx context.Context, user *store.User) error

	// ensureSuperAdminBinding / deleteSuperAdminBinding keep the
	// system-scoped super-admin RoleBinding in sync with an admin role
	// transition. Optional no-ops when nil.
	ensureSuperAdminBinding func(ctx context.Context, userID string)
	deleteSuperAdminBinding func(ctx context.Context, userID string)

	// syncGrants reconciles hub-membership role grants to match user.Role
	// after persistence. Optional no-op when nil. Failures are logged and
	// otherwise ignored — a grant-sync failure degrades the session but must
	// not fail the sign-in, matching the existing best-effort treatment.
	syncGrants func(ctx context.Context, userID, role string) error

	// auditActivated / auditDenied record invite-lifecycle audit events.
	// Optional no-ops when nil.
	auditActivated func(ctx context.Context, email, userID string)
	auditDenied    func(ctx context.Context, email string)
}

// signInPolicyPersistOpts controls when applyLiveSignInPolicy persists the
// user record and syncs grants. A caller invoked once per interactive
// sign-in (provisionUser, first-time linking) wants its existing
// always-persist/always-sync behavior; a caller invoked on every request for
// an already-linked identity (the resolver's bound-identity branch) must
// not turn every request into a full-row write — persistence there should
// happen only when something meaningful actually changed.
type signInPolicyPersistOpts struct {
	// AlwaysPersist persists and syncs grants unconditionally on success,
	// matching interactive login's existing behavior (including a LastLogin
	// bump on every call). Set by callers invoked once per sign-in event.
	AlwaysPersist bool

	// CallerChanged reports that the caller already mutated fields on user
	// (e.g. a presented-email update) before calling, so the helper must
	// still persist even if its own state-transition logic finds nothing
	// to change. Ignored when AlwaysPersist is set.
	CallerChanged bool
}

// applyLiveSignInPolicy enforces, for an already-existing user, the same
// live access policy and account-state handling on every sign-in path:
//
//   - a suspended account is always rejected;
//   - unless preAuthorized, the current sign-in policy (deps.authorize) must
//     pass before the record is admitted — no state mutation and no identity
//     link occurs when it does not;
//   - an invited record activates (invited -> active) and its role is
//     evaluated as a new user, mirroring interactive login;
//   - an already-active record has its role re-evaluated the same way login
//     does (picking up admin_emails additions/removals);
//   - the record is persisted and grants are synced when something
//     meaningful changed (activation, a role change, a profile backfill, or
//     opts.CallerChanged) or when opts.AlwaysPersist is set. A resulting
//     super-admin binding change is applied whenever the record persists.
//
// preAuthorized skips the policy check only — never the suspension check —
// and is for principals whose authorization decision was already made
// elsewhere before this helper is reached.
func applyLiveSignInPolicy(
	ctx context.Context,
	deps signInPolicyDeps,
	user *store.User,
	displayName, avatarURL string,
	preAuthorized bool,
	opts signInPolicyPersistOpts,
) (*store.User, error) {
	if user == nil {
		return nil, fmt.Errorf("sign-in policy: user record is nil")
	}
	if user.Status == store.UserStatusSuspended {
		slog.Warn("sign-in rejected: account suspended", "email", user.Email, "user_id", user.ID)
		return nil, ErrUserSuspended
	}

	authorized := preAuthorized
	if !authorized {
		authorized = deps.authorize != nil && deps.authorize(ctx, user.Email)
	}
	if !authorized {
		if deps.auditDenied != nil {
			deps.auditDenied(ctx, user.Email)
		}
		return nil, ErrAccessDenied
	}

	// Track whether a super-admin RoleBinding needs to be created or
	// deleted. The mutation is deferred until after persistence succeeds so
	// a failed update cannot leave the binding state diverged from
	// user.Role.
	var bindingSuperAdmin string // "", "ensure", or "delete"

	// activated / roleChanged / profileChanged track what actually changed,
	// so a caller invoked on every request can persist only when it matters
	// (see signInPolicyPersistOpts). activated implies a role/grant sync
	// regardless of AlwaysPersist; profileChanged alone does not.
	var activated, roleChanged, profileChanged bool

	if user.Status == store.UserStatusInvited {
		activated = true
		slog.Info("user activated from invited state", "email", user.Email, "user_id", user.ID)
		user.Status = store.UserStatusActive
		if displayName != "" {
			user.DisplayName = displayName
		}
		if avatarURL != "" {
			user.AvatarURL = avatarURL
		}

		oldRole := user.Role
		newRole := user.Role
		if deps.activationRole != nil {
			newRole = deps.activationRole(ctx, user.Email, user.Role, user.ID)
		}
		user.Role = newRole
		if oldRole == store.UserRoleAdmin && user.Role != store.UserRoleAdmin {
			bindingSuperAdmin = "delete"
		} else if user.Role == store.UserRoleAdmin && oldRole != store.UserRoleAdmin {
			bindingSuperAdmin = "ensure"
		}
		if deps.auditActivated != nil {
			deps.auditActivated(ctx, user.Email, user.ID)
		}
	} else {
		if avatarURL != "" && user.AvatarURL == "" {
			user.AvatarURL = avatarURL
			profileChanged = true
		}
		if displayName != "" && user.DisplayName == "" {
			user.DisplayName = displayName
			profileChanged = true
		}
		if deps.roleFor != nil {
			if newRole := deps.roleFor(ctx, user.Email, user.Role, user.ID); newRole != user.Role {
				oldRole := user.Role
				slog.Info("user role changed on sign-in", "email", user.Email, "old_role", oldRole, "new_role", newRole)
				user.Role = newRole
				roleChanged = true
				if oldRole == store.UserRoleAdmin {
					bindingSuperAdmin = "delete"
				} else if newRole == store.UserRoleAdmin {
					bindingSuperAdmin = "ensure"
				}
			}
		}
	}

	shouldPersist := opts.AlwaysPersist || opts.CallerChanged || activated || roleChanged || profileChanged
	shouldSyncGrants := opts.AlwaysPersist || activated || roleChanged

	if shouldPersist {
		// A per-request full-row write is exactly the cost (and the lost-
		// update risk on Status/Role) this bumps LastLogin to avoid paying
		// when nothing else changed — only touch it as part of a write that
		// is already happening for another reason.
		user.LastLogin = time.Now()
		if deps.UpdateUser != nil {
			if err := deps.UpdateUser(ctx, user); err != nil {
				slog.Error("failed to update user on sign-in", "email", user.Email, "user_id", user.ID, "error", err)
				return nil, fmt.Errorf("update user: %w", err)
			}
		}

		switch bindingSuperAdmin {
		case "ensure":
			if deps.ensureSuperAdminBinding != nil {
				deps.ensureSuperAdminBinding(ctx, user.ID)
			}
		case "delete":
			if deps.deleteSuperAdminBinding != nil {
				deps.deleteSuperAdminBinding(ctx, user.ID)
			}
		}
	}

	if shouldSyncGrants && deps.syncGrants != nil {
		if err := deps.syncGrants(ctx, user.ID, user.Role); err != nil {
			slog.Warn("failed to sync hub role grants on sign-in",
				"email", user.Email, "user_id", user.ID, "role", user.Role, "error", err)
		}
	}

	return user, nil
}
