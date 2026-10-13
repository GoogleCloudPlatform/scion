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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Bulk scope re-issue (ptone/scion#3652): POST
// /api/v1/admin/agents/reset-auth-all with {"reissue_scopes": true}. Every
// non-deleted agent on the hub goes through exactly the single-agent
// re-issue (runScopeReissue): its own computation, transaction, audit row,
// revocation and push. Nothing is computed per batch, and no agent's
// result depends on the operator.
//
//   - Dry run is the default: dry_run is true unless the body sets it to
//     false (the CLI's --apply).
//   - The enumeration is paginated to the end and never truncated. If a
//     page cannot be read the batch fails with an error before any agent is
//     processed: a truncated run would be an incomplete clawback that looks
//     complete.
//   - Within each project, agents run top-down by delegation depth (roots,
//     then their children), so a child is computed against its parent's
//     freshly committed record. A child whose parent was refused is
//     computed against the parent's unchanged record. Bounded concurrency
//     runs within one depth level only.
//   - A failure affects that agent only. Re-running is idempotent: agents
//     already re-issued are no-ops.
//   - One agent_scopes_reissue_batch row records the operator, dry_run and
//     counts only (no scope names). If it cannot be written the response
//     says so (batch_audit_recorded=false).
//   - A dry run runs each agent through the single-agent dry run, so it
//     audits exactly as --dry-run does (a dry_run=true row per agent, or a
//     denial row, each with the batch_op_id) and changes nothing else; the
//     would-be result of each changed agent goes to its descendants
//     (reissueOverlay), so it reports exactly what --apply would change.
//   - The run is detached from the request and bounded by
//     ReissueBulkRunTimeout.
//
// The bulk run is never triggered automatically.

// mutationTypeAgentScopesReissueBatch is the batch audit record.
const mutationTypeAgentScopesReissueBatch = "agent_scopes_reissue_batch"

// Bulk tuning.
const (
	reissueBulkConcurrency = 8
	// reissueBulkMaxPages bounds the enumeration against a cursor loop.
	reissueBulkMaxPages = 100000
	// reissueBulkAgentTimeout bounds one agent's re-issue (computation,
	// commit and push).
	reissueBulkAgentTimeout = time.Minute
	// ReissueBulkRunTimeout bounds a whole bulk run. The run is detached
	// from the request (a client that goes away does not stop it), so this
	// is its only deadline. The CLI waits this long, plus a margin, for the
	// response.
	ReissueBulkRunTimeout = 30 * time.Minute
	// reissueBulkAuditTimeout bounds the batch audit write, which uses a
	// fresh context so it is written even when the run's deadline passed.
	reissueBulkAuditTimeout = 10 * time.Second
)

// reissueBulkAgentHook is a test seam called as each agent's turn starts.
// Nil in production.
var reissueBulkAgentHook func(agentID string)

// reissueBulkPageSize is the enumeration page size (a variable so tests can
// force several pages).
var reissueBulkPageSize = 200

// errReissueEnumeration marks an enumeration that could not complete.
var errReissueEnumeration = errors.New("scope re-issue: agent enumeration incomplete")

// reissueBulkListFault is a test seam: when set, it is called for each page
// before the store read, and a non-nil error fails that page. Nil in
// production.
var reissueBulkListFault func(page int) error

// ScopeReissueBulkAgent is one agent's outcome in a bulk run.
type ScopeReissueBulkAgent struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	ProjectID   string   `json:"project_id"`
	Depth       int      `json:"depth"`
	Outcome     string   `json:"outcome"` // changed | noop | refused | push_failed
	Cause       string   `json:"cause,omitempty"`
	Added       []string `json:"added,omitempty"`
	Removed     []string `json:"removed,omitempty"`
	RoleBefore  string   `json:"role_before,omitempty"`
	RoleAfter   string   `json:"role_after,omitempty"`
	OpID        string   `json:"op_id,omitempty"`
	DispatchErr string   `json:"dispatch_error,omitempty"`
}

// ScopeReissueBulkRef names an agent in a result list.
type ScopeReissueBulkRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Cause string `json:"cause,omitempty"`
}

