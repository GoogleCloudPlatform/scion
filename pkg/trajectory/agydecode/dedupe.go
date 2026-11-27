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

package agydecode

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// identityFieldOrder defines the preference order when choosing a canonical
// identity key from a union-find equivalence class over protobuf IDs [11, 12, 7].
var identityFieldOrder = []int{11, 12, 7}

// DefaultMaxDedupeRoots is the default upper bound on canonical root identities
// retained by DedupeState.Prune so long-running conversations do not grow the
// persisted state file without bound. Because incremental SQLite polling reads
// strictly forward from Cursor (plus at most MaxPendingRowIDs recent NULL gaps),
// evicting the least-recently-touched roots keeps state compact without
// re-emitting already-scanned historical rows.
const DefaultMaxDedupeRoots = 4096

// DedupeState maintains a union-find (disjoint-set) index over protobuf call
// identity keys ("antigravity:<field>:<id>" for fields [11, 12, 7]) plus any
// fallback composite identities, along with the per-channel high-water mark
// of tokens already emitted for each canonical root.
//
// Across incremental polls, RecordDelta applies max-merge semantics per token
// channel: a first-seen identity emits its full TokenUsage; a later duplicate
// with larger counts (for example, a final gen_metadata row arriving after a
// partial steps row) emits only the positive per-channel delta; a later copy
// with smaller or equal counts emits nothing. This keeps incremental Track 1
// totals equal to single-pass Track 2 totals.
//
// Bridging limitation: if two earlier polls emit copies of the same call with
// completely disjoint identity key sets (for example {antigravity:11:x} in poll 1
// and {antigravity:7:z} in poll 2) and a later poll sees a bridging copy carrying
// both keys, unionRoots sums the two already-emitted high-water marks onto the
// winning root because past emissions cannot be retracted. That guarantees no
// further extra delta is emitted unless the bridging copy exceeds the sum, at
// the cost of breaking single-pass max-merge equality for that disjoint-then-bridged
// input class. In observed agy 1.5.18 shapes fields 7/11/12 appear together.
//
// Retention: long-running callers should call Prune (for example with
// DefaultMaxDedupeRoots) after committing a poll so Parent/Emitted/LastSeen do
// not grow without bound. LearnedModels and AmbiguousModels are bounded by the
// small set of distinct numeric model IDs and are never pruned.
//
// DedupeState is JSON-serializable so a live tailer can persist it across restarts.
// Callers that must survive outbox append failures should decode against Clone()
// and commit the mutated state only after the append succeeds.
type DedupeState struct {
	// Parent maps each known identity key to its union-find parent key.
	// Root keys map to themselves.
	Parent map[string]string `json:"parent,omitempty"`
	// Emitted maps each canonical root key to the highest per-channel token
	// counts emitted so far for that equivalence class.
	Emitted map[string]TokenUsage `json:"emitted,omitempty"`
	// LastSeen records the monotonic TouchSeq at which each canonical root was
	// last observed, used by Prune to evict the oldest roots first.
	LastSeen map[string]int64 `json:"last_seen,omitempty"`
	// TouchSeq is a monotonic counter incremented whenever an identity is added
	// or updated, providing deterministic LRU ordering for Prune.
	TouchSeq int64 `json:"touch_seq,omitempty"`
	// LearnedModels persists concrete numeric model_id -> model_name mappings
	// discovered across polls so later steps-only polls resolve the same model
	// names as a single full pass.
	LearnedModels map[int64]string `json:"learned_models,omitempty"`
	// AmbiguousModels tracks numeric model_ids observed with conflicting
	// concrete names across polls so they are not guessed.
	AmbiguousModels map[int64]bool `json:"ambiguous_models,omitempty"`
}

// NewDedupeState returns an initialized, empty DedupeState.
func NewDedupeState() *DedupeState {
	return &DedupeState{
		Parent:          make(map[string]string),
		Emitted:         make(map[string]TokenUsage),
		LastSeen:        make(map[string]int64),
		LearnedModels:   make(map[int64]string),
		AmbiguousModels: make(map[int64]bool),
	}
}

