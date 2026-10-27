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

package maintenance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// RetainedReleases is how many release directories the symlink installer
// keeps, in addition to whatever current and previous point to.
const RetainedReleases = 5

const (
	releasesDirName = "releases"
	currentLinkName = "current"
	prevLinkName    = "previous"
	partialSuffix   = ".partial"
	binaryName      = "scion"
)

// releaseDirRE limits release directory names to one safe path element:
// release tags (nightly-YYYYMMDD, v0.3.0-preview.4) and, later, git-<sha12>.
var releaseDirRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// symlinkInstaller installs releases into <root>/releases/<tag>/scion and
// switches the <root>/current symlink, which the systemd unit's ExecStart
// goes through. <root> is owned by the hub user, so no privilege is needed.
// Restart is a SIGTERM to this process; systemd restarts it.
type symlinkInstaller struct {
	root       string
	executable func() (string, error)
	signal     func() error
	uid        func() int
	keep       int

	mu   sync.Mutex
	last *activation // undo record of the last Activate
}

func newSymlinkInstaller(opts DetectOptions) *symlinkInstaller {
	return &symlinkInstaller{
		root:       opts.InstallRoot,
		executable: opts.Executable,
		signal:     opts.Signal,
		uid:        opts.UID,
		keep:       RetainedReleases,
	}
}

func (s *symlinkInstaller) releasesDir() string { return filepath.Join(s.root, releasesDirName) }
func (s *symlinkInstaller) linkPath(name string) string {
	return filepath.Join(s.root, name)
}

// Name implements Installer.
func (s *symlinkInstaller) Name() string { return StrategySymlink }

// Status implements Installer.
func (s *symlinkInstaller) Status() InstallerStatus {
	return InstallerStatus{Strategy: StrategySymlink, Supported: true}
}

// Preflight implements Installer: the running binary resolves under
// <root>/releases/, <root>/current (and <root>/previous, when present) is a
// symlink owned by the hub user that resolves to a release directory
// (checkReleaseLink), and the root and releases directories are writable.
func (s *symlinkInstaller) Preflight(context.Context) error {
	if !filepath.IsAbs(s.root) {
		return fmt.Errorf("install root %q is not an absolute path", s.root)
	}
	root, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return fmt.Errorf("install root %s not found", s.root)
	}
	releases := filepath.Join(root, releasesDirName)

	exe, err := s.executable()
	if err != nil {
		return fmt.Errorf("cannot determine the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	rel, err := filepath.Rel(releases, exe)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || !strings.Contains(rel, string(filepath.Separator)) {
		return fmt.Errorf("running binary %s is not under %s", exe, filepath.Join(s.root, releasesDirName))
	}

	if _, err := s.checkReleaseLink(currentLinkName); err != nil {
		return err
	}
	// previous is optional, but when present it must be as trustworthy as
	// current: Activate records it and Deactivate restores it.
	if s.linkExists(prevLinkName) {
		if _, err := s.checkReleaseLink(prevLinkName); err != nil {
			return fmt.Errorf("%w; repoint %s at a release directory under %s (as the hub user: ln -sfn releases/<tag> %s, or chown -h the link to the hub user afterwards) or remove it",
				err, s.linkPath(prevLinkName), s.releasesDir(), s.linkPath(prevLinkName))
		}
	}

	for _, dir := range []string{s.root, s.releasesDir()} {
		if !isWritableDir(dir) {
			return fmt.Errorf("%s is not writable by the hub user", dir)
		}
	}
	return nil
}

// Stage implements Installer: it copies bin to releases/<tag>.partial/scion
// and renames the directory to releases/<tag>. It refuses to replace the
// release current points to.
func (s *symlinkInstaller) Stage(_ context.Context, tag, bin string) (string, error) {
	if !releaseDirRE.MatchString(tag) || strings.HasSuffix(tag, partialSuffix) {
		return "", fmt.Errorf("invalid release name %q", tag)
	}
	final := filepath.Join(s.releasesDir(), tag)
	partial := final + partialSuffix

	if tag == s.linkTarget(currentLinkName) {
		return "", fmt.Errorf("release %s is the active release", tag)
	}

	if err := os.RemoveAll(partial); err != nil {
		return "", fmt.Errorf("remove stale %s: %w", partial, err)
	}
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", partial, err)
	}
	if err := copyExecutable(bin, filepath.Join(partial, binaryName)); err != nil {
		_ = os.RemoveAll(partial)
		return "", err
	}
	// A retained copy of the same tag (for example the one previous points
	// to) is replaced by the freshly verified one.
	if err := os.RemoveAll(final); err != nil {
		_ = os.RemoveAll(partial)
		return "", fmt.Errorf("remove old %s: %w", final, err)
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.RemoveAll(partial)
		return "", fmt.Errorf("rename %s into place: %w", partial, err)
	}
	return final, nil
}

