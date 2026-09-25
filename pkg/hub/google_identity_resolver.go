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
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// GoogleIdentityResolver — resolves a validated Google identity to a local Hub
// user via the external identity binding system.
//
// Extracted from ge_exchange.go so the GE credential-exchange
// endpoint and the external-bearer authentication path (auth_external_bearer.go)
// share exactly one resolution/provisioning/suspension code path. Both must
// reach identical decisions for the same Google identity during the soak
// between the two mechanisms.
// ---------------------------------------------------------------------------

var (
	errBindingConflict       = errors.New("external identity binding conflicts with existing user")
	errAmbiguousLinkage      = errors.New("ambiguous email-to-user linkage")
	errNonAuthoritativeEmail = errors.New("email domain not authoritative for automatic linkage")
)

// ResolvePolicy carries per-request policy decisions that Resolve needs but
// cannot infer from the validated identity alone.
type ResolvePolicy struct {
	// PreAuthorized skips the Hub sign-in policy (the injected authorize func)
	// for first-time provisioning. Set ONLY for service accounts admitted by
	// allowed_gcp_projects: the project allowlist IS the authorization decision.
	// It never bypasses the suspension check on an already-bound user — that
	// check runs unconditionally in Resolve/resolveAfterConflict.
	PreAuthorized bool
}

// GoogleIdentityResolver resolves a validated Google identity (see
// google_credential_validator.go) to a local Hub user, applying:
//   - Sub-bound identity: (provider=google, issuer, sub) -> user, so an email
//     change or a recycled email does not move the account.
//   - Authoritative-domain bootstrap: a first-time link by email happens only
//     for Gmail, matching Workspace hd, or a Google service-account email
//     (Google controls gserviceaccount.com).
//   - The Hub sign-in policy (authorize) and role assignment (roleFor) for
//     newly provisioned users, matching interactive OAuth.
//   - Suspension enforcement on every call — no cache, so a suspension takes
//     effect on the very next request.
type GoogleIdentityResolver struct {
	users     store.UserStore
	extIDs    store.ExternalIdentityStore
	authorize func(ctx context.Context, email string) bool
	roleFor   func(ctx context.Context, email string) string
	log       *slog.Logger
}

// NewGoogleIdentityResolver creates a GoogleIdentityResolver.
//
// authorize implements the Hub sign-in policy (domain restriction, invite-only,
// admin bypass) — typically (*Server).isUserAuthorized. roleFor assigns the
// role for newly-provisioned users (honouring admin_emails) — typically
// func(ctx, email) string { return srv.getUserRole(ctx, email, "", "") }.
// Both default to safe fail-closed/fail-plain behavior when nil, so tests that
// don't exercise those paths can omit them.
func NewGoogleIdentityResolver(
	users store.UserStore,
	extIDs store.ExternalIdentityStore,
	authorize func(ctx context.Context, email string) bool,
	roleFor func(ctx context.Context, email string) string,
	log *slog.Logger,
) *GoogleIdentityResolver {
	if authorize == nil {
		// Fail closed: no policy means no provisioning.
		authorize = func(ctx context.Context, email string) bool { return false }
	}
	if roleFor == nil {
		roleFor = func(ctx context.Context, email string) string { return "member" }
	}
	if log == nil {
		log = slog.Default()
	}
	return &GoogleIdentityResolver{
		users:     users,
		extIDs:    extIDs,
		authorize: authorize,
		roleFor:   roleFor,
		log:       log,
	}
}

