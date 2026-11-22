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
	"log/slog"
	"sort"
	"sync"
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

// messageActionDetailAllowLists stores per-kind allowed Details keys registered
// on a LogAuditLogger (C3). Keys outside a kind's registered allow-list are
// never emitted into structured logs.
type messageActionDetailAllowLists struct {
	mu     sync.RWMutex
	byKind map[string]map[string]bool
}

func (a *messageActionDetailAllowLists) register(kind string, keys ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byKind == nil {
		a.byKind = make(map[string]map[string]bool)
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k != "" {
			set[k] = true
		}
	}
	a.byKind[kind] = set
}

func (a *messageActionDetailAllowLists) allowListFor(kind string) (map[string]bool, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.byKind == nil {
		return nil, false
	}
	set, ok := a.byKind[kind]
	return set, ok
}

// RegisterMessageActionDetailKeys registers the per-kind allow-list of Details
// keys that LogMessageActionEvent may emit for kind (C3). When registered for
// a kind, any Details key not in keys is silently dropped before logging so
// sensitive values cannot reach logs.
func (l *LogAuditLogger) RegisterMessageActionDetailKeys(kind string, keys ...string) {
	if l == nil || kind == "" {
		return
	}
	l.messageActionDetails.register(kind, keys...)
}

// LogMessageActionEvent logs a message-action lifecycle or refusal audit event
// as structured slog attributes. Refused attempts are logged at warning level.
func (l *LogAuditLogger) LogMessageActionEvent(ctx context.Context, event *MessageActionEvent) error {
	if l == nil || event == nil {
		return nil
	}
	level := slog.LevelInfo
	if event.EventType == MessageActionEventRefused {
		level = slog.LevelWarn
	}
	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
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
	if event.ReplacesRowID != "" {
		attrs = append(attrs, slog.String("replaces_row_id", event.ReplacesRowID))
	}
	if event.Surface != "" {
		attrs = append(attrs, slog.String("surface", event.Surface))
	}
	if event.Reason != "" {
		attrs = append(attrs, slog.String("reason", event.Reason))
	}
	if len(event.Details) > 0 {
		allow, hasAllow := l.messageActionDetails.allowListFor(event.Kind)
		keys := make([]string, 0, len(event.Details))
		for k := range event.Details {
			if hasAllow && !allow[k] {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			attrs = append(attrs, slog.String("detail_"+k, event.Details[k]))
		}
	}
	if credAttr, ok := credentialLogAttr(ctx); ok {
		attrs = append(attrs, credAttr)
	}

	l.logger().LogAttrs(ctx, level, "message action event", attrs...)
	return nil
}

// emitMessageActionEvent emits ev through the server's audit logger when
// configured. Safe to call with nil server, nil logger, or nil event.
//
//nolint:unused // Consumed by P5 secret requests; decisions do not emit MessageActionEvent in P5a.
func (s *Server) emitMessageActionEvent(ctx context.Context, ev *MessageActionEvent) {
	if s == nil || s.auditLogger == nil || ev == nil {
		return
	}
	_ = s.auditLogger.LogMessageActionEvent(ctx, ev)
}
