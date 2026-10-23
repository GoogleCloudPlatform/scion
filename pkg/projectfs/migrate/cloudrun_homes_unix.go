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

// legacyCloudRunAgentDir is the retired Cloud Run per-agent directory name
// below a project root: agents/<agent ID>. This migration step is the only
// code that builds it, to read the legacy source.
func legacyCloudRunAgentDir(projectRel, agentID string) string {
	return filepath.Join(projectRel, "agents", agentID)
}

// cloudRunHomes moves the Cloud Run homes and secrets of a project's agents
// on the workspace Layout l (step 5).
func cloudRunHomes(l projectfs.Layout, projectID string, agents []AgentRef, opts Options) []Result {
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
		home, err := l.AgentHome(projectID, a.Slug, a.ID)
		if err != nil {
			out = append(out, Result{Step: StepCloudRunHomes, Outcome: OutcomeFailed, Err: fmt.Errorf("agent %s: %w", a.ID, err)})
			continue
		}
		secrets, err := l.AgentSecrets(projectID, a.Slug, a.ID)
		if err != nil {
			out = append(out, Result{Step: StepCloudRunHomes, Outcome: OutcomeFailed, Err: fmt.Errorf("agent %s: %w", a.ID, err)})
			continue
		}
		legacy := legacyCloudRunAgentDir(root.RelPath, a.ID)
		for _, mv := range []struct {
			name   string
			target projectfs.Resolved
		}{{"home", home}, {"secrets", secrets}} {
			spec := Spec{
				Step:      StepCloudRunHomes,
				Source:    Tree{Base: l.HostBase, Rel: filepath.Join(legacy, mv.name)},
				Target:    Tree{Base: l.HostBase, Rel: mv.target.RelPath},
				Normalize: NormalizeNone,
			}
			if !a.Stopped {
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
