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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// The hub-instance registry writer (health dashboard F3 design §5.3). Each
// hub process writes its own hub_instances row from a dedicated goroutine,
// so the health summary on any replica can list every hub instance. The
// registry is display-only: nothing in the control plane reads it.

const (
	// hubInstanceTickInterval is the registry write cadence. A row with no
	// write for hubInstanceStaleAfter (3 ticks) is shown as stale.
	hubInstanceTickInterval = 15 * time.Second
	// hubInstanceTickJitter is the ± fraction applied to every interval, so
	// replicas started together do not write in lock step.
	hubInstanceTickJitter = 0.10
	// hubInstanceTickTimeout bounds one tick (checks plus the write).
	hubInstanceTickTimeout = 10 * time.Second
	// hubInstanceStartWait bounds how long startup waits for the first
	// tick before the listener starts serving anyway.
	hubInstanceStartWait = 5 * time.Second
	// hubInstanceForcedUpsertEvery forces a full upsert every Nth tick
	// (5 minutes at 15 s), repairing any drift between the row and what
	// the writer believes it last wrote.
	hubInstanceForcedUpsertEvery = 20
	// hubInstanceWarnEvery rate-limits the failed-tick warning.
	hubInstanceWarnEvery = 5 * time.Minute

	// hubInstanceMaxLabelBytes and hubInstanceMaxVersionBytes cap the
	// display fields (F3 design §5.1).
	hubInstanceMaxLabelBytes   = 63
	hubInstanceMaxVersionBytes = 64
)

// hubInstanceSnapshot is one tick's view of this process's registry row.
// Stats is normalised and capped so the serialised payload (label,
// version, status, checks and stats) stays within
// api.HubInstanceRowMaxBytes (see hubInstanceSnapshotFromChecks). A tick
// compares the snapshot's material JSON (see material) with that of the
// last successfully written snapshot, and writes the full row only when
// they differ.
type hubInstanceSnapshot struct {
	Label   string               `json:"label"`
	Version string               `json:"version"`
	Status  string               `json:"status"`
	Checks  map[string]string    `json:"checks"`
	Stats   api.HubInstanceStats `json:"stats"`
}

// hubInstanceMaterial is the part of a snapshot whose change rewrites the
// full row: everything except the volatile pool gauges (in_use, idle,
// wait_count), which TouchHubInstance carries. encoding/json sorts map
// keys, so equal material marshals to equal bytes.
type hubInstanceMaterial struct {
	Label                 string                       `json:"label"`
	Version               string                       `json:"version"`
	Status                string                       `json:"status"`
	Checks                map[string]string            `json:"checks"`
	DBMaxOpen             *int                         `json:"db_max_open"`
	Integrations          []api.HubInstanceIntegration `json:"integrations"`
	IntegrationsTruncated bool                         `json:"integrations_truncated"`
}

// material returns the canonical JSON of the snapshot's material fields.
func (s hubInstanceSnapshot) material() []byte {
	m := hubInstanceMaterial{
		Label:                 s.Label,
		Version:               s.Version,
		Status:                s.Status,
		Checks:                s.Checks,
		Integrations:          s.Stats.Integrations,
		IntegrationsTruncated: s.Stats.IntegrationsTruncated,
	}
	if s.Stats.DB != nil {
		maxOpen := s.Stats.DB.MaxOpen
		m.DBMaxOpen = &maxOpen
	}
	// Strings, ints, bools and a string map always marshal; a nil result
	// would only force an upsert.
	b, _ := json.Marshal(m)
	return b
}

// statsJSON returns the encoded stats column for an upsert.
func (s hubInstanceSnapshot) statsJSON() json.RawMessage {
	b, _ := json.Marshal(s.Stats)
	return b
}