// ScopeReissueBulkResponse is the response to a bulk re-issue.
type ScopeReissueBulkResponse struct {
	BatchOpID  string                  `json:"batch_op_id"`
	DryRun     bool                    `json:"dry_run"`
	Total      int                     `json:"total"`
	Succeeded  []ScopeReissueBulkRef   `json:"succeeded"`
	Noop       []ScopeReissueBulkRef   `json:"noop"`
	Refused    []ScopeReissueBulkRef   `json:"refused"`
	PushFailed []ScopeReissueBulkRef   `json:"push_failed"`
	Agents     []ScopeReissueBulkAgent `json:"agents"`
	// DepthUnresolved lists agents placed at depth 0 because their
	// delegation edge could not be read (or was not a single active edge).
	// Their own run reports why; their descendants may have been computed
	// before them.
	DepthUnresolved []string `json:"depth_unresolved"`
	// BatchAuditRecorded is false when the agent_scopes_reissue_batch row
	// could not be written. The per-agent rows are unaffected.
	BatchAuditRecorded bool `json:"batch_audit_recorded"`
}

// reissueBatchSummary is the AfterSummary of agent_scopes_reissue_batch:
// counts only.
type reissueBatchSummary struct {
	BatchOpID  string `json:"batch_op_id"`
	DryRun     bool   `json:"dry_run"`
	Total      int    `json:"total"`
	Succeeded  int    `json:"succeeded"`
	Noop       int    `json:"noop"`
	Refused    int    `json:"refused"`
	PushFailed int    `json:"push_failed"`
}

// ScopeReissueBulkRequest is the reset-auth-all request body. DryRun is a
// pointer so an absent field means a dry run.
type ScopeReissueBulkRequest struct {
	ReissueScopes bool  `json:"reissue_scopes"`
	DryRun        *bool `json:"dry_run"`
}

// listAllAgentsForReissue returns every non-deleted agent on the hub,
// reading pages until the cursor is exhausted. Any page error, a repeated
// cursor, or too many pages returns errReissueEnumeration.
func (s *Server) listAllAgentsForReissue(ctx context.Context) ([]store.Agent, error) {
	var (
		all    []store.Agent
		cursor string
		seen   = map[string]bool{}
	)
	for page := 0; ; page++ {
		if page >= reissueBulkMaxPages {
			return nil, fmt.Errorf("%w: page limit reached", errReissueEnumeration)
		}
		if reissueBulkListFault != nil {
			if err := reissueBulkListFault(page); err != nil {
				return nil, fmt.Errorf("%w: page %d: %w", errReissueEnumeration, page, err)
			}
		}
		res, err := s.store.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{
			Limit: reissueBulkPageSize, Cursor: cursor, SkipTotalCount: true,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: page %d: %w", errReissueEnumeration, page, err)
		}
		if res == nil {
			return nil, fmt.Errorf("%w: page %d: no result", errReissueEnumeration, page)
		}
		for _, a := range res.Items {
			if a.DeletedAt.IsZero() {
				all = append(all, a)
			}
		}
		if res.NextCursor == "" {
			return all, nil
		}
		if seen[res.NextCursor] {
			return nil, fmt.Errorf("%w: cursor repeated", errReissueEnumeration)
		}
		seen[res.NextCursor] = true
		cursor = res.NextCursor
	}
}

// reissueDepths returns each agent's delegation depth within the set: 0 for
// an agent whose active edge names a user (or that has no single active
// edge, so its own run refuses), else 1 + its parent agent's depth. A
// parent outside the set (deleted) counts as depth 0 above it. A cycle or a
// lookup fault leaves the agent at depth 0; its own run then reports the
// fault.
//
// Ordering is per project: delegation edges are project-scoped
// (activeProjectEdges reads only the agent's own project), and an agent
// whose delegator agent sits in another project is refused by its own run
// (planAgentDelegatorReissue), so no cross-project parent can need to run
// first. unresolved lists the agents whose edge could not be read or was
// not a single active edge.
func (s *Server) reissueDepths(ctx context.Context, agents []store.Agent) (depths map[string]int, unresolved []string) {
	parentOf := make(map[string]string, len(agents))
	inSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		inSet[a.ID] = true
	}
	for _, a := range agents {
		edges, err := s.authzService.activeProjectEdges(ctx, a.ID, a.ProjectID)
		if err != nil || len(edges) != 1 {
			if err != nil {
				slog.ErrorContext(ctx, "scope re-issue: bulk depth: edge lookup failed", "agent_id", a.ID, "error", err)
			}
			unresolved = append(unresolved, a.ID)
			continue
		}
		if e := edges[0]; e.DelegatorType == store.DelegationPrincipalAgent && inSet[e.DelegatorID] {
			parentOf[a.ID] = e.DelegatorID
		}
	}
	depth := make(map[string]int, len(agents))
	var resolve func(id string, guard int) int
	resolve = func(id string, guard int) int {
		if d, ok := depth[id]; ok {
			return d
		}
		p, ok := parentOf[id]
		if !ok || guard > maxDelegationDepth {
			depth[id] = 0
			return 0
		}
		d := resolve(p, guard+1) + 1
		depth[id] = d
		return d
	}
	for _, a := range agents {
		resolve(a.ID, 0)
	}
	sort.Strings(unresolved)
	return depth, unresolved
}

