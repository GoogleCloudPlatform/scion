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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Forced user deletion (ptone/scion#2143), behind the
// hub.user_delete_force_cascade experiment.
//
// DELETE /api/v1/users/{id}?force=true deletes a user whom agents or active
// delegations depend on. It needs user.delete on the user and, for every
// blocking agent, the decision the agent delete route makes for the caller
// (agent.delete on that agent); otherwise it answers 403
// user_delete_force_forbidden and writes nothing. The decisions run before
// the transaction (the SQLite store has one connection, so a decision read
// inside a transaction would wait on it).
//
// One transaction then revokes the user's authority and records the
// tombstone: it locks the user row, refuses with 409
// user_delete_blockers_changed if an agent the decisions did not cover
// blocks, removes the user's role bindings (after the last-owner guard),
// revokes the user's access tokens, sets the status to deleting, and
// deactivates, with cause user_delete_cascade under the deletion's ID, the
// edges where the user is the delegator and the edges and service-account
// assignments of every blocking agent. It writes the UserDeletion and one
// UserDeletionAgent per blocking agent (outcome pending) and a
// user_delete_requested audit record. The request answers 202 with the
// deletion resource.
//
// After the commit the hub stops each agent through the agent delete
// engine (reconcileUserDeletion), deepest ancestry first, and completes the
// deletion in a second transaction once every agent is stopped or removed
// (completeUserDeletion). A repeated force request while the deletion is
// in progress joins it: no new write, 202 with the same resource. With no
// blocker at all, force behaves as a plain delete (204).

// userDeletionRequesterKind is the only requester kind in this version, a
// user. userDeletionCredentialKind is the credential kind of a deletion a
// session requested, userDeletionTokenCredentialKind that of one a user
// access token requested; the completion of the latter applies the token's
// system-binding rule (completeUserDeletion).
const (
	userDeletionRequesterKind       = "user"
	userDeletionCredentialKind      = store.InitiatorCredentialKindSession
	userDeletionTokenCredentialKind = store.InitiatorCredentialKindUAT
)

// errUserDeleteBlockersChanged is returned by the forced delete transaction
// when an agent outside the authorized set blocks the deletion.
var errUserDeleteBlockersChanged = errors.New("the agents blocking this user deletion changed; retry the request")

// userDeletionInProgressError is returned when a plain delete, or an
// update, meets a user whose forced deletion is in progress.
type userDeletionInProgressError struct {
	deletionID string
	state      string
}

func (e *userDeletionInProgressError) Error() string {
	return userDeletionInProgressMessage
}

const userDeletionInProgressMessage = "a deletion of this user is in progress; see GET /api/v1/users/{id}/deletion"

// writeUserDeletionInProgress writes the 409 user_deletion_in_progress
// response.
func writeUserDeletionInProgress(w http.ResponseWriter, e *userDeletionInProgressError) {
	writeError(w, http.StatusConflict, ErrCodeUserDeletionInProgress, e.Error(),
		map[string]interface{}{"deletionId": e.deletionID, "state": e.state})
}

// userDeletionInProgressFor returns the in-progress error for userID,
// naming its deletion when one is recorded.
func userDeletionInProgressFor(ctx context.Context, st store.Store, userID string) *userDeletionInProgressError {
	e := &userDeletionInProgressError{state: store.UserDeletionStateStopping}
	if d, err := st.GetUserDeletionByUser(ctx, userID); err == nil {
		e.deletionID, e.state = d.ID, d.State
	}
	return e
}

// forceDeleteUserBeforeTx, when set, runs between the forced delete's
// authorization decisions and its transaction. Tests use it to change the
// blockers in that window; it is nil otherwise.
var forceDeleteUserBeforeTx func()

// forceForbiddenAgentRef names a blocking agent the caller may not delete.
type forceForbiddenAgentRef struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
}

