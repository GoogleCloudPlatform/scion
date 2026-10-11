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

package hub

import "sync"

// inFlightCall is one in-flight probe shared by every caller that asks for
// the same key while it runs. res is written before done is closed and read
// only after it is closed.
type inFlightCall[T any] struct {
	done chan struct{}
	res  T
}

// joinOrStartInFlight returns the call in flight for key in m, or stores a
// new one and runs it. It is the dedupe shared by the workspace probes
// (probeWorkspaceContent and startWorkspaceHealthProbe): on a hung mount a
// probe never returns and holds an OS thread in the syscall, so there is at
// most one stuck probe per key and later callers wait on it, each with its
// own timeout. Each caller keeps its own map, key type and result type, and
// owns its timeout and error mapping.
//
// start is called only when no call is in flight for key, after the new call
// is stored and before its goroutine starts, so a caller reads its swappable
// package-level seams there and a joining caller reads none. It returns the
// probe to run and an optional beforeDone hook. The goroutine stores run's
// result, removes the entry with CompareAndDelete, calls beforeDone (when
// non-nil), and then closes done, so the first caller after done fires
// starts a fresh probe.
func joinOrStartInFlight[T any](m *sync.Map, key any, start func() (run func() T, beforeDone func())) *inFlightCall[T] {
	call := &inFlightCall[T]{done: make(chan struct{})}
	if existing, loaded := m.LoadOrStore(key, call); loaded {
		return existing.(*inFlightCall[T])
	}
	run, beforeDone := start()
	go func(c *inFlightCall[T]) {
		c.res = run()
		m.CompareAndDelete(key, c)
		if beforeDone != nil {
			beforeDone()
		}
		close(c.done)
	}(call)
	return call
}
