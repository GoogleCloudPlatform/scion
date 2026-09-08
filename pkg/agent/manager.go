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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type Manager interface {
	// Provision prepares the agent directory and configuration without starting it
	Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error)

	// Start launches a new agent with the given configuration
	Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error)

	// Stop terminates an agent
	Stop(ctx context.Context, agentID string, projectPath string) error

	// Delete terminates and removes an agent
	Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error)

	// List returns active agents
	List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error)

	// Message sends a message to an agent's harness via tmux.
	// projectID scopes delivery to a specific project, preventing cross-project
	// collision when agents share the same slug.
	Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error

	// MessageRaw sends literal bytes to an agent's tmux session via send-keys
	// with no trailing Enter keypresses, allowing control sequences like
	// arrow keys and Escape to be used directly.
	// projectID scopes delivery to a specific project.
	MessageRaw(ctx context.Context, agentID, projectID string, keys string) error

	// Watch returns a channel of status updates for an agent
	Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error)

	// Close flushes any pending message buffers. Must be called before
	// the process exits to ensure buffered messages are delivered.
	Close()
}

type AgentManager struct {
	Runtime   runtime.Runtime
	msgBuffer *MessageBuffer
}

// defaultBufferDelay is the debounce window for message delivery.
// Messages arriving within this window are coalesced into a single delivery.
const defaultBufferDelay = 2 * time.Second

func NewManager(rt runtime.Runtime) Manager {
	mgr := &AgentManager{
		Runtime: rt,
	}
	// Initialize the message buffer with a debounce delay. The buffer's
	// delivery function calls back into deliverImmediate to perform the
	// actual tmux send-keys when the debounce window expires.
	mgr.msgBuffer = NewMessageBuffer(defaultBufferDelay, func(agentID, projectID string, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	return mgr
}

func (m *AgentManager) Close() {
	m.msgBuffer.Close()
}

func (m *AgentManager) Stop(ctx context.Context, agentID string, projectPath string) error {
	// Resolve the agent name to a container ID so that runtimes which do
	// not support lookup-by-name (e.g. Apple's `container` CLI) receive
	// the actual container ID.  This mirrors the resolution logic in Delete().
	slug := api.Slugify(agentID)
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	// Resolve project name from projectPath (if provided) to scope the container lookup
	stopProjectName := ""
	if projectPath != "" {
		if resolvedDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
			stopProjectName = config.GetProjectName(resolvedDir)
		}
	}
	if err == nil {
		for _, a := range agents {
			if a.Name == agentID || a.ContainerID == agentID ||
				strings.TrimPrefix(a.Name, "/") == agentID ||
				strings.EqualFold(a.Name, agentID) {
				// If project info is available, skip containers from a different project
				if stopProjectName != "" && !matchAgentProject(a, stopProjectName, "") {
					continue
				}
				return m.Runtime.Stop(ctx, a.ContainerID)
			}
		}
	}
	// Fallback: agentID may already be a container ID, or the list
	// failed — pass it through directly.
	return m.Runtime.Stop(ctx, agentID)
}

func (m *AgentManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	// 1. Check if container exists
	// We use name filter if possible, but runtime.List might take map[string]string
	util.Debugf("delete: listing containers in mgr.Delete for %s", agentID)
	listStart := time.Now()
	slug := api.Slugify(agentID)
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	util.Debugf("delete: mgr.Delete container list completed in %v", time.Since(listStart))
	containerExists := false
	var targetID string
	// Resolve project name from projectPath (if provided) to scope the container lookup
	deletionProjectName := ""
	if projectPath != "" {
		if resolvedDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
			deletionProjectName = config.GetProjectName(resolvedDir)
		}
	}
	if err == nil {
		for _, a := range agents {
			if a.Name == agentID || a.ContainerID == agentID || strings.TrimPrefix(a.Name, "/") == agentID || strings.EqualFold(a.Name, agentID) {
				// If project info is available, skip containers from a different project
				if deletionProjectName != "" && !matchAgentProject(a, deletionProjectName, "") {
					continue
				}
				containerExists = true
				targetID = a.ContainerID
				break
			}
		}
	}

	if containerExists {
		// Stop the container gracefully before force-removing it. This ensures
		// bind mounts (e.g. shared-dir volumes) are properly released before
		// filesystem cleanup. Without this, docker rm -f / container kill sends
		// SIGKILL which can leave mounts in a state that causes permission
		// errors when DeleteAgentFiles tries to remove the agent directory.
		util.Debugf("delete: stopping container %s before removal", targetID)
		if err := m.Runtime.Stop(ctx, targetID); err != nil {
			// Log but don't fail — the container may already be stopped,
			// and Delete (force-remove) will handle it either way.
			util.Debugf("delete: stop returned error (continuing): %v", err)
		}

		util.Debugf("delete: starting runtime delete for container %s", targetID)
		if err := m.Runtime.Delete(ctx, targetID); err != nil {
			return false, fmt.Errorf("failed to delete container: %w", err)
		}
		util.Debugf("delete: runtime delete completed for container %s", targetID)
	}

	if deleteFiles {
		util.Debugf("delete: starting filesystem cleanup for agent %s", agentID)
		branchDeleted, err := DeleteAgentFiles(agentID, projectPath, removeBranch)
		util.Debugf("delete: filesystem cleanup completed for agent %s", agentID)
		return branchDeleted, err
	}
	return false, nil
}

