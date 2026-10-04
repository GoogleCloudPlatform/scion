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
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Admin recovery for delegation-provenance adoption: status, preview and
// commit (adopt or revert) under /api/v1/admin/delegation-adoption. Only a
// hub system admin authenticated by an interactive session or by the
// recognized local development user may call it. Agent tokens and user
// access tokens are refused. The Scion ActionAssign check and the GCP actAs
// check are not involved and not relaxed: adoption only records a bounded
// ceiling on an edge; every request is decided by the unchanged walk.

const (
	delegationAdoptionPath = "/api/v1/admin/delegation-adoption"

	// delegationAdoptionMaxHops caps the hops one commit writes. A commit is
	// all-or-nothing, so a larger plan must be narrowed by scope.
	delegationAdoptionMaxHops = 500

	delegationAdoptionOpAdopt  = "adopt"
	delegationAdoptionOpRevert = "revert"

	mutationTypeDelegationAdoption       = "delegation_provenance_adoption"
	mutationTypeDelegationAdoptionRevert = "delegation_provenance_adoption_revert"
	mutationTypeDelegationAdoptionCommit = "delegation_provenance_adoption_commit"
)

var (
	errAdoptionStalePlan      = errors.New("plan changed since preview")
	errAdoptionPermissionLost = errors.New("admin permission lost")
)

// delegationAdoptionActor is the authorized caller.
type delegationAdoptionActor struct {
	UserID         string
	CredentialKind string // store.InitiatorCredentialKindSession | ...DevLocal
}

// authorizeDelegationAdoption admits a hub system admin and writes 401/403
// otherwise. The adoption admin endpoints require an interactive or dev
// credential: the credential kind is read from the request's credential
// context (never inferred from the identity's Go type), and a missing or
// unrecognized credential context is denied. The returned actor's
// CredentialKind is derived from that same context.
func (s *Server) authorizeDelegationAdoption(w http.ResponseWriter, r *http.Request) (delegationAdoptionActor, bool) {
	ctx := r.Context()
	resource := Resource{Type: "hub", ID: delegationAdoptionPath}
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return delegationAdoptionActor{}, false
	}
	deny := func(reason string) (delegationAdoptionActor, bool) {
		logAuthzDenial(r, identity, resource, ActionManage, reason)
		Forbidden(w)
		return delegationAdoptionActor{}, false
	}
	credential := GetCredentialContextFromContext(ctx)
	switch credential.Kind {
	case CredentialKindInteractive, CredentialKindDev:
	default:
		// Fail closed: missing, unknown, UAT, agent, federation, broker
		// (including a broker request carrying a user identity) and hub
		// delivery credentials.
		return deny("credential kind may not run delegation adoption")
	}
	user, ok := identity.(UserIdentity)
	if !ok || isNilIdentity(user) || user.ID() == "" {
		return deny("non-user identity")
	}
	if IsScopedUserIdentity(user) {
		return deny("scoped user access token")
	}
	if _, federated := user.(FederatedIdentity); federated {
		return deny("federated identity")
	}
	kind := initiatorCredentialKindFor(identity, credential.Kind)
	switch kind {
	case store.InitiatorCredentialKindSession:
	case store.InitiatorCredentialKindDevLocal:
		if s.authzService == nil || !s.authzService.devLocalAuthorityEnabled() {
			return deny("local development authority not enabled")
		}
	default:
		// A dev credential on an identity other than the trusted local
		// development user.
		return deny("credential kind may not run delegation adoption")
	}
	if !s.isDelegationAdoptionAdmin(ctx, user) {
		return deny("not a system admin")
	}
	return delegationAdoptionActor{UserID: user.ID(), CredentialKind: kind}, true
}

// isDelegationAdoptionAdmin reports whether user is a hub system admin: an
// unscoped local platform admin or a holder of the system super-admin role.
// No registered permission covers this check (adoption adds none), so it is
// a direct system-admin test rather than a Decide call.
func (s *Server) isDelegationAdoptionAdmin(ctx context.Context, user UserIdentity) bool {
	if IsUnscopedLocalPlatformAdmin(user) {
		return true
	}
	return s.authzService != nil && s.authzService.IsSystemAdmin(ctx, user.ID())
}

