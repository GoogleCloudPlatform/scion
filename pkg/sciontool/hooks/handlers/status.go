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
// change it, and writes it back unless mutate returns false. mutate runs
// with h.mu held, which is not re-entrant, so it must not call any locking
// method of h (that deadlocks) or start another update of the same file
// (that waits out the lock wait and then writes unlocked).
func (h *StatusHandler) update(mutate func(info map[string]interface{}) bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	opts := h.lockOpts
	// The workload owns $HOME: refuse a symlink anywhere in the lock
	// file's path, as readAgentInfoMap and writeAgentInfoLocked do.
	opts.NoFollowParent = true
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
// the current activity. The Notification path writes it with the value
// activitySourceNotification, and the release of a sub-agent-held wait on the
// last owner's SubagentStop writes activitySourceSubagentEnd; every other
// activity or phase write removes it.
// That lets tool-end and agent-end clear a waiting_for_input that a harness
// prompt (permission or elicitation dialog) set, once the prompt is answered,
// while a waiting_for_input the agent set itself (sciontool status ask_user)
// stays sticky. The ask_user command usually runs inside a harness shell
// tool, so a tool-end follows it at once and must not clear it.
const activitySourceKey = "activitySource"

// activitySourceNotification marks an activity set by a Notification event.
const activitySourceNotification = "notification"

// activitySourceSubagentEnd marks the working activity written when the last
// owner's SubagentStop released a sub-agent-held wait. Only the HubHandler
// reads it, to report that release (see HubHandler.Handle); every other
// reader of activitySourceKey checks it only next to waiting_for_input.
const activitySourceSubagentEnd = "subagent-end"

// notificationWaitAgentsKey is the agent-info.json field that lists the
// sub-agents (by agent_id) whose question holds a notification-set
// waiting_for_input. It exists only next to the activitySource marker: every
// write that is not a notification write removes both. While the list is not
// empty, a sub-agent event clears only its own entry, and the wait stays until
// the last listed sub-agent's question is answered or that sub-agent ends
// (its SubagentStop then clears the wait to working, or, when the parent's
// own wait is recorded in parentWaitKey, leaves that wait). A sibling
// sub-agent and the parent's own tool activity leave it in place: the
// parent's tool-start, its tool-end (for example its Agent tool returning for
// another sub-agent) and its Stop. A background sub-agent keeps running after
// the parent's turn ends, so its question outlives the parent's Stop. A wait
// with no list behaves as before: any tool-start, tool-end or agent-end
// clears it. The parent's Stop also drops the list when it reports that no
// background work is in flight (see reportsNoBackgroundWork).
//
// What still clears a listed wait: the parent's next prompt (Claude Code
// takes a prompt only when no question dialog is open), session start, any
// phase write, and any explicit status write (sciontool status). That is the
// bound for an owner that never sends an end event. The accepted cost: inside
// a running turn, such a stale owner keeps waiting_for_input through the
// parent's tool activity until the next prompt, session start or status
// write.
//
// Every add, remove and clear of the list is part of one locked update of
// agent-info.json (see update), so hook processes that fire at the same
// moment do not lose each other's changes. Only when the lock cannot be
// used (its wait timed out, or the lock file is unusable) does an update run
// unlocked, and then a change can be lost. The effect is bounded. A lost
// entry lets that question be cleared early, as any tool-end did before. A
// lost removal can hold the wait longer than before: until the stale owner's
// SubagentStop, or at the latest the bound above.
const notificationWaitAgentsKey = "notificationWaitAgents"

// parentWaitKey is the agent-info.json field that records that the parent
// agent's own wait is open inside a wait that sub-agents hold. It exists only
// next to a non-empty notificationWaitAgentsKey list, and every write that
// removes the list removes it too. Its value is the kind of the parent's wait:
//
//   - parentWaitNotification: the parent's own Notification (for example a
//     permission prompt) is open.
//   - parentWaitAgent: the parent's own AskUserQuestion or ExitPlanMode is
//     open. A later parent Notification never changes it to
//     parentWaitNotification, as a Notification leaves an agent-set wait
//     alone.
//
// The parent's wait is cleared by the same parent events that would clear
// it if no sub-agent held the wait: the parent's tool-start clears either
// kind, and its tool-end and agent-end clear only parentWaitNotification.
// The owners are cleared by their own events, as described at
// notificationWaitAgentsKey. The activity stays waiting_for_input while
// either is left. When the last owner goes and the parent's wait is still
// open, the info becomes the parent's wait alone (see restoreParentWait)
// instead of working. The next prompt, session start, phase write and status
// write clear it with the owner list.
const parentWaitKey = "parentWait"

const (
	parentWaitNotification = "notification"
	parentWaitAgent        = "agent"
)

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
	// removed from the notification wait's owner list. When it was the last
	// owner, the wait is cleared to working, or it becomes the parent's own
	// wait when one is recorded (see restoreParentWait). A background
	// sub-agent's SubagentStop can come after the parent's turn has ended,
	// with no parent tool-end left to clear the wait.
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
			// blocked, completed, limits_exceeded and crashed are kept,
			// with no owner recorded (see setWaitingFromNotificationFor).
			if isSubagentEvent(event) {
				return h.setWaitingFromNotificationFor(subagentID(event))
			}
			return h.setParentQuestionWait()
		}

		// Tool-start clears waiting_for_input of either kind (the user has
		// responded) and keeps every other held status: completed (tools may
		// fire after task_completed as wrap-up), blocked, limits_exceeded and
		// crashed. A wait that sub-agents hold is kept: the parent can run
		// tools while a background sub-agent's question is open. The
		// HubHandler reports executing only when this write happened, so the
		// Hub and the local status agree.
		return h.updateActivityIfNotSticky(result.activity, event.Data.ToolName, clearAnyWaiting, "")
	}

	// Notification event: set waiting_for_input directly (sticky), marked as
	// set by a notification so tool-end or agent-end can clear it. A
	// sub-agent's Notification is owned by that sub-agent. A Notification
	// never replaces limits_exceeded or crashed (see
	// setWaitingFromNotificationFor).
	if event.Name == hooks.EventNotification {
		return h.setWaitingFromNotificationFor(subagentID(event))
	}

	// Tool-end and agent-end: the harness prompt that a Notification reported
	// has been answered (the tool ran) or the turn is over (no prompt of the
	// parent's can be open at turn end), so a notification-set
	// waiting_for_input is cleared.
	// Every other sticky activity, including an agent-set waiting_for_input,
	// is kept. A wait that sub-agents hold is cleared only by their own
	// events: the parent's agent-end keeps it, since a background sub-agent
	// can still be asking after the parent's turn is over, unless the
	// parent's Stop reports that no background work is in flight (see
	// reportsNoBackgroundWork).
	if reportsNoBackgroundWork(event) {
		return h.endTurnWithNoBackgroundWork(result.activity)
	}
	if event.Name == hooks.EventToolEnd || event.Name == hooks.EventAgentEnd {
		return h.updateActivityIfNotSticky(result.activity, "", clearNotificationWaiting, subagentID(event))
	}

	// All other events (model-end, etc.): update activity only if current
	// activity is not sticky.
	return h.updateActivityIfNotSticky(result.activity, "", clearNone, "")
}

