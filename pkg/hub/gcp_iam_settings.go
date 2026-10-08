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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The GCP service-account permission-check settings
// (server.hub.gcp_iam_check_mode, server.hub.gcp_iam_deny_unknown_policy)
// are reloadable operational settings in the gcp_iam section:
//
//   - A save is validated: each sent key must be a recognised, non-empty
//     value (validateGCPIAMRequest).
//   - Only a hub admin may change them (requireGCPIAMHubAdmin), by a save
//     or a section reset. A stored value overrides the deploy-time value
//     in either direction.
//   - A save or section reset that would move either key to its less strict
//     value is refused while hub-scoped service-account assignment is
//     configured (checkGCPIAMTransition).
//   - Every change is recorded in the mutation audit log before it is
//     written (auditGCPIAMChange), with the actor, old and new value, time
//     and surface. If the record cannot be written, the change is not made.
//   - ApplySnapshot applies the stored pair under s.mu
//     (applyGCPIAMSettingsLocked) only after decideGCPIAMReload admits the
//     change. A change made by a guarded write on this replica is admitted
//     directly (approveGCPIAMTransition). Any other change (another
//     replica's write, startup) must have usable values, pass the
//     transition rule, be attributable to the latest audit records for the
//     changed keys, and get its own audit record; otherwise it is refused
//     and the applied pair stays. When a reload cannot read the store no
//     snapshot is applied, so the applied pair stays too.

const (
	gcpIAMSection             = "gcp_iam"
	gcpIAMCheckModeKey        = "server.hub.gcp_iam_check_mode"
	gcpIAMDenyUnknownKey      = "server.hub.gcp_iam_deny_unknown_policy"
	gcpIAMDenyUnknownFailOpen = "fail-open"
	gcpIAMDenyUnknownFailShut = "fail-closed"

	// gcpIAMAuditMutation is the mutation audit type of a change to either
	// key; TargetID is the koanf key. gcpIAMApplyMutation records a replica
	// applying such a change on reload.
	gcpIAMAuditMutation = "hub_setting_update"
	gcpIAMApplyMutation = "hub_setting_apply"
	// gcpIAMRefusedMutation records a reload that was refused (the applied
	// pair stays); its actor is the hub itself.
	gcpIAMRefusedMutation = "hub_setting_apply_refused"
	// gcpIAMNotAppliedMutation records that a write recorded as
	// gcpIAMAuditMutation was not made; TargetID is the section and the
	// summaries are the "check_mode,deny_unknown_policy" pairs.
	gcpIAMNotAppliedMutation = gcpIAMAuditMutation + "_not_applied"
	gcpIAMSystemActorKind    = "system"
	gcpIAMSystemActorID      = "hub-settings-reload"
	gcpIAMAuditTarget        = "hub_setting"
	// gcpIAMSurfaceKind is the ExecutorKind of every gcp_iam audit record;
	// ExecutorID names the surface.
	gcpIAMSurfaceKind     = "hub_settings"
	gcpIAMSurfaceSave     = "server-config"
	gcpIAMSurfaceReset    = "section-reset"
	gcpIAMSurfaceReload   = "reload"
	gcpIAMReloadIOTimeout = 10 * time.Second
)

// gcpIAMTransition is a change of the applied pair.
type gcpIAMTransition struct{ From, To gcpIAMSettings }

// gcpIAMSettings is an effective pair of GCP permission-check settings.
type gcpIAMSettings struct {
	CheckMode           string // SAAssignCheckOff or SAAssignCheckEnforce
	DenyUnknownFailOpen bool
}

// denyUnknownPolicy returns the setting value for DenyUnknownFailOpen.
func (g gcpIAMSettings) denyUnknownPolicy() string {
	if g.DenyUnknownFailOpen {
		return gcpIAMDenyUnknownFailOpen
	}
	return gcpIAMDenyUnknownFailShut
}

// parseGCPIAMCheckMode reports whether v is a recognised check mode.
func parseGCPIAMCheckMode(v string) (string, bool) {
	switch v {
	case SAAssignCheckOff, SAAssignCheckEnforce:
		return v, true
	}
	return "", false
}

