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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Staging and commit of the signed-URL workspace bootstrap.
//
// Every file is downloaded first into a staging directory S, a sibling of
// the workspace W on the same filesystem named exactly
// .<base(W)>.bootstrap-<32 hex digits>. A failed download removes S and
// never touches W. The commit depends on how W is reached:
//
//   - case A, a project-level workspace that was absent at admission: W must
//     still be absent under the workspace lock (else the create skips, as
//     for a workspace that existed at admission), and S is renamed onto W in
//     one renameat(2);
//   - case B, the shared Layout's workspace leaf: under the provisioning
//     locks the leaf must hold no entries at all; S is created with the
//     leaf's owner, mode and default ACL and renamed over the empty leaf in
//     one renameat(2), and the sentinel is written after it;
//   - case C, a per-agent directory or a project path not checked at
//     admission, which is always applied: S is applied into W in place, file
//     by file, and W's entries outside the listed set are removed, except
//     the root identity entry, as the rclone mirror does.
//
// In cases A and B W changes only by one atomic rename, so a crash leaves
// either the old W or the complete new one, plus at most a stale S. A stale
// S is removed before the skip and in-use checks: for cases A and C right
// after the workspace lock is taken, for case B inside the provisioning
// locks, which every writer of an S for a shared leaf holds.

// bootstrapStageInfix separates W's base name from the stage suffix.
const bootstrapStageInfix = ".bootstrap-"

// bootstrapStageSuffixLen is the length of the stage name's hex suffix.
const bootstrapStageSuffixLen = 32

// Signed bootstrap commit error classes (signedCommitError.Class).
const (
	signedCommitLeafBusy = "leaf_busy"
	signedCommitStage    = "stage"
	signedCommitApply    = "commit"
	signedCommitRecover  = "recover"
	// signedCommitAmbiguous: more than one complete stage; nothing is
	// touched.
	signedCommitAmbiguous = "recovery_ambiguous"
	// signedCommitIdentityRestore: a failed case A commit could not put the
	// identity entry back; the stage holding it is kept for recovery.
	signedCommitIdentityRestore = "identity_restore"
)

// signedCommitError is a staging or commit failure of the signed bootstrap.
// It carries a fixed class only: the underlying filesystem errors name the
// broker's paths.
type signedCommitError struct {
	Class string
}

func (e *signedCommitError) Error() string {
	return "signed workspace bootstrap: " + e.Class
}

// errSignedBootstrapCrash is returned by a test crash hook: the bootstrap
// returns at once and skips its cleanup, as a process abort would.
var errSignedBootstrapCrash = errors.New("simulated crash")

// testHookSignedBootstrap, when set (tests only), is called at each commit
// step ("before_rename", "after_rename", "apply_file"). A returned
// errSignedBootstrapCrash makes the bootstrap return without cleanup.
var testHookSignedBootstrap func(step string) error

// signedBootstrapHook runs testHookSignedBootstrap for step.
func signedBootstrapHook(step string) error {
	if testHookSignedBootstrap == nil {
		return nil
	}
	return testHookSignedBootstrap(step)
}

