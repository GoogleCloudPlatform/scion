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

package harness

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// DeclarativeGenericHarness implements api.Harness for harness-config
// directories that have no provision.py and no built-in Go harness — they
// rely entirely on the metadata declared in config.yaml. It is the
// "Alternative C, partial adoption" path from the design doc and lets
// external harness authors stand up a new harness with no code.
type DeclarativeGenericHarness struct {
	entry config.HarnessConfigEntry
}

// NewDeclarativeGenericHarness wraps a HarnessConfigEntry as an api.Harness.
func NewDeclarativeGenericHarness(entry config.HarnessConfigEntry) *DeclarativeGenericHarness {
	if entry.Harness == "" {
		entry.Harness = "generic"
	}
	return &DeclarativeGenericHarness{entry: entry}
}

func (d *DeclarativeGenericHarness) Name() string { return d.entry.Harness }

func (d *DeclarativeGenericHarness) DefaultConfigDir() string {
	if d.entry.ConfigDir != "" {
		return d.entry.ConfigDir
	}
	return ".scion"
}

// SkillsDir returns the entry's skills_dir, or GenericSkillsDir when it names
// none. For the entries Resolve hands it (no provisioner block) this equals
// SkillsDirForEntry; the New shim can also wrap an embedded entry that has a
// provisioner block, so it keeps its own generic fallback.
func (d *DeclarativeGenericHarness) SkillsDir() string {
	if d.entry.SkillsDir != "" {
		return d.entry.SkillsDir
	}
	return GenericSkillsDir
}

func (d *DeclarativeGenericHarness) GetInterruptKey() string {
	if d.entry.InterruptKey != "" {
		return d.entry.InterruptKey
	}
	return "C-c"
}

func (d *DeclarativeGenericHarness) GetInterruptSequence() []string {
	if d.entry.InterruptSignal == "sequence" || len(d.entry.InterruptSequence) > 0 {
		return d.entry.InterruptSequence
	}
	return nil
}

// DeliverySpec returns the config's delivery block with defaults applied.
func (d *DeclarativeGenericHarness) DeliverySpec() api.DeliverySpec {
	return d.entry.ResolvedDelivery()
}

// ReadySpec returns the config's ready block with defaults applied.
func (d *DeclarativeGenericHarness) ReadySpec() api.ReadySpec {
	return d.entry.ResolvedReady()
}

func (d *DeclarativeGenericHarness) GetHarnessEmbedsFS() (embed.FS, string) {
	return embed.FS{}, ""
}

func (d *DeclarativeGenericHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	var caps api.HarnessAdvancedCapabilities
	if d.entry.Capabilities != nil {
		caps = *d.entry.Capabilities
	} else {
		// Reuse the legacy Generic capability matrix as a safe default.
		g := &Generic{}
		caps = g.AdvancedCapabilities()
	}
	caps.Harness = d.entry.Harness
	// agent_plugins: the bundle sidecar, else config.yaml's deprecated key;
	// the other capabilities keep the Generic defaults above when config.yaml
	// has no capabilities block.
	caps.AgentPlugins = d.entry.AgentPluginsCapability()
	return caps
}

func (d *DeclarativeGenericHarness) GetCommand(task string, resume bool, baseArgs []string) []string {
	if d.entry.Command == nil {
		args := append([]string{}, baseArgs...)
		if task != "" {
			args = append(args, task)
		}
		return args
	}
	cmd := d.entry.Command
	args := append([]string{}, cmd.Base...)
	if resume && cmd.ResumeFlag != "" {
		args = append(args, cmd.ResumeFlag)
	}
	args = append(args, baseArgs...)
	if task != "" {
		if cmd.TaskFlag != "" {
			args = append(args, cmd.TaskFlag, task)
		} else {
			args = append(args, task)
		}
	}
	return args
}

func (d *DeclarativeGenericHarness) GetEnv(agentName, agentHome, unixUsername string) map[string]string {
	out := map[string]string{
		"SCION_AGENT_NAME": agentName,
	}
	for k, v := range d.entry.EnvTemplate {
		out[k] = expandEnvTemplate(v, agentName, agentHome, unixUsername)
	}
	return out
}

func (d *DeclarativeGenericHarness) GetTelemetryEnv() map[string]string { return nil }

// HasSystemPrompt reports whether system_prompt_file exists in the agent
// home, looked up inside the home only and without following a symbolic
// link (readHomeFile).
func (d *DeclarativeGenericHarness) HasSystemPrompt(agentHome string) bool {
	if d.entry.SystemPromptFile == "" {
		return false
	}
	_, ok := readHomeFile(agentHome, d.entry.SystemPromptFile)
	return ok
}

func (d *DeclarativeGenericHarness) Provision(_ context.Context, _, _, _, _ string) error {
	return nil
}

