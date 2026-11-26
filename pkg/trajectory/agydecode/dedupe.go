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
	"fmt"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// identityFieldOrder defines the preference order when choosing a canonical
// identity key from a union-find equivalence class over protobuf IDs [11, 12, 7].
var identityFieldOrder = []int{11, 12, 7}

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
// DedupeState is JSON-serializable so a live tailer can persist it across restarts.
type DedupeState struct {
	// Parent maps each known identity key to its union-find parent key.
	// Root keys map to themselves.
	Parent map[string]string `json:"parent,omitempty"`
	// Emitted maps each canonical root key to the highest per-channel token
	// counts emitted so far for that equivalence class.
	Emitted map[string]TokenUsage `json:"emitted,omitempty"`
}

// NewDedupeState returns an initialized, empty DedupeState.
func NewDedupeState() *DedupeState {
	return &DedupeState{
		Parent:  make(map[string]string),
		Emitted: make(map[string]TokenUsage),
	}
}

func (d *DedupeState) ensureInit() {
	if d.Parent == nil {
		d.Parent = make(map[string]string)
	}
	if d.Emitted == nil {
		d.Emitted = make(map[string]TokenUsage)
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
	return root, isNew
}

// RecordDelta unions keys into DedupeState and compares tokens against the
// canonical root's persisted per-channel high-water mark:
//   - If the identity is new, stores tokens and returns (canonical, tokens, isDelta=false, emit=true).
//   - If the identity was already seen and at least one channel increased, updates
//     the high-water mark and returns (canonical, positiveDelta, isDelta=true, emit=true).
//   - If no channel increased, returns (canonical, zero, isDelta=false, emit=false).
func (d *DedupeState) RecordDelta(tokens TokenUsage, keys ...string) (canonical string, out TokenUsage, isDelta bool, emit bool) {
	canonical, isNew := d.Add(keys...)
	if canonical == "" {
		return "", TokenUsage{}, false, false
	}

	prev, hasPrev := d.Emitted[canonical]
	if isNew || !hasPrev {
		d.Emitted[canonical] = cloneTokenUsage(tokens)
		return canonical, cloneTokenUsage(tokens), false, true
	}

	delta, nextHW, changed := computeTokenDelta(prev, tokens)
	if !changed {
		return canonical, TokenUsage{}, false, false
	}
	d.Emitted[canonical] = nextHW
	return canonical, delta, true, true
}

// unionRoots merges two root keys, preferring the higher-priority key
// (field 11 > field 12 > field 7 > fallback, then lexicographically smaller)
// so canonical identities are deterministic regardless of insertion order.
// Any persisted Emitted high-water marks on both roots are combined onto the winner.
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
	return winner
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
