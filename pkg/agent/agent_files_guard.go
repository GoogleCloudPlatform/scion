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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
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
		if pathWithin(abs, a) && len(a) > len(anchor) {
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

// check returns nil when path, an existing path about to be removed, lies
// inside one of the agent's own directories (or one of extraAgentRoots,
// directories this delete has already established are the agent's to
// remove, verified against the anchors like the agent's own), and
// ErrAgentFilesDeleteRefused otherwise. A refusal is logged.
func (g *agentFilesGuard) check(path string, extraAgentRoots ...string) error {
	if err := g.classify(path, extraAgentRoots); err != nil {
		err = fmt.Errorf("%w %s for agent %s: %w", ErrAgentFilesDeleteRefused, path, g.agentName, err)
		slog.Warn("Agent files delete refused; agent deletion never removes a shared project workspace",
			"agent", g.agentName, "path", path, "error", err)
		return err
	}
	return nil
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
		if pathWithin(root, real) {
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
		if pathWithin(real, root) {
			return nil
		}
	}
	for _, root := range g.projectRoots {
		if pathWithin(real, root) {
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

// pathWithin reports whether path is dir or lies under it, comparing whole
// path elements. Both must be clean absolute paths.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
