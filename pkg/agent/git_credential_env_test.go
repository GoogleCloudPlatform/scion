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

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// All values below are fake placeholders.
const fakeHostCred = "fake-host-cred-0000"

func TestIsGitCredentialEnvKey(t *testing.T) {
	match := []string{
		"GITHUB_TOKEN", "github_token", "GH_TOKEN", "gh_token", "GH_ENTERPRISE_TOKEN",
		"GH_SOMEORG", "GH_SOME_ORG__SOME_REPO", "GH_", "GH_HOST", "GH_REPO",
	}
	for _, k := range match {
		if !IsGitCredentialEnvKey(k) {
			t.Errorf("IsGitCredentialEnvKey(%q) = false, want true", k)
		}
	}
	noMatch := []string{
		"", "GH", "GHTOKEN", "XGH_TOKEN", "MY_GITHUB_TOKEN", "GITHUB_TOKEN_X", "GITHUB_ACTIONS",
		"SCION_GITHUB_TOKEN_PATH", "SCION_GITHUB_APP_ENABLED", "SCION_GITHUB_TOKEN_EXPIRY",
		"SCION_USER_GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "ANTHROPIC_API_KEY", "PATH",
	}
	for _, k := range noMatch {
		if IsGitCredentialEnvKey(k) {
			t.Errorf("IsGitCredentialEnvKey(%q) = true, want false", k)
		}
	}
}

