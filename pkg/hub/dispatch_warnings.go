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

import (
	"context"
	"sync"
)

// dispatchWarnings collects user-facing warnings raised while a request is
// dispatched to a runtime broker. The AgentDispatcher methods return only an
// error, so a handler that wants to surface these warnings in its response
// attaches a collector to the request context with withDispatchWarnings and
// reads it after the dispatch returns.
type dispatchWarnings struct {
	mu   sync.Mutex
	list []string
}

type dispatchWarningsKey struct{}

// withDispatchWarnings returns a context carrying a fresh warnings collector,
// and the collector itself.
func withDispatchWarnings(ctx context.Context) (context.Context, *dispatchWarnings) {
	w := &dispatchWarnings{}
	return context.WithValue(ctx, dispatchWarningsKey{}, w), w
}

// addDispatchWarnings appends warnings to the collector carried by ctx, if
// any. Without a collector the warnings are dropped; callers log anything
// that must not be lost.
func addDispatchWarnings(ctx context.Context, warnings ...string) {
	if len(warnings) == 0 || ctx == nil {
		return
	}
	w, _ := ctx.Value(dispatchWarningsKey{}).(*dispatchWarnings)
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range warnings {
		if s == "" {
			continue
		}
		dup := false
		for _, have := range w.list {
			if have == s {
				dup = true
				break
			}
		}
		if !dup {
			w.list = append(w.list, s)
		}
	}
}

// Warnings returns a copy of the collected warnings in the order added.
func (w *dispatchWarnings) Warnings() []string {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.list...)
}

// dispatchWarningsFromContext returns a copy of the warnings collected by the
// collector carried by ctx, or nil when there is none.
func dispatchWarningsFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	w, _ := ctx.Value(dispatchWarningsKey{}).(*dispatchWarnings)
	return w.Warnings()
}
