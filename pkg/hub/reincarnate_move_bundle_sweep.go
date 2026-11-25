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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// defaultMoveBundleOrphanGrace is the minimum age a terminal reincarnation
	// record must have (based on UpdatedAt) before the orphan sweep deletes its
	// move-bundles/<rid>/ prefix (design §3.7). It MUST stay strictly greater
	// than SignedURLExpiry (15m) so a straggling source PUT on an already-issued
	// URL cannot recreate a blob after deletion.
	defaultMoveBundleOrphanGrace = time.Hour

	// moveBundleOrphanSweepLimit bounds how many <rid>/ prefixes a single
	// orphan-sweep pass inspects (design §3.7).
	moveBundleOrphanSweepLimit = 100
)

// validateMoveBundleOrphanGrace ensures the orphan-sweep grace period stays
// strictly greater than SignedURLExpiry so a straggling source PUT on an
// already-issued signed URL cannot recreate a blob after deletion.
func validateMoveBundleOrphanGrace(grace time.Duration) error {
	if grace <= SignedURLExpiry {
		return fmt.Errorf("move-bundle orphan sweep grace %s must be strictly greater than signed URL TTL %s", grace, SignedURLExpiry)
	}
	return nil
}

// sweepOrphanMoveBundles deletes orphaned move-bundle staging prefixes under
// MoveBundlesRoot(hubID) (design §3.7). It runs alongside the periodic
// stale-reincarnation sweep whenever Hub object storage is configured — even
// when the hub.agent_move_bundle experiment is off — so leftovers from before
// the experiment was disabled are still cleaned up.
func (s *Server) sweepOrphanMoveBundles(ctx context.Context) (int, error) {
	return s.sweepOrphanMoveBundlesAt(ctx, time.Now(), defaultMoveBundleOrphanGrace, moveBundleOrphanSweepLimit)
}

// sweepOrphanMoveBundlesAt is the testable core of sweepOrphanMoveBundles.
// It lists MoveBundlesRoot(hubID) with delimiter "/" and inspects at most
// limit prefixes per pass. A prefix is deleted only when:
//   - its <rid> passes storage.MoveBundlePath validation, AND
//   - the reincarnation record is missing (store.ErrNotFound), OR
//   - the reincarnation record is terminal (completed/failed) with
//     UpdatedAt strictly before now.Add(-grace).
//
// Non-terminal records and young terminal records are never touched.
func (s *Server) sweepOrphanMoveBundlesAt(ctx context.Context, now time.Time, grace time.Duration, limit int) (int, error) {
	if err := validateMoveBundleOrphanGrace(grace); err != nil {
		return 0, err
	}
	if limit <= 0 {
		limit = moveBundleOrphanSweepLimit
	}

	stor := s.GetStorage()
	if stor == nil {
		return 0, nil
	}

	rootPrefix := storage.DirPrefix(storage.MoveBundlesRoot(s.HubID()))
	if rootPrefix == "" {
		return 0, nil
	}

	res, err := stor.List(ctx, storage.ListOptions{
		Prefix:     rootPrefix,
		Delimiter:  "/",
		MaxResults: limit,
	})
	if err != nil {
		return 0, fmt.Errorf("list move-bundle prefixes under %q: %w", rootPrefix, err)
	}
	if res == nil || len(res.Prefixes) == 0 {
		return 0, nil
	}

	cutoff := now.Add(-grace)
	processed := 0
	deleted := 0

	for _, entry := range res.Prefixes {
		if processed >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		processed++

		if !strings.HasPrefix(entry, rootPrefix) || !strings.HasSuffix(entry, "/") {
			s.agentLifecycleLog.Warn("orphan sweep: skipping unexpected move-bundle prefix",
				"prefix", entry, "root", rootPrefix)
			continue
		}
		rid := strings.TrimSuffix(strings.TrimPrefix(entry, rootPrefix), "/")

		basePath, err := storage.MoveBundlePath(s.HubID(), rid)
		if err != nil {
			s.agentLifecycleLog.Warn("orphan sweep: skipping invalid move-bundle reincarnation ID",
				"prefix", entry, "reincarnation_id", rid, "error", err)
			continue
		}
		deletePrefix := storage.DirPrefix(basePath)
		if deletePrefix == "" || deletePrefix == rootPrefix || !strings.HasPrefix(deletePrefix, rootPrefix) {
			s.agentLifecycleLog.Warn("orphan sweep: refusing to delete unsafe move-bundle prefix",
				"prefix", entry, "delete_prefix", deletePrefix, "root", rootPrefix)
			continue
		}

		rec, err := s.store.GetAgentReincarnation(ctx, rid)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.agentLifecycleLog.Warn("orphan sweep: failed to look up reincarnation record; skipping prefix",
					"reincarnation_id", rid, "prefix", deletePrefix, "error", err)
				continue
			}
			// Record missing -> eligible for deletion.
		} else {
			// Only terminal records (completed or failed) may be swept, and only
			// after the grace period has elapsed since UpdatedAt.
			if rec.State != store.AgentReincarnationStateCompleted && rec.State != store.AgentReincarnationStateFailed {
				continue
			}
			if !rec.UpdatedAt.Before(cutoff) {
				continue
			}
		}

		if err := stor.DeletePrefix(ctx, deletePrefix); err != nil {
			s.agentLifecycleLog.Warn("orphan sweep: failed to delete orphaned move-bundle prefix",
				"reincarnation_id", rid, "prefix", deletePrefix, "error", err)
			continue
		}
		deleted++
	}

	return deleted, nil
}
