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
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent/google"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	ManagedAgentsProfile = "managed-agents"
	ManagedRuntimePrefix = "managed:"

	annotationCloudProvider = "scion.dev/cloud-provider"
	annotationInteractionID = "scion.dev/interaction-id"
	annotationEnvironmentID = "scion.dev/environment-id"
)

var (
	managedBackendMu   sync.Mutex
	managedBackendInst managedagent.ManagedAgentBackend
)

// getManagedBackend returns the lazily-initialized managed agent backend.
// It reads settings from the global settings file on first call.
// Transient init failures are not cached so subsequent calls can retry.
func getManagedBackend() (managedagent.ManagedAgentBackend, error) {
	managedBackendMu.Lock()
	defer managedBackendMu.Unlock()

	if managedBackendInst != nil {
		return managedBackendInst, nil
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, fmt.Errorf("resolving global dir: %w", err)
	}

	vs, err := config.LoadSingleFileVersioned(globalDir)
	if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}

	if vs.ManagedAgents == nil || vs.ManagedAgents.Google == nil {
		return nil, fmt.Errorf("managed_agents.google configuration not found in settings")
	}

	cfg := vs.ManagedAgents.Google
	backend, err := google.NewBackend(google.BackendConfig{
		APIKey:    cfg.APIKey,
		BaseAgent: cfg.BaseAgent,
		Model:     cfg.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("creating managed agent backend: %w", err)
	}

	managedBackendInst = backend
	return managedBackendInst, nil
}

// isManagedAgentRuntime returns true if the runtime string indicates a managed agent.
func isManagedAgentRuntime(runtime string) bool {
	return strings.HasPrefix(runtime, ManagedRuntimePrefix)
}

// managedAgentCreate handles agent creation for the managed-agents profile.
// It creates the cloud agent via the backend and stores the cloud agent ID
// in the agent's annotations.
func (s *Server) managedAgentCreate(ctx context.Context, agent *store.Agent, task string) error {
	backend, err := getManagedBackend()
	if err != nil {
		return fmt.Errorf("managed agent backend: %w", err)
	}

	agent.Runtime = ManagedRuntimePrefix + backend.Name()

	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations[annotationCloudProvider] = backend.Name()

	// Skip the /v1beta/agents CRUD endpoint — it may not be generally
	// available. Go directly to creating an interaction via
	// /v1beta/interactions, which works with built-in agent names.
	if task != "" {
		var systemInstruction string
		if agent.AppliedConfig != nil && agent.AppliedConfig.InlineConfig != nil {
			systemInstruction = agent.AppliedConfig.InlineConfig.SystemPrompt
			if systemInstruction == "" {
				systemInstruction = agent.AppliedConfig.InlineConfig.AgentInstructions
			}
		}
		handle, err := backend.CreateInteraction(ctx, managedagent.InteractionRequest{
			Input:             task,
			SystemInstruction: systemInstruction,
			Environment:       &managedagent.EnvironmentConfig{Type: "remote"},
			Background:        true,
		})
		if err != nil {
			return fmt.Errorf("creating initial interaction: %w", err)
		}
		agent.Annotations[annotationInteractionID] = handle.InteractionID
		if handle.EnvironmentID != "" {
			agent.Annotations[annotationEnvironmentID] = handle.EnvironmentID
		}
	}

	return nil
}

// managedAgentMessage sends a message to a managed agent by creating a new interaction.
func (s *Server) managedAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool) error {
	backend, err := getManagedBackend()
	if err != nil {
		return fmt.Errorf("managed agent backend: %w", err)
	}

	if interrupt {
		if interactionID := agent.Annotations[annotationInteractionID]; interactionID != "" {
			if cancelErr := backend.CancelInteraction(ctx, interactionID); cancelErr != nil {
				slog.Warn("failed to cancel interaction for interrupt", "agent_id", agent.ID, "err", cancelErr)
			}
		}
	}

	if message == "" {
		return nil
	}

	req := managedagent.InteractionRequest{
		Input:                 message,
		PreviousInteractionID: agent.Annotations[annotationInteractionID],
		EnvironmentID:         agent.Annotations[annotationEnvironmentID],
		Background:            true,
	}

	handle, err := backend.CreateInteraction(ctx, req)
	if err != nil {
		return fmt.Errorf("creating interaction: %w", err)
	}

	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations[annotationInteractionID] = handle.InteractionID
	if handle.EnvironmentID != "" {
		agent.Annotations[annotationEnvironmentID] = handle.EnvironmentID
	}

	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		slog.Warn("failed to persist interaction annotations", "agent_id", agent.ID, "err", err)
	}

	return nil
}

