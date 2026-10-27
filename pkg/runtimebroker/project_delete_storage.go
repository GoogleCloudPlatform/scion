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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// projectClaimReleaseTimeout bounds the shared-dir claim release of one
// project delete.
var projectClaimReleaseTimeout = 30 * time.Second

// sharedDirClaimReleaser is the part of a Kubernetes runtime the claim
// release needs (scionrt.KubernetesRuntime).
type sharedDirClaimReleaser interface {
	ReleaseProjectSharedDirClaims(ctx context.Context, projectID, projectName string, keep []string) (scionrt.SharedDirClaimRelease, error)
}

// claimReleasersHook, when set, replaces the broker's Kubernetes runtimes as
// the claim releasers. Tests only.
var claimReleasersHook func(s *Server) []sharedDirClaimReleaser

// claimReleasers returns the broker's Kubernetes runtimes (the default and
// the auxiliary ones), one per namespace and cluster context.
func (s *Server) claimReleasers() []sharedDirClaimReleaser {
	if claimReleasersHook != nil {
		return claimReleasersHook(s)
	}
	var out []sharedDirClaimReleaser
	seen := map[string]bool{}
	add := func(rt scionrt.Runtime) {
		k, ok := rt.(*scionrt.KubernetesRuntime)
		if !ok || k == nil {
			return
		}
		key := auxiliaryRuntimeIdentity(k)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, k)
	}
	if rt := s.currentRuntime(); rt != nil {
		add(rt)
	}
	for _, aux := range s.sortedAuxiliaryRuntimes() {
		add(aux.Runtime)
	}
	return out
}

// releaseProjectSharedDirClaims deletes, at project delete, the per-dir
// shared-dir claims of projectID in the default namespaces of this
// broker's Kubernetes runtimes (ptone/scion#5835). The hub asks for the
// cleanup only after its deletion service has committed the delete, which
// refuses while agents remain unless forced; whatever the delete, a claim
// an existing pod still references is kept and audited as in use. Only
// claims carrying the project-id label, a shared-dir label and the
// scion-shared-<...>-<dir> name are deleted
// (scionrt.KubernetesRuntime.ReleaseProjectSharedDirClaims); the layout's
// shared claims are excluded by name. One audit line per deleted claim;
// claims of the project's name without the project-id label are kept and
// named in an audit line each, for an operator. Best-effort: a failure is
// logged and audited and never fails the project delete.
func (s *Server) releaseProjectSharedDirClaims(reqCtx context.Context, slug, projectID string) {
	if projectID == "" {
		return
	}
	releasers := s.claimReleasers()
	if len(releasers) == 0 {
		return
	}
	layout := s.config.StorageLayout
	keep := []string{layout.Share.PVName, layout.SharedDirLayout().Share.PVName}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), projectClaimReleaseTimeout)
	defer cancel()
	log := s.agentLifecycleLog
	for _, r := range releasers {
		res, err := r.ReleaseProjectSharedDirClaims(ctx, projectID, slug, keep)
		if err != nil {
			projectfs.Audit(ctx, log, projectfs.DeleterBroker, projectID, "", projectfs.Deletion{
				Tree: projectfs.TreeClaim, Path: res.Namespace + "/*", Outcome: projectfs.OutcomeFailed, Err: err,
			})
			continue
		}
		for _, name := range res.Deleted {
			projectfs.Audit(ctx, log, projectfs.DeleterBroker, projectID, "", projectfs.Deletion{
				Tree: projectfs.TreeClaim, Path: res.Namespace + "/" + name, Outcome: projectfs.OutcomeDeleted,
			})
		}
		for name, ferr := range res.Failed {
			projectfs.Audit(ctx, log, projectfs.DeleterBroker, projectID, "", projectfs.Deletion{
				Tree: projectfs.TreeClaim, Path: res.Namespace + "/" + name, Outcome: projectfs.OutcomeFailed, Err: ferr,
			})
		}
		for _, name := range res.InUse {
			projectfs.Audit(ctx, log, projectfs.DeleterBroker, projectID, "", projectfs.Deletion{
				Tree: projectfs.TreeClaim, Path: res.Namespace + "/" + name, Outcome: projectfs.OutcomeKept,
				Reason: "in use: an existing pod references the claim",
			})
		}
		for _, name := range res.Unlabelled {
			projectfs.Audit(ctx, log, projectfs.DeleterBroker, projectID, "", projectfs.Deletion{
				Tree: projectfs.TreeClaim, Path: res.Namespace + "/" + name, Outcome: projectfs.OutcomeKept,
				Reason: "the claim has the project's name but no project-id label; check it and delete it by hand if it is the deleted project's",
			})
		}
	}
}

// auditBrokerDeletion writes the audit line of a broker-local tree this
// broker removed at project delete (its project directory or project-configs
// entry).
func (s *Server) auditBrokerDeletion(ctx context.Context, projectID string, tree projectfs.TreeKind, path string) {
	projectfs.Audit(ctx, s.agentLifecycleLog, projectfs.DeleterBroker, projectID, "", projectfs.Deletion{
		Tree: tree, Path: path, Outcome: projectfs.OutcomeDeleted,
	})
}

// agentsKeptReason is the audit reason of a project directory a project
// delete leaves in place because this broker still has agents in it.
const agentsKeptReason = "agents still exist on this broker"

// projectHasAgentsHere reports whether this broker lists any agent of
// projectID, in any phase (stopped, error and halted included): an entry
// labelled with the project, or a legacy entry whose recorded project path
// identifies as it (legacyEntryInProject). A project delete must not remove
// the project directory of such an agent: on a forced delete its legacy
// agent directory (~/.scion/projects/<slug>/.scion/agents/<name>, with a
// clone-per-agent workspace that may hold unpushed work, also when the agent
// is stopped) is still the agent's. A broker without a manager, or a
// listing that fails, counts as having agents.
func (s *Server) projectHasAgentsHere(ctx context.Context, projectID string) (bool, error) {
	mgr := s.currentManager()
	if mgr == nil {
		return true, nil
	}
	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		return true, err
	}
	for _, a := range agents {
		if agentInProjectStrict(a, projectID) || legacyEntryInProject(a, projectID) {
			return true, nil
		}
	}
	return false, nil
}