func (m *AgentManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, fmt.Errorf("Watch not implemented")
}

func (m *AgentManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	// Interrupt messages bypass the buffer entirely — they need to send
	// Ctrl+C immediately to get the agent's attention, and the accompanying
	// message (if any) should follow without delay.
	if interrupt {
		return m.deliverImmediate(ctx, agentID, projectID, message, interrupt)
	}

	// Non-interrupt messages go through the debounce buffer. This ensures
	// that a rapid burst of messages (e.g. from multiple senders or broadcast
	// fan-out) is coalesced into a single delivery, avoiding contention on
	// the agent's tmux input.
	m.msgBuffer.Send(agentID, projectID, message)
	return nil
}

// exitCopyModeCmd returns the tmux command that leaves copy-mode. Real keys
// (interrupts, raw sequences) are dispatched through the mode's key table
// rather than to the harness, so those paths must cancel the mode first.
// -q is a no-op when the pane is not in a mode.
func exitCopyModeCmd() []string {
	return []string{"tmux", "copy-mode", "-q", "-t", "scion:0"}
}

// isExitCopyModeCmd reports whether cmd is the copy-mode exit, whose failure
// is tolerated.
func isExitCopyModeCmd(cmd []string) bool {
	return len(cmd) > 1 && cmd[1] == "copy-mode"
}

// submitCmds returns the commands that submit whatever is in the harness's
// input, without disturbing copy-mode: paste-buffer writes to the pane's pty
// and bypasses the mode key table, so the operator's scroll position survives.
//
// A literal CR rather than LF, so this does not depend on paste-buffer's
// LF->CR default. -d consumes the buffer as it is pasted: a named buffer still
// sits on top of the operator's buffer stack, so without it their paste key
// would fetch this CR instead of the text they copied.
func submitCmds() [][]string {
	return [][]string{
		{"tmux", "set-buffer", "-b", "scion-submit", "--", "\r"},
		{"tmux", "paste-buffer", "-d", "-b", "scion-submit", "-t", "scion:0"},
	}
}

