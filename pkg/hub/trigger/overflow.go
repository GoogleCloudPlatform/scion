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

package trigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Security Surface S21 — Overflow webhook bodies in object storage (Stage 2 §8, §18):
//
// Invariant: Webhook bodies exceeding MaxPayloadBytes (64 KiB) up to
// MaxEndpointBodyBytes (1 MiB) are stored in hub-private object storage and
// must never be exposed via public/signed URLs, read across projects, exhaust
// storage unboundedly, or persist past retention or as orphans.
//
// Sinks & controls:
//  1. Object path: canonical `trigger-events/<projectID>/<eventID>.json` where
//     both IDs are validated lowercase UUIDs (OverflowPayloadObjectPath /
//     IsValidOverflowPayloadPath); never caller-controlled.
//  2. Ingress upload (Stage 2 §8 deviation, R1): runs BEFORE the DB transaction
//     after auth/signature verification, header bounds, non-locking dedup
//     pre-check, and non-locking advisory budget pre-check; Content-Type forced
//     to application/json; bounded 10s timeout; failure -> 503 before any DB
//     lock or connection is held. Inside the DB-only transaction: LockTriggerQuota,
//     authoritative dedup, pending cap, authoritative in-lock SumTriggerEventOverflowBytes
//     budget check (O1), and insert. If the tx closure itself returns an error
//     (rollback certain), best-effort delete of the just-uploaded object runs
//     with context.WithoutCancel(ctx); COMMIT errors never delete (left to sweep).
//  3. Read/stream: ONLY via GET /api/v1/projects/{p}/trigger-events/{id}?payload=1
//     under experiment hub.triggers and the same derived trigger.read + row-visibility
//     authz as GET /trigger-events/{id}; verifies IsValidOverflowPayloadPath(projectID, ref);
//     streams bytes directly with nosniff/no-store; never issues a signed URL.
//  4. Envelope / home file: ReadOverflowPayloadPreview reads at most 16 KiB (+ UTFMax)
//     bounded by io.LimitReader and builds ExternalPayload{Truncated: true, Bytes: N, Text: ...}.
//  5. CEL & templating: ev.Payload is "" for overflow rows, so has(event.payload) is false,
//     the drainer's per-event JSON parse stays bounded at 64 KiB, and templating treats
//     event.payload.* as out-of-line (optional ? -> empty; required -> refused).
//  6. Purge & orphan sweep: 7-day purge deletes objects with purged rows (failures logged
//     and retried hourly); orphan sweep lists trigger-events/ grouped by project against
//     the (project_id, payload_ref) index, skips zero-timestamp or <5m objects, and deletes
//     unreferenced objects.
//  7. Per-project byte budget: <= 256 MiB of live overflow objects per project over
//     HistoryRetention, computed authoritatively inside LockTriggerQuota -> 429 "queue full".
const (
	// OverflowPayloadPrefix is the bucket-relative prefix for all out-of-line
	// trigger event payloads (Stage 2 §8).
	OverflowPayloadPrefix = "trigger-events/"

	// MaxOverflowPayloadBytesPerProject bounds live overflow payload storage
	// per project over the retention window (256 MiB, Stage 2 §8).
	MaxOverflowPayloadBytesPerProject int64 = 256 * 1024 * 1024

	// DefaultOverflowUploadTimeout bounds pre-transaction object storage uploads.
	DefaultOverflowUploadTimeout = 10 * time.Second

	// DefaultOverflowDownloadTimeout bounds object storage reads for delivery
	// previews and maintenance/cleanup deletes.
	DefaultOverflowDownloadTimeout = 10 * time.Second

	// DefaultOverflowOrphanMinAge prevents the hourly orphan sweep from
	// deleting an object whose ingress transaction is still in flight
	// (strictly longer than worst-case upload-to-commit wait).
	DefaultOverflowOrphanMinAge = 5 * time.Minute

	// DefaultOverflowOrphanSweepBatch is the page size used when listing
	// overflow objects during the orphan sweep.
	DefaultOverflowOrphanSweepBatch = 500

	overflowPayloadSuffix = ".json"
)

// OverflowPayloadObjectPath returns the canonical hub-private object storage
// path `trigger-events/<projectID>/<eventID>.json` after verifying both IDs
// are canonical lowercase UUIDs.
func OverflowPayloadObjectPath(projectID, eventID string) (string, error) {
	if !isCanonicalLowerUUID(projectID) {
		return "", fmt.Errorf("invalid project ID for overflow payload path")
	}
	if !isCanonicalLowerUUID(eventID) {
		return "", fmt.Errorf("invalid event ID for overflow payload path")
	}
	return OverflowPayloadPrefix + projectID + "/" + eventID + overflowPayloadSuffix, nil
}

