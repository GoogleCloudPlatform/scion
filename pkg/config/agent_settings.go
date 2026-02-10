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

package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/ptone/scion-agent/pkg/util"
)

type AgentSettings struct {
	ApiKey   string `json:"apiKey"`
	Security struct {
		Auth struct {
			SelectedType string `json:"selectedType"`
		} `json:"auth"`
	} `json:"security"`
	Tools struct {
		Sandbox interface{} `json:"sandbox"`
	} `json:"tools"`
}

func LoadAgentSettings(path string) (*AgentSettings, error) {
	var settings AgentSettings
	if err := util.ReadJSONC(path, &settings); err != nil {
		return nil, err
	}

	return &settings, nil
}

func SaveAgentSettings(path string, settings *AgentSettings) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

func GetAgentSettings() (*AgentSettings, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(home, ".gemini", "settings.json")
	return LoadAgentSettings(path)
}
