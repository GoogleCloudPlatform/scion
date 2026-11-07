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
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
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
// directory, when either is not a path inside agentHome
// (config.IsHomeRelPath), when the old directory is missing, when either
// directory is a symbolic link or sits below one, or when the new directory
// already holds any entry (it never merges into or overwrites an existing
// skills directory). The old directory is left in place. It returns the
// names of the skills it copied.
//
// Every read and write goes through pkg/agenthome: the old directory is
// opened from the home with no component followed through a link, each
// real skill directory in it is installed with InstallTreeFrom (links and
// special files inside a skill are skipped, never followed, as are the
// agenthome.SkipCopyName entries), and an entry in the old directory that
// is not a real directory is not a skill.
//
// If the copy fails partway, what this call wrote is removed again (the new
// directory itself when this call created it), so a later call retries.
func carryOverSkillsDir(agentHome, oldSkillsDir, newSkillsDir string) ([]string, error) {
	if oldSkillsDir == "" || newSkillsDir == "" {
		return nil, nil
	}
	if !config.IsHomeRelPath(oldSkillsDir) || !config.IsHomeRelPath(newSkillsDir) {
		return nil, nil
	}
	oldRel := filepath.ToSlash(filepath.Clean(oldSkillsDir))
	newRel := filepath.ToSlash(filepath.Clean(newSkillsDir))
	if oldRel == newRel {
		return nil, nil
	}

	home, err := agenthome.Open(agentHome)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, carryOverStage("open", fmt.Errorf("open agent home: %w", err))
	}
	defer func() { _ = home.Close() }()

	src, err := agenthome.OpenBelow(agentHome, oldRel)
	switch {
	case errors.Is(err, fs.ErrNotExist), isLinkRefusal(err):
		return nil, nil
	case err != nil:
		return nil, carryOverStage("check", err)
	}
	defer func() { _ = src.Close() }()

	newMissing := false
	switch existing, err := home.ReadDir(newRel); {
	case err == nil && len(existing) > 0:
		return nil, nil
	case errors.Is(err, fs.ErrNotExist):
		newMissing = true
	case isLinkRefusal(err):
		return nil, nil
	case err != nil:
		return nil, carryOverStage("read", fmt.Errorf("read skills dir %s: %w", newRel, err))
	}

	entries, err := src.ReadDir(".")
	if err != nil {
		return nil, carryOverStage("read", fmt.Errorf("read skills dir %s: %w", oldRel, err))
	}

	var copied []string
	// undo removes what this call wrote after a failure, so the new directory
	// is again missing or empty and a later Start retries the carry-over
	// instead of skipping it as non-empty. A new directory that existed
	// before (empty) is kept; only the skills written into it are removed.
	// It returns cause joined with any removal error, so a failed cleanup is
	// visible to the caller.
	undo := func(cause error) error {
		errs := []error{cause}
		if newMissing {
			if err := home.RemoveAll(newRel); err != nil {
				errs = append(errs, carryOverStage("remove", fmt.Errorf("remove %s: %w", newRel, err)))
			}
			return errors.Join(errs...)
		}
		for _, name := range copied {
			p := skillRel(newRel, name)
			if err := home.RemoveAll(p); err != nil {
				errs = append(errs, carryOverStage("remove", fmt.Errorf("remove %s: %w", p, err)))
			}
		}
		return errors.Join(errs...)
	}
	for _, e := range entries {
		// Only real skill directories; a link is reported as a link by
		// ReadDir and never followed.
		if !e.IsDir() || agenthome.SkipCopyName(e.Name(), true) {
			continue
		}
		// An Agent Plugins bundle (claude installs them in its skills dir)
		// is not a skill: the new harness installs it natively from the
		// control-plane record instead.
		if isAgentPluginDir(src, e.Name()) {
			continue
		}
		// InstallTreeFrom leaves the destination as it was on failure, so
		// only the skills already copied need removing.
		if err := home.InstallTreeFrom(src, e.Name(), skillRel(newRel, e.Name()), skillTreeLimits); err != nil {
			return nil, undo(carryOverStage("copy", fmt.Errorf("copy skill %s to %s: %w", e.Name(), newRel, homeSkillError(e.Name(), err))))
		}
		copied = append(copied, e.Name())
	}
	return copied, nil
}

// isLinkRefusal reports whether err is pkg/agenthome refusing a path
// because a component is a symbolic link or not a directory.
func isLinkRefusal(err error) bool {
	return errors.Is(err, agenthome.ErrLinkComponent) || errors.Is(err, agenthome.ErrNotDirectory)
}

// isAgentPluginDir reports whether the directory name in dir holds a
// regular-file plugin.json at its top level, the mark of an Agent Plugins
// bundle. A linked plugin.json does not count (never followed).
func isAgentPluginDir(dir *agenthome.Home, name string) bool {
	fi, err := dir.Lstat(name + "/" + agentplugin.ManifestFile)
	return err == nil && fi.Mode().IsRegular()
}

// carryOverStageError is a carry-over failure together with the fixed name
// of the step that failed. Its text is the underlying error's.
type carryOverStageError struct {
	stage string
	err   error
}

func (e *carryOverStageError) Error() string { return e.err.Error() }
func (e *carryOverStageError) Unwrap() error { return e.err }

// carryOverStage returns err tagged with stage, or nil when err is nil.
func carryOverStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &carryOverStageError{stage: stage, err: err}
}

// carryOverFailureSummary returns, for a carryOverSkillsDir error, the
// fixed name of the step that failed ("open", "check", "read", "create",
// "copy" or "remove"; "other" when unknown) and the class of its error
// ("permission", "not-exist", "exist" or "other"), for the log.
func carryOverFailureSummary(err error) (stage, class string) {
	stage, cause := "other", err
	var se *carryOverStageError
	if errors.As(err, &se) {
		stage, cause = se.stage, se.err
	}
	switch {
	case errors.Is(cause, fs.ErrPermission):
		class = "permission"
	case errors.Is(cause, fs.ErrNotExist):
		class = "not-exist"
	case errors.Is(cause, fs.ErrExist):
		class = "exist"
	default:
		class = "other"
	}
	return stage, class
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
// owns the rule) and checked by harnessSkillsDir. It returns "" when the
// harness-config cannot be found or its skills directory is not a path
// inside the agent home.
func previousHarnessSkillsDir(name, projectDir string, templatePaths []string, settings *config.VersionedSettings, profile string) string {
	hcDir, err := config.ResolveHarnessConfigDir("", name, projectDir, templatePaths...)
	if err != nil || hcDir == nil {
		return ""
	}
	dir, err := harnessSkillsDir(entrySkills{harness.EffectiveConfig(name, hcDir, settings, profile)})
	if err != nil {
		return ""
	}
	return dir
}
