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
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// maxMuseTrackedTurns bounds the number of (session_id, turn_id) high-water
// marks retained for Muse Code's cumulative PostLLMCall.usage delta tracking.
// A turn finishes before the next main turn begins, so 64 slots easily cover
// the main session plus concurrent reminder/subagent turns.
const maxMuseTrackedTurns = 64

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
// cumulative high-water mark for key, and advances the high-water mark.
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
	if !exists {
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
// Verified live against muse 1.4.25-R9361.1: within a single turn_id, step 0
// reports step 0's usage, step 1 reports step 0 + step 1, and a failed step 1
// repeats step 0's cumulative totals. Separate turns (and subagent turns) have
// distinct turn_ids and start fresh at 0.
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
	if h.TurnUsageStatePath != "" {
		var err error
		delta, err = applyMuseTurnUsageFile(h.TurnUsageStatePath, key, cur)
		if err != nil {
			log.Debug("Muse turn usage delta state file failed (%v); falling back to in-memory delta", err)
			delta = h.applyMuseTurnUsageMem(key, cur)
		}
	} else {
		delta = h.applyMuseTurnUsageMem(key, cur)
	}

	copied := *event
	copied.Data.InputTokens = delta.Input
	copied.Data.OutputTokens = delta.Output
	copied.Data.CachedTokens = delta.Cached
	copied.Data.CacheWriteTokens = delta.CacheWrite
	copied.Data.ReasoningTokens = delta.Reasoning
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return museTurnUsageTotals{}, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return museTurnUsageTotals{}, err
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return museTurnUsageTotals{}, err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return museTurnUsageTotals{}, err
	}
	var st museTurnUsageState
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &st); err != nil && !errors.Is(err, io.EOF) {
			// Corrupt state file: reset rather than failing the hook.
			st = museTurnUsageState{}
		}
	}
	delta := st.apply(key, cur)

	encoded, err := json.Marshal(st)
	if err != nil {
		return museTurnUsageTotals{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return museTurnUsageTotals{}, err
	}
	if err := f.Truncate(0); err != nil {
		return museTurnUsageTotals{}, err
	}
	if _, err := f.Write(encoded); err != nil {
		return museTurnUsageTotals{}, err
	}
	return delta, nil
}