// Resolve resolves a validated Google identity to a local Hub user. The flow
// is:
//
//  1. Look up existing binding by (provider, canonical issuer, sub).
//  2. If found: verify the bound user exists and is not suspended, update
//     email if changed. Return the user.
//  3. If not found: attempt first-time bootstrap via email, guarded by the
//     authoritative email domain requirement.
//  4. Create atomic binding and return user.
func (r *GoogleIdentityResolver) Resolve(ctx context.Context, identity *ValidatedGoogleIdentity, policy ResolvePolicy) (*store.User, error) {
	if identity == nil {
		// A GoogleCredentialValidator that returns (nil, nil) is a contract
		// violation, not a verification failure — return a generic error
		// rather than reaching the nil pointer dereference on identity.Issuer
		// below. Both callers (ge_exchange.go and auth_external_bearer.go)
		// already map an unrecognized Resolve error to a 5xx, not the 4xx
		// arms reserved for the named sentinels below.
		return nil, fmt.Errorf("google identity resolver: no identity to resolve")
	}
	canonicalIssuer := canonicalizeGoogleIssuer(identity.Issuer)

	// Step 1: Look up existing binding.
	binding, err := r.extIDs.GetExternalIdentity(ctx, "google", canonicalIssuer, identity.Subject)
	if err == nil {
		// Binding exists — verify the bound user. Suspension is always
		// enforced here, regardless of policy.PreAuthorized.
		user, err := r.users.GetUser(ctx, binding.UserID)
		if err != nil {
			r.log.Error("google identity resolver: bound user not found",
				"binding_id", binding.ID,
				"user_id", binding.UserID,
				"sub", identity.Subject)
			return nil, fmt.Errorf("bound user not found: %w", err)
		}

		if user.Status == "suspended" {
			return nil, ErrUserSuspended
		}

		// Update email if it changed (informational, does not relink).
		normalizedEmail := strings.ToLower(identity.Email)
		if strings.ToLower(binding.Email) != normalizedEmail {
			r.log.Info("google identity resolver: updating binding email",
				"old", binding.Email, "new", normalizedEmail,
				"sub", identity.Subject, "user_id", user.ID)
			_ = r.extIDs.UpdateExternalIdentityEmail(ctx, binding.ID, normalizedEmail)
			// Also update the user's profile email if it matches the old binding email.
			if strings.EqualFold(user.Email, binding.Email) {
				user.Email = normalizedEmail
				_ = r.users.UpdateUser(ctx, user)
			}
		}

		// Update display name / avatar if missing.
		updated := false
		if identity.DisplayName != "" && user.DisplayName == "" {
			user.DisplayName = identity.DisplayName
			updated = true
		}
		if identity.AvatarURL != "" && user.AvatarURL == "" {
			user.AvatarURL = identity.AvatarURL
			updated = true
		}
		if updated {
			_ = r.users.UpdateUser(ctx, user)
		}

		return user, nil
	}

	// A binding-lookup fault that is not "no such binding" must not be
	// silently treated as "no binding": that would let a transient store
	// error either provision a duplicate user or (for a non-authoritative
	// email) surface as 403 instead of the store fault it actually is. Only
	// store.ErrNotFound means "no binding exists yet".
	if !errors.Is(err, store.ErrNotFound) {
		r.log.Error("google identity resolver: external identity lookup failed",
			"sub", identity.Subject, "error", err)
		return nil, fmt.Errorf("check external identity binding: %w", err)
	}

	// Step 2: No existing binding — attempt first-time bootstrap.
	// Automatic bootstrap only for authoritative email domains: Gmail,
	// verified Workspace hd, or a Google service-account email (Google
	// controls gserviceaccount.com, so SA emails are authoritative too).
	authoritative := isAuthoritativeEmailDomain(identity.Email, identity.HostedDomain) ||
		isGoogleServiceAccount(identity.Email)
	if !authoritative {
		r.log.Warn("google identity resolver: non-authoritative email, cannot auto-link",
			"email", identity.Email,
			"hd", identity.HostedDomain,
			"sub", identity.Subject)
		return nil, errNonAuthoritativeEmail
	}

	// Look up existing user by email.
	normalizedEmail := strings.ToLower(identity.Email)
	existingUser, err := r.users.GetUserByEmail(ctx, normalizedEmail)
	if err == nil {
		// Found a user by email. Verify no conflicting binding exists.
		existingBindings, _ := r.extIDs.GetExternalIdentitiesByUserID(ctx, existingUser.ID)
		for _, eb := range existingBindings {
			if eb.Provider == "google" && eb.Issuer == canonicalIssuer && eb.Subject != identity.Subject {
				// Another Google subject is already bound to this user.
				r.log.Error("google identity resolver: conflicting Google binding",
					"existing_sub", eb.Subject,
					"new_sub", identity.Subject,
					"user_id", existingUser.ID)
				return nil, errBindingConflict
			}
		}

		if existingUser.Status == "suspended" {
			return nil, ErrUserSuspended
		}

		// Create the binding atomically. If a concurrent resolution already
		// created it (unique constraint violation), fall back to the winner's
		// binding — this is the conflict-safe race-resolution path.
		now := time.Now()
		if err := r.extIDs.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
			ID:        uuid.New().String(),
			Provider:  "google",
			Issuer:    canonicalIssuer,
			Subject:   identity.Subject,
			UserID:    existingUser.ID,
			Email:     normalizedEmail,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			return r.resolveAfterConflict(ctx, canonicalIssuer, identity, existingUser.ID, err)
		}

		r.log.Info("google identity resolver: created new binding for existing user",
			"sub", identity.Subject,
			"email", normalizedEmail,
			"user_id", existingUser.ID)
		return existingUser, nil
	}

	// No existing user by email — provision a new user through the normal path.
	// This requires the same authorization checks as regular login, unless
	// policy.PreAuthorized (service accounts admitted by allowed_gcp_projects).
	//
	// provisionNewUser may return an existing user instead of a newly created
	// one when a concurrent resolution wins the unique-email race. The
	// provisioned flag distinguishes the two cases for orphan cleanup below.
	user, provisioned, err := r.provisionNewUser(ctx, identity, policy)
	if err != nil {
		return nil, err
	}

	// Create the binding. If a concurrent resolution already created it
	// (unique constraint violation), resolve via the winning binding and
	// clean up the orphaned user only if we actually provisioned a NEW user
	// that differs from the winner's user.
	now := time.Now()
	if err := r.extIDs.CreateExternalIdentity(ctx, &ExternalIdentityBinding{
		ID:        uuid.New().String(),
		Provider:  "google",
		Issuer:    canonicalIssuer,
		Subject:   identity.Subject,
		UserID:    user.ID,
		Email:     normalizedEmail,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		winner, resolveErr := r.resolveAfterConflict(ctx, canonicalIssuer, identity, "", err)
		// Orphan cleanup: only delete if we provisioned a new user AND the
		// winner resolved to a different user. When all concurrent resolutions
		// converge on the same user (via email-collision resolution), the
		// user is NOT an orphan even if we lose the binding race.
		if provisioned && resolveErr == nil && winner != nil && winner.ID != user.ID {
			if delErr := r.users.DeleteUser(ctx, user.ID); delErr != nil {
				r.log.Warn("google identity resolver: failed to clean up orphaned user after conflict",
					"user_id", user.ID, "error", delErr)
			}
		}
		return winner, resolveErr
	}

	return user, nil
}

