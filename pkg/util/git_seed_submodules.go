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

package util

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// seedGitEnv is the environment for every git command the seed checks run:
// the host environment scrub (HostGitEnv), with any inherited GIT_DIR,
// GIT_COMMON_DIR, GIT_WORK_TREE or GIT_INDEX_FILE removed, then the explicit
// git dirs, and no global or system config.
func seedGitEnv(gitDir, commonDir string) []string {
	var env []string
	for _, kv := range HostGitEnv(os.Environ()) {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_DIR="+gitDir,
		"GIT_COMMON_DIR="+commonDir,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	)
}

// seedAdminDir is one submodule admin dir the seed checks.
type seedAdminDir struct {
	abs   string // absolute path as discovered
	canon string // EvalSymlinks plus abs
	root  string // the work tree that owns it (R for <commonDir>/modules, the workspace for <own>/modules)
	label string // fixed layer label prefix, built from safe parts only
}

// CheckSeedSubmodules checks, before an agent is seeded, every submodule admin
// dir the agent's git (or host-side git acting on its workspace) can reach, and
// every gitlink in the agent's index that points at one. It must run after
// CheckSeedRepoConfigLayers has passed for the same repository: the gitlink
// enumeration below runs git, which reads those layers.
//
// commonDir is the repository's common .git dir, workspace the agent's work
// tree, and adminName the broker-trusted name of the agent's own worktree admin
// dir ("" for a main or shared-plain checkout, where the workspace's .git is
// commonDir itself). The seed assumes no agent is running on this workspace
// while it is checked; a change made after the check is a runtime concern.
//
// It returns a *RepoConfigRefusedError for the first violation, and an error
// for anything it cannot read, parse or run (fail closed).
func CheckSeedSubmodules(ctx context.Context, commonDir, workspace, adminName string) error {
	return checkSeedSubmodules(ctx, commonDir, workspace, adminName, readSeedConfigLayer)
}

// checkSeedSubmodules is CheckSeedSubmodules with the config reader passed in,
// so a test can make one admin dir's parsed set differ from what git reads and
// see that dir's cross-check refuse.
func checkSeedSubmodules(ctx context.Context, commonDir, workspace, adminName string, read func(path, label string) ([]gitConfigVar, error)) error {
	mainRoot := filepath.Dir(commonDir)
	gitDir := commonDir
	if adminName != "" {
		gitDir = filepath.Join(commonDir, "worktrees", adminName)
	}

	// Discovered admin dirs: <commonDir>/modules (D_main) and, for a linked
	// worktree, <own>/modules (D_own). Sibling worktrees are never walked.
	var dirs []seedAdminDir
	discover := func(base, root, layer, labelPrefix string) error {
		found, unsafe, err := SubmoduleAdminDirs(base)
		if err != nil || len(unsafe) > 0 {
			return newRepoConfigRefusedError("", layer, "<modules>", "", "submodule admin directory is not a plain directory tree", true, AudienceRemote)
		}
		for _, rel := range found {
			abs := filepath.Join(base, filepath.FromSlash(rel))
			canon, err := canonicalExisting(abs)
			if err != nil {
				return fmt.Errorf("cannot resolve submodule admin directory (refusing): %w", err)
			}
			name := strings.TrimPrefix(rel, "modules/")
			dirs = append(dirs, seedAdminDir{abs: abs, canon: canon, root: root, label: labelPrefix + safeSubsectionLabel("submodule", name)})
		}
		return nil
	}
	if err := discover(commonDir, mainRoot, "modules", "modules/"); err != nil {
		return err
	}
	if adminName != "" {
		if err := discover(gitDir, workspace, "worktree modules", "worktree modules/"); err != nil {
			return err
		}
	}

	// The gitlinks in the agent's own index, and the admin dir each one's
	// gitfile resolves to. refs maps an admin dir's canonical path to the
	// canonical work-tree path of the gitlink that references it.
	gitlinks, err := seedGitlinks(ctx, gitDir, commonDir)
	if err != nil {
		return err
	}
	refs := map[string]string{}
	for _, p := range gitlinks {
		target, wt, err := resolveSeedGitlink(workspace, p, dirs)
		if err != nil {
			return err
		}
		if target == "" {
			continue // not populated
		}
		if _, dup := refs[target]; dup {
			return refuseGitlink(p, "a submodule admin directory referenced by two git links")
		}
		refs[target] = wt
	}

	// Every discovered dir gets the full check. Every admin dir an accepted
	// gitfile targets is in dirs (resolveSeedGitlink only accepts those), so
	// each target is checked here, in this same call.
	for _, d := range dirs {
		ref, referenced := refs[d.canon]
		if err := checkSeedAdminDir(ctx, d, ref, referenced, commonDir, read); err != nil {
			return err
		}
	}
	return nil
}