// hubInstanceRegistry is the per-process registry writer. tick is called by
// one goroutine only (run), so ticks never overlap; mu guards the counters
// so tests can read them.
type hubInstanceRegistry struct {
	id       string
	store    store.HubInstanceStore
	snapshot func(ctx context.Context) hubInstanceSnapshot
	// interval returns the wait before the next tick.
	interval func() time.Duration
	log      *slog.Logger

	mu sync.Mutex
	// ticks counts ticks run; tick n (0-based) is forced when
	// n%hubInstanceForcedUpsertEvery == 0, so the first tick upserts.
	ticks int
	// lastWritten is the material JSON of the last snapshot written
	// successfully, or nil when the last write failed (or none ran yet), in
	// which case the next tick upserts.
	lastWritten []byte
	lastWarn    time.Time
	// lastPanicLog is when a recovered panic was last logged, and
	// panicsSuppressed counts the recovered panics since then that were
	// not logged (see panicked).
	lastPanicLog     time.Time
	panicsSuppressed int
	// writeUncertain is true when the last registry write returned an
	// error: the client gave up (a deadline, a dropped connection, or a
	// database error), so whether the database applied the write is not
	// known. It is cleared by the next write that succeeds. The clean stop
	// reads it after joining the loop (see hubInstanceRegistryStop.stop).
	writeUncertain atomic.Bool
	// now is the clock for log rate limiting; nil means time.Now. Tests
	// set it to step through the rate-limit window.
	now func() time.Time
}

// newHubInstanceRegistry returns the registry writer for this server.
func (s *Server) newHubInstanceRegistry() *hubInstanceRegistry {
	label := hubInstanceLabel(s.InstanceID())
	return &hubInstanceRegistry{
		id:       s.InstanceID(),
		store:    s.store,
		snapshot: func(ctx context.Context) hubInstanceSnapshot { return s.hubInstanceSnapshot(ctx, label) },
		interval: jitteredHubInstanceInterval,
		log:      slog.Default(),
	}
}

// hubInstanceSnapshot builds this process's registry snapshot. It runs only
// healthChecks (one store Ping plus in-process checks) and reads the
// connection pool counters in memory, never the agent, project or broker
// count queries of GetHealthInfo. It also reads the health of the plugins
// this instance runs (hubInstanceIntegrations: shared per-plugin queries,
// at most healthIntegrationQueryTimeout), which reaches every replica's
// health summary through this row. The status is derived from the raw
// checks before normalising, so a check dropped by the normaliser still
// counts toward it.
func (s *Server) hubInstanceSnapshot(ctx context.Context, label string) hubInstanceSnapshot {
	checks := s.healthChecks(ctx)
	stats := api.HubInstanceStats{
		DB:           s.hubInstanceDBStats(),
		Integrations: s.hubInstanceIntegrations(ctx),
	}
	return hubInstanceSnapshotFromChecks(label, version.Short(), checks, stats)
}

// hubInstanceDBStats returns this process's database connection pool
// counters from sql.DB.Stats(), or nil when the store exposes no *sql.DB.
// The health summary never calls this: each instance's pool reaches the
// summary through its own registry row.
func (s *Server) hubInstanceDBStats() *api.HubInstanceDBStats {
	dbp, ok := s.store.(interface{ DB() *sql.DB })
	if !ok {
		return nil
	}
	db := dbp.DB()
	if db == nil {
		return nil
	}
	st := db.Stats()
	return &api.HubInstanceDBStats{
		InUse:     st.InUse,
		Idle:      st.Idle,
		MaxOpen:   st.MaxOpenConnections,
		WaitCount: st.WaitCount,
	}
}

// hubInstanceSnapshotFromChecks builds a snapshot from a raw check map and
// raw stats: status from the raw checks, stored checks normalised, version
// bounded, and stats normalised and cut so the serialised payload (label,
// version, status, checks and stats; not the instance ID or timestamps) is
// at most api.HubInstanceRowMaxBytes.
func hubInstanceSnapshotFromChecks(label, ver string, raw map[string]string, stats api.HubInstanceStats) hubInstanceSnapshot {
	snap := hubInstanceSnapshot{
		Label:   label,
		Version: boundedPrintable(ver, hubInstanceMaxVersionBytes),
		Status:  deriveHealthStatus(raw),
		Checks:  api.NormalizeHubInstanceChecks(raw),
	}
	// The row with empty stats ("stats":{}) sets the room left for the
	// encoded stats, which replace the two-byte "{}".
	base, _ := json.Marshal(snap)
	snap.Stats, _ = api.CapHubInstanceStats(stats, api.HubInstanceRowMaxBytes-len(base)+len("{}"))
	return snap
}