// runScopeReissueBulk runs the bulk re-issue. It returns an error only when
// the enumeration fails; per-agent failures are reported in the response.
func (s *Server) runScopeReissueBulk(ctx context.Context, operator reissueOperator, dryRun bool) (*ScopeReissueBulkResponse, error) {
	agents, err := s.listAllAgentsForReissue(ctx)
	if err != nil {
		return nil, err
	}
	batchOpID := api.NewUUID()
	depths, unresolved := s.reissueDepths(ctx, agents)
	if dryRun {
		// The would-be result of each agent is handed to the agents after
		// it, so the dry run reports exactly what --apply would change.
		ctx = withReissueOverlay(ctx, newReissueOverlay())
	}

	// Group by project, then by depth.
	type key struct {
		project string
		depth   int
	}
	levels := map[key][]store.Agent{}
	projects := map[string]int{}
	for _, a := range agents {
		d := depths[a.ID]
		levels[key{a.ProjectID, d}] = append(levels[key{a.ProjectID, d}], a)
		if cur, ok := projects[a.ProjectID]; !ok || d > cur {
			projects[a.ProjectID] = d
		}
	}
	projectIDs := make([]string, 0, len(projects))
	for p := range projects {
		projectIDs = append(projectIDs, p)
	}
	sort.Strings(projectIDs)

	results := make([]ScopeReissueBulkAgent, 0, len(agents))
	var mu sync.Mutex
	for _, p := range projectIDs {
		for d := 0; d <= projects[p]; d++ {
			level := levels[key{p, d}]
			sort.Slice(level, func(i, j int) bool { return level[i].ID < level[j].ID })
			sem := make(chan struct{}, reissueBulkConcurrency)
			var wg sync.WaitGroup
			for i := range level {
				a := level[i]
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					r := s.reissueOneForBulk(ctx, &a, d, operator, dryRun, batchOpID)
					mu.Lock()
					results = append(results, r)
					mu.Unlock()
				}()
			}
			wg.Wait()
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].ProjectID != results[j].ProjectID {
			return results[i].ProjectID < results[j].ProjectID
		}
		if results[i].Depth != results[j].Depth {
			return results[i].Depth < results[j].Depth
		}
		return results[i].ID < results[j].ID
	})
	resp := &ScopeReissueBulkResponse{
		BatchOpID: batchOpID, DryRun: dryRun, Total: len(results),
		Succeeded: []ScopeReissueBulkRef{}, Noop: []ScopeReissueBulkRef{},
		Refused: []ScopeReissueBulkRef{}, PushFailed: []ScopeReissueBulkRef{},
		Agents: results, DepthUnresolved: unresolved,
	}
	if resp.DepthUnresolved == nil {
		resp.DepthUnresolved = []string{}
	}
	for _, r := range results {
		ref := ScopeReissueBulkRef{ID: r.ID, Name: r.Name, Cause: r.Cause}
		switch r.Outcome {
		case "changed":
			resp.Succeeded = append(resp.Succeeded, ref)
		case "noop":
			resp.Noop = append(resp.Noop, ref)
		case "push_failed":
			resp.PushFailed = append(resp.PushFailed, ref)
		default:
			resp.Refused = append(resp.Refused, ref)
		}
	}

	// The batch row is written with a fresh bounded context, so it is
	// recorded even when the run's own deadline has passed.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reissueBulkAuditTimeout)
	defer cancel()
	record, err := lifecycleAudit(mutationTypeAgentScopesReissueBatch, "", auditActorFromContext(ctx), time.Now(), reissueBatchSummary{
		BatchOpID: batchOpID, DryRun: dryRun, Total: resp.Total,
		Succeeded: len(resp.Succeeded), Noop: len(resp.Noop), Refused: len(resp.Refused), PushFailed: len(resp.PushFailed),
	})
	if err == nil {
		record.TargetType = "hub"
		record.TargetID = "hub"
		err = s.store.CreateMutationAudit(auditCtx, record)
	}
	if err != nil {
		slog.ErrorContext(ctx, "scope re-issue: batch audit write failed", "batch_op_id", batchOpID, "error", err)
	}
	resp.BatchAuditRecorded = err == nil
	return resp, nil
}

