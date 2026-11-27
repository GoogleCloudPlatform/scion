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

package cmd

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestMCPDefaultDisabled(t *testing.T) {
	gc := config.DefaultGlobalConfig()
	if gc.MCP.Enabled {
		t.Fatal("expected DefaultGlobalConfig().MCP.Enabled to be false")
	}
}

func TestValidateMCPStartup(t *testing.T) {
	if err := validateMCPStartup(&config.GlobalConfig{}, false); err != nil {
		t.Fatalf("disabled MCP without hub should succeed, got: %v", err)
	}
	if err := validateMCPStartup(&config.GlobalConfig{MCP: config.MCPServerConfig{Enabled: true}}, true); err != nil {
		t.Fatalf("enabled MCP with hub should succeed, got: %v", err)
	}
	if err := validateMCPStartup(&config.GlobalConfig{MCP: config.MCPServerConfig{Enabled: true}}, false); err == nil {
		t.Fatal("expected error when MCP is enabled without hub in P1")
	}
}

func TestV1ServerMCPRoundTrip(t *testing.T) {
	v1 := &config.V1ServerConfig{
		MCP: &config.V1ServerMCPConfig{
			Enabled:                true,
			Host:                   "127.0.0.1",
			Port:                   9821,
			Path:                   "/custom-mcp",
			PublicURL:              "https://mcp.example.com",
			HubEndpoint:            "https://hub.example.com",
			AllowedOrigins:         []string{"https://app.example.com"},
			MaxStreamsPerPrincipal: 12,
			StreamWait:             "45s",
		},
	}

	gc := config.ConvertV1ServerToGlobalConfig(v1)
	if !gc.MCP.Enabled || gc.MCP.Host != "127.0.0.1" || gc.MCP.Port != 9821 || gc.MCP.Path != "/custom-mcp" {
		t.Fatalf("unexpected GlobalConfig.MCP: %+v", gc.MCP)
	}
	if gc.MCP.PublicURL != "https://mcp.example.com" || gc.MCP.HubEndpoint != "https://hub.example.com" {
		t.Fatalf("unexpected URLs: %+v", gc.MCP)
	}
	if gc.MCP.MaxStreamsPerPrincipal != 12 || gc.MCP.StreamWait != 45*time.Second {
		t.Fatalf("unexpected stream settings: %+v", gc.MCP)
	}
	if len(gc.MCP.AllowedOrigins) != 1 || gc.MCP.AllowedOrigins[0] != "https://app.example.com" {
		t.Fatalf("unexpected AllowedOrigins: %+v", gc.MCP.AllowedOrigins)
	}

	back := config.ConvertGlobalToV1ServerConfig(gc)
	if back.MCP == nil {
		t.Fatal("expected back.MCP to be non-nil")
	}
	if !back.MCP.Enabled || back.MCP.Port != 9821 || back.MCP.StreamWait != "45s" {
		t.Fatalf("unexpected round-tripped V1ServerMCPConfig: %+v", back.MCP)
	}
}
