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

package projectfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// sharedLayouts returns the distinct shared Layouts a project's trees can
// be on: l, and the transitional shared-dir split when it is another share
// (shared dirs and nfs homes resolve there).
func (l Layout) sharedLayouts() []Layout {
	var out []Layout
	seen := map[string]bool{}
	for _, c := range []Layout{l, l.SharedDirLayout()} {
		c.SharedDirs = nil
		if !c.Backend.Shared() || seen[c.Key()] {
			continue
		}
		seen[c.Key()] = true
		out = append(out, c)
	}
	return out
}

// checkShareHostBase refuses an nfs Layout whose host base is not
// <mount root>/<share id> for a share ID that is one plain path element: a
// share ID such as ".." would move the host base to or above the mount
// root, and a deletion there could reach a tree beside it.
func (l Layout) checkShareHostBase() error {
	if l.Backend != BackendNFS || l.Share.ID == "" {
		return nil
	}
	id := l.Share.ID
	if id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") || filepath.Base(filepath.Clean(l.HostBase)) != id {
		return fmt.Errorf("%w: nfs share %q does not give a host base below the mount root (host base %q)", ErrRefused, id, l.HostBase)
	}
	return nil
}

// CheckTreeMounted refuses a shared Layout whose subpath root does not
// exist below its host base (the hub's health reports it too): every project on a shared backend has its
// tree there from create on, so a missing subpath root means the share is
// not mounted (only the mount point is), and a deletion must report that,
// never "absent". When the host base has no entries at all, the error also
// wraps ErrShareEmpty; it wraps ErrNoProjectTree either way. The emptiness
// is read through the same host-base descriptor as the walk
// (shareddirs.TreeDirState), and a read error is an error, never "empty".
func (l Layout) CheckTreeMounted() error {
	root, err := l.subPathRoot()
	if err != nil {
		return err
	}
	r := Resolved{Backend: l.Backend, HostBase: l.HostBase, RelPath: root}
	base, err := r.resolvedHostBase()
	if err != nil {
		return fmt.Errorf("%w: share not mounted: host base %s: %w", ErrRefused, l.HostBase, err)
	}
	exists, empty, err := shareddirs.TreeDirState(base, root)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %w", ErrRefused, err)
	case !exists && empty:
		return fmt.Errorf("%w: %w: %w: share not mounted: %s is empty", ErrRefused, ErrNoProjectTree, ErrShareEmpty, l.HostBase)
	case !exists:
		return fmt.Errorf("%w: %w: share not mounted: %s has no %s directory", ErrRefused, ErrNoProjectTree, l.HostBase, root)
	}
	return nil
}