// Activate implements Installer: previous is pointed at the old current
// target, then current is swapped to the staged release with
// symlink(current.new) + rename, which is atomic. If the current swap fails
// previous is restored. Old releases are then pruned (best effort), keeping
// what Deactivate would restore.
func (s *symlinkInstaller) Activate(_ context.Context, staged string) error {
	if filepath.Clean(filepath.Dir(staged)) != filepath.Clean(s.releasesDir()) {
		return fmt.Errorf("staged release %s is not in %s", staged, s.releasesDir())
	}
	tag := filepath.Base(staged)
	if fi, err := os.Stat(filepath.Join(staged, binaryName)); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("staged release %s has no %s binary", staged, binaryName)
	}
	target := filepath.Join(releasesDirName, tag)

	// Both links are re-validated here (Preflight ran before the download),
	// so the undo record only ever holds targets inside releases/.
	oldCurrent, err := s.checkReleaseLink(currentLinkName)
	if err != nil {
		return fmt.Errorf("refusing to activate: %w", err)
	}
	undo := &activation{oldCurrent: oldCurrent}
	if s.linkExists(prevLinkName) {
		oldPrevious, err := s.checkReleaseLink(prevLinkName)
		if err != nil {
			return fmt.Errorf("refusing to activate: %w", err)
		}
		undo.oldPrevious, undo.hadPrevious = oldPrevious, true
	}

	swappedPrevious := false
	if filepath.Base(oldCurrent) != tag {
		if err := s.swapLink(prevLinkName, oldCurrent); err != nil {
			return fmt.Errorf("update %s: %w", prevLinkName, err)
		}
		swappedPrevious = true
	}
	if err := s.swapLink(currentLinkName, target); err != nil {
		if swappedPrevious {
			_ = s.restorePrevious(undo)
		}
		return fmt.Errorf("update %s: %w", currentLinkName, err)
	}

	s.mu.Lock()
	s.last = undo
	s.mu.Unlock()

	_ = s.prune(filepath.Base(undo.oldPrevious))
	return nil
}

// activation records the links Activate replaced, for Deactivate.
type activation struct {
	oldCurrent  string
	oldPrevious string
	hadPrevious bool
}

// Deactivate implements Deactivator: it points current and previous back
// at what they were before the last Activate, so a failed update leaves the
// hub on its old release with nothing changed.
func (s *symlinkInstaller) Deactivate(context.Context) error {
	s.mu.Lock()
	undo := s.last
	s.last = nil
	s.mu.Unlock()
	if undo == nil {
		return errors.New("no activation to undo")
	}
	// Validate the links as they are now and the targets to restore, so a
	// rollback never writes a link that points outside releases/.
	for _, name := range []string{currentLinkName, prevLinkName} {
		if name == prevLinkName && !s.linkExists(name) {
			continue
		}
		if _, err := s.checkReleaseLink(name); err != nil {
			return fmt.Errorf("refusing to roll back: %w", err)
		}
	}
	if err := s.checkReleaseTarget(currentLinkName, undo.oldCurrent); err != nil {
		return fmt.Errorf("refusing to roll back: previous %s target: %w", currentLinkName, err)
	}
	if err := s.swapLink(currentLinkName, undo.oldCurrent); err != nil {
		return fmt.Errorf("restore %s: %w", currentLinkName, err)
	}
	return s.restorePrevious(undo)
}

