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
	"io"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SurfacePassthroughTranslationReview labels the passthrough gate's audit
// records when the translation review re-checks an existing agent.
const SurfacePassthroughTranslationReview = "passthrough-translation-review"

// annotationGCPIdentityTranslatedFrom marks an agent whose GCP identity was
// translated from passthrough to the broker's host service account. It
// selects agents for the review and is never an authority input.
const annotationGCPIdentityTranslatedFrom = "scion.dev/gcp-identity-translated-from"

// annotationGCPIdentityReview records that the review set the agent's GCP
// identity to block.
const annotationGCPIdentityReview = "scion.dev/gcp-identity-review"

// Review outcomes. Only fail is ever blocked.
const (
	translationReviewPass    = "pass"
	translationReviewFail    = "fail"
	translationReviewError   = "error"
	translationReviewSkipped = "skipped"
)

// errTranslationReviewChanged: the agent's GCP identity changed between the
// review and the block, so it was left as it is.
var errTranslationReviewChanged = errors.New("the agent's GCP identity changed since it was reviewed; left unchanged")

// translationReviewEntry is the review of one annotated agent.
type translationReviewEntry struct {
	AgentID       string
	ProjectID     string
	BrokerID      string
	PrincipalID   string // the principal the passthrough requirements were checked for
	SAID          string
	SAEmail       string
	RecordBound   bool      // the assigned record is this broker's translation record
	RecordScopeID string    // the assigned record's ScopeID (the broker it is bound to)
	RecordUnbound bool      // a translation record with no broker binding (empty ScopeID)
	RecordCreated time.Time // when the assigned record was created
	Outcome       string
	Reason        string
	Blocked       bool
	BlockedError  string
}

// PassthroughTranslationReviewExecutor reviews agents whose GCP identity was
// translated from passthrough to a broker's host service account for a
// cloudrun-sandbox profile. It selects agents by the translation annotation
// whose assigned service account record is a translation record, or whose
// active assignment of it was recorded by translation; others are reported
// as skipped. For each selected agent it re-runs the passthrough
// gate for the principal recorded as the source of the agent's assignment
// (the creator when none is recorded) against the agent's broker: broker
// owner or admin, and actAs on the broker's host service account.
//
// It only reports unless the block parameter is the string "true". Then each
// agent whose review is fail has its GCP identity set to block and its
// service-account assignment deactivated; a blocked agent is restored by
// recreating it. Agents whose review is error (the check could not be made,
// or the principal is suspended) or skipped are never changed.
type PassthroughTranslationReviewExecutor struct {
	srv *Server
}

// Run implements MaintenanceExecutor.
func (e *PassthroughTranslationReviewExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	if mode := e.srv.saAssignCheckModeValue(); mode != SAAssignCheckEnforce {
		return fmt.Errorf("the review requires gcpIamCheckMode=enforce (current: %q)", mode)
	}
	block := params["block"] == "true"
	if block {
		_, _ = fmt.Fprintln(logger, "Block mode: agents that fail the check will have their GCP identity set to block.")
	} else {
		_, _ = fmt.Fprintln(logger, "Report only: no agent is changed (set block to \"true\" to block agents that fail).")
	}

	entries, err := e.srv.reviewTranslatedAgents(ctx, block)
	counts := map[string]int{}
	blocked := 0
	for _, en := range entries {
		line := fmt.Sprintf("agent=%s project=%s broker=%s principal=%s sa=%s scope_id=%s record_bound=%t outcome=%s",
			en.AgentID, en.ProjectID, en.BrokerID, en.PrincipalID, en.SAEmail, en.RecordScopeID, en.RecordBound, en.Outcome)
		if en.RecordUnbound {
			line += " record_unbound=true"
		}
		if !en.RecordCreated.IsZero() {
			line += " record_created=" + en.RecordCreated.UTC().Format(time.RFC3339)
		}
		if en.Reason != "" {
			line += fmt.Sprintf(" reason=%q", en.Reason)
		}
		if en.Blocked {
			line += " action=blocked"
			blocked++
		}
		if en.BlockedError != "" {
			line += fmt.Sprintf(" action_error=%q", en.BlockedError)
		}
		_, _ = fmt.Fprintln(logger, line)
		counts[en.Outcome]++
	}
	_, _ = fmt.Fprintf(logger, "Reviewed %d annotated agent(s): pass %d, fail %d, error %d, skipped %d, blocked %d\n",
		len(entries), counts[translationReviewPass], counts[translationReviewFail],
		counts[translationReviewError], counts[translationReviewSkipped], blocked)
	return err
}

