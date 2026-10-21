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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Issuer-scoped orphan cleanup and the provenance explain endpoint
// (ptone/scion#4240, Phase 3c).
//
// POST /api/v1/test-identities/cleanup takes no body and no target: the
// server selects every resource whose creation-time provenance stamp
// (fixture_provenance.go) names a test identity that is expired or gone,
// and removes it with the 3b purge engine (purgeTestIdentity), one purge
// per identity, all sharing one run ID. The stamp's fixture_issuer_id
// bounds the selection to the caller:
//
//   - an issuer (session or issuer token) reaches the identities it issued;
//   - a workload delegation request (phase X2) reaches only the identities
//     issued through that same delegation;
//   - an unscoped platform admin session reaches every issuer.
//
// A live identity, and every resource of another issuer, is never touched.
// Each identity is admitted by the same rules as the purge route
// (resolveFixturePurgeRun). Test identities of every kind, the admin kinds
// included, are refused as callers (testIdentityCaller). ?dryRun=true
// writes nothing and lists the targets the real run would remove. A real
// run writes one test_identity_cleanup audit row with the counts, whose
// correlation ID is shared by every per-resource row of the run, each of
// which carries its provenance_basis.
//
// GET /api/v1/test-identities/provenance?resource=<type>:<id> answers
// whether the hub recorded the resource as created under a test identity
// of the caller's scope (the same issuer bound as the cleanup). A resource
// that is gone is proven by its deletion record (a delete row with an
// in-scope provenance_basis), so a run can verify by absence after the
// reaper or a purge. An unstamped resource, a missing one and a stamp of
// another issuer or delegation get the same answer (proven:false, reason
// "no fixture stamp"), with no owner, creator or other field, so the route
// is no existence oracle and leaks no other issuer's stamp.

const (
	// testIdentityCleanupMutation is the audit type of one cleanup run.
	testIdentityCleanupMutation = "test_identity_cleanup"
	// testIdentityCleanupTargetType is the target type of the run row: the
	// issuer scope the run selected by.
	testIdentityCleanupTargetType = "test_identity_issuer"
	// fixturePurgeExecutorCleanup is the executor of the cleanup's deletes.
	fixturePurgeExecutorCleanup = "system:test_identity_cleanup"
	// fixtureCleanupBasisName is provenance_basis.basis of the run row.
	fixtureCleanupBasisName = "fixture_issuer_id"
	// fixtureProvenanceNoStamp is the reason of every unproven answer.
	fixtureProvenanceNoStamp = "no fixture stamp"
	// fixtureProvenanceStamped is the reason of every proven answer.
	fixtureProvenanceStamped = "fixture stamp"
)

// fixtureCleanupRootLimit bounds the identities one cleanup run purges:
// admitted (in the caller's scope) and not live. Live and out-of-scope
// candidates do not count, so they can never fill a run. A run that stops
// at the limit with candidates left is truncated, and the next run, which
// no longer sees the identities this one removed, continues. A variable
// so tests can lower it.
var fixtureCleanupRootLimit = 200

// fixtureCleanupPage is the page size of the candidate reads (stamp roots
// and expired identities).
const fixtureCleanupPage = 500

// Issuer scopes of a cleanup run.
const (
	fixtureCleanupScopeIssuer     = "issuer"
	fixtureCleanupScopeDelegation = "delegation"
	fixtureCleanupScopeAll        = "all"
)

// Target statuses in a cleanup report.
const (
	fixtureCleanupTarget  = "target"
	fixtureCleanupDeleted = "deleted"
	fixtureCleanupNotDone = "not_done"
	fixtureCleanupKept    = "kept"
)

// Identity states in a cleanup report and a provenance answer.
const (
	fixtureStateLive    = "live"
	fixtureStateExpired = "expired"
	fixtureStateAbsent  = "absent"
)

// Provenance resource types.
const (
	fixtureResourceAgent        = "agent"
	fixtureResourceSchedule     = "schedule"
	fixtureResourceGroup        = "group"
	fixtureResourceProject      = "project"
	fixtureResourceTestIdentity = "test_identity"
)