// startHubInstanceRegistry starts this server's registry loop on a child of
// the server-lifetime context ctx and waits for its first tick, at most
// hubInstanceStartWait, so the serving replica's row usually exists before
// the listener serves the first summary. Before waiting it records the
// loop's stop handle on the server, so CleanupResources can stop the loop,
// join it and mark the row stopped (stopHubInstanceRegistry). It returns
// the loop's done channel, closed when the loop goroutine has exited.
//
// It must be called at most once per server (StartBackgroundServices is
// the only caller): a second call would replace the first loop's stop
// handle, and that loop would then stop only with the server-lifetime
// context, without a clean-stop write.
func (s *Server) startHubInstanceRegistry(ctx context.Context) <-chan struct{} {
	reg := s.newHubInstanceRegistry()
	loopCtx, cancel := context.WithCancel(ctx)
	first, done := launchHubInstanceRegistryLoop(loopCtx, reg)
	s.mu.Lock()
	s.hubInstanceRegistryStop = &hubInstanceRegistryStop{
		id:     reg.id,
		store:  reg.store,
		cancel: cancel,
		done:   done,
		log:    reg.log,

		writeUncertain: &reg.writeUncertain,
	}
	s.mu.Unlock()
	waitHubInstanceRegistryFirstTick(loopCtx, reg, first, hubInstanceStartWait)
	return done
}

// startHubInstanceRegistryLoop starts reg's loop on its own goroutine and
// returns once the first tick has ended, wait has passed, or ctx is
// cancelled, whichever comes first; a slow first tick keeps running in the
// background. The returned channel is closed when the goroutine exits
// (after ctx is cancelled and any in-flight tick has returned), so a caller
// can join the loop on shutdown.
func startHubInstanceRegistryLoop(ctx context.Context, reg *hubInstanceRegistry, wait time.Duration) <-chan struct{} {
	first, done := launchHubInstanceRegistryLoop(ctx, reg)
	waitHubInstanceRegistryFirstTick(ctx, reg, first, wait)
	return done
}

// launchHubInstanceRegistryLoop starts reg's loop on its own goroutine. first
// is closed when the first tick ends; done is closed when the goroutine
// exits.
func launchHubInstanceRegistryLoop(ctx context.Context, reg *hubInstanceRegistry) (first, done <-chan struct{}) {
	firstCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		reg.run(ctx, firstCh)
	}()
	return firstCh, doneCh
}

// waitHubInstanceRegistryFirstTick returns once first is closed, wait has
// passed, or ctx is cancelled, whichever comes first.
func waitHubInstanceRegistryFirstTick(ctx context.Context, reg *hubInstanceRegistry, first <-chan struct{}, wait time.Duration) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-first:
	case <-timer.C:
		reg.log.Warn("hub instance registry: first write still running; serving anyway", "wait", wait)
	case <-ctx.Done():
	}
}

// hubInstanceStopBudget bounds the clean-stop step on shutdown: stopping
// and joining the registry loop, then the MarkHubInstanceStopped write.
const hubInstanceStopBudget = 2 * time.Second

// hubInstanceRegistryStop is the handle CleanupResources uses to stop this
// process's registry loop and record a clean stop.
type hubInstanceRegistryStop struct {
	id     string
	store  store.HubInstanceStore
	cancel context.CancelFunc
	done   <-chan struct{}
	log    *slog.Logger
	// writeUncertain is the registry's flag: the last write returned an
	// error, so its outcome on the database is not known.
	writeUncertain *atomic.Bool
}

// errHubInstanceRegistryJoin is returned by stop when the loop did not exit
// within the budget, so the row was not marked stopped.
var errHubInstanceRegistryJoin = errors.New("hub instance registry: loop did not stop in time")

// errHubInstanceRegistryWriteUnknown is returned by stop when the loop's
// last write returned an error, so the row was not marked stopped.
var errHubInstanceRegistryWriteUnknown = errors.New("hub instance registry: last write outcome unknown")

