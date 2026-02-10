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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/util"
)

type GeminiCLI struct{}

func (g *GeminiCLI) Name() string {
	return "gemini"
}

func (g *GeminiCLI) DiscoverAuth(agentHome string) api.AuthConfig {
	auth := api.AuthConfig{
		GoogleAppCredentials: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		GoogleCloudProject:   util.FirstNonEmpty(os.Getenv("GOOGLE_CLOUD_PROJECT"), os.Getenv("GCP_PROJECT")),
	}

	home, _ := os.UserHomeDir()

	// 1. Check scion-agent.json for overrides
	selectedType := ""
	scionAgentPath := filepath.Join(filepath.Dir(agentHome), "scion-agent.json")
	if data, err := os.ReadFile(scionAgentPath); err == nil {
		var cfg api.ScionConfig
		if err := json.Unmarshal(data, &cfg); err == nil {
			if cfg.Gemini != nil {
				selectedType = cfg.Gemini.AuthSelectedType
			}
		}
	}

	// 2. Check agent settings
	agentSettingsPath := filepath.Join(agentHome, g.DefaultConfigDir(), "settings.json")
	if agentSettings, err := config.LoadAgentSettings(agentSettingsPath); err == nil {
		if selectedType == "" {
			selectedType = agentSettings.Security.Auth.SelectedType
		}
		if auth.GeminiAPIKey == "" && auth.GoogleAPIKey == "" {
			auth.GeminiAPIKey = agentSettings.ApiKey
		}
	}

	// 3. Load host settings for fallbacks
	hostSettings, _ := config.GetAgentSettings()
	if hostSettings != nil {
		if selectedType == "" {
			selectedType = hostSettings.Security.Auth.SelectedType
		}
		if auth.GeminiAPIKey == "" && auth.GoogleAPIKey == "" {
			auth.GeminiAPIKey = hostSettings.ApiKey
		}
	}

	auth.SelectedType = selectedType

	switch selectedType {
	case "oauth-personal":
		oauthPath := filepath.Join(home, g.DefaultConfigDir(), "oauth_creds.json")
		if _, err := os.Stat(oauthPath); err == nil {
			auth.OAuthCreds = oauthPath
		}
	case "vertex-ai":
		// Vertex might need project/location from env (already loaded) or settings
	}

	return auth
}

func (g *GeminiCLI) GetEnv(agentName string, agentHome string, unixUsername string, auth api.AuthConfig) map[string]string {
	env := make(map[string]string)

	env["GEMINI_AGENT_NAME"] = agentName
	
	if relPath := g.getSystemPromptRelPath(agentHome); relPath != "" {
		fullPath := fmt.Sprintf("%s/%s", util.GetHomeDir(unixUsername), relPath)
		env["GEMINI_SYSTEM_MD"] = fullPath
	}

	if auth.GeminiAPIKey != "" {
		env["GEMINI_API_KEY"] = auth.GeminiAPIKey
	}
	if auth.GoogleAPIKey != "" {
		env["GOOGLE_API_KEY"] = auth.GoogleAPIKey
	}
	if auth.VertexAPIKey != "" {
		env["VERTEX_API_KEY"] = auth.VertexAPIKey
	}

	if auth.SelectedType != "" {
		switch auth.SelectedType {
		case "gemini-api-key":
			env["GEMINI_DEFAULT_AUTH_TYPE"] = "gemini-api-key"
		case "vertex-ai":
			env["GEMINI_DEFAULT_AUTH_TYPE"] = "vertex-ai"
		case "oauth-personal":
			env["GEMINI_DEFAULT_AUTH_TYPE"] = "oauth-personal"
		}
	} else {
		// Legacy/Fallback behavior when SelectedType is not explicitly set
		if auth.GeminiAPIKey != "" || auth.GoogleAPIKey != "" {
			env["GEMINI_DEFAULT_AUTH_TYPE"] = "gemini-api-key"
		} else if auth.VertexAPIKey != "" {
			env["GEMINI_DEFAULT_AUTH_TYPE"] = "vertex-ai"
		}
	}

	if auth.GoogleCloudProject != "" {
		env["GOOGLE_CLOUD_PROJECT"] = auth.GoogleCloudProject
	}

	if auth.GoogleAppCredentials != "" {
		env["GEMINI_DEFAULT_AUTH_TYPE"] = "compute-default-credentials"
		// The path is fixed in PropagateFiles
		env["GOOGLE_APPLICATION_CREDENTIALS"] = fmt.Sprintf("%s/.config/gcp/application_default_credentials.json", util.GetHomeDir(unixUsername))
	}

	if auth.OAuthCreds != "" {
		env["GEMINI_DEFAULT_AUTH_TYPE"] = "oauth-personal"
	}

	return env
}

