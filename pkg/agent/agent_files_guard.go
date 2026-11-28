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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// ErrAgentFilesDeleteRefused is returned when an agent files delete would
// remove a project workspace, or a path whose real location cannot be
// established. Nothing is removed at that path.
var ErrAgentFilesDeleteRefused = errors.New("refusing to delete agent files")

// agentFilesGuard is the single check every removal of an agent's files
// passes before anything is deleted: DeleteAgentFiles (the ordinary delete,
// the localOnly delete that cleans up a moved agent's previous broker, and
// the cleanup of a failed start) and the removal of the agent's files on
// the NFS workspace export.
//
// It classifies the real location of a path (all symlinks resolved, then
// cleaned), never its lexical form:
//   - a path that is a project workspace root, or contains one, is refused;
//   - a path inside one of the agent's own directories may be removed;
//   - any other path is refused, including one that resolves into a project
//     workspace outside the agent's own directories.
//
// The agent's own directories are trusted only where they really are: each
// is given lexically under an anchor (a directory the delete resolved
// independently of the agent, such as the project directory), and an
// agent directory whose real location is not the anchor's real location
// joined with the same relative path (a symlink somewhere below the anchor,
// for example an agents directory that points into the project) is not the
// agent's, so nothing resolving into it may be removed.
//
// It fails closed: a path, anchor or project workspace whose real location
// cannot be established is refused. A root that does not exist is skipped,
// since it holds nothing to protect and cannot contain an existing path.
// Containment compares path elements (filepath.Rel), so /proj never
// contains /project2.
type verifiedAgentRoot struct {
	anchorLex  string // lexical, cleaned absolute anchor
	anchorReal string // resolved real anchor
	rootLex    string // lexical, cleaned absolute agent root
	rootReal   string // resolved real agent root
}

// agentFilesRemoveAll removes rel within rootDir, confined to an os.Root
// opened on rootDir. Tests may replace it to inject removal errors.
var agentFilesRemoveAll = func(rootDir, rel string) error {
	return removeAllInRoot(rootDir, rel, true)
}

type agentFilesGuard struct {
	// lazy, when set, builds the guard on its first check (see
	// projectAgentFilesGuard), so a caller that removes nothing pays
	// nothing; resolved holds the result.
	lazy     func() *agentFilesGuard
	resolved *agentFilesGuard

	agentName    string
	anchors      []string            // lexical, cleaned absolute
	agentRoots   []verifiedAgentRoot // resolved and verified against an anchor
	projectRoots []string            // resolved
	// err is a failure to resolve a root; every check then refuses.
	err error
}

// newAgentFilesGuard prepares the check for agentName's files. anchors are
// directories the caller resolved independently of the agent (its project
// directory, the external agents root); agentRoots are the agent's own
// directories, each lexically under one of the anchors; projectRoots are
// the project workspace roots that must never be removed. Empty entries
// are ignored.
func newAgentFilesGuard(agentName string, anchors, agentRoots, projectRoots []string) *agentFilesGuard {
	g := &agentFilesGuard{agentName: agentName}
	for _, a := range anchors {
		if a != "" {
			if abs, err := filepath.Abs(a); err == nil {
				g.anchors = append(g.anchors, filepath.Clean(abs))
			}
		}
	}
	for _, root := range agentRoots {
		if root == "" || g.err != nil {
			continue
		}
		real, ok, err := g.verifiedAgentRoot(root)
		if err != nil {
			g.err = err
			continue
		}
		if ok {
			g.agentRoots = append(g.agentRoots, real)
		}
	}
	for _, root := range projectRoots {
		if root == "" || g.err != nil {
			continue
		}
		real, err := resolveRealPath(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			g.err = fmt.Errorf("cannot resolve the project workspace %s: %w", root, err)
			continue
		}
		g.projectRoots = append(g.projectRoots, real)
	}
	return g
}

