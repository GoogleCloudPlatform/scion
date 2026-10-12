/*
Copyright 2025 The Scion Authors.
*/

// Package handlers provides hook handler implementations.
//
// Design references in this package (section N, Dn) are to
// .design/hosted/usage-telemetry.md (ptone/scion#2053).
package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentinfolock"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// agentInfoMaxBytes bounds readAgentInfoMap's read. A legitimate
// agent-info.json is a small, flat JSON object; 1 MiB is generous headroom
// with no legitimate case anywhere near it — see readAgentInfoMap's own
// doc comment for why an unbounded read here is unsafe.
const agentInfoMaxBytes = 1 << 20

// StatusHandler manages agent status in a JSON file.
//
// Every write is a read-modify-write of the whole file. h.mu orders writes
// inside one process; the agent-info lock (pkg/agentinfolock) orders them
// across processes: init (PID 1), each `sciontool hook` and `sciontool
// status`, and the host broker on bind-mount runtimes (#4681).
type StatusHandler struct {
	// StatusPath is the path to the agent-info.json file.
	StatusPath string

	// lockOpts tunes the agent-info lock. Tests shorten the wait.
	lockOpts agentinfolock.Options

	mu sync.Mutex
}

// update runs one locked read-modify-write: it reads the map, lets mutate
// change it, and writes it back unless mutate returns false.
func (h *StatusHandler) update(mutate func(info map[string]interface{}) bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	opts := h.lockOpts
	if opts.Logf == nil {
		opts.Logf = log.Debug
	}
	if opts.Warnf == nil {
		opts.Warnf = log.Warn
	}
	return agentinfolock.Update(h.StatusPath, opts, func() error {
		info := h.readAgentInfoMap()
		if !mutate(info) {
			return nil
		}
		return h.writeAgentInfoLocked(info)
	})
}

// NewStatusHandler creates a new status handler.
func NewStatusHandler() *StatusHandler {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}
	return &StatusHandler{
		StatusPath: filepath.Join(home, "agent-info.json"),
	}
}

// activitySourceKey is the agent-info.json field that records which path set
// the current activity. Only the Notification path writes it, with the value
// activitySourceNotification; every other activity or phase write removes it.
// That lets tool-end and agent-end clear a waiting_for_input that a harness
// prompt (permission or elicitation dialog) set, once the prompt is answered,
// while a waiting_for_input the agent set itself (sciontool status ask_user)
// stays sticky. The ask_user command usually runs inside a harness shell
// tool, so a tool-end follows it at once and must not clear it.
const activitySourceKey = "activitySource"

// activitySourceNotification marks an activity set by a Notification event.
const activitySourceNotification = "notification"

// isStickyActivity returns true if the given activity is a "sticky" value that
// should resist being overwritten by normal event-driven updates.
func isStickyActivity(activity string) bool {
	return state.Activity(activity).IsSticky()
}

// eventResult holds the result of mapping an event to phase/activity.
type eventResult struct {
	phase    state.Phase
	activity state.Activity
	isPhase  bool // true if this is a phase-level change
}

// Handle processes an event and updates the agent status.
func (h *StatusHandler) Handle(event *hooks.Event) error {
	result := eventToPhaseActivity(event)
	if result == nil {
		return nil // Event doesn't trigger a state change
	}

	// Phase-level changes (pre-start, post-start, pre-stop, session-end)
	if result.isPhase {
		return h.UpdatePhase(result.phase, result.activity, "")
	}

	// A sub-agent's new-work or tool-start event is activity inside the
	// parent's session, not the parent resuming. It follows the tool-end and
	// agent-end rule: a status the parent agent set and still holds (for
	// example an ask_user waiting_for_input, or completed) stays in place.
	if isHeldSubagentEvent(event) {
		return h.updateActivityIfNotSticky(result.activity, event.Data.ToolName, clearNotificationWaiting)
	}

	// New work events (prompt-submit, agent-start, session-start): always
	// update activity unconditionally — clears any sticky state.
	if isNewWorkEvent(event.Name) {
		return h.UpdateActivity(result.activity, "")
	}

	// Tool-start events require special handling for sticky states.
	if event.Name == hooks.EventToolStart {
		// Claude-specific: ExitPlanMode and AskUserQuestion set waiting_for_input (sticky).
		if isClaudeUserWaitTool(event) {
			// A sub-agent's question is a harness prompt inside the
			// parent's session, not a status the parent set. Record it as
			// notification-set, so the sub-agent's own tool-end (the user
			// answered) or later events clear it.
			if isSubagentEvent(event) {
				return h.setWaitingFromNotification()
			}
			return h.UpdateActivity(state.ActivityWaitingForInput, "")
		}

		// Tool-start clears waiting_for_input (user has responded) but
		// preserves completed (tools may fire after task_completed as wrap-up).
		return h.updateActivityIfNotSticky(result.activity, event.Data.ToolName, clearAnyWaiting)
	}

	// Notification event: set waiting_for_input directly (sticky), marked as
	// set by a notification so tool-end or agent-end can clear it.
	if event.Name == hooks.EventNotification {
		return h.setWaitingFromNotification()
	}

	// Tool-end and agent-end: the harness prompt that a Notification reported
	// has been answered (the tool ran) or the turn is over (no prompt can be
	// open at turn end), so a notification-set waiting_for_input is cleared.
	// Every other sticky activity, including an agent-set waiting_for_input,
	// is kept.
	if event.Name == hooks.EventToolEnd || event.Name == hooks.EventAgentEnd {
		return h.updateActivityIfNotSticky(result.activity, "", clearNotificationWaiting)
	}

	// All other events (model-end, etc.): update activity only if current
	// activity is not sticky.
	return h.updateActivityIfNotSticky(result.activity, "", clearNone)
}