func (g *GeminiCLI) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := []string{"gemini", "--yolo"}
	if resume {
		args = append(args, "--resume")
	}
	args = append(args, baseArgs...)
	if task != "" {
		args = append(args, "--prompt-interactive", task)
	}
	return args
}

func (g *GeminiCLI) PropagateFiles(homeDir, unixUsername string, auth api.AuthConfig) error {
	if homeDir == "" {
		return nil
	}

	if auth.SelectedType != "" {
		geminiSettingsPath := filepath.Join(homeDir, g.DefaultConfigDir(), "settings.json")
		if err := g.updateSelectedAuthType(geminiSettingsPath, auth.SelectedType); err != nil {
			return fmt.Errorf("failed to update gemini settings: %w", err)
		}
	}

	if auth.OAuthCreds != "" {
		dst := filepath.Join(homeDir, g.DefaultConfigDir(), "oauth_creds.json")
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := util.CopyFile(auth.OAuthCreds, dst); err != nil {
			return fmt.Errorf("failed to copy oauth creds: %w", err)
		}
	}

	if auth.GoogleAppCredentials != "" {
		dst := filepath.Join(homeDir, ".config", "gcp", "application_default_credentials.json")
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := util.CopyFile(auth.GoogleAppCredentials, dst); err != nil {
			return fmt.Errorf("failed to copy application default credentials: %w", err)
		}
	}

	return nil
}

func (g *GeminiCLI) GetVolumes(unixUsername string, auth api.AuthConfig) []api.VolumeMount {
	var volumes []api.VolumeMount
	if auth.OAuthCreds != "" {
		volumes = append(volumes, api.VolumeMount{
			Source:   auth.OAuthCreds,
			Target:   fmt.Sprintf("%s/%s/oauth_creds.json", util.GetHomeDir(unixUsername), g.DefaultConfigDir()),
			ReadOnly: true,
		})
	}
	if auth.GoogleAppCredentials != "" {
		volumes = append(volumes, api.VolumeMount{
			Source:   auth.GoogleAppCredentials,
			Target:   fmt.Sprintf("%s/.config/gcp/application_default_credentials.json", util.GetHomeDir(unixUsername)),
			ReadOnly: true,
		})
	}
	return volumes
}

func (g *GeminiCLI) DefaultConfigDir() string {
	return ".gemini"
}

func (g *GeminiCLI) HasSystemPrompt(agentHome string) bool {
	return g.getSystemPromptRelPath(agentHome) != ""
}

func (g *GeminiCLI) getSystemPromptRelPath(agentHome string) string {
	if agentHome == "" {
		return ""
	}

	// 1. Check .gemini/system_prompt.md (New Standard)
	relPath := filepath.Join(g.DefaultConfigDir(), "system_prompt.md")
	fullPath := filepath.Join(agentHome, relPath)
	if g.isValidPromptFile(fullPath) {
		return relPath
	}

	// 2. Check system_prompt.md (Legacy / Root)
	relPath = "system_prompt.md"
	fullPath = filepath.Join(agentHome, relPath)
	if g.isValidPromptFile(fullPath) {
		return relPath
	}

	return ""
}

func (g *GeminiCLI) isValidPromptFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	content := strings.TrimSpace(string(data))
	if content == "" || content == "# Placeholder" {
		return false
	}
	return true
}

