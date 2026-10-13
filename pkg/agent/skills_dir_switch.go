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
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// carryOverSkillsDir copies the skill subdirectories an agent was provisioned
// with in oldSkillsDir into newSkillsDir, both relative to agentHome. Start
// calls it when a --harness-config switch selects a harness whose skills
// directory differs from the one ProvisionAgent installed the skills into, so
// the agent keeps its skills under the new harness (ptone/scion#3129).
//
// It does nothing when either directory is empty, when both name the same
// directory, when either is not a local path inside agentHome, when the old
// directory is missing, or when the new directory already holds any entry
// (it never merges into or overwrites an existing skills directory). The old
// directory is left in place. It returns the names of the skills it copied.
//
// Every read and write goes through an os.Root opened on agentHome, so the
// carry-over stays within the agent home. Only regular files and directories
// are copied; a symbolic link anywhere (the skills directories themselves, a
// parent component of either, or an entry inside a skill) is not followed
// and is skipped. Python bytecode artifacts (__pycache__ directories and
// .pyc files) are skipped, as util.CopyDir does.
//
// If the copy fails partway, what this call wrote is removed again (the new
// directory itself when this call created it), so a later call retries.
func carryOverSkillsDir(agentHome, oldSkillsDir, newSkillsDir string) ([]string, error) {
	if oldSkillsDir == "" || newSkillsDir == "" {
		return nil, nil
	}
	oldRel, newRel := filepath.Clean(oldSkillsDir), filepath.Clean(newSkillsDir)
	if oldRel == newRel || !filepath.IsLocal(oldRel) || !filepath.IsLocal(newRel) {
		return nil, nil
	}

	root, err := os.OpenRoot(agentHome)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open agent home: %w", err)
	}
	defer func() { _ = root.Close() }()

	// The source and every parent component of both directories must be
	// real directories, not symbolic links.
	if ok, err := realDirChain(root, oldRel, true); err != nil || !ok {
		return nil, err
	}
	if ok, err := realDirChain(root, newRel, false); err != nil || !ok {
		return nil, err
	}

	newMissing := false
	if entries, err := readRootDir(root, newRel); err == nil && len(entries) > 0 {
		return nil, nil
	} else if os.IsNotExist(err) {
		newMissing = true
	} else if err != nil {
		return nil, fmt.Errorf("read skills dir %s: %w", newRel, err)
	}

	entries, err := readRootDir(root, oldRel)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read skills dir %s: %w", oldRel, err)
	}

	var copied []string
	// undo removes what this call wrote after a failure, so the new directory
	// is again missing or empty and a later Start retries the carry-over
	// instead of skipping it as non-empty. A new directory that existed
	// before (empty) is kept; only the skills written into it are removed.
	// It returns cause joined with any removal error, so a failed cleanup is
	// visible to the caller.
	undo := func(cause error, partial string) error {
		errs := []error{cause}
		if newMissing {
			if err := root.RemoveAll(newRel); err != nil {
				errs = append(errs, fmt.Errorf("remove %s: %w", newRel, err))
			}
			return errors.Join(errs...)
		}
		for _, name := range append(copied, partial) {
			if name == "" {
				continue
			}
			p := filepath.Join(newRel, name)
			if err := root.RemoveAll(p); err != nil {
				errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
			}
		}
		return errors.Join(errs...)
	}
	for _, e := range entries {
		// Only real skill directories; a symlinked entry is not followed.
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || skipCopyEntry(e) {
			continue
		}
		if err := root.MkdirAll(newRel, 0755); err != nil {
			return nil, undo(fmt.Errorf("create skills dir %s: %w", newRel, err), "")
		}
		if err := copyRootTree(root, filepath.Join(oldRel, e.Name()), filepath.Join(newRel, e.Name())); err != nil {
			return nil, undo(fmt.Errorf("copy skill %s to %s: %w", e.Name(), newRel, err), e.Name())
		}
		copied = append(copied, e.Name())
	}
	return copied, nil
}

// skipCopyEntry reports whether the carry-over leaves out e: Python bytecode
// artifacts (__pycache__ directories and .pyc files). It mirrors the skip
// rule in util.CopyDir (pkg/util/fs.go); keep the two in step.
func skipCopyEntry(e os.DirEntry) bool {
	if e.IsDir() {
		return e.Name() == "__pycache__"
	}
	return strings.HasSuffix(e.Name(), ".pyc")
}

// realDirChain reports whether every existing component of rel inside root is
// a real directory (Lstat, never following a symbolic link). A missing
// component ends the walk: it reports false when requireExist is set, and
// true otherwise (MkdirAll creates the rest).
func realDirChain(root *os.Root, rel string, requireExist bool) (bool, error) {
	cur := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := root.Lstat(cur)
		if os.IsNotExist(err) {
			return !requireExist, nil
		}
		if err != nil {
			return false, fmt.Errorf("stat %s: %w", cur, err)
		}
		if !fi.IsDir() {
			return false, nil
		}
	}
	return true, nil
}

// readRootDir lists the directory rel inside root, sorted by name like
// os.ReadDir (File.ReadDir returns directory order), so the copy order and
// the error reported on a failure do not depend on the filesystem.
func readRootDir(root *os.Root, rel string) ([]os.DirEntry, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}

// copyRootTree copies the directory src to dst, both inside root, keeping
// only regular files and directories. Symbolic links, other special files
// and the entries skipCopyEntry reports are skipped.
func copyRootTree(root *os.Root, src, dst string) error {
	fi, err := root.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return nil
	}
	if err := root.Mkdir(dst, fi.Mode().Perm()); err != nil && !os.IsExist(err) {
		return err
	}
	if dfi, err := root.Lstat(dst); err != nil {
		return err
	} else if !dfi.IsDir() {
		return fmt.Errorf("%s is not a directory", dst)
	}
	entries, err := readRootDir(root, src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		switch {
		case e.Type()&os.ModeSymlink != 0, skipCopyEntry(e):
			continue
		case e.IsDir():
			if err := copyRootTree(root, s, d); err != nil {
				return err
			}
		case e.Type().IsRegular():
			if err := copyRootFile(root, s, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyRootFile copies the regular file src to dst, both inside root. It
// re-checks src with Lstat and refuses to replace an existing dst.
func copyRootFile(root *os.Root, src, dst string) error {
	fi, err := root.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	in, err := root.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// storedHarnessConfigName is the harness-config name recorded in the agent's
// scion-agent.json at provisioning, or "" when none is recorded.
func storedHarnessConfigName(cfg *api.ScionConfig) string {
	if cfg == nil {
		return ""
	}
	return cfg.HarnessConfig
}

// previousHarnessSkillsDir returns the skills directory of the harness that
// harness-config name resolves to, read from its effective config entry
// without constructing a harness, via harness.SkillsDirForEntry (harness.Resolve
// owns the rule). It returns "" when the harness-config cannot be found.
func previousHarnessSkillsDir(name, projectDir string, templatePaths []string, settings *config.VersionedSettings, profile string) string {
	hcDir, err := config.ResolveHarnessConfigDir("", name, projectDir, templatePaths...)
	if err != nil || hcDir == nil {
		return ""
	}
	return harness.SkillsDirForEntry(harness.EffectiveConfig(name, hcDir, settings, profile))
}