// parseGCPIAMDenyUnknownPolicy reports whether v is a recognised policy and
// whether it is the fail-open one.
func parseGCPIAMDenyUnknownPolicy(v string) (failOpen, ok bool) {
	switch v {
	case gcpIAMDenyUnknownFailOpen:
		return true, true
	case gcpIAMDenyUnknownFailShut:
		return false, true
	}
	return false, false
}

// startupGCPIAMSettings resolves the deploy-time values (settings file and
// environment). An unset key takes its documented default (off,
// fail-open); a value that is set but not recognised takes the stricter
// value (enforce, fail-closed), with a warning.
func startupGCPIAMSettings(checkMode, denyPolicy string) gcpIAMSettings {
	out := gcpIAMSettings{CheckMode: SAAssignCheckOff, DenyUnknownFailOpen: true}
	if checkMode != "" {
		if m, ok := parseGCPIAMCheckMode(checkMode); ok {
			out.CheckMode = m
		} else {
			slog.Warn("unrecognised gcpIamCheckMode value, using enforce", "value", checkMode)
			out.CheckMode = SAAssignCheckEnforce
		}
	}
	if denyPolicy != "" {
		if failOpen, ok := parseGCPIAMDenyUnknownPolicy(denyPolicy); ok {
			out.DenyUnknownFailOpen = failOpen
		} else {
			slog.Warn("unrecognised gcpIamDenyUnknownPolicy value, using fail-closed", "value", denyPolicy)
			out.DenyUnknownFailOpen = false
		}
	}
	return out
}

// resolveGCPIAMSettings returns the effective pair for the stored values
// checkMode and denyPolicy. base is the deploy-time pair. A key that is
// empty (no stored value) or not recognised resolves to its base value,
// never to a less strict default; each unrecognised value adds a warning.
//
// A usable stored value overrides the deploy-time value in either
// direction; writes are limited to hub admins (requireGCPIAMHubAdmin).
func resolveGCPIAMSettings(checkMode, denyPolicy string, base gcpIAMSettings) (gcpIAMSettings, []string) {
	out := base
	var warns []string
	if checkMode != "" {
		if m, ok := parseGCPIAMCheckMode(checkMode); ok {
			out.CheckMode = m
		} else {
			warns = append(warns, fmt.Sprintf("unrecognised stored %s %q, using deploy-time value %q",
				gcpIAMCheckModeKey, checkMode, base.CheckMode))
		}
	}
	if denyPolicy != "" {
		if failOpen, ok := parseGCPIAMDenyUnknownPolicy(denyPolicy); ok {
			out.DenyUnknownFailOpen = failOpen
		} else {
			warns = append(warns, fmt.Sprintf("unrecognised stored %s %q, using deploy-time value %q",
				gcpIAMDenyUnknownKey, denyPolicy, base.denyUnknownPolicy()))
		}
	}
	return out, warns
}

// gcpIAMSettingsLocked returns the applied pair. The caller holds s.mu.
func (s *Server) gcpIAMSettingsLocked() gcpIAMSettings {
	return gcpIAMSettings{
		CheckMode:           s.saAssignCheckMode,
		DenyUnknownFailOpen: s.denyUnknownFailOpen.Load(),
	}
}

// gcpIAMBaseLocked returns the deploy-time pair. A Server not built by New
// has none; the applied pair stands in for it. The caller holds s.mu.
func (s *Server) gcpIAMBaseLocked() gcpIAMSettings {
	if s.gcpIAMStartup.CheckMode == "" {
		return s.gcpIAMSettingsLocked()
	}
	return s.gcpIAMStartup
}

// currentGCPIAMSettings returns the applied pair.
func (s *Server) currentGCPIAMSettings() gcpIAMSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gcpIAMSettingsLocked()
}