// stop cancels the registry loop, waits for its goroutine to exit (done),
// and only then writes the clean stop. The join is what makes the order
// safe: UpsertHubInstance clears stopped_at, so a write still in flight
// after the stop write would turn a clean stop back into a running row
// (and later a stale one). Cancelling the loop stops it between ticks
// only: a tick that has already started its write lets the write run (see
// tick), so done closes after that write's client call has returned.
//
// A successful return means the database has applied the write. An
// error does not: the client may have given up (for example at the
// tick's deadline) while the database still applies the write later. So
// the stop write runs only when the loop has exited and its last write
// succeeded. If the loop has not exited when ctx ends, or its last write
// returned an error, the stop write is skipped and the row goes stale
// like a crashed replica's; it is never marked stopped while a write may
// still follow.
func (h *hubInstanceRegistryStop) stop(ctx context.Context) error {
	h.cancel()
	select {
	case <-h.done:
	case <-ctx.Done():
		return errHubInstanceRegistryJoin
	}
	// Closing done happens before the receive above, so the loop's last
	// store of the flag is visible here.
	if h.writeUncertain != nil && h.writeUncertain.Load() {
		return errHubInstanceRegistryWriteUnknown
	}
	if err := h.store.MarkHubInstanceStopped(ctx, h.id); err != nil {
		return fmt.Errorf("mark hub instance stopped: %w", err)
	}
	return nil
}

// stopHubInstanceRegistry stops this server's registry loop and marks its
// row stopped, within hubInstanceStopBudget. It runs once: the handle is
// taken from the server, so a second call (or a server whose loop never
// started) does nothing. The budget is taken from a context detached from
// ctx's cancellation, so the best-effort write is still tried when the
// caller's context is already done. Failures are logged at warn; shutdown
// continues either way.
func (s *Server) stopHubInstanceRegistry(ctx context.Context) {
	s.mu.Lock()
	h := s.hubInstanceRegistryStop
	s.hubInstanceRegistryStop = nil
	s.mu.Unlock()
	if h == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubInstanceStopBudget)
	defer cancel()
	if err := h.stop(stopCtx); err != nil {
		h.log.Warn("hub instance registry: clean stop not recorded", "instance_id", h.id, "error", err)
	}
}

// run ticks once immediately (closing first when that tick ends), then once
// per interval until ctx is cancelled. A tick runs to completion before the
// next interval starts, so ticks never overlap. A panicking tick is
// recovered by safeTick and the loop keeps running.
func (r *hubInstanceRegistry) run(ctx context.Context, first chan<- struct{}) {
	r.safeTick(ctx)
	close(first)
	for {
		timer := time.NewTimer(r.interval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			r.safeTick(ctx)
		}
	}
}

// safeTick runs one tick and recovers a panic from it (for example from a
// health check), so a fault in a background tick cannot stop the hub
// process. The tick counts as a failed write, so the next tick upserts;
// see panicked for logging.
func (r *hubInstanceRegistry) safeTick(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			r.panicked(ctx, p, debug.Stack())
		}
	}()
	r.tick(ctx)
}

// hubInstancePanicStackBytes bounds the stack trace logged with a
// recovered panic.
const hubInstancePanicStackBytes = 4096

// panicked records a recovered tick panic: the last write's outcome is
// unknown (writeUncertain), and the next tick upserts. At most
// one line is logged per hubInstanceWarnEvery window, at error, starting
// with the first panic; every logged line carries the panic value, a stack
// trace cut to hubInstancePanicStackBytes, and the number of panics not
// logged since the previous line. Nothing is logged once ctx is done
// (shutdown has started).
func (r *hubInstanceRegistry) panicked(ctx context.Context, p any, stack []byte) {
	// The panic may have come from inside a store write, after the
	// statement was sent, so the write's outcome is unknown: a clean stop
	// right after skips its write. The next successful write clears it.
	r.writeUncertain.Store(true)
	r.mu.Lock()
	r.lastWritten = nil
	if ctx.Err() != nil {
		r.mu.Unlock()
		return
	}
	now := r.clock()
	if !r.lastPanicLog.IsZero() && now.Sub(r.lastPanicLog) < hubInstanceWarnEvery {
		r.panicsSuppressed++
		r.mu.Unlock()
		return
	}
	suppressed := r.panicsSuppressed
	r.panicsSuppressed = 0
	r.lastPanicLog = now
	r.mu.Unlock()

	if len(stack) > hubInstancePanicStackBytes {
		stack = stack[:hubInstancePanicStackBytes]
	}
	r.log.Error("hub instance registry: tick panicked; continuing",
		"instance_id", r.id,
		"panic", fmt.Sprint(p),
		"suppressed_since_last_log", suppressed,
		"stack", string(stack))
}