// verifiedAgentRoot resolves root, an agent directory, and checks it is
// where its anchor (the longest anchor lexically containing it) says. It
// reports false, without an error, for a root that does not exist, has no
// anchor, or resolves elsewhere; such a root contains nothing that may be
// removed. An anchor that cannot be resolved is an error.
func (g *agentFilesGuard) verifiedAgentRoot(root string) (verifiedAgentRoot, bool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return verifiedAgentRoot{}, false, fmt.Errorf("cannot resolve the agent directory %s: %w", root, err)
	}
	abs = filepath.Clean(abs)
	anchor := ""
	for _, a := range g.anchors {
		if guardPathWithin(abs, a) && len(a) > len(anchor) {
			anchor = a
		}
	}
	if anchor == "" {
		slog.Warn("Agent files delete: agent directory has no anchor; treated as not the agent's", "agent", g.agentName, "path", abs)
		return verifiedAgentRoot{}, false, nil
	}
	anchorReal, err := resolveRealPath(anchor)
	if errors.Is(err, fs.ErrNotExist) {
		return verifiedAgentRoot{}, false, nil
	}
	if err != nil {
		return verifiedAgentRoot{}, false, fmt.Errorf("cannot resolve %s: %w", anchor, err)
	}
	real, err := resolveRealPath(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return verifiedAgentRoot{}, false, nil
	}
	if err != nil {
		return verifiedAgentRoot{}, false, fmt.Errorf("cannot resolve the agent directory %s: %w", abs, err)
	}
	rel, err := filepath.Rel(anchor, abs)
	if err != nil {
		return verifiedAgentRoot{}, false, nil
	}
	if want := filepath.Join(anchorReal, rel); real != want {
		slog.Warn("Agent files delete: agent directory resolves elsewhere; treated as not the agent's",
			"agent", g.agentName, "path", abs, "resolves_to", real, "expected", want)
		return verifiedAgentRoot{}, false, nil
	}
	return verifiedAgentRoot{
		anchorLex:  anchor,
		anchorReal: anchorReal,
		rootLex:    abs,
		rootReal:   real,
	}, true, nil
}

