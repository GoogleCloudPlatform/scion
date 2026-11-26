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
	"reflect"
	"strings"
	"testing"
)

func writeAgentPolicyGlobalSettings(t *testing.T, doc string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if doc != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const agentPolicyFullDoc = `schema_version: "1"
agent_policy:
  allowed_image_prefixes:
    - registry.example.com/approved/
  max_resources:
    cpu: "8"
    memory: 8Gi
`

func TestLoadAgentPolicy_FromGlobalSettings(t *testing.T) {
	writeAgentPolicyGlobalSettings(t, agentPolicyFullDoc)
	p, err := LoadAgentPolicy()
	if err != nil {
		t.Fatalf("LoadAgentPolicy: %v", err)
	}
	if p == nil {
		t.Fatal("LoadAgentPolicy returned no policy")
	}
	if p.MaxResources == nil || p.MaxResources.CPU != "8" || p.MaxResources.Memory != "8Gi" ||
		len(p.AllowedImagePrefixes) != 1 || p.AllowedImagePrefixes[0] != "registry.example.com/approved/" {
		t.Fatalf("policy not loaded as written: %+v", p)
	}
}

func TestLoadAgentPolicy_AbsentIsNil(t *testing.T) {
	writeAgentPolicyGlobalSettings(t, "schema_version: \"1\"\n")
	p, err := LoadAgentPolicy()
	if err != nil || p != nil {
		t.Fatalf("no policy: got %+v, %v", p, err)
	}
	writeAgentPolicyGlobalSettings(t, "")
	p, err = LoadAgentPolicy()
	if err != nil || p != nil {
		t.Fatalf("no settings file: got %+v, %v", p, err)
	}
	writeAgentPolicyGlobalSettings(t, "schema_version: \"1\"\nagent_policy: {}\n")
	p, err = LoadAgentPolicy()
	if err != nil || p != nil {
		t.Fatalf("empty policy: got %+v, %v", p, err)
	}
}

func TestLoadAgentPolicy_InvalidFailsClosed(t *testing.T) {
	writeAgentPolicyGlobalSettings(t, "schema_version: \"1\"\nagent_policy:\n  max_resources:\n    cpu: \"0\"\n")
	if _, err := LoadAgentPolicy(); err == nil {
		t.Fatal("an invalid policy must be an error, not no policy")
	}
}

// A misspelled key is an error, not a rule silently left out.
func TestLoadAgentPolicy_UnknownKeyFailsClosed(t *testing.T) {
	for name, doc := range map[string]string{
		"only a typo":    "schema_version: \"1\"\nagent_policy:\n  max_resource:\n    cpu: \"8\"\n",
		"typo beside ok": "schema_version: \"1\"\nagent_policy:\n  allowed_image_prefixes: [registry.example.com/]\n  allowed_image_prefix: [other]\n",
		"nested typo":    "schema_version: \"1\"\nagent_policy:\n  max_resources:\n    cpus: \"8\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			writeAgentPolicyGlobalSettings(t, doc)
			if _, err := LoadAgentPolicy(); err == nil {
				t.Fatal("an unknown agent_policy key must be an error")
			}
		})
	}
}

// agentPolicyKnownKeys must list exactly the yaml keys of V1AgentPolicy
// and its nested blocks.
func TestAgentPolicyKnownKeys_MatchStruct(t *testing.T) {
	keysOf := func(typ reflect.Type) map[string]bool {
		out := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
			out[name] = true
		}
		return out
	}
	top := keysOf(reflect.TypeOf(V1AgentPolicy{}))
	if len(top) != len(agentPolicyKnownKeys) {
		t.Fatalf("struct keys %v, known keys %v", top, agentPolicyKnownKeys)
	}
	for k := range top {
		if _, ok := agentPolicyKnownKeys[k]; !ok {
			t.Errorf("agentPolicyKnownKeys lacks %q", k)
		}
	}
	if !reflect.DeepEqual(keysOf(reflect.TypeOf(V1AgentPolicyResources{})), agentPolicyKnownKeys["max_resources"]) {
		t.Error("max_resources keys differ from V1AgentPolicyResources")
	}
}

func TestLoadAgentPolicy_UnloadableMentioningPolicyFailsClosed(t *testing.T) {
	writeAgentPolicyGlobalSettings(t, "schema_version: \"1\"\nagent_policy: [this is: not valid\n")
	if _, err := LoadAgentPolicy(); err == nil {
		t.Fatal("a broken settings file that mentions agent_policy must be an error")
	}
}

func TestLoadAgentPolicy_LegacyFormatNotSilentlyDropped(t *testing.T) {
	writeAgentPolicyGlobalSettings(t, "harnesses:\n  claude:\n    image: test\nagent_policy:\n  allowed_image_prefixes:\n    - registry.example.com/approved/\n")
	p, err := LoadAgentPolicy()
	if err == nil && (p == nil || len(p.AllowedImagePrefixes) != 1) {
		t.Fatalf("a legacy settings file with agent_policy must enforce it or fail closed, got p=%+v err=%v", p, err)
	}
}