// setWaitingFromNotificationFor sets waiting_for_input, marks it as set by a
// Notification event and, when owner (a sub-agent's agent_id) is not empty,
// adds owner to the wait's owner list. A waiting_for_input the agent set
// itself is left unchanged: a harness prompt must not turn it into a
// notification-set wait that the prompt's answer would then clear. A
// sub-agent's question (owner not empty) also leaves blocked, completed,
// limits_exceeded and crashed unchanged and records no owner (see
// isHeldNonWaitingActivity). The parent's own Notification (owner empty)
// leaves the terminal statuses limits_exceeded and crashed unchanged: a
// notification is informational and must not clear how the agent ended. It
// still replaces blocked and completed, since for some harnesses (for
// example opencode and muse-code) the Notification is the question that
// asks the user. The parent's own Notification during a wait that sub-agents
// hold records the parent's wait in parentWaitKey, so the wait outlives the
// last owner. The check and the write are one locked update, as is the
// owner list change.
func (h *StatusHandler) setWaitingFromNotificationFor(owner string) error {
	return h.update(func(info map[string]interface{}) bool {
		if isAgentSetWaiting(info) {
			return false
		}
		current, _ := info["activity"].(string)
		if owner != "" && isHeldNonWaitingActivity(current) {
			return false
		}
		if state.Activity(current).IsTerminal() {
			return false
		}

		var owners []string
		parent := ""
		if isNotificationWaiting(info) {
			owners = notificationWaitOwners(info)
			parent = parentWaitOf(info)
		}
		if owner != "" && !containsString(owners, owner) && len(owners) < maxNotificationWaitAgents {
			owners = append(owners, owner)
		}
		if owner == "" && len(owners) > 0 && parent != parentWaitAgent {
			parent = parentWaitNotification
		}

		info["activity"] = string(state.ActivityWaitingForInput)
		info[activitySourceKey] = activitySourceNotification
		setNotificationWaitOwners(info, owners)
		setParentWait(info, parent, owners)
		delete(info, "toolName")
		delete(info, "status")
		delete(info, "sessionStatus")
		return true
	})
}

