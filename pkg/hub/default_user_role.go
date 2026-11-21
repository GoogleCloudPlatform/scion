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
	"net/http"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DefaultUserRoleWarningCode identifies why a configured custom
// server.auth.default_user_role is currently falling back to "member".
const (
	DefaultUserRoleWarningExperimentDisabled = "experiment_disabled"
	DefaultUserRoleWarningRoleNotFound       = "role_not_found"
	DefaultUserRoleWarningRoleIneligible     = "role_ineligible"
)

// DefaultUserRoleWarning describes an active fallback when the configured
// server.auth.default_user_role cannot be applied.
type DefaultUserRoleWarning struct {
	Code           string `json:"code"`
	ConfiguredRole string `json:"configured_role"`
	EffectiveRole  string `json:"effective_role"`
	Message        string `json:"message"`
}

// EligibleDefaultRole is a lightweight summary of a custom system-scope role
// definition that is eligible for server.auth.default_user_role.
type EligibleDefaultRole struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// unsupportedDefaultUserRolePermissions lists catalog permissions that rely on
// built-in-only scope filters and therefore cannot be carried by a custom
// system-scope default user role (ptone/scion#7779, ptone/scion#8288).
// Global-only write twins (skill.*_global, template.*_global) are permitted.
var unsupportedDefaultUserRolePermissions = map[string]struct{}{
	"skill.read":            {},
	"skill.list":            {},
	"skill.create":          {},
	"skill.update":          {},
	"skill.delete":          {},
	"template.read":         {},
	"template.list":         {},
	"template.create":       {},
	"template.update":       {},
	"template.delete":       {},
	"harness_config.read":   {},
	"harness_config.list":   {},
	"harness_config.create": {},
	"harness_config.update": {},
	"harness_config.delete": {},
}

// isBuiltinDefaultUserRole reports whether value is empty or one of the
// canonical built-in default user roles ("member" or "viewer").
func isBuiltinDefaultUserRole(value string) bool {
	return value == "" || value == store.UserRoleMember || value == store.UserRoleViewer
}

