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
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The agent directory of a deleted clone-per-agent (or empty-per-agent)
// agent always holds provisioning-lock litter: the broker's removal of the
// agent's workspace is the last user of the directory's lock, and every
// lock release leaves an ".evict-*" entry that must outlive the lock's GC
// cutoff (provision.LockLitterMinAge) before anyone may remove it
// (ptone/scion#7100). The hub clears that litter before it removes the
// empty agent directory, and retries once when it is still too young.
var (
	// agentLockLitterSweep is provision.SweepLockLitter, whose cutoff is
	// the lock's GC cutoff and cannot be lowered; tests replace it.
	agentLockLitterSweep = provision.SweepLockLitter
	// agentLockLitterRetryMargin is added to the remaining wait before the
	// retry, so the litter is past the cutoff when the sweep runs again.
	agentLockLitterRetryMargin = 30 * time.Second
)

// Reasons of an agent directory kept because of provisioning-lock litter.
const (
	agentLockLitterPendingReason = "provisioning lock litter in the agent directory is not old enough to remove yet"
	agentLockLitterUnagedReason  = "the agent directory holds an evicted provisioning lock without its eviction time, which is never removed automatically"
)

// sharedStorageLayouts returns the distinct shared Layouts an agent's
// directories can be on: layout and its shared-dir Layout when that is
// another share (projectfs.Layout.RemoveAgentStorage removes from the
// same set).
func sharedStorageLayouts(layout projectfs.Layout) []projectfs.Layout {
	var out []projectfs.Layout
	seen := map[string]bool{}
	for _, l := range []projectfs.Layout{layout, layout.SharedDirLayout()} {
		l.SharedDirs = nil
		if !l.Backend.Shared() || seen[l.Key()] {
			continue
		}
		seen[l.Key()] = true
		out = append(out, l)
	}
	return out
}

// sweepAgentLockLitter clears the provisioning-lock litter old enough to
// remove from the agent's directory on every shared Layout, and reports
// what is left: the longest remaining wait and the unaged entries. Each
// agent directory's real path must have no symlinked component
// (projectfs.Resolved.VerifyNoSymlinks). A sweep that cannot run is a
// failed agent-root Deletion; the litter is then kept.
func sweepAgentLockLitter(layout projectfs.Layout, a store.Agent) (left provision.LockLitter, failed []projectfs.Deletion) {
	for _, l := range sharedStorageLayouts(layout) {
		root, err := l.AgentRoot(a.ProjectID, a.Slug)
		if err != nil {
			failed = append(failed, projectfs.Deletion{Tree: projectfs.TreeAgentRoot, Outcome: projectfs.OutcomeFailed, Err: err})
			continue
		}
		dir, err := root.VerifyNoSymlinks()
		if err == nil {
			var got provision.LockLitter
			got, err = agentLockLitterSweep(dir)
			left.Pending = max(left.Pending, got.Pending)
			left.Unaged += got.Unaged
		}
		if err != nil {
			failed = append(failed, projectfs.Deletion{Tree: projectfs.TreeAgentRoot, Path: root.HostPath, Outcome: projectfs.OutcomeFailed,
				Err: fmt.Errorf("clear provisioning lock litter: %w", err)})
		}
	}
	return left, failed
}

// removeAgentSharedStorage removes the agent's storage on the hub's shared
// Layouts: lock litter old enough to remove, then its home and secrets and
// its agent directory when empty (projectfs.Layout.RemoveAgentStorage).
// audit is called with each Deletion once it is final: home, secrets and
// any sweep failure at once, so they are recorded even if the hub stops
// during a retry wait; the agent directory after the retry, if any.
//
// When an agent directory is kept and aged litter is still too young, it
// waits until the litter is past the cutoff (bounded by ctx) and retries
// the sweep and the agent-directory step once. Unaged litter is never
// waited for. An agent directory still kept because of litter gets the
// matching reason; one kept for other entries after a confirmed broker
// teardown gets agentRootOtherFilesReason.
func removeAgentSharedStorage(ctx context.Context, layout projectfs.Layout, a store.Agent, teardownConfirmed bool, audit func([]projectfs.Deletion)) {
	left, failed := sweepAgentLockLitter(layout, a)
	others, roots := splitAgentRootDeletions(removeAgentStorageOnce(layout, a))
	audit(append(others, failed...))

	if left.Pending > 0 && agentRootKept(roots) {
		wait := left.Pending + agentLockLitterRetryMargin
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > wait {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
				left, failed = sweepAgentLockLitter(layout, a)
				audit(failed)
				// Home and secrets were handled in the first pass; only the
				// agent directory's outcome is taken from the retry.
				_, roots = splitAgentRootDeletions(removeAgentStorageOnce(layout, a))
			}
		}
	}
	for i := range roots {
		if roots[i].Outcome != projectfs.OutcomeKept {
			continue
		}
		switch {
		case left.Unaged > 0:
			roots[i].Reason = agentLockLitterUnagedReason
		case left.Pending > 0:
			roots[i].Reason = agentLockLitterPendingReason
		case teardownConfirmed && roots[i].Reason == projectfs.AgentWorkspacePendingReason:
			roots[i].Reason = agentRootOtherFilesReason
		}
	}
	audit(roots)
}

// removeAgentStorageOnce is one projectfs.Layout.RemoveAgentStorage, with a
// refusal reported as a failed agent-root Deletion.
func removeAgentStorageOnce(layout projectfs.Layout, a store.Agent) []projectfs.Deletion {
	ds, err := layout.RemoveAgentStorage(a.ProjectID, a.Slug, a.ID)
	if err != nil {
		ds = append(ds, projectfs.Deletion{Tree: projectfs.TreeAgentRoot, Outcome: projectfs.OutcomeFailed, Err: err})
	}
	return ds
}

func agentRootKept(roots []projectfs.Deletion) bool {
	for _, d := range roots {
		if d.Outcome == projectfs.OutcomeKept {
			return true
		}
	}
	return false
}

// splitAgentRootDeletions splits ds into the agent-root Deletions and the
// others.
func splitAgentRootDeletions(ds []projectfs.Deletion) (others, roots []projectfs.Deletion) {
	for _, d := range ds {
		if d.Tree == projectfs.TreeAgentRoot {
			roots = append(roots, d)
		} else {
			others = append(others, d)
		}
	}
	return others, roots
}
