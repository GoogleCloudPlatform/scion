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

package telemetryemit

import (
	"context"
	"sync"
)

// MemorySink records delivered payloads, for tests. FailFunc, when set, is
// consulted before each delivery with the payload and its 1-based attempt
// number for that exact payload; a non-nil error fails the delivery and
// nothing is recorded.
type MemorySink struct {
	mu        sync.Mutex
	delivered [][]byte
	attempts  map[string]int

	// FailFunc is read under the sink's lock; set it before use or with
	// SetFailFunc.
	FailFunc func(payload []byte, attempt int) error
}

// NewMemorySink returns an empty MemorySink.
func NewMemorySink() *MemorySink { return &MemorySink{attempts: map[string]int{}} }

// SetFailFunc replaces FailFunc safely while the sink is in use.
func (m *MemorySink) SetFailFunc(f func(payload []byte, attempt int) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.FailFunc = f
}

// Emit implements Sink.
func (m *MemorySink) Emit(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return Transient("context", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attempts == nil {
		m.attempts = map[string]int{}
	}
	m.attempts[string(payload)]++
	if m.FailFunc != nil {
		if err := m.FailFunc(payload, m.attempts[string(payload)]); err != nil {
			return err
		}
	}
	m.delivered = append(m.delivered, append([]byte(nil), payload...))
	return nil
}

// Delivered returns copies of the delivered payloads in delivery order.
func (m *MemorySink) Delivered() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.delivered))
	for i, p := range m.delivered {
		out[i] = append([]byte(nil), p...)
	}
	return out
}

// Attempts returns how many times payload was offered to the sink.
func (m *MemorySink) Attempts(payload []byte) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attempts[string(payload)]
}
