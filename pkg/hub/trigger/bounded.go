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

import "container/list"

// boundedEvictProbes bounds the number of list-head inspections per insertion
// when a conditional-eviction table (such as a keyed token-bucket limiter) is
// at capacity. Un-evictable entries are rotated to the tail (clock / second
// chance) so subsequent insertions advance across the table in O(1) work per
// request without ever evicting an active bucket.
const boundedEvictProbes = 8

type boundedEntry[K comparable, V any] struct {
	key K
	val V
}

// boundedTable is an insertion/LRU-ordered map with O(1) lookup, update,
// deletion, and bounded-capacity eviction shared across ingress rate-limiter,
// endpoint/secret/negative cache, and failure-audit tables. Callers synchronize
// access with their own mutex.
type boundedTable[K comparable, V any] struct {
	items   map[K]*list.Element
	order   list.List // Front() = oldest, Back() = newest
	maxSize int
	onScan  func(steps int)
	onEvict func(key K, val V)
}

func newBoundedTable[K comparable, V any](maxSize int, onScan func(int), onEvict func(K, V)) *boundedTable[K, V] {
	return &boundedTable[K, V]{
		items:   make(map[K]*list.Element),
		maxSize: maxSize,
		onScan:  onScan,
		onEvict: onEvict,
	}
}

func (t *boundedTable[K, V]) recordScan(steps int) {
	if t.onScan != nil {
		t.onScan(steps)
	}
}

func (t *boundedTable[K, V]) len() int {
	return len(t.items)
}

func (t *boundedTable[K, V]) get(key K) (V, bool) {
	elem, ok := t.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	return elem.Value.(boundedEntry[K, V]).val, true
}

func (t *boundedTable[K, V]) touch(key K) {
	if elem, ok := t.items[key]; ok {
		t.order.MoveToBack(elem)
	}
}

func (t *boundedTable[K, V]) setInPlace(key K, val V) {
	if elem, ok := t.items[key]; ok {
		elem.Value = boundedEntry[K, V]{key: key, val: val}
	}
}

func (t *boundedTable[K, V]) removeElement(elem *list.Element) {
	entry := elem.Value.(boundedEntry[K, V])
	delete(t.items, entry.key)
	t.order.Remove(elem)
	if t.onEvict != nil {
		t.onEvict(entry.key, entry.val)
	}
}

func (t *boundedTable[K, V]) delete(key K) bool {
	elem, ok := t.items[key]
	if !ok {
		return false
	}
	t.removeElement(elem)
	return true
}

// putEvictOldest inserts or updates key -> val at the newest end of the table,
// evicting the oldest entry at Front() in O(1) when at capacity.
func (t *boundedTable[K, V]) putEvictOldest(key K, val V) {
	if elem, ok := t.items[key]; ok {
		elem.Value = boundedEntry[K, V]{key: key, val: val}
		t.order.MoveToBack(elem)
		t.recordScan(0)
		return
	}
	if t.maxSize <= 0 {
		t.recordScan(0)
		return
	}
	steps := 0
	for len(t.items) >= t.maxSize {
		front := t.order.Front()
		if front == nil {
			break
		}
		steps++
		t.removeElement(front)
	}
	elem := t.order.PushBack(boundedEntry[K, V]{key: key, val: val})
	t.items[key] = elem
	t.recordScan(steps)
}

// tryPutEvictWhere inserts a new key -> val at the newest end of the table.
// When at capacity, it probes at most maxProbes entries from Front(), evicting
// the first entry satisfying canEvict and rotating non-evictable entries to
// Back() (clock / second chance) so per-request work is strictly O(1). Returns
// false without inserting when the table remains full after bounded probing.
func (t *boundedTable[K, V]) tryPutEvictWhere(key K, val V, maxProbes int, canEvict func(K, V) bool) bool {
	if t.maxSize <= 0 {
		t.recordScan(0)
		return false
	}
	steps := 0
	if len(t.items) >= t.maxSize {
		limit := min(maxProbes, len(t.items))
		for range limit {
			front := t.order.Front()
			if front == nil {
				break
			}
			steps++
			entry := front.Value.(boundedEntry[K, V])
			if canEvict(entry.key, entry.val) {
				t.removeElement(front)
				break
			}
			t.order.MoveToBack(front)
		}
		if len(t.items) >= t.maxSize {
			t.recordScan(steps)
			return false
		}
	}
	elem := t.order.PushBack(boundedEntry[K, V]{key: key, val: val})
	t.items[key] = elem
	t.recordScan(steps)
	return true
}
