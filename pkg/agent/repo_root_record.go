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

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Sources of a repository root record (provisionedWorktreeStateFile). A record
// is written only when agentDir is outside every mount of the agent
// (agentDirOutsideAgentMounts), so its source also records that it was written
// under that condition. Source is recorded on write and used by
// writeRepoRootRecord's idempotence check, but the start read path no longer
// gates on it: a record is reconciled with the derived root regardless of
// source, so records predating Source stay usable.
const (
	// repoRootSourceBroker: the broker provisioned the agent's worktree
	// and passed its base on ctx (api.ContextWithProvisionedWorktreeRepoRoot).
	repoRootSourceBroker = "broker"
	// repoRootSourceProvision: ProvisionAgent recorded the project's
	// repository root (trustedProjectRepoRoot) for a git project.
	repoRootSourceProvision = "provision"
	// repoRootSourceBackfill: a legacy source. Start used to record
	// trustedProjectRepoRoot for an agent provisioned before records were
	// written; that start-time backfill is no longer done, but the value is
	// still accepted on read for records written by older versions.
	repoRootSourceBackfill = "backfill"
)

// errAgentDirMounted is returned when a record is not written because
// agentDir is inside one of the agent's mounts.
var errAgentDirMounted = errors.New("agent directory is inside one of the agent's mounts")

