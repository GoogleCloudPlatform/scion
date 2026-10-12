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
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// linkedCheckoutErrPrefix starts every client-facing refusal from
// resolveLinkedCheckout.
const linkedCheckoutErrPrefix = "cannot create a local worktree for this agent: "

// linkedCheckoutInput holds the dispatch fields resolveLinkedCheckout
// decides on. Factored out for testability (no Server dependency).
type linkedCheckoutInput struct {
	WorkspaceMode string
	GitClone      *api.GitCloneConfig
	// ProjectPath is the project path the hub sent: the provider's local
	// path on this broker for a linked project.
	ProjectPath              string
	ProjectPathFromContainer bool
	HubGlobalProject         bool
	// Colocated reports that the dispatch came from the hub co-located in
	// this process (Server.dispatchFromColocatedHub).
	Colocated bool
	// RuntimeName is the runtime this dispatch resolved to.
	RuntimeName string
	// HasExistingNonLocalWorkspace reports that a start or restart is for
	// an agent whose workspace already exists in another form: a
	// broker-managed worktree of its own or one it joined, or a clone (see
	// existingNonLocalWorkspace). That agent keeps its current path.
	HasExistingNonLocalWorkspace bool
	// WorkstationMode reports that this broker runs in a workstation-mode
	// server (ServerConfig.WorkstationMode). Hosted servers are never
	// routed.
	WorkstationMode bool

	// eligibilityOverride, when non-nil, replaces runtime.WorktreeModeEligible
	// (git >= 2.48). Used in tests.
	eligibilityOverride func() (bool, string)
}

// linkedCheckoutDecision is the outcome of resolveLinkedCheckout.
type linkedCheckoutDecision struct {
	// Route is true when the agent's workspace is a worktree of the
	// project's own checkout, created by pkg/agent's local worktree engine
	// (<repo>/.scion/agents/<slug>/workspace), with no clone.
	Route bool
	// RepoRoot is the verified checkout's root when Route is true.
	RepoRoot string
	// Reason says why the dispatch is not routed (for the debug log), when
	// neither Route nor Refusal is set.
	Reason string
	// Refusal is the client-facing error when every routing precondition
	// holds but the checkout failed verification. The dispatch then fails
	// closed: it never falls back to a clone or to the broker's base clone,
	// either of which would silently hide the user's local work.
	Refusal string
}

// resolveLinkedCheckout decides whether a create for a worktree-per-agent
// project uses the project's own local checkout as the worktree source.
// That is the case for a linked project on the workstation: the hub labels
// git projects that a user links from their checkout to the local
// workstation hub worktree-per-agent, and the co-located broker then gives
// each agent a worktree of that checkout, as local mode did, instead of
// cloning the remote.
//
// The routing preconditions are: the mode is worktree-per-agent with a git
// clone; the project path is the provider's local path (not a hub-managed
// project directory, the global project, or a path recovered from a
// container); the dispatch came from the co-located hub; and the runtime is
// not Kubernetes. When any one does not hold, the dispatch keeps its
// existing path. When all hold, the checkout must be verified: git >= 2.48,
// the project path is in a git checkout, and that checkout's origin is the
// hub project's repository. A failed check is a Refusal.
//
// Pure apart from read-only git and filesystem queries.
func resolveLinkedCheckout(in linkedCheckoutInput) linkedCheckoutDecision {
	if in.WorkspaceMode != store.WorkspaceModeWorktreePerAgent {
		return linkedCheckoutDecision{Reason: "workspace mode is not worktree-per-agent"}
	}
	if in.GitClone == nil || in.GitClone.URL == "" {
		return linkedCheckoutDecision{Reason: "project is not git-backed"}
	}
	if in.ProjectPath == "" {
		return linkedCheckoutDecision{Reason: "no project path"}
	}
	if in.ProjectPathFromContainer {
		return linkedCheckoutDecision{Reason: "project path was recovered from the agent's container"}
	}
	if in.HubGlobalProject {
		return linkedCheckoutDecision{Reason: "global project"}
	}
	if isHubManagedProjectPath(in.ProjectPath) {
		return linkedCheckoutDecision{Reason: "hub-managed project directory, not a linked checkout"}
	}
	if !in.WorkstationMode {
		return linkedCheckoutDecision{Reason: "not a workstation-mode server"}
	}
	if !in.Colocated {
		return linkedCheckoutDecision{Reason: "dispatch is not from the co-located hub"}
	}
	if isKubernetesRuntimeName(in.RuntimeName) {
		return linkedCheckoutDecision{Reason: "kubernetes runtime"}
	}
	if in.HasExistingNonLocalWorkspace {
		return linkedCheckoutDecision{Reason: "agent already has a broker-managed worktree or a clone"}
	}

	eligCheck := runtime.WorktreeModeEligible
	if in.eligibilityOverride != nil {
		eligCheck = in.eligibilityOverride
	}
	if ok, reason := eligCheck(); !ok {
		return linkedCheckoutDecision{Refusal: linkedCheckoutErrPrefix + reason +
			"; upgrade git on this machine"}
	}

	// Resolve the project directory the way ProvisionAgent does, so the
	// checkout verified here is the one its worktree case uses.
	repoRoot := ""
	if projectDir, err := config.GetResolvedProjectDir(in.ProjectPath); err == nil && util.IsGitRepoDir(projectDir) {
		repoRoot, _ = util.RepoRootDir(projectDir)
	}
	if repoRoot == "" {
		return linkedCheckoutDecision{Refusal: linkedCheckoutErrPrefix +
			"the project's local path " + in.ProjectPath + " is not in a git checkout; " +
			"restore the checkout there, or run 'scion hub link' from the project's git checkout"}
	}

	// The remotes are compared normalised; they are shown with any
	// credentials removed (redactRemoteForDisplay).
	wantRaw := in.GitClone.URL
	originRaw := util.GetGitRemoteDir(repoRoot)
	if originRaw == "" {
		return linkedCheckoutDecision{Refusal: linkedCheckoutErrPrefix +
			"the project's checkout at " + repoRoot + " has no origin remote, but the hub project's repository is " +
			redactRemoteForDisplay(wantRaw) + "; add it with 'git remote add origin <url>'"}
	}
	if util.NormalizeGitRemote(originRaw) != util.NormalizeGitRemote(wantRaw) {
		return linkedCheckoutDecision{Refusal: linkedCheckoutErrPrefix +
			"the project's checkout at " + repoRoot + " has origin " + redactRemoteForDisplay(originRaw) +
			", but the hub project's repository is " + redactRemoteForDisplay(wantRaw) +
			"; point origin at the hub project's repository, or link this checkout as its own project"}
	}
	return linkedCheckoutDecision{Route: true, RepoRoot: repoRoot}
}

