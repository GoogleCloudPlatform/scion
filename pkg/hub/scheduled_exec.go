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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// Exec schedules (ptone/scion#7818, Phase 1 ptone/scion#7883): an "exec"
// scheduled event runs an argv in the target agent's container over the
// agent's Conduit session (the exec-start and exec-status control routes,
// .design/conduit/exec-control-routes.md). Phase 1 covers one-shot events
// only; recurring exec schedules are rejected at authoring.

// scheduledEventTypeExec is the event type of an exec event.
const scheduledEventTypeExec = "exec"

// scheduledExecPermission is the permission an exec event's authoring and
// every fire exercise on the target agent (D1): the permission live /exec
// uses.
const scheduledExecPermission = "agent.attach"

// Bounds of an exec payload (design "Event type and payload"). The argv
// bounds are the exec routes' own (agentcontrol).
const (
	// scheduledExecDefaultTimeout is the timeout of a payload that sets
	// none (timeoutSeconds 0).
	scheduledExecDefaultTimeout = 60
	// scheduledExecMaxPayloadBytes bounds the stored payload.
	scheduledExecMaxPayloadBytes = 32 * 1024
	// scheduledExecMaxWindow is how late a fire may run: a one-shot exec
	// event found due more than this after its fire time is skipped
	// (missed window), never run.
	scheduledExecMaxWindow = 10 * time.Minute
)

// scheduledExecLeaseGrace is added to a run's timeout to form its lease
// (lease_until = started_at + timeout + grace). A variable only as a test
// seam: tests that change it must not use t.Parallel and must restore it.
var scheduledExecLeaseGrace = 120 * time.Second

// scheduleRequestBodyLimit bounds the body of a /schedules or
// /scheduled-events create or update request, for every event type.
const scheduleRequestBodyLimit = 64 * 1024

// scheduledExecPayload is the JSON payload of an exec event.
type scheduledExecPayload struct {
	// AgentID is the target, resolved at authoring. It is authoritative:
	// a fire never falls back to AgentName.
	AgentID string `json:"agentId"`
	// AgentName is the target's slug at authoring, for display only.
	AgentName string `json:"agentName,omitempty"`
	// Command is the argv, run directly with no shell.
	Command []string `json:"command"`
	// TimeoutSeconds is 0 (the default, 60) to agentcontrol.MaxExecTimeoutSec.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// Wake is reserved for waking suspended targets (F1); it must be false.
	Wake bool `json:"wake,omitempty"`
}

// errScheduledExecPayloadInvalid is the error of a payload outside the
// schema or bounds. Its text names no value.
var errScheduledExecPayloadInvalid = errors.New("invalid exec payload")

// scheduledExecPayloadError names the payload field that failed, never its
// value.
type scheduledExecPayloadError struct {
	field string
}

func (e *scheduledExecPayloadError) Error() string {
	return errScheduledExecPayloadInvalid.Error() + ": " + e.field
}

func (e *scheduledExecPayloadError) Unwrap() error { return errScheduledExecPayloadInvalid }

// decodeScheduledExecPayload decodes and validates a stored or submitted
// exec payload. It is strict: unknown fields, trailing data and anything
// outside the bounds fail, so a fire fails closed on a stored row that no
// longer validates. It does not require AgentID (authoring fills it in);
// validateScheduledExecPayloadForFire does.
func decodeScheduledExecPayload(raw string) (*scheduledExecPayload, error) {
	if raw == "" {
		return nil, &scheduledExecPayloadError{field: "payload"}
	}
	if len(raw) > scheduledExecMaxPayloadBytes {
		return nil, &scheduledExecPayloadError{field: "payload"}
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var p scheduledExecPayload
	if err := dec.Decode(&p); err != nil {
		return nil, &scheduledExecPayloadError{field: "payload"}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &scheduledExecPayloadError{field: "payload"}
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// validate checks the payload bounds (design "Event type and payload").
func (p *scheduledExecPayload) validate() error {
	if err := agentcontrol.ValidateExecArgv(p.Command); err != nil {
		return &scheduledExecPayloadError{field: "command"}
	}
	if p.TimeoutSeconds < 0 || p.TimeoutSeconds > agentcontrol.MaxExecTimeoutSec {
		return &scheduledExecPayloadError{field: "timeoutSeconds"}
	}
	if p.Wake {
		return &scheduledExecPayloadError{field: "wake"}
	}
	return nil
}

// timeout returns the run timeout in seconds.
func (p *scheduledExecPayload) timeout() int {
	if p.TimeoutSeconds == 0 {
		return scheduledExecDefaultTimeout
	}
	return p.TimeoutSeconds
}

// encode returns the stored form of the payload.
func (p *scheduledExecPayload) encode() (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return "", err
	}
	out := strings.TrimSuffix(buf.String(), "\n")
	if len(out) > scheduledExecMaxPayloadBytes {
		return "", &scheduledExecPayloadError{field: "payload"}
	}
	return out, nil
}

// execSchedulesEnabled reports whether the hub accepts and fires exec
// events: hub.exec_schedules and the hub.conduit it requires are both on
// (D8). It does not need a conduit router: authoring needs none, and the
// fire path's exec-start client checks routability itself.
func (s *Server) execSchedulesEnabled() bool {
	return s.experimentEnabled(experiments.ExecSchedules) && s.experimentEnabled(conduitExperiment)
}

// scheduledExecShells are the argv0 basenames whose "-c" form the audit
// records as shell_form (issue Q3: shell use is allowed and audited).
var scheduledExecShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true}

// scheduledExecArgv0 returns argv[0]'s basename for the audit: at most 64
// bytes, control characters (newlines included) removed. It is the only
// part of argv any log or audit record carries.
func scheduledExecArgv0(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	base := path.Base(argv[0])
	var b strings.Builder
	for _, r := range base {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			continue
		}
		if b.Len()+len(string(r)) > 64 {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// scheduledExecShellForm reports whether argv is a shell's -c form.
func scheduledExecShellForm(argv []string) bool {
	return len(argv) >= 2 && scheduledExecShells[path.Base(argv[0])] && argv[1] == "-c"
}

// Generic outcome texts an exec event records (ScheduledEvent.Error), which
// every project schedule reader sees. They never carry argv, output or a
// detailed refusal reason; the audit log and slog carry the reason.
const (
	execReasonDisabled         = "exec schedules disabled"
	execReasonMissedWindow     = "missed window"
	execReasonRefused          = "exec refused"
	execReasonTargetNotRunning = "target not running"
	execReasonOverlap          = "previous run in progress"
	execReasonCapacity         = "capacity"
	execReasonUnavailable      = "agent executor unavailable"
	execReasonStartFailed      = "command failed to start"
	execReasonDispatchFailed   = "exec dispatch failed"
	execReasonStartExpired     = "start expired (check agent clock)"
	execReasonAgentRestarted   = "agent restarted during run"
	execReasonAgentStopped     = "agent stopped during run"
	execReasonOutcomeUnknown   = "run outcome unknown"
	execReasonShuttingDown     = "hub shutting down"
	execReasonRecurring        = "recurring exec schedules are not supported"
)

// execReasonExitStatus is the text of a non-zero exit (D6).
func execReasonExitStatus(code int) string { return fmt.Sprintf("exit status %d", code) }

// execReasonTimedOut is the text of a run killed at its timeout.
func execReasonTimedOut(timeoutSeconds int) string {
	return fmt.Sprintf("timed out after %ds", timeoutSeconds)
}
