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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// This file is the one-shot promotion of the workspace-root provisioning
// marker, and the ONLY code that names the workspace-root marker and lock
// (the AC-L8 grep test allow-lists this file alone). Images from before the
// provisioning state directory wrote the sentinel into the workspace root
// (<workspace>/.scion-provisioned), added "/.scion-provisioned" to the
// workspace's git excludes, and took their lock there
// (<workspace>/.scion-provision.lock, with .stage-*, .evict-* and
// .clock-probe-* litter beside it). Current code reads, writes and locks
// only the state directory, so the hub runs this step once at start for
// every project tree on a shared backend. It can be removed in a later
// release, once no such workspace remains.

// legacyExcludeLine is the git exclude line older images added for the
// workspace-root marker.
const legacyExcludeLine = "/" + ProvisionSentinelFile

// LegacyPromotion reports what PromoteLegacyMarker did to one workspace.
type LegacyPromotion struct {
	// Promoted is set when the state-directory sentinel was written because
	// only the workspace-root marker said the workspace was provisioned.
	Promoted bool
	// MarkerRemoved is set when the workspace-root marker was removed.
	MarkerRemoved bool
	// ExcludeRemoved is set when its git exclude line was removed.
	ExcludeRemoved bool
	// LitterRemoved counts the aged workspace-root lock litter entries
	// removed. A live workspace-root lock is never removed.
	LitterRemoved int
}

// Changed reports whether the step changed anything.
func (p LegacyPromotion) Changed() bool {
	return p.Promoted || p.MarkerRemoved || p.ExcludeRemoved || p.LitterRemoved > 0
}

// PromoteLegacyMarker runs the one-shot promotion for the shared workspace
// at workspacePath (an absolute path through a mount of the whole export:
// <project root>/workspace, with the state directory ProjectStateDir beside
// it, which the caller has created with the leaf contract):
//
//   - when the workspace root holds the marker (a regular file) and the
//     state directory does not, the state-directory sentinel is written, so
//     the workspace stays provisioned (no clone, no recursive chown, and on
//     worktree-per-agent no base preparation under live worktrees);
//   - the workspace-root marker is removed, and its "/.scion-provisioned"
//     line is removed from the workspace's .git/info/exclude;
//   - workspace-root lock litter older than twice the stale threshold is
//     removed (evict entries aged from their eviction time, the others from
//     their own mtime, as garbageCollectLockLitter does). The live lock name
//     is never touched: a pod of an older image may still hold it.
//
// The changes run under the project's provisioning locks
// (WithWorkspaceProvisionLocks). A workspace with nothing to promote is
// detected first without taking a lock, so the step is idempotent and a
// second run changes nothing and takes no lock. A missing workspace is
// nothing to do.
func PromoteLegacyMarker(ctx context.Context, workspacePath string) (LegacyPromotion, error) {
	var out LegacyPromotion
	if workspacePath == "" || !filepath.IsAbs(workspacePath) || filepath.Clean(workspacePath) != workspacePath {
		return out, errors.New("provision: a clean absolute workspace path is required")
	}
	fi, err := os.Lstat(workspacePath)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("provision: workspace: %w", err)
	}
	if !fi.IsDir() {
		return out, fmt.Errorf("provision: workspace %s is not a directory", workspacePath)
	}
	if !LegacyPromotionNeeded(workspacePath) {
		return out, nil
	}
	stateDir := ProjectStateDir(workspacePath)
	err = WithWorkspaceProvisionLocks(ctx, workspacePath, func(lockCtx context.Context) error {
		marker := filepath.Join(workspacePath, ProvisionSentinelFile)
		if legacyMarkerPresent(workspacePath) {
			if !SentinelPresent(stateDir) {
				// The sentinel is irreversible: never write it under a
				// lost lock.
				if err := lockCtx.Err(); err != nil {
					return fmt.Errorf("provisioning lock lost: %w", err)
				}
				if err := MarkProvisioned(stateDir); err != nil {
					return err
				}
				out.Promoted = true
			}
		}
		if mfi, err := os.Lstat(marker); err == nil && !mfi.IsDir() {
			// os.Remove unlinks a symlink itself, never its target.
			if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove the workspace-root marker: %w", err)
			}
			out.MarkerRemoved = true
		}
		removed, err := removeLegacyExcludeLine(workspacePath)
		if err != nil {
			return err
		}
		out.ExcludeRemoved = removed
		// The server clock is probed in the state directory, so nothing
		// is written into the workspace. Best effort: what is left is
		// swept by a later run.
		_, out.LitterRemoved, _ = sweepLockLitterIn(workspacePath, stateDir, LockLitterMinAge())
		return nil
	})
	return out, err
}