// TestIdentityCleanupTarget is one resource a cleanup run selected.
type TestIdentityCleanupTarget struct {
	// Type is agent, schedule, group, project or test_identity.
	Type string `json:"type"`
	ID   string `json:"id"`
	// FixtureRootID is the test identity the resource is stamped with.
	FixtureRootID string `json:"fixtureRootId"`
	ProjectID     string `json:"projectId,omitempty"`
	// Status is target (dry run), deleted, not_done or kept.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// TestIdentityCleanupIdentity is one test identity a cleanup run reached.
type TestIdentityCleanupIdentity struct {
	ID string `json:"id"`
	// State is expired (its row exists) or absent (its row is gone).
	State string `json:"state"`
	// Report is the identity's purge report (real runs only).
	Report *TestIdentityPurgeReport `json:"report,omitempty"`
	// Error is set when its purge stopped on a store failure.
	Error string `json:"error,omitempty"`
}

// TestIdentityCleanupReport is the result of a cleanup run.
type TestIdentityCleanupReport struct {
	// RunID is the correlation ID of the run's audit rows (real runs only).
	RunID  string `json:"runId,omitempty"`
	DryRun bool   `json:"dryRun"`
	// Scope is issuer, delegation or all.
	Scope string `json:"scope"`
	// Complete is true when every target was removed (kept projects
	// aside) and nothing was truncated. Always false for a dry run.
	Complete   bool                          `json:"complete"`
	Identities []TestIdentityCleanupIdentity `json:"identities"`
	Targets    []TestIdentityCleanupTarget   `json:"targets"`
	Deleted    TestIdentityPurgeCounts       `json:"deleted"`
	// IdentitiesDeleted counts the expired identities whose rows the run
	// deleted.
	IdentitiesDeleted int `json:"identitiesDeleted"`
	// LiveSkipped counts the live identities of the scope that carry
	// stamped resources; the run never touches them.
	LiveSkipped int `json:"liveSkipped"`
	// NotDone and Unproven are collected from the identities' purge
	// reports.
	NotDone  []TestIdentityPurgeItem `json:"notDone,omitempty"`
	Unproven []TestIdentityPurgeItem `json:"unproven,omitempty"`
	// Truncated is true when more identities or resources exist than one
	// run handles; running it again continues.
	Truncated bool `json:"truncated,omitempty"`
}

// TestIdentityProvenance is the answer of the provenance endpoint.
type TestIdentityProvenance struct {
	Resource string `json:"resource"`
	Proven   bool   `json:"proven"`
	// The stamp and the state of its test identity: set only when proven.
	FixtureRootID   string `json:"fixtureRootId,omitempty"`
	FixtureIssuerID string `json:"fixtureIssuerId,omitempty"`
	// FixtureState is live, expired or absent.
	FixtureState string `json:"fixtureState,omitempty"`
	// Deleted is true when the resource is gone and its deletion record
	// (a purge, reaper or cleanup delete row with an in-scope
	// provenance_basis) proves it; DeletionRecord is that row's
	// correlation ID.
	Deleted        bool   `json:"deleted,omitempty"`
	DeletionRecord string `json:"deletionRecord,omitempty"`
	Reason         string `json:"reason"`
}

// fixtureCleanupBasis is the provenance_basis of the run row: the column
// and value the run selected identities by.
type fixtureCleanupBasis struct {
	Basis string `json:"basis"`
	// Scope is issuer, delegation or all; IssuerID is empty for all.
	Scope        string `json:"scope"`
	IssuerID     string `json:"fixture_issuer_id,omitempty"`
	DelegationID string `json:"delegation_id,omitempty"`
	RunID        string `json:"purge_run_id"`
	Trigger      string `json:"trigger"`
}

// fixtureCleanupScope is the issuer bound of one cleanup request.
type fixtureCleanupScope struct {
	name string
	// issuerID filters stamps by fixture_issuer_id; empty for all.
	issuerID     string
	delegationID string
}

// cleanupScopeFor returns the issuer bound of caller's cleanup: every
// issuer for an unscoped platform admin session, the delegation's grantor
// and the delegation for a workload delegation request, and the caller
// itself otherwise. ok is false for a workload credential without its
// delegation state (fail closed).
func (s *Server) cleanupScopeFor(ctx context.Context, caller UserIdentity) (fixtureCleanupScope, bool) {
	if st := workloadDelegationCaller(ctx); st != nil {
		return fixtureCleanupScope{name: fixtureCleanupScopeDelegation, issuerID: st.delegation.GrantorID, delegationID: st.delegation.ID}, true
	}
	if GetCredentialContextFromContext(ctx).Kind == CredentialKindWorkloadDelegation {
		return fixtureCleanupScope{}, false
	}
	if s.testIdentityCallerSeesAll(ctx, caller) {
		return fixtureCleanupScope{name: fixtureCleanupScopeAll}, true
	}
	return fixtureCleanupScope{name: fixtureCleanupScopeIssuer, issuerID: caller.ID()}, true
}

// fixtureCleanupPlan is one identity a cleanup run reaches and the
// resources stamped with it.
type fixtureCleanupPlan struct {
	run     fixturePurgeRun
	state   string
	targets []TestIdentityCleanupTarget
}

// planTestIdentityCleanup selects the identities caller's cleanup reaches
// and their stamped resources. It writes nothing. The candidates are, in
// order, every identity a stamp of the scope names (read in pages by root
// ID) and then the scope's expired identities (member and admin kinds) that
// no stamp names any more, for example because an earlier run removed
// their resources but not the identity. Each candidate is admitted by the
// purge route's rules; live ones are counted and skipped. At most
// fixtureCleanupRootLimit admitted, non-live identities are planned.
func (s *Server) planTestIdentityCleanup(ctx context.Context, caller UserIdentity, scope fixtureCleanupScope, report *TestIdentityCleanupReport) ([]fixtureCleanupPlan, error) {
	p := &fixtureCleanupPlanner{s: s, caller: caller, report: report, now: s.testIdentities.clock(), seen: map[string]bool{}}
	after := ""
	for {
		roots, more, err := s.store.ListFixtureRoots(ctx, scope.issuerID, after, fixtureCleanupPage)
		if err != nil {
			return nil, fmt.Errorf("list stamped identities: %w", err)
		}
		for _, root := range roots {
			if full, err := p.consider(ctx, root.RootID); err != nil || full {
				return p.plans, err
			}
		}
		if !more || len(roots) == 0 {
			break
		}
		after = roots[len(roots)-1].RootID
	}
	q := store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: p.now, IncludeAdminKinds: true, Limit: fixtureCleanupPage}
	for {
		page, err := s.store.ListExpiredTestFixtureUsers(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("list expired test identities: %w", err)
		}
		for i := range page {
			u := &page[i]
			if scope.issuerID != "" && (u.IssuedBy == nil || *u.IssuedBy != scope.issuerID) {
				continue
			}
			if full, err := p.consider(ctx, u.ID); err != nil || full {
				return p.plans, err
			}
		}
		if len(page) < fixtureCleanupPage || page[len(page)-1].ExpiresAt == nil {
			break
		}
		last := page[len(page)-1]
		q.AfterExpiresAt, q.AfterID = *last.ExpiresAt, last.ID
	}
	return p.plans, nil
}

