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
type agentFilesGuard struct {
	agentName    string
	anchors      []string // lexical, cleaned absolute
	agentRoots   []string // resolved and verified against an anchor
	projectRoots []string // resolved
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
func (g *agentFilesGuard) verifiedAgentRoot(root string) (string, bool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", false, fmt.Errorf("cannot resolve the agent directory %s: %w", root, err)
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
		return "", false, nil
	}
	anchorReal, err := resolveRealPath(anchor)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cannot resolve %s: %w", anchor, err)
	}
	real, err := resolveRealPath(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cannot resolve the agent directory %s: %w", abs, err)
	}
	rel, err := filepath.Rel(anchor, abs)
	if err != nil {
		return "", false, nil
	}
	if want := filepath.Join(anchorReal, rel); real != want {
		slog.Warn("Agent files delete: agent directory resolves elsewhere; treated as not the agent's",
			"agent", g.agentName, "path", abs, "resolves_to", real, "expected", want)
		return "", false, nil
	}
	return real, true, nil
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
	if err := g.classify(path, extraAgentRoots); err != nil {
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
func projectAgentFilesGuard(ctx context.Context, projectDir, agentName string, extraRoots ...string) *agentFilesGuard {
	if projectDir == "" {
		return &agentFilesGuard{agentName: agentName, err: errors.New("the agent's project directory is unknown")}
	}
	l := resolveAgentProjectLayout(projectDir)
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

// localAgentDirGuard prepares the check for an agent directory known only
// by its path, agentsRoot/<name>: the agent root is agentDir (and
// extraRoots), anchored at the directory holding agentsRoot, so an agents
// root that is a symlink is caught; no project workspace is known, so
// everything outside the agent's directories is refused.
func localAgentDirGuard(agentName, agentDir string, extraRoots ...string) *agentFilesGuard {
	anchors := []string{filepath.Dir(filepath.Dir(agentDir))}
	for _, r := range extraRoots {
		if r != "" && !guardPathWithin(filepath.Clean(r), filepath.Clean(anchors[0])) {
			anchors = append(anchors, filepath.Dir(r))
		}
	}
	return newAgentFilesGuard(agentName, anchors, append([]string{agentDir}, extraRoots...), nil)
}

// CheckAgentWorktreeRemoval applies the agent files guard to the removal of
// agentName's worktree at worktreePath, under the project checkout base:
// the worktree must really be where it says inside base, and base itself is
// never removed. It returns the path to remove (see agentFilesGuard.check).
func CheckAgentWorktreeRemoval(agentName, base, worktreePath string) (string, error) {
	return newAgentFilesGuard(agentName, []string{base}, []string{worktreePath}, []string{base}).check(worktreePath)
}

func (g *agentFilesGuard) classify(path string, extraAgentRoots []string) error {
	if g.err != nil {
		return g.err
	}
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("the path is not absolute")
	}
	real, err := resolveRealPath(path)
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
		if guardPathWithin(real, root) {
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
