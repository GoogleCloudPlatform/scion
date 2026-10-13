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

package agentplugin

import (
	"os"
	"path/filepath"
	"slices"
)

// DiscoverSkills lists the skills of a plugin rooted at root: each immediate
// child directory of skills/ that holds a regular file named SKILL.md
// (spec §7.1). Symlinked entries are ignored. A skills location that is not
// a real directory is reported as a warning (§6.2). It is used for reporting
// and the index only; harnesses discover skills themselves.
func DiscoverSkills(root *os.Root) ([]string, []Diagnostic) {
	fi, err := root.Lstat(SkillsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, []Diagnostic{warning("skills ignored: %v", err)}
	}
	if !fi.IsDir() {
		return nil, []Diagnostic{warning("skills ignored: %s is not a directory", SkillsDir)}
	}
	entries, err := readRootDir(root, SkillsDir)
	if err != nil {
		return nil, []Diagnostic{warning("skills ignored: %v", err)}
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		sfi, err := root.Lstat(filepath.Join(SkillsDir, e.Name(), "SKILL.md"))
		if err != nil || !sfi.Mode().IsRegular() {
			continue
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names, nil
}
