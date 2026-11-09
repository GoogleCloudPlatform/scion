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
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// Skills reach an agent home only through pkg/agenthome: each skill
// directory is installed with InstallTreeFrom (or InstallTreeFS for the
// embedded platform skills), which reads its source through descriptors
// without following a link and replaces the skill in the home without
// following one either. A skill is a unit: installing it replaces any
// earlier copy of the same name as a whole.

// skillTreeLimits bounds one skill directory installed into an agent home.
var skillTreeLimits = agenthome.Limits{MaxFiles: 4096, MaxBytes: 64 << 20, MaxDepth: 32}

// errSkillsDirInvalid is the error for a skills_dir that is not a path
// inside the agent home. It names the field, never the value.
var errSkillsDirInvalid = errors.New("harness skills_dir must be a relative path inside the agent home")

// errSkillPathRefused is the error for a skill install refused because a
// directory on the skill's path, in the agent home (the skills directory
// or one of its parents) or in the skill's source, is a symbolic link or
// not a directory. It is a fixed text that names no path.
var errSkillPathRefused = errors.New("a directory on the skill's path (in the agent home or in the skill's source) is a symbolic link or not a directory; replace it with a real directory")

// skillsDirSource is what harnessSkillsDir reads a skills directory from:
// an api.Harness, or entrySkills for a harness-config entry.
type skillsDirSource interface{ SkillsDir() string }

// entrySkills is the skills directory of the harness a harness-config
// entry resolves to (harness.SkillsDirForEntry), for a caller that has the
// entry but no harness.
type entrySkills struct{ entry config.HarnessConfigEntry }

func (e entrySkills) SkillsDir() string {
	return harness.SkillsDirForEntry(e.entry)
}

// harnessSkillsDir returns h's skills directory, slash separated and
// relative to the agent home, or "" when h has none. It is the only way
// this package gets a skills directory: the value comes from a
// harness-config (possibly bundled by a template) or settings, so one that
// is not a path inside the home (config.IsHomeRelPath) is
// errSkillsDirInvalid rather than a directory outside it.
func harnessSkillsDir(h skillsDirSource) (string, error) {
	dir := h.SkillsDir()
	if dir == "" {
		return "", nil
	}
	if !config.IsHomeRelPath(dir) {
		return "", errSkillsDirInvalid
	}
	return filepath.ToSlash(filepath.Clean(dir)), nil
}

// skillRel is the path, relative to the agent home, of the skill name in
// skillsDir.
func skillRel(skillsDir, name string) string { return path.Join(skillsDir, name) }

// homeSkillError maps an error from installing a skill into the agent
// home to the reported error: a limit, or a directory on the skill's path
// that is a link or not a directory, gets a fixed text that names no path.
func homeSkillError(name string, err error) error {
	var le *agenthome.LimitError
	switch {
	case errors.As(err, &le):
		return fmt.Errorf("skill %s exceeds the agent home limit of %s; it was not installed", name, le.Limit)
	case errors.Is(err, agenthome.ErrLinkComponent), errors.Is(err, agenthome.ErrNotDirectory):
		return errSkillPathRefused
	}
	return err
}

// homeLocalSkills lists the skills already in skillsDir of the agent home,
// except the names in skip, as resolution entries with source "local".
// The listing follows no link (agenthome.Home.ReadDir): only a real
// directory is a skill, a hidden name is not, and neither is an Agent
// Plugins bundle (a regular-file plugin.json at its top, which the harness
// loads as a plugin). A missing skills directory lists none; a linked one
// is errSkillPathRefused.
func homeLocalSkills(home *agenthome.Home, skillsDir string, skip map[string]bool) ([]SkillResolutionEntry, error) {
	if skillsDir == "" {
		return nil, nil
	}
	infos, err := home.ReadDir(skillsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, homeSkillError("", err)
	}
	var out []SkillResolutionEntry
	for _, fi := range infos {
		name := fi.Name()
		if !fi.IsDir() || strings.HasPrefix(name, ".") || skip[name] {
			continue
		}
		if m, err := home.Lstat(path.Join(skillRel(skillsDir, name), agentplugin.ManifestFile)); err == nil && m.Mode().IsRegular() {
			continue
		}
		out = append(out, SkillResolutionEntry{
			Name:          name,
			InstalledPath: skillRel(skillsDir, name),
			Source:        "local",
		})
	}
	return out, nil
}