// setWaitingFromNotification sets waiting_for_input and marks it as set by a
// Notification event.
func (h *StatusHandler) setWaitingFromNotification() error {
	return h.update(func(info map[string]interface{}) bool {
		info["activity"] = string(state.ActivityWaitingForInput)
		info[activitySourceKey] = activitySourceNotification
		delete(info, "toolName")
		delete(info, "status")
		delete(info, "sessionStatus")
		return true
	})
}

// UpdateActivity writes the activity to the agent-info.json file atomically.
// Used for runtime activity changes (most events). Preserves the current phase.
func (h *StatusHandler) UpdateActivity(activity state.Activity, toolName string) error {
	return h.update(func(info map[string]interface{}) bool {
		applyActivity(info, activity, toolName)
		return true
	})
}

// applyActivity sets or clears activity and toolName, and removes the
// notification marker and legacy fields.
func applyActivity(info map[string]interface{}, activity state.Activity, toolName string) {
	if activity == "" {
		delete(info, "activity")
	} else {
		info["activity"] = string(activity)
	}

	// Set or clear toolName
	if activity == state.ActivityExecuting && toolName != "" {
		info["toolName"] = toolName
	} else {
		delete(info, "toolName")
	}

	// This write did not come from a Notification.
	delete(info, activitySourceKey)

	// Remove legacy fields if present
	delete(info, "status")
	delete(info, "sessionStatus")
}

// applyMessage sets detail.message, or removes it when message is empty.
func applyMessage(info map[string]interface{}, message string) {
	detail, _ := info["detail"].(map[string]interface{})
	if detail == nil {
		detail = make(map[string]interface{})
	}
	if message != "" {
		detail["message"] = message
	} else {
		delete(detail, "message")
	}
	if len(detail) > 0 {
		info["detail"] = detail
	} else {
		delete(info, "detail")
	}
}

// UpdatePhase writes both phase and activity to the agent-info.json file atomically.
// Used for lifecycle phase changes (pre-start, post-start, pre-stop, session-end).
func (h *StatusHandler) UpdatePhase(phase state.Phase, activity state.Activity, toolName string) error {
	return h.update(func(info map[string]interface{}) bool {
		info["phase"] = string(phase)
		applyActivity(info, activity, toolName)
		return true
	})
}

// UpdatePhaseAndMessage writes phase, activity and detail.message in one
// locked read-modify-write, so no other writer can land between the phase
// and its message (#4681). An empty message removes detail.message.
func (h *StatusHandler) UpdatePhaseAndMessage(phase state.Phase, activity state.Activity, message string) error {
	return h.update(func(info map[string]interface{}) bool {
		info["phase"] = string(phase)
		applyActivity(info, activity, "")
		applyMessage(info, message)
		return true
	})
}

// ReplaceActivityIf sets the activity to `to` only when the current activity
// is `from`, and reports whether it did. The phase is left unchanged. It
// lets `sciontool init` end the initializing activity without overwriting an
// activity a hook has already written. The check and the write run under
// the agent-info lock, so a hook in another process cannot land between
// them while the lock is held (it is best effort; see pkg/agentinfolock).
func (h *StatusHandler) ReplaceActivityIf(from, to state.Activity) (bool, error) {
	replaced := false
	err := h.update(func(info map[string]interface{}) bool {
		current, _ := info["activity"].(string)
		if current != string(from) {
			return false
		}
		info["activity"] = string(to)
		delete(info, "toolName")
		delete(info, "status")
		delete(info, "sessionStatus")
		replaced = true
		return true
	})
	if err != nil {
		return false, err
	}
	return replaced, nil
}