// trustedProjectRepoRoot returns the main repository that holds projectDir,
// found without running git: the nearest ancestor of projectDir (projectDir
// included, symlinks resolved) that has a .git directory, not a .git file,
// with no commondir file in it. It reads no repository configuration and no
// environment, so neither core.worktree, GIT_DIR nor anything inside an
// agent's workspace (which sits below projectDir) can change the answer. It
// returns "" when projectDir cannot be resolved or no such ancestor exists,
// and stops (returning "") at the first .git that is not a directory.
func trustedProjectRepoRoot(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	dir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return ""
	}
	for {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Lstat(gitPath); err == nil {
			if !fi.IsDir() {
				return ""
			}
			if _, err := os.Lstat(filepath.Join(gitPath, "commondir")); err == nil {
				return ""
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// agentMountSources returns the host paths the agent's container mounts
// from: the workspace, the repository root mount (repoRoot/.git when the
// workspace is below repoRoot, otherwise the whole repoRoot, as
// pkg/runtime/common.go does), and configured local volumes.
func agentMountSources(workspace, repoRoot string, volumes []api.VolumeMount) []string {
	var sources []string
	if workspace != "" {
		sources = append(sources, workspace)
	}
	if repoRoot != "" {
		if workspace != "" && workspace != repoRoot && pathUnder(workspace, repoRoot) {
			sources = append(sources, filepath.Join(repoRoot, ".git"))
		} else {
			sources = append(sources, repoRoot)
		}
	}
	for _, v := range volumes {
		if v.Type == "gcs" || v.Source == "" {
			continue
		}
		src := v.Source
		if home, err := os.UserHomeDir(); err == nil {
			if src == "~" {
				src = home
			} else if strings.HasPrefix(src, "~/") {
				src = filepath.Join(home, src[2:])
			}
		}
		sources = append(sources, src)
	}
	return sources
}

// agentDirOutsideAgentMounts reports whether agentDir lies outside every
// path in sources: it is neither one of them nor below one. Paths are
// compared with symlinks resolved where they exist. An agentDir that cannot
// be resolved is reported as not outside.
//
// Only the agent's own mounts are known here; see the item notes for what
// this does not cover.
func agentDirOutsideAgentMounts(agentDir string, sources []string) bool {
	dir, err := filepath.EvalSymlinks(agentDir)
	if err != nil {
		return false
	}
	for _, src := range sources {
		if src == "" {
			continue
		}
		s := src
		if resolved, err := filepath.EvalSymlinks(src); err == nil {
			s = resolved
		} else if !filepath.IsAbs(s) {
			return false
		}
		if pathUnder(dir, filepath.Clean(s)) {
			return false
		}
	}
	return true
}

// samePath reports whether a and b name the same path, compared cleaned
// and, when both resolve, with symlinks resolved.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// pathUnder reports whether p is base or below it (both clean and absolute).
func pathUnder(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// readRepoRootRecord reads agentDir's repository root record. It reports
// false when there is no record, it does not parse, or its source is not
// one this version writes.
func readRepoRootRecord(agentDir string) (provisionedWorktreeState, bool) {
	data, err := os.ReadFile(filepath.Join(agentDir, provisionedWorktreeStateFile))
	if err != nil {
		return provisionedWorktreeState{}, false
	}
	var state provisionedWorktreeState
	if err := json.Unmarshal(data, &state); err != nil || state.RepoRoot == "" {
		return provisionedWorktreeState{}, false
	}
	switch state.Source {
	case repoRootSourceBroker, repoRootSourceProvision, repoRootSourceBackfill:
		return state, true
	default:
		return provisionedWorktreeState{}, false
	}
}

// writeRepoRootRecord writes root with source to agentDir's record, but only
// when agentDir is outside every path in mountSources; otherwise it writes
// nothing and returns errAgentDirMounted.
func writeRepoRootRecord(agentDir, root, source string, mountSources []string) error {
	if !agentDirOutsideAgentMounts(agentDir, mountSources) {
		return errAgentDirMounted
	}
	if cur, ok := readRepoRootRecord(agentDir); ok && cur.RepoRoot == root && cur.Source == source {
		return nil
	}
	data, err := json.Marshal(provisionedWorktreeState{RepoRoot: root, Source: source})
	if err != nil {
		return err
	}
	return writeAgentInfoFile(filepath.Join(agentDir, provisionedWorktreeStateFile), data, 0o644)
}

// checkProvisionRepoRoot returns trustedProjectRepoRoot(projectDir) after
// checking it against the repository root git reports for projectDir
// (util.RepoRootDir, with symlinks resolved). A difference, or no trusted
// root, is an error: provisioning does not proceed on a repository it cannot
// identify without git.
func checkProvisionRepoRoot(projectDir string) (string, error) {
	trusted := trustedProjectRepoRoot(projectDir)
	gitRoot, err := util.RepoRootDir(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolve the project's repository root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(gitRoot); err == nil {
		gitRoot = resolved
	}
	if trusted == "" || trusted != gitRoot {
		return "", fmt.Errorf("the project's repository root could not be confirmed without reading container-writable git metadata, so agents cannot be provisioned for it in this interim release (ptone/scion#4611): git reports %q but the directory walk from %s finds %q (the project's top-level .git must be a directory without a commondir file; a linked worktree or a submodule working directory is not supported here yet). Provision from the repository's main checkout instead. A project that merely contains submodules is unaffected", gitRoot, projectDir, trusted)
	}
	return trusted, nil
}

// startRepoRootInput is what resolveStartRepoRoot needs from Start.
type startRepoRootInput struct {
	agentName   string
	agentDir    string
	projectDir  string
	workspace   string // effectiveWorkspace, as Start has it
	ctxRepoRoot string // the broker's signal for this dispatch, if any
	explicit    bool   // an explicit-workspace start
	volumes     []api.VolumeMount
	settings    *config.VersionedSettings
}

// recordStatus is the tri-state result of reading the agent's on-disk
// repository root record for the start-time intersection.
type recordStatus int

const (
	// recordMissing: no record on disk, or one that is not trusted because
	// agentDir is under an agent mount. Treated as absent.
	recordMissing recordStatus = iota
	// recordPresent: a trusted, readable on-disk record.
	recordPresent
	// recordUnreadable: a record present on disk, outside every agent mount,
	// that cannot be read or is malformed. The start is refused.
	recordUnreadable
)

// repoRootRecordState is the on-disk record (R) that the intersection
// reconciles with the derived root (D).
type repoRootRecordState struct {
	status recordStatus
	root   string // set only when status is recordPresent
}

// repoRootRecordMax caps how much of the repository root record is read. The
// record is a small JSON object; anything larger is treated as unreadable.
const repoRootRecordMax = 4096

// resolveStartRepoRoot returns the repository root Start passes to the runtime,
// together with the on-disk record and the candidate it used, so Start can
// re-run the same composition after a workspace backend changes the workspace.
// The result is always the derived root (or ""), or a refusal:
//
//   - the candidate is the broker signal (ctx) or, when there is none, the
//     recorded root; it is the input to the derived root, as the upstream
//     broker-known-path derivation computes it on main, except that an
//     untrusted record (one under an agent mount) is not a candidate.
//   - R is the on-disk record only (never ctx), read tri-state: a record under
//     an agent mount, or none, is missing; a present record that cannot be read
//     or is malformed is unreadable.
//   - reconcileRepoRoot applies the decision table: the full root is mounted
//     only when a present record equals the derived root; a mismatch or an
//     unreadable record refuses the start; a missing or untrusted record uses
//     the derived root unchanged.
func resolveStartRepoRoot(in startRepoRootInput) (repoRoot string, rec repoRootRecordState, candidate string, err error) {
	rec = readRepoRootRecordState(in.agentName, in.agentDir, in.workspace, in.volumes)
	candidate = in.ctxRepoRoot
	if candidate == "" && rec.status == recordPresent {
		candidate = rec.root
	}
	repoRoot, err = composeRepoRoot(in.agentName, candidate, rec, in.workspace, in.projectDir, in.explicit, in.settings)
	return repoRoot, rec, candidate, err
}

// composeRepoRoot computes the derived root the way the upstream broker-known-
// path derivation does on main — validatedWorktreeRepoRoot(candidate, ws),
// falling back to detectRepoRoot when that is empty — and then applies the
// fail-closed record intersection (reconcileRepoRoot). The root is taken only
// from broker-known paths (the candidate, the workspace path and the project's
// own root); git is used only to check those paths, never to source the root
// from the workspace's own metadata.
func composeRepoRoot(agentName, candidate string, rec repoRootRecordState, workspace, projectDir string, explicit bool, settings *config.VersionedSettings) (string, error) {
	derived := validatedWorktreeRepoRoot(candidate, workspace)
	var detectErr error
	if derived == "" {
		derived, detectErr = detectRepoRoot(explicit, workspace, projectDir, settings)
	}
	return reconcileRepoRoot(agentName, derived, detectErr, rec)
}

// readRepoRootRecordState reads the on-disk record R, guarding first: when
// agentDir is under any agent mount the record is one an agent could reach or
// write, so it is treated as missing whatever its contents (never a refusal).
// A record outside every agent mount is then read: absent is missing; present
// but unreadable or malformed is unreadable; otherwise present. The broker ctx
// signal is not a record; the caller uses it as the candidate.
func readRepoRootRecordState(agentName, agentDir, workspace string, volumes []api.VolumeMount) repoRootRecordState {
	recPath := filepath.Join(agentDir, provisionedWorktreeStateFile)
	reachable := !agentDirOutsideAgentMounts(agentDir, agentMountSources(workspace, "", volumes))
	// Decide existence with Lstat before opening anything: an absent record is
	// missing with no log; a reachable record is missing whatever it is.
	if _, err := os.Lstat(recPath); err != nil {
		if os.IsNotExist(err) {
			return repoRootRecordState{status: recordMissing}
		}
		if reachable {
			slog.Warn("start: the agent directory is inside one of the agent's mounts; its repository root record is not used", "agent", agentName)
			return repoRootRecordState{status: recordMissing}
		}
		return repoRootRecordState{status: recordUnreadable}
	}
	// Guard before reading, on the root-independent mount sources: a record an
	// agent could reach is missing whatever its contents, so a corrupt record or
	// a FIFO placed there can neither refuse the start nor block the broker.
	if reachable {
		slog.Warn("start: the repository root record is inside one of the agent's mounts; it is not used", "agent", agentName)
		return repoRootRecordState{status: recordMissing}
	}
	// Read with O_NOFOLLOW, O_NONBLOCK, regular-file-only and a size cap, so a
	// symlink or FIFO at the path cannot redirect or block the read.
	data, err := util.ReadSmallRegularFile(recPath, repoRootRecordMax)
	if err != nil {
		return repoRootRecordState{status: recordUnreadable}
	}
	var state provisionedWorktreeState
	if err := json.Unmarshal(data, &state); err != nil {
		return repoRootRecordState{status: recordUnreadable}
	}
	// Source does not gate trust: the agentDir guard and the equality check
	// against the derived root do. Ignoring Source keeps records written before
	// Source existed usable on upgrade. A record with no root is malformed.
	if state.RepoRoot == "" {
		return repoRootRecordState{status: recordUnreadable}
	}
	// With the record's own root known, the guard can add the recorded
	// repository's mount to the sources, which can only tighten it.
	if !agentDirOutsideAgentMounts(agentDir, agentMountSources(workspace, state.RepoRoot, volumes)) {
		slog.Warn("start: the agent directory is inside the recorded repository's mount; the record is not used", "agent", agentName)
		return repoRootRecordState{status: recordMissing}
	}
	return repoRootRecordState{status: recordPresent, root: state.RepoRoot}
}

// reconcileRepoRoot is the fail-closed intersection of the broker-known-path
// derivation D (derived, detectErr) and the record R (rec). It is the single
// place the mismatch/unreadable policy lives, so it can be changed in one spot.
// The decision table:
//
//  1. D refuses (detectErr): refuse, whatever R is.
//  2. D is a plain mount (derived == ""): plain mount, whatever R is.
//  3. D is a full root X and R is missing or untrusted: use X.
//  4. D is a full root X and R is present and readable: use X when R equals X,
//     otherwise refuse (R must not name a different root).
//  5. D is a full root X and R is present but unreadable or malformed: refuse.
func reconcileRepoRoot(agentName, derived string, detectErr error, rec repoRootRecordState) (string, error) {
	if detectErr != nil {
		return "", detectErr
	}
	if derived == "" {
		return "", nil
	}
	switch rec.status {
	case recordMissing:
		return derived, nil
	case recordPresent:
		if samePath(rec.root, derived) {
			return derived, nil
		}
		return "", fmt.Errorf("agent %q: the recorded repository root %q does not match the repository root %q derived from the project; refusing to start", agentName, rec.root, derived)
	default: // recordUnreadable
		return "", fmt.Errorf("agent %q: the repository root record is present but could not be read; refusing to start (derived repository root %q)", agentName, derived)
	}
}

// refuseWorkspaceInAgentsDir rejects an explicit workspace that is the
// project's own .scion directory, or that resolves at or below .scion/agents
// (which holds agents' own directories, never a workspace). Either would
// mount the agents tree read-write: .scion/agents is not covered by the
// in-project agents-dir mask (the agents directory would not sit at
// <workspace>/.scion/agents), and the mask shadows <workspace>/.scion/agents,
// not the whole of a .scion mounted directly. Other locations under .scion
// (for example the hub-native base's .scion/workspace/worktrees/<agent>) are
// allowed. projectDir is the resolved .scion directory (or its parent); both
// spellings are handled.
func refuseWorkspaceInAgentsDir(projectDir, workspace string) error {
	scionDir := projectDir
	if filepath.Base(projectDir) != config.DotScion {
		scionDir = filepath.Join(projectDir, config.DotScion)
	}
	rw, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		// Fail closed: the caller passes an existing workspace, so a
		// resolution failure here is unexpected.
		return fmt.Errorf("resolve workspace %q: %w", workspace, err)
	}
	// Refuse the workspace being the .scion directory itself.
	if rs, err := filepath.EvalSymlinks(scionDir); err == nil {
		if rw == rs {
			return fmt.Errorf("workspace %q is the project's .scion directory (%s), which holds agent state, not a workspace; choose a workspace outside it", workspace, scionDir)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("resolve project .scion %q: %w", scionDir, err)
	}
	// Refuse the workspace being, or being under, the agents directory.
	agentsDir := filepath.Join(scionDir, "agents")
	if ra, err := filepath.EvalSymlinks(agentsDir); err == nil {
		if pathUnder(rw, ra) {
			return fmt.Errorf("workspace %q is inside the project's agents directory (%s), which holds agent state, not a workspace; choose a workspace outside it", workspace, agentsDir)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("resolve project agents directory %q: %w", agentsDir, err)
	}
	return nil
}
