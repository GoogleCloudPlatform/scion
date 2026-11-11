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
	"sync/atomic"
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
	// shutdown cancels the launch's ctx' with errBrokerShutdown as its
	// cause, so runLaunch reports broker_restarted instead of treating the
	// cancel as a local stop (design wave 3 §1 C-e). Nil for records that
	// are not async launches.
	shutdown func()

	// cleanupFailure is set to the class of the first clean-up step that
	// failed for this launch (failCleanup), so runLaunch keeps its journal
	// record for a retry (ptone/scion#5177).
	cleanupFailure atomic.Pointer[cleanupFailureClass]
	// filesCleanupFailed is set when deleting the agent's files failed
	// (files_lock or files_delete), whatever failed first, so runLaunch
	// keeps the launch marker the retry needs (ptone/scion#5177).
	filesCleanupFailed atomic.Bool
	// resourcesOnlyCleanup is set when the launch's clean-up was only its
	// recorded resources (a local stop or delete owns the files), so a
	// retry reruns only that.
	resourcesOnlyCleanup atomic.Bool
	// terminalUndelivered is set when the launch's terminal report got no
	// definitive hub answer within its TTL, so runLaunch keeps the journal
	// record (and the launch marker) for the next boot to resend it
	// (ptone/scion#5186).
	terminalUndelivered atomic.Bool

	// journal is the launch's journal entry (launch_journal.go), or nil
	// when the launch is not journalled. Its methods are nil-safe.
	journal *journalEntry

	// ownedRun is the flat instance's ownership key of this launch's run,
	// set before Manager.Start returns; launch cleanup finishes that run
	// after removing its objects (zero for a legacy Runtime Broker).
	ownedRunMu sync.Mutex
	ownedRun   ownedRunKey
	// filesRemoved is set by cleanupLaunchFiles when it removed the
	// agent's files and a flat instance's files proof then found none.
	filesRemoved atomic.Bool
	// done is closed once runLaunch (including any cleanup), or a create's
	// registration (syncStart.finish), has fully finished. A later create
	// for the same key waits on this before any name-addressed work
	// (§3.8.2 step 5.2, F5; beginCreateRegistration).
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
// (design §3.8.1). It is in memory only and a replica restart loses it.
// That has no correctness impact: the Hub resolves each launch's fate
// independently (the reaper, or the next report's answer). The launch
// journal (launch_journal.go, under the broker's state directory) is a
// separate, best-effort record: a restarted broker uses it to report each
// unfinished launch's outcome (failed, or succeeded when that was already
// decided) and to clean up what a failed launch created, never to resume a
// launch.
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
// none), which the caller must wait on before any name-addressed work
// (design §3.8.2 step 5.2, F5; beginCreateRegistration, which bounds the
// wait) -- the superseded launch's own cleanup runs under a fresh 60s
// context.
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

// TryBegin registers rec under key only if no record holds key, and reports
// whether it did. Unlike Begin it never replaces or cancels a record: boot
// recovery uses it for the short slot in which it deletes a dead launch's
// name-addressed resources, so it can never supersede or delay a newer
// start of the name that is already registered (design wave 3 §1 C-c step
// 5). A start that registers with Begin during the slot cancels rec and
// waits on its done channel as for any other record; release the slot with
// Finish.
func (r *launchRegistry) TryBegin(key launchKey, rec *launchRecord) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records[key] != nil {
		return false
	}
	r.records[key] = rec
	return true
}

// Handoff replaces from, the record currently held for key, with to,
// without cancelling either and without anything to wait on: to continues
// the same create under the name from has already waited for (an async
// launch taking over the create's registration, see syncStart.handOff). It
// reports false, changing nothing, if from is no longer the record held for
// key (a newer start replaced it) or fromCancelled reports true (a stop or
// delete cancelled it); both are checked under the registry lock.
func (r *launchRegistry) Handoff(key launchKey, from, to *launchRecord, fromCancelled func() bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records[key] != from {
		return false
	}
	if fromCancelled != nil && fromCancelled() {
		return false
	}
	r.records[key] = to
	return true
}

// CancelLocalFor wakes the launch or create registered under key for a stop
// or delete of the agent the hub knows as agentID, naming run runID (design
// §3.8.1: stop and delete on the same replica call this to wake long
// waits). It leaves alone a launch of a different agent (ptone/scion#2632:
// a late stop or delete for an old agent must not cancel the start of a
// newer agent recreated under the same name) or of a different run
// (ptone/scion#2550). Each match is skipped when either side lacks the
// value: a stop or delete from a hub that sends no agentId or runId, or a
// launch registered without one, matches by name as before.
//
// The cancel runs under the registry lock, as Begin's does, so it is
// ordered against Handoff: a cancel either reaches the create's
// registration before an async launch takes it over (and Handoff refuses)
// or reaches the launch's own record.
//
// It reports whether it woke a launch.
func (r *launchRegistry) CancelLocalFor(key launchKey, agentID, runID string) bool {
	return r.cancelLocalForExcept(key, agentID, runID, nil) != nil
}

// cancelLocalForExcept is CancelLocalFor that leaves skip alone (nil skips
// nothing) and returns the record it woke, or nil. A stop's second cancel,
// after its start tracker wait, passes the record its first cancel woke, so
// it reaches only a record registered in between: an async start that
// registered after the first cancel and handed over to its launch (design
// wave 3 §2 D-f).
func (r *launchRegistry) cancelLocalForExcept(key launchKey, agentID, runID string, skip *launchRecord) *launchRecord {
	if r == nil {
		// A *Server built directly (e.g. by a test that does not go through
		// New()) has a nil registry; treat it the same as "no launch held
		// for this key" rather than panicking.
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.records[key]
	if rec == nil || rec == skip {
		return nil
	}
	if runID != "" && rec.RunID != "" && rec.RunID != runID {
		return nil
	}
	if agentID != "" && rec.AgentID != "" && rec.AgentID != agentID {
		return nil
	}
	rec.CancelLocal()
	return rec
}

// OtherRunInFlight reports whether a launch of a run other than runID is
// registered under key (ptone/scion#2675). Such a launch is provisioning, or
// about to provision, the agent's files under this name, so a delete naming
// runID must leave them alone even before the launch has recorded its run
// on disk. False without a run ID on either side, matching CancelLocalFor's
// run match.
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

// anyInFlight reports whether any launch or create is registered under
// key, whatever its run or agent ID (a purge of retained files refuses
// then, ptone/scion#4744). False for a nil registry.
func (r *launchRegistry) anyInFlight(key launchKey) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records[key] != nil
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
