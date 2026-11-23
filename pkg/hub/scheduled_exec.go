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
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
)

// Exec schedules (ptone/scion#7818, Phase 1 ptone/scion#7883, Phase 2
// ptone/scion#7884): an "exec" scheduled event or recurring schedule runs an
// argv in the target agent's container over the agent's Conduit session (the
// exec-start and exec-status control routes,
// .design/conduit/exec-control-routes.md).

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
	// scheduledExecMaxWindow is how late a fire may run: an exec event
	// found due more than this after its scheduled due time is skipped
	// (missed window), never run.
	scheduledExecMaxWindow = 10 * time.Minute

	// Trigger-exec context and per-trigger caps (Stage 2 P8, ptone/scion#8241).
	// Trigger exec caps are separate from exec-schedule quotas; the container's
	// 2-live-run bound in sciontool remains the shared hard floor.
	triggerExecMaxContextAttributes     = 32
	triggerExecMaxContextAttrKeyLen     = 64
	triggerExecMaxContextAttrValueBytes = 4096
	triggerExecMaxActivePerTarget       = 20
	triggerExecMaxActivePerProject      = 100
	triggerExecMaxFiresPerMinute        = 30
)

// scheduledExecLeaseGrace is added to a run's timeout to form its lease
// (lease_until = started_at + timeout + grace). A variable only as a test
// seam: tests that change it must not use t.Parallel and must restore it.
var scheduledExecLeaseGrace = 120 * time.Second

// scheduleRequestBodyLimit bounds the body of a /schedules or
// /scheduled-events create or update request, for every event type.
const scheduleRequestBodyLimit = 64 * 1024

// scheduledExecPayload is the JSON payload of an exec event or trigger.
type scheduledExecPayload struct {
	// AgentID is the target, resolved at authoring. It is authoritative:
	// a fire never falls back to AgentName.
	AgentID string `json:"agentId"`
	// AgentName is the target's slug at authoring, for display only.
	AgentName string `json:"agentName,omitempty"`
	// Command is the argv, run directly with no shell. Never templated.
	Command []string `json:"command"`
	// TimeoutSeconds is 0 (the default, 60) to agentcontrol.MaxExecTimeoutSec.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// CaptureOutput opts into bounded output capture: "", "none" (stored as
	// absent), "json", or "tail".
	CaptureOutput string `json:"captureOutput,omitempty"`
	// Wake is reserved for waking suspended targets (F1); it must be false.
	Wake bool `json:"wake,omitempty"`
	// ContextAttributes is the per-rule allow-list of event attribute keys
	// copied into SCION_TRIGGER_CONTEXT for trigger-routed exec runs (P8).
	// Refused for schedules at authoring.
	ContextAttributes []string `json:"contextAttributes,omitempty"`
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
	switch p.CaptureOutput {
	case "", "none":
		p.CaptureOutput = ""
	case agentcontrol.ExecCaptureJSON, agentcontrol.ExecCaptureTail:
	default:
		return &scheduledExecPayloadError{field: "captureOutput"}
	}
	if p.Wake {
		return &scheduledExecPayloadError{field: "wake"}
	}
	if len(p.ContextAttributes) > 0 {
		normalized, err := normalizeExecContextAttributes(p.ContextAttributes)
		if err != nil {
			return &scheduledExecPayloadError{field: "contextAttributes"}
		}
		p.ContextAttributes = normalized
	}
	return nil
}

// isValidExecContextAttrKey reports whether k is a safe event attribute key
// for SCION_TRIGGER_CONTEXT allow-listing ([A-Za-z0-9_.-]{1,64}).
func isValidExecContextAttrKey(k string) bool {
	if k == "" || len(k) > triggerExecMaxContextAttrKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		b := k[i]
		ok := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			b == '_' || b == '.' || b == '-'
		if !ok {
			return false
		}
	}
	return true
}