// forceUserDeletionEnabled reports whether r asks for a forced delete and
// the experiment allows it. It is parsed before any side effect. While the
// experiment is off, force is ignored.
func (s *Server) forceUserDeletionEnabled(r *http.Request) bool {
	return r.URL.Query().Get("force") == "true" && s.experimentEnabled(experiments.UserDeleteForceCascade)
}

// forceDeleteUser serves DELETE /api/v1/users/{id}?force=true after the
// shared guards (session credential or user access token, self-delete,
// user.delete, the user read, and for a token the system-scope binding
// rule). It reports false, having written nothing, when nothing blocks the
// deletion and no deletion is in progress: the caller then runs the plain
// delete. byToken is set for a user access token: the agent.delete checks
// are decided on the token identity, the system-scope binding rule is
// judged again on the bindings the cascade deletes (forceDeleteUserTx), and
// the deletion records the token credential kind.
func (s *Server) forceDeleteUser(w http.ResponseWriter, r *http.Request, actor UserIdentity, byToken bool, user *store.User, superAdminRD *store.RoleDefinition) bool {
	ctx := r.Context()

	// A deletion in progress: join it.
	if d, err := s.store.GetUserDeletionByUser(ctx, user.ID); err == nil {
		if d.State == store.UserDeletionStateStopping {
			// Joining asks for a retry of failed agents at once.
			s.kickUserDeletion(d.ID, true)
			s.writeUserDeletion(w, ctx, http.StatusAccepted, d)
			return true
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "user force delete: read deletion failed", "user_id", user.ID, "error", err)
		InternalError(w)
		return true
	}

	pre, err := collectUserDeleteBlockers(ctx, s.store, user.ID)
	if err != nil {
		slog.ErrorContext(ctx, "user force delete: list blockers failed", "user_id", user.ID, "error", err)
		InternalError(w)
		return true
	}
	if pre.empty() {
		return false
	}

	// The caller's own agent.delete on every blocking agent, decided as
	// the agent delete route decides it.
	forbidden := []forceForbiddenAgentRef{}
	for i := range pre.agents {
		a := &pre.agents[i]
		denial := s.authorizeAgentTargetAction(ctx, actor, a, ActionDelete)
		if denial == nil {
			continue
		}
		if denial.indeterminate {
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
				"unable to authorize the deletion of the user's agents; retry", nil)
			return true
		}
		forbidden = append(forbidden, forceForbiddenAgentRef{ID: a.ID, ProjectID: a.ProjectID})
	}
	if len(forbidden) > 0 {
		writeError(w, http.StatusForbidden, ErrCodeUserDeleteForceForbidden,
			"cannot force-delete this user: you may not delete some of the user's agents", map[string]interface{}{"agents": forbidden})
		return true
	}
	authorized := pre.agentIDs()
	if forceDeleteUserBeforeTx != nil {
		forceDeleteUserBeforeTx()
	}

	auditActor := s.buildAuditActorFromContext(ctx)
	var (
		d      *store.UserDeletion
		joined bool
	)
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if err := s.checkLastSuperAdminTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return err
		}
		if err := checkUserRemovalAdminLockout(ctx, tx, time.Now(), user.ID, userRemovalDelete); err != nil {
			return err
		}
		var err error
		d, joined, err = s.forceDeleteUserTx(ctx, tx, actor, byToken, user, authorized, auditActor)
		return err
	})
	if err != nil {
		var inProgress *userDeletionInProgressError
		switch {
		case errors.Is(err, errLastSuperAdmin):
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"cannot delete the last super-admin; promote another user first", nil)
		case errors.Is(err, errBindingStateDrift):
			writeBindingStateDrift(w, err)
		case errors.Is(err, errUserDeleteBlockersChanged):
			writeError(w, http.StatusConflict, ErrCodeUserDeleteBlockersChanged, err.Error(), nil)
		case errors.As(err, &inProgress):
			writeUserDeletionInProgress(w, inProgress)
		case errors.Is(err, store.ErrNotFound):
			NotFound(w, "User")
		case errors.Is(err, store.ErrTransient):
			// A PostgreSQL deadlock or serialization failure, for example
			// with the finalize of one of the user's agents: the
			// transaction rolled back, so nothing was written.
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
				"the user deletion conflicted with a concurrent change and wrote nothing; retry", nil)
		default:
			if writeConstraintAdminLockoutError(w, err) || writeUserDeleteCascadeError(w, err) {
				break
			}
			slog.ErrorContext(ctx, "user force delete failed", "user_id", user.ID, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"user deletion failed", nil)
		}
		return true
	}

	if !joined {
		// The user's authority changed: sessions and live connections
		// re-check it.
		s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: user.ID})
		// The user is deleting, so every fire under the user's recorded
		// authority would be refused: pause those schedules with
		// creator_deleted (they do not block the delete).
		s.pauseSchedulesAuthoredByUser(ctx, user.ID)
	}
	s.kickUserDeletion(d.ID, joined)
	s.writeUserDeletion(w, ctx, http.StatusAccepted, d)
	return true
}