// restorePrevious points previous back at its pre-activation target, or
// removes it if there was none (or that target is no longer a release
// directory: a link outside releases/ is never written back).
func (s *symlinkInstaller) restorePrevious(undo *activation) error {
	if undo.hadPrevious && s.checkReleaseTarget(prevLinkName, undo.oldPrevious) == nil {
		return s.swapLink(prevLinkName, undo.oldPrevious)
	}
	if err := os.Remove(s.linkPath(prevLinkName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", prevLinkName, err)
	}
	return nil
}

// Restart implements Installer: SIGTERM to this process.
func (s *symlinkInstaller) Restart(context.Context) error {
	return s.signal()
}

// Retained implements Installer: the release directories, newest first.
func (s *symlinkInstaller) Retained() []InstalledRelease {
	rels, _ := s.listReleases()
	return rels
}

// linkExists reports whether <root>/<name> exists (as anything).
func (s *symlinkInstaller) linkExists(name string) bool {
	_, err := os.Lstat(s.linkPath(name))
	return err == nil
}

// checkReleaseLink validates <root>/<name>: a symlink, owned by the hub
// user, whose target resolves to a directory directly under
// <root>/releases/. It returns the target as stored in the link.
func (s *symlinkInstaller) checkReleaseLink(name string) (string, error) {
	link := s.linkPath(name)
	fi, err := os.Lstat(link)
	if err != nil {
		return "", fmt.Errorf("%s does not exist", link)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s is not a symlink", link)
	}
	if owner, ok := fileOwnerUID(fi); !ok || owner != s.uid() {
		return "", fmt.Errorf("%s is not owned by the hub user (uid %d)", link, s.uid())
	}
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", link, err)
	}
	if err := s.checkReleaseTarget(name, target); err != nil {
		return "", err
	}
	return target, nil
}

// checkReleaseTarget checks that a link target (relative to the root, or
// absolute) resolves to a directory directly under <root>/releases/.
func (s *symlinkInstaller) checkReleaseTarget(name, target string) error {
	if target == "" {
		return fmt.Errorf("%s has no target", name)
	}
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.root, abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("%s target %q does not resolve: %v", name, target, err)
	}
	releases, err := filepath.EvalSymlinks(s.releasesDir())
	if err != nil {
		return fmt.Errorf("%s not found", s.releasesDir())
	}
	rel, err := filepath.Rel(releases, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		strings.Contains(rel, string(filepath.Separator)) {
		return fmt.Errorf("%s target %q resolves outside %s", name, target, s.releasesDir())
	}
	if fi, err := os.Stat(resolved); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s target %q is not a release directory", name, target)
	}
	return nil
}

// swapLink atomically points <root>/<name> at target.
func (s *symlinkInstaller) swapLink(name, target string) error {
	link := s.linkPath(name)
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// linkTarget returns the release directory name the given link points to,
// or "" when the link does not exist.
func (s *symlinkInstaller) linkTarget(name string) string {
	t, err := os.Readlink(s.linkPath(name))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

func (s *symlinkInstaller) listReleases() ([]InstalledRelease, error) {
	entries, err := os.ReadDir(s.releasesDir())
	if err != nil {
		return nil, err
	}
	current, previous := s.linkTarget(currentLinkName), s.linkTarget(prevLinkName)
	var rels []InstalledRelease
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), partialSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rels = append(rels, InstalledRelease{
			Tag:         e.Name(),
			Path:        filepath.Join(s.releasesDir(), e.Name()),
			InstalledAt: info.ModTime(),
			Current:     e.Name() == current,
			Previous:    e.Name() == previous,
		})
	}
	sort.SliceStable(rels, func(i, j int) bool {
		return rels[i].InstalledAt.After(rels[j].InstalledAt)
	})
	return rels, nil
}

// prune removes release directories beyond the newest s.keep, never the ones
// current or previous point to, nor the extra names given.
func (s *symlinkInstaller) prune(extra ...string) error {
	rels, err := s.listReleases()
	if err != nil {
		return err
	}
	var errs []error
	for i, r := range rels {
		if i < s.keep || r.Current || r.Previous || slices.Contains(extra, r.Tag) {
			continue
		}
		if err := os.RemoveAll(r.Path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// copyExecutable copies src to dst (mode 0755) and syncs it to disk.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy to %s: %w", dst, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return os.Chmod(dst, 0o755)
}
