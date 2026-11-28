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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/clicontextseed"
)

// TestShellCLIContextSeed_MatchesCLIContextSchema is the §4.4 schema-drift
// guard (ptone/scion#9743): the plain-endpoint context.yaml seed rendered by
// pkg/config/clicontextseed must parse cleanly with parseCLIContext and produce
// CurrentHubEntry().Endpoint == NormalizeEndpoint(rawEndpoint) with the same
// derived hub nickname.
func TestShellCLIContextSeed_MatchesCLIContextSchema(t *testing.T) {
	endpoints := []string{
		"https://hub.example.com",
		"HTTPS://Prod.Example.COM:443///",
		"http://localhost:9810/",
		"http://127.0.0.1:8080",
	}

	for _, raw := range endpoints {
		t.Run(raw, func(t *testing.T) {
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
				t.Errorf("P1 plain-endpoint seed must leave Transport nil, got %+v", entry.Transport)
			}
		})
	}
}