// managedAgentStop stops a managed agent by cancelling the active interaction.
func (s *Server) managedAgentStop(ctx context.Context, agent *store.Agent) error {
	if _, err := getManagedBackend(); err != nil {
		return fmt.Errorf("managed agent backend: %w", err)
	}

	// Best-effort: a failed read or cancel is logged, not returned.
	if interactionID := agent.Annotations[annotationInteractionID]; interactionID != "" {
		if err := stopManagedInteraction(ctx, interactionID); err != nil {
			slog.Warn("managed agent stop: failed to stop interaction", "agent_id", agent.ID, "err", err)
		}
	}

	return nil
}

// managedAgentDelete deletes a managed agent's cloud resources.
func (s *Server) managedAgentDelete(ctx context.Context, agent *store.Agent) error {
	// Stop first (best-effort) — cancels the active interaction.
	_ = s.managedAgentStop(ctx, agent)
	return nil
}

// Warnings a managed create that lost to a delete reports in the 409's
// details.warnings (compensateManagedCreate).
const (
	managedCreateCompensatedWarning      = "agent was deleted while it was being created; its managed-agent interaction was stopped"
	managedCreateCompensateFailedWarning = "agent was deleted while it was being created; stopping its managed-agent interaction failed: "
)

// compensateManagedCreate cleans up the cloud side of a managed (hub-direct)
// create whose delete won the race (ptone/scion#3454), and returns the
// warnings for the 409.
//
// The delete engine already calls managedAgentDelete, but only when the row
// it read right after its claim has the managed Runtime, and it can only
// stop an interaction that row names. managedAgentCreate sets both in
// memory, and only the create's post-create write persists them. So
// whether the engine stops this create's interaction depends on whether
// that write landed first:
//
//   - recorded (the write succeeded): it landed before the claim, since a
//     claim bumps state_version and a later write would have conflicted. The
//     engine's row carries the managed Runtime and the interaction ID, and
//     the engine stops it, so nothing is done here; this avoids a second
//     stop.
//   - not recorded: the claim may have come first, so the engine's row has
//     neither the managed Runtime nor the interaction ID, and the engine
//     cannot stop it. This create holds the only copy of the ID, so it stops
//     the interaction itself.
//
// A create with no task started no interaction: nothing to clean up.
//
// The stop runs detached from the request with its own budget, as
// compensateLandedRun's delete does: a client that goes away must not leave
// an interaction running that nothing else can stop. Unlike
// managedAgentDelete, a failure is reported (stopManagedInteraction).
func (s *Server) compensateManagedCreate(ctx context.Context, agent *store.Agent, recorded bool) []string {
	interactionID := agent.Annotations[annotationInteractionID]
	if recorded || interactionID == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensatingDeleteTimeout)
	defer cancel()
	if err := stopManagedInteraction(cctx, interactionID); err != nil {
		s.agentLifecycleLog.Warn("Failed to stop the managed agent interaction of a create that lost to a delete",
			"agent_id", agent.ID, "interaction_id", interactionID, "error", err)
		return []string{managedCreateCompensateFailedWarning + err.Error()}
	}
	return []string{managedCreateCompensatedWarning}
}

// stopManagedInteraction cancels interactionID if it is still in progress,
// and returns any backend error, including a failed read of the
// interaction (its state is then unknown). An interaction that has already
// ended needs no cancel. A read that returns no state is an error too: the
// state is unknown. managedAgentStop calls it best-effort, logging and
// swallowing these errors.
func stopManagedInteraction(ctx context.Context, interactionID string) error {
	backend, err := getManagedBackend()
	if err != nil {
		return fmt.Errorf("managed agent backend: %w", err)
	}
	st, err := backend.GetInteraction(ctx, interactionID)
	if err != nil {
		return fmt.Errorf("reading interaction: %w", err)
	}
	if st == nil {
		return errors.New("reading interaction: no state returned")
	}
	if st.Status != managedagent.StatusInProgress {
		return nil
	}
	if err := backend.CancelInteraction(ctx, interactionID); err != nil {
		return fmt.Errorf("cancelling interaction: %w", err)
	}
	return nil
}