// applyGCPIAMSettingsLocked applies next to both caller-permission surfaces
// as one step and reports whether anything changed. The caller holds s.mu
// for writing, so no reader sees one key updated and the other not.
func (s *Server) applyGCPIAMSettingsLocked(next gcpIAMSettings) bool {
	cur := s.gcpIAMSettingsLocked()
	if cur == next && s.hookIdentityCheckMode == next.CheckMode {
		return false
	}
	s.saAssignCheckMode = next.CheckMode
	s.hookIdentityCheckMode = next.CheckMode
	s.denyUnknownFailOpen.Store(next.DenyUnknownFailOpen)
	slog.Info("GCP caller-permission settings applied",
		"check_mode", next.CheckMode, "deny_unknown_policy", next.denyUnknownPolicy(),
		"previous_check_mode", cur.CheckMode, "previous_deny_unknown_policy", cur.denyUnknownPolicy())
	return true
}

// invalidateCallerPermissionCaches drops every cached caller-permission
// decision, so none made under the previous settings is reused. Must not
// be called with s.mu held.
func (s *Server) invalidateCallerPermissionCaches() {
	s.mu.RLock()
	assignChecker := s.saAssignChecker
	hookChecker := s.hookIdentityChecker
	s.mu.RUnlock()
	if c, ok := assignChecker.(*CachedCallerPermissionChecker); ok {
		c.InvalidateAll()
	}
	if c, ok := hookChecker.(*CachedCallerPermissionChecker); ok && hookChecker != assignChecker {
		c.InvalidateAll()
	}
}

// gcpIAMRequestValues returns the gcp_iam keys present in a server-config
// PUT body (by JSON presence, so an explicit "" or null is reported) and
// their raw JSON values.
func gcpIAMRequestValues(rawBody []byte) map[string]json.RawMessage {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for _, key := range []string{gcpIAMCheckModeKey, gcpIAMDenyUnknownKey} {
		if v, ok := rawAtPath(top, strings.Split(key, ".")); ok {
			out[key] = v
		}
	}
	return out
}

// validateGCPIAMRequest rejects a sent gcp_iam key whose value is not a
// recognised non-empty string. It writes a 422 and returns false on
// failure.
func validateGCPIAMRequest(w http.ResponseWriter, rawBody []byte) bool {
	for key, raw := range gcpIAMRequestValues(rawBody) {
		var v string
		valid := json.Unmarshal(raw, &v) == nil && v != ""
		allowed := opsettings.GCPIAMCheckModes
		if key == gcpIAMDenyUnknownKey {
			allowed = opsettings.GCPIAMDenyUnknownPolicies
		}
		if valid {
			if key == gcpIAMCheckModeKey {
				_, valid = parseGCPIAMCheckMode(v)
			} else {
				_, valid = parseGCPIAMDenyUnknownPolicy(v)
			}
		}
		if !valid {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError,
				fmt.Sprintf("invalid %s %s: must be one of %s", key, string(raw), strings.Join(allowed, ", ")), nil)
			return false
		}
	}
	return true
}

// buildGCPIAMDoc returns the gcp_iam section document for a PUT: the keys
// the body sends over the current stored row (a key the body omits keeps
// its stored value), and the stored row's revision for the CAS write (0
// when no row exists). The body must have passed validateGCPIAMRequest.
func buildGCPIAMDoc(ctx context.Context, ops *OperationalSettings, req *ServerConfigUpdateRequest, rawBody []byte) (opsettings.GCPIAMSettings, int64, error) {
	var doc opsettings.GCPIAMSettings
	var rev int64
	row, err := ops.store.GetHubSetting(ctx, gcpIAMSection)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return doc, 0, fmt.Errorf("reading current %s row: %w", gcpIAMSection, err)
	default:
		rev = row.Revision
		if len(row.Value) > 0 {
			// A stored row that cannot be decoded contributes nothing; the
			// keys the body sends replace it.
			_ = json.Unmarshal(row.Value, &doc)
		}
		if row.Origin != "managed" {
			for _, k := range ops.EnvOverriddenKeys() {
				switch k {
				case gcpIAMCheckModeKey:
					doc.CheckMode = ""
				case gcpIAMDenyUnknownKey:
					doc.DenyUnknownPolicy = ""
				}
			}
		}
	}
	sent := gcpIAMRequestValues(rawBody)
	if req.Server != nil && req.Server.Hub != nil {
		if _, ok := sent[gcpIAMCheckModeKey]; ok {
			doc.CheckMode = req.Server.Hub.GCPIAMCheckMode
		}
		if _, ok := sent[gcpIAMDenyUnknownKey]; ok {
			doc.DenyUnknownPolicy = req.Server.Hub.GCPIAMDenyUnknownPolicy
		}
	}
	return doc, rev, nil
}