// seedGitlinks lists the gitlink (mode 160000) paths in the index of gitDir,
// with host pins, an explicit GIT_DIR and GIT_COMMON_DIR, and no work tree. An
// absent index lists nothing; a git error refuses.
func seedGitlinks(ctx context.Context, gitDir, commonDir string) ([]string, error) {
	args := append(HostGitArgs(), "ls-files", "--stage", "-z")
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = gitDir
	cmd.Env = seedGitEnv(gitDir, commonDir)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cannot list the index's submodule entries to check them (refusing): %w", err)
	}
	seen := map[string]bool{}
	var paths []string
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := strings.Cut(string(rec), "\t")
		if !ok {
			return nil, fmt.Errorf("cannot parse the index listing (refusing)")
		}
		if !strings.HasPrefix(meta, "160000 ") || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths, nil
}

// resolveSeedGitlink validates gitlink path p (untrusted, from the index) and,
// when it is populated, returns the canonical admin dir its gitfile targets and
// the canonical work-tree path <workspace>/p. An unpopulated gitlink returns
// "", "". The target must be one of dirs and must not hold a commondir entry.
func resolveSeedGitlink(workspace, p string, dirs []seedAdminDir) (target, wt string, err error) {
	if p == "" || strings.HasPrefix(p, "/") || filepath.IsAbs(filepath.FromSlash(p)) {
		// p is not printed: a fixed label stands in for an absolute or empty path.
		return "", "", newRepoConfigRefusedError("", "submodule <absolute or empty path>", "<gitlink>", "", "a submodule git link with an absolute or empty path", true, AudienceRemote)
	}
	comps := strings.Split(p, "/")
	cur := workspace
	for _, c := range comps {
		if c == "" || c == "." || c == ".." || isDotGitComponent(c) {
			return "", "", refuseGitlink(p, "a submodule git link with an unsafe path component")
		}
		cur = filepath.Join(cur, c)
		fi, lerr := os.Lstat(cur)
		if lerr != nil {
			if errors.Is(lerr, fs.ErrNotExist) {
				return "", "", nil // not populated
			}
			return "", "", fmt.Errorf("cannot check submodule path (refusing): %w", lerr)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", "", refuseGitlink(p, "a submodule git link behind a symlinked path")
		}
	}
	dotGit := filepath.Join(cur, ".git")
	fi, lerr := os.Lstat(dotGit)
	switch {
	case lerr != nil && errors.Is(lerr, fs.ErrNotExist):
		return "", "", nil
	case lerr != nil:
		return "", "", fmt.Errorf("cannot check submodule .git (refusing): %w", lerr)
	case fi.IsDir():
		return "", "", refuseGitlink(p, "a submodule with its git directory inside the work tree (run git submodule absorbgitdirs)")
	case !fi.Mode().IsRegular():
		return "", "", refuseGitlink(p, "a submodule .git that is neither a git file nor absent")
	}
	data, rerr := readSmallRegularFile(dotGit, smallGitFileMax)
	if rerr != nil {
		return "", "", fmt.Errorf("cannot read submodule git file (refusing): %w", rerr)
	}
	// Parse exactly as git does (read_gitfile): strip only trailing newline and
	// carriage-return bytes, require the prefix "gitdir: ", and take the rest
	// verbatim. Anything git could read differently from this check refuses.
	line := strings.TrimRight(string(data), "\r\n")
	ptr, ok := strings.CutPrefix(line, "gitdir: ")
	if !ok || ptr == "" || strings.TrimSpace(ptr) != ptr || strings.ContainsAny(ptr, "\r\n\x00") {
		return "", "", refuseGitlink(p, "a submodule git file that is not a plain gitdir pointer")
	}
	// A relative pointer is joined without lexical cleaning, so EvalSymlinks
	// resolves ".." after following links, as git does.
	if !filepath.IsAbs(ptr) {
		ptr = cur + string(filepath.Separator) + ptr
	}
	// Resolve, then compare against the discovered set. This assumes seeding
	// does not run concurrently with an agent on this workspace: a link swapped
	// between this resolution and git's own is a runtime concern, not checked
	// here.
	canon, cerr := canonicalExisting(ptr)
	if cerr != nil {
		return "", "", refuseGitlink(p, "a submodule git file whose target does not resolve")
	}
	matched := false
	for _, d := range dirs {
		if d.canon == canon {
			matched = true
			break
		}
	}
	if !matched {
		return "", "", refuseGitlink(p, "a submodule git file whose target is not a checked admin directory")
	}
	if _, cerr := os.Lstat(filepath.Join(canon, "commondir")); cerr == nil || !errors.Is(cerr, fs.ErrNotExist) {
		return "", "", refuseGitlink(p, "a submodule admin directory that redirects its common directory, or cannot be checked for one")
	}
	wtCanon, werr := canonicalExisting(cur)
	if werr != nil {
		return "", "", fmt.Errorf("cannot resolve submodule work tree (refusing): %w", werr)
	}
	return canon, wtCanon, nil
}

