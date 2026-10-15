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

package runtimebroker

import (
	"net/http"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// retainedPurgeGuard keeps a purge of an agent's retained files
// (ptone/scion#4744) and a start, restart or create of the same agent from
// running at the same time on this broker. No start path takes the
// agent-files lock the purge holds, so without it a start that arrives
// while the purge is removing the files would provision or launch on a
// half-removed home or workspace.
//
// Both sides register under the guard's one lock, so exactly one of them
// sees the other:
//   - a start, restart or create calls beginProvision when its handler is
//     entered and holds the entry until the handler returns; it is refused
//     while a purge of its key is registered;
//   - a purge calls beginPurge before it checks anything and holds the
//     entry until it returns; it is refused while a provision of one of
//     its keys is registered. Work a create hands over to an async launch
//     outlives the handler, but stays registered in the launch registry,
//     which the purge also checks after beginPurge.
//
// Keys are launchKey values (project ID, slug), as the launch registry and
// start tracker use. A nil guard (a Server built directly in a test)
// guards nothing.
type retainedPurgeGuard struct {
	mu         sync.Mutex
	purges     map[launchKey]int
	provisions map[launchKey]int
}

func newRetainedPurgeGuard() *retainedPurgeGuard {
	return &retainedPurgeGuard{
		purges:     make(map[launchKey]int),
		provisions: make(map[launchKey]int),
	}
}

// beginPurge registers a purge under keys unless a provision of any of them
// is registered. It reports whether it registered; the release func, which
// must be called exactly once when ok, removes the registration.
func (g *retainedPurgeGuard) beginPurge(keys ...launchKey) (release func(), ok bool) {
	if g == nil {
		return func() {}, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range keys {
		if g.provisions[k] > 0 {
			return func() {}, false
		}
	}
	for _, k := range keys {
		g.purges[k]++
	}
	return g.releaseFunc(g.purges, keys), true
}

// beginProvision registers a start, restart or create under key unless a
// purge of key is registered. It reports whether it registered; the release
// func, which must be called exactly once when ok, removes the
// registration.
func (g *retainedPurgeGuard) beginProvision(key launchKey) (release func(), ok bool) {
	if g == nil {
		return func() {}, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.purges[key] > 0 {
		return func() {}, false
	}
	g.provisions[key]++
	return g.releaseFunc(g.provisions, []launchKey{key}), true
}

// releaseFunc returns a func that removes one registration of each key from
// m, once.
func (g *retainedPurgeGuard) releaseFunc(m map[launchKey]int, keys []launchKey) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			for _, k := range keys {
				if m[k] <= 1 {
					delete(m, k)
				} else {
					m[k]--
				}
			}
		})
	}
}

// retainedPurgeRetryAfter is the Retry-After (seconds) of the answer to a
// start refused because a purge of the agent's retained files is running.
const retainedPurgeRetryAfter = "5"

// retainedPurgeInProgressReason is the reason a start, restart or create
// refused by the guard carries.
const retainedPurgeInProgressReason = "a removal of the agent's retained files is in progress on this broker"

// RetainedPurgeInProgress writes the retryable 409 answer, with
// api.BrokerErrCodeRetainedPurgeInProgress and a Retry-After header, to a
// start, restart or create refused because a purge of the agent's retained
// files is running. Nothing was started or written.
func RetainedPurgeInProgress(w http.ResponseWriter, op string) {
	w.Header().Set("Retry-After", retainedPurgeRetryAfter)
	writeError(w, http.StatusConflict, api.BrokerErrCodeRetainedPurgeInProgress,
		"Failed to "+op+" agent: "+retainedPurgeInProgressReason+"; retry shortly",
		map[string]interface{}{api.BrokerErrDetailReason: retainedPurgeInProgressReason})
}
