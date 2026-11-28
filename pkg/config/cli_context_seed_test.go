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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/clicontextseed"
)

// TestShellCLIContextSeed_MatchesCLIContextSchema is the §4.4 schema-drift
// guard (ptone/scion#9743, ptone/scion#9741): both the plain-endpoint and
// proxy-mode transport context.yaml seeds rendered by
// pkg/config/clicontextseed must parse cleanly with parseCLIContext and
// LoadCLIContext and produce CurrentHubEntry().Endpoint ==
// NormalizeEndpoint(rawEndpoint) with the same derived hub nickname and
// matching HubTransportConfig.
func TestShellCLIContextSeed_MatchesCLIContextSchema(t *testing.T) {
	endpoints := []string{
		"https://hub.example.com",
		"HTTPS://Prod.Example.COM:443///",
		"http://localhost:9810/",
		"http://127.0.0.1:8080",
	}

	for _, raw := range endpoints {
		t.Run("plain/"+raw, func(t *testing.T) {
			data, err := clicontextseed.Render(clicontextseed.Options{Endpoint: raw})
			if err != nil {
				t.Fatalf("clicontextseed.Render(%q): %v", raw, err)
			}

			c, err := parseCLIContext(data)
			if err != nil {
				t.Fatalf("parseCLIContext failed on seed for %q:\n%s\nerr: %v", raw, data, err)
			}
			if c.Version != CLIContextVersion {
				t.Errorf("Version = %d, want %d", c.Version, CLIContextVersion)
			}
			wantNick := DeriveHubName(raw)
			if c.CurrentHub != wantNick {
				t.Errorf("CurrentHub = %q, want %q", c.CurrentHub, wantNick)
			}
			entry := c.CurrentHubEntry()
			if entry == nil {
				t.Fatalf("CurrentHubEntry() is nil for %+v", c)
			}
			wantEndpoint := NormalizeEndpoint(raw)
			if entry.Endpoint != wantEndpoint {
				t.Errorf("CurrentHubEntry().Endpoint = %q, want %q", entry.Endpoint, wantEndpoint)
			}
			if entry.Transport != nil {
				t.Errorf("plain-endpoint seed must leave Transport nil, got %+v", entry.Transport)
			}
		})
	}

	proxyCases := []struct {
		name     string
		endpoint string
		mode     string
		audience string
	}{
		{
			name:     "iap_with_audience",
			endpoint: "HTTPS://IAP.Example.COM:443/",
			mode:     "iap",
			audience: "1234567890-abc.apps.googleusercontent.com",
		},
		{
			name:     "cloudrun_invoker_with_audience",
			endpoint: "https://hub-abc-uc.a.run.app/",
			mode:     "cloudrun_invoker",
			audience: "https://hub-abc-uc.a.run.app",
		},
		{
			name:     "iap_without_audience",
			endpoint: "https://iap-no-aud.example.com",
			mode:     "iap",
			audience: "",
		},
	}

	for _, tc := range proxyCases {
		t.Run("proxy/"+tc.name, func(t *testing.T) {
			data, err := clicontextseed.Render(clicontextseed.Options{
				Endpoint: tc.endpoint,
				Transport: &clicontextseed.TransportOptions{
					Mode:     tc.mode,
					Audience: tc.audience,
				},
			})
			if err != nil {
				t.Fatalf("clicontextseed.Render: %v", err)
			}

			// Verify via parseCLIContext and also through the real LoadCLIContext
			// file loader (which runs rejectHubSecrets and schema validation).
			home := t.TempDir()
			t.Setenv("HOME", home)
			scionDir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(scionDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scionDir, "context.yaml"), data, 0o600); err != nil {
				t.Fatal(err)
			}

			c, err := LoadCLIContext()
			if err != nil {
				t.Fatalf("LoadCLIContext failed on proxy seed:\n%s\nerr: %v", data, err)
			}
			if c.Version != CLIContextVersion {
				t.Errorf("Version = %d, want %d", c.Version, CLIContextVersion)
			}
			wantNick := DeriveHubName(tc.endpoint)
			if c.CurrentHub != wantNick {
				t.Errorf("CurrentHub = %q, want %q", c.CurrentHub, wantNick)
			}
			entry := c.CurrentHubEntry()
			if entry == nil {
				t.Fatalf("CurrentHubEntry() is nil for %+v", c)
			}
			wantEndpoint := NormalizeEndpoint(tc.endpoint)
			if entry.Endpoint != wantEndpoint {
				t.Errorf("CurrentHubEntry().Endpoint = %q, want %q", entry.Endpoint, wantEndpoint)
			}
			if entry.Transport == nil {
				t.Fatalf("CurrentHubEntry().Transport is nil, want {Mode:%q Audience:%q}", tc.mode, tc.audience)
			}
			if entry.Transport.Mode != tc.mode || entry.Transport.Audience != tc.audience {
				t.Errorf("CurrentHubEntry().Transport = %+v, want {Mode:%q Audience:%q}", *entry.Transport, tc.mode, tc.audience)
			}
		})
	}
}
