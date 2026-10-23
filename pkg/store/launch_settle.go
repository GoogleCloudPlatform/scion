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
	"time"
)

// LaunchSettleFence is the set of facts SettleLaunchedRun re-checks in its
// conditional write (WP1 slice 3b, ptone/scion#5927). Every field is
// compared against the row in the same statement that swaps the run, so a
// newer run, launch, broker or generation that landed after the caller's
// read refuses the settle.
type LaunchSettleFence struct {
	// LaunchID is the report's launch: the row's launch_id the report
	// matched.
	LaunchID string
	// FromRunID is the run the launch dispatched (the row's
	// launch_run_id), which must still be the row's run.
	FromRunID string
	// ToRunID is the run the broker reports.
	ToRunID string
	// BrokerID is the authenticated reporting broker, taken from the
	// request's verified identity and never from the row (a broker
	// predicate read from the row would match itself).
	BrokerID string
	// Generation is the row's generation as the report's transaction saw
	// it.
	Generation int
	// UnchangedSince is the row's run_id_updated_at as the report's
	// transaction saw it; the swap is refused when the run changed later.
	UnchangedSince time.Time
}

type runAdoptionSourceKey struct{}

// WithRunAdoptionSource returns ctx recording source as the
// adoption_source of an AdoptAgentRunID swap made with it. Only the
// launch-report settle sets it (AdoptionSourceCAS), on a context derived
// for that one call.
func WithRunAdoptionSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, runAdoptionSourceKey{}, source)
}

// RunAdoptionSourceFrom returns the source recorded by
// WithRunAdoptionSource, or AdoptionSourceHeartbeat when there is none (the
// heartbeat stranded-run adoption calls AdoptAgentRunID without it) or the
// recorded value is not one of the closed set of adoption sources, so a
// contract-invalid adoption_source can never reach the wire.
func RunAdoptionSourceFrom(ctx context.Context) string {
	switch s, _ := ctx.Value(runAdoptionSourceKey{}).(string); s {
	case AdoptionSourceCAS, AdoptionSourceHeartbeat:
		return s
	}
	return AdoptionSourceHeartbeat
}