// setParentQuestionWait sets the waiting_for_input of the parent's own
// AskUserQuestion or ExitPlanMode. During a wait that sub-agents hold, it
// keeps the notification marker and the owner list and records the parent's
// wait as parentWaitAgent, so a later owner's event still matches its entry
// and the last owner's leaves the parent's agent-set wait. Otherwise it
// writes an agent-set wait as UpdateActivity does, replacing any activity.
// It is one locked update.
func (h *StatusHandler) setParentQuestionWait() error {
	return h.update(func(info map[string]interface{}) bool {
		if isNotificationWaiting(info) && len(notificationWaitOwners(info)) > 0 {
			info[parentWaitKey] = parentWaitAgent
			delete(info, "toolName")
			delete(info, "status")
			delete(info, "sessionStatus")
			return true
		}
		applyActivity(info, state.ActivityWaitingForInput, "")
		return true
	})
}

// endTurnWithNoBackgroundWork handles the parent's Stop when it reports that
// no background work is in flight (see reportsNoBackgroundWork). No
// sub-agent can still be running, so the owners of a sub-agent-held wait are
// stale: the owner list is dropped (the parent's own wait, if recorded, is
// restored) and the Stop then follows the rule for a wait with no owners. It
// is one locked update.
func (h *StatusHandler) endTurnWithNoBackgroundWork(activity state.Activity) error {
	return h.update(func(info map[string]interface{}) bool {
		dropped := false
		if isNotificationWaiting(info) && len(notificationWaitOwners(info)) > 0 {
			restoreParentWait(info)
			dropped = true
		}
		if blocks, released := stickyBlocksUpdate(info, clearNotificationWaiting, ""); blocks {
			return dropped || released
		}
		applyActivity(info, activity, "")
		return true
	})
}

// reportsNoBackgroundWork reports whether event is the claude parent's Stop
// and its background_tasks field is present and an empty array. Claude Code
// documents that field as the in-flight background work of the session
// (running, pending or backgrounded tasks, sub-agents included), with an
// empty array when nothing is in flight. The rule relies only on that
// documented meaning of the empty array; it assumes no mapping from a list
// entry to a sub-agent's agent_id. A missing field (older versions and other
// dialects), a value that is not an array, or a non-empty list of any task
// type keeps the behaviour that infers running sub-agents from their start
// and end events. A SubagentStop's list is not used: it may still list the
// sub-agent that is stopping.
func reportsNoBackgroundWork(event *hooks.Event) bool {
	if event.Name != hooks.EventAgentEnd || event.Dialect != "claude" || isSubagentEvent(event) {
		return false
	}
	tasks, ok := event.Data.Raw["background_tasks"].([]interface{})
	return ok && len(tasks) == 0
}

// releaseNotificationWaitOwner removes owner from the owner list of a
// notification-set waiting_for_input. When owner was the last entry and the
// parent's own wait is recorded, the info becomes that wait (see
// restoreParentWait). Otherwise the last entry's release clears the wait to
// working, marked with activitySourceSubagentEnd so the HubHandler reports
// it. Nothing is written when owner is not listed. It is one locked update.
func (h *StatusHandler) releaseNotificationWaitOwner(owner string) error {
	return h.update(func(info map[string]interface{}) bool {
		if !isNotificationWaiting(info) {
			return false
		}
		remaining, removed := removeString(notificationWaitOwners(info), owner)
		if !removed {
			return false
		}
		if len(remaining) == 0 {
			if restoreParentWait(info) {
				return true
			}
			applyActivity(info, state.ActivityWorking, "")
			info[activitySourceKey] = activitySourceSubagentEnd
			return true
		}
		setNotificationWaitOwners(info, remaining)
		return true
	})
}