func TestGitCredentialsStripped_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		opts api.StartOptions
		want bool
	}{
		{"broker, absent", api.StartOptions{BrokerMode: true}, true},
		{"broker, allowed", api.StartOptions{BrokerMode: true, AllowGitCredentials: true}, false},
		{"local", api.StartOptions{}, false},
		{"local, allowed", api.StartOptions{AllowGitCredentials: true}, false},
	}
	for _, tc := range cases {
		if got := gitCredentialsStripped(tc.opts); got != tc.want {
			t.Errorf("%s: gitCredentialsStripped = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildAgentEnvWithPolicy_EmptyMarkerNeverReadsHost(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", fakeHostCred)
	t.Setenv("GH_TOKEN", fakeHostCred)
	t.Setenv("PLAIN_PASSTHROUGH", "plain-host-value")
	cfg := &api.ScionConfig{Env: map[string]string{
		"GITHUB_TOKEN":      "",
		"GH_TOKEN":          "",
		"REFERS_TO_CRED":    "${GITHUB_TOKEN}",
		"PLAIN_PASSTHROUGH": "",
	}}

	env, _, missing, _ := buildAgentEnvWithPolicy(cfg, nil, nil, true, true)
	got := envListToMap(env)
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "REFERS_TO_CRED"} {
		if _, ok := got[k]; ok {
			t.Errorf("env has %s, want it absent", k)
		}
	}
	for _, kv := range env {
		if strings.Contains(kv, fakeHostCred) {
			t.Errorf("env entry carries the host value: key %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	for _, k := range missing {
		if IsGitCredentialEnvKey(k) {
			t.Errorf("credential key %s reported as missing; it must be dropped silently", k)
		}
	}
	if got["PLAIN_PASSTHROUGH"] != "plain-host-value" {
		t.Errorf("non-credential passthrough = %q, want the host value", got["PLAIN_PASSTHROUGH"])
	}

	// Without the policy the same config pulls the host value (the
	// behaviour the policy changes).
	env, _, _, _ = buildAgentEnvWithPolicy(cfg, nil, nil, true, false)
	if envListToMap(env)["GITHUB_TOKEN"] != fakeHostCred {
		t.Fatalf("policy off: expected the empty marker to pass the host value through")
	}
}

func TestStripGitCredentialRunConfig_DoesNotMutateInputs(t *testing.T) {
	authEnv := map[string]string{"GH_TOKEN": "fake-a", "OTHER": "o"}
	secrets := []api.ResolvedSecret{
		{Name: "a", Type: "environment", Target: "GITHUB_TOKEN", Value: "fake-b"},
		{Name: "b", Type: "", Target: "GH_ORG", Value: "fake-c"},
		{Name: "c", Type: "file", Target: "/home/scion/GH_FILE", Value: "file-content"},
		{Name: "d", Type: "environment", Target: "KEEP_ME", Value: "k"},
	}
	cfg := runtime.RunConfig{
		Env:             []string{"GITHUB_TOKEN=fake-d", "KEEP=1", "gh_lower=fake-e"},
		ResolvedAuth:    &api.ResolvedAuth{Method: "m", EnvVars: authEnv},
		ResolvedSecrets: secrets,
	}
	origAuth := cfg.ResolvedAuth

	removed := stripGitCredentialRunConfig(&cfg)

	want := []string{"GH_ORG", "GH_TOKEN", "GITHUB_TOKEN", "gh_lower"}
	if strings.Join(removed, ",") != strings.Join(want, ",") {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	if strings.Join(cfg.Env, ",") != "KEEP=1" {
		t.Errorf("Env = %v, want [KEEP=1]", cfg.Env)
	}
	if _, ok := cfg.ResolvedAuth.EnvVars["GH_TOKEN"]; ok || cfg.ResolvedAuth.EnvVars["OTHER"] != "o" || cfg.ResolvedAuth.Method != "m" {
		t.Errorf("ResolvedAuth = %+v", cfg.ResolvedAuth)
	}
	if len(cfg.ResolvedSecrets) != 2 || cfg.ResolvedSecrets[0].Name != "c" || cfg.ResolvedSecrets[1].Name != "d" {
		t.Errorf("ResolvedSecrets = %+v, want the file secret and KEEP_ME", cfg.ResolvedSecrets)
	}
	if origAuth.EnvVars["GH_TOKEN"] != "fake-a" || len(secrets) != 4 {
		t.Errorf("inputs were mutated")
	}
}

// --- Start-level tests: one per env source -------------------------------

type gitCredFixture struct {
	hcDirEnv     map[string]string // broker-local harness-config directory env
	settingsEnv  map[string]string // broker settings harness_configs entry env
	templateEnv  map[string]string // broker-local template env
	agentCfgEnv  map[string]string // persisted scion-agent.json env (nil: no agent dir yet)
	authEnvKey   string            // harness-config auth required_env key, if any
	stagedSecret string            // pre-existing staged auth secret file name, if any
	// containerScript makes the harness a container-script harness, which
	// stages auth env values as files in the agent home.
	containerScript bool
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f gitCredFixture) setup(t *testing.T) (projectScionDir, agentDir string) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hc := map[string]any{"harness": "generic", "user": "scion", "image": "test-image:latest"}
	if f.hcDirEnv != nil {
		hc["env"] = f.hcDirEnv
	}
	if f.authEnvKey != "" {
		hc["auth"] = map[string]any{
			"default_type": "api-key",
			"types": map[string]any{
				"api-key": map[string]any{"required_env": []any{map[string]any{"any_of": []string{f.authEnvKey}}}},
			},
		}
	}
	if f.containerScript {
		hc["provisioner"] = map[string]any{"type": "container-script", "interface_version": 1, "command": []string{"true"}}
	}
	// YAML is a superset of JSON.
	writeJSONFile(t, filepath.Join(hcDir, "config.yaml"), hc)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tpl := map[string]any{"default_harness_config": "test-harness"}
	if f.templateEnv != nil {
		tpl["env"] = f.templateEnv
	}
	writeJSONFile(t, filepath.Join(tplDir, "scion-agent.json"), tpl)

	settings := map[string]any{
		"schema_version": "1",
		"active_profile": "local",
		"profiles":       map[string]any{"local": map[string]any{"runtime": "docker"}},
	}
	if f.settingsEnv != nil {
		settings["harness_configs"] = map[string]any{
			"test-harness": map[string]any{"harness": "generic", "env": f.settingsEnv},
		}
	}
	writeJSONFile(t, filepath.Join(globalScionDir, "settings.yaml"), settings)

	projectScionDir = filepath.Join(tmpDir, "project", ".scion")
	agentDir = filepath.Join(projectScionDir, "agents", "cred-agent")
	if f.agentCfgEnv != nil || f.stagedSecret != "" {
		if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSONFile(t, filepath.Join(agentDir, "scion-agent.json"), map[string]any{
			"harness": "generic", "harness_config": "test-harness", "env": f.agentCfgEnv,
		})
	} else if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if f.stagedSecret != "" {
		dir := filepath.Join(agentDir, "home", ".scion", "harness", "secrets")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f.stagedSecret), []byte("fake-staged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "OTHER_SECRET"), []byte("fake-other"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return projectScionDir, agentDir
}

// containerEnv is every env key=value the runtime would put in the
// container from cfg: Env, ResolvedAuth.EnvVars and environment-type
// ResolvedSecrets.
func containerEnv(cfg runtime.RunConfig) map[string]string {
	out := envListToMap(cfg.Env)
	if cfg.ResolvedAuth != nil {
		for k, v := range cfg.ResolvedAuth.EnvVars {
			out[k] = v
		}
	}
	for _, s := range cfg.ResolvedSecrets {
		if s.Type == "environment" || s.Type == "" {
			out[s.Target] = s.Value
		}
	}
	return out
}

func credKeys(env map[string]string) []string {
	var keys []string
	for k := range env {
		if IsGitCredentialEnvKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func runCredStart(t *testing.T, projectScionDir string, opts api.StartOptions) runtime.RunConfig {
	t.Helper()
	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	})
	opts.Name = "cred-agent"
	opts.ProjectPath = projectScionDir
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return captured
}

// assertNoCredentials fails if the container would get any matching key or
// the host placeholder value anywhere.
func assertNoCredentials(t *testing.T, cfg runtime.RunConfig) map[string]string {
	t.Helper()
	env := containerEnv(cfg)
	if keys := credKeys(env); len(keys) > 0 {
		t.Errorf("container env has credential keys %v, want none", keys)
	}
	for k, v := range env {
		if strings.Contains(v, fakeHostCred) {
			t.Errorf("container env key %s carries the host value", k)
		}
	}
	return env
}

func TestStart_GitCredentials_HostPassthroughStripped(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", fakeHostCred)
	t.Setenv("GH_TOKEN", fakeHostCred)
	t.Setenv("PLAIN_PASSTHROUGH", "plain-host-value")
	project, _ := gitCredFixture{
		hcDirEnv: map[string]string{"GITHUB_TOKEN": "", "GH_TOKEN": "", "PLAIN_PASSTHROUGH": ""},
	}.setup(t)

	env := assertNoCredentials(t, runCredStart(t, project, api.StartOptions{BrokerMode: true, NoAuth: true}))
	if env["PLAIN_PASSTHROUGH"] != "plain-host-value" {
		t.Errorf("non-credential passthrough = %q, want the host value", env["PLAIN_PASSTHROUGH"])
	}
}

func TestStart_GitCredentials_HostValueReferenceStripped(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", fakeHostCred)
	project, _ := gitCredFixture{
		agentCfgEnv: map[string]string{"REFERS_TO_CRED": "prefix-${GITHUB_TOKEN}", "KEEP": "kept"},
	}.setup(t)

	env := assertNoCredentials(t, runCredStart(t, project, api.StartOptions{BrokerMode: true, NoAuth: true}))
	// The reference expands as if the variable were unset.
	if got, ok := env["REFERS_TO_CRED"]; ok && got != "prefix-" {
		t.Errorf("REFERS_TO_CRED = %q, want %q", got, "prefix-")
	}
	if env["KEEP"] != "kept" {
		t.Errorf("KEEP = %q, want kept", env["KEEP"])
	}
}

func TestStart_GitCredentials_HubResolvedEnvStripped(t *testing.T) {
	project, _ := gitCredFixture{}.setup(t)
	env := assertNoCredentials(t, runCredStart(t, project, api.StartOptions{
		BrokerMode: true, NoAuth: true,
		Env: map[string]string{
			"GITHUB_TOKEN": "fake-hub-1", "GH_SOMEORG": "fake-hub-2",
			"SCION_GITHUB_TOKEN_PATH": "/tmp/x", "SCION_GITHUB_APP_ENABLED": "true",
			"SCION_GITHUB_TOKEN_EXPIRY": "2026-01-01T00:00:00Z", "SCION_USER_GITHUB_TOKEN": "true",
			"COPILOT_GITHUB_TOKEN": "fake-not-matched", "HUB_PLAIN": "p",
		},
	}))
	for _, k := range []string{"SCION_GITHUB_TOKEN_PATH", "SCION_GITHUB_APP_ENABLED", "SCION_GITHUB_TOKEN_EXPIRY"} {
		if _, ok := env[k]; ok {
			t.Errorf("credential-helper key %s present, want it removed", k)
		}
	}
	for k, want := range map[string]string{
		"COPILOT_GITHUB_TOKEN": "fake-not-matched", "HUB_PLAIN": "p", "SCION_USER_GITHUB_TOKEN": "true",
	} {
		if env[k] != want {
			t.Errorf("non-credential key %s = %q, want %q", k, env[k], want)
		}
	}
}

func TestStart_GitCredentials_PersistedAgentConfigStripped(t *testing.T) {
	project, agentDir := gitCredFixture{
		agentCfgEnv: map[string]string{"GITHUB_TOKEN": "fake-persisted", "GH_TOKEN": "fake-persisted-2", "KEEP": "kept"},
	}.setup(t)
	env := assertNoCredentials(t, runCredStart(t, project, api.StartOptions{BrokerMode: true, NoAuth: true}))
	if env["KEEP"] != "kept" {
		t.Errorf("KEEP = %q, want kept", env["KEEP"])
	}
	// The on-disk config is not rewritten by the policy.
	if readPersistedConfig(t, agentDir).Env["GITHUB_TOKEN"] != "fake-persisted" {
		t.Errorf("persisted scion-agent.json was modified")
	}
}

func TestStart_GitCredentials_BrokerLocalTemplateStripped(t *testing.T) {
	project, _ := gitCredFixture{
		templateEnv: map[string]string{"GH_TOKEN": "fake-template", "TPL_PLAIN": "t"},
	}.setup(t)
	env := assertNoCredentials(t, runCredStart(t, project, api.StartOptions{BrokerMode: true, NoAuth: true}))
	if env["TPL_PLAIN"] != "t" {
		t.Errorf("TPL_PLAIN = %q, want t", env["TPL_PLAIN"])
	}
}

func TestStart_GitCredentials_HarnessConfigDirEnvStripped(t *testing.T) {
	project, _ := gitCredFixture{
		hcDirEnv: map[string]string{"GITHUB_TOKEN": "fake-hcdir", "HC_PLAIN": "h"},
	}.setup(t)
	env := assertNoCredentials(t, runCredStart(t, project, api.StartOptions{BrokerMode: true, NoAuth: true}))
	if env["HC_PLAIN"] != "h" {
		t.Errorf("HC_PLAIN = %q, want h", env["HC_PLAIN"])
	}
}

func TestStart_GitCredentials_SettingsHarnessConfigEntryStripped(t *testing.T) {
	project, _ := gitCredFixture{
		settingsEnv: map[string]string{"GITHUB_TOKEN": "fake-entry", "ENTRY_PLAIN": "e"},
	}.setup(t)
	env := assertNoCredentials(t, runCredStart(t, project, api.StartOptions{BrokerMode: true, NoAuth: true}))
	if env["ENTRY_PLAIN"] != "e" {
		t.Errorf("ENTRY_PLAIN = %q, want e", env["ENTRY_PLAIN"])
	}
}

func TestStart_GitCredentials_AuthCandidateStripped(t *testing.T) {
	project, _ := gitCredFixture{authEnvKey: "GH_TOKEN"}.setup(t)
	cfg := runCredStart(t, project, api.StartOptions{
		BrokerMode: true,
		Env:        map[string]string{"GH_TOKEN": "fake-auth"},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "GITHUB_TOKEN", Type: "environment", Target: "GITHUB_TOKEN", Value: "fake-secret", Source: "project"},
			{Name: "PLAIN_SECRET", Type: "environment", Target: "PLAIN_SECRET", Value: "s", Source: "project"},
		},
	})
	env := assertNoCredentials(t, cfg)
	if cfg.ResolvedAuth != nil {
		if _, ok := cfg.ResolvedAuth.EnvVars["GH_TOKEN"]; ok {
			t.Errorf("ResolvedAuth carries GH_TOKEN")
		}
	}
	if env["PLAIN_SECRET"] != "s" {
		t.Errorf("PLAIN_SECRET = %q, want s", env["PLAIN_SECRET"])
	}
}

func TestStart_GitCredentials_StagedAuthFileRemoved(t *testing.T) {
	project, agentDir := gitCredFixture{agentCfgEnv: map[string]string{}, stagedSecret: "GH_TOKEN"}.setup(t)
	runCredStart(t, project, api.StartOptions{BrokerMode: true, NoAuth: true})
	dir := filepath.Join(agentDir, "home", ".scion", "harness", "secrets")
	if _, err := os.Stat(filepath.Join(dir, "GH_TOKEN")); !os.IsNotExist(err) {
		t.Errorf("staged GH_TOKEN file still present (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "OTHER_SECRET")); err != nil {
		t.Errorf("non-credential staged file removed: %v", err)
	}
}

// TestStart_GitCredentials_AllowedIsUnchanged pins that with the hub's
// allowance every source still delivers its credential key and value, as
// before the policy existed, and that the staged file is left alone.
func TestStart_GitCredentials_AllowedIsUnchanged(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", fakeHostCred)
	project, agentDir := gitCredFixture{
		settingsEnv:  map[string]string{"GH_ENTRY": "fake-entry"},
		agentCfgEnv:  map[string]string{"GITHUB_TOKEN": "", "GH_PERSISTED": "fake-persisted", "REFERS_TO_CRED": "${GITHUB_TOKEN}"},
		authEnvKey:   "GH_TOKEN",
		stagedSecret: "GH_STAGED",
	}.setup(t)
	opts := api.StartOptions{
		BrokerMode: true, AllowGitCredentials: true,
		Env: map[string]string{"GH_TOKEN": "fake-auth", "GH_SOMEORG": "fake-hub", "SCION_GITHUB_APP_ENABLED": "true"},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "GH_SECRET", Type: "environment", Target: "GH_SECRET", Value: "fake-secret", Source: "project"},
		},
	}
	allowed := containerEnv(runCredStart(t, project, opts))
	want := map[string]string{
		"GITHUB_TOKEN":   fakeHostCred,
		"REFERS_TO_CRED": fakeHostCred,
		"GH_ENTRY":       "fake-entry",
		"GH_PERSISTED":   "fake-persisted",
		"GH_TOKEN":       "fake-auth",
		"GH_SOMEORG":     "fake-hub",
		"GH_SECRET":      "fake-secret",

		"SCION_GITHUB_APP_ENABLED": "true",
	}
	for k, v := range want {
		if allowed[k] != v {
			t.Errorf("allowed: %s = %q, want %q", k, allowed[k], v)
		}
	}
	if _, err := os.Stat(filepath.Join(agentDir, "home", ".scion", "harness", "secrets", "GH_STAGED")); err != nil {
		t.Errorf("allowed: staged file removed: %v", err)
	}

	// A local start (no hub) is unchanged too.
	local := containerEnv(runCredStart(t, project, api.StartOptions{NoAuth: true}))
	if local["GITHUB_TOKEN"] != fakeHostCred || local["GH_PERSISTED"] != "fake-persisted" {
		t.Errorf("local start: credential keys missing: GITHUB_TOKEN set=%v GH_PERSISTED=%q",
			local["GITHUB_TOKEN"] != "", local["GH_PERSISTED"])
	}
}

