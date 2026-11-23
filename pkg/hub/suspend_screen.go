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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// capturedScreen is the in-memory result of a pre-suspend terminal capture
// (ptone/scion#8530).
type capturedScreen struct {
	Content    string    // ANSI-preserving, valid UTF-8, no NULs, <= suspendScreenMaxBytes
	CapturedAt time.Time // hub clock at capture (UTC, microsecond-truncated)
	Truncated  bool      // content was cut to suspendScreenMaxBytes (tail kept)
}

const (
	// suspendScreenExecTimeoutSeconds is the timeout sent with the exec to the broker.
	suspendScreenExecTimeoutSeconds = 4
	// suspendScreenMaxBytes is the hard byte cap on stored screen content (64 KiB).
	suspendScreenMaxBytes = 64 << 10
	// suspendScreenWriteTimeout bounds the post-suspend store upsert/delete.
	suspendScreenWriteTimeout = 3 * time.Second
)

// suspendScreenTimeout bounds the hub side of the pre-suspend capture. Must be
// <= workspaceCheckTimeout so the concurrent pre-stop step never exceeds the
// budget already in stopWriteBudget(). A variable so tests can shorten it.
var suspendScreenTimeout = 5 * time.Second

func suspendScreenCommand() []string {
	return []string{"tmux", "capture-pane", "-pe", "-t", "scion"}
}

// normalizeScreen cleans raw tmux capture-pane output for DB storage:
//  1. Coerces invalid UTF-8 to U+FFFD and strips NUL bytes (Postgres text rejects NUL).
//  2. Trims trailing blank lines (tmux pads the visible pane).
//  3. Caps length at suspendScreenMaxBytes by keeping the tail, cutting at the
//     first newline after the cut point (or the first UTF-8 rune boundary when no
//     newline exists in the tail).
func normalizeScreen(raw string) (string, bool) {
	s := strings.ToValidUTF8(raw, "\uFFFD")
	s = strings.ReplaceAll(s, "\x00", "")

	// Trim trailing blank lines while keeping a single trailing newline for
	// non-empty output.
	lines := strings.Split(s, "\n")
	end := len(lines)
	for end > 0 && strings.TrimRight(lines[end-1], " \t\r") == "" {
		end--
	}
	if end == 0 {
		return "", false
	}
	s = strings.Join(lines[:end], "\n") + "\n"

	if len(s) <= suspendScreenMaxBytes {
		return s, false
	}

	tail := s[len(s)-suspendScreenMaxBytes:]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl+1 < len(tail) {
		// Cut at the start of the first complete line after the cut point.
		return tail[nl+1:], true
	}
	// No newline inside the tail (single huge line): advance to a valid UTF-8
	// rune start so we never split a multi-byte character.
	for i := 0; i < len(tail); i++ {
		if utf8.RuneStart(tail[i]) {
			return tail[i:], true
		}
	}
	return "", true
}

// captureSuspendScreen captures the agent's visible tmux pane before a suspend.
// It is best-effort and never returns an error: nil means "no screen"
// (experiment disabled, no dispatcher/broker, not running, exec error/timeout,
// non-zero exit, or empty output). Screen content is never logged.
func (s *Server) captureSuspendScreen(ctx context.Context, d AgentDispatcher, a *store.Agent) *capturedScreen {
	if !s.experimentEnabled(experiments.SuspendScreen) {
		return nil
	}
	if d == nil || a == nil || a.RuntimeBrokerID == "" {
		return nil
	}
	if a.Phase != string(state.PhaseRunning) && !a.IdleSuspendClaimActive {
		return nil
	}

	cctx, cancel := context.WithTimeout(ctx, suspendScreenTimeout)
	output, exitCode, err := d.DispatchAgentExec(cctx, a, suspendScreenCommand(), suspendScreenExecTimeoutSeconds)
	cancel()

	if err != nil || exitCode != 0 {
		s.agentLifecycleLog.Info("Pre-suspend screen capture skipped; continuing without it",
			"agent_id", a.ID, "agent", a.Name, "exit_code", exitCode, "error", err)
		return nil
	}

	content, truncated := normalizeScreen(output)
	if content == "" {
		return nil
	}
	return &capturedScreen{
		Content:    content,
		CapturedAt: time.Now().UTC().Truncate(time.Microsecond),
		Truncated:  truncated,
	}
}

