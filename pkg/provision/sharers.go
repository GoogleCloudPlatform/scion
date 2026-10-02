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

package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// sharerMarker is the on-disk JSON shape stored per shared branch.
type sharerMarker struct {
	Branch       string   `json:"branch"`
	WorktreePath string   `json:"worktreePath"`
	Sharers      []string `json:"sharers"`
}

const sharerDir = "scion-sharers"

// sharerPath returns the marker file path for a branch under the base repo.
func sharerPath(base, branch string) string {
	return filepath.Join(base, ".git", sharerDir, sanitizeBranchName(branch)+".json")
}

// WorktreePathIsScionCreated reports whether candidate matches one of the two
// shapes scion itself creates a worktree at:
//
//  1. ProvisionShared (pkg/provision/provision.go): base/worktrees/<name> —
//     WorktreeIsLexicallyUnderBase's exact contract, used as-is. base is the
//     registry base (the repo root).
//  2. ProvisionAgent's worktree-mode create path (pkg/agent/provision.go,
//     config.SelectAgentsRoot/GetAgentDir): projectDir/agents/<name>/workspace
//     — a different shape WorktreeIsLexicallyUnderBase cannot express (it is
//     hardcoded to a single segment under a literal "worktrees", not two
//     segments under "agents" ending in "workspace"), so it gets its own
//     lexical check with the same fail-closed posture, without modifying the
//     frozen validator in worktree_validate.go.
//
// projectDir is the actual, resolved project directory the caller has in
// scope (config.GetResolvedProjectDir's return value) — NOT derived from base
// (e.g. as base/.scion). A project's .scion directory does not always sit at
// the repo top level (a project can be initialized in a repo subdirectory),
// so reconstructing it from base would false-reject a legitimate shape-2
// marker whenever that layout is in play, degrading it to a leak. Callers
// that have no meaningful shape-2 concept for their layout (ProvisionShared,
// the hub read path — both always produce shape-1 paths) pass projectDir="";
// isProvisionAgentWorkspaceShape then always reports false, leaving
// shape-1 as the sole check, which is correct for those callers.
//
// A candidate that matches NEITHER shape was not created by scion — most
// notably, ProvisionAgent's "attach to an existing worktree" path
// (util.FindWorktreeByBranch) can register a worktree anywhere git's own
// worktree list reports one, including a location a user created by hand.
// Such a path must never be used to mount or remove (see readMarker); it is
// not, by itself, evidence of an unexpected on-disk change.
func WorktreePathIsScionCreated(base, projectDir, candidate string) bool {
	return WorktreeIsLexicallyUnderBase(base, candidate) || isProvisionAgentWorkspaceShape(projectDir, candidate)
}

// agentsSubdirName mirrors config.SelectAgentsRoot's non-shared-workspace
// return value: filepath.Join(projectDir, "agents"). Duplicated as a literal
// (like pkg/runtime/common.go's sharerRegistryDirName) rather than importing
// pkg/config, which pkg/provision must not do — cmd/sciontool imports
// pkg/provision, and pkg/config carries broker-side project-path resolution
// that in-container code must never have transitive access to (see
// cmd/sciontool/commands/init_test.go's TestInitProjectDataIsolation, a
// compile-time canary for exactly this).
const agentsSubdirName = "agents"

