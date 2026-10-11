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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Stable error codes for GCP identity failures (ptone/scion#4004 spec §7,
// ptone/scion#4019). Each message names the failing step, the scope (project,
// profile, hub) and a remedy that says who can act.
//
// Account strings follow the visibility rule (spec §9 Q1): any project
// member, agents included, may see an account's email; a non-member may not.
// Every message below that names an account is written only on a path whose
// caller has already been authorized in the account's project (agent create,
// patch, reincarnate or start in that project) or, for a hub-scoped account,
// is a hub member. The "not available" text (msgSANotAvailableInProject)
// stays the single answer for both "no such account" and "not visible here",
// so it never names an account and never reveals that one exists.
//
// identity_not_mapped and identity_ksa_mismatch live in
// identity_mapping_error.go.
const (
	// ErrCodeIdentityNotVerified: the account is registered and reachable,
	// but the hub cannot obtain tokens for it (its verification failed or
	// never ran). Status 400.
	ErrCodeIdentityNotVerified = "identity_not_verified"

	// ErrCodeIdentityDefaultInvalid: a configured default (project,
	// per-profile or hub) names an account that is not available or not
	// verified. The message names the default and the setting to fix.
	// Status 400.
	ErrCodeIdentityDefaultInvalid = "identity_default_invalid"

	// ErrCodeIdentityAssignDenied: the caller may not assign the account,
	// because it lacks gcp_service_account.assign in the hub, lacks actAs on
	// the account in GCP, or the hub's check mode does not allow the
	// assignment. The message names what is missing and who grants it.
	// Status 403.
	ErrCodeIdentityAssignDenied = "identity_assign_denied"

	// ErrCodeIdentityModeUnsupported: the requested GCP identity mode is not
	// available where it was asked for. Status 400.
	ErrCodeIdentityModeUnsupported = "identity_mode_unsupported"
)

// identityVerifyRemedy is the remedy shared by every not-verified message.
// The token-creator grant is what verification probes, so it is the fix. The
// admin who acts depends on the account's scope; scope "" (unknown) names
// both.
func identityVerifyRemedy(scope string) string {
	var who, verify string
	switch scope {
	case store.ScopeHub:
		who, verify = "A hub admin", "scion service-accounts verify --global <id>"
	case store.ScopeProject:
		who, verify = "A project admin", "scion service-accounts verify <id>"
	default:
		who, verify = "A project admin (a hub admin for a hub-scoped account)",
			"scion service-accounts verify <id>, with --global for a hub-scoped account"
	}
	return who + " must grant the hub's service account roles/iam.serviceAccountTokenCreator on it, " +
		"then verify it (" + verify + ")."
}

// identityNotVerifiedMessage is the identity_not_verified text for sa.
func identityNotVerifiedMessage(sa *store.GCPServiceAccount) string {
	return fmt.Sprintf("GCP service account %q is not verified: the hub cannot obtain tokens for it. %s",
		sa.Email, identityVerifyRemedy(sa.Scope))
}

// writeIdentityNotVerified writes the identity_not_verified 400 for sa on the
// assign paths (create, patch, reincarnate). Their callers are authorized in
// the account's project, so naming the account follows the Q1 rule.
func writeIdentityNotVerified(w http.ResponseWriter, sa *store.GCPServiceAccount) {
	writeError(w, http.StatusBadRequest, ErrCodeIdentityNotVerified, identityNotVerifiedMessage(sa), nil)
}