// Clone returns a deep copy of DedupeState (or nil if d is nil). Callers
// should pass a clone to DecodeFile and swap it in only after downstream
// outbox appends succeed so a transient append failure never suppresses
// re-emission on the next poll.
func (d *DedupeState) Clone() *DedupeState {
	if d == nil {
		return nil
	}
	out := &DedupeState{
		Parent:          make(map[string]string, len(d.Parent)),
		Emitted:         make(map[string]TokenUsage, len(d.Emitted)),
		LastSeen:        make(map[string]int64, len(d.LastSeen)),
		TouchSeq:        d.TouchSeq,
		LearnedModels:   make(map[int64]string, len(d.LearnedModels)),
		AmbiguousModels: make(map[int64]bool, len(d.AmbiguousModels)),
	}
	for k, v := range d.Parent {
		out.Parent[k] = v
	}
	for k, v := range d.Emitted {
		out.Emitted[k] = cloneTokenUsage(v)
	}
	for k, v := range d.LastSeen {
		out.LastSeen[k] = v
	}
	for k, v := range d.LearnedModels {
		out.LearnedModels[k] = v
	}
	for k, v := range d.AmbiguousModels {
		out.AmbiguousModels[k] = v
	}
	return out
}

func (d *DedupeState) ensureInit() {
	if d.Parent == nil {
		d.Parent = make(map[string]string)
	}
	if d.Emitted == nil {
		d.Emitted = make(map[string]TokenUsage)
	}
	if d.LastSeen == nil {
		d.LastSeen = make(map[string]int64)
	}
	if d.LearnedModels == nil {
		d.LearnedModels = make(map[int64]string)
	}
	if d.AmbiguousModels == nil {
		d.AmbiguousModels = make(map[int64]bool)
	}
}

// Find returns the canonical root key for k, performing path compression.
// Returns ("", false) if k has never been recorded.
func (d *DedupeState) Find(k string) (string, bool) {
	if d == nil || d.Parent == nil {
		return "", false
	}
	if _, ok := d.Parent[k]; !ok {
		return "", false
	}
	root := k
	for d.Parent[root] != root {
		root = d.Parent[root]
	}
	// Path compression.
	curr := k
	for curr != root {
		next := d.Parent[curr]
		d.Parent[curr] = root
		curr = next
	}
	return root, true
}

// Contains reports whether any key in keys has already been seen. If so, it
// returns the canonical root identity and true.
func (d *DedupeState) Contains(keys ...string) (string, bool) {
	if d == nil || len(d.Parent) == 0 {
		return "", false
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		if root, ok := d.Find(k); ok {
			return root, true
		}
	}
	return "", false
}

// Add inserts all non-empty keys into the union-find structure, unioning them
// into a single equivalence class. It returns the canonical root key and
// isNew=true if NONE of the keys were previously seen (or isNew=false if at
// least one key was already present).
func (d *DedupeState) Add(keys ...string) (canonical string, isNew bool) {
	d.ensureInit()
	clean := dedupeNonEmpty(keys)
	if len(clean) == 0 {
		return "", false
	}

	var existingRoot string
	for _, k := range clean {
		if root, ok := d.Find(k); ok {
			if existingRoot == "" {
				existingRoot = root
			} else if existingRoot != root {
				existingRoot = d.unionRoots(existingRoot, root)
			}
		}
	}

	isNew = existingRoot == ""
	root := existingRoot
	for _, k := range clean {
		if _, ok := d.Parent[k]; !ok {
			if root == "" {
				d.Parent[k] = k
				root = k
			} else {
				// Keep the deterministic preferred key as root.
				d.Parent[k] = k
				root = d.unionRoots(root, k)
			}
		}
	}
	if root != "" {
		d.touchRoot(root)
	}
	return root, isNew
}

func (d *DedupeState) touchRoot(root string) {
	d.TouchSeq++
	d.LastSeen[root] = d.TouchSeq
}

