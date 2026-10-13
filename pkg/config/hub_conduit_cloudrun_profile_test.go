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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestConduitShutdownGrace_CloudRunProfiles: both Cloud Run hub settings
// profiles set server.hub.conduit.shutdown_grace to Cloud Run's 10s
// SIGTERM-to-kill window, and the value passes validation.
func TestConduitShutdownGrace_CloudRunProfiles(t *testing.T) {
	for _, rel := range []string{
		"scripts/cloudrun/hub-settings-template.yaml",
		"deploy/terraform/modules/hub-cloudrun/templates/settings.yaml.tftpl",
	} {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
			require.NoError(t, err)
			// Drop Terraform template directives (%{ ... }), which are
			// not YAML.
			var lines []string
			for _, l := range strings.Split(string(data), "\n") {
				if !strings.HasPrefix(strings.TrimSpace(l), "%{") {
					lines = append(lines, l)
				}
			}
			var doc struct {
				Server struct {
					Hub struct {
						Conduit struct {
							ShutdownGrace string `yaml:"shutdown_grace"`
						} `yaml:"conduit"`
					} `yaml:"hub"`
				} `yaml:"server"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &doc))
			assert.Equal(t, "10s", doc.Server.Hub.Conduit.ShutdownGrace)
			assert.NoError(t, HubConduitConfig{ShutdownGrace: doc.Server.Hub.Conduit.ShutdownGrace}.Validate())
		})
	}
}
