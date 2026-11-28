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
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestGeneric_Name(t *testing.T) {
	g := &Generic{}
	if g.Name() != "generic" {
		t.Errorf("Expected name 'generic', got '%s'", g.Name())
	}
}

func TestGeneric_GetEnv(t *testing.T) {
	g := &Generic{}

	env := g.GetEnv("test-agent", "", "test-user")

	// GetEnv should only return non-auth env vars
	if env["SCION_AGENT_NAME"] != "test-agent" {
		t.Errorf("Expected SCION_AGENT_NAME = 'test-agent', got '%s'", env["SCION_AGENT_NAME"])
	}
}

func TestGeneric_GetCommand(t *testing.T) {
	g := &Generic{}

	// Default exec form: without baseArgs, task is a single argv element (no implicit sh -c).
	cmd := g.GetCommand("sleep infinity", false, nil)
	if !reflect.DeepEqual(cmd, []string{"sleep infinity"}) {
		t.Errorf("Expected exec-form command ['sleep infinity'], got %v", cmd)
	}

	// Explicit shell-form opt-in via baseArgs ["sh", "-c"].
	cmdShellOptIn := g.GetCommand("npm test && echo ok", false, []string{"sh", "-c"})
	if !reflect.DeepEqual(cmdShellOptIn, []string{"sh", "-c", "npm test && echo ok"}) {
		t.Errorf("Expected shell opt-in command [sh -c 'npm test && echo ok'], got %v", cmdShellOptIn)
	}

	// Exec form with explicit command baseArgs keeps task as a single argv element.
	cmdWithArgs := g.GetCommand("test task", false, []string{"--arg1"})
	if !reflect.DeepEqual(cmdWithArgs, []string{"--arg1", "test task"}) {
		t.Errorf("Expected command ['--arg1', 'test task'], got %v", cmdWithArgs)
	}

	cmdEmpty := g.GetCommand("", false, nil)
	if len(cmdEmpty) != 0 {
		t.Errorf("Expected empty command, got %v", cmdEmpty)
	}
}

