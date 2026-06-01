/*
Copyright 2025 The Scion Authors.
*/

package dbmetrics

import (
	"context"
	"database/sql"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// DefaultPoolSampleInterval is the cadence used by StartPoolSampler when the
// caller passes a non-positive interval.
const DefaultPoolSampleInterval = 15 * time.Second

// StatsProvider is the subset of *sql.DB needed to sample connection-pool
// gauges. *sql.DB satisfies it directly; tests can supply a fake.
type StatsProvider interface {
	Stats() sql.DBStats
}

// poolStatsFrom maps a database/sql DBStats snapshot onto the PoolStats gauge
// set understood by the Recorder.
//
// Note on Waiting: database/sql does not expose an instantaneous "callers
// currently blocked on a connection" gauge. WaitCount is the cumulative number
// of times a caller had to wait for a connection, which is the canonical
// pool-saturation signal: a flat WaitCount means the pool is never exhausted, a
// rising one means callers are queuing (the trigger for the pooler decision in
// CONNECTION-BUDGET.md). It is reported as-is so dashboards can rate() it.
func poolStatsFrom(s sql.DBStats) PoolStats {
	return PoolStats{
		Active:  int64(s.InUse),
		Idle:    int64(s.Idle),
		Waiting: s.WaitCount,
		Max:     int64(s.MaxOpenConnections),
	}
}

// StartPoolSampler launches a goroutine that periodically snapshots db's
// connection-pool stats and records them via rec, until ctx is cancelled or the
// returned stop func is called (whichever happens first). stop is idempotent.
//
// It is the P3-6 wiring that feeds the P0-5 monitoring scaffold's pool gauges
// (scion.db.pool.connections.{active,idle,waiting,max}). When rec is disabled
// (no MeterProvider configured) or db is nil, sampling is skipped entirely so
// there is no idle goroutine in the common no-exporter case.
func StartPoolSampler(ctx context.Context, rec Recorder, db StatsProvider, interval time.Duration, attrs ...attribute.KeyValue) (stop func()) {
	if rec == nil || !rec.Enabled() || db == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = DefaultPoolSampleInterval
	}

	sampleCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Emit one snapshot immediately so the gauges are populated without
		// waiting a full interval.
		rec.ObservePoolStats(sampleCtx, poolStatsFrom(db.Stats()), attrs...)

		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				rec.ObservePoolStats(sampleCtx, poolStatsFrom(db.Stats()), attrs...)
			}
		}
	}()

	return cancel
}
