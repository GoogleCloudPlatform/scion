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

package messaging

import (
	"context"
	"sync"
)

// PathRecorder collects the ordered sequence of named steps that a
// message traverses on its way from the HTTP handler to the agent
// runtime.  It is attached to a request context during tests so that
// the production path test (DEF-79) can assert the *path*, not just
// the final output.
//
// In production there is never a PathRecorder in the context, so
// RecordStep is a zero-cost no-op (one interface assertion on a
// context value).
type PathRecorder struct {
	mu    sync.Mutex
	steps []string
}

// NewPathRecorder creates a new recorder.
func NewPathRecorder() *PathRecorder {
	return &PathRecorder{}
}

// Record appends a named step.
func (r *PathRecorder) Record(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

// Steps returns the recorded steps in order.  The returned slice is a
// copy; the caller may mutate it freely.
func (r *PathRecorder) Steps() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.steps))
	copy(out, r.steps)
	return out
}

type pathRecorderKey struct{}

// ContextWithPathRecorder returns a child context carrying the recorder.
func ContextWithPathRecorder(ctx context.Context, rec *PathRecorder) context.Context {
	return context.WithValue(ctx, pathRecorderKey{}, rec)
}

// RecordStep records a named step if a PathRecorder is present in the
// context.  In production (no recorder) this is a single failed type
// assertion — effectively free.
func RecordStep(ctx context.Context, step string) {
	if rec, ok := ctx.Value(pathRecorderKey{}).(*PathRecorder); ok {
		rec.Record(step)
	}
}