// gcpIAMEffectiveFor returns the pair a stored gcp_iam document would
// apply on this hub (nil: no row).
func (s *Server) gcpIAMEffectiveFor(doc *opsettings.GCPIAMSettings) gcpIAMSettings {
	s.mu.RLock()
	base := s.gcpIAMBaseLocked()
	s.mu.RUnlock()
	if doc == nil {
		return base
	}
	next, _ := resolveGCPIAMSettings(doc.CheckMode, doc.DenyUnknownPolicy, base)
	return next
}

// gcpIAMRelaxes reports whether moving from cur to next makes either key
// less strict.
func gcpIAMRelaxes(cur, next gcpIAMSettings) bool {
	return (cur.CheckMode == SAAssignCheckEnforce && next.CheckMode != SAAssignCheckEnforce) ||
		(!cur.DenyUnknownFailOpen && next.DenyUnknownFailOpen)
}

// errGCPIAMRelaxRefused is returned by checkGCPIAMTransition when the
// change is refused.
var errGCPIAMRelaxRefused = errors.New("this change makes the GCP service account permission check less strict, " +
	"and hub-scoped service account assignment is configured on this hub; " +
	"remove the hub default GCP identity and all hub-scoped service accounts first")

// checkGCPIAMTransition decides whether the applied pair may move from cur
// to next. A change that makes either key less strict is refused while
// hub-scoped service-account assignment is configured: the hub default GCP
// identity mode is "assign", or a hub-scoped service account is
// registered (agents and project defaults may reference it). pendingAssign
// reports that the same request sets the hub default mode to "assign". A
// store error while checking refuses the change too.
func (s *Server) checkGCPIAMTransition(ctx context.Context, cur, next gcpIAMSettings, pendingAssign bool) error {
	if !gcpIAMRelaxes(cur, next) {
		return nil
	}
	if pendingAssign || s.hubAgentDefaults().DefaultGCPIdentityMode == store.GCPMetadataModeAssign {
		return errGCPIAMRelaxRefused
	}
	n, err := s.store.CountGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{Scope: store.ScopeHub})
	if err != nil {
		return fmt.Errorf("checking for hub-scoped service accounts: %w", err)
	}
	if n > 0 {
		return errGCPIAMRelaxRefused
	}
	return nil
}

// gcpIAMHubAdmin reports whether the caller may change the gcp_iam
// settings: a hub user (not federated, not an agent or broker) with the
// admin role, on an interactive session or dev credential. A user access
// token is refused even for an admin.
func gcpIAMHubAdmin(ctx context.Context) bool {
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return false
	}
	switch principalContextForIdentity(identity).Kind {
	case PrincipalKindUser, PrincipalKindDev:
	default:
		return false
	}
	user, ok := identity.(UserIdentity)
	if !ok || user.Role() != store.UserRoleAdmin {
		return false
	}
	return sessionCredentialAllowed(ctx)
}

// requireGCPIAMHubAdmin writes a 403 and returns false unless gcpIAMHubAdmin.
func requireGCPIAMHubAdmin(w http.ResponseWriter, ctx context.Context) bool {
	if gcpIAMHubAdmin(ctx) {
		return true
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"only a hub admin can change the GCP service account permission check settings", nil)
	return false
}

// writeGCPIAMTransitionError writes the response for a refused transition.
func writeGCPIAMTransitionError(w http.ResponseWriter, err error) {
	if errors.Is(err, errGCPIAMRelaxRefused) {
		writeError(w, http.StatusConflict, "gcp_iam_change_refused", err.Error(), nil)
		return
	}
	slog.Error("GCP permission-check settings: transition check failed", "error", err)
	writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to check the settings change", nil)
}