// forceDeleteUserTx is the forced delete's revoke-and-tombstone
// transaction body, after the last super-admin and lockout guards. It
// returns the deletion, and joined=true when a deletion of the user was
// already recorded (by a concurrent request); then it writes nothing.
// byToken is set for a user access token: the role-binding cascade judges
// the token's system-binding rule on the bindings it deletes
// (tokenDeleteBindingsCheck), and the deletion records the token
// credential kind, so its completion applies the same rule.
func (s *Server) forceDeleteUserTx(ctx context.Context, tx store.Store, actor UserIdentity, byToken bool, user *store.User, authorized map[string]bool, auditActor AuditActor) (*store.UserDeletion, bool, error) {
	// User row first, exclusively: agent create, restore and the
	// completion serialize on it (lockAgentGuardUserTx).
	if err := tx.LockUserRow(ctx, user.ID, true); err != nil {
		return nil, false, err
	}
	if existing, err := tx.GetUserDeletionByUser(ctx, user.ID); err == nil {
		if existing.State != store.UserDeletionStateStopping {
			return nil, false, fmt.Errorf("user %s has a completed deletion but its row exists", user.ID)
		}
		return existing, true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	current, err := tx.GetUser(ctx, user.ID)
	if err != nil {
		return nil, false, err
	}

	cur, err := collectUserDeleteBlockers(ctx, tx, user.ID)
	if err != nil {
		return nil, false, err
	}
	for i := range cur.agents {
		if !authorized[cur.agents[i].ID] {
			return nil, false, errUserDeleteBlockersChanged
		}
	}

	ts := time.Now()
	// Last-project-owner guard and the role-binding cascade (with the
	// token rule for a token), before any other write.
	var bindingsCheck userBindingsCheck
	credentialKind := userDeletionCredentialKind
	if byToken {
		bindingsCheck = tokenDeleteBindingsCheck
		credentialKind = userDeletionTokenCredentialKind
	}
	if err := guardAndCascadeUserRoleBindingsCheckTx(ctx, tx, user.ID, s.membershipNow(), bindingsCheck); err != nil {
		return nil, false, err
	}
	tokens, err := revokeUserAccessTokensForUserTx(ctx, tx, user.ID)
	if err != nil {
		return nil, false, err
	}
	if err := tx.MarkUserDeleting(ctx, user.ID); err != nil {
		return nil, false, fmt.Errorf("mark user deleting: %w", err)
	}

	opID := uuid.NewString()
	deact := store.Deactivation{Cause: store.EdgeDeactivationUserDeleteCascade, OpID: opID}
	if _, err := tx.DeactivateDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, user.ID, deact); err != nil {
		return nil, false, fmt.Errorf("deactivate delegation edges of user: %w", err)
	}
	for i := range cur.agents {
		a := &cur.agents[i]
		if _, err := tx.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, a.ID, deact); err != nil {
			return nil, false, fmt.Errorf("deactivate delegation edges of agent %s: %w", a.ID, err)
		}
		if _, err := tx.DeactivateAgentServiceAccountAssignments(ctx, a.ID, deact); err != nil {
			return nil, false, fmt.Errorf("deactivate service-account assignments of agent %s: %w", a.ID, err)
		}
	}

	orphaned := make([]userDeletionRevokedEdge, 0, len(cur.edges))
	for _, e := range cur.edges {
		orphaned = append(orphaned, userDeletionRevokedEdge{AgentID: e.DelegateID, EdgeID: e.ID})
	}
	orphanedJSON, err := json.Marshal(orphaned)
	if err != nil {
		return nil, false, err
	}
	before, err := json.Marshal(map[string]string{"role": current.Role, "status": current.Status, "kind": current.Kind})
	if err != nil {
		return nil, false, err
	}
	d := &store.UserDeletion{
		ID:               opID,
		UserID:           user.ID,
		RequestedByKind:  userDeletionRequesterKind,
		RequestedByID:    actor.ID(),
		CredentialKind:   credentialKind,
		RequestedAt:      ts,
		BeforeSummary:    string(before),
		OrphanedByRevoke: string(orphanedJSON),
	}
	if err := tx.CreateUserDeletion(ctx, d); err != nil {
		return nil, false, fmt.Errorf("record user deletion: %w", err)
	}
	for i := range cur.agents {
		a := &cur.agents[i]
		if err := tx.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
			DeletionID: d.ID, AgentID: a.ID, Slug: a.Slug, ProjectID: a.ProjectID,
			Outcome: store.UserDeletionOutcomePending, UpdatedAt: ts,
		}); err != nil {
			return nil, false, fmt.Errorf("record user deletion agent %s: %w", a.ID, err)
		}
	}

	summary, err := json.Marshal(map[string]interface{}{
		"deletionId":  d.ID,
		"email":       current.Email,
		"role":        current.Role,
		"status":      current.Status,
		"agents":      len(cur.agents),
		"edges":       len(cur.edges),
		"uatsRevoked": tokens,
	})
	if err != nil {
		return nil, false, err
	}
	record := &store.MutationAuditRecord{
		MutationType:  "user_delete_requested",
		TargetType:    "user",
		TargetID:      user.ID,
		BeforeSummary: string(summary),
		Timestamp:     ts,
	}
	auditActor.ApplyActor(record)
	if err := tx.CreateMutationAudit(ctx, record); err != nil {
		return nil, false, fmt.Errorf("audit user delete request: %w", err)
	}
	return d, false, nil
}

