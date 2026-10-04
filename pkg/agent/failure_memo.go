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

package agent

import (
	"sync"
	"time"
)

const (
	// FailureMemoTTL is how long a FailureMemo remembers a failure. It is
	// kept short because the cause can be fixed at any time (a skill path is
	// pushed, access to a repository is granted).
	FailureMemoTTL = time.Minute

	// FailureMemoMaxEntries bounds how many failures a FailureMemo holds at
	// once. When the limit is reached after dropping expired entries, a new
	// failure is not remembered; not remembering is always safe, it only
	// means the next resolution asks upstream again.
	FailureMemoMaxEntries = 1024
)

// FailureMemo remembers recent resolution failures by key for
// FailureMemoTTL, in memory only, holding at most FailureMemoMaxEntries
// unexpired entries. Both the broker's GitHubResolutionCache and the Hub's
// gh:// resolution use it for GitHub not-found results. The zero value is
// ready to use and safe for concurrent use.
type FailureMemo struct {
	mu      sync.Mutex
	entries map[string]memoEntry
}

type memoEntry struct {
	err       error
	expiresAt time.Time
}

// Record remembers err for key until FailureMemoTTL from now. An existing
// key is always refreshed. A new key at FailureMemoMaxEntries entries first
// drops the expired ones; if the memo is still full of unexpired failures,
// the new failure is not remembered. Below the limit, expired entries are
// left in place: Recent never serves them and drops them when looked up.
func (m *FailureMemo) Record(key string, err error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[string]memoEntry)
	}
	if _, ok := m.entries[key]; !ok && len(m.entries) >= FailureMemoMaxEntries {
		for k, e := range m.entries {
			if !now.Before(e.expiresAt) {
				delete(m.entries, k)
			}
		}
		if len(m.entries) >= FailureMemoMaxEntries {
			return
		}
	}
	m.entries[key] = memoEntry{err: err, expiresAt: now.Add(FailureMemoTTL)}
}

// Recent returns the failure remembered for key, or nil if there is none or
// it has expired. An expired entry is dropped.
func (m *FailureMemo) Recent(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil
	}
	if !time.Now().Before(e.expiresAt) {
		delete(m.entries, key)
		return nil
	}
	return e.err
}

// Clear forgets any failure remembered for key.
func (m *FailureMemo) Clear(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

// Len returns how many failures are held, including expired ones not yet
// dropped.
func (m *FailureMemo) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