// RecordDelta unions keys into DedupeState and compares tokens against the
// canonical root's persisted per-channel high-water mark:
//   - If the identity is new, stores tokens and returns
//     (canonical, emissionKey, tokens, isDelta=false, emit=true).
//   - If the identity was already seen and at least one channel increased, updates
//     the high-water mark and returns
//     (canonical, emissionKey, positiveDelta, isDelta=true, emit=true).
//   - If no channel increased, returns (canonical, "", zero, isDelta=false, emit=false).
//
// emissionKey is deterministically derived from canonical plus the cumulative
// high-water mark after this emission, so full and delta emissions get distinct
// keys and replaying a delta after a crash reproduces the exact same key.
func (d *DedupeState) RecordDelta(tokens TokenUsage, keys ...string) (canonical, emissionKey string, out TokenUsage, isDelta, emit bool) {
	canonical, isNew := d.Add(keys...)
	if canonical == "" {
		return "", "", TokenUsage{}, false, false
	}

	prev, hasPrev := d.Emitted[canonical]
	if isNew || !hasPrev {
		hw := cloneTokenUsage(tokens)
		d.Emitted[canonical] = hw
		return canonical, computeEmissionKey(canonical, hw), cloneTokenUsage(tokens), false, true
	}

	delta, nextHW, changed := computeTokenDelta(prev, tokens)
	if !changed {
		return canonical, "", TokenUsage{}, false, false
	}
	d.Emitted[canonical] = nextHW
	return canonical, computeEmissionKey(canonical, nextHW), delta, true, true
}