// CheckTreesMounted runs CheckTreeMounted on every shared Layout of l (l
// and its transitional shared-dir split); nil on the local backend.
func (l Layout) CheckTreesMounted() error {
	var errs []error
	for _, sl := range l.sharedLayouts() {
		if err := sl.CheckTreeMounted(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deleteShared removes r (a directory resolved on a shared Layout) with
// shareddirs.DeleteTree: every component below the host base is opened
// without following symlinks, and a symlink at any of them is refused. The
// host base itself (the operator's mount point) may sit behind a symlink.
func deleteShared(tree TreeKind, r Resolved) Deletion {
	base, err := r.resolvedHostBase()
	if err != nil {
		return deletion(tree, r.HostPath, false, fmt.Errorf("%w: host base of %s: %w", ErrRefused, r.HostPath, err))
	}
	existed, err := shareddirs.DeleteTree(base, r.RelPath)
	if errors.Is(err, shareddirs.ErrTreeRefused) {
		err = fmt.Errorf("%w: %w", ErrRefused, err)
	}
	return deletion(tree, r.HostPath, existed, err)
}

// RemoveProjectTree removes projectID's project root <SubPathRoot>/<projectID>
// (workspace, project configuration, shared dirs, provisioning state,
// worktrees and agent directories) from every shared Layout of l (l and
// its transitional shared-dir split), and reports one Deletion per root.
//
// The removal is confined: every component below the host base is a plain
// directory or the removal is refused (a symlink is never followed), and
// a symlink inside the tree is unlinked, never followed. An invalid
// project ID is refused before anything is touched. A missing root is
// OutcomeAbsent, so the call is idempotent; an unusable host base (the
// share not mounted) is OutcomeFailed.
//
// On the local backend it removes nothing and returns no Deletion: the
// local slug-keyed trees keep their existing deleters.
func (l Layout) RemoveProjectTree(projectID string) ([]Deletion, error) {
	if err := validProjectID(projectID); err != nil {
		return nil, err
	}
	var out []Deletion
	for _, sl := range l.sharedLayouts() {
		r, err := sl.ProjectRoot(projectID)
		if err != nil {
			return out, err
		}
		if err := sl.checkShareHostBase(); err != nil {
			out = append(out, deletion(TreeProjectRoot, r.HostPath, false, err))
			continue
		}
		if err := sl.CheckTreeMounted(); err != nil {
			out = append(out, deletion(TreeProjectRoot, r.HostPath, false, err))
			continue
		}
		out = append(out, deleteShared(TreeProjectRoot, r))
	}
	return out, nil
}

// AgentWorkspacePendingReason is the reason of a kept agent directory: it
// still holds entries the hub does not own, normally the agent's own
// workspace, which only the broker removes.
const AgentWorkspacePendingReason = "the agent directory is not empty: workspace pending broker removal"

// RemoveAgentStorage removes what the hub owns of agent agentID (the hub agent ID,
// ValidAgentID) with slug agent: its home-<agentID> and secrets-<agentID>
// below agents/<slug> on the Layout homes resolve on, then the directory
// agents/<slug> itself on every shared Layout of l, but only when it is
// empty. It never removes anything else below agents/<slug>: the agent's
// own workspace (and its worktree under workspace/worktrees/) is the
// broker's to remove, under the provisioning lock and with its git
// bookkeeping. A directory that still has entries is kept
// (OutcomeKept, AgentWorkspacePendingReason). Confinement is RemoveProjectTree's.
// An invalid project ID, slug or agent ID is refused before anything is
// touched; what is missing is OutcomeAbsent. On the local backend it
// removes nothing.
func (l Layout) RemoveAgentStorage(projectID, agent, agentID string) ([]Deletion, error) {
	if err := validProjectID(projectID); err != nil {
		return nil, err
	}
	if err := validAgentSlug(agent); err != nil {
		return nil, err
	}
	// An agent ID equal to the slug is refused: home-<id> and the slug
	// would then name the same incarnation-independent leaf (agentLeaf
	// refuses it too), which only an agent named exactly as its UUID hits.
	if !ValidAgentID(agentID) || agentID == agent {
		return nil, fmt.Errorf("projectfs: invalid agent ID %q", agentID)
	}
	var out []Deletion
	if hl := l.SharedDirLayout(); hl.Backend.Shared() {
		home, err := l.AgentHome(projectID, agent, agentID)
		if err != nil {
			return nil, err
		}
		secrets, err := l.AgentSecrets(projectID, agent, agentID)
		if err != nil {
			return nil, err
		}
		if err := hl.checkShareHostBase(); err != nil {
			out = append(out, deletion(TreeAgentHome, home.HostPath, false, err), deletion(TreeAgentSecrets, secrets.HostPath, false, err))
		} else {
			out = append(out, deleteShared(TreeAgentHome, home), deleteShared(TreeAgentSecrets, secrets))
		}
	}
	for _, sl := range l.sharedLayouts() {
		r, err := sl.AgentRoot(projectID, agent)
		if err != nil {
			return out, err
		}
		if err := sl.checkShareHostBase(); err != nil {
			out = append(out, deletion(TreeAgentRoot, r.HostPath, false, err))
			continue
		}
		out = append(out, removeEmptyShared(TreeAgentRoot, r))
	}
	return out, nil
}

// removeEmptyShared removes r (a directory on a shared Layout) only when it
// is empty (shareddirs.RemoveDirIfEmpty); a directory with entries is kept.
// Known benign race: a broker provisioning a re-created agent of the same
// slug may have just created the (still empty) directory; removing it
// loses nothing (the removal is not recursive, and homes and secrets are
// keyed by agent ID), and at worst that start fails and is retried.
func removeEmptyShared(tree TreeKind, r Resolved) Deletion {
	base, err := r.resolvedHostBase()
	if err != nil {
		return deletion(tree, r.HostPath, false, fmt.Errorf("%w: host base of %s: %w", ErrRefused, r.HostPath, err))
	}
	existed, notEmpty, err := shareddirs.RemoveDirIfEmpty(base, r.RelPath)
	if errors.Is(err, shareddirs.ErrTreeRefused) {
		err = fmt.Errorf("%w: %w", ErrRefused, err)
	}
	d := deletion(tree, r.HostPath, existed, err)
	if err == nil && notEmpty {
		d.Outcome, d.Reason = OutcomeKept, AgentWorkspacePendingReason
	}
	return d
}

// Deferred project deletion (forced deletes, ptone/scion#5835): instead of
// removing a project root at once, the hub records in the tree the time
// after which it may be removed, in the authority's project-scoped state
// directory (provision/, inside no agent Mount), and its maintenance pass
// removes the tree once that time has passed.

// provisionDir is a project's state directory below its project root.
const provisionDir = "provision"

// DeleteAfterPath resolves the tombstone's directory, <ProjectRoot>/provision,
// on a shared Layout.
func (l Layout) DeleteAfterPath(projectID string) (Resolved, error) {
	pr, err := l.ProjectRoot(projectID)
	if err != nil {
		return Resolved{}, err
	}
	return l.shared(filepath.Join(pr.RelPath, provisionDir), Mount{}), nil
}

// MarkDeleteAfter writes the tombstone with time after on every shared
// Layout of l that holds the project root, without following a symlink at
// any component, and reports one Deletion per root (OutcomeKept with the
// deferral as the reason, or OutcomeFailed; a root that does not exist
// needs no tombstone and is OutcomeAbsent).
func (l Layout) MarkDeleteAfter(projectID string, after time.Time) ([]Deletion, error) {
	if err := validProjectID(projectID); err != nil {
		return nil, err
	}
	reason := "forced delete: deletion deferred until " + after.UTC().Format(time.RFC3339)
	var out []Deletion
	for _, sl := range l.sharedLayouts() {
		pr, err := sl.ProjectRoot(projectID)
		if err != nil {
			return out, err
		}
		if err := sl.checkShareHostBase(); err != nil {
			out = append(out, deletion(TreeProjectRoot, pr.HostPath, false, err))
			continue
		}
		base, err := pr.resolvedHostBase()
		if err != nil {
			out = append(out, deletion(TreeProjectRoot, pr.HostPath, false, fmt.Errorf("%w: host base of %s: %w", ErrRefused, pr.HostPath, err)))
			continue
		}
		if ok, err := shareddirs.DirExistsInTree(base, pr.RelPath); err != nil || !ok {
			if err != nil {
				out = append(out, deletion(TreeProjectRoot, pr.HostPath, false, err))
			} else {
				out = append(out, deletion(TreeProjectRoot, pr.HostPath, false, nil))
			}
			continue
		}
		rel := filepath.Join(pr.RelPath, provisionDir)
		if err := shareddirs.WriteFileInTree(base, rel, DeleteAfterFile, []byte(after.UTC().Format(time.RFC3339)+"\n"), true); err != nil {
			out = append(out, deletion(TreeProjectRoot, pr.HostPath, true, err))
			continue
		}
		out = append(out, Deletion{Tree: TreeProjectRoot, Path: pr.HostPath, Outcome: OutcomeKept, Reason: reason})
	}
	return out, nil
}

// DeferredDeletions lists the project roots on every shared Layout of l
// that carry a tombstone: one listing of the subpath root and one tombstone
// read per entry, none of them following a symlink. A subpath root that
// does not exist is an error (the share may not be mounted).
func (l Layout) DeferredDeletions() ([]DeferredDeletion, error) {
	var out []DeferredDeletion
	var errs []error
	for _, sl := range l.sharedLayouts() {
		root, err := sl.subPathRoot()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := sl.checkShareHostBase(); err != nil {
			errs = append(errs, err)
			continue
		}
		r := Resolved{Backend: sl.Backend, HostBase: sl.HostBase, RelPath: root}
		base, err := r.resolvedHostBase()
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: host base %s: %w", ErrRefused, sl.HostBase, err))
			continue
		}
		names, ok, err := shareddirs.ListDirInTree(base, root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			errs = append(errs, fmt.Errorf("%w: %w: %s has no %s directory; the share may not be mounted", ErrRefused, ErrNoProjectTree, sl.HostBase, root))
			continue
		}
		for _, name := range names {
			if validProjectID(name) != nil {
				continue
			}
			data, ok, err := shareddirs.ReadFileInTree(base, filepath.Join(root, name, provisionDir), DeleteAfterFile, 256)
			if err != nil || !ok {
				continue
			}
			after, _ := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
			out = append(out, DeferredDeletion{ProjectID: name, After: after, Layout: sl, Path: filepath.Join(sl.HostBase, root, name)})
		}
	}
	return out, errors.Join(errs...)
}

// ClearDeleteAfter removes the tombstone of projectID on every shared
// Layout of l (a project that exists again keeps its tree).
func (l Layout) ClearDeleteAfter(projectID string) error {
	if err := validProjectID(projectID); err != nil {
		return err
	}
	var errs []error
	for _, sl := range l.sharedLayouts() {
		r, err := sl.DeleteAfterPath(projectID)
		if err != nil {
			return err
		}
		base, err := r.resolvedHostBase()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := shareddirs.RemoveFileInTree(base, r.RelPath, DeleteAfterFile); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
