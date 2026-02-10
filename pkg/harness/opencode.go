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
	"fmt"
	"os"
	"path/filepath"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/util"
)

type OpenCode struct{}

func (o *OpenCode) Name() string {
	return "opencode"
}

func (o *OpenCode) SeedTemplateDir(templateDir string, force bool) error {
	if err := config.SeedCommonFiles(templateDir, "common", o.GetEmbedDir(), o.DefaultConfigDir(), force); err != nil {
		return err
	}

	// Seed opencode.json
	homeDir := filepath.Join(templateDir, "home")
	jsonPath := filepath.Join(homeDir, o.DefaultConfigDir(), "opencode.json")

	data, err := config.EmbedsFS.ReadFile(filepath.Join("embeds", o.GetEmbedDir(), "opencode.json"))
	if err == nil {
		// Always write opencode.json
		if err := os.WriteFile(jsonPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write opencode.json: %w", err)
		}
	}
	return nil
}

func (o *OpenCode) DiscoverAuth(agentHome string) api.AuthConfig {
	auth := api.AuthConfig{
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
	}
	// Check for OpenCode auth file in standard location
	home, _ := os.UserHomeDir()
	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if _, err := os.Stat(authPath); err == nil {
		auth.OpenCodeAuthFile = authPath
	}
	return auth
}

func (o *OpenCode) GetEnv(agentName string, agentHome string, unixUsername string, auth api.AuthConfig) map[string]string {
	env := make(map[string]string)
	if auth.AnthropicAPIKey != "" {
		env["ANTHROPIC_API_KEY"] = auth.AnthropicAPIKey
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		env["OPENAI_API_KEY"] = os.Getenv("OPENAI_API_KEY")
	}
	return env
}

func (o *OpenCode) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := []string{"opencode"}
	if resume {
		args = append(args, "--continue")
	} else {
		args = append(args, "--prompt")
		if task != "" {
			args = append(args, task)
		}
	}

	args = append(args, baseArgs...)
	return args
}
func (o *OpenCode) PropagateFiles(homeDir, unixUsername string, auth api.AuthConfig) error {
	if auth.OpenCodeAuthFile != "" {
		dst := filepath.Join(homeDir, ".local", "share", "opencode", "auth.json")
		// Check if it already exists in the template/agent home
		if _, err := os.Stat(dst); err == nil {
			return nil
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := util.CopyFile(auth.OpenCodeAuthFile, dst); err != nil {
			return fmt.Errorf("failed to copy opencode auth file: %w", err)
		}
	}
	return nil
}

func (o *OpenCode) GetVolumes(unixUsername string, auth api.AuthConfig) []api.VolumeMount {
	return nil
}

func (o *OpenCode) DefaultConfigDir() string {
	return ".config/opencode"
}

func (o *OpenCode) HasSystemPrompt(agentHome string) bool {
	return false
}

func (o *OpenCode) Provision(ctx context.Context, agentName, agentHome, agentWorkspace string) error {
	auth := o.DiscoverAuth(agentHome)
	return o.PropagateFiles(agentHome, "", auth)
}

func (o *OpenCode) GetEmbedDir() string {
	return "opencode"
}

func (o *OpenCode) GetInterruptKey() string {
	return "C-c"
}
