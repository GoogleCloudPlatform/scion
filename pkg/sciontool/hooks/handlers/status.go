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
type StatusHandler struct {
	// StatusPath is the path to the agent-info.json file.
	StatusPath string

	mu sync.Mutex
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

// notificationWaitAgentsKey is the agent-info.json field that lists the
// sub-agents (by agent_id) whose question holds a notification-set
// waiting_for_input. It exists only next to the activitySource marker: every
// write that is not a notification write removes both. While the list is not
// empty, a sub-agent event clears only its own entry, and the wait stays until
// the last listed sub-agent's question is answered. A sibling sub-agent and
// the parent's tool-end (for example the parent's Agent tool returning for
// another sub-agent) leave it in place. A wait with no list behaves as before:
// any tool-end or agent-end clears it.
const notificationWaitAgentsKey = "notificationWaitAgents"

// maxNotificationWaitAgents caps notificationWaitAgentsKey. Past the cap, a
// sub-agent's question still sets the wait but its agent_id is not recorded.
const maxNotificationWaitAgents = 64

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
	// A sub-agent that ends no longer holds a question open. Its entry is
	// removed from the notification wait's owner list; the activity is left
	// alone, so the parent's tool-end for that sub-agent then clears the wait
	// if no other sub-agent still holds it.
	if event.Name == hooks.EventSubagentEnd && isSubagentEvent(event) {
		return h.releaseNotificationWaitOwner(subagentID(event))
	}

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
		return h.updateActivityIfNotSticky(result.activity, event.Data.ToolName, clearNotificationWaiting, subagentID(event))
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
			// notification-set and owned by the sub-agent, so the
			// sub-agent's own tool-end (the user answered) clears it.
			if isSubagentEvent(event) {
				return h.setWaitingFromNotificationFor(subagentID(event))
			}
			return h.UpdateActivity(state.ActivityWaitingForInput, "")
		}

		// Tool-start clears waiting_for_input (user has responded) but
		// preserves completed (tools may fire after task_completed as wrap-up).
		return h.updateActivityIfNotSticky(result.activity, event.Data.ToolName, clearAnyWaiting, "")
	}

	// Notification event: set waiting_for_input directly (sticky), marked as
	// set by a notification so tool-end or agent-end can clear it. A
	// sub-agent's Notification is owned by that sub-agent.
	if event.Name == hooks.EventNotification {
		return h.setWaitingFromNotificationFor(subagentID(event))
	}

	// Tool-end and agent-end: the harness prompt that a Notification reported
	// has been answered (the tool ran) or the turn is over (no prompt can be
	// open at turn end), so a notification-set waiting_for_input is cleared.
	// Every other sticky activity, including an agent-set waiting_for_input,
	// is kept. A wait that sub-agents hold is cleared only by their own
	// events, or by the parent's agent-end (the turn is over).
	if event.Name == hooks.EventToolEnd {
		return h.updateActivityIfNotSticky(result.activity, "", clearNotificationWaiting, subagentID(event))
	}
	if event.Name == hooks.EventAgentEnd {
		return h.updateActivityIfNotSticky(result.activity, "", clearTurnEndWaiting, subagentID(event))
	}

	// All other events (model-end, etc.): update activity only if current
	// activity is not sticky.
	return h.updateActivityIfNotSticky(result.activity, "", clearNone, "")
}

// setWaitingFromNotification sets waiting_for_input and marks it as set by a
// Notification event, with no owning sub-agent.
func (h *StatusHandler) setWaitingFromNotification() error {
	return h.setWaitingFromNotificationFor("")
}

// setWaitingFromNotificationFor sets waiting_for_input, marks it as set by a
// Notification event and, when owner (a sub-agent's agent_id) is not empty,
// adds owner to the wait's owner list. A waiting_for_input the agent set
// itself is left unchanged: a harness prompt must not turn it into a
// notification-set wait that the prompt's answer would then clear.
func (h *StatusHandler) setWaitingFromNotificationFor(owner string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()
	if isAgentSetWaiting(info) {
		return nil
	}

	var owners []string
	if isNotificationWaiting(info) {
		owners = notificationWaitOwners(info)
	}
	if owner != "" && !containsString(owners, owner) && len(owners) < maxNotificationWaitAgents {
		owners = append(owners, owner)
	}

	info["activity"] = string(state.ActivityWaitingForInput)
	info[activitySourceKey] = activitySourceNotification
	setNotificationWaitOwners(info, owners)
	delete(info, "toolName")
	delete(info, "status")
	delete(info, "sessionStatus")

	return h.writeAgentInfoLocked(info)
}