// isHeldNonWaitingActivity reports whether activity is a held status other
// than waiting_for_input: blocked, completed, limits_exceeded or crashed. A
// sub-agent's question must not replace it, the same statuses a parent
// tool-start keeps.
func isHeldNonWaitingActivity(activity string) bool {
	return isStickyActivity(activity) && activity != string(state.ActivityWaitingForInput)
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

// parentWaitOf returns the recorded kind of the parent's own wait, or "".
func parentWaitOf(info map[string]interface{}) string {
	kind, _ := info[parentWaitKey].(string)
	return kind
}

// setParentWait writes the parent's wait kind, or removes the field when
// kind is empty or no owner is left (the field exists only next to owners).
func setParentWait(info map[string]interface{}, kind string, owners []string) {
	if kind == "" || len(owners) == 0 {
		delete(info, parentWaitKey)
		return
	}
	info[parentWaitKey] = kind
}

// restoreParentWait removes the owner list and the parent's wait field from
// info. When the parent's wait was recorded, info becomes that wait alone:
// a notification-set wait with no owners for parentWaitNotification, an
// agent-set wait (no marker) for parentWaitAgent; it then returns true, and
// the activity stays waiting_for_input. Otherwise it returns false and the
// caller decides the activity.
func restoreParentWait(info map[string]interface{}) bool {
	kind := parentWaitOf(info)
	delete(info, parentWaitKey)
	delete(info, notificationWaitAgentsKey)
	switch kind {
	case parentWaitNotification:
		info[activitySourceKey] = activitySourceNotification
		return true
	case parentWaitAgent:
		delete(info, activitySourceKey)
		return true
	}
	return false
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
	delete(info, notificationWaitAgentsKey)
	delete(info, parentWaitKey)

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
// activity a hook has already written. The check and the write are one
// locked update (see update). It leaves activitySource and the
// notification wait owners alone; that is safe because the only caller
// replaces initializing, which no notification write sets.
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
	// Notification set it (tool-end, agent-end, and a sub-agent's tool-start
	// or prompt). A wait that sub-agents hold is cleared only once the last
	// of them has had its own event; the parent's tool-end and agent-end
	// keep it.
	clearNotificationWaiting
	// clearAnyWaiting clears any waiting_for_input (the parent's tool-start:
	// the user has responded and the agent is running a tool), except a
	// wait that sub-agents hold.
	clearAnyWaiting
)

// stickyBlocksUpdate reports whether the activity recorded in info is sticky
// and must not be overwritten by an event with the given clear rule. Only
// waiting_for_input can be cleared; completed, blocked, limits_exceeded and
// crashed always block. subagent is the event's sub-agent agent_id, or ""
// for the parent's own event. When the event is blocked but changed info,
// released is true, so the caller writes info. That happens when a
// sub-agent's event removes its own entry while other owners remain, when
// the last owner's event leaves the parent's recorded wait (see
// restoreParentWait), and when a parent event clears the parent's recorded
// wait (parentWaitKey) while owners remain.
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
		// Only the parent's tool-start uses this rule. It keeps a wait
		// that sub-agents hold, and clears the parent's own wait of
		// either kind inside it.
		if !isNotificationWaiting(info) || len(notificationWaitOwners(info)) == 0 {
			return false, false
		}
		if parentWaitOf(info) != "" {
			delete(info, parentWaitKey)
			return true, true
		}
		return true, false
	case clearNotificationWaiting:
		if !isNotificationWaiting(info) {
			return true, false
		}
		owners := notificationWaitOwners(info)
		if len(owners) == 0 {
			return false, false
		}
		if subagent == "" {
			// The parent's own tool-end or agent-end keeps a wait that
			// sub-agents hold. It clears the parent's own Notification
			// inside it, as it would with no owners, and keeps the
			// parent's own question.
			if parentWaitOf(info) == parentWaitNotification {
				delete(info, parentWaitKey)
				return true, true
			}
			return true, false
		}
		remaining, removed := removeString(owners, subagent)
		if !removed {
			return true, false // a sibling's question is still open
		}
		if len(remaining) == 0 {
			if restoreParentWait(info) {
				return true, true
			}
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
	return h.update(func(info map[string]interface{}) bool {
		if blocks, released := stickyBlocksUpdate(info, clear, subagent); blocks {
			// Activity is sticky, don't overwrite. When only this
			// sub-agent's owner entry changed, write that.
			return released
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
// notification-set, see Handle), unless the agent holds blocked, completed,
// limits_exceeded or crashed (see setWaitingFromNotificationFor).
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
