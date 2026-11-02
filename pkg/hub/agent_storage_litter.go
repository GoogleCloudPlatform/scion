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
	// agentLockLitterSweep and agentLockLitterMinAge are
	// provision.SweepLockLitter and provision.LockLitterMinAge; tests
	// replace them.
	agentLockLitterSweep  = provision.SweepLockLitter
	agentLockLitterMinAge = provision.LockLitterMinAge
	// agentLockLitterRetryMargin is added to the remaining wait before the
	// retry, so the litter is past the cutoff when the sweep runs again.
	agentLockLitterRetryMargin = 30 * time.Second
)

// agentLockLitterPendingReason is the reason of an agent directory kept
// because its provisioning-lock litter is still younger than the cutoff.
const agentLockLitterPendingReason = "provisioning lock litter in the agent directory is not old enough to remove yet"

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
// how long the youngest remaining entry still needs (0 when none is left).
// Each agent directory's real path must have no symlinked component
// (projectfs.Resolved.VerifyNoSymlinks). A sweep that cannot run is a
// failed agent-root Deletion; the litter is then kept.
func sweepAgentLockLitter(layout projectfs.Layout, a store.Agent) (pending time.Duration, failed []projectfs.Deletion) {
	for _, l := range sharedStorageLayouts(layout) {
		root, err := l.AgentRoot(a.ProjectID, a.Slug)
		if err != nil {
			failed = append(failed, projectfs.Deletion{Tree: projectfs.TreeAgentRoot, Outcome: projectfs.OutcomeFailed, Err: err})
			continue
		}
		dir, err := root.VerifyNoSymlinks()
		if err == nil {
			var left time.Duration
			left, err = agentLockLitterSweep(dir, agentLockLitterMinAge())
			pending = max(pending, left)
		}
		if err != nil {
			failed = append(failed, projectfs.Deletion{Tree: projectfs.TreeAgentRoot, Path: root.HostPath, Outcome: projectfs.OutcomeFailed,
				Err: fmt.Errorf("clear provisioning lock litter: %w", err)})
		}
	}
	return pending, failed
}

// removeAgentSharedStorage removes the agent's storage on the hub's shared
// Layouts: lock litter old enough to remove, then its home and secrets and
// its agent directory when empty (projectfs.Layout.RemoveAgentStorage).
// When an agent directory is kept and litter is still too young, it waits
// until the litter is past the cutoff (bounded by ctx) and retries the
// sweep and the agent-directory step once; an agent directory still kept
// for that reason gets agentLockLitterPendingReason.
func removeAgentSharedStorage(ctx context.Context, layout projectfs.Layout, a store.Agent) []projectfs.Deletion {
	pending, failed := sweepAgentLockLitter(layout, a)
	ds := removeAgentStorageOnce(layout, a)

	if pending > 0 && agentRootKept(ds) {
		wait := pending + agentLockLitterRetryMargin
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > wait {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
				pending, failed = sweepAgentLockLitter(layout, a)
				// Home and secrets were handled in the first pass; only the
				// agent directory's outcome is taken from the retry.
				ds = replaceAgentRootDeletions(ds, removeAgentStorageOnce(layout, a))
			}
		}
	}
	if pending > 0 {
		for i := range ds {
			if ds[i].Tree == projectfs.TreeAgentRoot && ds[i].Outcome == projectfs.OutcomeKept {
				ds[i].Reason = agentLockLitterPendingReason
			}
		}
	}
	return append(ds, failed...)
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

func agentRootKept(ds []projectfs.Deletion) bool {
	for _, d := range ds {
		if d.Tree == projectfs.TreeAgentRoot && d.Outcome == projectfs.OutcomeKept {
			return true
		}
	}
	return false
}

// replaceAgentRootDeletions returns first with its agent-root Deletions
// replaced by retry's.
func replaceAgentRootDeletions(first, retry []projectfs.Deletion) []projectfs.Deletion {
	out := make([]projectfs.Deletion, 0, len(first))
	for _, d := range first {
		if d.Tree != projectfs.TreeAgentRoot {
			out = append(out, d)
		}
	}
	for _, d := range retry {
		if d.Tree == projectfs.TreeAgentRoot {
			out = append(out, d)
		}
	}
	return out
}