// auditGCPIAMChange records one mutation audit row per key that differs
// between cur and next (principal and credential from ctx, time, old and new
// value). It writes synchronously and returns the first store error, so a
// change whose record cannot be written is not made.
func (s *Server) auditGCPIAMChange(ctx context.Context, surface string, cur, next gcpIAMSettings) error {
	return s.writeGCPIAMAudit(ctx, gcpIAMAuditMutation, surface, cur, next, nil)
}

// writeGCPIAMAudit writes one record per changed key. actor, when set,
// supplies the principal and credential (a reload applying another
// replica's change); otherwise they come from ctx.
func (s *Server) writeGCPIAMAudit(ctx context.Context, mutation, surface string, cur, next gcpIAMSettings, actor *store.MutationAuditRecord) error {
	if s.store == nil {
		return errors.New("no store for the audit record")
	}
	type change struct{ key, before, after string }
	var changes []change
	if cur.CheckMode != next.CheckMode {
		changes = append(changes, change{gcpIAMCheckModeKey, cur.CheckMode, next.CheckMode})
	}
	if cur.DenyUnknownFailOpen != next.DenyUnknownFailOpen {
		changes = append(changes, change{gcpIAMDenyUnknownKey, cur.denyUnknownPolicy(), next.denyUnknownPolicy()})
	}
	for _, c := range changes {
		rec := &store.MutationAuditRecord{
			MutationType:  mutation,
			TargetType:    gcpIAMAuditTarget,
			TargetID:      c.key,
			BeforeSummary: c.before,
			AfterSummary:  c.after,
			ExecutorKind:  gcpIAMSurfaceKind,
			ExecutorID:    surface,
		}
		if actor != nil {
			rec.ActorPrincipalKind = actor.ActorPrincipalKind
			rec.ActorPrincipalID = actor.ActorPrincipalID
			rec.ActorCredentialID = actor.ActorCredentialID
			rec.ActorCredentialType = actor.ActorCredentialType
			rec.CredentialName = actor.CredentialName
		} else {
			auditActorFromContext(ctx).ApplyActor(rec)
		}
		if rec.ActorPrincipalKind == "" || rec.ActorPrincipalID == "" {
			return fmt.Errorf("recording %s change: no actor", c.key)
		}
		rec.Timestamp = time.Now().UTC()
		if err := s.store.CreateMutationAudit(ctx, rec); err != nil {
			return fmt.Errorf("recording %s change: %w", c.key, err)
		}
		slog.Warn("GCP permission-check setting changed",
			"key", c.key, "old", c.before, "new", c.after, "surface", surface,
			"principal_kind", rec.ActorPrincipalKind, "principal", rec.ActorPrincipalID,
			"audit_id", rec.ID)
	}
	return nil
}

// auditGCPIAMNotApplied records that a change recorded by
// auditGCPIAMChange was not written (the section write failed). The
// record is written through the store before it returns, like
// writeGCPIAMAudit, so a reload on any replica that reads the audit
// table after the handler responds sees it. A failed write is logged and
// does not change the response.
func (s *Server) auditGCPIAMNotApplied(ctx context.Context, cur, next gcpIAMSettings, cause error) {
	slog.Error("GCP permission-check setting change was recorded but not written",
		"check_mode", next.CheckMode, "deny_unknown_policy", next.denyUnknownPolicy(), "error", cause)
	rec := &store.MutationAuditRecord{
		MutationType:  gcpIAMNotAppliedMutation,
		TargetType:    gcpIAMAuditTarget,
		TargetID:      gcpIAMSection,
		BeforeSummary: cur.CheckMode + "," + cur.denyUnknownPolicy(),
		AfterSummary:  next.CheckMode + "," + next.denyUnknownPolicy(),
	}
	auditActorFromContext(ctx).ApplyActor(rec)
	rec.Timestamp = time.Now()
	if s.store == nil {
		slog.Warn("failed to emit mutation audit record",
			"mutation_type", rec.MutationType, "error", "no store")
		return
	}
	// Detached from request cancellation, bounded like emitMutationAudit.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := s.store.CreateMutationAudit(writeCtx, rec); err != nil {
		slog.Warn("failed to emit mutation audit record",
			"mutation_type", rec.MutationType, "error", err)
	}
}

