/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"golang.org/x/sys/unix"
)

// maxMuseTrackedTurns bounds the number of (session_id, turn_id) high-water
// marks retained for Muse Code's cumulative PostLLMCall.usage delta tracking.
// Each UUID-keyed entry is ~220 bytes across Order and Turns, so 1024 entries
// cap the state file at ~225 KiB (well under the 1 MiB read limit). Eviction
// is LRU (least-recently-touched), so an active main turn stays resident even
// when more than maxMuseTrackedTurns subagent/reminder turns interleave with
// it. Eviction trades a bounded state file for a possible over-count (if more
// than maxMuseTrackedTurns other turns run between two consecutive steps of
// one idle turn, its next step re-emits its cumulative total), never an
// under-count.
const maxMuseTrackedTurns = 1024

type museTurnUsageTotals struct {
	Input      int64 `json:"input,omitempty"`
	Output     int64 `json:"output,omitempty"`
	Cached     int64 `json:"cached,omitempty"`
	CacheWrite int64 `json:"cache_write,omitempty"`
	Reasoning  int64 `json:"reasoning,omitempty"`
}

type museTurnUsageState struct {
	Order []string                       `json:"order,omitempty"`
	Turns map[string]museTurnUsageTotals `json:"turns,omitempty"`
}

// apply computes the per-step non-negative delta from cur against the stored
// cumulative high-water mark for key, advances the high-water mark, and marks
// key as most-recently-used so active turns are never evicted by newer idle
// subagent turns.
func (s *museTurnUsageState) apply(key string, cur museTurnUsageTotals) museTurnUsageTotals {
	if s.Turns == nil {
		s.Turns = make(map[string]museTurnUsageTotals)
	}
	prev, exists := s.Turns[key]
	delta := museTurnUsageTotals{
		Input:      nonNegDelta(cur.Input, prev.Input),
		Output:     nonNegDelta(cur.Output, prev.Output),
		Cached:     nonNegDelta(cur.Cached, prev.Cached),
		CacheWrite: nonNegDelta(cur.CacheWrite, prev.CacheWrite),
		Reasoning:  nonNegDelta(cur.Reasoning, prev.Reasoning),
	}
	s.Turns[key] = museTurnUsageTotals{
		Input:      maxInt64(prev.Input, cur.Input),
		Output:     maxInt64(prev.Output, cur.Output),
		Cached:     maxInt64(prev.Cached, cur.Cached),
		CacheWrite: maxInt64(prev.CacheWrite, cur.CacheWrite),
		Reasoning:  maxInt64(prev.Reasoning, cur.Reasoning),
	}
	if exists {
		// Move key to the tail of Order (most-recently-used).
		for i, k := range s.Order {
			if k == key {
				copy(s.Order[i:], s.Order[i+1:])
				s.Order[len(s.Order)-1] = key
				break
			}
		}
	} else {
		s.Order = append(s.Order, key)
		for len(s.Order) > maxMuseTrackedTurns {
			oldest := s.Order[0]
			s.Order = s.Order[1:]
			delete(s.Turns, oldest)
		}
	}
	return delta
}

func nonNegDelta(cur, prev int64) int64 {
	if cur <= prev {
		return 0
	}
	return cur - prev
}

func maxInt64(a, b int64) int64 {
	if a >= b {
		return a
	}
	return b
}