// releaseNotificationWaitOwner removes owner from the owner list of a
// notification-set waiting_for_input. The activity is not changed. Nothing is
// written when owner is not listed.
func (h *StatusHandler) releaseNotificationWaitOwner(owner string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()
	if !isNotificationWaiting(info) {
		return nil
	}
	owners := notificationWaitOwners(info)
	remaining, removed := removeString(owners, owner)
	if !removed {
		return nil
	}
	setNotificationWaitOwners(info, remaining)
	return h.writeAgentInfoLocked(info)
}

// isNotificationWaiting reports whether info holds a waiting_for_input that a
// Notification (or a sub-agent's question) set.
func isNotificationWaiting(info map[string]interface{}) bool {
	activity, _ := info["activity"].(string)
	source, _ := info[activitySourceKey].(string)
	return activity == string(state.ActivityWaitingForInput) && source == activitySourceNotification
}

// isAgentSetWaiting reports whether info holds a waiting_for_input that the
// agent set itself (sciontool status ask_user, or the parent's own
// ExitPlanMode or AskUserQuestion).
func isAgentSetWaiting(info map[string]interface{}) bool {
	activity, _ := info["activity"].(string)
	source, _ := info[activitySourceKey].(string)
	return activity == string(state.ActivityWaitingForInput) && source != activitySourceNotification
}

// notificationWaitOwners returns the string entries of the owner list.
func notificationWaitOwners(info map[string]interface{}) []string {
	list, _ := info[notificationWaitAgentsKey].([]interface{})
	var owners []string
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			owners = append(owners, s)
		}
	}
	return owners
}

// setNotificationWaitOwners writes the owner list, or removes the field when
// the list is empty.
func setNotificationWaitOwners(info map[string]interface{}, owners []string) {
	if len(owners) == 0 {
		delete(info, notificationWaitAgentsKey)
		return
	}
	list := make([]interface{}, len(owners))
	for i, o := range owners {
		list[i] = o
	}
	info[notificationWaitAgentsKey] = list
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// removeString returns list without s and whether s was present.
func removeString(list []string, s string) ([]string, bool) {
	out := make([]string, 0, len(list))
	removed := false
	for _, v := range list {
		if v == s {
			removed = true
			continue
		}
		out = append(out, v)
	}
	return out, removed
}

// UpdateActivity writes the activity to the agent-info.json file atomically.
// Used for runtime activity changes (most events). Preserves the current phase.
func (h *StatusHandler) UpdateActivity(activity state.Activity, toolName string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()

	// Update the activity field
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
	delete(info, notificationWaitAgentsKey)

	// Remove legacy fields if present
	delete(info, "status")
	delete(info, "sessionStatus")

	return h.writeAgentInfoLocked(info)
}

// UpdatePhase writes both phase and activity to the agent-info.json file atomically.
// Used for lifecycle phase changes (pre-start, post-start, pre-stop, session-end).
func (h *StatusHandler) UpdatePhase(phase state.Phase, activity state.Activity, toolName string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()

	// Update phase
	info["phase"] = string(phase)

	// Update activity
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
	delete(info, notificationWaitAgentsKey)

	// Remove legacy fields if present
	delete(info, "status")
	delete(info, "sessionStatus")

	return h.writeAgentInfoLocked(info)
}

// ReplaceActivityIf sets the activity to `to` only when the current activity
// is `from`, and reports whether it did. The phase is left unchanged. It
// lets `sciontool init` end the initializing activity without overwriting an
// activity a hook has already written. Hooks run in other processes, so the
// check and the write are not atomic across processes; the window is a
// single read-modify-write of a small local file.
func (h *StatusHandler) ReplaceActivityIf(from, to state.Activity) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()
	current, _ := info["activity"].(string)
	if current != string(from) {
		return false, nil
	}
	info["activity"] = string(to)
	delete(info, "toolName")
	delete(info, "status")
	delete(info, "sessionStatus")
	if err := h.writeAgentInfoLocked(info); err != nil {
		return false, err
	}
	return true, nil
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
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()

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

	return h.writeAgentInfoLocked(info)
}

