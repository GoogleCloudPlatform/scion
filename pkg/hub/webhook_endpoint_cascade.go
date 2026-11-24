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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// releaseTriggerBindingsForAgent executes the Stage 2 §4.2 / S23 agent deletion
// cascade for webhook endpoints bound to agent:
//   - bound_lifetime == "agent": deletes the endpoint, its secret rows, and any
//     triggers on it authored by the deleted agent; pauses triggers on it
//     authored by others with status_reason = "source_deleted". Writes one
//     MutationAuditRecord per deleted endpoint (webhook_endpoint_delete,
//     BeforeSummary reason=bound_agent_deleted), per deleted trigger
//     (trigger_delete), and per paused trigger (trigger_pause).
//   - bound_lifetime == "persistent": unbinds the endpoint (bound_agent_id=NULL,
//     bound_lifetime="") leaving URL, secret, owner, and rules intact; writes
//     one MutationAuditRecord (webhook_endpoint_unbind). Rules targeting the
//     deleted agent are subsequently paused with target_deleted by
//     pauseTriggersTargetingAgent.
//
// Idempotent on deletion retry: already-deleted and already-unbound endpoints
// are no longer returned by ListWebhookEndpoints(BoundAgentID: agent.ID).
// Runs once on soft delete; restoring a soft-deleted agent does not resurrect
// deleted endpoints.
func (s *Server) releaseTriggerBindingsForAgent(ctx context.Context, agent *store.Agent) {
	if s == nil || s.store == nil || agent == nil || agent.ID == "" || agent.ProjectID == "" {
		return
	}

	var bound []store.WebhookEndpoint
	opts := store.ListOptions{Limit: 100}
	for {
		if ctx.Err() != nil {
			s.agentLifecycleLog.Warn("Stopped listing bound webhook endpoints for deleted agent: context done",
				"agent_id", agent.ID, "project_id", agent.ProjectID, "error", ctx.Err())
			return
		}
		page, err := s.store.ListWebhookEndpoints(ctx, store.WebhookEndpointFilter{
			ProjectID:    agent.ProjectID,
			BoundAgentID: agent.ID,
		}, opts)
		if err != nil {
			s.agentLifecycleLog.Warn("Failed to list bound webhook endpoints for deleted agent",
				"agent_id", agent.ID, "project_id", agent.ProjectID, "error", err)
			return
		}
		bound = append(bound, page.Items...)
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}

	for _, ep := range bound {
		if ctx.Err() != nil {
			s.agentLifecycleLog.Warn("Stopped releasing trigger bindings for deleted agent: context done",
				"agent_id", agent.ID, "project_id", agent.ProjectID, "error", ctx.Err())
			return
		}
		switch ep.BoundLifetime {
		case store.WebhookBoundLifetimeAgent:
			s.deleteAgentLifetimeWebhookEndpoint(ctx, agent, ep)
		case store.WebhookBoundLifetimePersistent:
			s.unbindPersistentWebhookEndpoint(ctx, agent, ep)
		}
	}
}

// sweepOrphanedAgentLifetimeWebhookEndpoints is the hourly maintenance backstop
// for agent-lifetime webhook endpoints whose bound agent is soft-deleted or
// missing (e.g. if the best-effort deletion-engine tail step failed on a
// transient store error). It delegates to deleteAgentLifetimeWebhookEndpoint so
// cleanup, trigger pausing/deletion, cache invalidation, and audit records are
// identical and idempotent.
func (s *Server) sweepOrphanedAgentLifetimeWebhookEndpoints(ctx context.Context) int {
	if s == nil || s.store == nil {
		return 0
	}
	const sweepBatchLimit = 100
	orphans, err := s.store.ListOrphanedAgentLifetimeWebhookEndpoints(ctx, sweepBatchLimit)
	if err != nil {
		s.agentLifecycleLog.Warn("Failed to list orphaned agent-lifetime webhook endpoints during maintenance",
			"error", err)
		return 0
	}
	swept := 0
	for _, ep := range orphans {
		if ctx.Err() != nil {
			break
		}
		if ep.BoundAgentID == "" {
			continue
		}
		if s.deleteAgentLifetimeWebhookEndpoint(ctx, &store.Agent{ID: ep.BoundAgentID, ProjectID: ep.ProjectID}, ep) {
			swept++
		}
	}
	return swept
}