// recheckDelegationAdoptionAdmin re-reads the actor inside the commit
// transaction: the user must be active and a system admin.
func (s *Server) recheckDelegationAdoptionAdmin(ctx context.Context, tx store.Store, actor delegationAdoptionActor) error {
	u, err := tx.GetUser(ctx, actor.UserID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && u == nil) {
		return errAdoptionPermissionLost
	}
	if err != nil {
		return err
	}
	if u.Status != store.UserStatusActive {
		return errAdoptionPermissionLost
	}
	if u.Role == store.UserRoleAdmin {
		return nil
	}
	if s.authzService != nil && s.authzService.IsSystemAdmin(ctx, u.ID) {
		return nil
	}
	return errAdoptionPermissionLost
}

// handleDelegationAdoption serves GET /api/v1/admin/delegation-adoption.
func (s *Server) handleDelegationAdoption(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := s.authorizeDelegationAdoption(w, r); !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			BadRequest(w, "limit must be a positive integer")
			return
		}
		limit = min(n, delegationAdoptionMaxHops)
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			BadRequest(w, "offset must be a non-negative integer")
			return
		}
		offset = n
	}

	resp := delegationAdoptionStatusResponse{
		Counts:  map[string]int{},
		Reasons: map[string]int{},
	}
	resp.Marker = s.readAdoptionHeader(ctx, delegationadoption.MarkerSection)
	resp.Cohort = s.readAdoptionHeader(ctx, delegationadoption.CohortSection)

	all, _, err := s.store.ListDelegationAdoptions(ctx, store.DelegationAdoptionFilter{ScopeID: q.Get("projectId")})
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to list adoption records", nil)
		return
	}
	inRecords := make(map[string]bool, len(all))
	for _, rec := range all {
		resp.Counts[string(rec.Status)]++
		if rec.Reason != "" {
			resp.Reasons[rec.Reason]++
		}
		if rec.OriginalEdgeID != "" {
			inRecords[rec.OriginalEdgeID] = true
		}
	}
	records, total, err := s.store.ListDelegationAdoptions(ctx, store.DelegationAdoptionFilter{
		ScopeID: q.Get("projectId"),
		Status:  store.DelegationAdoptionStatus(q.Get("status")),
		Reason:  q.Get("reason"),
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to list adoption records", nil)
		return
	}
	resp.Records = records
	resp.Total = total

	// Adoptable unrecorded hops on live chains that no record covers, for
	// example rows written after the boot snapshot. Only a commit adopts
	// them.
	plan, err := delegationadoption.Build(ctx, s.store, delegationadoption.Scope{ProjectID: q.Get("projectId")})
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to plan live chains", nil)
		return
	}
	for _, h := range plan.Hops {
		if h.Outcome == delegationadoption.OutcomeAdopt && !inRecords[h.Edge.ID] {
			resp.NotInCohortCount++
			if len(resp.NotInCohort) < delegationAdoptionMaxHops {
				resp.NotInCohort = append(resp.NotInCohort, previewHopFor(h))
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) readAdoptionHeader(ctx context.Context, section string) *delegationadoption.Header {
	setting, err := s.store.GetHubSetting(ctx, section)
	if err != nil {
		return nil
	}
	var h delegationadoption.Header
	if json.Unmarshal(setting.Value, &h) != nil {
		return &delegationadoption.Header{}
	}
	return &h
}

type delegationAdoptionStatusResponse struct {
	Marker           *delegationadoption.Header  `json:"marker"`
	Cohort           *delegationadoption.Header  `json:"cohort"`
	Counts           map[string]int              `json:"counts"`
	Reasons          map[string]int              `json:"reasons"`
	Records          []*store.DelegationAdoption `json:"records"`
	Total            int                         `json:"total"`
	NotInCohortCount int                         `json:"notInCohortCount"`
	NotInCohort      []delegationAdoptionPlanHop `json:"notInCohort,omitempty"`
}

// delegationAdoptionRequest is the body of a preview and of a commit. A
// commit repeats its preview's request and adds the preview's fingerprint;
// the server recomputes the plan from current state inside the commit
// transaction, so a client cannot supply a plan of its own.
type delegationAdoptionRequest struct {
	Scope                  delegationadoption.Scope `json:"scope"`
	Operation              string                   `json:"operation"`
	RecordIDs              []string                 `json:"recordIds,omitempty"`
	ConfirmOriginalEdgeIDs map[string]string        `json:"confirmOriginalEdgeIds,omitempty"`
	PlanID                 string                   `json:"planId,omitempty"`
	PlanFingerprint        string                   `json:"planFingerprint,omitempty"`
}

func (req *delegationAdoptionRequest) validate() string {
	switch req.Operation {
	case delegationAdoptionOpAdopt:
		if len(req.RecordIDs) > 0 || len(req.ConfirmOriginalEdgeIDs) > 0 {
			return "recordIds and confirmOriginalEdgeIds apply to revert only"
		}
	case delegationAdoptionOpRevert:
		if len(req.RecordIDs) == 0 {
			return "revert requires recordIds"
		}
		if len(req.RecordIDs) > delegationAdoptionMaxHops {
			return fmt.Sprintf("revert accepts at most %d records", delegationAdoptionMaxHops)
		}
	default:
		return `operation must be "adopt" or "revert"`
	}
	return ""
}

type delegationAdoptionCeilingView struct {
	ProvenanceVersion int      `json:"provenanceVersion"`
	CeilingKind       string   `json:"ceilingKind"`
	PermissionIDs     []string `json:"ceilingPermissionIds,omitempty"`
}

// delegationAdoptionPlanHop is one hop of a preview.
type delegationAdoptionPlanHop struct {
	DelegateID     string                         `json:"delegateId"`
	ProjectID      string                         `json:"projectId"`
	Depth          int                            `json:"depth"`
	Outcome        string                         `json:"outcome"`
	Reason         string                         `json:"reason,omitempty"`
	EdgeID         string                         `json:"edgeId,omitempty"`
	DelegatorType  string                         `json:"delegatorType,omitempty"`
	DelegatorID    string                         `json:"delegatorId,omitempty"`
	ScopeID        string                         `json:"scopeId,omitempty"`
	EdgeRole       string                         `json:"edgeRole,omitempty"`
	CeilingRole    string                         `json:"ceilingRole,omitempty"`
	OriginalEdgeID string                         `json:"originalEdgeId,omitempty"`
	Before         *delegationAdoptionCeilingView `json:"before,omitempty"`
	After          *delegationAdoptionCeilingView `json:"after,omitempty"`
}

func previewHopFor(h *delegationadoption.Hop) delegationAdoptionPlanHop {
	v := delegationAdoptionPlanHop{
		DelegateID:     h.DelegateID,
		ProjectID:      h.ProjectID,
		Depth:          h.Depth,
		Outcome:        string(h.Outcome),
		Reason:         string(h.Reason),
		CeilingRole:    h.Role,
		OriginalEdgeID: h.OriginalEdgeID,
	}
	if e := h.Edge; e != nil {
		v.EdgeID = e.ID
		v.DelegatorType = e.DelegatorType
		v.DelegatorID = e.DelegatorID
		v.ScopeID = e.ScopeID
		v.EdgeRole = e.Role
		v.Before = &delegationAdoptionCeilingView{ProvenanceVersion: e.ProvenanceVersion, CeilingKind: string(e.Kind), PermissionIDs: e.PermissionIDs}
	}
	if h.Outcome == delegationadoption.OutcomeAdopt {
		v.After = &delegationAdoptionCeilingView{ProvenanceVersion: store.ProvenanceVersionV1, CeilingKind: string(store.EffectCeilingBounded), PermissionIDs: h.CeilingIDs}
	}
	return v
}

type delegationAdoptionPreviewResponse struct {
	Operation       string                          `json:"operation"`
	PlanID          string                          `json:"planId"`
	PlanFingerprint string                          `json:"planFingerprint"`
	PolicyVersion   int                             `json:"policyVersion"`
	Writes          int                             `json:"writes"`
	Refused         int                             `json:"refused,omitempty"`
	ExceedsCap      bool                            `json:"exceedsCap,omitempty"`
	Counts          map[string]int                  `json:"counts"`
	Hops            []delegationAdoptionPlanHop     `json:"hops,omitempty"`
	Reverts         []*delegationadoption.RevertHop `json:"reverts,omitempty"`
}

func planIDFor(fingerprint string) string {
	if len(fingerprint) < 16 {
		return "plan-" + fingerprint
	}
	return "plan-" + fingerprint[:16]
}

// computedAdoptionPlan is a plan computed for a request against one store
// (the live store for a preview, the transaction for a commit).
type computedAdoptionPlan struct {
	adopt       *delegationadoption.Plan
	revert      *delegationadoption.RevertPlan
	fingerprint string
}

func (c *computedAdoptionPlan) adoptHops() []*delegationadoption.Hop {
	if c.adopt == nil {
		return nil
	}
	var out []*delegationadoption.Hop
	for _, h := range c.adopt.Hops {
		if h.Outcome == delegationadoption.OutcomeAdopt {
			out = append(out, h)
		}
	}
	return out
}

func (c *computedAdoptionPlan) writes() int {
	if c.revert != nil {
		return len(c.revert.Hops) - c.revert.Refused()
	}
	return len(c.adoptHops())
}

func computeAdoptionPlan(ctx context.Context, r store.Store, req *delegationAdoptionRequest) (*computedAdoptionPlan, error) {
	if req.Operation == delegationAdoptionOpRevert {
		rp, err := delegationadoption.BuildRevert(ctx, r, req.RecordIDs, req.ConfirmOriginalEdgeIDs)
		if err != nil {
			return nil, err
		}
		return &computedAdoptionPlan{revert: rp, fingerprint: rp.Fingerprint()}, nil
	}
	p, err := delegationadoption.Build(ctx, r, req.Scope)
	if err != nil {
		return nil, err
	}
	return &computedAdoptionPlan{adopt: p, fingerprint: p.Fingerprint(req.Operation)}, nil
}

// handleDelegationAdoptionPreviews serves POST .../previews.
func (s *Server) handleDelegationAdoptionPreviews(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	if _, ok := s.authorizeDelegationAdoption(w, r); !ok {
		return
	}
	var req delegationAdoptionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}
	if msg := req.validate(); msg != "" {
		BadRequest(w, msg)
		return
	}
	plan, err := computeAdoptionPlan(r.Context(), s.store, &req)
	if err != nil {
		slog.ErrorContext(r.Context(), "delegation adoption preview failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to compute plan", nil)
		return
	}
	resp := delegationAdoptionPreviewResponse{
		Operation:       req.Operation,
		PlanID:          planIDFor(plan.fingerprint),
		PlanFingerprint: plan.fingerprint,
		PolicyVersion:   int(delegationadoption.PolicyVersion),
		Writes:          plan.writes(),
		Counts:          map[string]int{},
	}
	resp.ExceedsCap = resp.Writes > delegationAdoptionMaxHops
	if plan.revert != nil {
		resp.Reverts = plan.revert.Hops
		resp.Refused = plan.revert.Refused()
		for _, h := range plan.revert.Hops {
			resp.Counts[string(h.Outcome)]++
		}
	} else {
		for _, h := range plan.adopt.Hops {
			resp.Counts[string(h.Outcome)]++
			resp.Hops = append(resp.Hops, previewHopFor(h))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type delegationAdoptionCommitResponse struct {
	Operation string                      `json:"operation"`
	PlanID    string                      `json:"planId"`
	Committed int                         `json:"committed"`
	Records   []*store.DelegationAdoption `json:"records"`
}

// handleDelegationAdoptionCommits serves POST .../commits. The plan is
// recomputed inside one transaction; a fingerprint that differs from the
// preview's returns 409 stale_authorization_preview. Every hop is written in
// that transaction, with one mutation audit record per hop and one summary
// record, or nothing is written.
func (s *Server) handleDelegationAdoptionCommits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	actor, ok := s.authorizeDelegationAdoption(w, r)
	if !ok {
		return
	}
	var req delegationAdoptionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}
	if msg := req.validate(); msg != "" {
		BadRequest(w, msg)
		return
	}
	if req.PlanFingerprint == "" {
		BadRequest(w, "planFingerprint is required; run a preview first")
		return
	}
	if req.PlanID != "" && req.PlanID != planIDFor(req.PlanFingerprint) {
		BadRequest(w, "planId does not match planFingerprint")
		return
	}
	if s.delegationAdoptionCommitHook != nil {
		s.delegationAdoptionCommitHook()
	}

	ctx := r.Context()
	planID := planIDFor(req.PlanFingerprint)
	var (
		written   []*store.DelegationAdoption
		badPlan   string
		requestID = logging.RequestIDFromContext(ctx)
	)
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		written = nil
		if err := s.recheckDelegationAdoptionAdmin(ctx, tx, actor); err != nil {
			return err
		}
		plan, err := computeAdoptionPlan(ctx, tx, &req)
		if err != nil {
			return err
		}
		if plan.fingerprint != req.PlanFingerprint {
			return errAdoptionStalePlan
		}
		switch {
		case plan.writes() == 0:
			badPlan = "the plan has nothing to write"
			return errAdoptionStalePlan
		case plan.writes() > delegationAdoptionMaxHops:
			badPlan = fmt.Sprintf("the plan writes more than %d hops; narrow the scope", delegationAdoptionMaxHops)
			return errAdoptionStalePlan
		case plan.revert != nil && plan.revert.Refused() > 0:
			badPlan = "the plan contains records that cannot be reverted"
			return errAdoptionStalePlan
		}
		if plan.revert != nil {
			written, err = s.commitAdoptionRevert(ctx, tx, plan.revert, actor, requestID)
		} else {
			written, err = s.commitAdoption(ctx, tx, plan.adopt, planID, actor, requestID)
		}
		if err != nil {
			return err
		}
		summary := &store.MutationAuditRecord{
			MutationType:       mutationTypeDelegationAdoptionCommit,
			ActorPrincipalKind: store.DelegationPrincipalUser,
			ActorPrincipalID:   actor.UserID,
			TargetType:         "delegation_adoption_plan",
			TargetID:           planID,
			AfterSummary:       fmt.Sprintf(`{"operation":%q,"plan_fingerprint":%q,"hops":%d,"policy_version":%d}`, req.Operation, req.PlanFingerprint, len(written), delegationadoption.PolicyVersion),
			CorrelationID:      requestID,
		}
		return s.writeAdoptionAudit(ctx, tx, summary)
	})
	switch {
	case err == nil:
	case errors.Is(err, errAdoptionPermissionLost):
		writeError(w, http.StatusForbidden, ErrCodeMutationPermissionLost, "admin permission was lost before the commit", nil)
		return
	case errors.Is(err, errAdoptionStalePlan) && badPlan != "":
		writeError(w, http.StatusUnprocessableEntity, ErrCodeUnprocessable, badPlan, nil)
		return
	case errors.Is(err, errAdoptionStalePlan):
		writeError(w, http.StatusConflict, ErrCodeStaleAuthorizationPreview, "delegation state changed since the preview; run a new preview", nil)
		return
	default:
		slog.ErrorContext(ctx, "delegation adoption commit failed", "plan_id", planID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "commit failed; nothing was written", nil)
		return
	}
	slog.InfoContext(ctx, "delegation adoption committed", "plan_id", planID, "operation", req.Operation,
		"hops", len(written), "actor_id", actor.UserID)
	writeJSON(w, http.StatusOK, delegationAdoptionCommitResponse{
		Operation: req.Operation, PlanID: planID, Committed: len(written), Records: written,
	})
}

