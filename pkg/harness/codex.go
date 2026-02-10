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

type Codex struct{}

func (c *Codex) Name() string {
	return "codex"
}

func (c *Codex) SeedTemplateDir(templateDir string, force bool) error {
	if err := config.SeedCommonFiles(templateDir, "common", c.GetEmbedDir(), c.DefaultConfigDir(), force); err != nil {
		return err
	}

	homeDir := filepath.Join(templateDir, "home")
	codexDir := filepath.Join(homeDir, ".codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		return err
	}

	// Seed config.toml
	tomlPath := filepath.Join(codexDir, "config.toml")
	data, err := config.EmbedsFS.ReadFile(filepath.Join("embeds", c.GetEmbedDir(), "config.toml"))
	if err == nil {
		// Always write config.toml
		if err := os.WriteFile(tomlPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write config.toml: %w", err)
		}
	}
	return nil
}

func (c *Codex) DiscoverAuth(agentHome string) api.AuthConfig {
	auth := api.AuthConfig{}
	// Check for Codex auth file in standard location
	home, _ := os.UserHomeDir()
	authPath := filepath.Join(home, ".codex", "auth.json")
	if _, err := os.Stat(authPath); err == nil {
		auth.CodexAuthFile = authPath
	}
	return auth
}

func (c *Codex) GetEnv(agentName string, agentHome string, unixUsername string, auth api.AuthConfig) map[string]string {
	env := make(map[string]string)
	if os.Getenv("OPENAI_API_KEY") != "" {
		env["OPENAI_API_KEY"] = os.Getenv("OPENAI_API_KEY")
	}
	if os.Getenv("CODEX_API_KEY") != "" {
		env["CODEX_API_KEY"] = os.Getenv("CODEX_API_KEY")
	}
	return env
}

func (c *Codex) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := []string{"codex", "--yolo"}
	if resume {
		args = append(args, "resume", "--last")
	} else {
		if task != "" {
			args = append(args, task)
		}
	}

	args = append(args, baseArgs...)
	return args
}

func (c *Codex) PropagateFiles(homeDir, unixUsername string, auth api.AuthConfig) error {
	if auth.CodexAuthFile != "" {
		dst := filepath.Join(homeDir, ".codex", "auth.json")
		// Check if it already exists in the template/agent home
		if _, err := os.Stat(dst); err == nil {
			return nil
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := util.CopyFile(auth.CodexAuthFile, dst); err != nil {
			return fmt.Errorf("failed to copy codex auth file: %w", err)
		}
	}
	return nil
}

func (c *Codex) GetVolumes(unixUsername string, auth api.AuthConfig) []api.VolumeMount {
	return nil
}

func (c *Codex) DefaultConfigDir() string {
	return ""
}

func (c *Codex) HasSystemPrompt(agentHome string) bool {
	return false
}

func (c *Codex) Provision(ctx context.Context, agentName, agentHome, agentWorkspace string) error {
	auth := c.DiscoverAuth(agentHome)
	return c.PropagateFiles(agentHome, "", auth)
}

func (c *Codex) GetEmbedDir() string {
	return "codex"
}

func (c *Codex) GetInterruptKey() string {
	return "C-c"
}