// normalizeMuseTurnUsage converts Muse Code's turn-cumulative PostLLMCall.usage
// into a per-step canonical increment keyed by (session_id, turn_id).
//
// Version note: cumulative-per-turn PostLLMCall.usage semantics were verified
// live against muse 1.4.25 (1.4.25-R9361.1). Because harnesses/muse-code/Dockerfile
// does not pin a muse version, these semantics must be re-verified on muse
// upgrades: if a future release changes PostLLMCall.usage to per-step counts,
// this high-water subtraction would under-count steps after step 0.
//
// Out-of-order hooks: if step N+1's hook process acquires the lock before
// step N's, the monotonic high-water mark keeps summed turn totals exact,
// attributing the combined increment to N+1 and leaving N with zero/not_reported
// tokens (never producing negative deltas).
//
// Failure mode: when cross-process state persistence is configured or required
// (MuseTurnUsageStatePath != "" or MuseTurnUsageStateRequired == true) and the
// state file cannot be used (empty path, mkdir/open/flock/read/write/rename
// failure), this fails closed on token counts: it logs a warning, zeroes all
// token fields on the copied event so turn-cumulative totals are never
// published as a step delta, and keeps the model-end call (token_status =
// not_reported).
//
// Returns event unchanged for non-muse-code events, non-model-end events, or
// payloads without a turn_id. Otherwise returns a shallow copy of event with
// delta-adjusted token fields so callers' Event structs are never mutated.
func (h *TelemetryHandler) normalizeMuseTurnUsage(event *hooks.Event) *hooks.Event {
	if event == nil || event.Dialect != "muse-code" || event.Name != hooks.EventModelEnd {
		return event
	}
	turnID := rawScalar(event.Data.Raw, "turn_id")
	if turnID == "" {
		return event
	}
	key := turnID
	if event.Data.SessionID != "" {
		key = event.Data.SessionID + ":" + turnID
	}
	cur := museTurnUsageTotals{
		Input:      event.Data.InputTokens,
		Output:     event.Data.OutputTokens,
		Cached:     event.Data.CachedTokens,
		CacheWrite: event.Data.CacheWriteTokens,
		Reasoning:  event.Data.ReasoningTokens,
	}

	var delta museTurnUsageTotals
	var failedClosed bool
	switch {
	case h.MuseTurnUsageStatePath != "":
		var err error
		delta, err = applyMuseTurnUsageFile(h.MuseTurnUsageStatePath, key, cur)
		if err != nil {
			log.Warn("Muse turn usage delta state file %q failed (%v); dropping tokens for untrackable turn %s", h.MuseTurnUsageStatePath, err, key)
			failedClosed = true
		}
	case h.MuseTurnUsageStateRequired:
		log.Warn("Muse turn usage state path is unavailable (empty home); dropping tokens for untrackable turn %s", key)
		failedClosed = true
	default:
		delta = h.applyMuseTurnUsageMem(key, cur)
	}

	copied := *event
	if failedClosed {
		copied.Data.ClearTokens()
		return &copied
	}
	// Failed steps in Muse Code repeat the prior step's cumulative usage,
	// yielding all-zero deltas. Clear token presence so failed calls report
	// token_status="not_reported" with nil token pointers rather than
	// fabricating measured zeros.
	if event.Data.Error != "" && delta == (museTurnUsageTotals{}) {
		copied.Data.ClearTokens()
		return &copied
	}
	copied.Data.InputTokens = delta.Input
	copied.Data.OutputTokens = delta.Output
	copied.Data.CachedTokens = delta.Cached
	copied.Data.CacheWriteTokens = delta.CacheWrite
	copied.Data.ReasoningTokens = delta.Reasoning
	copied.Data.TokenPresence = hooks.TokenPresence{
		Input:      event.Data.HasInputTokens(),
		Output:     event.Data.HasOutputTokens(),
		Cached:     event.Data.HasCachedTokens(),
		CacheWrite: event.Data.HasCacheWriteTokens(),
		Reasoning:  event.Data.HasReasoningTokens(),
	}
	return &copied
}

func (h *TelemetryHandler) applyMuseTurnUsageMem(key string, cur museTurnUsageTotals) museTurnUsageTotals {
	h.turnUsageMu.Lock()
	defer h.turnUsageMu.Unlock()
	if h.turnUsageMem == nil {
		h.turnUsageMem = &museTurnUsageState{}
	}
	return h.turnUsageMem.apply(key, cur)
}

func applyMuseTurnUsageFile(path, key string, cur museTurnUsageTotals) (museTurnUsageTotals, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return museTurnUsageTotals{}, err
	}

	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return museTurnUsageTotals{}, err
	}
	defer func() { _ = lockFile.Close() }()

	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return museTurnUsageTotals{}, err
	}
	defer func() { _ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN) }()

	var st museTurnUsageState
	dataFile, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err == nil {
		raw, readErr := io.ReadAll(io.LimitReader(dataFile, 1<<20))
		_ = dataFile.Close()
		if readErr != nil {
			return museTurnUsageTotals{}, readErr
		}
		if len(raw) > 0 {
			if unmarshalErr := json.Unmarshal(raw, &st); unmarshalErr != nil && !errors.Is(unmarshalErr, io.EOF) {
				// Corrupt state file: reset rather than failing the hook.
				log.Warn("Muse turn usage state file %q corrupt (%v); resetting", path, unmarshalErr)
				st = museTurnUsageState{}
			}
		}
	} else if !os.IsNotExist(err) {
		return museTurnUsageTotals{}, err
	}

	delta := st.apply(key, cur)

	encoded, err := json.Marshal(st)
	if err != nil {
		return museTurnUsageTotals{}, err
	}

	tmp, err := os.CreateTemp(dir, ".muse-turn-usage-*.tmp")
	if err != nil {
		return museTurnUsageTotals{}, err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return museTurnUsageTotals{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return museTurnUsageTotals{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return museTurnUsageTotals{}, err
	}
	return delta, nil
}
