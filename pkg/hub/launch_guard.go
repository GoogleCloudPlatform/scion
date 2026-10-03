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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Start guard for agents whose asynchronous create is in flight or did not
// complete.

const (
	// ErrCodeAgentLaunching is returned by entry points that refuse to act
	// on an agent whose create launch is still in flight.
	ErrCodeAgentLaunching = "agent_launching"
	// ErrCodeAgentCreateIncomplete is returned by every start path for an
	// agent whose create launch stopped or failed before the agent ran.
	ErrCodeAgentCreateIncomplete = "agent_create_incomplete"
)

const (
	// launchRestartNotPerformedWarning is the lifecycle restart answer for an
	// agent that is launching.
	launchRestartNotPerformedWarning = "agent is launching; restart not performed"
	// launchInFlightMessage is the wake-DM and reincarnate message for an
	// agent that is launching.
	launchInFlightMessage = "agent is launching"
)

// ErrLaunchInFlight is returned by DispatchAgentStart and
// DispatchAgentRestart when the agent's create launch is in flight and has
// not reached its deadline. No broker call is made.
var ErrLaunchInFlight = errors.New("agent is launching")

// ErrAgentCreateIncomplete is matched (via errors.Is) by the
// *AgentCreateIncompleteError the dispatcher returns for an agent whose
// create launch did not complete.
var ErrAgentCreateIncomplete = errors.New("agent create did not complete")

// AgentCreateIncompleteError carries the user-facing refusal for an agent
// whose create launch did not complete. errors.Is(err,
// ErrAgentCreateIncomplete) is true for it.
type AgentCreateIncompleteError struct {
	Message string
}

func (e *AgentCreateIncompleteError) Error() string { return e.Message }

// Is reports whether target is ErrAgentCreateIncomplete.
func (e *AgentCreateIncompleteError) Is(target error) bool {
	return target == ErrAgentCreateIncomplete
}

// launchInFlightBeforeDeadline reports whether a's create launch is in
// flight and has not passed its deadline. Past the deadline the launch is
// being reaped or superseded, so start may proceed.
func launchInFlightBeforeDeadline(a *store.Agent, now time.Time) bool {
	if a == nil || !a.IsInFlight() {
		return false
	}
	return a.LaunchDeadline.IsZero() || now.Before(a.LaunchDeadline)
}

// agentRecoveryName is the name shown in recovery hints.
func agentRecoveryName(a *store.Agent) string {
	if a.Slug != "" {
		return a.Slug
	}
	return a.Name
}

// incompleteCreateRecoveryHint tells the user how to recover an incomplete
// create. A plain delete is soft while soft-delete retention is configured,
// and a soft-deleted agent keeps its name reserved, so the hint does not
// promise that a plain delete frees the name.
const incompleteCreateRecoveryHint = "delete it and create it again; " +
	"if soft-delete retention is enabled, the name stays reserved until the agent is " +
	"deleted with force=true or purged"

// incompleteCreateMessage is the user-facing message for an incomplete
// create.
func incompleteCreateMessage(a *store.Agent) string {
	name := agentRecoveryName(a)
	if a.LaunchState == store.LaunchStateActive {
		return fmt.Sprintf("agent %s cannot be started: its create is still stopping; %s", name, incompleteCreateRecoveryHint)
	}
	return fmt.Sprintf("agent %s cannot be started: its create did not complete (%s); %s", name, a.LaunchError, incompleteCreateRecoveryHint)
}

// incompleteCreateRefusal returns the 409 agent_create_incomplete refusal
// when a matches the incomplete-create predicate, or nil.
func incompleteCreateRefusal(a *store.Agent) *startRefusal {
	if a == nil || !a.IsIncompleteCreate() {
		return nil
	}
	details := map[string]interface{}{
		"template": a.Template,
		"task":     "",
	}
	if a.AppliedConfig != nil {
		details["task"] = a.AppliedConfig.Task
	}
	return &startRefusal{
		HTTPStatus: http.StatusConflict,
		Code:       ErrCodeAgentCreateIncomplete,
		Message:    incompleteCreateMessage(a),
		Details:    details,
		launch:     true,
	}
}

// inFlightRefusal returns the 409 agent_launching refusal when a's create
// launch is in flight and before its deadline, or nil. Callers that answer
// 200 with the current agent check InFlight and do not write it.
func inFlightRefusal(a *store.Agent, now time.Time) *startRefusal {
	if !launchInFlightBeforeDeadline(a, now) {
		return nil
	}
	return &startRefusal{
		HTTPStatus: http.StatusConflict,
		Code:       ErrCodeAgentLaunching,
		Message:    launchInFlightMessage,
		InFlight:   true,
		launch:     true,
	}
}

// launchStartRefusal runs startGate's launch steps in order: incomplete
// create, then in flight. Call startGate rather than this directly, so the
// delete check runs first.
func launchStartRefusal(a *store.Agent, now time.Time) *startRefusal {
	if r := incompleteCreateRefusal(a); r != nil {
		return r
	}
	return inFlightRefusal(a, now)
}

// launchGuardError is the dispatcher backstop: it re-reads the agent and
// returns ErrLaunchInFlight or an *AgentCreateIncompleteError. A read error
// keeps today's behaviour (no refusal); the caller's own dispatch reports
// any real problem.
func (d *HTTPAgentDispatcher) launchGuardError(ctx context.Context, agent *store.Agent, op string) error {
	if d.store == nil || agent == nil || agent.ID == "" {
		return nil
	}
	fresh, err := d.store.GetAgent(ctx, agent.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("start guard: agent re-read failed, proceeding",
				"op", op, "agent_id", agent.ID, "error", err)
		}
		return nil
	}
	if fresh.IsIncompleteCreate() {
		return &AgentCreateIncompleteError{Message: incompleteCreateMessage(fresh)}
	}
	if launchInFlightBeforeDeadline(fresh, time.Now()) {
		return ErrLaunchInFlight
	}
	return nil
}