// templateSkillsDirName is the fixed skills directory of a template.
const templateSkillsDirName = "skills"

// installTemplateSkills installs the skills of each template in chain
// (base first) into skillsDir of the agent home and returns the names of
// the skill directories the chain provides. Each skill directory is
// installed as a unit (InstallTreeFrom), so a later template's skill
// replaces an earlier template's skill of the same name as a whole; a
// loose regular file in a template's skills/ is copied as a file.
//
// A template is read from the trusted directory it was found from
// (config.Template.OpenDir): a template whose directory path, or whose
// skills/, is a symbolic link or sits below one is skipped with a warning
// naming the template, never followed. Inside skills/, links, special
// files and multi-link files are skipped too.
func installTemplateSkills(agentHome string, chain []*config.Template, skillsDir string) (map[string]bool, error) {
	home, err := agenthome.Open(agentHome)
	if err != nil {
		return nil, err
	}
	defer func() { _ = home.Close() }()
	names := map[string]bool{}
	for _, tpl := range chain {
		if err := installOneTemplateSkills(home, tpl, skillsDir, names); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func installOneTemplateSkills(home *agenthome.Home, tpl *config.Template, skillsDir string, names map[string]bool) error {
	src, err := tpl.OpenDir()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case isLinkRefusal(err):
		slog.Warn("Skipping the skills of a template: a directory on its path is a symbolic link or not a directory", "template", templateRef(tpl))
		return nil
	case err != nil:
		return fmt.Errorf("failed to open template %s: %w", templateRef(tpl), err)
	}
	defer func() { _ = src.Close() }()
	entries, err := src.ReadDir(templateSkillsDirName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case isLinkRefusal(err):
		slog.Warn("Skipping a template skills directory that is a symbolic link or not a directory", "template", templateRef(tpl))
		return nil
	case err != nil:
		return fmt.Errorf("failed to read template skills %s: %w", templateRef(tpl), err)
	}
	for _, e := range entries {
		name := e.Name()
		srcRel := path.Join(templateSkillsDirName, name)
		rel := skillRel(skillsDir, name)
		switch {
		case e.IsDir():
			if agenthome.SkipCopyName(name, true) {
				continue
			}
			names[name] = true
			if err := home.InstallTreeFrom(src, srcRel, rel, skillTreeLimits); err != nil {
				return fmt.Errorf("failed to copy template skill %s from %s: %w", name, templateRef(tpl), homeSkillError(name, err))
			}
		case e.Mode().IsRegular():
			if agenthome.SkipCopyName(name, false) {
				continue
			}
			data, mode, err := src.ReadFileMode(srcRel, skillTreeLimits.MaxBytes)
			if errors.Is(err, dirfd.ErrNotSingleLinkRegular) || errors.Is(err, syscall.ELOOP) {
				// Replaced by a link or hard-linked since the listing:
				// skipped like any link in the source.
				continue
			}
			if err != nil {
				return fmt.Errorf("failed to read template skills file %s from %s: %w", name, templateRef(tpl), err)
			}
			if err := home.MkdirAll(skillsDir, 0o755); err != nil {
				return homeSkillError(name, err)
			}
			if err := home.WriteFile(rel, data, agenthome.CopyFileMode(mode)); err != nil {
				return fmt.Errorf("failed to copy template skills file %s from %s: %w", name, templateRef(tpl), homeSkillError(name, err))
			}
		}
	}
	return nil
}
