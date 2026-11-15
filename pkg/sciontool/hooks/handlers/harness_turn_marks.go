/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Files the hook process leaves for `sciontool init`'s harness_error
// watchdogs, which read them without IPC (design §4.2 (2) and (3)).
const (
	// DefaultTurnEventsPath records when the last turn event arrived: a
	// turn-start (prompt-submit, agent-start, model-start, tool-start) or
	// an error-free turn progress (model-end, agent-end) event.
	DefaultTurnEventsPath = "/tmp/scion-turn-events"
	// DefaultPendingHarnessErrorPath records the last errored model-end
	// or tool-end: evidence for the first-turn watchdog's reason. It does
	// not set harness_error by itself; the harness may still retry.
	DefaultPendingHarnessErrorPath = "/tmp/scion-last-harness-error.json"
)

// turnMarkFileMode lets init (root) and the agent user read both files.
const turnMarkFileMode = 0o644

// turnMarkMaxBytes bounds reads of either file. The pending error's reason
// is capped at harnessErrorReasonMaxRunes runes (at most 4 bytes each).
const turnMarkMaxBytes = 4096

// PendingHarnessError is the content of the pending error file.
type PendingHarnessError struct {
	At     time.Time `json:"at"`
	Event  string    `json:"event"`
	Reason string    `json:"reason"`
}

// TurnMarkHandler writes the turn-event marker and the pending error file.
// It is a hook handler: it never fails the hook, never waits on the
// network, and logs and drops every error.
type TurnMarkHandler struct {
	TurnEventsPath   string
	PendingErrorPath string
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// Handle records ev when it is a turn event (see IsTurnEvent) or an
// errored model-end or tool-end. A sub-agent's event counts: it shows the
// harness is running the task.
func (h *TurnMarkHandler) Handle(ev *hooks.Event) error {
	if h == nil || ev == nil {
		return nil
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	switch {
	case IsTurnEvent(ev):
		h.write(h.TurnEventsPath, []byte(now().UTC().Format(time.RFC3339Nano)+"\n"))
	case isPendingErrorEvent(ev):
		data, err := json.Marshal(PendingHarnessError{
			At:     now().UTC(),
			Event:  ev.Name,
			Reason: pendingErrorReason(ev),
		})
		if err != nil {
			log.Debug("pending harness error: marshal: %v", err)
			return nil
		}
		h.write(h.PendingErrorPath, data)
	}
	return nil
}

func (h *TurnMarkHandler) write(path string, data []byte) {
	if path == "" {
		return
	}
	if err := dirfd.WriteFileNoFollow(path, data, turnMarkFileMode, 0, 0, dirfd.ReplaceLeaf); err != nil {
		log.Debug("turn mark: writing %s: %v", path, err)
	}
}

// IsTurnStartEvent reports whether name is an event that starts or advances
// a turn: prompt-submit, agent-start, model-start or tool-start.
func IsTurnStartEvent(name string) bool {
	switch name {
	case hooks.EventPromptSubmit, hooks.EventAgentStart, hooks.EventModelStart, hooks.EventToolStart:
		return true
	}
	return false
}

// IsTurnEvent reports whether ev shows the harness running a turn: a
// turn-start event, or a model-end or agent-end without an error. The
// error-free ends count because some harnesses send no turn-start event for
// the initial prompt: OpenCode maps no agent-start, model-start or
// prompt-submit for it (its first event is a step-finish model-end, a tool
// call or the turn's agent-end), and pi's prompt-submit is human input only.
// A model-end or agent-end with an error is not proof of progress; it is the
// pending error (or B1's hook path). Neither is an uncounted model-end.
func IsTurnEvent(ev *hooks.Event) bool {
	if IsTurnStartEvent(ev.Name) {
		return true
	}
	switch ev.Name {
	case hooks.EventModelEnd:
		return isProgressModelEnd(ev)
	case hooks.EventAgentEnd:
		return strings.TrimSpace(ev.Data.Error) == ""
	}
	// Everything else is not progress, including hooks.EventIgnored (an
	// event its dialect discards, such as grok-build's agent_error
	// notification) and hooks.EventAgentError (B1's hook path).
	return false
}

// isProgressModelEnd reports whether ev, a model-end, shows the harness
// reached the model: its error field is empty, and it is not uncounted (an
// uncounted model-end, copilot's errorOccurred, reports only failed calls,
// so it is never proof of progress, even if its error text is missing).
// An error field holding only whitespace still says the call failed, so it
// is not progress either. The turn marks (IsTurnEvent) and the
// harness_error clearing rule (isHarnessProgressEvent) share it.
func isProgressModelEnd(ev *hooks.Event) bool {
	return ev.Data.Error == "" && !ev.Data.Uncounted
}

// isPendingErrorEvent reports whether ev is a model-end or tool-end that
// carries an error.
func isPendingErrorEvent(ev *hooks.Event) bool {
	if ev.Name != hooks.EventModelEnd && ev.Name != hooks.EventToolEnd {
		return false
	}
	return strings.TrimSpace(ev.Data.Error) != ""
}

// pendingErrorReason renders ev's error with the harness_error reason
// format and redaction: "model API: <text>" for a model-end, "tool <name>:
// <text>" for a tool-end.
func pendingErrorReason(ev *hooks.Event) string {
	source := harnessErrorSourceModelAPI
	if ev.Name == hooks.EventToolEnd {
		source = "tool"
		if name := singleLine(ev.Data.ToolName); name != "" {
			source = "tool " + truncateRunes(name, 64)
		}
	}
	return formatHarnessErrorReason(source, ev.Data.Error)
}

// ReadLastTurnEventAt returns when the last turn-start event was recorded
// at path, and false when there is none or the file is not readable.
func ReadLastTurnEventAt(path string) (time.Time, bool) {
	data, err := dirfd.ReadFileNoFollow(path, turnMarkMaxBytes)
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// ReadPendingHarnessError returns the pending error recorded at path, and
// false when there is none or the file is not well formed.
func ReadPendingHarnessError(path string) (PendingHarnessError, bool) {
	data, err := dirfd.ReadFileNoFollow(path, turnMarkMaxBytes)
	if err != nil {
		return PendingHarnessError{}, false
	}
	var p PendingHarnessError
	if err := json.Unmarshal(data, &p); err != nil || p.Reason == "" || p.At.IsZero() {
		return PendingHarnessError{}, false
	}
	return p, true
}

// FormatHarnessErrorReason renders a harness_error reason as
// "<source>: <text>" with the redaction and 300-rune cap every
// harness_error reason gets (see formatHarnessErrorReason).
func FormatHarnessErrorReason(source, text string) string {
	return formatHarnessErrorReason(source, text)
}