// fixtureCleanupPlanner collects the plans of one cleanup run.
type fixtureCleanupPlanner struct {
	s      *Server
	caller UserIdentity
	report *TestIdentityCleanupReport
	now    time.Time
	seen   map[string]bool
	plans  []fixtureCleanupPlan
}

// consider plans candidate rootID when the caller reaches it and it is not
// live. full is true when the plan limit was already reached before this
// candidate: the run is then truncated and planning stops.
func (p *fixtureCleanupPlanner) consider(ctx context.Context, rootID string) (full bool, err error) {
	if p.seen[rootID] {
		return false, nil
	}
	p.seen[rootID] = true
	var u *store.User
	state := fixtureStateAbsent
	switch got, err := p.s.store.GetUser(ctx, rootID); {
	case err == nil:
		u, state = got, fixtureStateExpired
	case errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput):
	default:
		return false, fmt.Errorf("read test identity %s: %w", rootID, err)
	}
	// The purge route's rules decide whether the caller reaches the
	// identity (its issuer, its own delegation, or an admin session); a row
	// that is no test identity is never reached.
	run, ok, err := p.s.resolveFixturePurgeRun(ctx, p.caller, rootID, u, true)
	if err != nil {
		return false, fmt.Errorf("test identity %s: %w", rootID, err)
	}
	if !ok {
		return false, nil
	}
	if u != nil && (u.ExpiresAt == nil || u.ExpiresAt.After(p.now)) {
		// A live identity is never touched.
		p.report.LiveSkipped++
		return false, nil
	}
	if len(p.plans) >= fixtureCleanupRootLimit {
		p.report.Truncated = true
		return true, nil
	}
	targets, more, err := p.s.fixtureCleanupTargets(ctx, rootID, u)
	if err != nil {
		return false, err
	}
	p.report.Truncated = p.report.Truncated || more
	p.plans = append(p.plans, fixtureCleanupPlan{run: run, state: state, targets: targets})
	return false, nil
}