// newBootstrapStageName returns a fresh stage name for workspace base name
// base.
func newBootstrapStageName(base string) (string, error) {
	var b [bootstrapStageSuffixLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "." + base + bootstrapStageInfix + hex.EncodeToString(b[:]), nil
}

// isBootstrapStageName reports whether name is exactly a stage name for
// workspace base name base: the fixed prefix and 32 lower-case hex digits.
func isBootstrapStageName(name, base string) bool {
	suffix, ok := strings.CutPrefix(name, "."+base+bootstrapStageInfix)
	if !ok || len(suffix) != bootstrapStageSuffixLen {
		return false
	}
	for _, c := range suffix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// removeStaleBootstrapStages removes every stale stage directory of the
// workspace named base in parent. Only entries matching the exact stage name
// form are considered, through an os.Root on parent; a symlink or a
// non-directory with such a name is neither followed nor removed. A missing
// parent has nothing to remove. It returns how many it removed.
func removeStaleBootstrapStages(parent, base string) (int, error) {
	root, err := os.OpenRoot(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, &signedCommitError{Class: signedCommitRecover}
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, &signedCommitError{Class: signedCommitRecover}
	}
	removed := 0
	for _, e := range entries {
		if !isBootstrapStageName(e.Name(), base) {
			continue
		}
		fi, err := root.Lstat(e.Name())
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			continue
		}
		if err := root.RemoveAll(e.Name()); err != nil {
			return removed, &signedCommitError{Class: signedCommitRecover}
		}
		removed++
	}
	return removed, nil
}

// signedBootstrapLocal runs the signed bootstrap of a host-local workspace
// w under the workspace lock, after recovery: case A for a project-level
// workspace checked at admission, case C otherwise. In the signed mode the
// skip is decided here, not at admission: case A skips a workspace that
// holds any entry other than its root identity entry, and applies to one
// that is absent or holds only that entry, so an attempt that failed
// cleanly earlier is applied again. populated reports a completed case A
// bootstrap, which created the project directory's content.
func (s *Server) signedBootstrapLocal(ctx context.Context, req CreateAgentRequest, w string) (skipped, populated bool, err error) {
	if req.ProjectSlug != "" && req.workspaceAdmissionChecked {
		skipped, err := s.signedBootstrapAbsent(ctx, req, w)
		// Case A applies only to an absent or marker-only workspace, so a
		// completed case A bootstrap populated the project directory.
		return skipped, !skipped && err == nil, err
	}
	return false, false, s.signedBootstrapInPlace(ctx, req, w)
}

// workspaceHoldsOnlyIdentity reports whether the directory w holds no entry
// other than its root identity entry. Creating the agent's start context
// writes that entry into a project workspace before the download, so a
// workspace in that state has no content yet.
func workspaceHoldsOnlyIdentity(w string) (bool, error) {
	entries, err := os.ReadDir(w)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !transfer.IsWorkspaceIdentityName(e.Name()) {
			return false, nil
		}
	}
	return true, nil
}

// signedBootstrapAbsent is case A: a project-level workspace w checked at
// admission. Under the workspace lock, after recovery, a w that holds any
// entry other than its root identity entry is in use and is skipped; an
// absent w, or one holding only the identity entry (written with the start
// context before the download), is applied. Otherwise everything is downloaded into a stage beside w; the
// identity entry is then moved into the stage, which marks the stage
// complete, and the stage is renamed over the now empty w in one
// renameat(2). A crash between those two renames leaves an empty w and a
// complete stage holding the identity entry, which recovery renames into
// place (recoverLocalBootstrapStages).
func (s *Server) signedBootstrapAbsent(ctx context.Context, req CreateAgentRequest, w string) (skipped bool, err error) {
	parent, base := filepath.Dir(w), filepath.Base(w)
	exists := false
	if fi, err := os.Lstat(w); err == nil {
		if !fi.IsDir() {
			return false, &signedCommitError{Class: signedCommitStage}
		}
		only, err := workspaceHoldsOnlyIdentity(w)
		if err != nil {
			return false, &signedCommitError{Class: signedCommitStage}
		}
		if !only {
			return true, nil
		}
		exists = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, &signedCommitError{Class: signedCommitStage}
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, &signedCommitError{Class: signedCommitStage}
	}
	stage, err := newBootstrapStageName(base)
	if err != nil {
		return false, &signedCommitError{Class: signedCommitStage}
	}
	stagePath := filepath.Join(parent, stage)
	// The stage is created as the workspace directory is today.
	if err := os.Mkdir(stagePath, 0o755); err != nil {
		return false, &signedCommitError{Class: signedCommitStage}
	}
	if _, err := s.signedWorkspaceDownload(ctx, req, stagePath); err != nil {
		_ = os.RemoveAll(stagePath)
		return false, err
	}
	if err := signedBootstrapHook("before_rename"); err != nil {
		return false, err
	}
	if exists {
		if err := moveIdentityIntoStage(parent, base, stage); err != nil {
			_ = os.RemoveAll(stagePath)
			return false, err
		}
		if err := signedBootstrapHook("identity_moved"); err != nil {
			return false, err
		}
	}
	if err := renameSiblingDirFn(parent, stage, base); err != nil {
		// Put the identity entry back before removing the stage, so w is
		// left as it was. (A concurrent create of the same project can
		// rewrite w's identity entry meanwhile, and the rename then fails
		// with ENOTEMPTY.) If the identity entry cannot be put back, the
		// stage is kept, so the entry is never lost: recovery restores it.
		if exists {
			if err := moveIdentityBackFn(parent, base, stage); err != nil {
				return false, &signedCommitError{Class: signedCommitIdentityRestore}
			}
		}
		_ = os.RemoveAll(stagePath)
		return false, &signedCommitError{Class: signedCommitApply}
	}
	if err := signedBootstrapHook("after_rename"); err != nil {
		return false, err
	}
	return false, nil
}