// ParseOverflowPayloadObjectPath validates objectPath against the canonical
// `trigger-events/<projectID>/<eventID>.json` shape and returns the embedded
// project and event UUIDs.
func ParseOverflowPayloadObjectPath(objectPath string) (projectID, eventID string, ok bool) {
	if !strings.HasPrefix(objectPath, OverflowPayloadPrefix) || !strings.HasSuffix(objectPath, overflowPayloadSuffix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(objectPath, OverflowPayloadPrefix)
	rest = strings.TrimSuffix(rest, overflowPayloadSuffix)
	pid, eid, hasSlash := strings.Cut(rest, "/")
	if !hasSlash || strings.Contains(eid, "/") {
		return "", "", false
	}
	if !isCanonicalLowerUUID(pid) || !isCanonicalLowerUUID(eid) {
		return "", "", false
	}
	return pid, eid, true
}

// IsValidOverflowPayloadPath reports whether objectPath is a valid canonical
// overflow payload object path belonging specifically to projectID.
func IsValidOverflowPayloadPath(projectID, objectPath string) bool {
	pid, _, ok := ParseOverflowPayloadObjectPath(objectPath)
	return ok && pid == projectID
}

// ReadOverflowPayloadPreview downloads at most the first 16 KiB (+ UTF-8 rune
// margin) of an overflow payload object for delivery envelopes and home files.
func ReadOverflowPayloadPreview(
	ctx context.Context,
	stor storage.Storage,
	projectID, objectPath string,
	timeout time.Duration,
) ([]byte, error) {
	if stor == nil {
		return nil, errors.New("overflow storage is not configured")
	}
	if !IsValidOverflowPayloadPath(projectID, objectPath) {
		return nil, storage.ErrInvalidPath
	}
	if timeout <= 0 {
		timeout = DefaultOverflowDownloadTimeout
	}
	dlCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rc, _, err := stor.Download(dlCtx, objectPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	limit := int64(messaging.MaxExternalPayloadInlineBytes + utf8.UTFMax)
	data, err := io.ReadAll(io.LimitReader(rc, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// PurgeOverflowObjects deletes the storage objects referenced by purged event
// rows. Missing objects (storage.ErrNotFound) are treated as already deleted.
// Other failures are logged and counted; because the DB rows are already gone,
// the hourly orphan sweep will retry them on the next pass.
func PurgeOverflowObjects(
	ctx context.Context,
	stor storage.Storage,
	refs []string,
	logger *slog.Logger,
) (deleted int, failed int) {
	if stor == nil || len(refs) == 0 {
		return 0, 0
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, ref := range refs {
		if _, _, ok := ParseOverflowPayloadObjectPath(ref); !ok {
			logger.Warn("trigger maintenance: skipping invalid overflow payload_ref during purge", "payload_ref", ref)
			continue
		}
		delCtx, cancel := context.WithTimeout(ctx, DefaultOverflowDownloadTimeout)
		err := stor.Delete(delCtx, ref)
		cancel()
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			failed++
			logger.Warn("trigger maintenance: failed to delete purged overflow payload object; will retry on next sweep",
				"payload_ref", ref, "error", err)
			continue
		}
		deleted++
	}
	return deleted, failed
}

// SweepOrphanOverflowObjects lists objects under OverflowPayloadPrefix, groups
// candidates by project ID so ExistingTriggerEventPayloadRefs uses the
// (project_id, payload_ref) index (O3), and deletes any object with no matching
// row whose timestamp is non-zero (N3) and at least minAge old (Stage 2 §8, S21).
func SweepOrphanOverflowObjects(
	ctx context.Context,
	ts store.TriggerStore,
	stor storage.Storage,
	now time.Time,
	minAge time.Duration,
	logger *slog.Logger,
) (swept int, failed int, err error) {
	if ts == nil || stor == nil {
		return 0, 0, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	cutoff := now.UTC().Add(-minAge)
	var nextOffset string
	seenOffsets := make(map[string]bool)

	for {
		if ctx.Err() != nil {
			return swept, failed, ctx.Err()
		}
		res, lErr := stor.List(ctx, storage.ListOptions{
			Prefix:      OverflowPayloadPrefix,
			MaxResults:  DefaultOverflowOrphanSweepBatch,
			StartOffset: nextOffset,
		})
		if lErr != nil {
			return swept, failed, fmt.Errorf("list overflow objects: %w", lErr)
		}
		if res == nil || len(res.Objects) == 0 {
			break
		}

		byProject := make(map[string][]string)
		for _, obj := range res.Objects {
			pid, _, ok := ParseOverflowPayloadObjectPath(obj.Name)
			if !ok {
				continue
			}
			tsObj := obj.Updated
			if tsObj.IsZero() {
				tsObj = obj.Created
			}
			// Fail safe (N3): skip objects with missing/zero timestamps or
			// within the in-flight grace window.
			if tsObj.IsZero() || tsObj.After(cutoff) {
				continue
			}
			byProject[pid] = append(byProject[pid], obj.Name)
		}

		if len(byProject) > 0 {
			projectIDs := make([]string, 0, len(byProject))
			for pid := range byProject {
				projectIDs = append(projectIDs, pid)
			}
			sort.Strings(projectIDs)

			for _, pid := range projectIDs {
				candidates := byProject[pid]
				existing, qErr := ts.ExistingTriggerEventPayloadRefs(ctx, pid, candidates)
				if qErr != nil {
					return swept, failed, fmt.Errorf("check existing trigger event payload refs: %w", qErr)
				}
				for _, ref := range candidates {
					if existing[ref] {
						continue
					}
					delCtx, cancel := context.WithTimeout(ctx, DefaultOverflowDownloadTimeout)
					dErr := stor.Delete(delCtx, ref)
					cancel()
					if dErr != nil {
						if errors.Is(dErr, storage.ErrNotFound) {
							continue
						}
						failed++
						logger.Warn("trigger maintenance: failed to delete orphan overflow payload object",
							"payload_ref", ref, "error", dErr)
						continue
					}
					swept++
				}
			}
		}

		if res.NextOffset == "" || seenOffsets[res.NextOffset] {
			break
		}
		seenOffsets[res.NextOffset] = true
		nextOffset = res.NextOffset
	}
	return swept, failed, nil
}