// handleManagedAgentLifecycle handles lifecycle actions (start, stop, restart) for managed agents.
func (s *Server) handleManagedAgentLifecycle(w http.ResponseWriter, r *http.Request, agent *store.Agent, action string) {
	ctx := r.Context()

	var newPhase string
	var actionErr error

	// Record the run intent before acting, as the broker-backed lifecycle
	// paths do. A stop whose action fails keeps intent stopped.
	var intent store.RunIntent
	switch action {
	case "start", "restart":
		intent = store.RunIntentRunning
	case "stop":
		intent = store.RunIntentStopped
	}
	if intent != "" {
		if _, err := s.recordRunIntent(ctx, agent, intent); err != nil {
			writeRunIntentError(w, err, agent.ID)
			return
		}
	}

	switch action {
	case "start":
		newPhase = string("running")
	case "stop":
		newPhase = string("stopped")
		// Clear exposed ports — agent is stopping, ports are unreachable
		s.clearExposedPortsForAgent(ctx, agent.ID)
		actionErr = s.managedAgentStop(ctx, agent)
	case "restart":
		_ = s.managedAgentStop(ctx, agent)
		newPhase = string("running")
	case "suspend":
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"Suspend is not supported for managed agents — use stop instead.", nil)
		return
	default:
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			fmt.Sprintf("Unknown lifecycle action %q for managed agent", action), nil)
		return
	}

	if actionErr != nil {
		RuntimeError(w, "Failed managed agent lifecycle action: "+actionErr.Error())
		return
	}

	statusUpdate := store.AgentStatusUpdate{
		Phase: newPhase,
	}
	if action == "stop" {
		statusUpdate.Activity = ""
	}
	if err := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// A successful start/stop/restart clears a failed delete marker
	// (design ptone/scion#2483 §2.1); publish and respond from the stored
	// row, which a racing delete claim may have kept off newPhase.
	s.settleLifecycleWrite(ctx, agent, newPhase)
	s.events.PublishAgentStatus(ctx, agent)

	respAgent := *agent
	respAgent.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
	respAgent.Deletion = deletionViewForCaller(agent, time.Now(), callerSeesDeletionDetail(ctx))
	writeJSON(w, http.StatusOK, &respAgent)
}

// formatManagedAgentLook returns the latest interaction formatted as structured text.
func formatManagedAgentLook(ctx context.Context, agent *store.Agent) (string, error) {
	state, err := managedAgentGetInteraction(ctx, agent)
	if err != nil {
		return fmt.Sprintf("[status] %s (no active interaction)\n", agent.Phase), nil
	}

	// TODO: add per-step timestamps ([HH:MM:SS] prefix) once the backend
	// API exposes them on managedagent.Step — see design doc section 4.5.
	var b strings.Builder
	for _, step := range state.Steps {
		stepType := step.Type
		if stepType == "" {
			stepType = "output"
		}
		text := step.Text
		if text == "" && step.Arguments != "" {
			text = step.ToolName + "(" + step.Arguments + ")"
		}
		fmt.Fprintf(&b, "[%s] %s\n", stepType, text)
	}

	statusLine := string(state.Status)
	if state.Usage != nil {
		statusLine += fmt.Sprintf(" (%d input / %d output tokens)",
			state.Usage.TotalInputTokens, state.Usage.TotalOutputTokens)
	}
	fmt.Fprintf(&b, "[status] %s\n", statusLine)

	return b.String(), nil
}

// managedAgentGetInteraction retrieves the latest interaction state for a managed agent.
func managedAgentGetInteraction(ctx context.Context, agent *store.Agent) (*managedagent.InteractionState, error) {
	backend, err := getManagedBackend()
	if err != nil {
		return nil, fmt.Errorf("managed agent backend: %w", err)
	}

	interactionID := agent.Annotations[annotationInteractionID]
	if interactionID == "" {
		return nil, fmt.Errorf("no active interaction for agent %s", agent.Slug)
	}

	return backend.GetInteraction(ctx, interactionID)
}