// moveIdentityIntoStage renames w's root identity entry into the stage, if
// w has one. The downloaded set never holds a root identity entry
// (transfer.ExcludeWorkspaceObject), so nothing is replaced.
func moveIdentityIntoStage(parent, w, stage string) error {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return &signedCommitError{Class: signedCommitApply}
	}
	defer func() { _ = root.Close() }()
	src := w + "/" + transfer.WorkspaceIdentityEntry
	if _, err := root.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := root.Rename(src, stage+"/"+transfer.WorkspaceIdentityEntry); err != nil {
		return &signedCommitError{Class: signedCommitApply}
	}
	return nil
}

// renameSiblingDirFn and moveIdentityBackFn are the case A commit's rename
// steps, replaced only by tests to inject failures.
var (
	renameSiblingDirFn = renameSiblingDir
	moveIdentityBackFn = moveIdentityBack
)

// moveIdentityBack undoes moveIdentityIntoStage.
func moveIdentityBack(parent, w, stage string) error {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	src := stage + "/" + transfer.WorkspaceIdentityEntry
	if _, err := root.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return root.Rename(src, w+"/"+transfer.WorkspaceIdentityEntry)
}

// recoverLocalBootstrapStages is the recovery of a host-local workspace's
// stages, run under the workspace lock before the skip check. A stale stage
// holding a root identity entry is complete: case A crashed between moving
// the identity entry into it and renaming it over w. If w is then empty or
// absent, that stage is renamed into place (roll forward). If w has gained
// entries since (the next create's start context writes a fresh identity
// entry first, which is the usual route), the stage is removed with the
// others and the download is applied again. Two or more complete stages
// fail closed with recovery_ambiguous and nothing is touched.
func recoverLocalBootstrapStages(parent, base string) error {
	root, err := os.OpenRoot(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return &signedCommitError{Class: signedCommitRecover}
	}
	all, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		_ = root.Close()
		return &signedCommitError{Class: signedCommitRecover}
	}
	var complete []string
	for _, e := range all {
		if !isBootstrapStageName(e.Name(), base) {
			continue
		}
		fi, err := root.Lstat(e.Name())
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			continue
		}
		if _, err := root.Lstat(e.Name() + "/" + transfer.WorkspaceIdentityEntry); err == nil {
			complete = append(complete, e.Name())
		}
	}
	_ = root.Close()
	if len(complete) > 1 {
		return &signedCommitError{Class: signedCommitAmbiguous}
	}
	if len(complete) == 1 {
		w := filepath.Join(parent, base)
		entries, err := os.ReadDir(w)
		switch {
		case errors.Is(err, fs.ErrNotExist) || (err == nil && len(entries) == 0):
			if err := renameSiblingDir(parent, complete[0], base); err != nil {
				return &signedCommitError{Class: signedCommitRecover}
			}
		case err == nil:
			// W has entries but no identity entry of its own: a failed
			// commit kept the stage because it could not put the identity
			// entry back. Restore it before the stage is removed.
			if _, lerr := os.Lstat(filepath.Join(w, transfer.WorkspaceIdentityEntry)); errors.Is(lerr, fs.ErrNotExist) {
				if err := moveIdentityBack(parent, base, complete[0]); err != nil {
					return &signedCommitError{Class: signedCommitRecover}
				}
			}
		}
	}
	_, err = removeStaleBootstrapStages(parent, base)
	return err
}

// signedBootstrapInPlace is case C: a per-agent directory, or a project
// path not checked at admission, which every dispatch applies. It downloads
// everything into a stage beside W first, so a failed download leaves W as
// it was, then applies the stage into W in place.
func (s *Server) signedBootstrapInPlace(ctx context.Context, req CreateAgentRequest, w string) error {
	parent, base := filepath.Dir(w), filepath.Base(w)
	if err := os.MkdirAll(w, 0o755); err != nil {
		return &signedCommitError{Class: signedCommitStage}
	}
	stage, err := newBootstrapStageName(base)
	if err != nil {
		return &signedCommitError{Class: signedCommitStage}
	}
	stagePath := filepath.Join(parent, stage)
	if err := os.Mkdir(stagePath, 0o755); err != nil {
		return &signedCommitError{Class: signedCommitStage}
	}
	written, err := s.signedWorkspaceDownload(ctx, req, stagePath)
	if err != nil {
		_ = os.RemoveAll(stagePath)
		return err
	}
	if err := applyStageInPlace(parent, stage, base, written); err != nil {
		if errors.Is(err, errSignedBootstrapCrash) {
			return err
		}
		_ = os.RemoveAll(stagePath)
		return err
	}
	if err := os.RemoveAll(stagePath); err != nil {
		return &signedCommitError{Class: signedCommitApply}
	}
	return nil
}

