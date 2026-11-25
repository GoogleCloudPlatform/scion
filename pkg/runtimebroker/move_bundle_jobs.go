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
	"context"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
)

// defaultMoveBundleJobTTL is how long completed/failed move-bundle jobs are
// retained in memory before garbage collection (design §3.5).
const defaultMoveBundleJobTTL = time.Hour

type moveBundleAgentKey struct {
	ProjectID string
	AgentSlug string
	Kind      string // "export" | "import"
}

type moveBundleJobKey struct {
	Agent           moveBundleAgentKey
	ReincarnationID string
}

type moveBundleJob struct {
	mu         sync.Mutex
	key        moveBundleJobKey
	state      string
	progress   api.MoveBundleProgress
	manifest   *bundle.Manifest
	skipCounts *api.MoveBundleImportSkipCounts
	errMsg     string
	errCode    string
	finishedAt time.Time
	cancel     context.CancelFunc
	done       chan struct{}
}

func (j *moveBundleJob) snapshot() api.MoveBundleJobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := api.MoveBundleJobStatus{
		ReincarnationID: j.key.ReincarnationID,
		Kind:            j.key.Agent.Kind,
		State:           j.state,
		Progress:        j.progress,
		Error:           j.errMsg,
		ErrorCode:       j.errCode,
	}
	if j.manifest != nil {
		mCopy := *j.manifest
		out.Manifest = &mCopy
	}
	if j.skipCounts != nil {
		sCopy := *j.skipCounts
		out.SkipCounts = &sCopy
	}
	return out
}

func (j *moveBundleJob) addProgress(bytesDelta int64, partsDelta int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.progress.BytesDone += bytesDelta
	j.progress.PartsDone += partsDelta
}

func (j *moveBundleJob) setTotals(bytesTotal int64, partsTotal int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.progress.BytesTotal = bytesTotal
	j.progress.PartsTotal = partsTotal
}

func (j *moveBundleJob) markSucceeded(manifest *bundle.Manifest, skipCounts *api.MoveBundleImportSkipCounts, now time.Time) {
	j.mu.Lock()
	j.state = api.MoveBundleJobStateSucceeded
	j.manifest = manifest
	j.skipCounts = skipCounts
	j.finishedAt = now
	j.cancel = nil
	done := j.done
	j.mu.Unlock()
	close(done)
}

func (j *moveBundleJob) markFailed(errMsg, errCode string, now time.Time) {
	j.mu.Lock()
	j.state = api.MoveBundleJobStateFailed
	j.errMsg = errMsg
	j.errCode = errCode
	j.finishedAt = now
	j.cancel = nil
	done := j.done
	j.mu.Unlock()
	close(done)
}

// moveBundleJobRegistry holds in-flight and recently-finished move-bundle
// export/import jobs in memory (design §3.5 "Broker job model").
type moveBundleJobRegistry struct {
	mu   sync.Mutex
	jobs map[moveBundleJobKey]*moveBundleJob
	now  func() time.Time
	ttl  time.Duration
}

func newMoveBundleJobRegistry(now func() time.Time, ttl time.Duration) *moveBundleJobRegistry {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = defaultMoveBundleJobTTL
	}
	return &moveBundleJobRegistry{
		jobs: make(map[moveBundleJobKey]*moveBundleJob),
		now:  now,
		ttl:  ttl,
	}
}

func (r *moveBundleJobRegistry) gcLocked() {
	cutoff := r.now().Add(-r.ttl)
	for k, j := range r.jobs {
		j.mu.Lock()
		expired := j.state != api.MoveBundleJobStateRunning && !j.finishedAt.IsZero() && j.finishedAt.Before(cutoff)
		j.mu.Unlock()
		if expired {
			delete(r.jobs, k)
		}
	}
}

// start looks up or registers a job for (agentKey, rid).
//   - If the same (agentKey, rid) already exists, returns (existing, false, false).
//   - If a different rid is currently running for agentKey, returns (nil, true, false).
//   - Otherwise registers a new running job and returns (job, false, true).
func (r *moveBundleJobRegistry) start(agentKey moveBundleAgentKey, rid string, cancel context.CancelFunc) (*moveBundleJob, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcLocked()

	key := moveBundleJobKey{Agent: agentKey, ReincarnationID: rid}
	if existing, ok := r.jobs[key]; ok {
		return existing, false, false
	}
	for k, j := range r.jobs {
		if k.Agent != agentKey {
			continue
		}
		j.mu.Lock()
		running := j.state == api.MoveBundleJobStateRunning
		j.mu.Unlock()
		if running {
			return nil, true, false
		}
	}
	job := &moveBundleJob{
		key:    key,
		state:  api.MoveBundleJobStateRunning,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	r.jobs[key] = job
	return job, false, true
}

func (r *moveBundleJobRegistry) get(agentKey moveBundleAgentKey, rid string) (*moveBundleJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcLocked()

	job, ok := r.jobs[moveBundleJobKey{Agent: agentKey, ReincarnationID: rid}]
	return job, ok
}

func (r *moveBundleJobRegistry) cancelAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		j.mu.Lock()
		cancel := j.cancel
		j.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

// cancelAndWaitForAgent cancels any running move-bundle job (export or import)
// for (projectID, agentSlug) and waits for each to finish before returning,
// respecting ctx cancellation. A delete with empty projectID cancels matching
// jobs regardless of projectID (and vice versa).
func (r *moveBundleJobRegistry) cancelAndWaitForAgent(ctx context.Context, projectID, agentSlug string) error {
	r.mu.Lock()
	var dones []chan struct{}
	for k, j := range r.jobs {
		if k.Agent.AgentSlug != agentSlug {
			continue
		}
		if projectID != "" && k.Agent.ProjectID != "" && k.Agent.ProjectID != projectID {
			continue
		}
		j.mu.Lock()
		if j.state == api.MoveBundleJobStateRunning {
			if j.cancel != nil {
				j.cancel()
			}
			dones = append(dones, j.done)
		}
		j.mu.Unlock()
	}
	r.mu.Unlock()

	for _, done := range dones {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// hasRunningForAgent reports whether any move-bundle job (export or import) is
// currently running for (projectID, agentSlug).
func (r *moveBundleJobRegistry) hasRunningForAgent(projectID, agentSlug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcLocked()
	for k, j := range r.jobs {
		if k.Agent.AgentSlug != agentSlug {
			continue
		}
		if projectID != "" && k.Agent.ProjectID != "" && k.Agent.ProjectID != projectID {
			continue
		}
		j.mu.Lock()
		running := j.state == api.MoveBundleJobStateRunning
		j.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}
