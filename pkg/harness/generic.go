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

type Generic struct{}

func (g *Generic) SeedTemplateDir(templateDir string, force bool) error {
	return config.SeedCommonFiles(templateDir, "common", g.GetEmbedDir(), g.DefaultConfigDir(), force)
}

func (g *Generic) Name() string {
	return "generic"
}

func (g *Generic) DiscoverAuth(agentHome string) api.AuthConfig {
	auth := api.AuthConfig{
		GoogleAppCredentials: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		GoogleCloudProject:   os.Getenv("GOOGLE_CLOUD_PROJECT"),
	}

	if auth.GoogleCloudProject == "" {
		auth.GoogleCloudProject = os.Getenv("GCP_PROJECT")
	}

	// Check agent settings (from template)
	agentSettingsPath := filepath.Join(agentHome, g.DefaultConfigDir(), "settings.json")
	if agentSettings, err := config.LoadAgentSettings(agentSettingsPath); err == nil {
		if auth.GeminiAPIKey == "" && auth.GoogleAPIKey == "" && agentSettings.ApiKey != "" {
			// Determine where to put the API key.
			// Since we don't know the harness, we might not be able to assign it correctly
			// if it's not one of the known ones.
			// However, AgentSettings struct is somewhat tailored to Gemini currently.
			// We'll leave it as is for now or maybe try to guess?
			// For generic, if ApiKey is there, maybe we put it in GeminiAPIKey as a fallback,
			// or maybe we need a GenericAPIKey field in AuthConfig?
			// Given AuthConfig limitations, we'll skip assigning it to a specific field
			// if we are not sure, or default to one if it seems appropriate.
			// But for "generic", maybe we just ignore settings.json specific keys unless we know what they are.
		}
	}

	// Check for OAuth creds in default location
	home, _ := os.UserHomeDir()
	oauthPath := filepath.Join(home, g.DefaultConfigDir(), "oauth_creds.json")
	if _, err := os.Stat(oauthPath); err == nil {
		auth.OAuthCreds = oauthPath
	}

	return auth
}

func (g *Generic) GetEnv(agentName string, agentHome string, unixUsername string, auth api.AuthConfig) map[string]string {
	env := make(map[string]string)

	env["SCION_AGENT_NAME"] = agentName

	// Map AuthConfig back to standard env vars
	if auth.AnthropicAPIKey != "" {
		env["ANTHROPIC_API_KEY"] = auth.AnthropicAPIKey
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
	if auth.GoogleCloudProject != "" {
		env["GOOGLE_CLOUD_PROJECT"] = auth.GoogleCloudProject
	}

	if auth.GoogleAppCredentials != "" {
		env["GOOGLE_APPLICATION_CREDENTIALS"] = fmt.Sprintf("%s/.config/gcp/application_default_credentials.json", util.GetHomeDir(unixUsername))
	}

	// We don't set GEMINI_DEFAULT_AUTH_TYPE as that is vendor specific

	return env
}

func (g *Generic) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := append([]string{}, baseArgs...)
	if task != "" {
		args = append(args, task)
	}
	return args
}

func (g *Generic) PropagateFiles(homeDir, unixUsername string, auth api.AuthConfig) error {
	if homeDir == "" {
		return nil
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

func (g *Generic) GetVolumes(unixUsername string, auth api.AuthConfig) []api.VolumeMount {
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

func (g *Generic) DefaultConfigDir() string {
	return ".scion"
}

func (g *Generic) HasSystemPrompt(agentHome string) bool {
	return false
}

func (g *Generic) Provision(ctx context.Context, agentName, agentHome, agentWorkspace string) error {
	return nil
}

func (g *Generic) GetEmbedDir() string {
	return ""
}

func (g *Generic) GetInterruptKey() string {
	return "C-c"
}
