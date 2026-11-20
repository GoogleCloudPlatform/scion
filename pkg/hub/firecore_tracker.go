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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
)

// scheduledFireTracker adapts the hub's detachedFireRun and scheduledDetachedFires
// tracking to the source-neutral hubfire.FireTracker interface.
type scheduledFireTracker struct {
	srv    *Server
	fireID string
}

func newScheduledFireTracker(srv *Server, fireID string) hubfire.FireTracker {
	return &scheduledFireTracker{
		srv:    srv,
		fireID: fireID,
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
	trackCtx, endWork, ok := t.srv.scheduledDetachedFires.begin()
	if !ok {
		return ctx, func() {}, false
	}
	run := &detachedFireRun{eventID: t.fireID, ranAt: time.Now()}
	t.srv.runningDetachedFires.add(run)
	fireCtx, cancelFire := context.WithCancel(ctx)
	stop := context.AfterFunc(trackCtx, cancelFire)
	releaseRun := func() {
		t.srv.runningDetachedFires.remove(run)
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