// ---------------------------------------------------------------------------
// GET /api/v1/users/{id}/deletion — the forced deletion of a user
//
// Authorization: the same as DELETE /api/v1/users/{id} (session credential,
// then user.delete on the user). Behind hub.user_delete_force_cascade
// (404 while off). It answers from the tombstone, so it also answers after
// the user row is gone; 404 when the user has no deletion.
// ---------------------------------------------------------------------------

// userDeletionPrincipal names the requester of a deletion.
type userDeletionPrincipal struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// userDeletionAgentView is one agent of a deletion. Outcome stopped means
// the delete engine confirmed the agent's runtime teardown; removed means
// the agent's record is gone without that confirmation.
type userDeletionAgentView struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	ProjectID string `json:"projectId"`
	Outcome   string `json:"outcome"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"lastError"`
}

// userDeletionCounts counts a deletion's agents by outcome.
type userDeletionCounts struct {
	Pending  int `json:"pending"`
	Stopping int `json:"stopping"`
	Failed   int `json:"failed"`
	Stopped  int `json:"stopped"`
	Removed  int `json:"removed"`
}

func (c *userDeletionCounts) add(outcome string) {
	switch outcome {
	case store.UserDeletionOutcomePending:
		c.Pending++
	case store.UserDeletionOutcomeStopping:
		c.Stopping++
	case store.UserDeletionOutcomeFailed:
		c.Failed++
	case store.UserDeletionOutcomeStopped:
		c.Stopped++
	case store.UserDeletionOutcomeRemoved:
		c.Removed++
	}
}

