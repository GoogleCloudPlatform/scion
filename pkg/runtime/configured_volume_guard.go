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

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// errVolumeNamesAgentsTree is returned for a configured volume whose source
// resolves inside an agents directory under ~/.scion/project-configs, or
// (when writable) to a directory that contains one.
var errVolumeNamesAgentsTree = errors.New("volume source resolves to an agents directory under project-configs")

// projectConfigsDirFunc returns ~/.scion/project-configs. A variable so
// tests can point it elsewhere.
var projectConfigsDirFunc = func() (string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, projectkeys.ProjectConfigsDir), nil
}

// checkVolumeSource applies the configured-volume checks: the agents
// directories under ~/.scion/project-configs
// (checkVolumeSourceOutsideAgentsTrees).
func checkVolumeSource(src string, readOnly bool) error {
	return checkVolumeSourceOutsideAgentsTrees(src, readOnly)
}

// checkVolumeSourceOutsideAgentsTrees refuses a configured volume source
// that resolves inside ~/.scion/project-configs/<dir>/.scion/agents, where
// agents' own directories (and their repository root records) live. A
// writable volume whose source contains such a directory
// (project-configs itself, <dir>, or <dir>/.scion) is refused too. The
// source is resolved by resolvePhysicalPath (relative paths against the
// working directory, symlinks followed, ".." applied after the component
// before it is resolved, and a fail-closed error when a component cannot be
// resolved), and its position under project-configs is found by file
// identity as well as by path.
func checkVolumeSourceOutsideAgentsTrees(src string, readOnly bool) error {
	pc, err := projectConfigsDirFunc()
	if err != nil {
		return fmt.Errorf("%w: cannot locate the project-configs directory: %v", errVolumeNamesAgentsTree, err)
	}
	resolvedPC, err := resolvePhysicalPath(pc)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve the project-configs directory: %v", errVolumeNamesAgentsTree, err)
	}
	resolvedSrc, err := resolvePhysicalPath(src)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve volume source %q: %v", errVolumeNamesAgentsTree, src, err)
	}
	if !readOnly && (pathAtOrBelow(resolvedPC, resolvedSrc) || sameAsPathOrAncestor(resolvedSrc, resolvedPC)) {
		return fmt.Errorf("%w: writable volume %q contains %s, which holds agents' own directories (mount it read-only, or mount a directory that does not contain it)", errVolumeNamesAgentsTree, src, resolvedPC)
	}
	rel, ok := relBelow(resolvedPC, resolvedSrc)
	if !ok {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	switch {
	case len(parts) >= 3 && parts[1] == ".scion" && parts[2] == "agents":
		return fmt.Errorf("%w: %q is inside an agents directory under %s", errVolumeNamesAgentsTree, src, resolvedPC)
	case !readOnly && (rel == "." || len(parts) == 1 || (len(parts) == 2 && parts[1] == ".scion")):
		return fmt.Errorf("%w: writable volume %q contains an agents directory under %s (mount it read-only, or mount a directory that does not contain it)", errVolumeNamesAgentsTree, src, resolvedPC)
	}
	return nil
}

// relBelow returns p relative to base when p is base or below it, found by
// path or by file identity: if an existing ancestor of p is the same file
// as base (os.SameFile), the rest of p below that ancestor is returned.
func relBelow(base, p string) (string, bool) {
	if pathAtOrBelow(p, base) {
		rel, err := filepath.Rel(base, p)
		return rel, err == nil
	}
	baseInfo, err := os.Stat(base)
	if err != nil {
		return "", false
	}
	for dir := p; ; {
		if fi, err := os.Stat(dir); err == nil && os.SameFile(fi, baseInfo) {
			rel, err := filepath.Rel(dir, p)
			return rel, err == nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// pathAtOrBelow reports whether p is base or below it. Both must be clean,
// absolute paths.
func pathAtOrBelow(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolvePhysicalPath returns p with symlinks resolved, without cleaning it
// lexically first, so a ".." after a symlink goes to the link target's
// parent as it would for the kernel. A relative p is taken against the
// working directory. Components past the deepest existing ancestor are
// appended and the result cleaned (they cannot be symlinks, as they do not
// exist). A symlink whose target does not exist (a dangling link) is an
// error: where it leads cannot be checked.
func resolvePhysicalPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + string(filepath.Separator) + p
	}
	parts := strings.Split(p, string(filepath.Separator))
	for i := len(parts); i > 0; i-- {
		prefix := strings.Join(parts[:i], string(filepath.Separator))
		if prefix == "" {
			prefix = string(filepath.Separator)
		}
		resolved, err := filepath.EvalSymlinks(prefix)
		if err == nil {
			return filepath.Clean(filepath.Join(append([]string{resolved}, parts[i:]...)...)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(prefix); lerr == nil {
			return "", fmt.Errorf("%s is a symlink whose target does not exist", prefix)
		}
	}
	return "", fmt.Errorf("no part of %q exists", p)
}

// sameAsPathOrAncestor reports whether the existing directory target is the
// same file (os.SameFile) as p or as one of p's existing ancestors. It
// compares file identity, not spelling, so another name for target (a
// different letter case on a case-insensitive filesystem, or another mount
// of the same directory) is found too. It reports false when target does
// not exist.
func sameAsPathOrAncestor(target, p string) bool {
	targetInfo, err := os.Stat(target)
	if err != nil {
		return false
	}
	for dir := p; ; {
		if fi, err := os.Stat(dir); err == nil && os.SameFile(fi, targetInfo) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