// fixtureCleanupTargets lists the resources stamped with rootID that its
// purge removes: agents, schedules, groups (a project's own groups go with
// it and are not listed), projects, and the identity's row when it still
// exists.
func (s *Server) fixtureCleanupTargets(ctx context.Context, rootID string, u *store.User) ([]TestIdentityCleanupTarget, bool, error) {
	set, err := s.store.ListFixtureRootedResources(ctx, rootID, fixturePurgeListLimit)
	if err != nil {
		return nil, false, fmt.Errorf("list resources of %s: %w", rootID, err)
	}
	var out []TestIdentityCleanupTarget
	add := func(typ, id, projectID string) {
		out = append(out, TestIdentityCleanupTarget{Type: typ, ID: id, FixtureRootID: rootID, ProjectID: projectID, Status: fixtureCleanupTarget})
	}
	for i := range set.Agents {
		if a := &set.Agents[i]; a.Fixture.RootID == rootID {
			add(fixtureResourceAgent, a.ID, a.ProjectID)
		}
	}
	for i := range set.Schedules {
		if sc := &set.Schedules[i]; sc.Fixture.RootID == rootID {
			add(fixtureResourceSchedule, sc.ID, sc.ProjectID)
		}
	}
	for i := range set.Groups {
		if g := &set.Groups[i]; g.Fixture.RootID == rootID && !isProjectImplicitGroup(g) {
			add(fixtureResourceGroup, g.ID, g.ProjectID)
		}
	}
	for i := range set.Projects {
		if p := &set.Projects[i]; p.Fixture.RootID == rootID {
			add(fixtureResourceProject, p.ID, "")
		}
	}
	if u != nil {
		add(fixtureResourceTestIdentity, u.ID, "")
	}
	return out, set.Truncated, nil
}