func TestStripGitCredentialAuthEnv(t *testing.T) {
	auth := api.AuthConfig{EnvVars: map[string]string{"GH_TOKEN": "fake", "GITHUB_TOKEN": "fake", "ANTHROPIC_API_KEY": "fake-k"}}
	stripGitCredentialAuthEnv(&auth)
	if len(auth.EnvVars) != 1 || auth.EnvVars["ANTHROPIC_API_KEY"] != "fake-k" {
		t.Errorf("auth env = keys %v, want only ANTHROPIC_API_KEY", credKeys(auth.EnvVars))
	}
	stripGitCredentialAuthEnv(nil)
}

// TestStart_GitCredentials_AuthValueNotStagedAsFile covers the auth path of
// a container-script harness, which writes auth env values to files in the
// agent home before the container starts: a matching key must not be
// written, while other auth keys still are.
func TestStart_GitCredentials_AuthValueNotStagedAsFile(t *testing.T) {
	project, agentDir := gitCredFixture{authEnvKey: "GH_TOKEN", containerScript: true}.setup(t)
	runCredStart(t, project, api.StartOptions{
		BrokerMode: true,
		Env:        map[string]string{"GH_TOKEN": "fake-auth"},
	})
	staged := filepath.Join(agentDir, "home", ".scion", "harness", "secrets", "GH_TOKEN")
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("GH_TOKEN staged as a file in the agent home (err=%v)", err)
	}

	// Control: with the allowance the same start stages it.
	project, agentDir = gitCredFixture{authEnvKey: "GH_TOKEN", containerScript: true}.setup(t)
	runCredStart(t, project, api.StartOptions{
		BrokerMode: true, AllowGitCredentials: true,
		Env: map[string]string{"GH_TOKEN": "fake-auth"},
	})
	staged = filepath.Join(agentDir, "home", ".scion", "harness", "secrets", "GH_TOKEN")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("control: allowed start did not stage GH_TOKEN (err=%v); the test does not exercise staging", err)
	}
}
