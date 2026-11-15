/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// SetHarnessErrorIf records harness_error with reason in detail.message,
// in one locked update of agent-info.json, when allow accepts the current
// activity and the phase is running (or not recorded). It reports whether
// it wrote. sciontool init's watchdogs use it (ptone/scion#7858 design
// §4.2 (2), (3)); allow lets each watchdog name the activities it may
// replace, and a harness_error already set by the hook path is never
// replaced when allow rejects it, so the hook path wins and the reason is
// not duplicated.
func (h *StatusHandler) SetHarnessErrorIf(reason string, allow func(state.Activity) bool) (bool, error) {
	wrote := false
	err := h.update(func(info map[string]interface{}) bool {
		if phase := stringField(info, "phase"); phase != "" && phase != string(state.PhaseRunning) {
			return false
		}
		current := state.Activity(stringField(info, "activity"))
		if current == state.ActivityHarnessError || current.IsTerminal() || allow == nil || !allow(current) {
			return false
		}
		applyActivity(info, state.ActivityHarnessError, "")
		applyMessage(info, reason)
		wrote = true
		return true
	})
	if err != nil {
		return false, err
	}
	return wrote, nil
}

// RestoreFromHarnessErrorIf replaces harness_error with `to`, and drops the
// reason from detail.message, in one locked update, only while the activity
// is harness_error and its reason is still `reason`. sciontool init's
// first-turn watchdog uses it to retract its own report; a harness_error
// with any other reason (the hook path's) is left alone. It reports
// whether it wrote.
func (h *StatusHandler) RestoreFromHarnessErrorIf(reason string, to state.Activity) (bool, error) {
	wrote := false
	err := h.update(func(info map[string]interface{}) bool {
		if stringField(info, "activity") != string(state.ActivityHarnessError) || detailMessage(info) != reason {
			return false
		}
		applyActivity(info, to, "")
		applyMessage(info, "")
		wrote = true
		return true
	})
	if err != nil {
		return false, err
	}
	return wrote, nil
}

// HarnessErrorStatusUpdate is the Hub status update that reports
// harness_error with reason, shared by the hook path (HubHandler) and
// sciontool init's watchdogs.
func HarnessErrorStatusUpdate(reason string) hub.StatusUpdate {
	as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityHarnessError}
	return hub.StatusUpdate{
		Activity: state.ActivityHarnessError,
		Status:   as.DisplayStatus(),
		Message:  reason,
	}
}