// legacyMarkerPresent reports whether the workspace root holds the marker
// as a regular file (a symlink or anything else never promotes).
func legacyMarkerPresent(workspacePath string) bool {
	fi, err := os.Lstat(filepath.Join(workspacePath, ProvisionSentinelFile))
	return err == nil && fi.Mode().IsRegular()
}

// LegacyPromotionNeeded reports, without a lock and without writing
// anything, whether the workspace at workspacePath has anything for
// PromoteLegacyMarker to do: a marker entry, its exclude line, or lock
// litter entries other than the live lock. The workspace is agent-writable,
// so every read here is bounded (readLegacyExclude, lockLitterEntries).
func LegacyPromotionNeeded(workspacePath string) bool {
	if _, err := os.Lstat(filepath.Join(workspacePath, ProvisionSentinelFile)); err == nil {
		return true
	}
	if hasLegacyExcludeLine(workspacePath) {
		return true
	}
	litter, err := lockLitterEntries(workspacePath)
	return err == nil && len(litter) > 0
}

// legacyExcludeMaxBytes caps the workspace's .git/info/exclude that the
// promotion reads (gitExcludeMaxBytes, shared with appendGitExclude). The
// file is agent-writable; a larger one is skipped with a warning.
const legacyExcludeMaxBytes = gitExcludeMaxBytes

// errLegacyExcludeTooLarge marks an exclude file above legacyExcludeMaxBytes
// (errFileTooLarge, the shared bounded read's refusal).
var errLegacyExcludeTooLarge = errFileTooLarge

// legacyExcludePath is the workspace's .git/info/exclude when .git and info
// are plain directories and exclude is a regular file; "" otherwise. It is
// only a pre-check that keeps openGitExclude from creating the file in a
// workspace that has none; the read itself goes through readLegacyExclude.
func legacyExcludePath(workspacePath string) string {
	gitDir := filepath.Join(workspacePath, ".git")
	for _, d := range []string{gitDir, filepath.Join(gitDir, "info")} {
		fi, err := os.Lstat(d)
		if err != nil || !fi.IsDir() {
			return ""
		}
	}
	p := filepath.Join(gitDir, "info", "exclude")
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}

// readLegacyExclude is the one reader of the workspace's .git/info/exclude
// for the promotion: it opens the file through openGitExclude (each
// component with O_NOFOLLOW, O_NONBLOCK, and a regular-file check on the
// opened descriptor, so a symlink, a FIFO or a device is refused), refuses a
// file whose size is above legacyExcludeMaxBytes, and reads at most that
// many bytes through a limited reader (a file that grows past the cap after
// the size check is refused too). It returns the open file (the caller
// closes it) and its content, or nil, nil, nil when the workspace has no
// exclude file.
func readLegacyExclude(workspacePath string) (*os.File, []byte, error) {
	if legacyExcludePath(workspacePath) == "" {
		return nil, nil, nil
	}
	f, err := openGitExclude(workspacePath)
	if err != nil {
		return nil, nil, err
	}
	data, err := readBoundedFile(f, legacyExcludeMaxBytes)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, data, nil
}

// hasLegacyExcludeLine reports whether the workspace's git excludes hold
// the legacy marker's line. An exclude file that cannot be read safely
// (readLegacyExclude) is logged and treated as holding no such line.
func hasLegacyExcludeLine(workspacePath string) bool {
	f, data, err := readLegacyExclude(workspacePath)
	if err != nil {
		slog.Warn("provisioning state promotion: skipping the workspace's git exclude file", "path", workspacePath, "error", err)
		return false
	}
	if f == nil {
		return false
	}
	_ = f.Close()
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == legacyExcludeLine {
			return true
		}
	}
	return false
}

// removeLegacyExcludeLine removes every "/.scion-provisioned" line from the
// workspace's .git/info/exclude (read through readLegacyExclude), and keeps
// every other line as it was. A workspace without that file is left alone;
// one that cannot be read safely is logged and left alone.
func removeLegacyExcludeLine(workspacePath string) (bool, error) {
	f, data, err := readLegacyExclude(workspacePath)
	if err != nil {
		slog.Warn("provisioning state promotion: leaving the workspace's git exclude file as it is", "path", workspacePath, "error", err)
		return false, nil
	}
	if f == nil {
		return false, nil
	}
	defer func() { _ = f.Close() }()
	lines := strings.SplitAfter(string(data), "\n")
	kept := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if line != "" && strings.TrimSpace(line) == legacyExcludeLine {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, nil
	}
	// The file is opened with O_APPEND, so after the truncation the write
	// lands at offset 0.
	if err := f.Truncate(0); err != nil {
		return false, fmt.Errorf("truncate %s: %w", f.Name(), err)
	}
	if _, err := f.WriteString(strings.Join(kept, "")); err != nil {
		return false, fmt.Errorf("write %s: %w", f.Name(), err)
	}
	return true, nil
}
