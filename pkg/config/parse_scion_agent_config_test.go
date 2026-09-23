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

import "testing"

func TestParseScionAgentConfig(t *testing.T) {
	yamlCfg, err := ParseScionAgentConfig("scion-agent.yaml", []byte(
		"harness-config: claude\nskills:\n  - uri: skill://scion/global/a@latest\n  - uri: gh://o/r/s@main\n    optional: true\n"))
	if err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if yamlCfg.HarnessConfig != "claude" {
		t.Errorf("hyphenated keys should be normalized, got HarnessConfig=%q", yamlCfg.HarnessConfig)
	}
	if len(yamlCfg.Skills) != 2 || yamlCfg.Skills[0].URI != "skill://scion/global/a@latest" || !yamlCfg.Skills[1].Optional {
		t.Errorf("unexpected skills: %+v", yamlCfg.Skills)
	}

	jsonCfg, err := ParseScionAgentConfig("scion-agent.json", []byte(`{"skills":[{"uri":"b"}]}`))
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(jsonCfg.Skills) != 1 || jsonCfg.Skills[0].URI != "b" {
		t.Errorf("unexpected json skills: %+v", jsonCfg.Skills)
	}

	if _, err := ParseScionAgentConfig("scion-agent.yaml", []byte("skills: [")); err == nil {
		t.Error("expected error for malformed YAML")
	}
}
