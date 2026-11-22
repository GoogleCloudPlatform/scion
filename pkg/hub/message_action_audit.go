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
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

// Core kind-neutral message-action audit event types (C3). Kind-specific event
// types (such as "fulfilled" or "claim_recovered") are registered by each kind
// in its msgaction.KindSpec.EventTypes.
const (
	MessageActionEventRequested = msgaction.EventRequested
	MessageActionEventReplaced  = msgaction.EventReplaced
	MessageActionEventWithdrawn = msgaction.EventWithdrawn
	MessageActionEventResolved  = msgaction.EventResolved
	MessageActionEventDismissed = msgaction.EventDismissed
	MessageActionEventExpired   = msgaction.EventExpired
	MessageActionEventRefused   = msgaction.EventRefused
)

// Core actor kinds of a MessageActionEvent. ActorKind is open-ended (C3) so
// future kinds may record additional actor kinds (e.g. an external executor).
const (
	MessageActionActorAgent = "agent"
	MessageActionActorUser  = "user"
	MessageActionActorHub   = "hub"
)

// MessageActionEvent is the kind-agnostic audit envelope emitted on message
// action lifecycle transitions and refused attempts. It carries metadata only
// and never secret material or sensitive payload values.
type MessageActionEvent struct {
	Kind          string            `json:"kind"`
	EventType     string            `json:"eventType"`
	RowID         string            `json:"rowId,omitempty"`
	MessageID     string            `json:"messageId,omitempty"`
	ProjectID     string            `json:"projectId,omitempty"`
	AgentID       string            `json:"agentId,omitempty"`
	AgentName     string            `json:"agentName,omitempty"`
	Key           string            `json:"key,omitempty"`
	Version       int               `json:"version,omitempty"`
	ReplacesRowID string            `json:"replacesRowId,omitempty"`
	ActorKind     string            `json:"actorKind,omitempty"`
	ActorID       string            `json:"actorId,omitempty"`
	Surface       string            `json:"surface,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	Details       map[string]string `json:"details,omitempty"`
	Timestamp     time.Time         `json:"timestamp"`
}

// filterMessageActionDetails returns a copy of details containing only keys
// allow-listed by spec.DetailKeys. Fails closed: returns nil when hasSpec is
// false or spec.DetailKeys is empty.
func filterMessageActionDetails(spec msgaction.KindSpec, hasSpec bool, details map[string]string) map[string]string {
	if !hasSpec || len(spec.DetailKeys) == 0 || len(details) == 0 {
		return nil
	}
	var filtered map[string]string
	for k, v := range details {
		if !spec.AcceptsDetailKey(k) {
			continue
		}
		if filtered == nil {
			filtered = make(map[string]string)
		}
		filtered[k] = v
	}
	return filtered
}

// LogMessageActionEvent logs a message-action lifecycle or refusal audit event
// as structured slog attributes.
//
// Enforcement is fail-closed against the msgaction.KindSpec registry:
//   - Details are filtered strictly through spec.DetailKeys; an unregistered
//     kind or empty DetailKeys drops all Details so values can never reach logs.
//   - EventType is validated against spec.AcceptsEventType (or the core event
//     types when the kind is unregistered). An unregistered event type is
//     rejected: a WARN refusal record is emitted without any Details and an
//     error is returned.
func (l *LogAuditLogger) LogMessageActionEvent(ctx context.Context, event *MessageActionEvent) error {
	if l == nil || event == nil {
		return nil
	}
	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	spec, hasSpec := msgaction.LookupKind(event.Kind)
	if !spec.AcceptsEventType(event.EventType) {
		attrs := []slog.Attr{
			slog.String("kind", event.Kind),
			slog.String("event_type", event.EventType),
			slog.String("row_id", event.RowID),
			slog.String("message_id", event.MessageID),
			slog.String("project_id", event.ProjectID),
			slog.String("agent_id", event.AgentID),
			slog.String("key", event.Key),
			slog.Int("version", event.Version),
			slog.String("actor_kind", event.ActorKind),
			slog.String("actor_id", event.ActorID),
			slog.String("reason", "unregistered_event_type"),
			slog.Time("timestamp", ts),
		}
		if credAttr, ok := credentialLogAttr(ctx); ok {
			attrs = append(attrs, credAttr)
		}
		l.logger().LogAttrs(ctx, slog.LevelWarn, "message action event rejected", attrs...)
		return fmt.Errorf("message action event rejected: unregistered event_type %q for kind %q", event.EventType, event.Kind)
	}

	level := slog.LevelInfo
	if event.EventType == MessageActionEventRefused || !hasSpec {
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("kind", event.Kind),
		slog.String("event_type", event.EventType),
		slog.String("row_id", event.RowID),
		slog.String("message_id", event.MessageID),
		slog.String("project_id", event.ProjectID),
		slog.String("agent_id", event.AgentID),
		slog.String("agent_name", event.AgentName),
		slog.String("key", event.Key),
		slog.Int("version", event.Version),
		slog.String("actor_kind", event.ActorKind),
		slog.String("actor_id", event.ActorID),
		slog.Time("timestamp", ts),
	}
	if !hasSpec {
		attrs = append(attrs, slog.Bool("unregistered_kind", true))
	}
	if event.ReplacesRowID != "" {
		attrs = append(attrs, slog.String("replaces_row_id", event.ReplacesRowID))
	}
	if event.Surface != "" {
		attrs = append(attrs, slog.String("surface", event.Surface))
	}
	if event.Reason != "" {
		attrs = append(attrs, slog.String("reason", event.Reason))
	}
	if allowedDetails := filterMessageActionDetails(spec, hasSpec, event.Details); len(allowedDetails) > 0 {
		keys := make([]string, 0, len(allowedDetails))
		for k := range allowedDetails {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			attrs = append(attrs, slog.String("detail_"+k, allowedDetails[k]))
		}
	}
	if credAttr, ok := credentialLogAttr(ctx); ok {
		attrs = append(attrs, credAttr)
	}

	l.logger().LogAttrs(ctx, level, "message action event", attrs...)
	return nil
}

// emitMessageActionEvent filters ev.Details fail-closed through the kind's
// registered msgaction.KindSpec before emitting through the server's audit
// logger. Safe to call with nil server, nil logger, or nil event.
//
//nolint:unused // Consumed by P5 secret requests; decisions do not emit MessageActionEvent in P5a.
func (s *Server) emitMessageActionEvent(ctx context.Context, ev *MessageActionEvent) {
	if s == nil || s.auditLogger == nil || ev == nil {
		return
	}
	spec, hasSpec := msgaction.LookupKind(ev.Kind)
	sanitized := *ev
	sanitized.Details = filterMessageActionDetails(spec, hasSpec, ev.Details)
	_ = s.auditLogger.LogMessageActionEvent(ctx, &sanitized)
}
