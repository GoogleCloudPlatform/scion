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

package store

import (
	"context"
	"encoding/json"
	"time"
)

// HubInstance is one hub process's row in the hub-instance registry (table
// hub_instances, health dashboard F3 design §5.1). Each hub replica writes
// only its own row; the health summary lists them all.
type HubInstance struct {
	// ID is the hub process's Server.InstanceID().
	ID string
	// Label is a short display name (pod name, Cloud Run revision or host
	// name). Display only, never a key.
	Label string
	// Version is the hub binary's short version.
	Version string
	// StartedAt is the store clock at the row's first insert. Set by the
	// store; ignored by UpsertHubInstance.
	StartedAt time.Time
	// LastSeen is the store clock of the last write. Set by the store;
	// ignored by UpsertHubInstance.
	LastSeen time.Time
	// StoppedAt is set when the process stopped cleanly; nil otherwise.
	StoppedAt *time.Time
	// Status is healthy, degraded or unhealthy.
	Status string
	// Checks is the instance's normalised check map (fixed values only).
	Checks map[string]string
	// Stats holds bounded per-instance figures as JSON. Empty (nil) until a
	// writer fills it.
	Stats json.RawMessage
}

// HubInstanceStore persists the hub-instance registry. Every method reads
// the store clock (Postgres "SELECT now()", SQLite Go clock) and binds it
// as a parameter; SQL never does time arithmetic, so the same code runs on
// both dialects.
type HubInstanceStore interface {
	// UpsertHubInstance writes the full row for in.ID. last_seen is set to
	// the store clock. On insert, started_at is also the store clock. On
	// conflict, every field except started_at is replaced and stopped_at is
	// cleared. in.StartedAt, in.LastSeen and in.StoppedAt are ignored.
	UpsertHubInstance(ctx context.Context, in HubInstance) error

	// TouchHubInstance sets last_seen to the store clock on the row for id.
	// found is false (with a nil error) when the row does not exist, so the
	// caller can upsert instead.
	TouchHubInstance(ctx context.Context, id string) (found bool, err error)

	// ListHubInstances reads the store clock once and returns the rows
	// whose last write (stopped_at when set, otherwise last_seen) is at or
	// after now - window, together with that now. The caller computes
	// state and ages against the same clock, so the cut, the states and
	// the ages never mix the database clock with a hub's local clock. Rows
	// are returned in ID order.
	ListHubInstances(ctx context.Context, window time.Duration) (rows []HubInstance, now time.Time, err error)
}
