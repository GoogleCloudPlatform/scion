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

package templateimport

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"gopkg.in/yaml.v3"
)

// WriteTemplate creates a scion template directory from an ImportedAgent.
// For scion-format templates, it performs a direct copy. For harness-specific
// formats, it seeds the base agnostic template, then overwrites the instruction
// file with the imported system prompt and updates scion-agent.yaml with the
// model from the import.
// Returns the path to the created template directory.
func WriteTemplate(agent *ImportedAgent, templatesDir string, force bool) (string, error) {
	templateDir := filepath.Join(templatesDir, agent.Name)

	// Scion-format templates are copied directly
	if agent.ScionFormat && agent.SourcePath != "" {
		return CopyScionTemplate(agent.SourcePath, templateDir, force)
	}

	// Check if template already exists
	if !force {
		if _, err := os.Stat(templateDir); err == nil {
			return "", fmt.Errorf("template '%s' already exists at %s (use --force to overwrite)", agent.Name, templateDir)
		}
	}

	// If force and exists, remove existing
	if force {
		_ = os.RemoveAll(templateDir)
	}

	// Seed the base agnostic template
	if err := config.SeedAgnosticTemplate(templateDir, true); err != nil {
		return "", fmt.Errorf("failed to seed template directory: %w", err)
	}

	// Overwrite the instruction file with the imported system prompt
	if agent.SystemPrompt != "" {
		instructionPath := getInstructionFilePath(agent.Harness, templateDir)
		if instructionPath != "" {
			if err := os.MkdirAll(filepath.Dir(instructionPath), 0755); err != nil {
				return "", fmt.Errorf("failed to create instruction directory: %w", err)
			}
			if err := os.WriteFile(instructionPath, []byte(agent.SystemPrompt+"\n"), 0644); err != nil {
				return "", fmt.Errorf("failed to write instruction file: %w", err)
			}
		}
	}

	// Update scion-agent.yaml with model and default_harness_config
	if agent.Model != "" || agent.Harness != "" {
		if err := updateScionAgentConfig(templateDir, agent); err != nil {
			return "", fmt.Errorf("failed to update scion-agent.yaml: %w", err)
		}
	}

	return templateDir, nil
}

// getInstructionFilePath returns the path to the harness instruction file within a template.
func getInstructionFilePath(harnessName, templateDir string) string {
	homeDir := filepath.Join(templateDir, "home")
	switch harnessName {
	case "claude":
		return filepath.Join(homeDir, ".claude", "CLAUDE.md")
	case "gemini":
		return filepath.Join(homeDir, ".gemini", "GEMINI.md")
	default:
		return ""
	}
}

// updateScionAgentConfig reads the existing scion-agent.yaml, updates the model
// and default_harness_config fields, and writes it back.
func updateScionAgentConfig(templateDir string, agent *ImportedAgent) error {
	configPath := filepath.Join(templateDir, "scion-agent.yaml")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	var cfg api.ScionConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return err
	}

	if agent.Model != "" {
		cfg.Model = agent.Model
	}
	if agent.Harness != "" {
		// Map config-file dialect names to harness identifiers. The dialect names
		// ("claude", "gemini") are on-disk path conventions (.claude/, .gemini/) and
		// must not be renamed. The harness identifiers are the runtime names under
		// harnesses/ that DefaultHarnessConfig expects.
		harnessID := agent.Harness
		switch agent.Harness {
		case "gemini":
			harnessID = "gemini-cli"
			// "claude" maps to "claude" — no translation needed.
		}
		cfg.DefaultHarnessConfig = harnessID
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, out, 0644)
}
