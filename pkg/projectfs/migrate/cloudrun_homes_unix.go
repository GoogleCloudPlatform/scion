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

//go:build unix

package migrate

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// Step 5 (ptone/scion#5825): the Cloud Run agent runtime kept an agent's
// home and secrets at <subpath root>/<project>/agents/<agent ID>/{home,
// secrets} on the workspace share. The layout authority places them where
// the Kubernetes NFS home does, agents/<slug>/{home-,secrets-}<agent ID>
// (projectfs.Layout.AgentHome/AgentSecrets). Each is moved by the mover's
// rule (rename into an absent target, otherwise copy what the target
// lacks, never overwrite, symlinks refused), only for a stopped agent: a
// running agent's directories are reported deferred and move on a later
// run. The emptied legacy agents/<agent ID> directory is removed with the
// project's legacy trees (ptone/scion#5835).

// cloudRunHomesPresent reports whether any of agents has a legacy Cloud
// Run home or secrets directory on l (an unreadable one counts), so step 5
// has something to move.
func cloudRunHomesPresent(l projectfs.Layout, projectID string, agents []AgentRef) bool {
	l.SharedDirs = nil
	root, err := l.ProjectRoot(projectID)
	if err != nil {
		return false
	}
	for _, a := range agents {
		if !projectfs.ValidAgentID(a.ID) {
			continue
		}
		legacy := projectfs.LegacyCloudRunAgentDir(root.RelPath, a.ID)
		for _, name := range []string{"home", "secrets"} {
			if k, err := entryKind(Tree{Base: l.HostBase, Rel: filepath.Join(legacy, name)}); err != nil || k != kindAbsent {
				return true
			}
		}
	}
	return false
}

// cloudRunHomes moves the Cloud Run homes and secrets of a project's agents
// on the workspace Layout l (step 5). recheck, when set, re-reads an agent's
// stopped state right before its moves (Run passes HubInput.AgentStopped);
// the caller runs the step under the project's provisioning locks.
func cloudRunHomes(l projectfs.Layout, projectID string, agents []AgentRef, recheck func(ctx context.Context, projectID, agentID string) (bool, error), opts Options) []Result {
	// The Cloud Run runtime keeps homes on the workspace share, never on a
	// transitional shared-dir share.
	l.SharedDirs = nil
	root, err := l.ProjectRoot(projectID)
	if err != nil {
		return []Result{{Step: StepCloudRunHomes, Outcome: OutcomeFailed, Err: fmt.Errorf("resolve project root: %w", err)}}
	}
	var out []Result
	for _, a := range agents {
		if !projectfs.ValidAgentID(a.ID) {
			continue // no Cloud Run home is keyed by anything but a hub agent ID
		}
		legacy := projectfs.LegacyCloudRunAgentDir(root.RelPath, a.ID)
		sources := []Tree{{Base: l.HostBase, Rel: filepath.Join(legacy, "home")}, {Base: l.HostBase, Rel: filepath.Join(legacy, "secrets")}}
		present := false
		for _, src := range sources {
			k, err := entryKind(src)
			if err != nil {
				out = append(out, Result{Step: StepCloudRunHomes, Source: src.String(), Outcome: OutcomeFailed, Err: fmt.Errorf("stat source: %w", err)})
				present = false
				break
			}
			present = present || k != kindAbsent
		}
		if !present {
			continue // nothing of this agent to move (not a Cloud Run agent)
		}
		home, err := l.AgentHome(projectID, a.Slug, a.ID)
		if err != nil {
			out = append(out, Result{Step: StepCloudRunHomes, Source: sources[0].String(), Outcome: OutcomeFailed, Err: fmt.Errorf("agent %s: %w", a.ID, err)})
			continue
		}
		secrets, err := l.AgentSecrets(projectID, a.Slug, a.ID)
		if err != nil {
			out = append(out, Result{Step: StepCloudRunHomes, Source: sources[1].String(), Outcome: OutcomeFailed, Err: fmt.Errorf("agent %s: %w", a.ID, err)})
			continue
		}
		stopped := a.Stopped
		if stopped && recheck != nil && !opts.DryRun {
			ctx := opts.Context
			if ctx == nil {
				ctx = context.Background()
			}
			ok, err := recheck(ctx, projectID, a.ID)
			if err != nil {
				out = append(out, Result{Step: StepCloudRunHomes, Source: sources[0].String(), Outcome: OutcomeFailed, Err: fmt.Errorf("agent %s: re-read state: %w", a.ID, err)})
				continue
			}
			stopped = ok
		}
		for i, target := range []projectfs.Resolved{home, secrets} {
			spec := Spec{
				Step:      StepCloudRunHomes,
				Source:    sources[i],
				Target:    Tree{Base: l.HostBase, Rel: target.RelPath},
				Normalize: NormalizeNone,
			}
			if !stopped {
				if r := deferIfPresent(spec); r != nil {
					out = append(out, *r)
				}
				continue
			}
			out = append(out, Move(spec, opts))
		}
	}
	return out
}

// deferIfPresent reports a Spec whose source holds anything as deferred
// (the agent is running), or nil when there is nothing to move.
func deferIfPresent(spec Spec) *Result {
	res := Result{Step: spec.Step, Source: spec.Source.String(), Target: spec.Target.String()}
	if err := spec.Source.validate(); err != nil {
		res.Outcome, res.Err = OutcomeFailed, fmt.Errorf("source: %w", err)
		return &res
	}
	k, err := entryKind(spec.Source)
	if err != nil {
		res.Outcome, res.Err = OutcomeFailed, fmt.Errorf("stat source: %w", err)
		return &res
	}
	if k == kindAbsent {
		return nil
	}
	res.Outcome = OutcomeDeferred
	return &res
}