// applyStageInPlace moves the files of the stage directory stage into the
// workspace directory w, both entries of parent, and then removes every
// entry of w outside the listed set, except the root identity entry. A W
// entry whose type conflicts with the listed set (a file where a listed
// path needs a directory, or a directory where a file is listed) is removed
// first; the root identity entry is never touched. Each file is renamed into
// place atomically; W is never emptied as a whole. All operations go through
// an os.Root on parent.
func applyStageInPlace(parent, stage, w string, files []string) error {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return &signedCommitError{Class: signedCommitApply}
	}
	defer func() { _ = root.Close() }()

	listed := make(map[string]bool, len(files))
	dirs := map[string]bool{}
	for _, p := range files {
		if transfer.ExcludeWorkspaceObject(p) != transfer.WorkspaceObjectKept {
			return &signedCommitError{Class: signedCommitApply}
		}
		listed[p] = true
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)

	for _, p := range sorted {
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			if err := ensureApplyDir(root, w+"/"+strings.Join(parts[:i], "/")); err != nil {
				return err
			}
		}
		target := w + "/" + p
		if fi, err := root.Lstat(target); err == nil && fi.IsDir() {
			if err := root.RemoveAll(target); err != nil {
				return &signedCommitError{Class: signedCommitApply}
			}
		}
		if err := root.Rename(stage+"/"+p, target); err != nil {
			return &signedCommitError{Class: signedCommitApply}
		}
		if err := signedBootstrapHook("apply_file"); err != nil {
			return err
		}
	}

	var extras []string
	err = fs.WalkDir(root.FS(), w, func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == w {
			return nil
		}
		rel := strings.TrimPrefix(name, w+"/")
		if first, _, _ := strings.Cut(rel, "/"); transfer.IsWorkspaceIdentityName(first) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if !dirs[rel] {
				extras = append(extras, name)
				return fs.SkipDir
			}
			return nil
		}
		if !listed[rel] {
			extras = append(extras, name)
		}
		return nil
	})
	if err != nil {
		return &signedCommitError{Class: signedCommitApply}
	}
	for _, name := range extras {
		if err := root.RemoveAll(name); err != nil {
			return &signedCommitError{Class: signedCommitApply}
		}
	}
	return nil
}

// ensureApplyDir makes name (relative to root) a real directory: an entry
// of another type there (a file or a symlink) is removed first, then a
// missing directory is created.
func ensureApplyDir(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		if err := root.Remove(name); err != nil {
			return &signedCommitError{Class: signedCommitApply}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return &signedCommitError{Class: signedCommitApply}
	}
	if err := root.Mkdir(name, 0o777); err != nil {
		return &signedCommitError{Class: signedCommitApply}
	}
	return nil
}

// leafHasEntries reports whether the directory ws holds any entry at all,
// provisioning artifacts included. An unreadable directory counts as
// holding entries.
func leafHasEntries(ws string) bool {
	entries, err := os.ReadDir(ws)
	return err != nil || len(entries) > 0
}