// The policy is read from the global settings file only: SCION_*
// environment variables cannot set or loosen it, and a project's settings
// never contribute.
func TestLoadAgentPolicy_IgnoresEnvAndProject(t *testing.T) {
	dir := writeAgentPolicyGlobalSettings(t, "schema_version: \"1\"\nagent_policy:\n  max_resources:\n    cpu: \"4\"\n")
	t.Setenv("SCION_AGENT_POLICY_MAX_RESOURCES_CPU", "64")
	t.Setenv("SCION_AGENT_POLICY_MAXRESOURCES_CPU", "64")
	project := filepath.Join(filepath.Dir(dir), "proj", ".scion")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "settings.yaml"),
		[]byte("schema_version: \"1\"\nagent_policy:\n  max_resources:\n    cpu: \"64\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(project))
	p, err := LoadAgentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.MaxResources == nil || p.MaxResources.CPU != "4" {
		t.Fatalf("policy loosened by env or project: %+v", p)
	}
}

func TestAgentPolicyValidate(t *testing.T) {
	bad := []struct {
		name string
		p    V1AgentPolicy
		want string
	}{
		{"empty prefix", V1AgentPolicy{AllowedImagePrefixes: []string{""}}, "allowed_image_prefixes[0]"},
		{"whitespace prefix", V1AgentPolicy{AllowedImagePrefixes: []string{"registry.example.com/a b"}}, "whitespace"},
		{"scheme prefix", V1AgentPolicy{AllowedImagePrefixes: []string{"https://registry.example.com/x"}}, "URL scheme"},
		{"leading slash prefix", V1AgentPolicy{AllowedImagePrefixes: []string{"/registry.example.com/x"}}, "starts with"},
		{"bad cpu", V1AgentPolicy{MaxResources: &V1AgentPolicyResources{CPU: "lots"}}, "max_resources.cpu"},
		{"zero cpu", V1AgentPolicy{MaxResources: &V1AgentPolicyResources{CPU: "0"}}, "positive"},
		{"ambiguous memory", V1AgentPolicy{MaxResources: &V1AgentPolicyResources{Memory: "512m"}}, "read differently"},
		{"docker-only memory", V1AgentPolicy{MaxResources: &V1AgentPolicyResources{Memory: "8g"}}, "max_resources.memory"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	good := V1AgentPolicy{
		AllowedImagePrefixes: []string{"registry.example.com/approved/", "ghcr.io/org/image"},
		MaxResources:         &V1AgentPolicyResources{CPU: "500m", Memory: "1.5Gi"},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid policy refused: %v", err)
	}
}

func TestAgentPolicyQuantities(t *testing.T) {
	if v, err := AgentPolicyCPUMax("500m"); err != nil || v != 0.5 {
		t.Errorf("cpu 500m = %v, %v", v, err)
	}
	if v, err := AgentPolicyMemoryMax("8Gi"); err != nil || v != 8*(1<<30) {
		t.Errorf("memory 8Gi = %v, %v", v, err)
	}
	// An agent's value takes its largest reading: "512m" is 512 MB to the
	// docker parser, so it is compared as 512 MB, not half a byte.
	if v, err := AgentPolicyMemoryValue("512m"); err != nil || v < 500e6 {
		t.Errorf("memory 512m = %v, %v; want the 512 MB reading", v, err)
	}
	// A docker-only spelling still parses for an agent value.
	if v, err := AgentPolicyMemoryValue("64g"); err != nil || v != 64e9 {
		t.Errorf("memory 64g = %v, %v", v, err)
	}
	if _, err := AgentPolicyCPUValue("not-a-number"); err == nil {
		t.Error("an unparsable cpu value must be an error")
	}
}

func TestSettingsSchema_AgentPolicy(t *testing.T) {
	errs, err := ValidateSettings([]byte(agentPolicyFullDoc), "1")
	if err != nil || len(errs) != 0 {
		t.Fatalf("agent_policy: errs=%v err=%v", errs, err)
	}
	errs, err = ValidateSettings([]byte("schema_version: \"1\"\nagent_policy:\n  max_resource:\n    cpu: \"8\"\n"), "1")
	if err != nil || len(errs) == 0 {
		t.Fatalf("misspelled agent_policy key accepted: errs=%v err=%v", errs, err)
	}
	errs, err = ValidateSettings([]byte("schema_version: \"1\"\nagent_policy:\n  max_resources:\n    cpus: \"8\"\n"), "1")
	if err != nil || len(errs) == 0 {
		t.Fatalf("misspelled max_resources key accepted: errs=%v err=%v", errs, err)
	}
}
