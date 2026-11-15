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

// Runtime orphan report (ptone/scion#7930, behind
// experiments.RuntimeOrphanReconcile). A broker heartbeat lists every
// runtime entry of its targets; the entries the per-agent loop cannot match
// to an agent of this broker, and entries whose reported agent ID differs
// from the agent their slug matches, are orphan candidates. After the loop,
// recordRuntimeOrphans classifies the candidates on the targets the
// heartbeat lists completely and records them in runtime_orphans. Report
// only: nothing here stops or deletes anything.
//
// Reasons:
//   - no_agent: the project is known, and no agent has the entry's slug or
//     agent ID;
//   - superseded: a live agent has the slug but another agent ID (the slug
//     was reused);
//   - deleted: the entry's agent ID is a soft-deleted agent;
//   - other_broker: the agent is assigned to another broker;
//   - unknown_project: the project ID is not in this Hub's database (a
//     wrong hub or a database restore; never actionable);
//   - unlabeled: the entry has no project or slug.
//
// An entry from an older broker carries no agent ID and is matched by slug
// only, so a reused slug reads as matched: the safe direction. An entry
// whose agent has a delete in flight or a queued start is not an orphan.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Reasons of a runtime orphan.
const (
	orphanNoAgent        = "no_agent"
	orphanSuperseded     = "superseded"
	orphanDeleted        = "deleted"
	orphanOtherBroker    = "other_broker"
	orphanUnknownProject = "unknown_project"
	orphanUnlabeled      = "unlabeled"
)

// runtimeOrphanRemoveAfter is the number of complete inventories in a row
// without an orphan after which its row is removed as resolved.
const runtimeOrphanRemoveAfter = 3

// runtimeOrphanCandidate is a heartbeat entry the per-agent loop did not
// match to an agent of the broker, or matched by slug to an agent with
// another ID. bySlug is the live agent with the entry's slug, nil when the
// lookup failed with lookupErr.
type runtimeOrphanCandidate struct {
	projectID string
	hb        brokerAgentHeartbeat
	bySlug    *store.Agent
	lookupErr error
}

// heartbeatAgentID returns a reported agent ID in canonical UUID form, or ""
// when it is not a UUID: a legacy container's agent_id label holds its
// name, which must not read as another agent's ID.
func heartbeatAgentID(id string) string {
	u, err := uuid.Parse(id)
	if err != nil {
		return ""
	}
	return u.String()
}

// recordRuntimeOrphans classifies the heartbeat's orphan candidates and
// records them, with the store's resolution rule for rows this inventory no
// longer lists. It does nothing while the experiment is off or when the
// heartbeat lists no target completely (a project-filtered broker, or every
// target's listing failed). Candidates on targets that were not listed
// completely are dropped.
func (s *Server) recordRuntimeOrphans(ctx context.Context, brokerID string, hb *brokerHeartbeatRequest, report *heartbeatReport) {
	if !s.experimentEnabled(experiments.RuntimeOrphanReconcile) {
		return
	}
	complete := hb.completeTargets()
	if len(complete) == 0 {
		return
	}
	log := s.agentLifecycleLog.With("broker_id", brokerID)
	pending, err := report.pendingStarts(ctx, s, brokerID)
	if err != nil {
		log.Warn("runtime orphans: failed to list pending dispatches", "error", err)
		return
	}
	// projectKnown memoises project existence for this heartbeat.
	known := map[string]bool{}
	projectKnown := func(id string) (bool, error) {
		if ok, seen := known[id]; seen {
			return ok, nil
		}
		_, err := s.store.GetProject(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidInput) {
			return false, err
		}
		known[id] = err == nil
		return err == nil, nil
	}
	// The agents of candidates without a live slug match, by reported
	// ID, read in one batch (soft-deleted rows included).
	var ids []string
	for _, c := range report.orphanCandidates {
		if complete[c.hb.RuntimeTarget] && c.bySlug == nil && c.hb.AgentID != "" {
			ids = append(ids, c.hb.AgentID)
		}
	}
	byID, err := s.store.GetAgentsByIDsIncludingDeleted(ctx, ids)
	if err != nil {
		log.Warn("runtime orphans: failed to read reported agents", "error", err)
		return
	}
	var orphans []store.RuntimeOrphan
	for _, c := range report.orphanCandidates {
		if !complete[c.hb.RuntimeTarget] {
			continue
		}
		reason, err := s.classifyRuntimeOrphan(brokerID, c, pending, projectKnown, byID)
		if err != nil {
			// Not classified this time; the row, if any, counts as missed,
			// so three failures in a row remove it and reset first_seen_at
			// (the safe direction for a later grace clock).
			log.Warn("runtime orphans: failed to classify entry", "project_id", c.projectID, "slug", c.hb.Slug, "error", err)
			continue
		}
		if reason == "" {
			continue
		}
		orphans = append(orphans, store.RuntimeOrphan{
			Kind: store.RuntimeOrphanKindInstance, RuntimeTarget: c.hb.RuntimeTarget, ProjectID: c.projectID,
			Slug: c.hb.Slug, AgentID: c.hb.AgentID, RunID: c.hb.RunID, Reason: reason, ObservedPhase: c.hb.Phase,
		})
	}
	targets := make([]string, 0, len(complete))
	for t := range complete {
		targets = append(targets, t)
	}
	created, err := s.store.RecordRuntimeOrphans(ctx, brokerID, targets, orphans, time.Now(), runtimeOrphanRemoveAfter)
	if err != nil {
		log.Warn("runtime orphans: failed to record", "error", err)
	}
	for _, o := range created {
		log.Warn("runtime orphan: runtime entry has no hub agent",
			"reason", o.Reason, "runtime_target", o.RuntimeTarget, "project_id", o.ProjectID,
			"slug", o.Slug, "agent_id", o.AgentID, "run_id", o.RunID, "observed_phase", o.ObservedPhase)
	}
}