// clock returns the rate-limit clock's current time.
func (r *hubInstanceRegistry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// jitteredHubInstanceInterval returns hubInstanceTickInterval ± 10%.
func jitteredHubInstanceInterval() time.Duration {
	f := 1 + hubInstanceTickJitter*(2*rand.Float64()-1)
	return time.Duration(float64(hubInstanceTickInterval) * f)
}

// tick takes one snapshot and writes it: TouchHubInstance (last_seen and the
// pool gauges) when the material fields equal the last successful write and
// the tick is not forced, otherwise (or when the row is missing) UpsertHubInstance. A failure is
// logged at warn, rate-limited, and the next tick upserts.
func (r *hubInstanceRegistry) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, hubInstanceTickTimeout)
	defer cancel()

	r.mu.Lock()
	n := r.ticks
	r.ticks++
	last := r.lastWritten
	r.mu.Unlock()

	snap := r.snapshot(ctx)
	material := snap.material()
	forced := n%hubInstanceForcedUpsertEvery == 0

	// Shutdown has started: write nothing, so the clean-stop write is the
	// last write to the row.
	if parent.Err() != nil {
		return
	}
	// A write that has started is not cancelled by shutdown, only bounded
	// by this tick's deadline. Cancelling a statement on the client does
	// not stop it on the database (pgx only closes its side), so a
	// cancelled upsert could still commit after the clean-stop write and
	// clear stopped_at. With an uncancelled write, the loop returns only
	// after the write's client call has returned, and the stop's join on
	// the loop orders the two. A write that returns an error (including
	// at the deadline) leaves its outcome unknown; failed records that, and
	// the clean stop then skips its write.
	deadline, _ := ctx.Deadline()
	wctx, wcancel := context.WithDeadline(context.WithoutCancel(parent), deadline)
	defer wcancel()

	if !forced && last != nil && bytes.Equal(material, last) {
		// Touch carries the volatile pool gauges; max_open is material,
		// so it is unchanged since the last upsert.
		found, err := r.store.TouchHubInstance(wctx, r.id, snap.Stats.DB)
		if err != nil {
			r.failed(parent, "touch", err)
			return
		}
		r.writeUncertain.Store(false)
		if found {
			return
		}
		// The row is gone (pruned, or never written): write it in full.
	}

	err := r.store.UpsertHubInstance(wctx, store.HubInstance{
		ID:      r.id,
		Label:   snap.Label,
		Version: snap.Version,
		Status:  snap.Status,
		Checks:  snap.Checks,
		Stats:   snap.statsJSON(),
	})
	if err != nil {
		r.failed(parent, "upsert", err)
		return
	}
	r.writeUncertain.Store(false)
	r.mu.Lock()
	r.lastWritten = material
	r.mu.Unlock()
}

// failed records a failed write: its outcome on the database is unknown
// (writeUncertain), the next tick upserts, and the warning is logged at
// most once per hubInstanceWarnEvery. Nothing is logged after shutdown
// has started.
func (r *hubInstanceRegistry) failed(parent context.Context, op string, err error) {
	r.writeUncertain.Store(true)
	r.mu.Lock()
	r.lastWritten = nil
	now := r.clock()
	warn := now.Sub(r.lastWarn) >= hubInstanceWarnEvery
	if warn {
		r.lastWarn = now
	}
	r.mu.Unlock()
	if warn && parent.Err() == nil {
		r.log.Warn("hub instance registry: write failed", "op", op, "instance_id", r.id, "error", err)
	}
}

// hubInstanceLabel returns this process's display label: POD_NAME; else, on
// Cloud Run, K_REVISION + "/" + the first 8 characters of the instance ID;
// else the host name. It is cut to 63 bytes of printable ASCII.
func hubInstanceLabel(instanceID string) string {
	label := os.Getenv("POD_NAME")
	if label == "" {
		if rev := os.Getenv("K_REVISION"); rev != "" {
			short := instanceID
			if len(short) > 8 {
				short = short[:8]
			}
			label = rev + "/" + short
		}
	}
	if label == "" {
		label, _ = os.Hostname()
	}
	return boundedPrintable(label, hubInstanceMaxLabelBytes)
}

// boundedPrintable drops every byte outside printable ASCII from s and cuts
// the result to at most max bytes.
func boundedPrintable(s string, max int) string {
	out := make([]byte, 0, min(len(s), max))
	for i := 0; i < len(s) && len(out) < max; i++ {
		if c := s[i]; c >= 0x20 && c <= 0x7e {
			out = append(out, c)
		}
	}
	return string(out)
}
