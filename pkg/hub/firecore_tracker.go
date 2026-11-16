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
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
)

// scheduledFireTracker adapts the hub's wakeFireRun and scheduledWakeFires
// tracking to the source-neutral hubfire.FireTracker interface without
// renaming wakeFireRun or scheduledWakeFires (Phase 2 owns detachedFireRun).
type scheduledFireTracker struct {
	srv    *Server
	fireID string

	mu  sync.Mutex
	run *wakeFireRun
}

func newScheduledFireTracker(srv *Server, fireID string, run *wakeFireRun) hubfire.FireTracker {
	return &scheduledFireTracker{
		srv:    srv,
		fireID: fireID,
		run:    run,
	}
}

func (t *scheduledFireTracker) Detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return detachKeepFireAbort(ctx)
}

func (t *scheduledFireTracker) Admit(ctx context.Context) (context.Context, func(), bool) {
	if t.srv == nil {
		return ctx, func() {}, true
	}
	if abort, _ := ctx.Value(scheduledFireAbortKey{}).(context.Context); abort != nil {
		return ctx, func() {}, true
	}
	trackCtx, endWork, ok := t.srv.scheduledWakeFires.begin()
	if !ok {
		return ctx, func() {}, false
	}
	run := &wakeFireRun{eventID: t.fireID, ranAt: time.Now()}
	t.mu.Lock()
	if t.run == nil {
		t.run = run
	}
	t.mu.Unlock()
	t.srv.runningWakeFires.add(run)
	fireCtx, cancelFire := context.WithCancel(ctx)
	stop := context.AfterFunc(trackCtx, cancelFire)
	releaseRun := func() {
		t.srv.runningWakeFires.remove(run)
		endWork()
	}
	if scope := oneShotFireScopeFromContext(ctx); scope != nil {
		scope.attach(run, trackCtx, releaseRun)
		return withScheduledFireAbort(fireCtx, trackCtx), func() {
			stop()
			cancelFire()
		}, true
	}
	return withScheduledFireAbort(fireCtx, trackCtx), func() {
		stop()
		cancelFire()
		releaseRun()
	}, true
}

// Record executes write at most once against the wakeFireRun injected into
// newScheduledFireTracker or registered with runningWakeFires by Admit. When
// neither happened, Record refuses (logs an error and returns false) rather
// than creating a private wakeFireRun outside shutdown/abandon coordination.
func (t *scheduledFireTracker) Record(write func()) bool {
	t.mu.Lock()
	run := t.run
	t.mu.Unlock()
	if run == nil {
		slog.Error("fire tracker Record called without an admitted or injected run",
			"fireID", t.fireID,
		)
		return false
	}
	return run.tryRecord(write)
}
