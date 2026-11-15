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

// FireTracker coordinates fires that outlive their caller's context, shutdown
// admission, and once-only outcome persistence.
type FireTracker interface {
	// Detach returns a context that survives the caller's deadline while still
	// observing hub shutdown.
	Detach(ctx context.Context) (context.Context, context.CancelFunc)
	// Admit registers a long-running fire with the hub's shutdown tracking;
	// returns false when shutdown has begun.
	Admit(ctx context.Context) (context.Context, func(), bool)
	// Record executes write at most once across normal completion and abandon
	// paths.
	Record(write func()) bool
}

// FireTriggerRef identifies the trigger rule that initiated a FireEvent.
type FireTriggerRef struct {
	ID   string
	Name string
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
	Payload    []byte          // bounded raw JSON or nil
	Trigger    *FireTriggerRef // populated when fired by a trigger
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
	Status    FireStatus
	Error     string // generic text for the history row; "" when fired
	ResultRef string // message id, created agent id, or exec run id
	// AfterRecord runs after the caller has persisted the fire outcome.
	AfterRecord func(ctx context.Context)
	// Detached indicates that the fire's outcome will be recorded later by its
	// tracker (for example, wake and recurring exec fires on the once-write
	// path); the caller must not record the outcome immediately.
	Detached bool
}