// resolveAfterConflict handles the case where CreateExternalIdentity failed
// due to a unique constraint violation (race between concurrent resolutions).
// It looks up the winning binding and resolves to the winner's user.
//
// If expectedUserID is non-empty (linking to an existing user), the winner's
// binding must point to the same user — otherwise it fails closed with
// errBindingConflict to prevent silently adopting a mismatched user.
func (r *GoogleIdentityResolver) resolveAfterConflict(ctx context.Context, canonicalIssuer string, identity *ValidatedGoogleIdentity, expectedUserID string, createErr error) (*store.User, error) {
	// Retry by looking up the binding the winner created.
	winner, err := r.extIDs.GetExternalIdentity(ctx, "google", canonicalIssuer, identity.Subject)
	if err != nil {
		// Binding still not found: this was a genuine error, not a race.
		r.log.Error("google identity resolver: binding creation failed and no winning binding found",
			"create_error", createErr, "lookup_error", err, "sub", identity.Subject)
		return nil, fmt.Errorf("failed to create identity binding: %w", createErr)
	}

	// If we expected a specific user (existing-user linkage path), validate
	// the winner bound to the same user. Fail closed otherwise.
	if expectedUserID != "" && winner.UserID != expectedUserID {
		r.log.Error("google identity resolver: conflict resolution mismatch — winner bound to different user",
			"expected_user_id", expectedUserID, "winner_user_id", winner.UserID,
			"sub", identity.Subject)
		return nil, errBindingConflict
	}

	// Found the winner's binding — resolve to the winner's user.
	user, err := r.users.GetUser(ctx, winner.UserID)
	if err != nil {
		return nil, fmt.Errorf("bound user not found after conflict resolution: %w", err)
	}
	if user.Status == "suspended" {
		return nil, ErrUserSuspended
	}

	r.log.Info("google identity resolver: resolved to existing binding after race",
		"sub", identity.Subject, "user_id", user.ID)
	return user, nil
}

