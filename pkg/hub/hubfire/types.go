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

// Package hubfire holds the source-neutral data types shared between package
// hub's fire core and package hub/trigger without either importing the other.
// It contains types only and never imports package hub.
package hubfire

import (
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/principal"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DefinitionKind values identify the deferred-execution definition that
// authorized a fire, for audit and AuthorityProvenance records.
const (
	DefinitionKindSchedule       = "schedule"
	DefinitionKindScheduledEvent = "scheduled_event"
	DefinitionKindTrigger        = "trigger"
)

// RecordedAuthority records who authorized a deferred fire, under which
// authorization revision, and with which frozen effect ceiling. Both
// schedules/scheduled events and triggers carry these fields.
type RecordedAuthority struct {
	Attribution store.InitiatorAttribution
	Ceiling     store.EffectCeiling
	// Provenance of the definition, for audit and for AuthorityProvenance rows
	// written by authority-producing actions (dispatch_agent).
	DefinitionKind string // "schedule" | "scheduled_event" | "trigger"
	DefinitionID   string
	FireID         string // scheduled event id, or trigger_fires.id
}

// ExecutorContext aliases principal.ExecutorContext so package hub and package
// hub/trigger share the identical executor attribution type without conversion.
type ExecutorContext = principal.ExecutorContext

// FireTracker coordinates fires that outlive their caller's context and
// shutdown admission.
type FireTracker interface {
	// Detach returns a context that survives the caller's deadline while still
	// observing hub shutdown.
	Detach(ctx context.Context) (context.Context, context.CancelFunc)
	// Admit registers a long-running fire with the hub's shutdown tracking;
	// returns false when shutdown has begun.
	Admit(ctx context.Context) (context.Context, func(), bool)
}

// FireTriggerRef identifies the trigger rule that initiated a FireEvent.
type FireTriggerRef struct {
	ID          string
	Name        string
	SubjectMode string // "new" | "skip" (Stage 2 §5.3, §5.6)
}

// FireScheduleRef identifies the schedule and firing event for a schedule.fired
// FireEvent (§8.4).
type FireScheduleRef struct {
	ID      string
	Name    string
	EventID string
	FireAt  time.Time
}

// FireEvent is the normalised event envelope as seen by the fire core.
type FireEvent struct {
	ID         string
	ProjectID  string
	SourceKind string
	SourceID   string
	Type       string
	Subject    string
	OccurredAt time.Time
	ReceivedAt time.Time
	Attributes map[string]string
	Lists      map[string][]string
	Payload    []byte           // bounded raw JSON or nil
	Trigger    *FireTriggerRef  // populated when fired by a trigger
	Schedule   *FireScheduleRef // populated when fired by a schedule (§8.4)
}

// FireRequest carries everything a fire-core Action needs from an adapter
// (scheduler or trigger dispatcher).
type FireRequest struct {
	ProjectID  string
	Authority  RecordedAuthority
	ActionType string
	Payload    string // the action's JSON payload, as stored
	Event      *FireEvent
	Executor   ExecutorContext
	FireAt     time.Time // nominal fire/receipt time for staleness checks
	Attempts   int       // 1-based delivery attempt count (>1 enables marker-based retry deduplication)
	Tracked    FireTracker
}

// FireStatus is the terminal outcome classification of a fire.
type FireStatus string

const (
	FireStatusFired   FireStatus = "fired"
	FireStatusFailed  FireStatus = "failed"
	FireStatusRefused FireStatus = "refused"
	FireStatusSkipped FireStatus = "skipped"
)

// String returns the string form of s.
func (s FireStatus) String() string {
	return string(s)
}

// FireOutcome is the result returned by the fire core for a single FireRequest.
type FireOutcome struct {
	Status        FireStatus
	Error         string // generic text for the history row; "" when fired
	ResultRef     string // message id, created agent id, or exec run id
	TargetAgentID string // optional created/targeted agent UUID (trigger_fires.target_agent_id)
	// AfterRecord runs after the caller has persisted the fire outcome.
	AfterRecord func(ctx context.Context)
	// Detached indicates that the action owns the fire's terminal outcome write
	// (for example, an admitted scheduled exec run recorded by its tracker or
	// sweeper); the caller must not record the outcome immediately. A detaching
	// action that returns Detached: true must own the terminal write itself: in
	// runScheduleFire, claiming detachedFireRun with a no-op means a provisional
	// firing event is not recovered by the shutdown abandon sweep.
	Detached bool
}