// MessageRaw sends literal bytes to an agent's tmux session via send-keys
// with no trailing Enter keypresses. This bypasses the paste buffer and
// debounce buffer, sending directly via tmux send-keys so that control
// sequences (arrow keys, Escape, etc.) are interpreted by the terminal.
// Leaves copy-mode first, since send-keys is dispatched through the mode's
// key table rather than to the harness.
func (m *AgentManager) MessageRaw(ctx context.Context, agentID, projectID string, keys string) error {
	filter := map[string]string{"scion.name": strings.ToLower(agentID)}
	if projectID != "" {
		filter["scion.project_id"] = projectID
	}
	agents, err := m.List(ctx, filter)
	if err != nil {
		return err
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if matchesAgentID(a, agentID) {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' not found or not running", agentID)
	}

	cmds := [][]string{
		exitCopyModeCmd(),
		{"tmux", "send-keys", "-t", "scion:0", "--", keys},
	}
	for _, cmd := range cmds {
		if _, err := m.Runtime.Exec(ctx, agent.ContainerID, cmd); err != nil {
			if isExitCopyModeCmd(cmd) {
				continue
			}
			return fmt.Errorf("failed to send raw keys to agent '%s': %w", agent.Name, err)
		}
	}

	return nil
}

// deliverImmediate sends a message to an agent's tmux session right now,
// bypassing the message buffer. This is the low-level delivery mechanism
// used both for interrupt messages (called directly) and for buffered
// messages (called by the MessageBuffer when the debounce timer fires).
func (m *AgentManager) deliverImmediate(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	// 1. Find the agent, scoped to project to prevent cross-project delivery
	filter := map[string]string{"scion.name": strings.ToLower(agentID)}
	if projectID != "" {
		filter["scion.project_id"] = projectID
	}
	agents, err := m.List(ctx, filter)
	if err != nil {
		return err
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if matchesAgentID(a, agentID) {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' not found or not running", agentID)
	}

	// 2. Resolve harness — probe both layouts (worktree vs shared-workspace
	// per .design/hub-shared-workspace-isolation.md) since the mode isn't
	// passed through this lookup path.
	harnessName := "generic"
	if agent.ProjectPath != "" {
		projectDir, _ := config.GetResolvedProjectDir(agent.ProjectPath)
		if projectDir == "" {
			projectDir = agent.ProjectPath
		}
		scionJSON := filepath.Join(config.ResolveAgentDir(projectDir, agent.Name), "scion-agent.json")
		if data, err := os.ReadFile(scionJSON); err == nil {
			var cfg api.ScionConfig
			if err := json.Unmarshal(data, &cfg); err == nil && cfg.Harness != "" {
				harnessName = cfg.Harness
			}
		}
	}
	h := harness.New(harnessName)

	// 3. Prepare commands
	var cmds [][]string

	// Only the paths that send real KEYS need the mode cancelled; the message
	// path submits via paste instead and leaves a reading operator alone.
	if interrupt || message == "" {
		cmds = append(cmds, exitCopyModeCmd())
	}

	if interrupt {
		if seq := h.GetInterruptSequence(); len(seq) > 0 {
			for _, key := range seq {
				cmds = append(cmds, []string{"tmux", "send-keys", "-t", "scion:0", key})
			}
		} else {
			key := h.GetInterruptKey()
			cmds = append(cmds, []string{"tmux", "send-keys", "-t", "scion:0", key})
		}
	}

	if message == "" {
		// Empty messages send a bare Enter keypress to trigger confirmations
		cmds = append(cmds, []string{"tmux", "send-keys", "-t", "scion:0", "Enter"})
	} else {
		// Use tmux paste buffer with bracketed paste (-p) instead of send-keys.
		// send-keys simulates typing character-by-character, which allows TUI
		// applications to intercept special characters as hotkeys (e.g., Gemini
		// CLI treats '!' as a shell-mode toggle). Bracketed paste wraps the
		// content in escape sequences (\e[200~...\e[201~) that signal the
		// application to treat all characters as literal pasted text.
		// Named buffer: an unnamed set-buffer pushes onto the operator's
		// buffer stack, so their paste key would fetch the agent's message.
		cmds = append(cmds, []string{"tmux", "set-buffer", "-b", "scion-msg", "--", message})
		cmds = append(cmds, []string{"tmux", "paste-buffer", "-d", "-b", "scion-msg", "-t", "scion:0", "-p"})
		cmds = append(cmds, submitCmds()...)
	}

	// 4. Execute
	for _, cmd := range cmds {
		_, err := m.Runtime.Exec(ctx, agent.ContainerID, cmd)
		if err != nil {
			if isExitCopyModeCmd(cmd) {
				// tmux before 3.1 has no -q. Leaving the mode is best-effort:
				// failing the delivery over it would lose a message that older
				// tmux would otherwise have taken.
				continue
			}
			return fmt.Errorf("failed to send message to agent '%s': %w", agent.Name, err)
		}
	}

	// After sending a message, send two extra Enter keypresses with a brief delay
	// to ensure the input is accepted by the agent.
	if message != "" {
		for range 2 {
			time.Sleep(300 * time.Millisecond)
			for _, cmd := range submitCmds() {
				if _, err := m.Runtime.Exec(ctx, agent.ContainerID, cmd); err != nil {
					return fmt.Errorf("failed to send Enter to agent '%s': %w", agent.Name, err)
				}
			}
		}
	}

	return nil
}

func matchesAgentID(a api.AgentInfo, id string) bool {
	return a.Name == id || a.ContainerID == id || strings.TrimPrefix(a.Name, "/") == id
}
