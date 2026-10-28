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
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// The operator-invoked star reconciliation (maintenance operation
// chat-star-reconcile). It drives the same store scanner as the periodic
// upkeep (PurgeMessageStarCandidates) over the whole table until a full
// sweep finds nothing to repair or purge. An operator runs it to completion
// before enabling web.chat_starred_messages again after running an older
// binary, whose DM promotions do not update the stored conversation of a
// star.
//
// It is safe to run with the experiment on or off and on several replicas
// at once, alongside their upkeep passes: each batch repairs and purges in
// its own transaction, a repair writes the current target (repeating it
// changes nothing) and a purge of an already deleted row deletes nothing.

const (
	// messageStarReconcileBatch bounds how many associations one batch
	// examines; the same bound as an upkeep pass.
	messageStarReconcileBatch = messageStarUpkeepBatch
	// messageStarReconcileBatchTimeout bounds one batch.
	messageStarReconcileBatchTimeout = 30 * time.Second
	// messageStarReconcileMaxSweeps bounds the full sweeps of one run. A
	// sweep repairs and purges everything it examines, so the second one
	// normally finds nothing; more are needed only while stars keep
	// becoming stale during the run.
	messageStarReconcileMaxSweeps = 3
	// messageStarReconcileTimeout bounds a whole run
	// (maintenanceRunTimeouts).
	messageStarReconcileTimeout = 30 * time.Minute
)

// errMessageStarReconcileNotExhausted reports a run that stopped at its
// sweep bound while sweeps were still changing stars.
var errMessageStarReconcileNotExhausted = errors.New("stars still needed repair or purge after the last sweep; run the operation again")

// messageStarReconcileLimits bounds a reconciliation.
type messageStarReconcileLimits struct {
	Batch        int
	BatchTimeout time.Duration
	MaxSweeps    int
}

// defaultMessageStarReconcileLimits are the limits of the maintenance
// operation.
var defaultMessageStarReconcileLimits = messageStarReconcileLimits{
	Batch:        messageStarReconcileBatch,
	BatchTimeout: messageStarReconcileBatchTimeout,
	MaxSweeps:    messageStarReconcileMaxSweeps,
}

// messageStarSweep counts what one full sweep did.
type messageStarSweep struct {
	Examined int
	Repaired int64
	Purged   int64
}

// changed reports whether the sweep repaired or purged a star.
func (s messageStarSweep) changed() bool { return s.Repaired > 0 || s.Purged > 0 }

// messageStarReconcileReport counts what a reconciliation did.
type messageStarReconcileReport struct {
	Sweeps   int
	Examined int
	Repaired int64
	Purged   int64
	// Exhausted is set once a full sweep repaired and purged nothing.
	Exhausted bool
}

// reconcileMessageStars sweeps the star table from the start, in batches,
// until a full sweep repairs and purges nothing (the report is Exhausted)
// or lim.MaxSweeps sweeps ran (errMessageStarReconcileNotExhausted).
// onSweep, if set, is called after every completed sweep. A store error or
// the end of ctx stops it with the counts so far; a later run starts over,
// so nothing is lost.
func reconcileMessageStars(ctx context.Context, stars MessageStarStore, lim messageStarReconcileLimits,
	onSweep func(n int, sw messageStarSweep)) (messageStarReconcileReport, error) {
	var report messageStarReconcileReport
	if lim.Batch < 1 || lim.MaxSweeps < 1 || lim.BatchTimeout <= 0 {
		return report, fmt.Errorf("star reconciliation: invalid limits %+v", lim)
	}
	for report.Sweeps < lim.MaxSweeps {
		sw, err := sweepMessageStars(ctx, stars, lim)
		report.Examined += sw.Examined
		report.Repaired += sw.Repaired
		report.Purged += sw.Purged
		if err != nil {
			if ctx.Err() != nil {
				return report, messageStarReconcileStopped(ctx, report)
			}
			return report, err
		}
		report.Sweeps++
		if onSweep != nil {
			onSweep(report.Sweeps, sw)
		}
		if !sw.changed() {
			report.Exhausted = true
			return report, nil
		}
	}
	return report, errMessageStarReconcileNotExhausted
}

// messageStarReconcileStopped is the error of a run that ctx stopped (its
// time limit, or the hub shutting down): it says so, gives the totals so
// far and asks for another run, which starts over. It wraps ctx's error.
func messageStarReconcileStopped(ctx context.Context, report messageStarReconcileReport) error {
	why := "the run was stopped"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		why = "the run hit its time limit"
	}
	return fmt.Errorf("%s after examining %d stars (repaired %d, purged %d); run the operation again: %w",
		why, report.Examined, report.Repaired, report.Purged, ctx.Err())
}

// sweepMessageStars runs the scanner from the start of the table until it
// wraps, one bounded batch at a time.
func sweepMessageStars(ctx context.Context, stars MessageStarStore, lim messageStarReconcileLimits) (messageStarSweep, error) {
	var sw messageStarSweep
	var pos StarCleanupPosition
	for {
		if err := ctx.Err(); err != nil {
			return sw, err
		}
		batchCtx, cancel := context.WithTimeout(ctx, lim.BatchTimeout)
		res, err := stars.PurgeMessageStarCandidates(batchCtx, pos, lim.Batch)
		cancel()
		if err != nil {
			return sw, err
		}
		sw.Examined += res.Examined
		sw.Repaired += res.Repaired
		sw.Purged += res.Purged
		if res.Next == (StarCleanupPosition{}) {
			return sw, nil
		}
		pos = res.Next
	}
}

// ChatStarReconcileExecutor runs the star reconciliation as a maintenance
// operation. It takes no parameters. The run log lists every sweep and the
// totals; the run completes only when the stars are exhausted.
type ChatStarReconcileExecutor struct {
	srv *Server
	// limits overrides defaultMessageStarReconcileLimits (tests).
	limits *messageStarReconcileLimits
}

// Run implements MaintenanceExecutor.
func (e *ChatStarReconcileExecutor) Run(ctx context.Context, logger io.Writer, _ map[string]string) error {
	stars := e.srv.messageStarStore()
	if stars == nil {
		return errors.New("star reconciliation: the chat store has no starred messages support (native chat is disabled or its store is unavailable)")
	}
	lim := defaultMessageStarReconcileLimits
	if e.limits != nil {
		lim = *e.limits
	}
	_, _ = fmt.Fprintf(logger, "Reconciling starred messages in batches of %d, at most %d sweeps\n", lim.Batch, lim.MaxSweeps)
	report, err := reconcileMessageStars(ctx, stars, lim, func(n int, sw messageStarSweep) {
		_, _ = fmt.Fprintf(logger, "Sweep %d: examined %d, repaired %d, purged %d\n", n, sw.Examined, sw.Repaired, sw.Purged)
	})
	_, _ = fmt.Fprintf(logger, "Total: sweeps %d, examined %d, repaired %d, purged %d\n",
		report.Sweeps, report.Examined, report.Repaired, report.Purged)
	if err != nil {
		return fmt.Errorf("star reconciliation: %w", err)
	}
	_, _ = fmt.Fprintln(logger, "Complete: no star needs repair or purge")
	return nil
}