// waitingClear selects which waiting_for_input values an event may clear.
type waitingClear int

const (
	// clearNone keeps every sticky activity.
	clearNone waitingClear = iota
	// clearNotificationWaiting clears waiting_for_input only when a
	// Notification set it (tool-end, and a sub-agent's tool-start or
	// prompt). A wait that sub-agents hold is cleared only once the last of
	// them has had its own event; the parent's tool-end keeps it.
	clearNotificationWaiting
	// clearTurnEndWaiting is clearNotificationWaiting for agent-end: the
	// parent's turn is over, so no prompt can still be open and a wait that
	// sub-agents hold is cleared too.
	clearTurnEndWaiting
	// clearAnyWaiting clears any waiting_for_input (tool-start: the user has
	// responded and the agent is running a tool).
	clearAnyWaiting
)

// stickyBlocksUpdate reports whether the activity recorded in info is sticky
// and must not be overwritten by an event with the given clear rule. Only
// waiting_for_input can be cleared; completed, blocked, limits_exceeded and
// crashed always block. subagent is the event's sub-agent agent_id, or ""
// for the parent's own event. When a sub-agent's event is blocked only
// because other sub-agents still hold the wait, its own entry is removed from
// info's owner list and released is true, so the caller writes info.
func stickyBlocksUpdate(info map[string]interface{}, clear waitingClear, subagent string) (blocks, released bool) {
	currentActivity, _ := info["activity"].(string)
	if !isStickyActivity(currentActivity) {
		return false, false
	}
	if currentActivity != string(state.ActivityWaitingForInput) {
		return true, false
	}
	switch clear {
	case clearAnyWaiting:
		return false, false
	case clearNotificationWaiting, clearTurnEndWaiting:
		if !isNotificationWaiting(info) {
			return true, false
		}
		owners := notificationWaitOwners(info)
		if len(owners) == 0 {
			return false, false
		}
		if subagent == "" {
			// The parent's own event: agent-end clears the wait, tool-end
			// keeps a wait that sub-agents hold.
			return clear != clearTurnEndWaiting, false
		}
		remaining, removed := removeString(owners, subagent)
		if !removed {
			return true, false // a sibling's question is still open
		}
		if len(remaining) == 0 {
			return false, false
		}
		setNotificationWaitOwners(info, remaining)
		return true, true
	default:
		return true, false
	}
}

// updateActivityIfNotSticky updates the activity only if the current activity
// is not sticky, or is a waiting_for_input that clear allows this event to
// clear. subagent is the event's sub-agent agent_id, or "" for the parent.
func (h *StatusHandler) updateActivityIfNotSticky(activity state.Activity, toolName string, clear waitingClear, subagent string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()

	if blocks, released := stickyBlocksUpdate(info, clear, subagent); blocks {
		if released {
			return h.writeAgentInfoLocked(info)
		}
		return nil // Activity is sticky, don't overwrite
	}

	// Update activity
	info["activity"] = string(activity)

	// Set or clear toolName
	if activity == state.ActivityExecuting && toolName != "" {
		info["toolName"] = toolName
	} else {
		delete(info, "toolName")
	}

	// This write did not come from a Notification.
	delete(info, activitySourceKey)
	delete(info, notificationWaitAgentsKey)

	// Remove legacy fields if present
	delete(info, "status")
	delete(info, "sessionStatus")

	return h.writeAgentInfoLocked(info)
}

// readAgentInfoMap reads agent-info.json into a generic map, preserving all
// fields. Caller must hold h.mu.
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

// subagentID returns the agent_id of a sub-agent's event, or "" for any other
// event.
func subagentID(event *hooks.Event) string {
	if !isSubagentEvent(event) {
		return ""
	}
	id, _ := event.Data.Raw["agent_id"].(string)
	return id
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
