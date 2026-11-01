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
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// authorizedDomainsKey is the koanf path of the sign-in domain list.
const authorizedDomainsKey = "server.auth.authorized_domains"

// authorizedDomainsPinnedReason is the skipped reason of a saved
// authorized_domains on a node whose SCION_SERVER_* variable pins the list.
const authorizedDomainsPinnedReason = "Saved; a deployment setting on this Hub pins this value."

// authorizedDomainsLockoutMessage is the message of the save-time refusal.
// It names no domain or email.
const authorizedDomainsLockoutMessage = "This authorized_domains list would refuse sign-in for your account. " +
	"Include your email domain, or add your email to admin_emails."

// snapshotPinsAuthorizedDomains reports whether a SCION_SERVER_* variable
// on this node pins authorized_domains, so a DB-backed snapshot's value
// does not apply (ptone/scion#6000). File snapshots already fold the
// variable into their value.
func snapshotPinsAuthorizedDomains(snap Layer1Snapshot) bool {
	return snap.FromOperationalSettings && slices.Contains(snap.EnvOverrides, authorizedDomainsKey)
}

// authorizedDomainsForSnapshot returns the authorized domain list the hub
// runs with for snap (ptone/scion#6000). startup is the list the hub
// started with (settings.yaml and SCION_SERVER_*).
//
// Precedence on a DB-backed hub: a SCION_SERVER_* pin on this node, then
// the saved value, then settings.yaml and the seed. A DB-backed snapshot
// with an empty list (cleared, or never set in the stored access row; a
// cleared list is stored without the key) uses the startup value, so a
// save never allows more domains than the deployment does. A file
// snapshot is the loaded configuration and applies as it is.
//
// The list is applied as loaded (copied, not otherwise normalised), as at
// startup: the matcher lowercases both sides, and dropping blank entries
// could turn a list that matches nothing into an empty one, which allows
// every domain.
func authorizedDomainsForSnapshot(snap Layer1Snapshot, startup []string) []string {
	if !snap.FromOperationalSettings {
		return slices.Clone(snap.AuthorizedDomains)
	}
	if snapshotPinsAuthorizedDomains(snap) || len(snap.AuthorizedDomains) == 0 {
		return slices.Clone(startup)
	}
	return slices.Clone(snap.AuthorizedDomains)
}

// sameDomainSet reports whether two domain lists hold the same entries,
// ignoring order, case and duplicates.
func sameDomainSet(a, b []string) bool {
	norm := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, d := range in {
			out = append(out, strings.ToLower(d))
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(norm(a), norm(b))
}

// WarnAuthorizedDomainsDrift logs one warning at startup when the saved
// authorized_domains differs from the deployment value the hub started
// with (ptone/scion#6000). Before that fix a saved list was stored but not
// enforced, so after an upgrade it may start to apply. The warning names
// which value is in effect and the list sizes, never the domains.
func (s *Server) WarnAuthorizedDomainsDrift(snap Layer1Snapshot) {
	if !snap.FromOperationalSettings || len(snap.AuthorizedDomains) == 0 {
		return
	}
	s.mu.RLock()
	startup := s.startupLayer1.AuthorizedDomains
	s.mu.RUnlock()
	if sameDomainSet(snap.AuthorizedDomains, startup) {
		return
	}
	winner := "saved"
	if snapshotPinsAuthorizedDomains(snap) {
		winner = "deployment_pin"
	}
	slog.Warn("authorized_domains: the saved value differs from the deployment value",
		"in_effect", winner,
		"saved_count", len(snap.AuthorizedDomains),
		"deployment_count", len(startup))
}

// authorizedDomainsLockout reports whether the access settings a save
// would leave in effect refuse sign-in for the saving account
// (ptone/scion#6000). It mirrors the admin and domain checks of
// checkSignInDenial: an email in admin_emails is always allowed, else a
// non-empty domain list must match the email. domains and admins are the
// lists that apply after the save. An empty email (no human sign-in to
// lock out) is never refused.
func authorizedDomainsLockout(email string, domains, admins []string) bool {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" || len(domains) == 0 {
		return false
	}
	for _, a := range admins {
		if strings.ToLower(strings.TrimSpace(a)) == e {
			return false
		}
	}
	return !isEmailInDomains(e, domains)
}

// accessListsAfterDBSave returns the authorized domains and admin emails a
// DB-backed hub applies after saving the access document merged (the
// request merged on the stored row), worked out as ApplySnapshot does:
// domains from authorizedDomainsForSnapshot (a pin on this node, else the
// saved list, else the startup value), and admin_emails from the saved
// list, else the running one (ApplySnapshot keeps the running list when
// the saved one is empty).
func (s *Server) accessListsAfterDBSave(merged opsettings.AccessSettings, pinned bool) (domains, admins []string) {
	s.mu.RLock()
	startup := slices.Clone(s.startupLayer1.AuthorizedDomains)
	running := slices.Clone(s.config.AdminEmails)
	s.mu.RUnlock()
	snap := Layer1Snapshot{FromOperationalSettings: true, AuthorizedDomains: merged.AuthorizedDomains}
	if pinned {
		snap.EnvOverrides = []string{authorizedDomainsKey}
	}
	domains = authorizedDomainsForSnapshot(snap, startup)
	admins = running
	if len(merged.AdminEmails) > 0 {
		admins = config.SanitizeEmailList(merged.AdminEmails)
	}
	return domains, admins
}

