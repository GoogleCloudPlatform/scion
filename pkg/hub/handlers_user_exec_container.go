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
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// Per-user execution container registration (/api/v1/users/me/exec-container)
//
// Allows a human user to register, inspect, and clear their default execution
// container (design §4.2–§4.4).
// =============================================================================

const (
	execContainerMaxBodyBytes = 4 * 1024

	// ErrCodeInvalidAgentID is returned (HTTP 400) when PUT
	// /api/v1/users/me/exec-container receives a malformed agentId.
	ErrCodeInvalidAgentID = "invalid_agent_id"

	// ErrCodeExecContainerIneligible is returned (HTTP 422) for every
	// ineligible target agent, with one fixed message and no existence oracle.
	ErrCodeExecContainerIneligible = "exec_container_ineligible"

	execContainerIneligibleMessage = "That agent can't be used as your default execution container. It must be an execution container you can attach to, in a project you can access."
	execContainerUnavailableReason = "Your registered execution container is no longer available. Register another."

	mutationTypeUserExecContainerSet   = "user.exec_container.set"
	mutationTypeUserExecContainerClear = "user.exec_container.clear"
)

type execContainerAgentResponse struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	ProjectID      string `json:"projectId"`
	ProjectName    string `json:"projectName"`
	Phase          string `json:"phase"`
	Activity       string `json:"activity"`
	HomePersistent *bool  `json:"homePersistent,omitempty"`
}

type execContainerResponse struct {
	Configured bool                        `json:"configured"`
	State      ExecContainerState          `json:"state,omitempty"`
	Agent      *execContainerAgentResponse `json:"agent,omitempty"`
	Reason     string                      `json:"reason,omitempty"`
}

type execContainerPutRequest struct {
	AgentID string `json:"agentId"`
}

// handleUserMeExecContainer routes GET/PUT/DELETE on
// /api/v1/users/me/exec-container. All three methods require an interactive
// session or dev credential (no UAT, PAT, or agent token). GET and PUT are
// gated on hub.exec_containers; DELETE is always allowed so a user can clear a
// registration even while the experiment is off.
func (s *Server) handleUserMeExecContainer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonInteractiveState)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.requireExperiment(experiments.ExecContainers, func(w http.ResponseWriter, r *http.Request) {
			s.getUserExecContainer(w, r, actor)
		})(w, r)
	case http.MethodPut:
		s.requireExperiment(experiments.ExecContainers, func(w http.ResponseWriter, r *http.Request) {
			s.putUserExecContainer(w, r, actor)
		})(w, r)
	case http.MethodDelete:
		s.deleteUserExecContainer(w, r, actor)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) getUserExecContainer(w http.ResponseWriter, r *http.Request, actor UserIdentity) {
	ctx := r.Context()

	res, err := s.resolveExecContainerForIdentity(ctx, actor)
	if err != nil {
		slog.Error("failed to resolve execution container", "user_id", actor.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to load execution container", nil)
		return
	}

	writeJSON(w, http.StatusOK, s.buildExecContainerResponse(ctx, res))
}

func (s *Server) putUserExecContainer(w http.ResponseWriter, r *http.Request, actor UserIdentity) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, execContainerMaxBodyBytes)

	var body execContainerPutRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		BadRequest(w, "invalid JSON body")
		return
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		BadRequest(w, "body must contain a single JSON object")
		return
	}

	if !canonicalUUIDPattern.MatchString(body.AgentID) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidAgentID, "agentId must be a canonical UUID", nil)
		return
	}
	agentID := strings.ToLower(body.AgentID)

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("failed to load agent for execution container registration", "user_id", actor.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to validate execution container", nil)
		return
	}

	eligible := err == nil && s.isExecContainerEligible(ctx, actor, agent)
	if !eligible {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeExecContainerIneligible, execContainerIneligibleMessage, nil)
		return
	}

	prev, prevErr := s.store.GetUserExecContainer(ctx, actor.ID())
	if prevErr != nil && !errors.Is(prevErr, store.ErrNotFound) {
		slog.Error("failed to read previous execution container", "user_id", actor.ID(), "error", prevErr)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save execution container", nil)
		return
	}

	if _, err := s.store.PutUserExecContainer(ctx, actor.ID(), agentID); err != nil {
		slog.Error("failed to save execution container", "user_id", actor.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save execution container", nil)
		return
	}

	auditRec := &store.MutationAuditRecord{
		MutationType: mutationTypeUserExecContainerSet,
		TargetType:   "user",
		TargetID:     actor.ID(),
		AfterSummary: fmt.Sprintf("agent_id=%s", agentID),
	}
	if prev != nil && prev.AgentID != agentID {
		auditRec.BeforeSummary = fmt.Sprintf("agent_id=%s", prev.AgentID)
	}
	s.emitMutationAuditSync(ctx, auditRec)

	res := ExecContainerResolution{
		State:          mapExecContainerState(agent),
		Agent:          agent,
		RunID:          agent.RunID,
		HomePersistent: resolveAgentHomePersistent(agent),
	}
	writeJSON(w, http.StatusOK, s.buildExecContainerResponse(ctx, res))
}

func (s *Server) deleteUserExecContainer(w http.ResponseWriter, r *http.Request, actor UserIdentity) {
	ctx := r.Context()

	prev, err := s.store.GetUserExecContainer(ctx, actor.ID())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("failed to check execution container before clear", "user_id", actor.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to clear execution container", nil)
		return
	}

	if err := s.store.DeleteUserExecContainer(ctx, actor.ID()); err != nil {
		slog.Error("failed to clear execution container", "user_id", actor.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to clear execution container", nil)
		return
	}

	if prev != nil {
		s.emitMutationAuditSync(ctx, &store.MutationAuditRecord{
			MutationType:  mutationTypeUserExecContainerClear,
			TargetType:    "user",
			TargetID:      actor.ID(),
			BeforeSummary: fmt.Sprintf("agent_id=%s", prev.AgentID),
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) buildExecContainerResponse(ctx context.Context, res ExecContainerResolution) execContainerResponse {
	switch res.State {
	case ExecContainerStateUnconfigured:
		return execContainerResponse{Configured: false}
	case ExecContainerStateUnavailable:
		return execContainerResponse{
			Configured: true,
			State:      ExecContainerStateUnavailable,
			Reason:     execContainerUnavailableReason,
		}
	default:
		if res.Agent == nil {
			return execContainerResponse{
				Configured: true,
				State:      ExecContainerStateUnavailable,
				Reason:     execContainerUnavailableReason,
			}
		}
		var projectName string
		if res.Agent.ProjectID != "" {
			if proj, err := s.store.GetProject(ctx, res.Agent.ProjectID); err == nil && proj != nil {
				projectName = proj.Name
			}
		}
		return execContainerResponse{
			Configured: true,
			State:      res.State,
			Agent: &execContainerAgentResponse{
				ID:             res.Agent.ID,
				Slug:           res.Agent.Slug,
				Name:           res.Agent.Name,
				ProjectID:      res.Agent.ProjectID,
				ProjectName:    projectName,
				Phase:          res.Agent.Phase,
				Activity:       res.Agent.Activity,
				HomePersistent: res.HomePersistent,
			},
		}
	}
}