// CurrentActivity returns the activity recorded in agent-info.json, or ""
// when the file is missing or unreadable.
func (h *StatusHandler) CurrentActivity() state.Activity {
	h.mu.Lock()
	defer h.mu.Unlock()
	current, _ := h.readAgentInfoMap()["activity"].(string)
	return state.Activity(current)
}

// SetMessage writes a message to the agent-info.json detail section.
// This is used to persist error messages so the broker heartbeat can
// relay them to the hub even after the container exits.
func (h *StatusHandler) SetMessage(message string) error {
	return h.update(func(info map[string]interface{}) bool {
		applyMessage(info, message)
		return true
	})
}

// waitingClear selects which waiting_for_input values an event may clear.
type waitingClear int

const (
	// clearNone keeps every sticky activity.
	clearNone waitingClear = iota
	// clearNotificationWaiting clears waiting_for_input only when a
	// Notification set it (tool-end, agent-end).
	clearNotificationWaiting
	// clearAnyWaiting clears any waiting_for_input (tool-start: the user has
	// responded and the agent is running a tool).
	clearAnyWaiting
)

// stickyBlocksUpdate reports whether the activity recorded in info is sticky
// and must not be overwritten by an event with the given clear rule. Only
// waiting_for_input can be cleared; completed, blocked, limits_exceeded and
// crashed always block.
func stickyBlocksUpdate(info map[string]interface{}, clear waitingClear) bool {
	currentActivity, _ := info["activity"].(string)
	if !isStickyActivity(currentActivity) {
		return false
	}
	if currentActivity != string(state.ActivityWaitingForInput) {
		return true
	}
	switch clear {
	case clearAnyWaiting:
		return false
	case clearNotificationWaiting:
		source, _ := info[activitySourceKey].(string)
		return source != activitySourceNotification
	default:
		return true
	}
}

// updateActivityIfNotSticky updates the activity only if the current activity
// is not sticky, or is a waiting_for_input that clear allows this event to
// clear.
func (h *StatusHandler) updateActivityIfNotSticky(activity state.Activity, toolName string, clear waitingClear) error {
	return h.update(func(info map[string]interface{}) bool {
		if stickyBlocksUpdate(info, clear) {
			return false // Activity is sticky, don't overwrite
		}
		// activity is never empty here, so applyActivity always sets it.
		applyActivity(info, activity, toolName)
		return true
	})
}

// readAgentInfoMap reads agent-info.json into a generic map, preserving all
// fields. Caller must hold h.mu (and, via update, the agent-info lock).
//
// This handler is registered in the root PID-1 init process and is called
// throughout the workload's lifetime (post-start, pre-stop, session-end,
// limits-exceeded, auth-reset), while $HOME — which StatusPath lives in —
// is owned outright by the workload. The workload can unlink and replace
// agent-info.json with anything: a symlink, a FIFO, or an oversized file.
// dirfd.ReadFileNoFollow refuses (rather than follows) a symlink at any
// component, requires a single-link regular file, and bounds the read, so
// none of those substitutions can make root either read arbitrary content
// or block. In particular a FIFO with no writer must never hang this call:
// this runs in root's own PID-1 process, and every lifecycle update that
// depends on it (including the shutdown/limits paths) would stall with it.
// Any refusal is logged and treated as "no prior state" — the same
// non-fatal, empty-map fallback a missing file already produced — never a
// crash or a propagated error.
func (h *StatusHandler) readAgentInfoMap() map[string]interface{} {
	info := make(map[string]interface{})
	data, err := dirfd.ReadFileNoFollow(h.StatusPath, agentInfoMaxBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Error("Refusing to read %s: %v", h.StatusPath, err)
		}
		return info
	}
	_ = json.Unmarshal(data, &info)
	return info
}

// writeAgentInfoLocked writes the agent info map to disk atomically.
// Caller must hold h.mu.
//
// dirfd.WriteFileNoFollow creates the temp file in StatusPath's parent
// directory via that directory's own no-follow fd, sets its mode via fchmod
// on the open fd, and renames it into place with a single fd-relative
// rename — never a path-based os.Chmod, which a symlink swapped into the
// temp file's directory entry between create and chmod (something the
// workload can always do, since it owns the containing directory) could
// otherwise redirect onto an arbitrary target's permissions. dirfd.ReplaceLeaf
// is the right policy here, not dirfd.RefuseSymlink: StatusPath lives inside
// a directory the workload owns outright, so a symlink already there is a
// stale leaf to overwrite on this write, exactly like every other write —
// refusing it would buy nothing, since the read side (readAgentInfoMap,
// above) already refuses to follow it regardless of what this write does.
func (h *StatusHandler) writeAgentInfoLocked(info map[string]interface{}) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling status: %w", err)
	}

	// Widen to 0644 (CreateExclAt's own default is 0600) so the broker
	// process — which may run as a different uid than the container init —
	// can read and converge the file after the container exits.
	if err := dirfd.WriteFileNoFollow(h.StatusPath, data, 0644, 0, 0, dirfd.ReplaceLeaf); err != nil {
		return fmt.Errorf("writing %s: %w", h.StatusPath, err)
	}
	return nil
}

