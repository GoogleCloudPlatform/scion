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

package runtimebroker

import (
	"context"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// launchKey identifies one in-flight (or just-ended) launch for the
// registry, the same key stop and delete resolve by (resolveDeleteTarget,
// design t1-async-create-v11.md §3.8.1).
type launchKey struct {
	ProjectID string
	Slug      string
}

// launchRecord is one launch's local bookkeeping (design §3.8.1). It is an
// optimisation only: correctness comes from the Hub's answers to claim,
// checkpoint and keepalive reports, never from this record. Seq is written
// only by the launch's own runLaunch goroutine (and the sender it owns), so
// it needs no lock of its own. Handles is appended from the runtime's
// OnResourceCreated hook, which runs on Manager.Start's goroutine, so it is
// guarded by mu (AddHandle, HandlesSnapshot); OwnerHub can be read and
// written from the sender's fan-out routing and is also guarded by mu.
type launchRecord struct {
	ID       string
	AgentID  string
	Kind     string
	Deadline time.Time
	// RunID is the Hub-minted run this launch starts (ptone/scion#2550),
	// or "" when the request carried none. Set before Begin and never
	// changed, so it needs no lock.
	RunID string

	// Seq is the last report sequence number sent for this launch.
	Seq int64
	// Handles accumulates every runtime resource created during the launch
	// (design §3.8.4), reported by the runtime's OnResourceCreated hook, for
	// CleanupLaunch on an abort. Guarded by mu: use AddHandle and
	// HandlesSnapshot rather than the field once the launch has started.
	Handles []agent.ResourceHandle
	// HubName is the hub connection resolved at admission time (routing
	// rules 1-3, design §3.8.5); "" means the sender fans out and pins
	// OwnerHub itself (rule 4).
	HubName string

	mu       sync.Mutex
	ownerHub string // the connection whose answer the sender has pinned to (rule 4)

	cancel context.CancelFunc
	// done is closed once runLaunch (including any cleanup) has fully
	// finished. A later launch for the same key waits on this before
	// writing the shared marker (§3.8.2 step 5.2, F5).
	done chan struct{}
}

// OwnerHub returns the hub connection name the sender has pinned to for a
// fanned-out launch, or "" if none has been pinned yet.
func (rec *launchRecord) OwnerHub() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.ownerHub
}

// SetOwnerHub pins the fan-out answer to connName, if not already pinned
// (design §3.8.5 routing rule 4: "The first 2xx or 409 pins OwnerHub").
func (rec *launchRecord) SetOwnerHub(connName string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.ownerHub == "" {
		rec.ownerHub = connName
	}
}

// AddHandle records one runtime resource the launch created (the runtime's
// OnResourceCreated hook, design §3.8.4).
func (rec *launchRecord) AddHandle(h agent.ResourceHandle) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.Handles = append(rec.Handles, h)
}

// HandlesSnapshot returns a copy of the handles recorded so far.
func (rec *launchRecord) HandlesSnapshot() []agent.ResourceHandle {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.Handles) == 0 {
		return nil
	}
	return append([]agent.ResourceHandle(nil), rec.Handles...)
}

// CancelLocal wakes any local wait on this launch (e.g. a blocked
// claim/checkpoint, or the keepalive loop) without affecting Hub state.
func (rec *launchRecord) CancelLocal() {
	if rec.cancel != nil {
		rec.cancel()
	}
}

// launchRegistry tracks local launch bookkeeping keyed by (projectID, slug)
// (design §3.8.1). It is a pure optimisation: a replica restart loses it
// with no correctness impact, because the Hub always resolves the launch's
// fate independently (the reaper, or the next report's answer).
type launchRegistry struct {
	mu      sync.Mutex
	records map[launchKey]*launchRecord
}

// newLaunchRegistry creates an empty registry.
func newLaunchRegistry() *launchRegistry {
	return &launchRegistry{records: make(map[launchKey]*launchRecord)}
}

// Begin registers rec under key. A record already held for key is replaced
// and locally cancelled -- "there is no 409" (design §3.8.1): correctness is
// the Hub's job, so a same-replica collision never fails the new launch
// locally. It returns the superseded record's done channel (nil if there was
// none), which the caller must wait on before writing the shared marker
// (design §3.8.2 step 5.2, F5) -- that wait is bounded by the superseded
// launch's own cleanup, which itself runs under a fresh 60s context, never
// by this call.
func (r *launchRegistry) Begin(key launchKey, rec *launchRecord) (supersededDone <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.records[key]
	r.records[key] = rec
	if !ok || old == nil {
		return nil
	}
	old.CancelLocal()
	return old.done
}