// InjectAgentInstructions writes content to instructions_file (default
// agents.md) in the agent home, removing a file in the same directory whose
// name differs only in case. Every access goes through a handle confined to
// the agent home (pkg/agenthome): a path that leaves the home is refused, a
// linked directory component is refused, and a link at the file is
// replaced, never written through.
func (d *DeclarativeGenericHarness) InjectAgentInstructions(agentHome string, content []byte) error {
	target := d.entry.InstructionsFile
	if target == "" {
		target = "agents.md"
	}
	home, rel, err := openHomeTarget(agentHome, target, "instructions_file")
	if err != nil {
		return err
	}
	defer func() { _ = home.Close() }()
	dir := path.Dir(rel)
	if err := mkdirHomeParent(home, rel); err != nil {
		return fmt.Errorf("create instructions dir: %w", err)
	}
	base := path.Base(rel)
	if entries, err := readHomeDir(agentHome, dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(e.Name(), base) && e.Name() != base {
				_ = home.Remove(path.Join(dir, e.Name()))
			}
		}
	}
	return home.WriteFile(rel, content, 0644)
}

// InjectSystemPrompt writes the system prompt as system_prompt_mode says:
// prepended to instructions_file, or written to system_prompt_file (default
// .scion/system_prompt.md). Every read and write is confined to the agent
// home, as for InjectAgentInstructions.
func (d *DeclarativeGenericHarness) InjectSystemPrompt(agentHome string, content []byte) error {
	switch d.entry.SystemPromptMode {
	case "none":
		return nil
	case "prepend_to_instructions":
		instr := d.entry.InstructionsFile
		if instr == "" {
			instr = "agents.md"
		}
		home, rel, err := openHomeTarget(agentHome, instr, "instructions_file")
		if err != nil {
			return err
		}
		defer func() { _ = home.Close() }()
		header := fmt.Sprintf("# System Prompt\n\n%s\n\n---\n\n", string(content))
		existing, err := home.ReadFile(rel, maxHomePromptBytes)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := mkdirHomeParent(home, rel); err != nil {
			return err
		}
		merged := append([]byte(header), existing...)
		return home.WriteFile(rel, merged, 0644)
	default:
		// "native" or empty → write to declared file or fall back.
		target := d.entry.SystemPromptFile
		if target == "" {
			target = filepath.Join(".scion", "system_prompt.md")
		}
		home, rel, err := openHomeTarget(agentHome, target, "system_prompt_file")
		if err != nil {
			return err
		}
		defer func() { _ = home.Close() }()
		if err := mkdirHomeParent(home, rel); err != nil {
			return err
		}
		return home.WriteFile(rel, content, 0644)
	}
}

// maxHomePromptBytes bounds an instructions or system prompt file read from
// the agent home.
const maxHomePromptBytes = 4 << 20

// openHomeTarget opens the agent home (creating it) for a file named by the
// config.yaml field key, and returns the file's slash-separated path in the
// home. A value that is not a local relative path is refused; the error
// names the field, not the value.
func openHomeTarget(agentHome, target, key string) (*agenthome.Home, string, error) {
	if !filepath.IsLocal(target) || filepath.Clean(target) == "." {
		return nil, "", fmt.Errorf("%s must be a relative path inside the agent home", key)
	}
	home, err := agenthome.OpenCreate(agentHome, 0755)
	if err != nil {
		return nil, "", err
	}
	return home, path.Clean(filepath.ToSlash(target)), nil
}

// mkdirHomeParent creates the parent directories of rel in home; a linked
// or non-directory component is refused.
func mkdirHomeParent(home *agenthome.Home, rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	return home.MkdirAll(dir, 0755)
}

// readHomeDir lists dir, a slash-separated path in the agent home, through
// an os.Root opened on the home, so the listing never leaves the home. It
// only yields names; anything removed is removed through agenthome.Home.
func readHomeDir(agentHome, dir string) ([]fs.DirEntry, error) {
	r, err := os.OpenRoot(agentHome)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return fs.ReadDir(r.FS(), dir)
}

// readHomeFile reads rel, a path relative to the agent home, through a
// handle confined to the home (pkg/agenthome): a path that leaves the home,
// a linked component or a linked or non-regular file is not read. ok is
// false when the file is absent or refused.
func readHomeFile(agentHome, rel string) (data []byte, ok bool) {
	if agentHome == "" || !filepath.IsLocal(rel) {
		return nil, false
	}
	home, err := agenthome.Open(agentHome)
	if err != nil {
		return nil, false
	}
	defer func() { _ = home.Close() }()
	data, err = home.ReadFile(filepath.ToSlash(rel), maxHomePromptBytes)
	if err != nil {
		return nil, false
	}
	return data, true
}

// ResolveAuth defers to the legacy passthrough behavior — DeclarativeGeneric
// is meant for harnesses without script-managed auth selection. Auth metadata
// declared in config.yaml drives broker preflight (Phase 3).
func (d *DeclarativeGenericHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	g := &Generic{}
	return g.ResolveAuth(auth)
}