// unsupportedPermissionsForDefaultUserRole returns a sorted, deduplicated list
// of any permissions in perms that are not eligible for a custom default user role.
func unsupportedPermissionsForDefaultUserRole(perms []string) []string {
	var seen map[string]struct{}
	for _, p := range perms {
		if _, bad := unsupportedDefaultUserRolePermissions[p]; bad {
			if seen == nil {
				seen = make(map[string]struct{})
			}
			seen[p] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// checkCustomDefaultUserRoleEligible verifies that rd is a custom system-scope
// role definition containing no unsupported catalog permissions.
func checkCustomDefaultUserRoleEligible(rd *store.RoleDefinition) error {
	if rd == nil {
		return errors.New("role definition is nil")
	}
	if rd.ScopeType != store.RoleScopeSystem {
		return fmt.Errorf("role %q has scope %q; must be %q", rd.Name, rd.ScopeType, store.RoleScopeSystem)
	}
	if rd.System {
		return fmt.Errorf("built-in system role %q cannot be selected as a custom default user role", rd.Name)
	}
	if bad := unsupportedPermissionsForDefaultUserRole(rd.Permissions); len(bad) > 0 {
		return fmt.Errorf("role %q contains permissions not supported on default_user_role: %s", rd.Name, strings.Join(bad, ", "))
	}
	return nil
}

// resolvedDefaultUserRole is the result of resolving server.auth.default_user_role.
type resolvedDefaultUserRole struct {
	// BuiltinRole is always store.UserRoleMember or store.UserRoleViewer.
	// When CustomRole is non-nil or a fallback occurred, BuiltinRole is "member".
	BuiltinRole string
	// CustomRole is non-nil only when an eligible custom system-scope role is active.
	CustomRole *store.RoleDefinition
	// Warning is non-nil when a non-built-in configured value fell back to "member".
	Warning *DefaultUserRoleWarning
}

// resolveDefaultUserRole resolves configured against the store when
// customEnabled is true. When logWarning is true (runtime provisioning and
// demotion), any fallback to "member" is logged via slog.Warn; GET handlers
// pass logWarning=false to populate default_user_role_warning without log spam.
func resolveDefaultUserRole(ctx context.Context, st store.Store, configured string, customEnabled, logWarning bool) resolvedDefaultUserRole {
	var res resolvedDefaultUserRole
	switch {
	case configured == "" || configured == store.UserRoleMember:
		res = resolvedDefaultUserRole{BuiltinRole: store.UserRoleMember}
	case configured == store.UserRoleViewer:
		res = resolvedDefaultUserRole{BuiltinRole: store.UserRoleViewer}
	case !customEnabled:
		res = resolvedDefaultUserRole{
			BuiltinRole: store.UserRoleMember,
			Warning: &DefaultUserRoleWarning{
				Code:           DefaultUserRoleWarningExperimentDisabled,
				ConfiguredRole: configured,
				EffectiveRole:  store.UserRoleMember,
				Message:        fmt.Sprintf("Configured default_user_role %q requires experiment %s; falling back to %q.", configured, experiments.CustomDefaultUserRole, store.UserRoleMember),
			},
		}
	case st == nil:
		res = resolvedDefaultUserRole{
			BuiltinRole: store.UserRoleMember,
			Warning: &DefaultUserRoleWarning{
				Code:           DefaultUserRoleWarningRoleNotFound,
				ConfiguredRole: configured,
				EffectiveRole:  store.UserRoleMember,
				Message:        fmt.Sprintf("Configured default_user_role %q could not be resolved; falling back to %q.", configured, store.UserRoleMember),
			},
		}
	default:
		rd, err := st.GetRoleDefinitionByName(ctx, configured, store.RoleScopeSystem)
		if err != nil {
			res = resolvedDefaultUserRole{
				BuiltinRole: store.UserRoleMember,
				Warning: &DefaultUserRoleWarning{
					Code:           DefaultUserRoleWarningRoleNotFound,
					ConfiguredRole: configured,
					EffectiveRole:  store.UserRoleMember,
					Message:        fmt.Sprintf("Configured default_user_role %q was not found as a system-scope role; falling back to %q.", configured, store.UserRoleMember),
				},
			}
		} else if eligErr := checkCustomDefaultUserRoleEligible(rd); eligErr != nil {
			res = resolvedDefaultUserRole{
				BuiltinRole: store.UserRoleMember,
				Warning: &DefaultUserRoleWarning{
					Code:           DefaultUserRoleWarningRoleIneligible,
					ConfiguredRole: configured,
					EffectiveRole:  store.UserRoleMember,
					Message:        fmt.Sprintf("Configured default_user_role %q is not eligible (%s); falling back to %q.", configured, eligErr.Error(), store.UserRoleMember),
				},
			}
		} else {
			res = resolvedDefaultUserRole{
				BuiltinRole: store.UserRoleMember,
				CustomRole:  rd,
			}
		}
	}
	if logWarning && res.Warning != nil {
		slog.Warn("default_user_role falling back to member",
			"code", res.Warning.Code,
			"configured_role", res.Warning.ConfiguredRole,
			"effective_role", res.Warning.EffectiveRole,
			"message", res.Warning.Message)
	}
	return res
}

// defaultUserRoleValidationError marks a client validation error (HTTP 422)
// as distinct from an internal store lookup error (HTTP 500).
type defaultUserRoleValidationError struct {
	msg string
}

func (e *defaultUserRoleValidationError) Error() string { return e.msg }

// validateDefaultUserRoleSetting validates value for server.auth.default_user_role
// when custom default user roles are enabled. Returns (nil, nil) for built-in
// values ("", "member", "viewer") and the eligible RoleDefinition for custom roles.
func validateDefaultUserRoleSetting(ctx context.Context, st store.Store, value string) (*store.RoleDefinition, error) {
	if isBuiltinDefaultUserRole(value) {
		return nil, nil
	}
	if value == store.UserRoleAdmin {
		return nil, &defaultUserRoleValidationError{
			msg: fmt.Sprintf("invalid server.auth.default_user_role %q: \"admin\" cannot be used as the default user role", value),
		}
	}
	if st == nil {
		return nil, &defaultUserRoleValidationError{
			msg: fmt.Sprintf("invalid server.auth.default_user_role %q: must be \"member\" or \"viewer\"", value),
		}
	}
	rd, err := st.GetRoleDefinitionByName(ctx, value, store.RoleScopeSystem)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &defaultUserRoleValidationError{
				msg: fmt.Sprintf("invalid server.auth.default_user_role %q: system-scope role definition not found", value),
			}
		}
		return nil, fmt.Errorf("lookup role definition %q: %w", value, err)
	}
	if eligErr := checkCustomDefaultUserRoleEligible(rd); eligErr != nil {
		return nil, &defaultUserRoleValidationError{
			msg: fmt.Sprintf("invalid server.auth.default_user_role %q: %s", value, eligErr.Error()),
		}
	}
	return rd, nil
}

// writeDefaultUserRoleExperimentDisabled writes the 412 experiment_disabled
// refusal when saving a custom server.auth.default_user_role while
// hub.custom_default_user_role is off.
func writeDefaultUserRoleExperimentDisabled(w http.ResponseWriter) {
	writeError(w, http.StatusPreconditionFailed, ErrCodeExperimentDisabled,
		"custom server.auth.default_user_role is not enabled on this Hub (experiment "+experiments.CustomDefaultUserRole+" is off): enable it, or use \"member\" or \"viewer\"",
		map[string]interface{}{"experiment": experiments.CustomDefaultUserRole})
}

// validateDefaultUserRoleSave enforces the experiment gate, custom-role
// eligibility, and CanDelegate check for PUT /api/v1/admin/server-config in
// both file and DB modes. Returns true if the save may proceed; otherwise it
// has already written the HTTP error response.
func (s *Server) validateDefaultUserRoleSave(w http.ResponseWriter, r *http.Request, candidate, stored string) bool {
	if isBuiltinDefaultUserRole(candidate) {
		return true
	}
	// Echoing an already-stored custom value passes whether the experiment is
	// off or on, so saving other settings on the page is never blocked.
	if candidate == stored {
		return true
	}
	if !s.experimentEnabled(experiments.CustomDefaultUserRole) {
		writeDefaultUserRoleExperimentDisabled(w)
		return false
	}
	rd, err := validateDefaultUserRoleSetting(r.Context(), s.store, candidate)
	if err != nil {
		var valErr *defaultUserRoleValidationError
		if errors.As(err, &valErr) {
			writeSettingsFieldError(w, "server.auth.default_user_role", valErr.Error())
			return false
		}
		slog.Error("PUT server-config: failed to validate default_user_role", "role", candidate, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to validate default_user_role", nil)
		return false
	}
	if rd != nil && s.authzService != nil {
		if user := GetUserIdentityFromContext(r.Context()); user != nil {
			decision := s.authzService.CanDelegate(r.Context(), user, GrantDescriptor{
				Type:                  GrantTypeCustomRole,
				CustomRolePermissions: rd.Permissions,
				ScopeType:             store.RoleScopeSystem,
			})
			if !decision.Allowed {
				writeForbidden(w, "cannot set server.auth.default_user_role: "+decision.Reason)
				return false
			}
		}
	}
	return true
}

// listEligibleDefaultRoles returns all custom system-scope role definitions
// eligible for server.auth.default_user_role, sorted by name.
func listEligibleDefaultRoles(ctx context.Context, st store.Store) []EligibleDefaultRole {
	if st == nil {
		return nil
	}
	defs, err := st.ListRoleDefinitions(ctx)
	if err != nil {
		slog.Warn("GET server-config: failed to list role definitions for eligible_default_roles", "error", err)
		return nil
	}
	var out []EligibleDefaultRole
	for _, rd := range defs {
		if checkCustomDefaultUserRoleEligible(rd) == nil {
			out = append(out, EligibleDefaultRole{
				ID:          rd.ID,
				Name:        rd.Name,
				Description: rd.Description,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// populateDefaultUserRoleView attaches default_user_role_warning (when a
// fallback is active) and eligible_default_roles (when the experiment is
// enabled) to a GET /api/v1/admin/server-config response.
func (s *Server) populateDefaultUserRoleView(ctx context.Context, resp *ServerConfigResponse) {
	if resp == nil {
		return
	}
	customEnabled := s.experimentEnabled(experiments.CustomDefaultUserRole)
	configured := ""
	if resp.Server != nil && resp.Server.Auth != nil {
		configured = resp.Server.Auth.DefaultUserRole
	}
	res := resolveDefaultUserRole(ctx, s.store, configured, customEnabled, false)
	resp.DefaultUserRoleWarning = res.Warning
	if customEnabled && s.store != nil {
		resp.EligibleDefaultRoles = listEligibleDefaultRoles(ctx, s.store)
	}
}