// WaitSuperseded blocks until supersededDone closes or ctx is done, matching
// Begin's contract: the wait is bounded by the superseded launch's own
// cleanup context, with ctx (the new launch's ctx') as a backstop so a stuck
// cleanup can never block the new launch past its own deadline.
func WaitSuperseded(ctx context.Context, supersededDone <-chan struct{}) {
	if supersededDone == nil {
		return
	}
	select {
	case <-supersededDone:
	case <-ctx.Done():
	}
}

// CancelLocal wakes the record held for key, if any (design §3.8.1: stop and
// delete on the same replica call this to wake long waits).
func (r *launchRegistry) CancelLocal(key launchKey) {
	if r == nil {
		// A *Server built directly (e.g. by a test that does not go through
		// New()) has a nil registry; treat it the same as "no launch held
		// for this key" rather than panicking.
		return
	}
	r.mu.Lock()
	rec := r.records[key]
	r.mu.Unlock()
	if rec != nil {
		rec.CancelLocal()
	}
}

// CancelLocalForRun is CancelLocal for a delete that names run runID
// (ptone/scion#2550): it leaves alone a launch of a different run, so a
// stale delete for an earlier run cannot cancel the start of the agent
// recreated under the same name. A launch or a delete without a run ID
// matches as before.
//
// It reports whether it woke a launch.
func (r *launchRegistry) CancelLocalForRun(key launchKey, runID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	rec := r.records[key]
	r.mu.Unlock()
	if rec == nil {
		return false
	}
	if runID != "" && rec.RunID != "" && rec.RunID != runID {
		return false
	}
	rec.CancelLocal()
	return true
}

// OtherRunInFlight reports whether a launch of a run other than runID is
// registered under key (ptone/scion#2675). Such a launch is provisioning, or
// about to provision, the agent's files under this name, so a delete naming
// runID must leave them alone even before the launch has recorded its run
// on disk. False without a run ID on either side, matching
// CancelLocalForRun.
func (r *launchRegistry) OtherRunInFlight(key launchKey, runID string) bool {
	_, ok := r.otherRunInFlightID(key, runID)
	return ok
}

// otherRunInFlightID is OtherRunInFlight that also returns the other run ID, for
// a run-scoped stop to report which run holds the name (ptone/scion#2550).
func (r *launchRegistry) otherRunInFlightID(key launchKey, runID string) (string, bool) {
	if r == nil || runID == "" {
		return "", false
	}
	r.mu.Lock()
	rec := r.records[key]
	r.mu.Unlock()
	if rec != nil && rec.RunID != "" && rec.RunID != runID {
		return rec.RunID, true
	}
	return "", false
}

// runInFlight reports whether the launch registered under key is of run
// runID exactly. False for an empty runID or a launch with no run ID.
func (r *launchRegistry) runInFlight(key launchKey, runID string) bool {
	if r == nil || runID == "" {
		return false
	}
	r.mu.Lock()
	rec := r.records[key]
	r.mu.Unlock()
	return rec != nil && rec.RunID == runID
}

// Finish closes rec's done channel and removes it from the registry if it is
// still the current record for key (a newer launch may already have
// replaced it via Begin, in which case removal here would wrongly drop the
// newer record). Callers defer this once runLaunch, including any cleanup,
// has fully finished.
func (r *launchRegistry) Finish(key launchKey, rec *launchRecord) {
	close(rec.done)
	r.mu.Lock()
	if r.records[key] == rec {
		delete(r.records, key)
	}
	r.mu.Unlock()
}

// newLaunchRecord builds a launchRecord with its cancel func and done
// channel wired up. cancel cancels ctx (the launch's ctx'), so any step
// still blocked on it (a claim/checkpoint retry, the keepalive loop)
// unblocks promptly on a local stop/delete or supersede.
func newLaunchRecord(id, agentID, kind, hubName string, deadline time.Time, cancel context.CancelFunc) *launchRecord {
	return &launchRecord{
		ID:       id,
		AgentID:  agentID,
		Kind:     kind,
		Deadline: deadline,
		HubName:  hubName,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
}
