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
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
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
func carryOverSkillsDir(agentHome, oldSkillsDir, newSkillsDir string) ([]string, error) {
	if oldSkillsDir == "" || newSkillsDir == "" {
		return nil, nil
	}
	oldRel, newRel := filepath.Clean(oldSkillsDir), filepath.Clean(newSkillsDir)
	if oldRel == newRel || !filepath.IsLocal(oldRel) || !filepath.IsLocal(newRel) {
		return nil, nil
	}
	src := filepath.Join(agentHome, oldRel)
	dst := filepath.Join(agentHome, newRel)

	if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
		return nil, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read skills dir %s: %w", newRel, err)
	}

	entries, err := os.ReadDir(src)
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
		if err := os.MkdirAll(dst, 0755); err != nil {
			return copied, fmt.Errorf("create skills dir %s: %w", newRel, err)
		}
		if err := util.CopyDir(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return copied, fmt.Errorf("copy skill %s to %s: %w", e.Name(), newRel, err)
		}
		copied = append(copied, e.Name())
	}
	return copied, nil
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
