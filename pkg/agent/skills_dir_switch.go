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
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// and is skipped.
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
	defer root.Close()

	// The source and every parent component of both directories must be
	// real directories, not symbolic links.
	if ok, err := realDirChain(root, oldRel, true); err != nil || !ok {
		return nil, err
	}
	if ok, err := realDirChain(root, newRel, false); err != nil || !ok {
		return nil, err
	}

	if entries, err := readRootDir(root, newRel); err == nil && len(entries) > 0 {
		return nil, nil
	} else if err != nil && !os.IsNotExist(err) {
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
	for _, e := range entries {
		// Only real skill directories; a symlinked entry is not followed.
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if err := root.MkdirAll(newRel, 0755); err != nil {
			return copied, fmt.Errorf("create skills dir %s: %w", newRel, err)
		}
		if err := copyRootTree(root, filepath.Join(oldRel, e.Name()), filepath.Join(newRel, e.Name())); err != nil {
			return copied, fmt.Errorf("copy skill %s to %s: %w", e.Name(), newRel, err)
		}
		copied = append(copied, e.Name())
	}
	return copied, nil
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

// readRootDir lists the directory rel inside root.
func readRootDir(root *os.Root, rel string) ([]os.DirEntry, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// copyRootTree copies the directory src to dst, both inside root, keeping
// only regular files and directories. Symbolic links and other special files
// are skipped.
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
		case e.Type()&os.ModeSymlink != 0:
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
	defer in.Close()
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

// genericSkillsDir is the skills directory of the generic harnesses when the
// harness-config names none (harness.Generic, harness.DeclarativeGenericHarness).
const genericSkillsDir = ".scion/skills"

// previousHarnessSkillsDir returns the skills directory of the harness that
// harness-config name resolves to, read from its effective config entry
// without constructing a harness. It follows the implementation choice
// harness.Resolve makes: a container-script harness (provisioner block) uses
// the entry's skills_dir as is, and the generic harnesses fall back to
// genericSkillsDir. It returns "" when the harness-config cannot be found.
func previousHarnessSkillsDir(name, projectDir string, templatePaths []string, settings *config.VersionedSettings, profile string) string {
	hcDir, err := config.ResolveHarnessConfigDir("", name, projectDir, templatePaths...)
	if err != nil || hcDir == nil {
		return ""
	}
	entry := harness.EffectiveConfig(name, hcDir, settings, profile)
	if entry.Provisioner != nil || entry.SkillsDir != "" {
		return entry.SkillsDir
	}
	return genericSkillsDir
}
