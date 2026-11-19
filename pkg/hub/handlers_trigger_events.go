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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// handleTriggerEvents routes requests under
// /api/v1/projects/{projectId}/trigger-events[/{id}] (§10.1, §10.2).
func (s *Server) handleTriggerEvents(w http.ResponseWriter, r *http.Request, projectID, eventPath string) {
	s.requireExperiment(experiments.Triggers, func(w http.ResponseWriter, r *http.Request) {
		identity := GetIdentityFromContext(r.Context())
		if identity == nil {
			Unauthorized(w)
			return
		}
		if !checkAgentReadScope(w, r) {
			return
		}
		if agentIdentity := GetAgentIdentityFromContext(r.Context()); agentIdentity != nil {
			if agentIdentity.ProjectID() != projectID {
				Forbidden(w)
				return
			}
		}

		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}

		authzAction := ActionList
		if eventPath != "" {
			if strings.Contains(eventPath, "/") {
				NotFound(w, "Trigger event")
				return
			}
			authzAction = ActionRead
		}

		if !s.authorizeTriggerEventAccess(w, r, projectID, authzAction) {
			return
		}

		if eventPath == "" {
			s.listTriggerEvents(w, r, projectID)
			return
		}
		s.getTriggerEvent(w, r, projectID, eventPath)
	})(w, r)
}

func (s *Server) listTriggerEvents(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	query := r.URL.Query()

	sourceKind := strings.TrimSpace(query.Get("source"))
	if sourceKind == "" {
		sourceKind = strings.TrimSpace(query.Get("sourceKind"))
	}
	sourceID := strings.TrimSpace(query.Get("sourceId"))
	if sourceID != "" {
		if _, parseErr := uuid.Parse(sourceID); parseErr != nil {
			// Allow callers to filter by an endpoint name they can read without
			// revealing endpoints owned by other principals.
			if ep, err := s.store.GetWebhookEndpointByName(ctx, projectID, sourceID); err == nil && ep != nil {
				if s.webhookEndpointAccessDecision(ctx, projectID, ActionRead, ep).allowed {
					sourceID = ep.ID
				}
			}
		}
	}

	status := strings.TrimSpace(query.Get("status"))
	if status != "" {
		switch status {
		case store.TriggerEventStatusPending, store.TriggerEventStatusProcessed, store.TriggerEventStatusFailed:
		default:
			ValidationError(w, fmt.Sprintf("invalid status %q (expected pending, processed, or failed)", status), nil)
			return
		}
	}

	var since *time.Time
	if sinceRaw := strings.TrimSpace(query.Get("since")); sinceRaw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, sinceRaw)
		if err != nil {
			ValidationError(w, "invalid since timestamp (expected RFC3339)", nil)
			return
		}
		utc := parsed.UTC()
		since = &utc
	}

	filter := store.TriggerEventFilter{
		ProjectID:  projectID,
		SourceKind: sourceKind,
		SourceID:   sourceID,
		Status:     status,
		Since:      since,
	}
	scope := ""
	if isAgentCaller(ctx) {
		identity := GetIdentityFromContext(ctx)
		if identity == nil || identity.ID() == "" {
			Unauthorized(w)
			return
		}
		filter.VisibleToAgentID = identity.ID()
		scope = "caller"
	}

	result, err := s.store.ListTriggerEvents(ctx, filter, listOptionsFromQuery(query))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListTriggerEventsResponse{
		Scope:      scope,
		Events:     buildTriggerEventSummaryResponses(result.Items),
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
		ServerTime: time.Now().UTC(),
	})
}

func (s *Server) getTriggerEvent(w http.ResponseWriter, r *http.Request, projectID, eventID string) {
	ctx := r.Context()
	ev, err := s.store.GetTriggerEvent(ctx, eventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Trigger event")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if ev == nil || ev.ProjectID != projectID {
		NotFound(w, "Trigger event")
		return
	}
	if !s.authorizeTriggerEventRow(w, r, projectID, ActionRead, ev) {
		return
	}

	writeJSON(w, http.StatusOK, buildTriggerEventDetailResponse(*ev))
}