// check decides whether path, an existing path about to be removed, may be
// removed: it must lie inside one of the agent's own directories (or one of
// extraAgentRoots, directories this delete has already established are the
// agent's to remove, verified against the anchors like the agent's own).
// Otherwise it returns ErrAgentFilesDeleteRefused, and the refusal is
// logged.
//
// On success it returns the path to remove: the real location of path's
// parent directory joined with path's last element. The caller removes that
// path, not path itself, so a symlink in a parent directory that changes
// after the check cannot redirect the removal. The last element is kept as
// it is, so a symlink there is unlinked, never followed. (Callers that hand
// a path to git, such as git worktree remove, pass git's own path instead:
// git looks the worktree up by the path it registered.)
func (g *agentFilesGuard) check(path string, extraAgentRoots ...string) (string, error) {
	if g.lazy != nil {
		return g.built().check(path, extraAgentRoots...)
	}
	if err := g.classify(path, extraAgentRoots, false); err != nil {
		err = fmt.Errorf("%w %s for agent %s: %w", ErrAgentFilesDeleteRefused, path, g.agentName, err)
		slog.Warn("Agent files delete refused; agent deletion never removes a shared project workspace",
			"agent", g.agentName, "path", path, "error", err)
		return "", err
	}
	parent, err := resolveRealPath(filepath.Dir(path))
	if err != nil {
		err = fmt.Errorf("%w %s for agent %s: cannot resolve its directory: %w", ErrAgentFilesDeleteRefused, path, g.agentName, err)
		slog.Warn("Agent files delete refused; agent deletion never removes a shared project workspace",
			"agent", g.agentName, "path", path, "error", err)
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

// checkLeaf is check for a directory entry that is removed as it is: it is
// classified by its parent's real location joined with its name, without
// resolving the entry itself, so a symlink there (dangling or not) is
// unlinked, never followed, and never makes the delete fail.
func (g *agentFilesGuard) checkLeaf(path string) (string, error) {
	if g.lazy != nil {
		return g.built().checkLeaf(path)
	}
	if err := g.classify(path, nil, true); err != nil {
		err = fmt.Errorf("%w %s for agent %s: %w", ErrAgentFilesDeleteRefused, path, g.agentName, err)
		slog.Warn("Agent files delete refused; agent deletion never removes a shared project workspace",
			"agent", g.agentName, "path", path, "error", err)
		return "", err
	}
	parent, err := resolveRealPath(filepath.Dir(path))
	if err != nil {
		err = fmt.Errorf("%w %s for agent %s: cannot resolve its directory: %w", ErrAgentFilesDeleteRefused, path, g.agentName, err)
		slog.Warn("Agent files delete refused; agent deletion never removes a shared project workspace",
			"agent", g.agentName, "path", path, "error", err)
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

// built returns the guard a lazy guard stands for, building it once.
func (g *agentFilesGuard) built() *agentFilesGuard {
	if g.resolved == nil {
		g.resolved = g.lazy()
	}
	return g.resolved
}

// agentProjectLayout is where an agent's project keeps the directories
// DeleteAgentFiles and the guard work with.
type agentProjectLayout struct {
	// projectRoot is the project directory without a trailing .scion.
	projectRoot string
	// sharedBase is the worktree-per-agent shared base,
	// <projectRoot>/workspace; sharedBaseIsRepo reports whether it holds a
	// repository (.git as a directory or a file).
	sharedBase       string
	sharedBaseIsRepo bool
	// repoRoot is the repository the project's worktrees belong to: the
	// shared base when it is one, else the repository holding projectDir
	// ("" when there is none).
	repoRoot string
	// externalAgentsRoot is the external agents root of a git project with
	// split storage ("" when there is none).
	externalAgentsRoot string
}

// resolveAgentProjectLayout resolves agentProjectLayout for projectDir.
func resolveAgentProjectLayout(projectDir string) agentProjectLayout {
	var l agentProjectLayout
	// GetResolvedProjectDir may have appended .scion (e.g. hub-managed
	// projects), so strip that suffix to match the path the workspace
	// backend used during provisioning.
	l.projectRoot = projectDir
	if filepath.Base(projectDir) == config.DotScion {
		l.projectRoot = filepath.Dir(projectDir)
	}
	l.sharedBase = filepath.Join(l.projectRoot, "workspace")
	// Accept .git as either a directory (normal clone) or a file (gitdir
	// pointer, e.g. if the base is itself a linked worktree/submodule) —
	// existence is enough to identify a valid repo root. (upstream #351 review)
	if _, err := os.Stat(filepath.Join(l.sharedBase, ".git")); err == nil {
		l.sharedBaseIsRepo = true
		l.repoRoot = l.sharedBase
	} else if root, err := util.RepoRootDir(projectDir); err == nil {
		// Passing projectDir (not its parent) is robust for both local
		// projects (where projectDir is the repo root) and hub-managed
		// projects (where it is the .scion subdir). MUST match the base
		// used at sharer registration (ProvisionAgent).
		l.repoRoot = root
	}
	if extDir, err := config.GetGitProjectExternalAgentsDir(projectDir); err == nil && extDir != "" {
		l.externalAgentsRoot = extDir
	}
	return l
}

// projectAgentFilesGuard prepares the check for agentName's files in the
// project at projectDir:
//   - anchors: projectDir, the project root, the repository the project is
//     in and the external agents root;
//   - the agent's own directories: <projectDir>/agents/<name>, the external
//     agent directory, the global project's per-agent workspace and the
//     worktree-per-agent worktree, plus extraRoots (agent directories the
//     caller resolved through provision's own resolver, such as the agent
//     state directory and home); an extra root outside every anchor is
//     anchored at its parent directory;
//   - the project workspaces that are never removed: the project root as
//     provision resolves it (resolveProjectRoot, with the project's
//     effective settings), a plain project's mounted parent directory
//     (plainProjectWorkspaceSource) and the repository.
//
// It fails closed: when the project directory is unknown or its settings
// cannot be loaded, every check refuses.
//
// The guard is built on its first check, so a caller that removes nothing
// (most creates and starts) neither pays for it nor can fail on it.
func projectAgentFilesGuard(ctx context.Context, projectDir, agentName string, extraRoots ...string) *agentFilesGuard {
	return &agentFilesGuard{agentName: agentName, lazy: func() *agentFilesGuard {
		if projectDir == "" {
			return &agentFilesGuard{agentName: agentName, err: errors.New("the agent's project directory is unknown")}
		}
		return buildProjectAgentFilesGuard(ctx, projectDir, resolveAgentProjectLayout(projectDir), agentName, extraRoots...)
	}}
}

// projectAgentFilesGuardWithLayout is projectAgentFilesGuard for a caller
// that already resolved the project's layout l.
func projectAgentFilesGuardWithLayout(ctx context.Context, projectDir string, l agentProjectLayout, agentName string, extraRoots ...string) *agentFilesGuard {
	return &agentFilesGuard{agentName: agentName, lazy: func() *agentFilesGuard {
		if projectDir == "" {
			return &agentFilesGuard{agentName: agentName, err: errors.New("the agent's project directory is unknown")}
		}
		return buildProjectAgentFilesGuard(ctx, projectDir, l, agentName, extraRoots...)
	}}
}

// buildProjectAgentFilesGuard builds the guard projectAgentFilesGuard
// describes, for the project at projectDir with layout l.
func buildProjectAgentFilesGuard(ctx context.Context, projectDir string, l agentProjectLayout, agentName string, extraRoots ...string) *agentFilesGuard {
	anchors := []string{projectDir, l.projectRoot, l.repoRoot, l.externalAgentsRoot}
	roots := []string{filepath.Join(projectDir, "agents", agentName)}
	if l.externalAgentsRoot != "" {
		roots = append(roots, filepath.Join(l.externalAgentsRoot, agentName))
	}
	if config.IsGlobalProjectDir(projectDir) {
		roots = append(roots, filepath.Join(projectDir, "workspace", agentName))
	}
	if l.sharedBaseIsRepo {
		roots = append(roots, filepath.Join(l.sharedBase, "worktrees", agentName))
	}
	strong := append([]string(nil), anchors...)
	for _, r := range extraRoots {
		if r == "" {
			continue
		}
		roots = append(roots, r)
		anchored := false
		for _, a := range strong {
			if a != "" && guardPathWithin(filepath.Clean(r), filepath.Clean(a)) {
				anchored = true
				break
			}
		}
		if !anchored {
			anchors = append(anchors, filepath.Dir(r))
		}
	}
	settings, _, settingsErr := config.LoadEffectiveSettingsFor(ctx, projectDir)
	projectRoots := []string{l.repoRoot}
	if settingsErr == nil {
		projectRoots = append(projectRoots, resolveProjectRoot(settings, projectDir))
	}
	if !config.IsGlobalProjectDir(projectDir) {
		projectRoots = append(projectRoots, plainProjectWorkspaceSource(projectDir))
	}
	g := newAgentFilesGuard(agentName, anchors, roots, projectRoots)
	if settingsErr != nil && g.err == nil {
		g.err = fmt.Errorf("cannot load the project settings to find its workspace: %w", settingsErr)
	}
	return g
}

// CheckAgentWorktreeRemoval applies the agent files guard to the removal of
// agentName's worktree at worktreePath, under the project checkout base:
// the worktree must really be where it says inside base, and base itself is
// never removed. It returns the path to remove (see agentFilesGuard.check).
func CheckAgentWorktreeRemoval(agentName, base, worktreePath string) (string, error) {
	return newAgentFilesGuard(agentName, []string{base}, []string{worktreePath}, []string{base}).check(worktreePath)
}

// RemoveAgentWorktreeDir applies the agent files guard to worktreePath under
// base and removes it confined to base via os.Root, never following an
// intermediate symbolic link. Unlike removeAll, it does not chmod read-only
// directories before removal, preserving os.RemoveAll permission semantics.
func RemoveAgentWorktreeDir(agentName, base, worktreePath string) error {
	return newAgentFilesGuard(agentName, []string{base}, []string{worktreePath}, []string{base}).removeAllMode(worktreePath, nil, false, false)
}

// removeAll checks path with the guard and removes it confined to the verified
// agent files root via os.Root. Every intermediate component between the root
// and path must be a real directory; a symbolic link at any intermediate
// component is refused (wrapping ErrAgentFilesDeleteRefused) rather than
// followed or repaired.
func (g *agentFilesGuard) removeAll(path string, extraAgentRoots ...string) error {
	return g.removeAllMode(path, extraAgentRoots, false, true)
}

// removeAllLeaf is removeAll for a directory entry removed as it is (see
// checkLeaf): intermediate components are still verified as real directories
// inside os.Root, while a symbolic link at the leaf itself is unlinked without
// being followed.
func (g *agentFilesGuard) removeAllLeaf(path string) error {
	return g.removeAllMode(path, nil, true, true)
}

func (g *agentFilesGuard) removeAllMode(path string, extraAgentRoots []string, leaf, makeWritable bool) error {
	if g.lazy != nil {
		return g.built().removeAllMode(path, extraAgentRoots, leaf, makeWritable)
	}
	if err := g.classify(path, extraAgentRoots, leaf); err != nil {
		err = fmt.Errorf("%w %s for agent %s: %w", ErrAgentFilesDeleteRefused, path, g.agentName, err)
		slog.Warn("Agent files delete refused; agent deletion never removes a shared project workspace",
			"agent", g.agentName, "path", path, "error", err)
		return err
	}
	rootDir, rel, err := g.confinedTarget(path, extraAgentRoots)
	if err != nil {
		err = fmt.Errorf("%w %s for agent %s: %w", ErrAgentFilesDeleteRefused, path, g.agentName, err)
		slog.Warn("Agent files delete refused; agent deletion never removes a shared project workspace",
			"agent", g.agentName, "path", path, "error", err)
		return err
	}
	var rmErr error
	if makeWritable {
		rmErr = agentFilesRemoveAll(rootDir, rel)
	} else {
		rmErr = removeAllInRoot(rootDir, rel, false)
	}
	if rmErr != nil {
		if errors.Is(rmErr, ErrAgentFilesDeleteRefused) {
			slog.Warn("Agent files delete refused; traversal confined to agent files root",
				"agent", g.agentName, "path", path, "error", rmErr)
		}
		return rmErr
	}
	return nil
}

// confinedTarget maps path to a verified root directory and a clean relative
// path beneath it so removal can open rootDir with os.OpenRoot and walk every
// component within that root without OS-level symlink traversal.
func (g *agentFilesGuard) confinedTarget(path string, extraAgentRoots []string) (string, string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("the path is not absolute")
	}
	if hasDotDotSegment(path) {
		return "", "", fmt.Errorf("the path contains a parent-directory traversal component")
	}
	abs := filepath.Clean(path)

	roots := g.agentRoots
	for _, root := range extraAgentRoots {
		if root == "" {
			continue
		}
		r, ok, err := g.verifiedAgentRoot(root)
		if err != nil {
			return "", "", err
		}
		if ok {
			roots = append(roots[:len(roots):len(roots)], r)
		}
	}

	// Prefer a lexical match against a verified agent root so intermediate
	// symlinks inside the root are preserved in rel and caught by the os.Root
	// walk rather than silently collapsed before opening the root.
	var matched *verifiedAgentRoot
	bestLen := -1
	useReal := false
	for i := range roots {
		vr := &roots[i]
		if guardPathWithin(abs, vr.rootLex) && len(vr.rootLex) > bestLen {
			matched = vr
			bestLen = len(vr.rootLex)
		}
	}
	if matched == nil {
		for i := range roots {
			vr := &roots[i]
			if guardPathWithin(abs, vr.rootReal) && len(vr.rootReal) > bestLen {
				matched = vr
				bestLen = len(vr.rootReal)
				useReal = true
			}
		}
	}
	if matched == nil {
		return "", "", fmt.Errorf("cannot confine %s to a verified agent files root", abs)
	}

	var rootDir, base string
	if !useReal {
		if matched.anchorLex != "" && guardPathWithin(abs, matched.anchorLex) && abs != matched.anchorLex {
			rootDir = matched.anchorReal
			base = matched.anchorLex
		} else if abs != matched.rootLex {
			rootDir = matched.rootReal
			base = matched.rootLex
		} else {
			parent := filepath.Dir(matched.rootReal)
			leaf := filepath.Base(matched.rootReal)
			if parent == matched.rootReal || leaf == "" || leaf == "." || leaf == ".." {
				return "", "", fmt.Errorf("cannot confine root directory %s", abs)
			}
			return parent, leaf, nil
		}
	} else {
		if matched.anchorReal != "" && guardPathWithin(abs, matched.anchorReal) && abs != matched.anchorReal {
			rootDir = matched.anchorReal
			base = matched.anchorReal
		} else if abs != matched.rootReal {
			rootDir = matched.rootReal
			base = matched.rootReal
		} else {
			parent := filepath.Dir(matched.rootReal)
			leaf := filepath.Base(matched.rootReal)
			if parent == matched.rootReal || leaf == "" || leaf == "." || leaf == ".." {
				return "", "", fmt.Errorf("cannot confine root directory %s", abs)
			}
			return parent, leaf, nil
		}
	}

	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "" || rel == "." || filepath.IsAbs(rel) || hasDotDotSegment(rel) {
		return "", "", fmt.Errorf("cannot compute confined relative path for %s", abs)
	}
	return rootDir, rel, nil
}

// removeAllInRoot removes rel inside rootDir using os.Root so every path
// component is resolved within the root rather than through the OS. Every
// intermediate component must be a real directory; a symbolic link at an
// intermediate component is refused (ErrAgentFilesDeleteRefused) without
// following or removing it. A symbolic link at the leaf itself is unlinked
// without being followed.
func removeAllInRoot(rootDir, rel string, makeWritable bool) error {
	cleanRel := filepath.Clean(rel)
	if cleanRel == "" || cleanRel == "." || filepath.IsAbs(cleanRel) || hasDotDotSegment(cleanRel) {
		return fmt.Errorf("%w: invalid relative path %q", ErrAgentFilesDeleteRefused, rel)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return fmt.Errorf("%w: open root %s: %w", ErrAgentFilesDeleteRefused, rootDir, err)
	}
	cur := root
	defer func() {
		if cur != nil {
			_ = cur.Close()
		}
	}()

	parts := strings.Split(cleanRel, string(filepath.Separator))
	for i, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: invalid path component %q in %q", ErrAgentFilesDeleteRefused, part, rel)
		}
		fi, err := cur.Lstat(part)
		if err != nil {
			return fmt.Errorf("%w: intermediate path %s: %w", ErrAgentFilesDeleteRefused, filepath.Join(parts[:i+1]...), err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: intermediate path %s is a symbolic link", ErrAgentFilesDeleteRefused, filepath.Join(parts[:i+1]...))
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: intermediate path %s is not a directory", ErrAgentFilesDeleteRefused, filepath.Join(parts[:i+1]...))
		}
		next, err := cur.OpenRoot(part)
		if err != nil {
			return fmt.Errorf("%w: open intermediate directory %s: %w", ErrAgentFilesDeleteRefused, filepath.Join(parts[:i+1]...), err)
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(fi, opened) {
			_ = next.Close()
			return fmt.Errorf("%w: intermediate directory %s changed while opening", ErrAgentFilesDeleteRefused, filepath.Join(parts[:i+1]...))
		}
		_ = cur.Close()
		cur = next
	}

	leaf := parts[len(parts)-1]
	if leaf == "" || leaf == "." || leaf == ".." {
		return fmt.Errorf("%w: invalid target component %q", ErrAgentFilesDeleteRefused, leaf)
	}
	fi, err := cur.Lstat(leaf)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if makeWritable && fi.IsDir() {
		makeWritableInRoot(cur, leaf, fi)
	}
	return cur.RemoveAll(leaf)
}

// makeWritableInRoot walks name inside parent via os.Root, making real
// directories and regular files writable so RemoveAll can delete read-only
// entries (such as git pack/object files). Symbolic links are skipped and
// never followed. Errors are ignored because the subsequent RemoveAll surfaces
// any remaining permission failure.
func makeWritableInRoot(parent *os.Root, name string, expected fs.FileInfo) {
	if expected.Mode().Perm()&0o700 != 0o700 {
		_ = parent.Chmod(name, 0o700) //winsec:not-protection makes the entry writable or removable before deletion
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return
	}
	defer func() { _ = child.Close() }()
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(expected, opened) {
		return
	}
	f, err := rootopen.OpenDir(child, ".")
	if err != nil {
		return
	}
	entries, readErr := f.ReadDir(-1)
	_ = f.Close()
	if readErr != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		cfi, err := child.Lstat(n)
		if err != nil {
			continue
		}
		if cfi.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if cfi.IsDir() {
			makeWritableInRoot(child, n, cfi)
		} else if cfi.Mode().Perm()&0o200 == 0 {
			_ = child.Chmod(n, cfi.Mode().Perm()|0o200) //winsec:not-protection makes the entry writable or removable before deletion
		}
	}
}

func hasDotDotSegment(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}

// classify decides path's place: leaf false classifies its real location
// (every symlink resolved), leaf true its parent's real location joined
// with its name (the entry itself is not resolved).
func (g *agentFilesGuard) classify(path string, extraAgentRoots []string, leaf bool) error {
	if g.err != nil {
		return g.err
	}
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("the path is not absolute")
	}
	var real string
	var err error
	if leaf {
		var parent string
		if parent, err = resolveRealPath(filepath.Dir(path)); err == nil {
			real = filepath.Join(parent, filepath.Base(path))
		}
	} else {
		real, err = resolveRealPath(path)
	}
	if err != nil {
		return fmt.Errorf("cannot resolve its real location: %w", err)
	}
	for _, root := range g.projectRoots {
		if guardPathWithin(root, real) {
			return fmt.Errorf("it resolves to %s, which is or contains the project workspace %s", real, root)
		}
	}
	agentRoots := g.agentRoots
	for _, root := range extraAgentRoots {
		if root == "" {
			continue
		}
		r, ok, err := g.verifiedAgentRoot(root)
		if err != nil {
			return err
		}
		if ok {
			agentRoots = append(agentRoots[:len(agentRoots):len(agentRoots)], r)
		}
	}
	for _, root := range agentRoots {
		if guardPathWithin(real, root.rootReal) {
			return nil
		}
	}
	for _, root := range g.projectRoots {
		if guardPathWithin(real, root) {
			return fmt.Errorf("it resolves to %s, inside the project workspace %s and outside the agent's own directories", real, root)
		}
	}
	return fmt.Errorf("it resolves to %s, outside the agent's own directories", real)
}

// resolveRealPath returns path's real location: absolute, with every
// symlink resolved, cleaned.
func resolveRealPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

// guardPathWithin reports whether path is dir or lies under it, comparing whole
// path elements. Both must be clean absolute paths.
func guardPathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