// checkSeedAdminDir is the full check of one submodule admin dir: its config
// and config.worktree layers as data (seed rules, no exemption), then the git
// cross-check with GIT_DIR and GIT_COMMON_DIR set to the dir. A core.worktree
// is allowed only as checkSeedModuleWorktree permits.
func checkSeedAdminDir(ctx context.Context, d seedAdminDir, ref string, referenced bool, commonDir string, read func(path, label string) ([]gitConfigVar, error)) error {
	type layer struct{ path, label string }
	layers := []layer{
		{filepath.Join(d.abs, "config"), d.label + "/config"},
		{filepath.Join(d.abs, "config.worktree"), d.label + "/config.worktree"},
	}
	if entries, err := os.ReadDir(filepath.Join(d.abs, "worktrees")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				layers = append(layers, layer{filepath.Join(d.abs, "worktrees", e.Name(), "config.worktree"), d.label + "/worktrees/config.worktree"})
			}
		}
	}
	// Whether any layer file exists, by Lstat and independent of the parser:
	// the cross-check below is skipped only when none does.
	present := false
	for _, l := range layers {
		_, err := os.Lstat(l.path)
		switch {
		case err == nil:
			present = true
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("cannot check submodule config %s (refusing): %w", l.label, err)
		}
	}
	var parsed [][]gitConfigVar
	var all []gitConfigVar
	for _, l := range layers {
		vars, err := read(l.path, l.label)
		if err != nil {
			return err
		}
		parsed = append(parsed, vars)
		all = append(all, vars...)
	}
	opts := repoConfigAllowOptions{seed: true}
	for _, v := range all {
		if v.Section == "core" && !v.HasSubsection && v.Name == "worktree" {
			if checkSeedModuleWorktree(d, v.Value, ref, referenced, commonDir) {
				opts.worktreePath = v.Value
			}
			break
		}
	}
	for i, l := range layers {
		if err := checkRepoConfigVars("", l.label, parsed[i], RepoPolicyLinked, opts, nil); err != nil {
			return err
		}
	}
	if !present {
		return nil // git reads no repository config from this dir
	}
	// The merged set cannot attribute a key to one file, so the layer names the
	// module only.
	return crossCheckAgainstGit(ctx, d.abs, seedGitEnv(d.abs, d.abs), all, d.label, true, AudienceRemote)
}

// checkSeedModuleWorktree reports whether a submodule admin dir's core.worktree
// value is allowed. A relative value is joined, without lexical cleaning,
// against the admin dir's canonical path, as git resolves it against the real
// git dir. Referenced (R1): the target exists and resolves (physically, through
// EvalSymlinks) exactly to the referencing gitlink's work-tree path.
// Unreferenced (R2): a relative value may climb only with leading ".."
// components (the form git writes), so its cleaned form equals git's physical
// walk through the real directories above the canonical admin dir; an absolute
// value is canonicalised with pathcanon.Resolve; the target
// then lies strictly inside the owning root (canonical), has no ".git"
// component below it, is not in the common dir, and every existing component
// resolves the same way.
func checkSeedModuleWorktree(d seedAdminDir, value, ref string, referenced bool, commonDir string) bool {
	raw := value
	if !filepath.IsAbs(raw) {
		raw = d.canon + string(filepath.Separator) + raw
	}
	if referenced {
		canon, err := canonicalExisting(raw)
		return err == nil && canon == ref
	}
	if !filepath.IsAbs(value) {
		climbing := true
		for _, c := range strings.Split(filepath.ToSlash(value), "/") {
			switch c {
			case "..":
				if !climbing {
					return false
				}
			case "", ".":
				// no-op components
			default:
				climbing = false
			}
		}
	}
	// Both bases are canonical: an absolute value is resolved (deepest
	// existing ancestor through its links, plus the validated tail); a relative
	// value is already rooted at the canonical admin dir.
	t := filepath.Clean(raw)
	if filepath.IsAbs(value) {
		r, err := pathcanon.Resolve(t)
		if err != nil {
			return false
		}
		t = r
	}
	root, err := filepath.EvalSymlinks(d.root)
	if err != nil {
		return false
	}
	within := func(base, p string) bool {
		in, err := pathcanon.Within(base, p)
		return err == nil && in
	}
	ok := func(p string) bool {
		if p == root || !within(root, p) || within(commonDir, p) {
			return false
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return false
		}
		for _, c := range strings.Split(filepath.ToSlash(rel), "/") {
			if isDotGitComponent(c) {
				return false
			}
		}
		return true
	}
	if !ok(t) {
		return false
	}
	rel, _ := filepath.Rel(root, t)
	cur := root
	for _, c := range strings.Split(filepath.ToSlash(rel), "/") {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			return false
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			r, err := filepath.EvalSymlinks(cur)
			if err != nil || !ok(r) {
				return false
			}
		}
	}
	return true
}

// isDotGitComponent reports whether a path component names .git, ignoring
// case and trailing dots and spaces.
func isDotGitComponent(c string) bool {
	return strings.EqualFold(strings.TrimRight(c, ". "), ".git")
}

// canonicalExisting returns EvalSymlinks plus abs of an existing path.
func canonicalExisting(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(r)
}

// refuseGitlink is the refusal for a gitlink that fails a structural rule. The
// layer names the gitlink path through the safe label only.
func refuseGitlink(p, reason string) error {
	return newRepoConfigRefusedError("", "submodule "+safeSubsectionLabel("submodule", p), "<gitlink>", "", reason, true, AudienceRemote)
}