// approveGCPIAMTransition admits t for the next ApplySnapshot on this
// replica: the caller has validated, checked, authorized and audited it.
// clearGCPIAMApproval withdraws it (the write failed).
func (s *Server) approveGCPIAMTransition(t gcpIAMTransition) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcpIAMApproved = &t
}

func (s *Server) clearGCPIAMApproval() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcpIAMApproved = nil
}

// decideGCPIAMReload decides, outside s.mu, whether ApplySnapshot may move
// the applied pair to the values snap carries. It returns the admitted
// transition and true, or false to keep the applied pair. Every refusal is
// logged at error level.
func (s *Server) decideGCPIAMReload(snap Layer1Snapshot) (gcpIAMTransition, bool) {
	s.mu.RLock()
	cur := s.gcpIAMSettingsLocked()
	base := s.gcpIAMBaseLocked()
	approved := s.gcpIAMApproved
	s.mu.RUnlock()

	cand, warns := resolveGCPIAMSettings(snap.GCPIAMCheckMode, snap.GCPIAMDenyUnknownPolicy, base)
	t := gcpIAMTransition{From: cur, To: cand}
	if cand == cur {
		return t, false
	}
	refuse := func(reason string, err error) (gcpIAMTransition, bool) {
		slog.Error("GCP permission-check settings: reload refused, keeping the applied values",
			"reason", reason, "error", err,
			"check_mode", cur.CheckMode, "deny_unknown_policy", cur.denyUnknownPolicy(),
			"stored_check_mode", snap.GCPIAMCheckMode, "stored_deny_unknown_policy", snap.GCPIAMDenyUnknownPolicy)
		s.recordGCPIAMRefusal(cur, snap, reason)
		return t, false
	}
	if len(warns) > 0 {
		return refuse("stored value cannot be used", errors.New(strings.Join(warns, "; ")))
	}
	if approved != nil && *approved == t {
		return t, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), gcpIAMReloadIOTimeout)
	defer cancel()
	if s.store == nil {
		return refuse("no store", nil)
	}
	pendingAssign := snap.DefaultGCPIdentityMode == store.GCPMetadataModeAssign
	if err := s.checkGCPIAMTransition(ctx, cur, cand, pendingAssign); err != nil {
		return refuse("transition rule", err)
	}
	// A change that makes neither key less strict is applied even when no
	// audited write names it (for example after audit retention removed
	// the record); the hub is then recorded as the actor.
	actor, err := s.gcpIAMAttribution(ctx, cur, cand)
	if err != nil {
		if gcpIAMRelaxes(cur, cand) {
			return refuse("change is not attributable to an audited write", err)
		}
		actor = &store.MutationAuditRecord{
			ActorPrincipalKind: gcpIAMSystemActorKind,
			ActorPrincipalID:   gcpIAMSystemActorID,
		}
	}
	if err := s.writeGCPIAMAudit(ctx, gcpIAMApplyMutation, gcpIAMSurfaceReload, cur, cand, actor); err != nil {
		return refuse("audit record", err)
	}
	return t, true
}