// deferredLaunchGuardError maps a failed cross-node start or restart onto
// the start guard's error when the guard now refuses. The owner node re-runs
// the guard; if a launch began, or a create stopped, between this node's
// check and the owner's, the owner refuses and this node's wait fails
// without the reason. Re-checking here gives the caller the same answer it
// would have had from a local dispatch. Any other failure is returned
// unchanged. The re-read is detached from ctx, which may have expired.
func (d *HTTPAgentDispatcher) deferredLaunchGuardError(ctx context.Context, agent *store.Agent, op string, err error) error {
	if err == nil {
		return nil
	}
	guardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if guardErr := d.launchGuardError(guardCtx, agent, op); guardErr != nil {
		return guardErr
	}
	return err
}

// launchRefusalFromError maps a dispatcher guard error onto the HTTP
// refusal, re-reading the agent for the incomplete-create details. It
// returns nil for any other error.
func (s *Server) launchRefusalFromError(ctx context.Context, agentID string, err error) *startRefusal {
	switch {
	case errors.Is(err, ErrLaunchInFlight):
		return &startRefusal{
			HTTPStatus: http.StatusConflict,
			Code:       ErrCodeAgentLaunching,
			Message:    launchInFlightMessage,
			InFlight:   true,
			launch:     true,
		}
	case errors.Is(err, ErrAgentCreateIncomplete):
		if fresh, gerr := s.store.GetAgent(ctx, agentID); gerr == nil {
			if r := incompleteCreateRefusal(fresh); r != nil {
				return r
			}
		}
		return &startRefusal{
			HTTPStatus: http.StatusConflict,
			Code:       ErrCodeAgentCreateIncomplete,
			Message:    err.Error(),
			launch:     true,
		}
	}
	return nil
}

// AgentWithWarnings is the lifecycle start/restart answer when the request
// was not performed because the agent is launching: the agent plus
// warnings.
type AgentWithWarnings struct {
	store.Agent
	Warnings []string `json:"warnings,omitempty"`
}

// writeLaunchingAgent answers 200 with the current agent (re-read, with its
// launch view) and the given warnings.
func (s *Server) writeLaunchingAgent(ctx context.Context, w http.ResponseWriter, agent *store.Agent, warnings []string) {
	current := agent
	if fresh, err := s.store.GetAgent(ctx, agent.ID); err == nil {
		current = fresh
	}
	current.Launch = store.ComputeAgentLaunch(current, time.Now())
	resp := AgentWithWarnings{Agent: *redactedAgentCopy(ctx, s, current), Warnings: warnings}
	writeJSON(w, http.StatusOK, resp)
}

// createRequestHasInputs reports whether a create/start request carried
// inputs that a launching agent will not apply (template, task, config and
// the like).
func createRequestHasInputs(req CreateAgentRequest) bool {
	return req.Template != "" || req.Task != "" || req.HarnessConfig != "" ||
		req.HarnessAuth != "" || req.Profile != "" || req.Branch != "" ||
		req.Workspace != "" || len(req.Labels) > 0 || req.Config != nil ||
		req.AgentRole != "" || req.MessageMode != "" || req.GCPIdentity != nil ||
		len(req.WorkspaceFiles) > 0 || req.Resume || req.ForceResume
}

// lifecycleStartHasInputs reports whether a lifecycle start request body
// carries inputs a launching agent will not apply. The body is decoded
// rather than judged by its length, so an empty object or a chunked body is
// classified by its fields. A body that cannot be decoded counts as inputs,
// so the caller is still warned. It consumes the request body.
func lifecycleStartHasInputs(r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return true
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var req AgentLifecycleStartRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return true
	}
	return req != (AgentLifecycleStartRequest{})
}

// writeExistingAgentLaunching answers handleExistingAgent's 200 shape for an
// agent whose create launch is in flight.
func (s *Server) writeExistingAgentLaunching(ctx context.Context, w http.ResponseWriter, agent *store.Agent, project *store.Project, req CreateAgentRequest) {
	current := agent
	if fresh, err := s.store.GetAgent(ctx, agent.ID); err == nil {
		current = fresh
	}
	s.enrichAgent(ctx, current, project, nil)
	resp := CreateAgentResponse{Agent: redactedAgentCopy(ctx, s, current)}
	if createRequestHasInputs(req) {
		resp.Warnings = []string{launchInFlightInputsWarning}
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeExistingAgentGuardError answers a dispatcher start-guard error in
// handleExistingAgent. It reports false (nothing written) for any other
// error.
func (s *Server) writeExistingAgentGuardError(ctx context.Context, w http.ResponseWriter, agent *store.Agent, project *store.Project, req CreateAgentRequest, err error) (existingAgentResult, bool) {
	refusal := s.launchRefusalFromError(ctx, agent.ID, err)
	if refusal == nil {
		return existingAgentNone, false
	}
	if !refusal.InFlight {
		refusal.write(w)
		return existingAgentErrored, true
	}
	s.writeExistingAgentLaunching(ctx, w, agent, project, req)
	return existingAgentStarted, true
}
