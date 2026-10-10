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
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime/debug"
	"sync"
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
// count queries of GetHealthInfo. The status is derived from the raw checks
// before normalising, so a check dropped by the normaliser still counts
// toward it.
func (s *Server) hubInstanceSnapshot(ctx context.Context, label string) hubInstanceSnapshot {
	return hubInstanceSnapshotFromChecks(label, version.Short(), s.healthChecks(ctx),
		api.HubInstanceStats{DB: s.hubInstanceDBStats()})
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

// startHubInstanceRegistry starts this server's registry loop on the
// server-lifetime context ctx and waits for its first tick, at most
// hubInstanceStartWait, so the serving replica's row usually exists before
// the listener serves the first summary. It returns the loop's done
// channel, closed when the loop goroutine has exited.
func (s *Server) startHubInstanceRegistry(ctx context.Context) <-chan struct{} {
	return startHubInstanceRegistryLoop(ctx, s.newHubInstanceRegistry(), hubInstanceStartWait)
}

// startHubInstanceRegistryLoop starts reg's loop on its own goroutine and
// returns once the first tick has ended, wait has passed, or ctx is
// cancelled, whichever comes first; a slow first tick keeps running in the
// background. The returned channel is closed when the goroutine exits
// (after ctx is cancelled and any in-flight tick has returned), so a caller
// can join the loop on shutdown.
func startHubInstanceRegistryLoop(ctx context.Context, reg *hubInstanceRegistry, wait time.Duration) <-chan struct{} {
	first := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.run(ctx, first)
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-first:
	case <-timer.C:
		reg.log.Warn("hub instance registry: first write still running; serving anyway", "wait", wait)
	case <-ctx.Done():
	}
	return done
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

// panicked records a recovered tick panic. The next tick upserts. At most
// one line is logged per hubInstanceWarnEvery window, at error, starting
// with the first panic; every logged line carries the panic value, a stack
// trace cut to hubInstancePanicStackBytes, and the number of panics not
// logged since the previous line. Nothing is logged once ctx is done
// (shutdown has started).
func (r *hubInstanceRegistry) panicked(ctx context.Context, p any, stack []byte) {
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

	if !forced && last != nil && bytes.Equal(material, last) {
		// Touch carries the volatile pool gauges; max_open is material,
		// so it is unchanged since the last upsert.
		found, err := r.store.TouchHubInstance(ctx, r.id, snap.Stats.DB)
		if err != nil {
			r.failed(parent, "touch", err)
			return
		}
		if found {
			return
		}
		// The row is gone (pruned, or never written): write it in full.
	}

	err := r.store.UpsertHubInstance(ctx, store.HubInstance{
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
	r.mu.Lock()
	r.lastWritten = material
	r.mu.Unlock()
}

// failed records a failed write: the next tick upserts, and the warning is
// logged at most once per hubInstanceWarnEvery. Nothing is logged after
// shutdown has started.
func (r *hubInstanceRegistry) failed(parent context.Context, op string, err error) {
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
