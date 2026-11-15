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
	"encoding/json"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The response serializer of schedules and scheduled events
// (ptone/scion#7883, design "Read API and redaction", D3, SEC-P1, SEC-P5).
// Every handler that writes a schedule or scheduled-event body (the
// /schedules and /scheduled-events routes and /admin/scheduler) builds it
// here. For an exec row the payload is redacted by default and revealed
// only to a caller whose current credential holds live agent.attach on the
// target; the revision's principal (the creator) is not exempt.
// TestScheduledResponses_RouteEnumerationGuard holds every route to it.

// ScheduledEventResponse is the API form of one scheduled event.
type ScheduledEventResponse struct {
	store.ScheduledEvent
	// Payload shadows the stored payload: the stored one for non-exec
	// events, and for an exec event the stored one only when revealed,
	// otherwise its redacted form.
	Payload string `json:"payload"`
	// ExecSummary is the non-secret summary of an exec event's run, for
	// every scheduled_event.read holder. Absent until the run is admitted.
	ExecSummary *ExecSummary `json:"execSummary,omitempty"`
}

// ScheduleResponse is the API form of one recurring schedule.
type ScheduleResponse struct {
	store.Schedule
	// Payload shadows the stored payload as ScheduledEventResponse's does.
	Payload string `json:"payload"`
}

// ExecSummary is an exec run's outcome without its command or output (D2 =
// b): exit codes, durations and byte counts are not secret.
type ExecSummary struct {
	Outcome     string     `json:"outcome"`
	ExitCode    *int       `json:"exitCode,omitempty"`
	DurationMS  *int64     `json:"durationMs,omitempty"`
	OutputBytes *int64     `json:"outputBytes,omitempty"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// redactedExecPayload is the payload an exec row shows a caller without
// live attach on its target.
type redactedExecPayload struct {
	AgentID         string `json:"agentId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	TimeoutSeconds  int    `json:"timeoutSeconds,omitempty"`
	CommandRedacted bool   `json:"commandRedacted"`
	Argc            int    `json:"argc,omitempty"`
	CommandSHA256   string `json:"commandSha256,omitempty"`
}

// redactExecPayload returns the redacted form of a stored exec payload.
// A payload that does not decode redacts to the bare marker: nothing of it
// is shown.
func redactExecPayload(raw string) string {
	out := redactedExecPayload{CommandRedacted: true}
	var p scheduledExecPayload
	if err := json.Unmarshal([]byte(raw), &p); err == nil {
		out.AgentID, out.AgentName, out.TimeoutSeconds = p.AgentID, p.AgentName, p.TimeoutSeconds
		out.Argc = len(p.Command)
		out.CommandSHA256 = agentcontrol.ExecArgvDigest(p.Command)
	}
	b, _ := json.Marshal(out) // a fixed struct of strings and ints always encodes
	return string(b)
}

// execPayloadTarget returns the target agent ID of a stored exec payload,
// "" when it does not decode.
func execPayloadTarget(raw string) string {
	var p struct {
		AgentID string `json:"agentId"`
	}
	if json.Unmarshal([]byte(raw), &p) != nil {
		return ""
	}
	return p.AgentID
}

// execRevealer decides, once per target agent per request, whether the
// request's caller may see exec argv: live agent.attach on the target
// through authorizeAgentTargetAction with the caller's current credential.
type execRevealer struct {
	s        *Server
	ctx      context.Context
	identity Identity
	never    bool
	memo     map[string]bool
}

// newExecRevealer returns the revealer of the request in ctx. A revealer
// for the admin view (never) reveals nothing.
func (s *Server) newExecRevealer(ctx context.Context) *execRevealer {
	return &execRevealer{s: s, ctx: ctx, identity: GetIdentityFromContext(ctx), memo: map[string]bool{}}
}

// noExecRevealer reveals nothing (/admin/scheduler).
func (s *Server) noExecRevealer(ctx context.Context) *execRevealer {
	return &execRevealer{s: s, ctx: ctx, never: true}
}