// reissueOneForBulk runs the single-agent re-issue for a (runScopeReissue,
// dry run or applied) and classifies the outcome. It re-reads the agent so
// it computes against its own current row. In a dry run the context carries
// the overlay, so the would-be result of a changed agent reaches the agents
// after it.
func (s *Server) reissueOneForBulk(ctx context.Context, a *store.Agent, depth int, operator reissueOperator, dryRun bool, batchOpID string) ScopeReissueBulkAgent {
	if reissueBulkAgentHook != nil {
		reissueBulkAgentHook(a.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, reissueBulkAgentTimeout)
	defer cancel()
	out := ScopeReissueBulkAgent{ID: a.ID, Name: a.Name, ProjectID: a.ProjectID, Depth: depth}
	fresh, err := s.store.GetAgent(ctx, a.ID)
	if err != nil || fresh == nil {
		out.Outcome = "refused"
		if errors.Is(err, store.ErrNotFound) || (err == nil && fresh == nil) {
			out.Cause = string(DenyCauseCeilingOrphaned)
		} else {
			slog.ErrorContext(ctx, "scope re-issue: bulk agent lookup failed",
				"agent_id", a.ID, "batch_op_id", batchOpID, "error", err)
			out.Cause = mintErrorClassLookup
		}
		return out
	}
	resp, err := s.runScopeReissue(ctx, fresh, operator, dryRun, batchOpID)
	if err != nil {
		out.Outcome = "refused"
		out.Cause = reissueBulkCause(ctx, err, a.ID, batchOpID)
		return out
	}
	out.OpID = resp.OpID
	out.Added = resp.Added
	out.Removed = resp.Removed
	out.RoleBefore = resp.RoleBefore
	out.RoleAfter = resp.RoleAfter
	switch {
	case resp.Noop:
		out.Outcome = "noop"
	case resp.DispatchError != "":
		out.Outcome = "push_failed"
		out.DispatchErr = resp.DispatchError
	default:
		out.Outcome = "changed"
	}
	return out
}

// reissueBulkCause is the cause reported for a refused agent.
func reissueBulkCause(ctx context.Context, err error, agentID, batchOpID string) string {
	var issueErr *agentTokenIssueError
	switch {
	case errors.As(err, &issueErr):
		if issueErr.Lookup {
			slog.ErrorContext(ctx, "scope re-issue: bulk agent lookup fault",
				"agent_id", agentID, "batch_op_id", batchOpID, "error", err)
		}
		return issueErr.errorClass()
	case errors.Is(err, errReissueConflict), errors.Is(err, store.ErrVersionConflict), errors.Is(err, store.ErrAlreadyExists):
		return "conflict"
	default:
		slog.ErrorContext(ctx, "scope re-issue: bulk agent failed with an unclassified error",
			"agent_id", agentID, "batch_op_id", batchOpID, "error", err)
		return "error"
	}
}

// handleAdminScopeReissueAll handles POST /api/v1/admin/agents/reset-auth-all
// with {"reissue_scopes": true}.
func (s *Server) handleAdminScopeReissueAll(w http.ResponseWriter, r *http.Request, req ScopeReissueBulkRequest) {
	operator, ok := s.authorizeScopeReissue(w, r, "")
	if !ok {
		return
	}
	dryRun := req.DryRun == nil || *req.DryRun
	// The run is detached from the request: a client that disconnects or
	// times out does not cancel the remaining agents or lose the batch row.
	// ReissueBulkRunTimeout is its own deadline, and the response may take
	// that long, past the server's usual write timeout.
	extendWriteDeadline(r.Context(), w, s.config.WriteTimeout, ReissueBulkRunTimeout+time.Minute)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), ReissueBulkRunTimeout)
	defer cancel()
	resp, err := s.runScopeReissueBulk(ctx, operator, dryRun)
	if err != nil {
		slog.ErrorContext(r.Context(), "bulk scope re-issue failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"The agent list could not be read completely; nothing was changed. Retry later", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
