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
	"strings"
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
	// CreatedByName is the resolved human-readable identifier for CreatedBy
	// (user display name or email, or agent slug), omitted when unresolvable
	// or when the caller is not authorized to read the creator principal.
	CreatedByName string `json:"createdByName,omitempty"`
	// CreatedByType is the principal kind of CreatedBy ("user" or "agent")
	// when CreatedByName is populated.
	CreatedByType string `json:"createdByType,omitempty"`
}

// ExecSummary is an exec run's non-secret outcome metadata without its command
// or captured output bytes: exit codes, durations, byte counts, capture mode
// and output status are non-secret.
type ExecSummary struct {
	Outcome      string     `json:"outcome"`
	ExitCode     *int       `json:"exitCode,omitempty"`
	DurationMS   *int64     `json:"durationMs,omitempty"`
	OutputBytes  *int64     `json:"outputBytes,omitempty"`
	CaptureMode  string     `json:"captureMode,omitempty"`
	OutputStatus string     `json:"outputStatus,omitempty"`
	StartedAt    time.Time  `json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
}

// redactedExecPayload is the payload an exec row shows a caller without
// live attach on its target.
type redactedExecPayload struct {
	AgentID         string `json:"agentId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	TimeoutSeconds  int    `json:"timeoutSeconds,omitempty"`
	CaptureOutput   string `json:"captureOutput,omitempty"`
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
		if p.CaptureOutput != "none" {
			out.CaptureOutput = p.CaptureOutput
		}
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

type resolvedScheduleCreator struct {
	name string
	kind string
}

// execRevealer decides, once per target agent per request, whether the
// request's caller may see exec argv: live agent.attach on the target
// through authorizeAgentTargetAction with the caller's current credential.
// It also memoizes schedule creator resolution across rows of one response.
type execRevealer struct {
	s        *Server
	ctx      context.Context
	identity Identity
	never    bool
	memo     map[string]bool
	creators map[string]resolvedScheduleCreator
}

// newExecRevealer returns the revealer of the request in ctx. A revealer
// for the admin view (never) reveals nothing.
func (s *Server) newExecRevealer(ctx context.Context) *execRevealer {
	return &execRevealer{
		s:        s,
		ctx:      ctx,
		identity: GetIdentityFromContext(ctx),
		memo:     map[string]bool{},
		creators: map[string]resolvedScheduleCreator{},
	}
}

// noExecRevealer reveals no exec argv (/admin/scheduler), while still carrying
// the caller's identity so schedule creator resolution is authorized per viewer.
func (s *Server) noExecRevealer(ctx context.Context) *execRevealer {
	return &execRevealer{
		s:        s,
		ctx:      ctx,
		identity: GetIdentityFromContext(ctx),
		never:    true,
		creators: map[string]resolvedScheduleCreator{},
	}
}

// canReadCreatorUser mirrors getUser (handlers_users_core.go) so schedule
// responses never widen who can see another user's display name or email.
func (r *execRevealer) canReadCreatorUser(userID string) bool {
	if r.identity == nil {
		return false
	}
	if _, isAgent := r.identity.(AgentIdentity); isAgent {
		return false
	}
	if token, isToken := r.identity.(*ScopedUserIdentity); isToken {
		if r.s.authzService == nil {
			return false
		}
		return r.s.authzService.Decide(r.ctx, AuthzRequest{
			Principal:  principalContextForIdentity(token),
			Credential: credentialContextForIdentity(token),
			Resource:   hubScopedResource("user", userID),
			Action:     ActionRead,
			Permission: "user.read",
		}).Allowed
	}
	return true
}

// canReadCreatorAgent reports whether the caller may read agent's identity.
func (r *execRevealer) canReadCreatorAgent(agent *store.Agent) bool {
	if r.identity == nil {
		return false
	}
	if agentIdent, isAgent := r.identity.(AgentIdentity); isAgent {
		return agentIdent.ID() == agent.ID
	}
	if r.s.authzService == nil {
		return false
	}
	return r.s.authzService.CheckAccess(r.ctx, r.identity, agentResource(agent), ActionRead).Allowed
}

// resolveCreatorUser formats a user creator if the caller is allowed to read it.
func (r *execRevealer) resolveCreatorUser(user *store.User) resolvedScheduleCreator {
	if user == nil || !r.canReadCreatorUser(user.ID) {
		return resolvedScheduleCreator{}
	}
	name := user.DisplayName
	if name == "" {
		name = user.Email
	}
	if name == "" {
		return resolvedScheduleCreator{}
	}
	return resolvedScheduleCreator{name: name, kind: "user"}
}

// resolveCreatorAgent formats an agent creator if active and readable by the caller.
func (r *execRevealer) resolveCreatorAgent(agent *store.Agent) resolvedScheduleCreator {
	if agent == nil || !agent.DeletedAt.IsZero() || !r.canReadCreatorAgent(agent) {
		return resolvedScheduleCreator{}
	}
	name := agent.Slug
	if name == "" {
		name = agent.Name
	}
	if name == "" {
		return resolvedScheduleCreator{}
	}
	return resolvedScheduleCreator{name: name, kind: "agent"}
}

// preloadCreators batch-resolves all unique schedule CreatedBy IDs in at most
// two store lookups (GetUsersByIDs, then GetAgentsByIDs for remaining IDs) and
// populates r.creators so scheduleResponse does not issue per-row queries.
func (r *execRevealer) preloadCreators(schedules []store.Schedule) {
	if r == nil || r.s == nil || r.s.store == nil || r.identity == nil || len(schedules) == 0 {
		return
	}
	if r.creators == nil {
		r.creators = make(map[string]resolvedScheduleCreator, len(schedules))
	}
	seen := make(map[string]struct{}, len(schedules))
	ids := make([]string, 0, len(schedules))
	for i := range schedules {
		id := schedules[i].CreatedBy
		if id == "" {
			continue
		}
		if _, cached := r.creators[id]; cached {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}

	users, err := r.s.store.GetUsersByIDs(r.ctx, ids)
	if err != nil {
		slog.WarnContext(r.ctx, "schedule response: batch user lookup failed", "error", err)
	}

	remaining := make([]string, 0, len(ids))
	for _, id := range ids {
		if user, ok := users[id]; ok && user != nil {
			r.creators[id] = r.resolveCreatorUser(user)
		} else {
			remaining = append(remaining, id)
		}
	}
	if len(remaining) == 0 {
		return
	}

	agents, err := r.s.store.GetAgentsByIDs(r.ctx, remaining)
	if err != nil {
		slog.WarnContext(r.ctx, "schedule response: batch agent lookup failed", "error", err)
	}
	for _, id := range remaining {
		if agent, ok := agents[id]; ok && agent != nil {
			r.creators[id] = r.resolveCreatorAgent(agent)
		} else {
			r.creators[id] = resolvedScheduleCreator{}
		}
	}
}

// resolveCreator resolves a schedule's CreatedBy UUID to a readable name and
// principal kind ("user" or "agent") without widening caller visibility.
func (r *execRevealer) resolveCreator(createdBy string) (string, string) {
	if r == nil || r.s == nil || r.s.store == nil || r.identity == nil || createdBy == "" {
		return "", ""
	}
	if r.creators != nil {
		if cached, ok := r.creators[createdBy]; ok {
			return cached.name, cached.kind
		}
	}
	var resolved resolvedScheduleCreator
	if user, err := r.s.store.GetUser(r.ctx, createdBy); err == nil && user != nil {
		resolved = r.resolveCreatorUser(user)
	} else if agent, err := r.s.store.GetAgent(r.ctx, createdBy); err == nil && agent != nil {
		resolved = r.resolveCreatorAgent(agent)
	}
	if r.creators != nil {
		r.creators[createdBy] = resolved
	}
	return resolved.name, resolved.kind
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

// payloadHasExecContent reports whether raw carries exec payload fields
// ("command" or "timeoutSeconds", matched case-insensitively like
// encoding/json), even on a row whose claimed eventType is not "exec".
// Truncated/malformed JSON containing either key also matches so redaction
// fails closed.
func payloadHasExecContent(raw string) bool {
	if raw == "" {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		for k := range obj {
			if strings.EqualFold(k, "command") || strings.EqualFold(k, "timeoutSeconds") {
				return true
			}
		}
		return false
	}
	lower := strings.ToLower(raw)
	return strings.Contains(lower, `"command"`) || strings.Contains(lower, `"timeoutseconds"`)
}

// payloadFor returns the payload of a row of eventType a caller sees.
// Any row whose eventType is exec OR whose stored payload carries exec
// content is redacted fail-closed unless the caller holds live agent.attach
// on the stored target.
func (r *execRevealer) payloadFor(eventType, raw string) string {
	if eventType != scheduledEventTypeExec && !payloadHasExecContent(raw) {
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
		if events[i].EventType == scheduledEventTypeExec || payloadHasExecContent(events[i].Payload) {
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
			Outcome:      run.Outcome,
			ExitCode:     run.ExitCode,
			DurationMS:   run.DurationMS,
			OutputBytes:  run.OutputBytes,
			CaptureMode:  run.CaptureMode,
			OutputStatus: run.OutputStatus,
			StartedAt:    run.StartedAt,
			FinishedAt:   run.FinishedAt,
		}
	}
	return resp
}

// scheduleResponses serializes schedules for the caller of rev.
func scheduleResponses(rev *execRevealer, schedules []store.Schedule) []ScheduleResponse {
	rev.preloadCreators(schedules)
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
	createdByName, createdByType := rev.resolveCreator(sc.CreatedBy)
	return ScheduleResponse{
		Schedule:      sc,
		Payload:       payload,
		CreatedByName: createdByName,
		CreatedByType: createdByType,
	}
}