// preSuspendProbes runs the ephemeral workspace check and the screen capture
// concurrently and returns the captured screen (or nil). Both are read-only
// execs against the live container, each bounded to <= workspaceCheckTimeout,
// so the pair never exceeds the budget already in stopWriteBudget().
func (s *Server) preSuspendProbes(ctx context.Context, d AgentDispatcher, a *store.Agent, warn bool) *capturedScreen {
	var cap *capturedScreen
	var wg sync.WaitGroup
	var cp store.Agent
	if a != nil {
		cp = *a
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if a == nil {
			return
		}
		cap = s.captureSuspendScreen(ctx, d, &cp)
	}()
	s.checkEphemeralWorkspaceBeforeStop(ctx, d, a, warn)
	wg.Wait()
	return cap
}

// persistSuspendScreen updates the stored screen to match this suspend:
// upserts when cap != nil, deletes any older row when cap == nil. Best-effort,
// detached from request cancellation with a short timeout and panic recovery;
// logs at Warn on failure and never returns an error or logs screen content.
func (s *Server) persistSuspendScreen(ctx context.Context, agentID string, cap *capturedScreen) {
	if s == nil || s.store == nil || agentID == "" || !s.experimentEnabled(experiments.SuspendScreen) {
		return
	}
	_ = runBestEffortCleanupStep(ctx, suspendScreenWriteTimeout, s.lifecycleLogger(),
		"persist_suspend_screen", "Recovered panic persisting pre-suspend terminal screen",
		[]any{"agent_id", agentID},
		func(wctx context.Context) error {
			if cap != nil {
				if err := s.store.PutAgentSuspendScreen(wctx, &store.AgentSuspendScreen{
					AgentID:    agentID,
					Content:    cap.Content,
					Truncated:  cap.Truncated,
					CapturedAt: cap.CapturedAt,
				}); err != nil {
					slog.Warn("Persisting pre-suspend terminal screen failed", "agent_id", agentID, "error", err)
				}
				return nil
			}
			if err := s.store.DeleteAgentSuspendScreen(wctx, agentID); err != nil {
				slog.Warn("Clearing stale pre-suspend terminal screen failed", "agent_id", agentID, "error", err)
			}
			return nil
		})
}

// agentScreenResponse is the JSON response for GET .../agents/{id}/screen.
type agentScreenResponse struct {
	AgentID    string    `json:"agentId"`
	Content    string    `json:"content"`
	CapturedAt time.Time `json:"capturedAt"`
	Truncated  bool      `json:"truncated"`
	Phase      string    `json:"phase"`
}

// handleAgentScreen handles GET /api/v1/agents/{id}/screen.
func (s *Server) handleAgentScreen(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.experimentEnabled(experiments.SuspendScreen) {
		NotFound(w, "route")
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	targetAgent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if denial := s.authorizeAgentTargetAction(ctx, identity, targetAgent, ActionAttach); denial != nil {
		writeAgentTargetDenial(w, r, identity, targetAgent, ActionAttach, denial)
		return
	}

	s.serveAgentScreen(w, r, targetAgent)
}

// handleProjectAgentScreen handles GET /api/v1/projects/{projectId}/agents/{id}/screen.
func (s *Server) handleProjectAgentScreen(w http.ResponseWriter, r *http.Request, projectID, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.experimentEnabled(experiments.SuspendScreen) {
		NotFound(w, "route")
		return
	}

	resolvedAgent, err := s.resolveProjectAgent(r.Context(), projectID, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeAgentLifecycle(w, r, resolvedAgent, ActionAttach) {
		return
	}

	s.serveAgentScreen(w, r, resolvedAgent)
}

func (s *Server) serveAgentScreen(w http.ResponseWriter, r *http.Request, targetAgent *store.Agent) {
	if isManagedAgentRuntime(targetAgent.Runtime) {
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "No saved screen for this agent", nil)
		return
	}

	screen, err := s.store.GetAgentSuspendScreen(r.Context(), targetAgent.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "No saved screen for this agent", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, agentScreenResponse{
		AgentID:    targetAgent.ID,
		Content:    screen.Content,
		CapturedAt: screen.CapturedAt,
		Truncated:  screen.Truncated,
		Phase:      targetAgent.Phase,
	})
}