// isNewWorkEvent returns true for events that indicate new work is starting.
// These events unconditionally update activity, clearing any sticky state.
func isNewWorkEvent(name string) bool {
	switch name {
	case hooks.EventPromptSubmit, hooks.EventAgentStart, hooks.EventSessionStart:
		return true
	}
	return false
}

// isSubagentEvent reports whether a codex or claude hook event came from a
// sub-agent rather than the parent agent. Both harnesses send the parent
// session's session_id on a sub-agent's hook events and name the child only
// in agent_id. An event whose agent_id is set and differs from session_id is
// therefore a sub-agent's. Other dialects are not checked, so their handling
// is unchanged.
func isSubagentEvent(event *hooks.Event) bool {
	if event.Dialect != "codex" && event.Dialect != "claude" {
		return false
	}
	agentID, _ := event.Data.Raw["agent_id"].(string)
	return agentID != "" && agentID != event.Data.SessionID
}

// isClaudeUserWaitTool reports whether a claude tool-start is for a tool that
// asks the user (ExitPlanMode, AskUserQuestion) and so sets waiting_for_input.
func isClaudeUserWaitTool(event *hooks.Event) bool {
	return event.Name == hooks.EventToolStart && event.Dialect == "claude" &&
		(event.Data.ToolName == "ExitPlanMode" || event.Data.ToolName == "AskUserQuestion")
}

// isHeldSubagentEvent reports whether an event is a sub-agent's new-work or
// tool-start event that must not clear a status the parent agent holds. A
// sub-agent's ExitPlanMode or AskUserQuestion tool-start is excluded: the
// user is being asked, so it still sets waiting_for_input (recorded as
// notification-set, see Handle).
func isHeldSubagentEvent(event *hooks.Event) bool {
	if !isSubagentEvent(event) || isClaudeUserWaitTool(event) {
		return false
	}
	return isNewWorkEvent(event.Name) || event.Name == hooks.EventToolStart
}

// eventToPhaseActivity maps normalized events to phase/activity pairs.
// Returns nil if the event doesn't trigger a state change.
func eventToPhaseActivity(event *hooks.Event) *eventResult {
	switch event.Name {
	case hooks.EventPreStart:
		return &eventResult{phase: state.PhaseStarting, isPhase: true}

	case hooks.EventPostStart:
		// The harness process is up but not yet known to read input;
		// `sciontool init` replaces initializing with working once the
		// harness is ready (or at state.ReadyDeadline). Any hook event
		// replaces it too, since initializing is not sticky.
		return &eventResult{phase: state.PhaseRunning, activity: state.ActivityInitializing, isPhase: true}

	case hooks.EventSessionStart:
		// session-start clears sticky — treated as activity-only (working)
		return &eventResult{activity: state.ActivityWorking}

	case hooks.EventPromptSubmit, hooks.EventAgentStart:
		return &eventResult{activity: state.ActivityThinking}

	case hooks.EventModelStart:
		return &eventResult{activity: state.ActivityThinking}

	case hooks.EventModelEnd:
		return &eventResult{activity: state.ActivityWorking}

	case hooks.EventToolStart:
		return &eventResult{activity: state.ActivityExecuting}

	case hooks.EventToolEnd, hooks.EventAgentEnd:
		return &eventResult{activity: state.ActivityWorking}

	case hooks.EventResponseComplete:
		return &eventResult{activity: state.ActivityCompleted}

	case hooks.EventNotification:
		return &eventResult{activity: state.ActivityWaitingForInput}

	case hooks.EventPreStop:
		return &eventResult{phase: state.PhaseStopping, isPhase: true}

	case hooks.EventSessionEnd:
		return &eventResult{phase: state.PhaseStopped, isPhase: true}

	default:
		return nil
	}
}

// GetFormattedState returns the state with tool name if applicable.
func (h *StatusHandler) GetFormattedState(event *hooks.Event) string {
	result := eventToPhaseActivity(event)
	if result == nil {
		return ""
	}
	if result.activity == state.ActivityExecuting && event.Data.ToolName != "" {
		return fmt.Sprintf("%s (%s)", result.activity, event.Data.ToolName)
	}
	if result.isPhase {
		return string(result.phase)
	}
	return string(result.activity)
}