func (s *Server) writeAdoptionAudit(ctx context.Context, tx store.Store, rec *store.MutationAuditRecord) error {
	auditActorFromContext(ctx).ApplyActor(rec)
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}
	return tx.CreateMutationAudit(ctx, rec)
}

func (s *Server) commitAdoption(ctx context.Context, tx store.Store, plan *delegationadoption.Plan, planID string, actor delegationAdoptionActor, requestID string) ([]*store.DelegationAdoption, error) {
	byDelegate := map[string]*delegationadoption.Hop{}
	for _, h := range plan.Hops {
		byDelegate[h.DelegateID] = h
	}
	edgeActor := delegationadoption.Actor{
		PrincipalKind:  store.DelegationPrincipalUser,
		PrincipalID:    actor.UserID,
		CredentialKind: actor.CredentialKind,
	}
	var out []*store.DelegationAdoption
	for _, rec := range delegationadoption.SnapshotRecords(plan, planID, store.DelegationAdoptionOriginAdmin) {
		if rec.Status != store.DelegationAdoptionPending {
			continue
		}
		if s.delegationAdoptionHopHook != nil {
			if err := s.delegationAdoptionHopHook(len(out)); err != nil {
				return nil, err
			}
		}
		rec.ActorKind = store.DelegationPrincipalUser
		rec.ActorID = actor.UserID
		if err := tx.CreateDelegationAdoption(ctx, rec); err != nil {
			return nil, err
		}
		res, err := delegationadoption.ApplyPlannedAdopt(ctx, tx, byDelegate[rec.DelegateID], rec.ID, edgeActor)
		if err != nil {
			return nil, err
		}
		if res.Status != store.DelegationAdoptionAdopted {
			return nil, errAdoptionStalePlan
		}
		rec.Status = res.Status
		rec.AdoptedEdgeID = res.AdoptedEdgeID
		rec.AfterSummary = res.AfterSummary
		if err := tx.UpdateDelegationAdoption(ctx, rec); err != nil {
			return nil, err
		}
		if err := s.writeAdoptionAudit(ctx, tx, &store.MutationAuditRecord{
			MutationType:       mutationTypeDelegationAdoption,
			ActorPrincipalKind: store.DelegationPrincipalUser,
			ActorPrincipalID:   actor.UserID,
			TargetType:         "delegation_edge",
			TargetID:           res.AdoptedEdgeID,
			BeforeSummary:      res.Before,
			AfterSummary:       res.AfterSummary,
			CorrelationID:      requestID,
		}); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (s *Server) commitAdoptionRevert(ctx context.Context, tx store.Store, plan *delegationadoption.RevertPlan, actor delegationAdoptionActor, requestID string) ([]*store.DelegationAdoption, error) {
	var out []*store.DelegationAdoption
	for _, h := range plan.Hops {
		rec := h.Record()
		res, err := delegationadoption.ApplyRevert(ctx, tx, h, rec.ID)
		if err != nil {
			return nil, err
		}
		if res.Status != store.DelegationAdoptionReverted {
			return nil, errAdoptionStalePlan
		}
		rec.Status = store.DelegationAdoptionReverted
		rec.OriginalEdgeID = h.OriginalEdgeID
		rec.AfterSummary = res.AfterSummary
		rec.ActorKind = store.DelegationPrincipalUser
		rec.ActorID = actor.UserID
		if err := tx.UpdateDelegationAdoption(ctx, rec); err != nil {
			return nil, err
		}
		if err := s.writeAdoptionAudit(ctx, tx, &store.MutationAuditRecord{
			MutationType:       mutationTypeDelegationAdoptionRevert,
			ActorPrincipalKind: store.DelegationPrincipalUser,
			ActorPrincipalID:   actor.UserID,
			TargetType:         "delegation_edge",
			TargetID:           h.OriginalEdgeID,
			BeforeSummary:      res.Before,
			AfterSummary:       res.AfterSummary,
			CorrelationID:      requestID,
		}); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