func TestDeclarativeGenericHarness_GetCommand(t *testing.T) {
	cases := []struct {
		name     string
		entry    config.HarnessConfigEntry
		task     string
		resume   bool
		baseArgs []string
		want     []string
	}{
		{
			name:  "nil command uses exec form",
			entry: config.HarnessConfigEntry{Harness: "generic"},
			task:  "npm test && echo ok",
			want:  []string{"npm test && echo ok"},
		},
		{
			name: "explicit sh -c opt-in via command.base",
			entry: config.HarnessConfigEntry{
				Harness: "generic",
				Command: &config.HarnessCommandConfig{Base: []string{"sh", "-c"}},
			},
			task: "npm test && echo ok",
			want: []string{"sh", "-c", "npm test && echo ok"},
		},
		{
			name: "empty command.base uses exec form",
			entry: config.HarnessConfigEntry{
				Harness: "generic",
				Command: &config.HarnessCommandConfig{},
			},
			task: "sleep infinity",
			want: []string{"sleep infinity"},
		},
		{
			name: "empty command.base with task_flag",
			entry: config.HarnessConfigEntry{
				Harness: "generic",
				Command: &config.HarnessCommandConfig{TaskFlag: "--x"},
			},
			task: "npm test && echo ok",
			want: []string{"--x", "npm test && echo ok"},
		},
		{
			name: "empty command.base with baseArgs",
			entry: config.HarnessConfigEntry{
				Harness: "generic",
				Command: &config.HarnessCommandConfig{},
			},
			task:     "npm test && echo ok",
			baseArgs: []string{"python3", "agent.py"},
			want:     []string{"python3", "agent.py", "npm test && echo ok"},
		},
		{
			name: "empty command.base with resume_flag",
			entry: config.HarnessConfigEntry{
				Harness: "generic",
				Command: &config.HarnessCommandConfig{ResumeFlag: "--resume"},
			},
			task:   "npm test && echo ok",
			resume: true,
			want:   []string{"--resume", "npm test && echo ok"},
		},
		{
			name: "command.base with task_flag",
			entry: config.HarnessConfigEntry{
				Harness: "custom",
				Command: &config.HarnessCommandConfig{Base: []string{"my-agent"}, TaskFlag: "--task"},
			},
			task:     "do work",
			baseArgs: []string{"--verbose"},
			want:     []string{"my-agent", "--verbose", "--task", "do work"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDeclarativeGenericHarness(tc.entry)
			got := d.GetCommand(tc.task, tc.resume, tc.baseArgs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("GetCommand() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSupportsModelFlag(t *testing.T) {
	cases := []struct {
		name string
		h    api.Harness
		want bool
	}{
		{"plain Generic", &Generic{}, false},
		{"Shell", &Shell{}, false},
		{"declarative generic without command", NewDeclarativeGenericHarness(config.HarnessConfigEntry{Harness: "generic"}), false},
		{"declarative generic with sh -c", NewDeclarativeGenericHarness(config.HarnessConfigEntry{
			Harness: "generic",
			Command: &config.HarnessCommandConfig{Base: []string{"sh", "-c"}},
		}), false},
		{"declarative generic with custom base", NewDeclarativeGenericHarness(config.HarnessConfigEntry{
			Harness: "generic",
			Command: &config.HarnessCommandConfig{Base: []string{"python3", "worker.py"}},
		}), false},
		{"declarative custom without command.base", NewDeclarativeGenericHarness(config.HarnessConfigEntry{
			Harness: "custom-cli",
		}), false},
		{"declarative custom with /bin/bash base", NewDeclarativeGenericHarness(config.HarnessConfigEntry{
			Harness: "custom-shell",
			Command: &config.HarnessCommandConfig{Base: []string{"/bin/bash", "-lc"}},
		}), false},
		{"declarative custom with non-shell command.base", NewDeclarativeGenericHarness(config.HarnessConfigEntry{
			Harness: "custom-cli",
			Command: &config.HarnessCommandConfig{Base: []string{"custom-cli"}},
		}), true},
		{"container-script harness", &ContainerScriptHarness{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SupportsModelFlag(tc.h); got != tc.want {
				t.Errorf("SupportsModelFlag(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestGeneric_ValidateCommand(t *testing.T) {
	// Isolate HOME and CWD so Resolve("generic") uses the synthetic built-in
	// entry even if the developer host has ~/.scion/harness-configs/generic.
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	resolvedGeneric, err := Resolve(context.Background(), ResolveOptions{Name: "generic"})
	if err != nil {
		t.Fatalf("Resolve(generic): %v", err)
	}
	if err := ValidateCommand(resolvedGeneric.Harness, "", false, nil); !errors.Is(err, ErrEmptyGenericCommand) {
		t.Fatalf("ValidateCommand(built-in generic, empty) = %v, want ErrEmptyGenericCommand", err)
	}
	if err := ValidateCommand(resolvedGeneric.Harness, "", true, nil); !errors.Is(err, ErrEmptyGenericCommand) {
		t.Fatalf("ValidateCommand(built-in generic, empty resume) = %v, want ErrEmptyGenericCommand", err)
	}
	if err := ValidateCommand(resolvedGeneric.Harness, "echo hi", false, nil); err != nil {
		t.Errorf("ValidateCommand(built-in generic, with task) = %v, want nil", err)
	}
	if err := ValidateCommand(resolvedGeneric.Harness, "", false, []string{"tail", "-f", "/dev/null"}); err != nil {
		t.Errorf("ValidateCommand(built-in generic, with baseArgs) = %v, want nil", err)
	}

	// Built-in generic with a settings overlay adding declarative metadata
	// (e.g. ready) still rejects an empty command.
	withReadySettings := &config.VersionedSettings{
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"generic": {
				Harness: "generic",
				Ready:   &config.HarnessReadyConfig{Strategy: config.ReadyStrategyDelay},
			},
		},
	}
	resolvedDeclBuiltin, err := Resolve(context.Background(), ResolveOptions{
		Name:     "generic",
		Settings: withReadySettings,
	})
	if err != nil {
		t.Fatalf("Resolve(generic with ready overlay): %v", err)
	}
	if err := ValidateCommand(resolvedDeclBuiltin.Harness, "", false, nil); !errors.Is(err, ErrEmptyGenericCommand) {
		t.Fatalf("ValidateCommand(built-in declarative generic, empty) = %v, want ErrEmptyGenericCommand", err)
	}

	// Non-builtin Generic / DeclarativeGenericHarness instances and all
	// bundled harnesses are unaffected by early command validation.
	if err := ValidateCommand(&Generic{}, "", false, nil); err != nil {
		t.Errorf("ValidateCommand(non-builtin Generic) = %v, want nil", err)
	}
	declCustom := NewDeclarativeGenericHarness(config.HarnessConfigEntry{Harness: "generic", SkillsDir: ".skills"})
	if err := ValidateCommand(declCustom, "", false, nil); err != nil {
		t.Errorf("ValidateCommand(custom declarative generic) = %v, want nil", err)
	}
	for _, name := range AllHarnessNames() {
		h := New(name)
		if err := ValidateCommand(h, "", false, nil); err != nil {
			t.Errorf("ValidateCommand(built-in %q) = %v, want nil", name, err)
		}
	}

	if err := ValidateEntryCommand(config.HarnessConfigSourceBuiltin, config.HarnessConfigEntry{Harness: "generic"}, "", false, nil); !errors.Is(err, ErrEmptyGenericCommand) {
		t.Errorf("ValidateEntryCommand(builtin generic empty) = %v, want ErrEmptyGenericCommand", err)
	}
	if err := ValidateEntryCommand(config.HarnessConfigSourceBuiltin, config.HarnessConfigEntry{Harness: "generic"}, "echo hi", false, nil); err != nil {
		t.Errorf("ValidateEntryCommand(builtin generic with task) = %v, want nil", err)
	}
	if err := ValidateEntryCommand(config.HarnessConfigSourceBrokerLocal, config.HarnessConfigEntry{Harness: "generic"}, "", false, nil); err != nil {
		t.Errorf("ValidateEntryCommand(broker-local generic empty) = %v, want nil", err)
	}
}

func TestGeneric_DefaultConfigDir(t *testing.T) {
	g := &Generic{}
	if g.DefaultConfigDir() != ".scion" {
		t.Errorf("Expected DefaultConfigDir '.scion', got '%s'", g.DefaultConfigDir())
	}
}

func TestGenericInjectAgentInstructions(t *testing.T) {
	agentHome := t.TempDir()
	g := &Generic{}
	content := []byte("# Agent Instructions\nDo good work.")

	if err := g.InjectAgentInstructions(agentHome, content); err != nil {
		t.Fatalf("InjectAgentInstructions failed: %v", err)
	}

	target := filepath.Join(agentHome, "agents.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}

func TestGenericResolveAuth_EmptyConfig(t *testing.T) {
	g := &Generic{}
	result, err := g.ResolveAuth(api.AuthConfig{})
	if err != nil {
		t.Fatalf("generic should never error: %v", err)
	}
	if result.Method != "passthrough" {
		t.Errorf("Method = %q, want %q", result.Method, "passthrough")
	}
	if len(result.EnvVars) != 0 {
		t.Errorf("expected empty EnvVars for empty config, got %v", result.EnvVars)
	}
	if len(result.Files) != 0 {
		t.Errorf("expected empty Files for empty config, got %d", len(result.Files))
	}
}

func TestGenericResolveAuth_AllCreds(t *testing.T) {
	g := &Generic{}
	auth := api.AuthConfig{
		GoogleCloudProject:   "proj",
		GoogleCloudRegion:    "region",
		GoogleAppCredentials: "/adc.json",
		EnvVars: map[string]string{
			"ANTHROPIC_API_KEY": "anthropic",
			"GEMINI_API_KEY":    "gemini",
			"GOOGLE_API_KEY":    "google",
			"OPENAI_API_KEY":    "openai",
			"CODEX_API_KEY":     "codex",
		},
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("generic should never error: %v", err)
	}

	expectedEnvVars := map[string]string{
		"ANTHROPIC_API_KEY":    "anthropic",
		"GEMINI_API_KEY":       "gemini",
		"GOOGLE_API_KEY":       "google",
		"OPENAI_API_KEY":       "openai",
		"CODEX_API_KEY":        "codex",
		"GOOGLE_CLOUD_PROJECT": "proj",
		"GOOGLE_CLOUD_REGION":  "region",
	}
	for k, want := range expectedEnvVars {
		if got := result.EnvVars[k]; got != want {
			t.Errorf("EnvVars[%q] = %q, want %q", k, got, want)
		}
	}

	// Should have 1 file mapping: ADC
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file mapping (ADC), got %d", len(result.Files))
	}
}

func TestGenericResolveAuth_PartialCreds(t *testing.T) {
	g := &Generic{}
	auth := api.AuthConfig{
		EnvVars: map[string]string{"ANTHROPIC_API_KEY": "key-only"},
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("generic should never error: %v", err)
	}
	if result.EnvVars["ANTHROPIC_API_KEY"] != "key-only" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want %q", result.EnvVars["ANTHROPIC_API_KEY"], "key-only")
	}
	if len(result.EnvVars) != 1 {
		t.Errorf("expected 1 env var, got %d: %v", len(result.EnvVars), result.EnvVars)
	}
	if len(result.Files) != 0 {
		t.Errorf("expected no files, got %d", len(result.Files))
	}
}

func TestGenericInjectSystemPrompt(t *testing.T) {
	agentHome := t.TempDir()
	g := &Generic{}
	content := []byte("You are a helpful coding assistant.")

	if err := g.InjectSystemPrompt(agentHome, content); err != nil {
		t.Fatalf("InjectSystemPrompt failed: %v", err)
	}

	target := filepath.Join(agentHome, ".scion", "system_prompt.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}
