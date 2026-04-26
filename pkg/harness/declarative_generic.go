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
	"fmt"
	"os"
	"path/filepath"

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

func (d *DeclarativeGenericHarness) SkillsDir() string {
	if d.entry.SkillsDir != "" {
		return d.entry.SkillsDir
	}
	return ".scion/skills"
}

func (d *DeclarativeGenericHarness) GetInterruptKey() string {
	if d.entry.InterruptKey != "" {
		return d.entry.InterruptKey
	}
	return "C-c"
}

func (d *DeclarativeGenericHarness) GetEmbedDir() string { return "" }

func (d *DeclarativeGenericHarness) GetHarnessEmbedsFS() (embed.FS, string) {
	return embed.FS{}, ""
}

func (d *DeclarativeGenericHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	if d.entry.Capabilities != nil {
		caps := *d.entry.Capabilities
		caps.Harness = d.entry.Harness
		return caps
	}
	// Reuse the legacy Generic capability matrix as a safe default.
	g := &Generic{}
	caps := g.AdvancedCapabilities()
	caps.Harness = d.entry.Harness
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

func (d *DeclarativeGenericHarness) HasSystemPrompt(agentHome string) bool {
	if d.entry.SystemPromptFile == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(agentHome, d.entry.SystemPromptFile))
	return err == nil
}

func (d *DeclarativeGenericHarness) Provision(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (d *DeclarativeGenericHarness) InjectAgentInstructions(agentHome string, content []byte) error {
	target := d.entry.InstructionsFile
	if target == "" {
		target = "agents.md"
	}
	full := filepath.Join(agentHome, target)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return fmt.Errorf("create instructions dir: %w", err)
	}
	return os.WriteFile(full, content, 0644)
}

func (d *DeclarativeGenericHarness) InjectSystemPrompt(agentHome string, content []byte) error {
	switch d.entry.SystemPromptMode {
	case "none":
		return nil
	case "prepend_to_instructions":
		instr := d.entry.InstructionsFile
		if instr == "" {
			instr = "agents.md"
		}
		full := filepath.Join(agentHome, instr)
		header := fmt.Sprintf("# System Prompt\n\n%s\n\n---\n\n", string(content))
		existing, err := os.ReadFile(full)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return err
		}
		merged := append([]byte(header), existing...)
		return os.WriteFile(full, merged, 0644)
	default:
		// "native" or empty → write to declared file or fall back.
		target := d.entry.SystemPromptFile
		if target == "" {
			target = filepath.Join(".scion", "system_prompt.md")
		}
		full := filepath.Join(agentHome, target)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return err
		}
		return os.WriteFile(full, content, 0644)
	}
}

// ResolveAuth defers to the legacy passthrough behavior — DeclarativeGeneric
// is meant for harnesses without script-managed auth selection. Auth metadata
// declared in config.yaml drives broker preflight (Phase 3).
func (d *DeclarativeGenericHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	g := &Generic{}
	return g.ResolveAuth(auth)
}
