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
// cleanly earlier is applied again.
func (s *Server) signedBootstrapLocal(ctx context.Context, req CreateAgentRequest, w string) (skipped bool, err error) {
	if req.ProjectSlug != "" && req.workspaceAdmissionChecked {
		return s.signedBootstrapAbsent(ctx, req, w)
	}
	return false, s.signedBootstrapInPlace(ctx, req, w)
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
		if e.Name() != transfer.WorkspaceIdentityEntry {
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
	if err := renameSiblingDir(parent, stage, base); err != nil {
		// Put the identity entry back before removing the stage, so w is
		// left as it was.
		if exists {
			_ = moveIdentityBack(parent, base, stage)
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
		entries, err := os.ReadDir(filepath.Join(parent, base))
		if errors.Is(err, fs.ErrNotExist) || (err == nil && len(entries) == 0) {
			if err := renameSiblingDir(parent, complete[0], base); err != nil {
				return &signedCommitError{Class: signedCommitRecover}
			}
		}
	}
	_, err = removeStaleBootstrapStages(parent, base)
	return err
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