func (s *Server) deleteAgentLifetimeWebhookEndpoint(ctx context.Context, agent *store.Agent, ep store.WebhookEndpoint) bool {
	var deletedTriggerIDs, pausedTriggerIDs []string
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		var dErr error
		deletedTriggerIDs, pausedTriggerIDs, _, dErr = tx.DeleteWebhookEndpointForBoundAgent(ctx, ep.ProjectID, ep.ID, agent.ID)
		if dErr != nil {
			return dErr
		}
		epAudit := newWebhookEndpointAudit(ctx, mutationTypeWebhookEndpointDelete, ep.ID)
		beforeBytes, _ := json.Marshal(map[string]any{
			"reason":         "bound_agent_deleted",
			"endpoint_id":    ep.ID,
			"project_id":     ep.ProjectID,
			"name":           ep.Name,
			"bound_agent_id": agent.ID,
			"bound_lifetime": ep.BoundLifetime,
		})
		epAudit.BeforeSummary = string(beforeBytes)
		afterBytes, _ := json.Marshal(map[string]any{
			"project_id":            ep.ProjectID,
			"deleted_trigger_count": len(deletedTriggerIDs),
			"paused_trigger_count":  len(pausedTriggerIDs),
		})
		epAudit.AfterSummary = string(afterBytes)
		if aErr := tx.CreateMutationAudit(ctx, epAudit); aErr != nil {
			return aErr
		}
		for _, trID := range deletedTriggerIDs {
			trAudit := newTriggerAudit(ctx, mutationTypeTriggerDelete, trID)
			trBefore, _ := json.Marshal(map[string]any{
				"reason":         "bound_agent_deleted",
				"trigger_id":     trID,
				"project_id":     ep.ProjectID,
				"endpoint_id":    ep.ID,
				"bound_agent_id": agent.ID,
			})
			trAudit.BeforeSummary = string(trBefore)
			if aErr := tx.CreateMutationAudit(ctx, trAudit); aErr != nil {
				return aErr
			}
		}
		for _, trID := range pausedTriggerIDs {
			trAudit := newTriggerAudit(ctx, mutationTypeTriggerPause, trID)
			trAfter, _ := json.Marshal(map[string]any{
				"trigger_id":    trID,
				"project_id":    ep.ProjectID,
				"endpoint_id":   ep.ID,
				"status":        store.TriggerStatusPaused,
				"status_reason": store.TriggerStatusReasonSourceDeleted,
			})
			trAudit.AfterSummary = string(trAfter)
			if aErr := tx.CreateMutationAudit(ctx, trAudit); aErr != nil {
				return aErr
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		s.agentLifecycleLog.Warn("Failed to delete agent-lifetime webhook endpoint for deleted agent",
			"agent_id", agent.ID, "project_id", ep.ProjectID, "endpoint_id", ep.ID, "error", err)
		return false
	}
	if s.webhookIngress != nil {
		s.webhookIngress.InvalidateEndpoint(ep.ID)
	}
	return true
}

func (s *Server) unbindPersistentWebhookEndpoint(ctx context.Context, agent *store.Agent, ep store.WebhookEndpoint) {
	var unbound bool
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		var uErr error
		unbound, uErr = tx.UnbindWebhookEndpointForAgent(ctx, ep.ProjectID, ep.ID, agent.ID)
		if uErr != nil || !unbound {
			return uErr
		}
		audit := newWebhookEndpointAudit(ctx, mutationTypeWebhookEndpointUnbind, ep.ID)
		beforeBytes, _ := json.Marshal(map[string]any{
			"reason":         "bound_agent_deleted",
			"endpoint_id":    ep.ID,
			"project_id":     ep.ProjectID,
			"name":           ep.Name,
			"bound_agent_id": agent.ID,
			"bound_lifetime": ep.BoundLifetime,
		})
		audit.BeforeSummary = string(beforeBytes)
		afterBytes, _ := json.Marshal(map[string]any{
			"endpoint_id":    ep.ID,
			"project_id":     ep.ProjectID,
			"bound_agent_id": "",
			"bound_lifetime": "",
		})
		audit.AfterSummary = string(afterBytes)
		return tx.CreateMutationAudit(ctx, audit)
	})
	if err != nil {
		s.agentLifecycleLog.Warn("Failed to unbind persistent webhook endpoint for deleted agent",
			"agent_id", agent.ID, "project_id", ep.ProjectID, "endpoint_id", ep.ID, "error", err)
		return
	}
	if unbound && s.webhookIngress != nil {
		s.webhookIngress.InvalidateEndpoint(ep.ID)
	}
}