// handleTestIdentityCleanup handles POST /api/v1/test-identities/cleanup.
// The only input is ?dryRun=true|false; any other parameter, or a body, is
// refused with 400, so a caller can never name what is removed. A complete
// real run answers 200 with the report, an incomplete one 409
// purge_incomplete with the report in details (running it again
// continues), and a dry run 200 with the targets.
func (s *Server) handleTestIdentityCleanup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.testIdentityCaller(w, r)
	if !ok {
		return
	}
	dryRun := false
	for key, vals := range r.URL.Query() {
		if key != "dryRun" {
			BadRequest(w, fmt.Sprintf("invalid query parameter %q: a cleanup takes only dryRun", key))
			return
		}
		if len(vals) != 1 || (vals[0] != "true" && vals[0] != "false") {
			BadRequest(w, `invalid dryRun: must be "true" or "false"`)
			return
		}
		dryRun = vals[0] == "true"
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, testIdentityMaxBodyBytes+1))
	if err != nil {
		BadRequest(w, "failed to read request body")
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		BadRequest(w, "a cleanup takes no request body: the hub selects what it removes")
		return
	}
	scope, ok := s.cleanupScopeFor(ctx, caller)
	if !ok {
		writeForbiddenStructured(w, "requires test_identity.issue permission", permissions.ResourceTestIdentity, ActionIssue)
		return
	}
	// Its own bucket per issuer (or delegation), so a run's issuance does
	// not use up its cleanup.
	if !s.testIdentityAllow(w, "cleanup:"+testIdentityRateKey(ctx, caller)) {
		return
	}

	report := &TestIdentityCleanupReport{DryRun: dryRun, Scope: scope.name, Identities: []TestIdentityCleanupIdentity{}, Targets: []TestIdentityCleanupTarget{}}
	plans, err := s.planTestIdentityCleanup(ctx, caller, scope, report)
	if err != nil {
		slog.ErrorContext(ctx, "test identity cleanup: selecting targets failed", "error", err)
		InternalError(w)
		return
	}
	if dryRun {
		for _, p := range plans {
			report.Identities = append(report.Identities, TestIdentityCleanupIdentity{ID: p.run.rootID, State: p.state})
			report.Targets = append(report.Targets, p.targets...)
		}
		writeJSON(w, http.StatusOK, report)
		return
	}

	runID := logging.RequestIDFromContext(ctx)
	if runID == "" {
		runID = api.NewUUID()
	}
	report.RunID = runID
	s.runTestIdentityCleanup(ctx, caller, plans, runID, report)

	if err := s.writeTestIdentityCleanupAudit(ctx, scope, report); err != nil {
		slog.ErrorContext(ctx, "test identity cleanup: run audit failed", "run_id", runID, "error", err)
		report.Complete = false
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "test identity cleanup audit failed; run it again",
			map[string]interface{}{"reason": testIdentityReasonPurgeIncomplete, "report": report})
		return
	}
	slog.InfoContext(ctx, "test identity cleanup", "run_id", runID, "scope", scope.name, "by", caller.ID(),
		"identities", len(report.Identities), "complete", report.Complete, "agents", report.Deleted.Agents, "projects", report.Deleted.Projects)
	if !report.Complete {
		writeError(w, http.StatusConflict, ErrCodeConflict, "test identity cleanup did not complete; run it again",
			map[string]interface{}{"reason": testIdentityReasonPurgeIncomplete, "report": report})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// runTestIdentityCleanup purges every planned identity with the 3b engine,
// all under runID, and fills report: the per-identity purge reports, the
// counts, and each target's status (deleted when its row is gone after the
// run, kept or not_done otherwise).
func (s *Server) runTestIdentityCleanup(ctx context.Context, caller UserIdentity, plans []fixtureCleanupPlan, runID string, report *TestIdentityCleanupReport) {
	complete := !report.Truncated
	for _, p := range plans {
		run := p.run
		run.trigger = testIdentityPurgeMutation
		run.basisTrigger = testIdentityCleanupMutation
		run.executor = fixturePurgeExecutorCleanup
		run.requestedBy = caller.ID()
		run.actor = s.buildAuditActorFromContext(ctx)
		run.runID = runID
		entry := TestIdentityCleanupIdentity{ID: run.rootID, State: p.state}
		pr, err := s.purgeTestIdentity(ctx, run)
		if err != nil {
			slog.ErrorContext(ctx, "test identity cleanup: purge failed", "test_identity_id", run.rootID, "run_id", runID, "error", err)
			entry.Error = "purge_failed"
			complete = false
		}
		if pr != nil {
			entry.Report = pr
			report.Deleted.Agents += pr.Deleted.Agents
			report.Deleted.Schedules += pr.Deleted.Schedules
			report.Deleted.Groups += pr.Deleted.Groups
			report.Deleted.Projects += pr.Deleted.Projects
			report.Deleted.Grants += pr.Deleted.Grants
			if pr.IdentityDeleted {
				report.IdentitiesDeleted++
			}
			report.NotDone = append(report.NotDone, pr.NotDone...)
			report.Unproven = append(report.Unproven, pr.Unproven...)
			if !pr.Complete {
				complete = false
			}
		}
		report.Identities = append(report.Identities, entry)
		for i := range p.targets {
			t := p.targets[i]
			s.settleCleanupTarget(ctx, &t, pr)
			if t.Status == fixtureCleanupNotDone {
				complete = false
			}
			report.Targets = append(report.Targets, t)
		}
	}
	report.Complete = complete
}

// settleCleanupTarget sets t's status after its identity's purge: deleted
// when its row is gone, kept for a project the purge kept, not_done
// otherwise (with the purge's reason when it named the resource).
func (s *Server) settleCleanupTarget(ctx context.Context, t *TestIdentityCleanupTarget, pr *TestIdentityPurgeReport) {
	var err error
	switch t.Type {
	case fixtureResourceAgent:
		_, err = s.store.GetAgent(ctx, t.ID)
	case fixtureResourceSchedule:
		_, err = s.store.GetSchedule(ctx, t.ID)
	case fixtureResourceGroup:
		_, err = s.store.GetGroup(ctx, t.ID)
	case fixtureResourceProject:
		_, err = s.store.GetProject(ctx, t.ID)
	case fixtureResourceTestIdentity:
		_, err = s.store.GetUser(ctx, t.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Status = fixtureCleanupDeleted
		return
	}
	t.Status = fixtureCleanupNotDone
	if err != nil {
		t.Reason = "store_error"
		return
	}
	if pr == nil {
		t.Reason = "purge_failed"
		return
	}
	if t.Type == fixtureResourceProject {
		for _, k := range pr.KeptProjects {
			if k.ID == t.ID {
				t.Status, t.Reason = fixtureCleanupKept, k.Reason
				return
			}
		}
	}
	if t.Type == fixtureResourceTestIdentity && pr.IdentityKeptReason != "" {
		t.Reason = pr.IdentityKeptReason
		return
	}
	for _, it := range pr.NotDone {
		if it.Type == t.Type && it.ID == t.ID {
			t.Reason = it.Status
			if it.Reason != "" {
				t.Reason += ": " + it.Reason
			}
			return
		}
	}
}

// writeTestIdentityCleanupAudit writes the run's test_identity_cleanup row:
// the counts, the scope, and the run ID as its correlation ID (shared by
// every per-resource row of the run).
func (s *Server) writeTestIdentityCleanupAudit(ctx context.Context, scope fixtureCleanupScope, report *TestIdentityCleanupReport) error {
	notDone := 0
	for _, t := range report.Targets {
		if t.Status == fixtureCleanupNotDone {
			notDone++
		}
	}
	after, err := json.Marshal(struct {
		Scope             string                  `json:"scope"`
		Identities        int                     `json:"identities"`
		IdentitiesDeleted int                     `json:"identities_deleted"`
		Deleted           TestIdentityPurgeCounts `json:"deleted"`
		Targets           int                     `json:"targets"`
		NotDone           int                     `json:"not_done"`
		Unproven          int                     `json:"unproven"`
		LiveSkipped       int                     `json:"live_skipped"`
		Complete          bool                    `json:"complete"`
		Truncated         bool                    `json:"truncated"`
	}{scope.name, len(report.Identities), report.IdentitiesDeleted, report.Deleted, len(report.Targets), notDone,
		len(report.Unproven), report.LiveSkipped, report.Complete, report.Truncated})
	if err != nil {
		return err
	}
	basis, err := json.Marshal(fixtureCleanupBasis{
		Basis: fixtureCleanupBasisName, Scope: scope.name, IssuerID: scope.issuerID, DelegationID: scope.delegationID,
		RunID: report.RunID, Trigger: testIdentityCleanupMutation,
	})
	if err != nil {
		return err
	}
	target := scope.issuerID
	if target == "" {
		target = "*"
	}
	record := &store.MutationAuditRecord{
		MutationType:    testIdentityCleanupMutation,
		TargetType:      testIdentityCleanupTargetType,
		TargetID:        target,
		AfterSummary:    string(after),
		Timestamp:       time.Now(),
		CorrelationID:   report.RunID,
		ProvenanceBasis: string(basis),
	}
	actor := s.buildAuditActorFromContext(ctx)
	actor.CorrelationID = report.RunID
	actor.ExecutorKind, actor.ExecutorID = fixturePurgeExecutorCleanup, "test_identity_issuer:"+target
	actor.ApplyActor(record)
	applyHubActorFallback(record)
	if f := fixturePurgeAuditFault; f != nil {
		if err := f(record); err != nil {
			return err
		}
	}
	return s.store.CreateMutationAudit(ctx, record)
}

// fixtureDeleteMutations maps each provenance resource type to the audit
// type of its removal by the purge, the reaper or the cleanup; those rows
// carry provenance_basis.
var fixtureDeleteMutations = map[string]string{
	fixtureResourceAgent:    mutationTypeAgentHardDelete,
	fixtureResourceSchedule: mutationTypeScheduleDelete,
	fixtureResourceGroup:    fixturePurgeGroupDeleteMutation,
	fixtureResourceProject:  "project_delete",
}

// fixtureProvenanceDeleted is the reason of a proven answer for a resource
// that is gone, proven by its deletion record.
const fixtureProvenanceDeleted = "deleted under fixture stamp"

// handleTestIdentityProvenance handles GET
// /api/v1/test-identities/provenance?resource=<type>:<id>, type one of
// agent, project, schedule, group. It needs test_identity.issue with the
// feature on (testIdentityCaller: 404 off, 403 without the permission,
// test identities refused). The answer is proven only for a stamp in the
// caller's cleanup scope (stampInScope): its own identities, its own
// delegation's, or every issuer's for an unscoped platform admin session.
// A stamped resource in scope gets its stamp and its identity's state. A
// resource that is gone is proven by its deletion record: a purge, reaper
// or cleanup delete row for it whose provenance_basis is in scope
// (deleted:true, deletionRecord = that row's correlation ID). Everything
// else, an unstamped resource, a missing one and a stamp outside the
// scope, gets the same proven:false answer with only the reason, so the
// route reveals neither other issuers' stamps nor which IDs exist.
func (s *Server) handleTestIdentityProvenance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.testIdentityCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	for key := range q {
		if key != "resource" {
			BadRequest(w, fmt.Sprintf("invalid query parameter %q: provenance takes only resource", key))
			return
		}
	}
	if len(q["resource"]) != 1 {
		BadRequest(w, "resource is required: <type>:<id>, type one of agent, project, schedule, group")
		return
	}
	resource := q.Get("resource")
	typ, id, found := strings.Cut(resource, ":")
	if !found || id == "" || !validPathID(id) {
		BadRequest(w, "invalid resource: must be <type>:<id>, type one of agent, project, schedule, group")
		return
	}
	if _, known := fixtureDeleteMutations[typ]; !known {
		BadRequest(w, "invalid resource type: must be one of agent, project, schedule, group")
		return
	}
	scope, ok := s.cleanupScopeFor(ctx, caller)
	if !ok {
		writeForbiddenStructured(w, "requires test_identity.issue permission", permissions.ResourceTestIdentity, ActionIssue)
		return
	}
	answer, err := s.fixtureProvenance(ctx, caller, scope, typ, id)
	if err != nil {
		slog.ErrorContext(ctx, "test identity provenance: read failed", "resource_type", typ, "error", err)
		InternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// fixtureProvenance computes the provenance answer for typ:id as seen from
// scope (see handleTestIdentityProvenance).
func (s *Server) fixtureProvenance(ctx context.Context, caller UserIdentity, scope fixtureCleanupScope, typ, id string) (TestIdentityProvenance, error) {
	unproven := TestIdentityProvenance{Resource: typ + ":" + id, Reason: fixtureProvenanceNoStamp}
	var (
		stamp store.FixtureProvenance
		err   error
	)
	switch typ {
	case fixtureResourceAgent:
		var a *store.Agent
		if a, err = s.store.GetAgent(ctx, id); err == nil && a != nil {
			stamp = a.Fixture
		}
	case fixtureResourceProject:
		var p *store.Project
		if p, err = s.store.GetProject(ctx, id); err == nil && p != nil {
			stamp = p.Fixture
		}
	case fixtureResourceSchedule:
		var sc *store.Schedule
		if sc, err = s.store.GetSchedule(ctx, id); err == nil && sc != nil {
			stamp = sc.Fixture
		}
	case fixtureResourceGroup:
		var g *store.Group
		if g, err = s.store.GetGroup(ctx, id); err == nil && g != nil {
			stamp = g.Fixture
		}
	}
	gone := errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput)
	if err != nil && !gone {
		return unproven, err
	}
	deletionRecord := ""
	if gone {
		// The resource is gone: its deletion record proves it, when the
		// purge, the reaper or the cleanup removed it.
		var rec *store.MutationAuditRecord
		if stamp, rec, err = s.fixtureDeletionRecord(ctx, caller, scope, typ, id); err != nil || rec == nil {
			return unproven, err
		}
		deletionRecord = rec.CorrelationID
	} else {
		if stamp.RootID == "" || !stamp.Valid() {
			return unproven, nil
		}
		in, err := s.stampInScope(ctx, caller, scope, stamp)
		if err != nil || !in {
			return unproven, err
		}
	}
	state, err := s.fixtureRootState(ctx, stamp.RootID)
	if err != nil {
		return unproven, err
	}
	answer := TestIdentityProvenance{
		Resource: unproven.Resource, Proven: true,
		FixtureRootID: stamp.RootID, FixtureIssuerID: stamp.IssuerID, FixtureState: state,
		Reason: fixtureProvenanceStamped,
	}
	if gone {
		answer.Deleted, answer.DeletionRecord, answer.Reason = true, deletionRecord, fixtureProvenanceDeleted
	}
	return answer, nil
}

// fixtureDeletionRecord returns the newest deletion record of typ:id, a
// delete row of fixtureDeleteMutations[typ] with a provenance_basis whose
// stamp is in scope, and that stamp; rec is nil when there is none.
func (s *Server) fixtureDeletionRecord(ctx context.Context, caller UserIdentity, scope fixtureCleanupScope, typ, id string) (store.FixtureProvenance, *store.MutationAuditRecord, error) {
	recs, _, err := s.store.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: fixtureDeleteMutations[typ],
		TargetType:   typ,
		TargetID:     id,
		Limit:        20,
	})
	if err != nil {
		return store.FixtureProvenance{}, nil, err
	}
	for _, rec := range recs {
		if rec.ProvenanceBasis == "" {
			continue
		}
		var b fixturePurgeBasis
		if json.Unmarshal([]byte(rec.ProvenanceBasis), &b) != nil || b.RootID == "" || b.IssuerID == "" {
			continue
		}
		stamp := store.FixtureProvenance{RootID: b.RootID, IssuerID: b.IssuerID}
		in, err := s.stampInScope(ctx, caller, scope, stamp)
		if err != nil {
			return store.FixtureProvenance{}, nil, err
		}
		if in {
			return stamp, rec, nil
		}
	}
	return store.FixtureProvenance{}, nil, nil
}

