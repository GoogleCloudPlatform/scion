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
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"os"
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

// hubInstanceSnapshot holds the material fields of a registry row: a tick
// compares its canonical JSON with the last successfully written snapshot,
// and writes the full row only when they differ. encoding/json sorts map
// keys, so equal snapshots marshal to equal bytes.
type hubInstanceSnapshot struct {
	Label   string            `json:"label"`
	Version string            `json:"version"`
	Status  string            `json:"status"`
	Checks  map[string]string `json:"checks"`
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
	// lastWritten is the canonical JSON of the last snapshot written
	// successfully, or nil when the last write failed (or none ran yet), in
	// which case the next tick upserts.
	lastWritten []byte
	lastWarn    time.Time
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
// healthChecks (one store Ping plus in-process checks), never the agent,
// project or broker count queries of GetHealthInfo. The status is derived
// from the raw checks before normalising, so a check dropped by the
// normaliser still counts toward it.
func (s *Server) hubInstanceSnapshot(ctx context.Context, label string) hubInstanceSnapshot {
	return hubInstanceSnapshotFromChecks(label, version.Short(), s.healthChecks(ctx))
}

// hubInstanceSnapshotFromChecks builds a snapshot from a raw check map:
// status from the raw checks, stored checks normalised, version bounded.
func hubInstanceSnapshotFromChecks(label, ver string, raw map[string]string) hubInstanceSnapshot {
	return hubInstanceSnapshot{
		Label:   label,
		Version: boundedPrintable(ver, hubInstanceMaxVersionBytes),
		Status:  deriveHealthStatus(raw),
		Checks:  api.NormalizeHubInstanceChecks(raw),
	}
}

// startHubInstanceRegistry starts the registry loop on its own goroutine
// and waits for its first tick, at most hubInstanceStartWait, so the
// serving replica's row usually exists before the listener serves the
// first summary. The loop stops when ctx is cancelled.
func (s *Server) startHubInstanceRegistry(ctx context.Context) {
	reg := s.newHubInstanceRegistry()
	first := make(chan struct{})
	go reg.run(ctx, first)
	timer := time.NewTimer(hubInstanceStartWait)
	defer timer.Stop()
	select {
	case <-first:
	case <-timer.C:
		slog.Warn("hub instance registry: first write still running; serving anyway", "wait", hubInstanceStartWait)
	case <-ctx.Done():
	}
}

// run ticks once immediately (closing first when that tick ends), then once per
// interval until ctx is cancelled. A tick runs to completion before the
// next interval starts, so ticks never overlap.
func (r *hubInstanceRegistry) run(ctx context.Context, first chan<- struct{}) {
	r.tick(ctx)
	close(first)
	for {
		timer := time.NewTimer(r.interval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			r.tick(ctx)
		}
	}
}

// jitteredHubInstanceInterval returns hubInstanceTickInterval ± 10%.
func jitteredHubInstanceInterval() time.Duration {
	f := 1 + hubInstanceTickJitter*(2*rand.Float64()-1)
	return time.Duration(float64(hubInstanceTickInterval) * f)
}

// tick takes one snapshot and writes it: TouchHubInstance when the material
// fields equal the last successful write and the tick is not forced,
// otherwise (or when the row is missing) UpsertHubInstance. A failure is
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
	// Marshalling a struct of strings and a string map cannot fail; a nil
	// result would only force an upsert.
	material, _ := json.Marshal(snap)
	forced := n%hubInstanceForcedUpsertEvery == 0

	if !forced && last != nil && bytes.Equal(material, last) {
		found, err := r.store.TouchHubInstance(ctx, r.id)
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
	now := time.Now()
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
