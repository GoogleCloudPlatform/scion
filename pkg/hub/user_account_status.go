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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Account-status rule outside decide.
//
// checkUserAccount loads a user principal's users row and applies
// activeUserPredicate, the rule decide's account-status gate
// (principalStatusGate) applies. The checks below call it, so they cannot
// drift from decide:
//   - requireActiveUser (project access) and the dev_local ceiling checks;
//   - UnifiedAuthMiddleware, when it accepts a hub user token or a
//     development token, so that the development user is held to the same
//     rule as any other user with a record, on every request, whichever
//     handler serves it;
//   - the user access token checks (ValidateToken, bearerStillValid);
//   - the live-connection checks (conduitUserStatus, relayUserIdentity,
//     conduitRenewalIdentity).
//
// Code that reads a users row for its own purpose may still apply the same
// status rule inline (for example resolveScheduledUser, which maps every
// non-active outcome, including a nil row, to reasonPrincipalInactive).
//
// The rule: an existing users row with status active passes. A missing row
// denies a local user (user, dev); a federated user with no row passes
// through to the binding check. Every other status (suspended, invited,
// pending, or any status added later) denies. A lookup fault denies and is
// reported as such (isProjectAccessLookupFault), never as an inactive user.

// userRowReader is the users-row lookup checkUserAccount needs.
type userRowReader interface {
	GetUser(ctx context.Context, id string) (*store.User, error)
}

// checkUserAccount loads principal's users row through users and applies
// activeUserPredicate to it. It returns the row when the lookup found one
// (even when the rule denies it, so a caller can tell a suspended account
// from another refusal) and the predicate's error. A nil users reader, and
// a lookup that returns neither a row nor an error, is a lookup fault. Principals that are not users pass with no lookup.
func checkUserAccount(ctx context.Context, users userRowReader, principal PrincipalContext) (*store.User, error) {
	if NormalizePrincipalType(string(principal.Kind)) != store.RoleBindingPrincipalUser {
		return nil, nil
	}
	if isNilUserRowReader(users) {
		return nil, projectAccessLookupFault(fmt.Errorf("%w: user lookup failed: no user store", ErrProjectAccessDenied))
	}
	user, err := users.GetUser(ctx, principal.ID)
	if err == nil && user == nil {
		// Neither a row nor an error: a store inconsistency, not a
		// decision about the account.
		return nil, projectAccessLookupFault(fmt.Errorf("%w: user lookup returned no user", ErrProjectAccessDenied))
	}
	if err != nil {
		user = nil
	}
	return user, activeUserPredicate(principal, user, err)
}

// isNilUserRowReader reports whether r is nil, including a typed nil
// store.UserStore held in the interface.
func isNilUserRowReader(r userRowReader) bool {
	if r == nil {
		return true
	}
	if s, ok := r.(store.UserStore); ok && s == nil {
		return true
	}
	return false
}

// accountRefusal classifies checkUserAccount's result.
type accountRefusal int

const (
	// accountAllowed: the rule passed.
	accountAllowed accountRefusal = iota
	// accountLookupFault: the users row could not be read.
	accountLookupFault
	// accountMissing: no users row (store.ErrNotFound) for a local user.
	accountMissing
	// accountSuspended: the row's status is suspended.
	accountSuspended
	// accountNotActive: the row exists with any other non-active status.
	accountNotActive
)

// classifyUserAccount maps checkUserAccount's (user, err) to an
// accountRefusal.
func classifyUserAccount(user *store.User, err error) accountRefusal {
	switch {
	case err == nil:
		return accountAllowed
	case isProjectAccessLookupFault(err):
		return accountLookupFault
	case user == nil:
		return accountMissing
	case user.Status == store.UserStatusSuspended:
		return accountSuspended
	default:
		return accountNotActive
	}
}

// accountRefusalReason is a short, stable cause for logs and conduit
// verdicts. It never carries an email or a user id.
func accountRefusalReason(r accountRefusal) string {
	switch r {
	case accountLookupFault:
		return "user lookup failed"
	case accountMissing:
		return "user not found"
	case accountSuspended:
		return "user suspended"
	case accountNotActive:
		return "user not active"
	default:
		return ""
	}
}

// writeAccountRefusal writes UnifiedAuthMiddleware's answer to a bearer
// whose user account failed checkUserAccount. A hub user token and a
// development token get the same answer for the same account state:
//   - lookup fault: 503 store_error;
//   - suspended: 403 user_suspended;
//   - no users row: 401 user_not_found;
//   - any other non-active status: 401 unauthorized.
func writeAccountRefusal(w http.ResponseWriter, r accountRefusal) {
	switch r {
	case accountLookupFault:
		writeError(w, http.StatusServiceUnavailable, "store_error",
			"unable to verify user status", nil)
	case accountSuspended:
		writeError(w, http.StatusForbidden, "user_suspended",
			"access denied: user account is suspended", nil)
	case accountMissing:
		writeError(w, http.StatusUnauthorized, ErrCodeUserNotFound,
			"invalid access token: no user record for this token", nil)
	default:
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"invalid access token", nil)
	}
}

// devUserPrincipal is the principal checkUserAccount evaluates for the
// development user.
func devUserPrincipal() PrincipalContext {
	return PrincipalContext{Kind: PrincipalKindDev, ID: DevUserID}
}

// admitDevUserAccount applies checkUserAccount to the development user in
// UnifiedAuthMiddleware's development-token arms. It returns true when the
// account is active; otherwise it has written the refusal (writeAccountRefusal)
// and logged the cause, with no email or user id.
//
// A missing row is refused, as for any local user. The row exists before
// the first request wherever development tokens are accepted: New seeds it
// (seedDevUser) at startup under the same ServerConfig.DevAuthToken != ""
// condition that sets AuthConfig.DevAuthEnabled. decide's account-status
// gate already denies the development user without a row.
//
// Without a user store the check is skipped, as the hub user token arm
// skips its row block: every hub server sets AuthConfig.UserStore.
func admitDevUserAccount(ctx context.Context, cfg AuthConfig, log *slog.Logger, w http.ResponseWriter) bool {
	if isNilUserRowReader(cfg.UserStore) {
		return true
	}
	user, err := checkUserAccount(ctx, cfg.UserStore, devUserPrincipal())
	refusal := classifyUserAccount(user, err)
	if refusal == accountAllowed {
		return true
	}
	if refusal == accountLookupFault {
		log.Error("Dev token: user status lookup failed", "error", err)
	} else {
		log.Warn("Dev token rejected: development user is not active",
			"reason", accountRefusalReason(refusal))
	}
	writeAccountRefusal(w, refusal)
	return false
}