// reviewTranslatedAgents reviews every agent carrying the translation
// annotation with an assign-mode identity and, when block is true, blocks
// the ones whose review is fail.
func (s *Server) reviewTranslatedAgents(ctx context.Context, block bool) ([]translationReviewEntry, error) {
	var entries []translationReviewEntry
	cursor := ""
	for {
		page, err := s.store.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 200, Cursor: cursor})
		if err != nil {
			return entries, fmt.Errorf("list agents: %w", err)
		}
		for i := range page.Items {
			a := &page.Items[i]
			if a.Annotations[annotationGCPIdentityTranslatedFrom] != "passthrough" {
				continue
			}
			if a.AppliedConfig == nil || a.AppliedConfig.GCPIdentity == nil ||
				a.AppliedConfig.GCPIdentity.MetadataMode != store.GCPMetadataModeAssign {
				continue // blocked, or set back to another mode
			}
			en := s.reviewTranslatedAgent(ctx, a)
			if block && en.Outcome == translationReviewFail {
				if err := s.blockTranslatedAgent(ctx, a.ID, en.SAID); err != nil {
					en.BlockedError = err.Error()
				} else {
					en.Blocked = true
				}
			}
			entries = append(entries, en)
		}
		if page.NextCursor == "" {
			return entries, nil
		}
		cursor = page.NextCursor
	}
}

// reviewTranslatedAgent reviews one agent.
func (s *Server) reviewTranslatedAgent(ctx context.Context, a *store.Agent) translationReviewEntry {
	gcp := a.AppliedConfig.GCPIdentity
	en := translationReviewEntry{
		AgentID: a.ID, ProjectID: a.ProjectID, BrokerID: a.RuntimeBrokerID,
		SAID: gcp.ServiceAccountID, SAEmail: gcp.ServiceAccountEmail,
	}

	// An agent is reviewed when its assigned record is a translation record,
	// or when its active assignment of that account was recorded by
	// translation (which also covers translations onto a hub record created
	// some other way). Both are selectors only. An agent whose identity was
	// set to another account through the assign path matches neither.
	sa, err := s.store.GetGCPServiceAccount(ctx, gcp.ServiceAccountID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		en.Outcome, en.Reason = translationReviewError, "load service account: "+err.Error()
		return en
	}
	if err != nil {
		sa = nil
	}
	rows, err := s.store.GetActiveAgentServiceAccountAssignments(ctx, a.ID)
	if err != nil {
		en.Outcome, en.Reason = translationReviewError, "load assignment: "+err.Error()
		return en
	}
	var reviewedRows []store.AgentServiceAccountAssignment
	translatedRow := false
	for _, row := range rows {
		if row.ServiceAccountID != gcp.ServiceAccountID {
			continue
		}
		reviewedRows = append(reviewedRows, row)
		if row.Origin == store.SAAssignmentOriginHostPassthroughTranslation {
			translatedRow = true
		}
	}
	translationRecord := store.IsHostSATranslationRecord(sa)
	if !translationRecord && !translatedRow {
		en.Outcome, en.Reason = translationReviewSkipped,
			"neither the assigned service account record nor its assignment was made by passthrough translation"
		return en
	}
	if sa != nil {
		en.RecordCreated = sa.CreatedAt
		en.RecordBound = translationRecord && sa.ScopeID == a.RuntimeBrokerID && !sa.Managed
		en.RecordScopeID = sa.ScopeID
		en.RecordUnbound = translationRecord && sa.ScopeID == ""
	}

	if a.RuntimeBrokerID == "" {
		en.Outcome, en.Reason = translationReviewError, "agent has no broker"
		return en
	}
	broker, err := s.store.GetRuntimeBroker(ctx, a.RuntimeBrokerID)
	if err != nil {
		en.Outcome, en.Reason = translationReviewError, "load broker: "+err.Error()
		return en
	}

	principalID, refused := translationReviewPrincipal(a, reviewedRows)
	en.PrincipalID = principalID
	if refused != nil {
		en.Outcome, en.Reason = refused.outcome, refused.reason
		return en
	}

	user, err := s.store.GetUser(ctx, principalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			en.Outcome, en.Reason = translationReviewFail, "the principal is not a user"
			return en
		}
		en.Outcome, en.Reason = translationReviewError, "load principal: "+err.Error()
		return en
	}
	if user == nil {
		en.Outcome, en.Reason = translationReviewFail, "the principal is not a user"
		return en
	}
	if user.Status != store.UserStatusActive {
		// Possibly temporary (for example suspended): left for an operator.
		en.Outcome, en.Reason = translationReviewError, "the principal's account is "+user.Status
		return en
	}

	principalCtx := contextWithIdentity(ctx, NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, ""))
	if d := s.evaluatePassthroughIdentity(principalCtx, nil, broker, SurfacePassthroughTranslationReview); d != nil {
		en.Reason = d.msg
		if d.retryable {
			en.Outcome = translationReviewError
		} else {
			en.Outcome = translationReviewFail
		}
		return en
	}
	if broker.GCPHostServiceAccountEmail != "" && !strings.EqualFold(broker.GCPHostServiceAccountEmail, gcp.ServiceAccountEmail) {
		// The principal passes for the broker's current host account, but
		// the agent holds a different one.
		en.Outcome, en.Reason = translationReviewFail, "agent holds "+gcp.ServiceAccountEmail+
			", not the broker's current host service account"
		return en
	}
	en.Outcome = translationReviewPass
	return en
}