// reveals reports whether the caller may see the argv of an exec row
// targeting agentID. Any lookup failure hides it.
func (r *execRevealer) reveals(agentID string) bool {
	if r == nil || r.never || r.identity == nil || agentID == "" {
		return false
	}
	if v, ok := r.memo[agentID]; ok {
		return v
	}
	reveal := false
	if target, err := r.s.store.GetAgent(r.ctx, agentID); err == nil && target != nil && target.DeletedAt.IsZero() {
		reveal = r.s.authorizeAgentTargetAction(r.ctx, r.identity, target, ActionAttach) == nil
	}
	r.memo[agentID] = reveal
	return reveal
}

// payloadFor returns the payload of a row of eventType a caller sees.
func (r *execRevealer) payloadFor(eventType, raw string) string {
	if eventType != scheduledEventTypeExec {
		return raw
	}
	if r.reveals(execPayloadTarget(raw)) {
		return raw
	}
	return redactExecPayload(raw)
}

// scheduledEventResponses serializes events for the caller of rev. The run
// summaries of exec events are read in one lookup; a failed lookup omits
// them.
func (s *Server) scheduledEventResponses(ctx context.Context, rev *execRevealer, events []store.ScheduledEvent) []ScheduledEventResponse {
	var execIDs []string
	for i := range events {
		if events[i].EventType == scheduledEventTypeExec {
			execIDs = append(execIDs, events[i].ID)
		}
	}
	var runs map[string]store.ScheduledExecRun
	if len(execIDs) > 0 && s.store != nil {
		var err error
		if runs, err = s.store.ListScheduledExecRunsByEvent(ctx, execIDs); err != nil {
			slog.WarnContext(ctx, "scheduled event response: exec run lookup failed", "error", err)
		}
	}
	out := make([]ScheduledEventResponse, 0, len(events))
	for i := range events {
		out = append(out, buildScheduledEventResponse(rev, events[i], runs))
	}
	return out
}

// scheduledEventResponse serializes one event for the caller of rev.
func (s *Server) scheduledEventResponse(ctx context.Context, rev *execRevealer, evt store.ScheduledEvent) ScheduledEventResponse {
	return s.scheduledEventResponses(ctx, rev, []store.ScheduledEvent{evt})[0]
}

func buildScheduledEventResponse(rev *execRevealer, evt store.ScheduledEvent, runs map[string]store.ScheduledExecRun) ScheduledEventResponse {
	payload := rev.payloadFor(evt.EventType, evt.Payload)
	// The embedded copy never carries the stored payload, so no encoder
	// path can emit it.
	evt.Payload = ""
	resp := ScheduledEventResponse{ScheduledEvent: evt, Payload: payload}
	if run, ok := runs[evt.ID]; ok && evt.EventType == scheduledEventTypeExec {
		resp.ExecSummary = &ExecSummary{
			Outcome:     run.Outcome,
			ExitCode:    run.ExitCode,
			DurationMS:  run.DurationMS,
			OutputBytes: run.OutputBytes,
			StartedAt:   run.StartedAt,
			FinishedAt:  run.FinishedAt,
		}
	}
	return resp
}

// scheduleResponses serializes schedules for the caller of rev.
func scheduleResponses(rev *execRevealer, schedules []store.Schedule) []ScheduleResponse {
	out := make([]ScheduleResponse, 0, len(schedules))
	for i := range schedules {
		out = append(out, scheduleResponse(rev, schedules[i]))
	}
	return out
}

// scheduleResponse serializes one schedule for the caller of rev.
func scheduleResponse(rev *execRevealer, sc store.Schedule) ScheduleResponse {
	payload := rev.payloadFor(sc.EventType, sc.Payload)
	sc.Payload = ""
	return ScheduleResponse{Schedule: sc, Payload: payload}
}
