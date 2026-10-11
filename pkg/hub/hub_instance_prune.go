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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Pruning of the hub-instance registry (health dashboard F3 design §5.11).
// A row is deleted 24 h after its last write (stopped_at when set,
// otherwise last_seen), so the table keeps a day of restarts while staying
// small. A live replica whose row was pruned re-creates it on its next
// tick: TouchHubInstance reports found=false and the writer upserts.

const (
	// hubInstanceRetention is how long a row is kept after its last write.
	hubInstanceRetention = 24 * time.Hour
	// hubInstancePruneIntervalMinutes is the prune job's cadence.
	hubInstancePruneIntervalMinutes = 60
	// hubInstancePruneJobName is the prune job's scheduler name.
	hubInstancePruneJobName = "hub-instance-prune"
)

// registerHubInstancePrune registers the prune as a recurring singleton on
// sched: at most one replica per tick runs it, under
// store.LockHubInstancePrune (a no-op lock on SQLite).
func (s *Server) registerHubInstancePrune(sched *Scheduler) {
	sched.RegisterRecurringSingleton(hubInstancePruneJobName, hubInstancePruneIntervalMinutes, store.LockHubInstancePrune, s.hubInstancePruneHandler())
}

// hubInstancePruneHandler deletes the registry rows past
// hubInstanceRetention. The cut is taken on the store clock inside
// PruneHubInstances. A failure is logged at warn and retried next tick.
func (s *Server) hubInstancePruneHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		n, err := s.store.PruneHubInstances(ctx, hubInstanceRetention)
		if err != nil {
			slog.Warn("hub instance registry: prune failed", "error", err)
			return
		}
		if n > 0 {
			slog.Info("hub instance registry: pruned rows", "deleted", n, "retention", hubInstanceRetention)
		}
	}
}