// translationReviewRefusal is a review outcome decided while resolving the
// principal.
type translationReviewRefusal struct {
	outcome, reason string
}

// translationReviewPrincipal returns the principal to check: the user
// recorded as the source of the agent's active assignment of the reviewed
// account (rows) when one is recorded, otherwise the agent's creator. A
// recorded agent source is a fail, since an agent cannot pass the
// passthrough requirements.
func translationReviewPrincipal(a *store.Agent, rows []store.AgentServiceAccountAssignment) (string, *translationReviewRefusal) {
	for _, row := range rows {
		if row.ProvenanceVersion < 1 {
			continue
		}
		switch row.SourcePrincipalKind {
		case store.DelegationPrincipalUser:
			return row.SourcePrincipalID, nil
		case store.DelegationPrincipalAgent:
			return row.SourcePrincipalID, &translationReviewRefusal{
				outcome: translationReviewFail, reason: "the assignment's source is an agent principal"}
		}
	}
	return a.CreatedBy, nil
}

// blockTranslatedAgent sets the agent's GCP identity to block and deactivates
// its service-account assignment, in one transaction. It leaves the agent
// unchanged (errTranslationReviewChanged) unless the agent holds the
// reviewed service account in assign mode.
func (s *Server) blockTranslatedAgent(ctx context.Context, agentID, reviewedSAID string) error {
	return s.store.WithTx(ctx, func(tx store.Store) error {
		a, err := tx.GetAgent(ctx, agentID)
		if err != nil {
			return err
		}
		if a.AppliedConfig == nil || a.AppliedConfig.GCPIdentity == nil ||
			a.AppliedConfig.GCPIdentity.MetadataMode != store.GCPMetadataModeAssign ||
			a.AppliedConfig.GCPIdentity.ServiceAccountID != reviewedSAID {
			return errTranslationReviewChanged
		}
		a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations[annotationGCPIdentityReview] = "blocked"
		a.Updated = time.Now()
		if err := tx.UpdateAgent(ctx, a); err != nil {
			return err
		}
		now := time.Now()
		_, err = tx.DeactivateAgentServiceAccountAssignments(ctx, agentID, store.Deactivation{
			Cause: store.EdgeDeactivationSACleared,
			At:    &now,
			OpID:  api.NewUUID(),
		})
		return err
	})
}