func (g *GeminiCLI) SeedTemplateDir(templateDir string, force bool) error {
	if err := config.SeedCommonFiles(templateDir, "common", g.GetEmbedDir(), g.DefaultConfigDir(), force); err != nil {
		return err
	}

	// Seed gemini.md
	homeDir := filepath.Join(templateDir, "home")
	mdPath := filepath.Join(homeDir, g.DefaultConfigDir(), "gemini.md")

	data, err := config.EmbedsFS.ReadFile(filepath.Join("embeds", g.GetEmbedDir(), "gemini.md"))
	if err != nil {
		return nil
	}

	if _, err := os.Stat(mdPath); os.IsNotExist(err) || force {
		if err := os.WriteFile(mdPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write gemini.md: %w", err)
		}
	}
	return nil
}

func (g *GeminiCLI) Provision(ctx context.Context, agentName, agentHome, agentWorkspace string) error {
	agentDir := filepath.Dir(agentHome)
	scionAgentPath := filepath.Join(agentDir, "scion-agent.json")

	data, err := os.ReadFile(scionAgentPath)
	if err != nil {
		return fmt.Errorf("failed to read scion-agent.json: %w", err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse scion-agent.json: %w", err)
	}

	selectedType := ""
	if cfg.Gemini != nil {
		selectedType = cfg.Gemini.AuthSelectedType
	}

	if selectedType != "" {
		// Update ~/.gemini/settings.json
		geminiSettingsPath := filepath.Join(agentHome, g.DefaultConfigDir(), "settings.json")
		if err := g.updateSelectedAuthType(geminiSettingsPath, selectedType); err != nil {
			return fmt.Errorf("failed to update gemini settings: %w", err)
		}
	}

	// Update scion-agent.json
	var envUpdates map[string]string
	var volUpdates []api.VolumeMount

	home, _ := os.UserHomeDir()

	switch selectedType {
	case "gemini-api-key":
		envUpdates = map[string]string{"GEMINI_API_KEY": "${GEMINI_API_KEY}"}
	case "oauth-personal":
		envUpdates = map[string]string{"GOOGLE_CLOUD_PROJECT": "${GOOGLE_CLOUD_PROJECT}"}
	case "vertex-ai":
		envUpdates = map[string]string{
			"GOOGLE_CLOUD_PROJECT":  "${GOOGLE_CLOUD_PROJECT}",
			"GOOGLE_CLOUD_LOCATION": "${GOOGLE_CLOUD_LOCATION}",
		}
		volUpdates = append(volUpdates, api.VolumeMount{
			Source:   filepath.Join(home, ".config", "gcloud"),
			Target:   "/home/scion/.config/gcloud",
			ReadOnly: true,
		})
	}

	if len(envUpdates) > 0 {
		if cfg.Env == nil {
			cfg.Env = make(map[string]string)
		}
		for k, v := range envUpdates {
			if _, exists := cfg.Env[k]; !exists {
				cfg.Env[k] = v
			}
		}
	}

	if len(volUpdates) > 0 {
		cfg.Volumes = append(cfg.Volumes, volUpdates...)
	}

	newData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}
	if err := os.WriteFile(scionAgentPath, newData, 0644); err != nil {
		return fmt.Errorf("failed to write updated scion-agent.json: %w", err)
	}

	return nil
}

func (g *GeminiCLI) updateSelectedAuthType(settingsPath string, selectedType string) error {
	var settings map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}

	if _, ok := settings["security"]; !ok {
		settings["security"] = make(map[string]interface{})
	}
	sec := settings["security"].(map[string]interface{})

	if _, ok := sec["auth"]; !ok {
		sec["auth"] = make(map[string]interface{})
	}
	auth := sec["auth"].(map[string]interface{})

	if current, _ := auth["selectedType"].(string); current == selectedType {
		return nil
	}

	auth["selectedType"] = selectedType
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(settingsPath, data, 0644)
}

func (g *GeminiCLI) GetEmbedDir() string {
	return "gemini"
}

func (g *GeminiCLI) GetInterruptKey() string {
	return "C-c"
}
