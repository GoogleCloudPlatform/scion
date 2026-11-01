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

package lifecycleevents

import "context"

// CreateContext is the creating principal of an agent create. It is not on
// the agent row; the hub attaches it to the create transaction's context
// (WP1 design 2.1) and the observer records it on created and in the
// cursor, from which later snapshots copy it.
type CreateContext struct {
	CreatorType string // "user" | "agent" | ...
	CreatorID   string
}

type createContextKey struct{}

// WithCreateContext attaches the create's principal to ctx.
func WithCreateContext(ctx context.Context, c CreateContext) context.Context {
	return context.WithValue(ctx, createContextKey{}, c)
}

// CreateContextFrom returns the create principal attached to ctx.
func CreateContextFrom(ctx context.Context) (CreateContext, bool) {
	c, ok := ctx.Value(createContextKey{}).(CreateContext)
	return c, ok
}

// Create-failure causes (WP1 design 2.3, 2.4a steps 1 and 3).
const (
	// CauseCreateRolledBack: a failed create removes its row; the removal
	// emits launch_failed(create_rolled_back) and deleted(create_rolled_back).
	CauseCreateRolledBack = "create_rolled_back"
	// CauseCreateFailed: a failed create that keeps its row.
	CauseCreateFailed = "create_failed"
)

// Cause annotates a write with why it happened, for writes whose row
// change alone does not say (a create rollback looks like a hard delete).
type Cause struct {
	Kind  string // CauseCreateRolledBack | CauseCreateFailed
	Stage string // the failed create stage, emitted as error_code
}

type causeKey struct{}

// WithCause attaches a create-failure cause to ctx.
func WithCause(ctx context.Context, kind, stage string) context.Context {
	return context.WithValue(ctx, causeKey{}, Cause{Kind: kind, Stage: stage})
}

// CauseFrom returns the cause attached to ctx.
func CauseFrom(ctx context.Context) (Cause, bool) {
	c, ok := ctx.Value(causeKey{}).(Cause)
	return c, ok && c.Kind != ""
}

// DispatchResult is the sync dispatch result a dispatch-result write
// carries (WP1 design 2.4a E(iv), r7 N1/N2): the run the Hub dispatched,
// and what the broker's answer said about it.
type DispatchResult struct {
	// RunID is the run the dispatch result belongs to (the row's run
	// after adoptBrokerRunID). The evidence holds only while the row
	// still records it.
	RunID string
	// BrokerEvidence: the broker's answer named RunID and reported
	// running (brokerNamedRun && brokerPhaseRunning).
	BrokerEvidence bool
	// NoBrokerRun: the broker's answer named no run (a legacy broker) and
	// itself reported running (P1b).
	NoBrokerRun bool
	// BrokerNamedRun: the broker's answer named RunID, whatever its phase.
	BrokerNamedRun bool
}

// NewDispatchResult maps a synchronous broker answer to the evidence a
// dispatch-result write carries (design 2.4a E(iv), r7 N1/N2, F2 ruling
// 22:06): rowRunID is the row's run after adoptBrokerRunID, brokerRunID
// the run the answer named ("" for a legacy broker), brokerRunning whether
// the answer itself reported running.
//   - named run and running: E(iv) (BrokerEvidence);
//   - named run, not running: correlated but not a start;
//   - no run and running: a legacy running observation (P1b);
//   - no run and not running (a Hub-forced running on env submit), or a
//     different run (a failed CAS, 10b item 12): no evidence at all.
func NewDispatchResult(rowRunID, brokerRunID string, brokerRunning bool) DispatchResult {
	named := brokerRunID != "" && brokerRunID == rowRunID
	return DispatchResult{
		RunID:          rowRunID,
		BrokerNamedRun: named,
		BrokerEvidence: named && brokerRunning,
		NoBrokerRun:    brokerRunID == "" && brokerRunning,
	}
}

type dispatchResultKey struct{}

// WithDispatchResult marks the writes made with ctx as the sync dispatch
// result r. Only an UpdateAgent write honours it.
func WithDispatchResult(ctx context.Context, r DispatchResult) context.Context {
	return context.WithValue(ctx, dispatchResultKey{}, r)
}

// DispatchResultFrom returns the dispatch result attached to ctx.
func DispatchResultFrom(ctx context.Context) (DispatchResult, bool) {
	r, ok := ctx.Value(dispatchResultKey{}).(DispatchResult)
	return r, ok
}

type configResolutionKey struct{}

// WithConfigResolution marks the writes made with ctx as config-resolution
// writes: a configuration change they commit is configured, not
// metadata_changed (persistAcceptedLaunch, the reincarnation reprovision
// step). The dispatch result and launch reports are config resolution
// without this marker.
func WithConfigResolution(ctx context.Context) context.Context {
	return context.WithValue(ctx, configResolutionKey{}, true)
}

// ConfigResolutionFrom reports whether ctx carries WithConfigResolution.
func ConfigResolutionFrom(ctx context.Context) bool {
	v, _ := ctx.Value(configResolutionKey{}).(bool)
	return v
}

// WithoutResolution returns ctx with any WithConfigResolution and
// WithDispatchResult marks masked, for a writer whose writes are never
// config resolution whatever its caller's context carries (UpdateAgentConfig:
// held config edits are requested values, so a change they commit is
// metadata_changed, never configured).
func WithoutResolution(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, configResolutionKey{}, false)
	return context.WithValue(ctx, dispatchResultKey{}, nil)
}

// Start-outcome annotations of a status write (design 2.4a step 3).
const (
	// StartOutcomeRestartFailed: recordRestartStopped, after a confirmed
	// stop leg whose start leg failed.
	StartOutcomeRestartFailed = "restart_failed"
	// StartOutcomeUnconfirmed: the start rollback of a start whose outcome
	// is unknown (keepForUnknownOutcome).
	StartOutcomeUnconfirmed = "unconfirmed"
)

type startOutcomeKey struct{}

// WithStartOutcome annotates the writes made with ctx with a start outcome
// (StartOutcomeRestartFailed or StartOutcomeUnconfirmed).
func WithStartOutcome(ctx context.Context, outcome string) context.Context {
	return context.WithValue(ctx, startOutcomeKey{}, outcome)
}

// StartOutcomeFrom returns the start outcome attached to ctx, or "".
func StartOutcomeFrom(ctx context.Context) string {
	v, _ := ctx.Value(startOutcomeKey{}).(string)
	return v
}