// provisionNewUser creates a new user via the normal Hub provisioning path.
// Enforces the same domain/invite/allow-registration policy as the normal
// Hub login flow via the injected authorize func, UNLESS policy.PreAuthorized
// is set (service accounts admitted by allowed_gcp_projects: the project
// allowlist is itself the authorization decision). PreAuthorized never
// bypasses suspension checks — those happen in Resolve/resolveAfterConflict on
// every call, not just at provisioning time.
//
// Returns (user, true, nil) when a new user was created, or
// (winner, false, nil) when CreateUser lost a unique-email race and the
// winning user was found. The caller uses the provisioned flag to decide
// whether orphan cleanup is appropriate.
//
// When CreateUser fails with a unique-email constraint violation (concurrent
// resolution race), re-queries by normalized email. If the winning user is
// found and passes suspension checks, returns the winner with
// provisioned=false; otherwise returns the original create error to fail
// closed.
func (r *GoogleIdentityResolver) provisionNewUser(ctx context.Context, identity *ValidatedGoogleIdentity, policy ResolvePolicy) (user *store.User, provisioned bool, err error) {
	normalizedEmail := strings.ToLower(identity.Email)

	if policy.PreAuthorized {
		r.log.Info("google identity resolver: bypassing sign-in policy for pre-authorized principal",
			"email", normalizedEmail, "sub", identity.Subject, "reason", "allowed_gcp_projects")
	} else if !r.authorize(ctx, normalizedEmail) {
		r.log.Warn("google identity resolver: user not authorized for auto-provisioning",
			"email", normalizedEmail, "sub", identity.Subject)
		return nil, false, fmt.Errorf("%w: user not authorized for auto-provisioning", ErrAccessDenied)
	}

	newUser := &store.User{
		ID:          uuid.New().String(),
		Email:       normalizedEmail,
		DisplayName: identity.DisplayName,
		AvatarURL:   identity.AvatarURL,
		Role:        r.roleFor(ctx, normalizedEmail),
		Status:      store.UserStatusActive,
		Created:     time.Now(),
		LastLogin:   time.Now(),
	}

	if createErr := r.users.CreateUser(ctx, newUser); createErr != nil {
		// Unique-email collision: another concurrent resolution won the race
		// and created the user first. Re-query by email to find the winner.
		if errors.Is(createErr, store.ErrAlreadyExists) {
			winner, lookupErr := r.users.GetUserByEmail(ctx, normalizedEmail)
			if lookupErr != nil {
				// No winner found — return the original create error (fail closed).
				r.log.Error("google identity resolver: user creation conflict but no winner found",
					"email", normalizedEmail, "create_error", createErr, "lookup_error", lookupErr)
				return nil, false, fmt.Errorf("create user: %w", createErr)
			}
			if winner.Status == "suspended" {
				return nil, false, ErrUserSuspended
			}
			r.log.Info("google identity resolver: resolved to existing user after email collision",
				"email", normalizedEmail, "winner_user_id", winner.ID,
				"sub", identity.Subject)
			return winner, false, nil
		}
		return nil, false, fmt.Errorf("create user: %w", createErr)
	}

	r.log.Info("google identity resolver: provisioned new user",
		"email", normalizedEmail,
		"user_id", newUser.ID,
		"role", newUser.Role,
		"sub", identity.Subject)

	return newUser, true, nil
}