// isProvisionAgentWorkspaceShape reports whether candidate is lexically
// exactly <projectDir>/agents/<single-clean-element>/workspace — the shape
// pkg/agent.ProvisionAgent's worktree-mode create path uses
// (config.SelectAgentsRoot(projectDir, false), a non-shared-workspace call
// always returning exactly this). Reports false when projectDir is empty
// (see WorktreePathIsScionCreated for which callers pass "").
//
// This is a purely lexical, string-only comparison — no filepath.EvalSymlinks
// anywhere in this function. That is deliberate: this is a classification
// check on the RECORDED form of a path (does it look like something scion
// built), not a check on where a path resolves on disk (that
// resolved-containment check belongs at the removal step in
// util.RemoveWorktree, a different concern). Resolving symlinks here would
// falsely reject a legitimate marker whenever any ancestor (a symlinked
// project directory, most commonly) differs textually between how the path
// was recorded and how it would resolve — see RegisterSharer's doc comment
// for the write-side half of this same concern (an immutable first-recorded
// path).
//
// Mirrors WorktreeIsLexicallyUnderBase's wildcard-single-segment approach,
// but the wildcard (the agent name) precedes a fixed "workspace" leaf instead
// of following a fixed "worktrees" prefix. Both projectDir and candidate must
// be non-empty absolute paths; either being empty, relative, or not matching
// the shape reports false rather than resolving against the calling
// process's current working directory.
func isProvisionAgentWorkspaceShape(projectDir, candidate string) bool {
	if projectDir == "" || candidate == "" || !filepath.IsAbs(projectDir) || !filepath.IsAbs(candidate) {
		return false
	}
	agentsDir := filepath.Join(filepath.Clean(projectDir), agentsSubdirName)
	rel, err := filepath.Rel(agentsDir, filepath.Clean(candidate))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	segments := strings.SplitN(rel, string(filepath.Separator), 3)
	return len(segments) == 2 && segments[0] != "" && segments[1] == "workspace"
}

// readMarker loads the marker file for a branch under base. Returns nil (no
// error) when the file does not exist. projectDir is passed through to
// WorktreePathIsScionCreated for the ProvisionAgent-layout shape check (see
// its doc comment); pass "" for callers with no such layout.
//
// This is the single read boundary every consumer of the sharer registry
// passes through (RegisterSharer, UnregisterSharer, ListSharers,
// FindBranchForAgent). The recorded WorktreePath is not produced solely by
// scion's own write path and is treated as untrusted input; a source able to
// set WorktreePath could set Sharers too. It is classified into one of three
// outcomes:
//   - It matches a scion-created worktree shape (WorktreePathIsScionCreated:
//     base/worktrees/<name>, or the ProvisionAgent-layout
//     projectDir/agents/<name>/workspace): the marker is returned as-is; the
//     caller still independently proves it is a genuine git worktree before
//     acting on it (ValidateWorktreeForBase / IsValidJoinWorktree).
//   - It matches neither shape but shows no sign of a non-canonical or
//     symlink-crossing form (see shouldRefuseWorktreePath) — a plain stale
//     or foreign value, including ProvisionAgent's legitimate
//     attach-to-an-existing-worktree path, which git's own worktree list can
//     point anywhere: the Sharers refcount is preserved and only
//     WorktreePath is blanked, not the whole marker discarded (discarding it
//     previously caused a real data-loss regression — a live sharer's
//     worktree removed out from under it because its refcount registration
//     was silently dropped). Callers observe this as worktreePath=="" and
//     must not use "" as a target to mount or remove.
//   - It is not already in canonical textual form, or crosses a symlink on
//     its way from base to the leaf: the read fails outright with an error,
//     instead of being silently discarded or degraded, so the caller cannot
//     mistake a value shaped this way for ordinary stale state.
//
// projectDir is passed through to WorktreePathIsScionCreated for the
// ProvisionAgent-layout shape check (see its doc comment); pass "" for
// callers with no such layout.
func readMarker(base, projectDir, path string) (*sharerMarker, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m sharerMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.WorktreePath == "" || WorktreePathIsScionCreated(base, projectDir, m.WorktreePath) {
		return &m, nil
	}
	if shouldRefuseWorktreePath(base, m.WorktreePath) {
		return nil, fmt.Errorf("sharer marker %s: worktreePath %q is not in canonical form or crosses a symlink", path, m.WorktreePath)
	}
	slog.Warn("sharer marker worktreePath does not match a scion-created worktree shape; keeping sharer refcount, discarding only the path",
		"path", path, "worktreePath", m.WorktreePath)
	m.WorktreePath = ""
	return &m, nil
}