// normalizeExecContextAttributes validates, deduplicates, and sorts
// contextAttributes keys.
func normalizeExecContextAttributes(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > triggerExecMaxContextAttributes {
		return nil, fmt.Errorf("contextAttributes exceeds maximum of %d keys", triggerExecMaxContextAttributes)
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, k := range raw {
		if !isValidExecContextAttrKey(k) {
			return nil, fmt.Errorf("invalid contextAttributes key")
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	slices.Sort(out)
	return out, nil
}

// validateTriggerExecCommandNoTemplates rejects any "${" sequence in a trigger
// exec command at authoring time so event data can never be templated into argv.
func validateTriggerExecCommandNoTemplates(argv []string) (int, bool) {
	for i, arg := range argv {
		if strings.Contains(arg, "${") {
			return i, false
		}
	}
	return -1, true
}

// triggerExecContextPayload is the bounded JSON object delivered via the
// SCION_TRIGGER_CONTEXT environment variable on trigger-routed exec runs.
type triggerExecContextPayload struct {
	EventID    string            `json:"event_id"`
	TriggerID  string            `json:"trigger_id"`
	FireID     string            `json:"fire_id"`
	Subject    string            `json:"subject,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// buildTriggerExecContext constructs the bounded SCION_TRIGGER_CONTEXT JSON
// string for req, copying only event attributes explicitly listed in allowAttrs.
// Any invalid UTF-8, NUL byte, or bound violation returns an error (never truncates).
func buildTriggerExecContext(req hubfire.FireRequest, allowAttrs []string) (string, error) {
	if req.Event == nil {
		return "", errors.New("missing trigger event for exec context")
	}
	subject := req.Event.Subject
	if req.Event.Trigger != nil && req.Event.Trigger.FireSubject != "" {
		subject = req.Event.Trigger.FireSubject
	}
	if subject != "" && (!utf8.ValidString(subject) || strings.IndexByte(subject, 0) >= 0) {
		return "", errors.New("trigger context subject violates bounds")
	}
	payload := triggerExecContextPayload{
		EventID:   req.Event.ID,
		TriggerID: req.Authority.DefinitionID,
		FireID:    req.Authority.FireID,
		Subject:   subject,
	}
	if len(allowAttrs) > 0 && len(req.Event.Attributes) > 0 {
		attrs := make(map[string]string, len(allowAttrs))
		for _, k := range allowAttrs {
			v, ok := req.Event.Attributes[k]
			if !ok {
				continue
			}
			if len(v) > triggerExecMaxContextAttrValueBytes || !utf8.ValidString(v) || strings.IndexByte(v, 0) >= 0 {
				return "", errors.New("trigger context attribute violates bounds")
			}
			attrs[k] = v
		}
		if len(attrs) > 0 {
			payload.Attributes = attrs
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return "", err
	}
	out := strings.TrimSuffix(buf.String(), "\n")
	if err := agentcontrol.ValidateExecTriggerContext(out); err != nil {
		return "", err
	}
	return out, nil
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
	execReasonDisabled                 = "exec schedules disabled"
	execReasonMissedWindow             = "missed window"
	execReasonRefused                  = "exec refused"
	execReasonTargetNotRunning         = "target not running"
	execReasonOverlap                  = "previous run in progress"
	execReasonCapacity                 = "capacity"
	execReasonUnavailable              = "agent executor unavailable"
	execReasonCaptureUnsupported       = "capture_unsupported"
	execReasonStartFailed              = "command failed to start"
	execReasonDispatchFailed           = "exec dispatch failed"
	execReasonStartExpired             = "start expired (check agent clock)"
	execReasonAgentRestarted           = "agent restarted during run"
	execReasonAgentStopped             = "agent stopped during run"
	execReasonOutcomeUnknown           = "run outcome unknown"
	execReasonShuttingDown             = "hub shutting down"
	execReasonNotDispatched            = "hub restarted before dispatch"
	execReasonSourceNotReplayProtected = "source not replay protected"
	execReasonRateLimit                = "rate limit"
	execReasonSubjectBusy              = "subject busy"
	execReasonContextInvalid           = "trigger context invalid"
	execReasonContextUnsupported       = "sciontool lacks exec_context.v1"
	execReasonMissingDeliveryID        = "webhook delivery missing delivery id"
)

// execReasonExitStatus is the text of a non-zero exit (D6).
func execReasonExitStatus(code int) string { return fmt.Sprintf("exit status %d", code) }

// execReasonTimedOut is the text of a run killed at its timeout.
func execReasonTimedOut(timeoutSeconds int) string {
	return fmt.Sprintf("timed out after %ds", timeoutSeconds)
}