// classifyRuntimeOrphan returns the orphan reason of c, or "" when c is not
// an orphan. An error means c could not be classified.
// byID holds the agents of the reported IDs; an ID it lacks has no agent.
func (s *Server) classifyRuntimeOrphan(brokerID string, c runtimeOrphanCandidate, pending map[string]bool, projectKnown func(string) (bool, error), byID map[string]*store.Agent) (string, error) {
	if c.projectID == "" || c.projectID == "default" || c.hb.Slug == "" {
		return orphanUnlabeled, nil
	}
	if c.lookupErr != nil && !errors.Is(c.lookupErr, store.ErrNotFound) && !errors.Is(c.lookupErr, store.ErrInvalidInput) {
		return "", c.lookupErr
	}
	if ok, err := projectKnown(c.projectID); err != nil || !ok {
		if err != nil {
			return "", err
		}
		return orphanUnknownProject, nil
	}
	// busy reports whether a's delete or start may still change what runs.
	busy := func(a *store.Agent) bool {
		return a.DeletionHoldsRow(time.Now()) || pending[a.ID] || s.lifecycleOps.active(a.ID)
	}
	if a := c.bySlug; a != nil {
		switch {
		case busy(a):
			return "", nil
		case c.hb.AgentID != "" && c.hb.AgentID != a.ID:
			return orphanSuperseded, nil
		case a.RuntimeBrokerID != brokerID:
			return orphanOtherBroker, nil
		}
		return "", nil
	}
	if c.hb.AgentID == "" {
		return orphanNoAgent, nil
	}
	a := byID[c.hb.AgentID]
	switch {
	case a == nil:
		return orphanNoAgent, nil
	case !a.DeletedAt.IsZero():
		return orphanDeleted, nil
	case busy(a):
		return "", nil
	case a.RuntimeBrokerID != brokerID:
		return orphanOtherBroker, nil
	}
	// A live agent of this broker under another slug: not an orphan.
	return "", nil
}

// RegisterRuntimeOrphanMetrics registers scion.hub.runtime_orphans, the
// number of recorded runtime orphans by broker, kind and reason, read from
// the store at each collection. It observes nothing while the experiment
// is off.
func (s *Server) RegisterRuntimeOrphanMetrics(mp metric.MeterProvider) error {
	if mp == nil {
		return errors.New("runtime orphan metrics: nil MeterProvider")
	}
	_, err := mp.Meter(instrumentationScope).Int64ObservableGauge("scion.hub.runtime_orphans",
		metric.WithUnit("{orphan}"),
		metric.WithDescription("Runtime objects no Hub agent owns, by broker, kind and reason (report only)"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			if !s.experimentEnabled(experiments.RuntimeOrphanReconcile) {
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			counts, err := s.store.CountRuntimeOrphans(ctx)
			if err != nil {
				return err
			}
			for _, c := range counts {
				o.Observe(int64(c.Count), metric.WithAttributes(attribute.String("broker", c.BrokerID),
					attribute.String("kind", c.Kind), attribute.String("reason", c.Reason)))
			}
			return nil
		}))
	return err
}

// RuntimeOrphansResponse is the body of GET /api/v1/admin/runtime-orphans.
type RuntimeOrphansResponse struct {
	Orphans []store.RuntimeOrphan `json:"orphans"`
}

// handleAdminRuntimeOrphans serves GET /api/v1/admin/runtime-orphans: the
// recorded runtime orphans, oldest first, for operators. Query parameters
// broker, project and kind (instance or nfs_tree) filter the list. The
// route guard requires hub.health.read on a session credential. It answers
// 404 while the experiment is off.
func (s *Server) handleAdminRuntimeOrphans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.experimentEnabled(experiments.RuntimeOrphanReconcile) {
		NotFound(w, "route")
		return
	}
	q := r.URL.Query()
	f := store.RuntimeOrphanFilter{BrokerID: q.Get("broker"), ProjectID: q.Get("project"), Kind: q.Get("kind")}
	switch f.Kind {
	case "", store.RuntimeOrphanKindInstance, store.RuntimeOrphanKindNFSTree:
	default:
		BadRequest(w, "kind must be instance or nfs_tree")
		return
	}
	orphans, err := s.store.ListRuntimeOrphans(r.Context(), f)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, RuntimeOrphansResponse{Orphans: orphans})
}
