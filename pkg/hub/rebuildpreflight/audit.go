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

package rebuildpreflight

import (
	"context"
	"fmt"
	"time"
)

// MaintenanceOverrideEventType is the audit event type emitted when a
// forced rebuild-server run overrides blocking preflight findings (S4).
const MaintenanceOverrideEventType = "maintenance_preflight_override"

// MaintenanceOverrideEvent is the audit event emitted when an admin forces
// a rebuild-server run past blocking preflight findings or a could-not-run
// outcome (S4-S6). It carries no finding text, subjects, or profile names,
// and is self-contained even if maintenance run rows are later pruned.
//
// Authority (S6): the hub.audit log stream is an index and may be
// incomplete; the maintenance run rows (RunID and RerunOf) are
// authoritative. Every emitted record carries record="index" and
// authoritative="maintenance run rows <RunID>, <RerunOf>" from Authority().
type MaintenanceOverrideEvent struct {
	EventType        string    `json:"eventType"`
	ActorID          string    `json:"actorId"`
	ActorEmail       string    `json:"actorEmail"`
	Timestamp        time.Time `json:"timestamp"`
	RunID            string    `json:"runId"`
	RerunOf          string    `json:"rerunOf"`
	Operation        string    `json:"operation"`
	Commit           string    `json:"commit"`
	Branch           string    `json:"branch"`
	Blocking         int       `json:"blocking"`
	CouldNotRunKind  string    `json:"couldNotRunKind,omitempty"`
	CouldNotRunClass string    `json:"couldNotRunClass,omitempty"`
}

// Authority returns the S6 record-role and authoritative-source fields for
// this event. Callers must always emit these from Authority(), never from
// call-site literals.
func (ev *MaintenanceOverrideEvent) Authority() (record, authoritative string) {
	if ev == nil {
		return "index", "maintenance run rows , "
	}
	return "index", fmt.Sprintf("maintenance run rows %s, %s", ev.RunID, ev.RerunOf)
}

// OverrideSink is the one-method audit interface satisfied by pkg/hub's
// AuditLogger so pkg/hub/maintenance can emit override events without
// importing pkg/hub.
type OverrideSink interface {
	LogMaintenanceOverrideEvent(ctx context.Context, event *MaintenanceOverrideEvent) error
}

// EmitOverride sends ev to sink (S4-S6). Emission is fail-open (S5): a nil
// sink (CN5) or an error from LogMaintenanceOverrideEvent logs at Error via
// logErr with every event field so the audit gap can be backfilled.
func EmitOverride(ctx context.Context, sink OverrideSink, ev *MaintenanceOverrideEvent, logErr func(msg string, args ...any)) {
	if ev == nil {
		return
	}
	if ev.EventType == "" {
		ev.EventType = MaintenanceOverrideEventType
	}
	if ev.Operation == "" {
		ev.Operation = RebuildServerKey
	}
	record, authoritative := ev.Authority()
	logFailure := func(errVal any) {
		if logErr == nil {
			return
		}
		logErr("maintenance override audit emit failed",
			"error", errVal,
			"event_type", ev.EventType,
			"actor_id", ev.ActorID,
			"actor_email", ev.ActorEmail,
			"timestamp", ev.Timestamp.UTC().Format(time.RFC3339),
			"run_id", ev.RunID,
			"rerun_of", ev.RerunOf,
			"operation", ev.Operation,
			"commit", ev.Commit,
			"branch", ev.Branch,
			"blocking", ev.Blocking,
			"could_not_run_kind", ev.CouldNotRunKind,
			"could_not_run_class", ev.CouldNotRunClass,
			"record", record,
			"authoritative", authoritative,
		)
	}
	if sink == nil {
		logFailure("audit sink is nil")
		return
	}
	if err := sink.LogMaintenanceOverrideEvent(ctx, ev); err != nil {
		logFailure(err)
	}
}