func formatOptInt64(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

func computeEmissionKey(identity string, highWater TokenUsage) string {
	payload := fmt.Sprintf("%s|in=%s|out=%s|cr=%s|cw=%s|reas=%s",
		identity,
		formatOptInt64(highWater.Input),
		formatOptInt64(highWater.Output),
		formatOptInt64(highWater.CacheRead),
		formatOptInt64(highWater.CacheWrite),
		formatOptInt64(highWater.Reasoning),
	)
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%s#%x", identity, sum[:16])
}

// unionRoots merges two root keys, preferring the higher-priority key
// (field 11 > field 12 > field 7 > fallback, then lexicographically smaller)
// so canonical identities are deterministic regardless of insertion order.
// Any persisted Emitted high-water marks on both roots are summed onto the
// winner (see the bridging limitation in DedupeState's doc comment).
func (d *DedupeState) unionRoots(a, b string) string {
	if a == b {
		return a
	}
	winner, loser := a, b
	if compareIdentityKeys(a, b) > 0 {
		winner, loser = b, a
	}
	d.Parent[loser] = winner
	if loserTok, ok := d.Emitted[loser]; ok {
		if winnerTok, hasWinner := d.Emitted[winner]; hasWinner {
			d.Emitted[winner] = sumTokenUsage(winnerTok, loserTok)
		} else {
			d.Emitted[winner] = loserTok
		}
		delete(d.Emitted, loser)
	}
	if loserSeq, ok := d.LastSeen[loser]; ok {
		if loserSeq > d.LastSeen[winner] {
			d.LastSeen[winner] = loserSeq
		}
		delete(d.LastSeen, loser)
	}
	return winner
}

// Prune evicts the least-recently-touched canonical root identities (and all
// union-find aliases pointing to them) when the number of tracked roots exceeds
// maxRoots. LearnedModels and AmbiguousModels are tiny and never pruned.
// Returns the number of evicted roots. If d is nil or maxRoots <= 0, returns 0.
func (d *DedupeState) Prune(maxRoots int) int {
	if d == nil || maxRoots <= 0 || len(d.Parent) == 0 {
		return 0
	}
	// Path-compress every key and record its root before mutating d.Parent.
	rootOf := make(map[string]string, len(d.Parent))
	rootSet := make(map[string]struct{}, len(d.Emitted))
	for k := range d.Parent {
		if root, ok := d.Find(k); ok {
			rootOf[k] = root
			rootSet[root] = struct{}{}
		}
	}
	for r := range d.Emitted {
		rootSet[r] = struct{}{}
	}
	if len(rootSet) <= maxRoots {
		return 0
	}
	roots := make([]string, 0, len(rootSet))
	for r := range rootSet {
		roots = append(roots, r)
	}
	// Sort oldest-first by LastSeen, tie-breaking lexicographically for determinism.
	sort.Slice(roots, func(i, j int) bool {
		si, sj := d.LastSeen[roots[i]], d.LastSeen[roots[j]]
		if si != sj {
			return si < sj
		}
		return roots[i] < roots[j]
	})

	evictCount := len(roots) - maxRoots
	evicted := make(map[string]struct{}, evictCount)
	for _, r := range roots[:evictCount] {
		evicted[r] = struct{}{}
		delete(d.Emitted, r)
		delete(d.LastSeen, r)
	}
	for k, root := range rootOf {
		if _, drop := evicted[root]; drop {
			delete(d.Parent, k)
		}
	}
	return evictCount
}

// RootCount returns the number of distinct canonical roots tracked in Emitted.
func (d *DedupeState) RootCount() int {
	if d == nil {
		return 0
	}
	return len(d.Emitted)
}

// Size returns the number of tracked keys in the state.
func (d *DedupeState) Size() int {
	if d == nil {
		return 0
	}
	return len(d.Parent)
}

func cloneInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneBoolPtr(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneTokenUsage(t TokenUsage) TokenUsage {
	return TokenUsage{
		Input:                   cloneInt64Ptr(t.Input),
		Output:                  cloneInt64Ptr(t.Output),
		CacheRead:               cloneInt64Ptr(t.CacheRead),
		CacheWrite:              cloneInt64Ptr(t.CacheWrite),
		Reasoning:               cloneInt64Ptr(t.Reasoning),
		ReasoningOverlapsOutput: cloneBoolPtr(t.ReasoningOverlapsOutput),
		InputDerivation:         t.InputDerivation,
	}
}

func sumInt64Ptr(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	var total int64
	if a != nil {
		total += *a
	}
	if b != nil {
		total += *b
	}
	return &total
}

func sumTokenUsage(a, b TokenUsage) TokenUsage {
	out := TokenUsage{
		Input:      sumInt64Ptr(a.Input, b.Input),
		Output:     sumInt64Ptr(a.Output, b.Output),
		CacheRead:  sumInt64Ptr(a.CacheRead, b.CacheRead),
		CacheWrite: sumInt64Ptr(a.CacheWrite, b.CacheWrite),
		Reasoning:  sumInt64Ptr(a.Reasoning, b.Reasoning),
	}
	if out.Input != nil {
		out.InputDerivation = telemetrycontract.InputDerivationReported
	}
	if out.Reasoning != nil && out.Output != nil {
		overlap := true
		out.ReasoningOverlapsOutput = &overlap
	}
	return out
}

func channelDelta(prev, incoming *int64) (delta *int64, nextHW *int64, increased bool) {
	if incoming == nil {
		return nil, cloneInt64Ptr(prev), false
	}
	var prevVal int64
	if prev != nil {
		prevVal = *prev
	}
	if *incoming > prevVal {
		d := *incoming - prevVal
		hw := *incoming
		return &d, &hw, true
	}
	return nil, cloneInt64Ptr(prev), false
}

func computeTokenDelta(prev, incoming TokenUsage) (delta TokenUsage, nextHW TokenUsage, changed bool) {
	var chIn, chOut, chCR, chCW, chReas bool
	delta.Input, nextHW.Input, chIn = channelDelta(prev.Input, incoming.Input)
	delta.Output, nextHW.Output, chOut = channelDelta(prev.Output, incoming.Output)
	delta.CacheRead, nextHW.CacheRead, chCR = channelDelta(prev.CacheRead, incoming.CacheRead)
	delta.CacheWrite, nextHW.CacheWrite, chCW = channelDelta(prev.CacheWrite, incoming.CacheWrite)
	delta.Reasoning, nextHW.Reasoning, chReas = channelDelta(prev.Reasoning, incoming.Reasoning)

	changed = chIn || chOut || chCR || chCW || chReas
	if !changed {
		return TokenUsage{}, prev, false
	}
	if delta.Input != nil {
		delta.InputDerivation = telemetrycontract.InputDerivationReported
	}
	if delta.Reasoning != nil {
		overlap := true
		delta.ReasoningOverlapsOutput = &overlap
	}
	if nextHW.Input != nil {
		nextHW.InputDerivation = telemetrycontract.InputDerivationReported
	}
	if nextHW.Reasoning != nil && nextHW.Output != nil {
		overlap := true
		nextHW.ReasoningOverlapsOutput = &overlap
	}
	return delta, nextHW, true
}

func formatProtoIdentityKey(fieldNum int, id string) string {
	return fmt.Sprintf("antigravity:%d:%s", fieldNum, id)
}

func identityKeyPriority(k string) int {
	for i, fn := range identityFieldOrder {
		prefix := fmt.Sprintf("antigravity:%d:", fn)
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			return i
		}
	}
	return len(identityFieldOrder)
}

func compareIdentityKeys(a, b string) int {
	pa, pb := identityKeyPriority(a), identityKeyPriority(b)
	if pa != pb {
		if pa < pb {
			return -1
		}
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func dedupeNonEmpty(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		return compareIdentityKeys(out[i], out[j]) < 0
	})
	return out
}
