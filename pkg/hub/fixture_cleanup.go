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
// whether the hub recorded the resource as created under a test identity.
// An unstamped resource and a missing one get the same answer
// (proven:false, reason "no fixture stamp"), with no owner, creator or
// other field, so the route is no existence oracle for real users'
// resources.

const (
	// testIdentityCleanupMutation is the audit type of one cleanup run.
	testIdentityCleanupMutation = "test_identity_cleanup"
	// testIdentityCleanupTargetType is the target type of the run row: the
	// issuer scope the run selected by.
	testIdentityCleanupTargetType = "test_identity_issuer"
	// fixturePurgeExecutorCleanup is the executor of the cleanup's deletes.
	fixturePurgeExecutorCleanup = "system:test_identity_cleanup"
	// fixtureCleanupRootLimit bounds the identities one cleanup run reaches;
	// a run that hits it is truncated and the next run continues.
	fixtureCleanupRootLimit = 200
	// fixtureCleanupBasisName is provenance_basis.basis of the run row.
	fixtureCleanupBasisName = "fixture_issuer_id"
	// fixtureProvenanceNoStamp is the reason of every unproven answer.
	fixtureProvenanceNoStamp = "no fixture stamp"
	// fixtureProvenanceStamped is the reason of every proven answer.
	fixtureProvenanceStamped = "fixture stamp"
)

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
	Reason       string `json:"reason"`
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
// and their stamped resources. It writes nothing.
func (s *Server) planTestIdentityCleanup(ctx context.Context, caller UserIdentity, scope fixtureCleanupScope, report *TestIdentityCleanupReport) ([]fixtureCleanupPlan, error) {
	roots, truncated, err := s.store.ListFixtureRoots(ctx, scope.issuerID, fixtureCleanupRootLimit)
	if err != nil {
		return nil, fmt.Errorf("list stamped identities: %w", err)
	}
	report.Truncated = report.Truncated || truncated
	now := s.testIdentities.clock()
	// The candidates: every identity a stamp of the scope names (expired,
	// deleted or live), then the scope's expired identities that no stamp
	// names any more (for example an earlier run removed their resources
	// but not the identity).
	var candidates []string
	for _, root := range roots {
		candidates = append(candidates, root.RootID)
	}
	expired, more, err := s.expiredScopeIdentities(ctx, scope, now)
	if err != nil {
		return nil, err
	}
	report.Truncated = report.Truncated || more
	candidates = append(candidates, expired...)
	seen := map[string]bool{}
	var plans []fixtureCleanupPlan
	for _, rootID := range candidates {
		if seen[rootID] {
			continue
		}
		seen[rootID] = true
		if len(seen) > fixtureCleanupRootLimit {
			report.Truncated = true
			break
		}
		var u *store.User
		state := fixtureStateAbsent
		switch got, err := s.store.GetUser(ctx, rootID); {
		case err == nil:
			u, state = got, fixtureStateExpired
		case errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput):
		default:
			return nil, fmt.Errorf("read test identity %s: %w", rootID, err)
		}
		// The purge route's rules decide whether the caller reaches the
		// identity (its issuer, its own delegation, or an admin session);
		// a row that is no test identity is never reached.
		run, ok, err := s.resolveFixturePurgeRun(ctx, caller, rootID, u, true)
		if err != nil {
			return nil, fmt.Errorf("test identity %s: %w", rootID, err)
		}
		if !ok {
			continue
		}
		if u != nil && (u.ExpiresAt == nil || u.ExpiresAt.After(now)) {
			// A live identity is never touched.
			report.LiveSkipped++
			continue
		}
		targets, more, err := s.fixtureCleanupTargets(ctx, rootID, u)
		if err != nil {
			return nil, err
		}
		report.Truncated = report.Truncated || more
		plans = append(plans, fixtureCleanupPlan{run: run, state: state, targets: targets})
	}
	return plans, nil
}

// fixtureCleanupExpiredPage is the page size of expiredScopeIdentities.
const fixtureCleanupExpiredPage = 500

// expiredScopeIdentities returns the IDs of the test identities (member
// and admin kinds) that expired at or before now and were issued by the
// scope's issuer (every issuer for the all scope), at most
// fixtureCleanupRootLimit; more is true when others remain. Whether the
// caller reaches each one is decided afterwards, by the purge route's
// rules.
func (s *Server) expiredScopeIdentities(ctx context.Context, scope fixtureCleanupScope, now time.Time) ([]string, bool, error) {
	var out []string
	q := store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: now, IncludeAdminKinds: true, Limit: fixtureCleanupExpiredPage}
	for {
		page, err := s.store.ListExpiredTestFixtureUsers(ctx, q)
		if err != nil {
			return nil, false, fmt.Errorf("list expired test identities: %w", err)
		}
		for i := range page {
			u := &page[i]
			if scope.issuerID != "" && (u.IssuedBy == nil || *u.IssuedBy != scope.issuerID) {
				continue
			}
			if len(out) == fixtureCleanupRootLimit {
				return out, true, nil
			}
			out = append(out, u.ID)
		}
		if len(page) < fixtureCleanupExpiredPage {
			return out, false, nil
		}
		last := page[len(page)-1]
		if last.ExpiresAt == nil {
			return out, false, nil
		}
		q.AfterExpiresAt, q.AfterID = *last.ExpiresAt, last.ID
	}
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

// handleTestIdentityProvenance handles GET
// /api/v1/test-identities/provenance?resource=<type>:<id>, type one of
// agent, project, schedule, group. It needs test_identity.issue with the
// feature on (testIdentityCaller: 404 off, 403 without the permission,
// test identities refused). A resource without a stamp and a missing one
// get the same proven:false answer with only the reason; a stamped one
// gets its stamp and its identity's state.
func (s *Server) handleTestIdentityProvenance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.testIdentityCaller(w, r); !ok {
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
	default:
		BadRequest(w, "invalid resource type: must be one of agent, project, schedule, group")
		return
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidInput) {
		slog.ErrorContext(ctx, "test identity provenance: read failed", "resource_type", typ, "error", err)
		InternalError(w)
		return
	}
	answer := TestIdentityProvenance{Resource: typ + ":" + id, Reason: fixtureProvenanceNoStamp}
	if err != nil || stamp.RootID == "" || !stamp.Valid() {
		writeJSON(w, http.StatusOK, answer)
		return
	}
	state, err := s.fixtureRootState(ctx, stamp.RootID)
	if err != nil {
		slog.ErrorContext(ctx, "test identity provenance: identity read failed", "fixture_root_id", stamp.RootID, "error", err)
		InternalError(w)
		return
	}
	answer.Proven = true
	answer.FixtureRootID = stamp.RootID
	answer.FixtureIssuerID = stamp.IssuerID
	answer.FixtureState = state
	answer.Reason = fixtureProvenanceStamped
	writeJSON(w, http.StatusOK, answer)
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