// gcpIAMAttribution returns the audited write that a reload from cur to
// next applies: for every changed key, the latest hub_setting_update record
// must name the new value, and no later not-applied record may name the
// same change. It returns the actor of the latest such record.
func (s *Server) gcpIAMAttribution(ctx context.Context, cur, next gcpIAMSettings) (*store.MutationAuditRecord, error) {
	// idx is the key's position in a not-applied record's summaries.
	type change struct {
		key, after string
		idx        int
	}
	var changes []change
	if cur.CheckMode != next.CheckMode {
		changes = append(changes, change{gcpIAMCheckModeKey, next.CheckMode, 0})
	}
	if cur.DenyUnknownFailOpen != next.DenyUnknownFailOpen {
		changes = append(changes, change{gcpIAMDenyUnknownKey, next.denyUnknownPolicy(), 1})
	}
	notApplied, _, err := s.store.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: gcpIAMNotAppliedMutation,
		TargetType:   gcpIAMAuditTarget,
		TargetID:     gcpIAMSection,
	})
	if err != nil {
		return nil, fmt.Errorf("reading not-applied audit records: %w", err)
	}
	var actor *store.MutationAuditRecord
	for _, c := range changes {
		recs, _, err := s.store.ListMutationAudits(ctx, store.MutationAuditFilter{
			MutationType: gcpIAMAuditMutation,
			TargetType:   gcpIAMAuditTarget,
			TargetID:     c.key,
		})
		if err != nil {
			return nil, fmt.Errorf("reading audit records for %s: %w", c.key, err)
		}
		// The latest records (several may share the latest timestamp).
		var latest []*store.MutationAuditRecord
		for _, r := range recs {
			switch {
			case len(latest) == 0 || r.Timestamp.After(latest[0].Timestamp):
				latest = []*store.MutationAuditRecord{r}
			case r.Timestamp.Equal(latest[0].Timestamp):
				latest = append(latest, r)
			}
		}
		var match *store.MutationAuditRecord
		for _, r := range latest {
			if r.AfterSummary == c.after {
				match = r
			}
		}
		if match == nil {
			return nil, fmt.Errorf("no audit record sets %s to %q", c.key, c.after)
		}
		for _, na := range notApplied {
			if na.Timestamp.Before(match.Timestamp) {
				continue
			}
			before := strings.Split(na.BeforeSummary, ",")
			after := strings.Split(na.AfterSummary, ",")
			if len(before) == 2 && len(after) == 2 &&
				before[c.idx] != after[c.idx] && after[c.idx] == c.after {
				return nil, fmt.Errorf("the latest audit record setting %s to %q was not applied", c.key, c.after)
			}
		}
		if actor == nil || match.Timestamp.After(actor.Timestamp) {
			actor = match
		}
	}
	return actor, nil
}

// recordGCPIAMRefusal writes an audit record for a refused reload (a stored
// value that cannot be used, a row that changed or disappeared without an
// audited write, a transition the rule refuses, or a failed check). The
// same refusal is recorded once, not on every later reload.
func (s *Server) recordGCPIAMRefusal(cur gcpIAMSettings, snap Layer1Snapshot, reason string) {
	stored := fmt.Sprintf("stored %s=%q %s=%q; %s", gcpIAMCheckModeKey, snap.GCPIAMCheckMode,
		gcpIAMDenyUnknownKey, snap.GCPIAMDenyUnknownPolicy, reason)
	applied := cur.CheckMode + "," + cur.denyUnknownPolicy()
	key := applied + "|" + stored
	s.mu.Lock()
	dup := s.gcpIAMLastRefusal == key
	s.gcpIAMLastRefusal = key
	st := s.store
	s.mu.Unlock()
	if dup || st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gcpIAMReloadIOTimeout)
	defer cancel()
	rec := &store.MutationAuditRecord{
		MutationType:       gcpIAMRefusedMutation,
		TargetType:         gcpIAMAuditTarget,
		TargetID:           gcpIAMSection,
		BeforeSummary:      applied,
		AfterSummary:       stored,
		ActorPrincipalKind: gcpIAMSystemActorKind,
		ActorPrincipalID:   gcpIAMSystemActorID,
		ExecutorKind:       gcpIAMSurfaceKind,
		ExecutorID:         gcpIAMSurfaceReload,
		Timestamp:          time.Now().UTC(),
	}
	if err := st.CreateMutationAudit(ctx, rec); err != nil {
		s.mu.Lock()
		s.gcpIAMLastRefusal = "" // retry on the next reload
		s.mu.Unlock()
		slog.Error("GCP permission-check settings: failed to record a refused reload", "error", err)
	}
}
