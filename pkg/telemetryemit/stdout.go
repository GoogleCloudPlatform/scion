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
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
)

// StdoutSink writes each payload as one JSON line, for workstations and
// development. It writes to any io.Writer (normally os.Stdout).
type StdoutSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewStdoutSink returns a sink writing JSON lines to w.
func NewStdoutSink(w io.Writer) *StdoutSink { return &StdoutSink{w: w} }

// Emit implements Sink. A payload containing a newline would break the
// one-event-per-line framing and is rejected as permanent (Marshal never
// produces one).
func (s *StdoutSink) Emit(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return Transient("context", err)
	}
	if bytes.ContainsAny(payload, "\r\n") {
		return Permanent(ReasonInvalidPayload, errors.New("payload contains a line break"))
	}
	line := make([]byte, 0, len(payload)+1)
	line = append(append(line, payload...), '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(line); err != nil {
		return Transient("write", err)
	}
	return nil
}