// accessListsAfterFileSave returns the authorized domains and admin emails
// a file-mode hub applies after a save that sends the given values
// (sentDomains/sentAdmins report whether each key was sent). An unsent key
// keeps the loaded value, which is the running one. When a SCION_SERVER_*
// variable sets authorized_domains, the reloaded configuration keeps the
// variable's value, the startup one. An empty admin_emails keeps the
// running list, as ApplySnapshot does.
func (s *Server) accessListsAfterFileSave(domainsSent bool, sentDomains []string, adminsSent bool, sentAdmins []string, pinned bool) (domains, admins []string) {
	s.mu.RLock()
	startup := slices.Clone(s.startupLayer1.AuthorizedDomains)
	runningDomains := slices.Clone(s.config.AuthorizedDomains)
	running := slices.Clone(s.config.AdminEmails)
	s.mu.RUnlock()
	switch {
	case pinned:
		domains = startup
	case domainsSent:
		domains = slices.Clone(sentDomains)
	default:
		domains = runningDomains
	}
	admins = running
	if adminsSent && len(sentAdmins) > 0 {
		admins = config.SanitizeEmailList(sentAdmins)
	}
	return domains, admins
}

// rejectAuthorizedDomainsLockout writes a 422 naming
// server.auth.authorized_domains and returns true when the access lists a
// save leaves in effect refuse sign-in for the saving account
// (authorizedDomainsLockout, ptone/scion#6000). Nothing has been written
// when it refuses.
//
// Test identity clamp: for an admin-kind test identity (or when its kind
// cannot be read) admin_emails grants no bypass here, so a test admin never
// counts as a real admin for this rule. Such callers are refused earlier
// anyway (rejectTestAdminSettingsWrite denies the access keys), and the
// check can only refuse a save, never allow one.
func (s *Server) rejectAuthorizedDomainsLockout(ctx context.Context, w http.ResponseWriter, email string, domains, admins []string) bool {
	if _, testAdmin := s.testAdminCallerIsAdminKind(ctx); testAdmin {
		admins = nil
	}
	if !authorizedDomainsLockout(email, domains, admins) {
		return false
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, authorizedDomainsLockoutMessage,
		map[string]interface{}{"field": authorizedDomainsKey})
	return true
}

// rejectFileAccessLockout runs the lockout check for a file-mode
// server-config save (handlePutServerConfig) and writes the 422 when it
// refuses. It returns true when the request was refused.
func (s *Server) rejectFileAccessLockout(w http.ResponseWriter, r *http.Request, rawBody []byte, req *ServerConfigUpdateRequest) bool {
	top, err := parseFieldPresence(rawBody)
	if err != nil {
		return false // the typed decode reports malformed bodies
	}
	serverFP := top.nestedPresence("server")
	domainsSent := serverFP.nestedPresence("auth").has("authorized_domains")
	adminsSent := serverFP.nestedPresence("hub").has("admin_emails")
	pinned := config.LoadEnvKoanf().Exists(authorizedDomainsKey)
	if !adminsSent && (!domainsSent || pinned) {
		return false
	}
	var sentDomains, sentAdmins []string
	if req.Server != nil && req.Server.Auth != nil {
		sentDomains = req.Server.Auth.AuthorizedDomains
	}
	if req.Server != nil && req.Server.Hub != nil {
		sentAdmins = req.Server.Hub.AdminEmails
	}
	email := ""
	if caller := GetUserIdentityFromContext(r.Context()); caller != nil {
		email = caller.Email()
	}
	domains, admins := s.accessListsAfterFileSave(domainsSent, sentDomains, adminsSent, sentAdmins, pinned)
	return s.rejectAuthorizedDomainsLockout(r.Context(), w, email, domains, admins)
}

// rejectAccessResetLockout runs the lockout check for a reset of the access
// section (DELETE /api/v1/admin/server-config/sections/access) and writes
// the 422 when it refuses (ptone/scion#6000). Without a row the snapshot
// uses the bootstrap access section, so the lists after the reset are
// worked out from it as for a save (accessListsAfterDBSave): domains from
// the bootstrap list, else the startup list (and, on a node that pins the
// list, the pinned list as well); admin_emails from the bootstrap list,
// else the running one. The test identity clamp applies. A bootstrap
// section that cannot be read, or a nil ops, refuses the reset. Nothing is
// deleted when it refuses.
func (s *Server) rejectAccessResetLockout(ctx context.Context, w http.ResponseWriter, ops *OperationalSettings) bool {
	if ops == nil {
		// The reset handler refuses a hub without OperationalSettings
		// before it gets here; a nil ops still refuses the reset (fail
		// closed), it never skips the check.
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"Section reset requires DB-backed operational settings", nil)
		return true
	}
	var boot opsettings.AccessSettings
	if ops.bootstrapKoanf != nil {
		doc, err := opsettings.ExtractSectionFromKoanf(ops.bootstrapKoanf, "access")
		if err == nil && len(doc) > 0 {
			err = json.Unmarshal(doc, &boot)
		}
		if err != nil {
			slog.Error("Access reset: cannot read the bootstrap access section", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to reset section", nil)
			return true
		}
	}
	email := ""
	if caller := GetUserIdentityFromContext(ctx); caller != nil {
		email = caller.Email()
	}
	domains, admins := s.accessListsAfterDBSave(boot, false)
	if s.rejectAuthorizedDomainsLockout(ctx, w, email, domains, admins) {
		return true
	}
	if slices.Contains(ops.EnvOverriddenKeys(), authorizedDomainsKey) {
		domains, admins = s.accessListsAfterDBSave(boot, true)
		return s.rejectAuthorizedDomainsLockout(ctx, w, email, domains, admins)
	}
	return false
}