// stampInScope reports whether the stamp is in the caller's cleanup scope:
// always for the all scope; otherwise only a stamp of the scope's issuer,
// and for a delegation scope only one whose identity was issued through
// that delegation (canManageTestIdentity on the identity's row, or its
// issuance record when the row is gone).
func (s *Server) stampInScope(ctx context.Context, caller UserIdentity, scope fixtureCleanupScope, stamp store.FixtureProvenance) (bool, error) {
	if scope.name == fixtureCleanupScopeAll {
		return true, nil
	}
	if stamp.IssuerID == "" || stamp.IssuerID != scope.issuerID {
		return false, nil
	}
	if scope.name != fixtureCleanupScopeDelegation {
		return true, nil
	}
	u, err := s.store.GetUser(ctx, stamp.RootID)
	switch {
	case err == nil:
		return s.canManageTestIdentity(ctx, caller, u), nil
	case errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput):
		return s.fixtureIssuedThroughDelegation(ctx, stamp.RootID, stamp.IssuerID)
	}
	return false, err
}

// fixtureRootState returns live, expired or absent for the stamp root
// rootID, as the cleanup sees it.
func (s *Server) fixtureRootState(ctx context.Context, rootID string) (string, error) {
	u, err := s.store.GetUser(ctx, rootID)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
		return fixtureStateAbsent, nil
	}
	if err != nil {
		return "", err
	}
	if u.ExpiresAt == nil || u.ExpiresAt.After(s.testIdentities.clock()) {
		return fixtureStateLive, nil
	}
	return fixtureStateExpired, nil
}
