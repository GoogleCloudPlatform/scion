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
)

// identityFieldOrder defines the preference order when choosing a canonical
// identity key from a union-find equivalence class over protobuf IDs [11, 12, 7].
var identityFieldOrder = []int{11, 12, 7}

// DedupeState maintains a union-find (disjoint-set) index over protobuf call
// identity keys ("antigravity:<field>:<id>" for fields [11, 12, 7]) plus any
// fallback composite identities.
//
// It is JSON-serializable so a live Track 1 tailer can persist seen call
// identities across restarts and avoid double-counting when the same call
// appears across gen_metadata, retry arrays, and steps.
type DedupeState struct {
	// Parent maps each known identity key to its union-find parent key.
	// Root keys map to themselves.
	Parent map[string]string `json:"parent,omitempty"`
}

// NewDedupeState returns an initialized, empty DedupeState.
func NewDedupeState() *DedupeState {
	return &DedupeState{
		Parent: make(map[string]string),
	}
}

func (d *DedupeState) ensureInit() {
	if d.Parent == nil {
		d.Parent = make(map[string]string)
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

// unionRoots merges two root keys, preferring the higher-priority key
// (field 11 > field 12 > field 7 > fallback, then lexicographically smaller)
// so canonical identities are deterministic regardless of insertion order.
func (d *DedupeState) unionRoots(a, b string) string {
	if a == b {
		return a
	}
	if compareIdentityKeys(a, b) <= 0 {
		d.Parent[b] = a
		return a
	}
	d.Parent[a] = b
	return b
}

// Size returns the number of tracked keys in the state.
func (d *DedupeState) Size() int {
	if d == nil {
		return 0
	}
	return len(d.Parent)
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