// applySharedSignedBootstrap is case B, the signed bootstrap of a
// hub-managed project's shared workspace leaf (rel below the Layout's host
// base). Under the provisioning locks it first removes any stale stage of
// the leaf, then leaves an in-use leaf alone (skipped). A leaf holding only
// provisioning artifacts fails closed with leaf_busy, touching nothing.
// Otherwise it creates the stage with the leaf's owner, mode and default
// ACL, downloads into it, re-checks that the leaf is still empty, renames
// the stage over the empty leaf and writes the sentinel last.
func (s *Server) applySharedSignedBootstrap(ctx context.Context, req CreateAgentRequest, rel string) (skipped bool, err error) {
	base, err := filepath.EvalSymlinks(s.config.StorageLayout.HostBase)
	if err != nil {
		return false, fmt.Errorf("resolve host base: %w", err)
	}
	stateRel := filepath.Join(filepath.Dir(rel), provision.ProvisionStateDirName)
	owner := shareddirs.Owner{UID: s.config.StorageLayout.UID, GID: s.config.StorageLayout.GID}
	if err := ensureWorkspaceAndStateLeaves(base, rel, stateRel, owner); err != nil {
		return false, err
	}
	wsPath := filepath.Join(base, rel)
	parent, leaf := filepath.Dir(wsPath), filepath.Base(wsPath)
	err = provision.WithWorkspaceProvisionLocks(ctx, wsPath, func(lockCtx context.Context) error {
		// Every writer of a stage for this leaf holds these locks while the
		// stage exists, so any stage seen here is stale.
		if _, err := removeStaleBootstrapStages(parent, leaf); err != nil {
			return err
		}
		if sharedWorkspaceProvisioned(wsPath) {
			skipped = true
			return nil
		}
		if leafHasEntries(wsPath) {
			return &signedCommitError{Class: signedCommitLeafBusy}
		}
		stage, err := newBootstrapStageName(leaf)
		if err != nil {
			return &signedCommitError{Class: signedCommitStage}
		}
		stageRel := filepath.Join(filepath.Dir(rel), stage)
		fd, existed, err := shareddirs.EnsureLeafOwned(base, stageRel, owner)
		if err != nil {
			return &signedCommitError{Class: signedCommitStage}
		}
		if err := shareddirs.CloseFd(fd); err != nil || existed {
			return &signedCommitError{Class: signedCommitStage}
		}
		stagePath := filepath.Join(parent, stage)
		if _, err := s.signedWorkspaceDownload(lockCtx, req, stagePath); err != nil {
			_ = os.RemoveAll(stagePath)
			return err
		}
		if err := signedBootstrapHook("before_rename"); err != nil {
			return err
		}
		// Re-checked under the same locks: the rename only ever replaces a
		// truly empty leaf.
		if sharedWorkspaceProvisioned(wsPath) {
			_ = os.RemoveAll(stagePath)
			skipped = true
			return nil
		}
		if leafHasEntries(wsPath) {
			_ = os.RemoveAll(stagePath)
			return &signedCommitError{Class: signedCommitLeafBusy}
		}
		if err := lockCtx.Err(); err != nil {
			_ = os.RemoveAll(stagePath)
			return fmt.Errorf("provisioning lock lost: %w", err)
		}
		if err := renameSiblingDir(parent, stage, leaf); err != nil {
			_ = os.RemoveAll(stagePath)
			return &signedCommitError{Class: signedCommitApply}
		}
		if err := signedBootstrapHook("after_rename"); err != nil {
			return err
		}
		// The sentinel is irreversible: never write it under a lost lock.
		if err := lockCtx.Err(); err != nil {
			return fmt.Errorf("provisioning lock lost: %w", err)
		}
		return provision.MarkProvisioned(provision.ProjectStateDir(wsPath))
	})
	return skipped, err
}

// Fixed texts and ops of the signed bootstrap.
const (
	// opSelectWorkspaceDownload is the op logged for a refused download mode.
	opSelectWorkspaceDownload = "select workspace download mode"
	// opDownloadWorkspaceSignedExpired is the op of a signed download whose
	// URLs kept expiring or being refused.
	opDownloadWorkspaceSignedExpired = "download workspace (signed URLs expired or refused)"
	// opApplySharedWorkspaceBusy is the op of a case B bootstrap whose leaf
	// holds provisioning files and nothing else.
	opApplySharedWorkspaceBusy = "download workspace (the shared workspace holds provisioning files)"
)

// workspaceDownloadModeUnsupportedMessage is the client text for
// errWorkspaceDownloadModeUnsupported.
const workspaceDownloadModeUnsupportedMessage = "Workspace download mode not supported by this broker"

// errWorkspaceDownloadModeUnsupported marks a create whose
// workspaceDownloadMode this broker does not know. It is answered with 422
// before anything is created.
var errWorkspaceDownloadModeUnsupported = errors.New("workspace download mode not supported by this broker")

// selectWorkspaceDownloadMode reports whether req asks for the signed-URL
// download. An empty mode is the bucket download; any other value fails
// with errWorkspaceDownloadModeUnsupported.
func selectWorkspaceDownloadMode(req CreateAgentRequest) (signed bool, err error) {
	switch req.WorkspaceDownloadMode {
	case "":
		return false, nil
	case api.WorkspaceDownloadModeSignedURLs:
		return true, nil
	}
	return false, errWorkspaceDownloadModeUnsupported
}

// signedBootstrapOp is the op whose fixed text a bootstrap failure reports.
func signedBootstrapOp(err error) string {
	var dlErr *signedDownloadError
	var commitErr *signedCommitError
	switch {
	case errors.As(err, &dlErr) && dlErr.Class == signedClassExpired:
		return opDownloadWorkspaceSignedExpired
	case errors.As(err, &commitErr) && commitErr.Class == signedCommitLeafBusy:
		return opApplySharedWorkspaceBusy
	}
	return opDownloadWorkspace
}
