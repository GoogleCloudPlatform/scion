// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auditevent

import (
	"context"
	"sync"
)

// Sink receives validated audit envelopes.
type Sink interface {
	Emit(context.Context, EnvelopeV1) error
}

// CaptureSink is a concurrency-safe test sink that stores the exact rendered
// records which would cross the audit boundary.
type CaptureSink struct {
	mu      sync.RWMutex
	records [][]byte
}

// NewCaptureSink creates an empty capture sink.
func NewCaptureSink() *CaptureSink {
	return &CaptureSink{}
}

// Emit validates, renders, and captures one event.
func (s *CaptureSink) Emit(_ context.Context, event EnvelopeV1) error {
	record, err := Render(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.records = append(s.records, append([]byte(nil), record...))
	s.mu.Unlock()
	return nil
}

// Records returns a defensive copy of every captured serialized event.
func (s *CaptureSink) Records() [][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([][]byte, len(s.records))
	for i, record := range s.records {
		records[i] = append([]byte(nil), record...)
	}
	return records
}

var _ Sink = (*CaptureSink)(nil)