// shouldRefuseWorktreePath reports whether an untrusted worktreePath value
// shows a non-canonical or symlink-crossing shape, as opposed to merely
// being stale or foreign. Two signs, either sufficient:
//
//   - The stored text is not already in filepath.Clean'd form — it embeds a
//     ".." or a redundant separator — so its lexical and logical
//     interpretations disagree (e.g. "worktrees/a/up/../a" naming
//     "worktrees/a" lexically after cleaning, while reading as something
//     under "up" before that).
//   - Some component strictly between base and the leaf, read lexically
//     from the stored (already absolute, in-tree-by-prefix) path, is itself
//     a symlink on disk — the marker's own text can point convincingly at
//     "worktrees/<name>" while actually resolving somewhere else entirely.
//
// A value that is empty, relative, or simply outside base's tree altogether
// does not warrant a refusal by this definition — it has no claim on being
// in-tree to begin with, so there is nothing to disagree with. Resolution
// failures (a missing intermediate directory, for example) are treated the
// same way: readMarker's caller already discards a path that doesn't check
// out as a real worktree, which covers that case without this function
// needing to distinguish a filesystem error from a benign absence.
func shouldRefuseWorktreePath(base, path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	if path != filepath.Clean(path) {
		return true
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Genuinely outside base (or base itself) — foreign, with no claim
		// on being in-tree to begin with.
		return false
	}
	for dir := filepath.Dir(path); strings.HasPrefix(dir, base+string(filepath.Separator)); dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if err != nil {
			return false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// writeMarkerAtomic writes the marker via a temp file + rename to avoid torn
// reads. The caller MUST hold the per-project advisory lock / provision mutex.
func writeMarkerAtomic(path string, m *sharerMarker) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Unique temp file (not a static path+".tmp") so concurrent writers don't
	// clobber each other's temp data before the atomic rename.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// hasRealWorktreeGitfile reports whether path is non-empty and currently has
// a .git entry (file or directory) on disk — a cheap, shape-agnostic signal
// that a recorded WorktreePath is actually backed by a worktree, used by
// RegisterSharer to decide whether an existing recorded path is worth
// protecting from being overwritten (see its doc comment). Deliberately not
// EvalSymlinks-based or a full worktree-relationship check (ValidateWorktreeForBase
// is shape-1-only and frozen) — existence of a .git entry is enough to
// distinguish "something real was created here" from "this was never
// backed by anything" (an externally-written or stale value with no
// worktree behind it), which is exactly the distinction this decision needs.
func hasRealWorktreeGitfile(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// RegisterSharer adds agentID to the sharer list for the given branch
// worktree. The call is idempotent: re-registering an already-present agent is
// a no-op. worktreePath is recorded so that teardown can locate the worktree
// directory even after the last sharer unregisters — but once a marker's
// recorded WorktreePath is backed by a real, on-disk worktree
// (hasRealWorktreeGitfile), later RegisterSharer calls never replace it; they
// only add/refresh the Sharers entry. A recorded path that is NOT currently
// backed by a real worktree (empty, or an externally-written or stale value
// with no .git there) is not worth protecting and is replaced normally — this is
// what lets a legitimate CREATE overwrite a stale or fake registry entry, per
// the existing JOIN-fallback-to-CREATE behavior in ensureWorktree.
//
// The immutability matters beyond just guarding against a peer overwriting a
// good path with a bad one: two legitimate callers can report the SAME
// on-disk worktree in two different string forms. pkg/agent's ProvisionAgent
// worktree-create path (:902) passes the RECORDED form it just built
// (matching config.SelectAgentsRoot's own construction, which
// WorktreePathIsScionCreated's classification check compares against
// lexically); its attach-to-existing-worktree path (:847) passes whatever
// util.FindWorktreeByBranch reports, which is git's own (possibly symlink-
// resolved) view of the same directory. If a later joiner's call were allowed
// to overwrite the creator's recorded form with git's resolved form — e.g.
// because some ancestor (a symlinked project directory, most commonly)
// differs between the two string forms — the marker would still point at the
// right directory on disk, but would no longer match the recorded-form shape
// the classification check expects, degrading a legitimate marker's
// WorktreePath to blank and leaking the worktree at the last sharer instead
// of removing it. Keeping a real recorded form immutable for the marker's
// life closes this from the write side; WorktreePathIsScionCreated's
// lexical-only (no EvalSymlinks) comparison closes the matching read-side
// gap — see its doc comment. Both are required together.
//
// projectDir is passed through to the read boundary's shape check — see
// readMarker; pass "" for callers with no ProvisionAgent-layout concept.
//
// Callers MUST hold the per-project advisory lock / provision mutex.
func RegisterSharer(base, projectDir, branch, worktreePath, agentID string) error {
	p := sharerPath(base, branch)
	m, err := readMarker(base, projectDir, p)
	if err != nil {
		return err
	}
	if m == nil {
		m = &sharerMarker{Branch: branch}
	}
	if worktreePath != "" && !hasRealWorktreeGitfile(m.WorktreePath) {
		m.WorktreePath = worktreePath
	}
	if !slices.Contains(m.Sharers, agentID) {
		m.Sharers = append(m.Sharers, agentID)
	}
	return writeMarkerAtomic(p, m)
}

// UnregisterSharer removes agentID from the sharer list for the given branch.
// It returns the remaining sharers and the recorded worktreePath. When the
// sharer list becomes empty the marker file is deleted, but worktreePath is
// still returned so the caller can remove the worktree directory. projectDir
// is passed through to the read boundary's shape check — see readMarker.
//
// Unregistering an agent that is not in the list is a no-op (returns the
// current state). If no marker exists, remaining is nil and worktreePath is "".
//
// Callers MUST hold the per-project advisory lock / provision mutex.
func UnregisterSharer(base, projectDir, branch, agentID string) (remaining []string, worktreePath string, err error) {
	p := sharerPath(base, branch)
	m, err := readMarker(base, projectDir, p)
	if err != nil {
		return nil, "", err
	}
	if m == nil {
		return nil, "", nil
	}
	m.Sharers = slices.DeleteFunc(m.Sharers, func(s string) bool { return s == agentID })
	if len(m.Sharers) == 0 {
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return nil, m.WorktreePath, rerr
		}
		return []string{}, m.WorktreePath, nil
	}
	if err := writeMarkerAtomic(p, m); err != nil {
		return nil, m.WorktreePath, err
	}
	return m.Sharers, m.WorktreePath, nil
}

// ListSharers returns the current sharer agent IDs and worktreePath for a
// branch. If no marker exists, sharers is nil and worktreePath is "".
// projectDir is passed through to the read boundary's shape check — see
// readMarker.
func ListSharers(base, projectDir, branch string) ([]string, string, error) {
	p := sharerPath(base, branch)
	m, err := readMarker(base, projectDir, p)
	if err != nil {
		return nil, "", err
	}
	if m == nil {
		return nil, "", nil
	}
	return m.Sharers, m.WorktreePath, nil
}

// FindBranchForAgent scans all marker files under base to find which branch
// (and worktree path) agentID is sharing. Returns found=false when the agent
// is not present in any marker. projectDir is passed through to the read
// boundary's shape check — see readMarker.
func FindBranchForAgent(base, projectDir, agentID string) (branch, worktreePath string, found bool, err error) {
	dir := filepath.Join(base, ".git", sharerDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		m, err := readMarker(base, projectDir, filepath.Join(dir, e.Name()))
		if err != nil {
			// A single corrupted/unreadable marker must not block the whole
			// scan (and thus all agent deletions). Skip it and keep looking;
			// dir-level failures are still returned above.
			slog.Warn("FindBranchForAgent: skipping unreadable sharer marker",
				"file", e.Name(), "error", err)
			continue
		}
		if m != nil && slices.Contains(m.Sharers, agentID) {
			return m.Branch, m.WorktreePath, true, nil
		}
	}
	return "", "", false, nil
}
