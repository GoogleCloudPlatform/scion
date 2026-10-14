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
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Residue report of a test identity with an admin role (ptone/scion#4240,
// admin tier phase A3; design §4.5).
//
// The report lists what the identity left on the hub:
//
//   - every grant row stamped with its issuance (StampedGrants) and every
//     resource stamped with it as the provenance root (StampedResources):
//     the purge removes these;
//   - every mutation the identity, or an agent rooted at it, made to a row
//     that does not carry its stamp (UnstampedChanges): for example a human
//     it suspended or a project it deleted. These are reported and never
//     reverted.
//
// Limit: the report is only as complete as the mutation audit. A write
// path that records no mutation audit row leaves no trace here.
//
// It is computed at the start of each purge of an admin-kind identity (in
// the purge report), and served on GET
// /api/v1/test-identities/admin/{id}/residue to a human super-admin. When
// a purge's report lists unstamped changes a test_admin_residue line is
// written to the test admin audit sink (a read of the route writes none).

// Test admin audit events of A3.
const (
	// testAdminAuditEventPurge is written when an admin-kind identity's
	// purge finishes a run.
	testAdminAuditEventPurge = "test_admin_purge"
	// testAdminAuditEventResidue is written when an admin-kind identity's
	// residue report lists changes to rows without its stamp.
	testAdminAuditEventResidue = "test_admin_residue"
)

// testAdminResidueAuditLimit bounds the audit records one report reads per
// actor; a report that hits it is Truncated.
const testAdminResidueAuditLimit = 1000

// TestAdminResidueResources lists the IDs of resources stamped with the
// identity as their provenance root.
type TestAdminResidueResources struct {
	Agents    []string `json:"agents"`
	Schedules []string `json:"schedules"`
	Groups    []string `json:"groups"`
	Projects  []string `json:"projects"`
}

// TestAdminResidueChange is one mutation the identity (or an agent rooted
// at it) made to a row without its stamp.
type TestAdminResidueChange struct {
	MutationType string    `json:"mutationType"`
	TargetType   string    `json:"targetType"`
	TargetID     string    `json:"targetId"`
	ActorKind    string    `json:"actorKind"`
	ActorID      string    `json:"actorId"`
	Timestamp    time.Time `json:"timestamp"`
}

// TestAdminResidueReport is the residue report of one admin-kind identity.
type TestAdminResidueReport struct {
	IdentityID       string                       `json:"identityId"`
	IssuanceID       string                       `json:"issuanceId"`
	StampedGrants    []store.IssuanceStampedGrant `json:"stampedGrants"`
	StampedResources TestAdminResidueResources    `json:"stampedResources"`
	UnstampedChanges []TestAdminResidueChange     `json:"unstampedChanges"`
	// KeptRoles lists the custom roles stamped with the issuance that a
	// role binding without the stamp names: the purge keeps them (their
	// content is the identity's; the binding is not).
	KeptRoles []store.IssuanceStampedGrant `json:"keptRoles"`
	// Truncated is true when a list hit its bound.
	Truncated bool `json:"truncated,omitempty"`
	// Limit states what the report can see.
	Limit string `json:"limit"`
}

// testAdminResidueLimitText is TestAdminResidueReport.Limit.
const testAdminResidueLimitText = "complete only as far as the mutation audit: changes are listed from the audit records of the identity and its agents"

// testAdminResidue computes the residue report of the admin-kind identity
// identityID with issuance issuanceID, whether or not its row still
// exists. A change is unstamped when its target is neither a row stamped
// now nor a row an earlier purge run of the identity removed (that run's
// audit records carry the executor test_identity:<id>).
func (s *Server) testAdminResidue(ctx context.Context, identityID, issuanceID string) (*TestAdminResidueReport, error) {
	rep := &TestAdminResidueReport{
		IdentityID: identityID, IssuanceID: issuanceID,
		StampedGrants:    []store.IssuanceStampedGrant{},
		StampedResources: TestAdminResidueResources{Agents: []string{}, Schedules: []string{}, Groups: []string{}, Projects: []string{}},
		UnstampedChanges: []TestAdminResidueChange{},
		KeptRoles:        []store.IssuanceStampedGrant{},
		Limit:            testAdminResidueLimitText,
	}
	stamped := map[string]bool{identityID: true}

	grants, truncated, err := s.store.ListIssuanceStampedGrants(ctx, issuanceID, fixturePurgeListLimit)
	if err != nil {
		return nil, err
	}
	rep.Truncated = rep.Truncated || truncated
	for _, g := range grants {
		rep.StampedGrants = append(rep.StampedGrants, g)
		stamped[g.ID] = true
		if g.Kind == store.IssuanceGrantRoleDefinition && g.BoundOutsideIssuance {
			rep.KeptRoles = append(rep.KeptRoles, g)
		}
	}

	actors := []string{identityID}
	set, err := s.store.ListFixtureRootedResources(ctx, identityID, fixturePurgeListLimit)
	if err != nil && !errors.Is(err, store.ErrInvalidInput) {
		return nil, err
	}
	if set != nil {
		rep.Truncated = rep.Truncated || set.Truncated
		for _, a := range set.Agents {
			rep.StampedResources.Agents = append(rep.StampedResources.Agents, a.ID)
			stamped[a.ID] = true
			actors = append(actors, a.ID)
		}
		for _, sc := range set.Schedules {
			rep.StampedResources.Schedules = append(rep.StampedResources.Schedules, sc.ID)
			stamped[sc.ID] = true
		}
		for _, g := range set.Groups {
			rep.StampedResources.Groups = append(rep.StampedResources.Groups, g.ID)
			stamped[g.ID] = true
		}
		for _, p := range set.Projects {
			rep.StampedResources.Projects = append(rep.StampedResources.Projects, p.ID)
			stamped[p.ID] = true
		}
	}

	// Rows earlier purge runs removed were stamped; agents among them
	// were also actors.
	purged, _, err := s.store.ListMutationAudits(ctx, store.MutationAuditFilter{
		ExecutorID: "test_identity:" + identityID,
		Limit:      testAdminResidueAuditLimit,
	})
	if err != nil {
		return nil, err
	}
	if len(purged) >= testAdminResidueAuditLimit {
		rep.Truncated = true
	}
	for _, r := range purged {
		if r.TargetID == "" {
			continue
		}
		if !stamped[r.TargetID] && r.TargetType == "agent" {
			actors = append(actors, r.TargetID)
		}
		stamped[r.TargetID] = true
	}

	for _, actor := range actors {
		recs, _, err := s.store.ListMutationAudits(ctx, store.MutationAuditFilter{
			ActorPrincipalID: actor,
			Limit:            testAdminResidueAuditLimit,
		})
		if err != nil {
			return nil, err
		}
		if len(recs) >= testAdminResidueAuditLimit {
			rep.Truncated = true
		}
		for _, r := range recs {
			if r.TargetID == "" || stamped[r.TargetID] {
				continue
			}
			rep.UnstampedChanges = append(rep.UnstampedChanges, TestAdminResidueChange{
				MutationType: r.MutationType, TargetType: r.TargetType, TargetID: r.TargetID,
				ActorKind: r.ActorPrincipalKind, ActorID: r.ActorPrincipalID, Timestamp: r.Timestamp,
			})
		}
	}
	return rep, nil
}

// emitTestAdminResidueWarning writes the test_admin_residue sink line when
// the report lists unstamped changes, and logs at WARN. A sink failure is
// logged; it never stops a purge.
func (s *Server) emitTestAdminResidueWarning(ctx context.Context, rep *TestAdminResidueReport) {
	if rep == nil || len(rep.UnstampedChanges) == 0 {
		return
	}
	slog.WarnContext(ctx, "test admin identity left changes to rows without its stamp; they are not reverted",
		"test_identity_id", rep.IdentityID, "issuance_id", rep.IssuanceID, "unstamped_changes", len(rep.UnstampedChanges))
	if err := s.testAdminAudit.emit(testAdminAuditEventResidue, map[string]any{
		"level":             "WARN",
		"test_identity_id":  rep.IdentityID,
		"issuance_id":       rep.IssuanceID,
		"unstamped_changes": len(rep.UnstampedChanges),
		"truncated":         rep.Truncated,
		"request_id":        logging.RequestIDFromContext(ctx),
	}); err != nil {
		slog.ErrorContext(ctx, "test admin audit sink write failed", "event", testAdminAuditEventResidue, "error", err)
	}
}

// emitTestAdminPurge writes the test_admin_purge sink line for one purge
// run of an admin-kind identity. A sink failure is logged; it never stops
// a purge.
func (s *Server) emitTestAdminPurge(ctx context.Context, run fixturePurgeRun, report *TestIdentityPurgeReport) {
	if run.issuanceID == "" || report == nil {
		return
	}
	if err := s.testAdminAudit.emit(testAdminAuditEventPurge, map[string]any{
		"test_identity_id": run.rootID,
		"issuance_id":      run.issuanceID,
		"trigger":          run.trigger,
		"executor":         run.executor,
		"complete":         report.Complete,
		"grants":           report.Deleted.Grants,
		"agents":           report.Deleted.Agents,
		"projects":         report.Deleted.Projects,
		"request_id":       logging.RequestIDFromContext(ctx),
	}); err != nil {
		slog.ErrorContext(ctx, "test admin audit sink write failed", "event", testAdminAuditEventPurge, "error", err)
	}
}

// handleTestAdminResidue handles GET
// /api/v1/test-identities/admin/{id}/residue: the residue report of an
// admin-kind identity, live or purged, for a human super-admin on an
// interactive session (or dev credential). An ID that is not an admin-kind
// identity, current or past, gets 404.
func (s *Server) handleTestAdminResidue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.testAdminTierOn() {
		NotFound(w, "endpoint")
		return
	}
	actor, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonCredentialManagement)
	if !ok {
		return
	}
	k, err := s.kindForPrincipal(ctx, actor.ID())
	if err != nil || !k.isHuman() {
		logAuthzDenial(r, actor, Resource{Type: permissions.ResourceTestIdentity}, Action("residue"), "caller is not a human user")
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "the residue report requires a human super-admin session",
			map[string]interface{}{"reason": testAdminReasonCallerNotHuman})
		return
	}
	if s.testAdminPolicyRefusesUser(ctx, actor.ID(), "test_admin_residue.go:handleTestAdminResidue") ||
		!s.authzService.IsSystemAdmin(ctx, actor.ID()) {
		logAuthzDenial(r, actor, Resource{Type: permissions.ResourceTestIdentity}, Action("residue"), "caller is not a super-admin")
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "the residue report requires a human super-admin session", nil)
		return
	}

	id := r.PathValue("id")
	var issuanceID string
	u, err := s.store.GetUser(ctx, id)
	switch {
	case err == nil:
		if !store.IsTestAdminKind(u.Kind) || u.IssuanceID == nil || *u.IssuanceID == "" {
			NotFound(w, "test admin identity")
			return
		}
		issuanceID = *u.IssuanceID
	case errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput):
		iss, _, found, aerr := s.testAdminIssuanceFromAudit(ctx, id)
		if aerr != nil {
			writeErrorFromErr(w, aerr, "")
			return
		}
		if !found {
			NotFound(w, "test admin identity")
			return
		}
		issuanceID = iss
	default:
		writeErrorFromErr(w, err, "")
		return
	}
	rep, err := s.testAdminResidue(ctx, id, issuanceID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	// A read writes no sink line: the WARN is the purge's (design §4.5);
	// the read is in the request lines of the audit sink only when an
	// admin kind makes it, which the route refuses.
	writeJSON(w, http.StatusOK, rep)
}
