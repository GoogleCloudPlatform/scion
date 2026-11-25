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
	"strings"
	"sync"
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
//  2. Ingress upload: inside the ingress transaction AFTER auth/signature
//     verification, header bounds, dedup, pending cap, and per-project 256 MiB
//     byte budget; Content-Type forced to application/json; bounded timeout;
//     upload failure rolls back the DB transaction and answers 503.
//  3. Read/stream: ONLY via GET /api/v1/projects/{p}/trigger-events/{id}?payload=1
//     under experiment hub.triggers and the same derived trigger.read + row-visibility
//     authz as GET /trigger-events/{id}; verifies IsValidOverflowPayloadPath(projectID, ref);
//     streams bytes directly with nosniff/no-store; never issues a signed URL.
//  4. Envelope / home file: ReadOverflowPayloadPreview reads at most 16 KiB (+ UTFMax)
//     bounded by io.LimitReader and builds ExternalPayload{Truncated: true, Bytes: N, Text: ...}.
//  5. CEL evaluation: ev.Payload is "" for overflow rows, so has(event.payload) is false
//     and the drainer's per-event JSON parse stays bounded at 64 KiB.
//  6. Purge & orphan sweep: 7-day purge deletes objects with purged rows (failures logged
//     and retried hourly); orphan sweep lists trigger-events/ against trigger_events rows
//     and deletes unreferenced objects older than DefaultOverflowOrphanMinAge.
//  7. Per-project byte budget: <= 256 MiB of live overflow objects per project over
//     HistoryRetention (cached per minute, updated on commit) -> 429 "queue full".
const (
	// OverflowPayloadPrefix is the bucket-relative prefix for all out-of-line
	// trigger event payloads (Stage 2 §8).
	OverflowPayloadPrefix = "trigger-events/"

	// MaxOverflowPayloadBytesPerProject bounds live overflow payload storage
	// per project over the retention window (256 MiB, Stage 2 §8).
	MaxOverflowPayloadBytesPerProject int64 = 256 * 1024 * 1024

	// DefaultOverflowBudgetCacheTTL is the per-project byte-budget cache TTL
	// (1 minute, Stage 2 §8).
	DefaultOverflowBudgetCacheTTL = time.Minute

	// DefaultOverflowUploadTimeout bounds in-transaction object storage uploads.
	DefaultOverflowUploadTimeout = 10 * time.Second

	// DefaultOverflowDownloadTimeout bounds object storage reads for delivery
	// previews and maintenance deletes.
	DefaultOverflowDownloadTimeout = 10 * time.Second

	// DefaultOverflowOrphanMinAge prevents the hourly orphan sweep from
	// deleting an object whose ingress transaction is still in flight.
	DefaultOverflowOrphanMinAge = 5 * time.Minute

	// DefaultOverflowOrphanSweepBatch is the page size used when listing
	// overflow objects during the orphan sweep.
	DefaultOverflowOrphanSweepBatch = 500

	overflowBudgetCacheMaxEntries = 10000
	overflowPayloadSuffix         = ".json"
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

type overflowBudgetEntry struct {
	usedBytes int64
	expiresAt time.Time
}

// OverflowBudgetCache caches per-project live overflow byte totals for TTL
// (default 1 minute) and increments live entries on commit so a burst within
// one TTL window cannot exceed MaxOverflowPayloadBytesPerProject.
type OverflowBudgetCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]overflowBudgetEntry
}

// NewOverflowBudgetCache constructs a bounded per-project overflow byte budget cache.
func NewOverflowBudgetCache(ttl time.Duration) *OverflowBudgetCache {
	if ttl <= 0 {
		ttl = DefaultOverflowBudgetCacheTTL
	}
	return &OverflowBudgetCache{
		ttl:     ttl,
		entries: make(map[string]overflowBudgetEntry),
	}
}

// Get returns the cached live overflow byte count for projectID if fresh at now.
func (c *OverflowBudgetCache) Get(projectID string, now time.Time) (int64, bool) {
	if c == nil || projectID == "" {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.entries[projectID]
	if !ok || !now.Before(ent.expiresAt) {
		if ok {
			delete(c.entries, projectID)
		}
		return 0, false
	}
	return ent.usedBytes, true
}

// Set stores usedBytes for projectID until now + ttl.
func (c *OverflowBudgetCache) Set(projectID string, usedBytes int64, now time.Time) {
	if c == nil || projectID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= overflowBudgetCacheMaxEntries {
		// Evict expired entries first; if still full, clear map to stay O(1) bounded.
		for k, v := range c.entries {
			if !now.Before(v.expiresAt) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= overflowBudgetCacheMaxEntries {
			clear(c.entries)
		}
	}
	c.entries[projectID] = overflowBudgetEntry{
		usedBytes: usedBytes,
		expiresAt: now.Add(c.ttl),
	}
}

// Add increments a fresh cached entry for projectID by delta after a successful
// overflow commit. If no fresh entry exists, it is a no-op (the next check will
// query the store authoritatively).
func (c *OverflowBudgetCache) Add(projectID string, delta int64, now time.Time) {
	if c == nil || projectID == "" || delta <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.entries[projectID]
	if !ok || !now.Before(ent.expiresAt) {
		return
	}
	ent.usedBytes += delta
	c.entries[projectID] = ent
}

// Invalidate removes projectID from the cache.
func (c *OverflowBudgetCache) Invalidate(projectID string) {
	if c == nil || projectID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, projectID)
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

// SweepOrphanOverflowObjects lists objects under OverflowPayloadPrefix, checks
// them against live trigger_events rows, and deletes any object with no matching
// row whose timestamp is at least minAge old (Stage 2 §8, S21).
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

		candidates := make([]string, 0, len(res.Objects))
		for _, obj := range res.Objects {
			if _, _, ok := ParseOverflowPayloadObjectPath(obj.Name); !ok {
				continue
			}
			tsObj := obj.Updated
			if tsObj.IsZero() {
				tsObj = obj.Created
			}
			if !tsObj.IsZero() && tsObj.After(cutoff) {
				// Grace window: skip objects written recently so we never race
				// with an in-flight ingress transaction before it commits.
				continue
			}
			candidates = append(candidates, obj.Name)
		}

		if len(candidates) > 0 {
			existing, qErr := ts.ExistingTriggerEventPayloadRefs(ctx, candidates)
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

		if res.NextOffset == "" || seenOffsets[res.NextOffset] {
			break
		}
		seenOffsets[res.NextOffset] = true
		nextOffset = res.NextOffset
	}
	return swept, failed, nil
}