// userDeletionRevokedEdge is an edge the request deactivated whose
// delegate is an agent the cascade does not stop (another user's agent).
type userDeletionRevokedEdge struct {
	AgentID string `json:"agentId"`
	EdgeID  string `json:"edgeId"`
}

// userDeletionView is the deletion resource: the body of GET
// /api/v1/users/{id}/deletion and of the 202 answer to a forced delete.
type userDeletionView struct {
	DeletionID       string                    `json:"deletionId"`
	UserID           string                    `json:"userId"`
	State            string                    `json:"state"`
	RequestedAt      time.Time                 `json:"requestedAt"`
	RequestedBy      userDeletionPrincipal     `json:"requestedBy"`
	CompletedAt      *time.Time                `json:"completedAt"`
	LastReconciledAt *time.Time                `json:"lastReconciledAt"`
	LastError        string                    `json:"lastError"`
	Agents           []userDeletionAgentView   `json:"agents"`
	Counts           userDeletionCounts        `json:"counts"`
	OrphanedByRevoke []userDeletionRevokedEdge `json:"orphanedByRevoke"`
}

// buildUserDeletionView reads the agents of d and builds its resource.
func buildUserDeletionView(ctx context.Context, st store.Store, d *store.UserDeletion) (*userDeletionView, error) {
	rows, err := st.ListUserDeletionAgents(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	v := &userDeletionView{
		DeletionID:       d.ID,
		UserID:           d.UserID,
		State:            d.State,
		RequestedAt:      d.RequestedAt,
		RequestedBy:      userDeletionPrincipal{Kind: d.RequestedByKind, ID: d.RequestedByID},
		CompletedAt:      d.CompletedAt,
		LastReconciledAt: d.LastReconciledAt,
		LastError:        d.LastError,
		Agents:           make([]userDeletionAgentView, 0, len(rows)),
		OrphanedByRevoke: []userDeletionRevokedEdge{},
	}
	for _, r := range rows {
		v.Agents = append(v.Agents, userDeletionAgentView{
			ID: r.AgentID, Slug: r.Slug, ProjectID: r.ProjectID,
			Outcome: r.Outcome, Attempts: r.Attempts, LastError: r.LastError,
		})
		v.Counts.add(r.Outcome)
	}
	if d.OrphanedByRevoke != "" {
		if err := json.Unmarshal([]byte(d.OrphanedByRevoke), &v.OrphanedByRevoke); err != nil {
			return nil, fmt.Errorf("decode revoked edges of deletion %s: %w", d.ID, err)
		}
	}
	return v, nil
}

// writeUserDeletion writes the deletion resource of d with status.
func (s *Server) writeUserDeletion(w http.ResponseWriter, ctx context.Context, status int, d *store.UserDeletion) {
	v, err := buildUserDeletionView(ctx, s.store, d)
	if err != nil {
		slog.ErrorContext(ctx, "user deletion: build resource failed", "deletion_id", d.ID, "error", err)
		InternalError(w)
		return
	}
	writeJSON(w, status, v)
}

// getUserDeletion serves GET /api/v1/users/{id}/deletion. It needs
// user.delete on the user (authorizeUserDelete): a session credential, or
// a user access token with user:delete on the hub-level user record.
func (s *Server) getUserDeletion(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	actor, token, ok := s.sessionOrUserTokenCredential(w, ctx, authzop.ReasonGovernancePending)
	if !ok {
		return
	}
	if !s.authorizeUserDelete(ctx, w, actor, token, id) {
		return
	}
	d, err := s.store.GetUserDeletionByUser(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "User deletion")
			return
		}
		slog.ErrorContext(ctx, "user deletion: read failed", "user_id", id, "error", err)
		InternalError(w)
		return
	}
	s.writeUserDeletion(w, ctx, http.StatusOK, d)
}
