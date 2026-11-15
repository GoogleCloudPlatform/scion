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

package commands

import (
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/liveness"
)

// liveWorkFreshness is how long one live-work sample stands for the
// container's state: three heartbeats, as the Hub's own freshness window
// (store.LiveWorkFreshness).
const liveWorkFreshness = 3 * hub.DefaultHeartbeatInterval

// liveWorkRecorder shares the heartbeat's live-work samples with the
// harness_error API-error watchdog (ptone/scion#7855, ptone/scion#7858).
// The liveness sampler compares CPU times between successive samples and is
// not safe for concurrent use, so it is sampled only by the heartbeat loop;
// the watchdog reads the latest result instead of sampling again.
type liveWorkRecorder struct {
	mu   sync.Mutex
	now  func() time.Time
	at   time.Time
	work *liveness.LiveWork
}

func newLiveWorkRecorder() *liveWorkRecorder {
	return &liveWorkRecorder{now: time.Now}
}

// record wraps a heartbeat live-work provider so every sample it returns is
// also recorded.
func (r *liveWorkRecorder) record(sample func() *liveness.LiveWork) func() *liveness.LiveWork {
	return func() *liveness.LiveWork {
		work := sample()
		r.mu.Lock()
		r.at, r.work = r.now(), work
		r.mu.Unlock()
		return work
	}
}

// Live reports whether the latest sample, taken within liveWorkFreshness,
// found live work of any kind: a tool call (capped or not) or CPU work. A
// running tool or background job means the agent's turn is not over, so
// the API-error watchdog must not call it a harness error. With no sample
// yet (no Hub heartbeat), it is false: the watchdog behaves as it did
// before live-work detection.
func (r *liveWorkRecorder) Live() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.work != nil && r.now().Sub(r.at) < liveWorkFreshness
}