// existingNonLocalWorkspace reports whether the agent of a start or restart
// already has a workspace that is not a local worktree of the checkout, so
// it must keep its current path rather than be routed by
// resolveLinkedCheckout. It mirrors tryProvisionWorktree's own pre-existing
// worktree checks:
//   - the agent's own broker-managed worktree
//     (<project>/workspace/worktrees/<agent-id>) exists;
//   - the sharer registry of that base records an existing worktree for the
//     agent's branch (an agent that joined another agent's worktree), or
//     the registry refuses the branch's entry (tryProvisionWorktree then
//     fails closed on it);
//   - the agent's workspace (<project>/agents/<name>/workspace) is a clone,
//     i.e. its .git is a directory (an agent that fell back to an
//     in-container clone).
func existingNonLocalWorkspace(in startContextInputs) bool {
	if in.ProjectPath == "" || in.ProjectPathFromContainer {
		return false
	}
	if isSingleCleanPathElement(in.AgentID) && isSingleCleanPathElement(in.ProjectID) {
		mode := store.SharingModeWorktreePerAgent
		resolved, err := runtime.SelectWorkspaceBackend(nil, mode).Resolve(runtime.ResolveInput{
			ProjectDir: in.ProjectPath,
			ProjectID:  in.ProjectID,
			AgentID:    in.AgentID,
			Mode:       mode,
		})
		if err == nil && resolved.HostPath != "" {
			if _, statErr := os.Lstat(provision.WorktreePath(resolved.HostPath, in.AgentID)); statErr == nil {
				return true
			}
			// The branch is the sharer-registry key tryProvisionWorktree
			// uses (resolveWorktreeProvision's AgentName).
			branch := in.Name
			if in.Config != nil && in.Config.Branch != "" {
				branch = in.Config.Branch
			}
			if branch == "" {
				branch = in.AgentID
			}
			_, regPath, listErr := provision.ListSharersForJoin(resolved.HostPath, branch)
			if listErr != nil {
				return true
			}
			if regPath != "" {
				if _, statErr := os.Lstat(regPath); statErr == nil {
					return true
				}
			}
		}
	}
	if projectDir, err := config.GetResolvedProjectDir(in.ProjectPath); err == nil {
		if agentDir, err := config.AgentDirForProject(projectDir, in.Name, false, ""); err == nil {
			if info, statErr := os.Lstat(filepath.Join(agentDir, "workspace", ".git")); statErr == nil && info.IsDir() {
				return true
			}
		}
	}
	return false
}

// redactRemoteForDisplay returns a git remote (URL or scp-like form) for a
// client-facing message with any credentials removed: everything between
// the scheme and the last '@' is dropped, and so are a query string and a
// fragment. It errs on the side of dropping too much, so it does not rely
// on the remote being well-formed.
func redactRemoteForDisplay(raw string) string {
	s := strings.TrimSpace(raw)
	scheme := ""
	if i := strings.Index(s, "://"); i >= 0 {
		scheme, s = s[:i+3], s[i+3:]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return scheme + s
}

// isHubManagedProjectPath reports whether projectPath is a hub-managed
// project directory on this broker (~/.scion/projects/<slug> or below),
// rather than a linked project's own path.
func isHubManagedProjectPath(projectPath string) bool {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return false
	}
	projectsDir := filepath.Join(globalDir, "projects")
	clean := filepath.Clean(projectPath)
	return clean == projectsDir || strings.HasPrefix(clean, projectsDir+string(filepath.Separator))
}
