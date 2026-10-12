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
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for the template env layer (ptone/scion#4524 rework): a template's
// env is a lowest-precedence fill for the job's own tools, recomputed from
// the live template chain on every start, applied with literal values as
// the final step of container env assembly.

// templateEnvStart describes one template-env Start fixture.
type templateEnvStart struct {
	// tplEnv is the env of the "tenv" template.
	tplEnv map[string]string
	// hcFileEnv is the env of the harness-config file (config.yaml).
	hcFileEnv map[string]string
	// settingsHCEnv is the env of the settings harness_configs entry.
	settingsHCEnv map[string]string
	// settingsExtra is appended to settings.yaml (e.g. a telemetry block).
	settingsExtra string
}

func yamlEnvBlock(indent string, env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(indent + "env:\n")
	for _, k := range keys {
		b.WriteString(indent + "  " + k + ": \"" + env[k] + "\"\n")
	}
	return b.String()
}

// fixture builds HOME with a test-harness harness-config, settings and the
// "tenv" template, and returns the project .scion dir.
func (f templateEnvStart) fixture(t *testing.T) string {
	t.Helper()
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			k := strings.SplitN(e, "=", 2)[0]
			t.Setenv(k, "")
			os.Unsetenv(k) //nolint:errcheck
		}
	}
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hcYAML := "harness: gemini\nuser: scion\nimage: test-image:latest\n" + yamlEnvBlock("", f.hcFileEnv)
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"
	if len(f.settingsHCEnv) > 0 {
		settings += "harness_configs:\n  test-harness:\n    harness: gemini\n" + yamlEnvBlock("    ", f.settingsHCEnv)
	}
	settings += f.settingsExtra
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTenvTemplate(t, f.tplEnv)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// writeTenvTemplate (re)writes the "tenv" template with env tplEnv.
func writeTenvTemplate(t *testing.T, tplEnv map[string]string) {
	t.Helper()
	cfg := map[string]any{"default_harness_config": "test-harness"}
	if tplEnv != nil {
		cfg["env"] = tplEnv
	}
	writeTemplateJSON(t, "tenv", cfg)
}

// startTemplateEnv runs a broker-mode Start of agent name (from the "tenv"
// template on create) and returns the container env and the start result.
func startTemplateEnv(t *testing.T, ctx context.Context, projectScionDir, name string, mutate func(*api.StartOptions)) (map[string]string, *api.AgentInfo) {
	t.Helper()
	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	}
	opts := api.StartOptions{
		Name:        name,
		Template:    "tenv",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Env:         map[string]string{},
	}
	if mutate != nil {
		mutate(&opts)
	}
	info, err := NewManager(mockRT).Start(ctx, opts)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return effectiveContainerEnv(t, captured), info
}

// effectiveContainerEnv returns the env a runtime gives the container from
// cfg's env and fallback layers: RunConfig.Env, then RunConfig.FallbackEnv
// applied as the last step (runtime.FallbackEnvEntries). Runtime-side sources
// (auth env, harness env, env secrets) are covered by the pkg/runtime tests.
// It also checks that template env never rides RunConfig.Env.
func effectiveContainerEnv(t *testing.T, cfg runtime.RunConfig) map[string]string {
	t.Helper()
	env := map[string]string{}
	emitted := map[string]bool{}
	for _, kv := range cfg.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if _, dup := env[k]; dup {
				t.Errorf("container env has %s twice", k)
			}
			env[k] = v
			emitted[k] = true
		}
	}
	for _, kv := range runtime.FallbackEnvEntries(cfg.FallbackEnv, cfg.SuppressedFallbackKeys, emitted) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

func startWarnings(info *api.AgentInfo) string {
	if info == nil {
		return ""
	}
	return strings.Join(info.Warnings, "\n")
}

// Acceptance 1: a template key absent from every other source reaches the
// container with its literal value.
func TestStart_TemplateEnvFillsAbsentKey(t *testing.T) {
	project := templateEnvStart{tplEnv: map[string]string{"TPL_TOOL_HOME": "/opt/tool"}}.fixture(t)
	env, info := startTemplateEnv(t, context.Background(), project, "fill", nil)
	assertEnv(t, env, "TPL_TOOL_HOME", "/opt/tool")
	if w := startWarnings(info); strings.Contains(w, "TPL_TOOL_HOME") {
		t.Errorf("an applied template key must not warn:\n%s", w)
	}
}

// Acceptance 2: the same key set by any other source beats the template
// value, one subtest per source.
//
// Order-detecting subtests use non-reserved keys, so a fill applied too
// early would make the template value win: the harness-config file and the
// requester env (config tier), the settings harness-config entry and the
// hub env scope (request tier), and a broker-injected var (GCE_METADATA_HOST,
// which the broker injects for metadata interception).
//
// Guard subtests ("guard: ...") cover sources whose keys are all SCION_*,
// which a template may not set at all: Start's telemetry env
// (config.TelemetryConfigToEnv emits only SCION_* keys, and no harness's
// GetTelemetryEnv returns anything today), the hub default env
// (SCION_AUTO_EXPOSE_PORTS) and the launch id injected after assembly. They
// show the template value is ignored and the source wins; they cannot
// detect a wrong fill order.
func TestStart_TemplateEnvLosesToEverySource(t *testing.T) {
	const key = "TPL_SHARED"
	cases := []struct {
		name   string
		f      templateEnvStart
		ctx    func() context.Context
		mutate func(*api.StartOptions)
		key    string
		want   string
	}{
		{
			name: "harness-config file",
			f:    templateEnvStart{tplEnv: map[string]string{key: "template"}, hcFileEnv: map[string]string{key: "hc-file"}},
			key:  key, want: "hc-file",
		},
		{
			name: "settings harness-config entry",
			f:    templateEnvStart{tplEnv: map[string]string{key: "template"}, settingsHCEnv: map[string]string{key: "settings"}},
			key:  key, want: "settings",
		},
		{
			name:   "hub env scope",
			f:      templateEnvStart{tplEnv: map[string]string{key: "template"}},
			mutate: func(o *api.StartOptions) { o.Env[key] = "hub-scope" },
			key:    key, want: "hub-scope",
		},
		{
			name:   "requester env",
			f:      templateEnvStart{tplEnv: map[string]string{key: "template"}},
			mutate: func(o *api.StartOptions) { o.InlineConfig = &api.ScionConfig{Env: map[string]string{key: "requester"}} },
			key:    key, want: "requester",
		},
		{
			name: "guard: telemetry env",
			f: templateEnvStart{
				tplEnv:        map[string]string{"SCION_OTEL_ENDPOINT": "template:4317"},
				settingsExtra: "telemetry:\n  cloud:\n    endpoint: settings:4317\n",
			},
			key: "SCION_OTEL_ENDPOINT", want: "settings:4317",
		},
		{
			name: "guard: hub default env",
			f:    templateEnvStart{tplEnv: map[string]string{api.EnvAutoExposePorts: "false"}},
			ctx: func() context.Context {
				on := true
				return api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{AutoExposePorts: &on})
			},
			key: api.EnvAutoExposePorts, want: "true",
		},
		{
			name:   "broker-injected var",
			f:      templateEnvStart{tplEnv: map[string]string{"GCE_METADATA_HOST": "template-metadata:80"}},
			mutate: func(o *api.StartOptions) { o.Env["GCE_METADATA_HOST"] = "169.254.169.254:8888" },
			key:    "GCE_METADATA_HOST", want: "169.254.169.254:8888",
		},
		{
			name:   "guard: broker-injected SCION_* var",
			f:      templateEnvStart{tplEnv: map[string]string{"SCION_AGENT_ID": "template-id"}},
			mutate: func(o *api.StartOptions) { o.Env["SCION_AGENT_ID"] = "broker-id" },
			key:    "SCION_AGENT_ID", want: "broker-id",
		},
		{
			name:   "guard: launch id injected after assembly",
			f:      templateEnvStart{tplEnv: map[string]string{envLaunchID: "template-launch"}},
			mutate: func(o *api.StartOptions) { o.RunID = "run-123" },
			key:    envLaunchID, want: "run-123",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := tc.f.fixture(t)
			ctx := context.Background()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			env, _ := startTemplateEnv(t, ctx, project, "lose", tc.mutate)
			assertEnv(t, env, tc.key, tc.want)
		})
	}
}

// Acceptance 4: a restart applies the current live template env, and an
// unresolvable template gives a warning and no template env.
func TestStart_TemplateEnvFollowsLiveTemplate(t *testing.T) {
	project := templateEnvStart{tplEnv: map[string]string{"TPL_LIVE": "v1"}}.fixture(t)
	env, _ := startTemplateEnv(t, context.Background(), project, "live", nil)
	assertEnv(t, env, "TPL_LIVE", "v1")

	writeTenvTemplate(t, map[string]string{"TPL_LIVE": "v2"})
	env, _ = startTemplateEnv(t, context.Background(), project, "live", func(o *api.StartOptions) { o.Template = "" })
	assertEnv(t, env, "TPL_LIVE", "v2")

	if err := os.RemoveAll(filepath.Join(os.Getenv("HOME"), ".scion", "templates", "tenv")); err != nil {
		t.Fatal(err)
	}
	env, info := startTemplateEnv(t, context.Background(), project, "live", func(o *api.StartOptions) { o.Template = "" })
	if v, ok := env["TPL_LIVE"]; ok {
		t.Errorf("TPL_LIVE=%q applied from an unresolvable template", v)
	}
	if w := startWarnings(info); !strings.Contains(w, "template tenv not found; its env fallback is not applied") {
		t.Errorf("missing the unresolvable-template env warning:\n%s", w)
	}
}

// Acceptance 5: template values are literal: "${HOST_VAR}" is not expanded
// against the broker host env, and an empty value is not a host
// passthrough; both warn.
func TestStart_TemplateEnvValuesAreLiteral(t *testing.T) {
	t.Setenv("TS6_HOST_SECRET", "broker-host-secret")
	project := templateEnvStart{tplEnv: map[string]string{
		"TPL_EXPAND":      "${TS6_HOST_SECRET}",
		"TS6_HOST_SECRET": "",
	}}.fixture(t)
	env, info := startTemplateEnv(t, context.Background(), project, "literal", nil)

	assertEnv(t, env, "TPL_EXPAND", "${TS6_HOST_SECRET}")
	if v, ok := env["TS6_HOST_SECRET"]; ok {
		t.Errorf("empty template value passed the broker host value through: TS6_HOST_SECRET=%q", v)
	}
	for k, v := range env {
		if strings.Contains(v, "broker-host-secret") {
			t.Errorf("broker host secret reached the container via %s", k)
		}
	}
	w := startWarnings(info)
	if !strings.Contains(w, "template env TPL_EXPAND value contains ${...}; used literally, not expanded") {
		t.Errorf("missing the literal ${...} warning:\n%s", w)
	}
	if !strings.Contains(w, "template env TS6_HOST_SECRET has an empty value; skipped") {
		t.Errorf("missing the empty-value warning:\n%s", w)
	}
}

// Acceptance 6 (env-gather side): a template key never becomes a required
// ("missing") env key: an empty template value is skipped with the template
// warning only. (Auth detection reads only the request env and resolved
// secrets, assembled before the template fill; the fill-ordering mutant in
// the PR covers the order.)
func TestStart_TemplateEnvNotARequiredKey(t *testing.T) {
	project := templateEnvStart{tplEnv: map[string]string{"TPL_NEEDS_VALUE": ""}}.fixture(t)
	env, info := startTemplateEnv(t, context.Background(), project, "gather", nil)
	if _, ok := env["TPL_NEEDS_VALUE"]; ok {
		t.Error("empty template key reached the container")
	}
	for _, w := range info.Warnings {
		if strings.Contains(w, "TPL_NEEDS_VALUE") && !strings.Contains(w, "template env TPL_NEEDS_VALUE has an empty value") {
			t.Errorf("template key reported as a required/missing env key: %q", w)
		}
	}
}

// Acceptance 7: an agent whose scion-agent.json Env has template keys baked
// in by an older version: equal-value keys apply only through the template
// fill, and nothing on disk is rewritten.
func TestStart_TemplateEnvLegacyBakedKeysDemoted(t *testing.T) {
	project := templateEnvStart{tplEnv: map[string]string{"TPL_BAKED": "same"}}.fixture(t)
	startTemplateEnv(t, context.Background(), project, "legacy", nil)

	agentDir := filepath.Join(project, "agents", "legacy")
	stored, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	stored.Env = map[string]string{"TPL_BAKED": "same", "LEGACY_OWN": "kept"}
	writeStoredAgentConfig(t, agentDir, stored)

	env, _ := startTemplateEnv(t, context.Background(), project, "legacy", func(o *api.StartOptions) { o.Template = "" })
	assertEnv(t, env, "TPL_BAKED", "same")
	assertEnv(t, env, "LEGACY_OWN", "kept")

	after, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if after.Env["TPL_BAKED"] != "same" {
		t.Errorf("stored scion-agent.json was rewritten: Env = %v", after.Env)
	}
}

func TestDemoteLegacyTemplateEnv(t *testing.T) {
	cfg := &api.ScionConfig{Model: "m", Env: map[string]string{"EQ": "v", "DIFF": "mine", "OWN": "x"}}
	got, demoted := demoteLegacyTemplateEnv(cfg, map[string]string{"EQ": "v", "DIFF": "template"})
	if !reflect.DeepEqual(demoted, []string{"EQ"}) {
		t.Errorf("demoted = %v, want [EQ]", demoted)
	}
	if want := map[string]string{"DIFF": "mine", "OWN": "x"}; !reflect.DeepEqual(got.Env, want) {
		t.Errorf("Env = %v, want %v (only the equal-value key demoted)", got.Env, want)
	}
	if cfg.Env["EQ"] != "v" || got.Model != "m" {
		t.Errorf("input modified or job keys lost: cfg=%v got=%+v", cfg.Env, got)
	}
	if same, none := demoteLegacyTemplateEnv(cfg, map[string]string{"OTHER": "1"}); same != cfg || none != nil {
		t.Error("nothing to demote: want cfg itself and no demoted keys")
	}
}

// A demoted legacy key that is also an auth-candidate key is reported, never
// dropped silently.
func TestSuppressedDemotedKeyWarnings(t *testing.T) {
	got := suppressedDemotedKeyWarnings([]string{"ANTHROPIC_API_KEY", "TOOL_HOME"}, []string{"ANTHROPIC_API_KEY", "GOOGLE_CLOUD_PROJECT"})
	if len(got) != 1 || !strings.Contains(got[0], "stored env ANTHROPIC_API_KEY came from the template") {
		t.Errorf("warnings = %q, want one for ANTHROPIC_API_KEY", got)
	}
	if got := suppressedDemotedKeyWarnings([]string{"TOOL_HOME"}, []string{"ANTHROPIC_API_KEY"}); got != nil {
		t.Errorf("non-auth demoted key warned: %q", got)
	}
}

// Acceptance 8: TZ from a template is skipped for a hub-dispatched (broker)
// start: the template env is a broker-local layer, and the hub-only rule
// applies.
func TestStart_TemplateEnvHubOnlyKeySkipped(t *testing.T) {
	project := templateEnvStart{tplEnv: map[string]string{"TZ": "Pacific/Auckland"}}.fixture(t)
	env, _ := startTemplateEnv(t, context.Background(), project, "tz", nil)
	if v, ok := env["TZ"]; ok {
		t.Errorf("template TZ=%q applied on a broker start", v)
	}
}

// Acceptance 9: a template's SCION_* env is ignored and reported.
func TestStart_TemplateEnvReservedScionKeyIgnored(t *testing.T) {
	project := templateEnvStart{tplEnv: map[string]string{"SCION_TPL_FLAG": "1"}}.fixture(t)
	env, info := startTemplateEnv(t, context.Background(), project, "reserved", nil)
	if v, ok := env["SCION_TPL_FLAG"]; ok {
		t.Errorf("template SCION_TPL_FLAG=%q applied", v)
	}
	if w := startWarnings(info); !strings.Contains(w, "template tenv sets environment key(s) env.SCION_TPL_FLAG; ignored") {
		t.Errorf("missing the reserved-key warning:\n%s", w)
	}
}

func TestTemplateEnvFallback(t *testing.T) {
	got, warnings := templateEnvFallback(map[string]string{
		"NEW":    "n",
		"BLANK":  "",
		"DOLLAR": "${HOME}/x",
		"TZ":     "UTC",
	}, true, "a")
	want := map[string]string{"NEW": "n", "DOLLAR": "${HOME}/x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fallback = %v, want %v (empty skipped, ${...} literal, TZ hub-only on a broker start)", got, want)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %q, want the empty-value and ${...} warnings", warnings)
	}
	// Outside broker mode the hub-only rule does not apply.
	got, _ = templateEnvFallback(map[string]string{"TZ": "UTC"}, false, "a")
	if !reflect.DeepEqual(got, map[string]string{"TZ": "UTC"}) {
		t.Errorf("non-broker TZ fallback = %v", got)
	}
}

// Template env never rides RunConfig.Env: it travels only in FallbackEnv,
// which the runtime applies last.
func TestStart_TemplateEnvTravelsAsFallbackEnv(t *testing.T) {
	project := templateEnvStart{tplEnv: map[string]string{"TPL_ONLY": "v"}}.fixture(t)
	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	}
	if _, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name: "travel", Template: "tenv", ProjectPath: project, BrokerMode: true, NoAuth: true, Env: map[string]string{},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, kv := range captured.Env {
		if strings.HasPrefix(kv, "TPL_ONLY=") {
			t.Errorf("template env rode RunConfig.Env: %s", kv)
		}
	}
	if captured.FallbackEnv["TPL_ONLY"] != "v" {
		t.Errorf("FallbackEnv = %v, want TPL_ONLY=v", captured.FallbackEnv)
	}
}

// FYI-5 ruling: ALL of the harness-config's auth-candidate keys are
// suppressed for the fallback, so a template never supplies a credential,
// even one nothing else set. The agent resolves to vertex-ai, so
// ANTHROPIC_API_KEY belongs to an auth type it does NOT use: suppressing only
// the selected type's keys would let it through (the vertex scenario: a
// present API key can switch the in-container CLI off vertex).
func TestStart_TemplateEnvAuthCandidateKeysSuppressed(t *testing.T) {
	project := templateEnvStart{tplEnv: map[string]string{
		"ANTHROPIC_API_KEY":    "template-key",
		"GOOGLE_CLOUD_PROJECT": "template-project",
		"TPL_TOOL":             "ok",
	}}.fixture(t)
	hcFile := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "test-harness", "config.yaml")
	auth := "auth:\n  default_type: vertex-ai\n  types:\n    api-key:\n      required_env:\n        - any_of: [ANTHROPIC_API_KEY]\n    vertex-ai:\n      required_env:\n        - any_of: [GOOGLE_CLOUD_PROJECT]\n"
	data, err := os.ReadFile(hcFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hcFile, append(data, []byte(auth)...), 0o644); err != nil {
		t.Fatal(err)
	}
	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	}
	if _, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name: "suppress", Template: "tenv", ProjectPath: project, BrokerMode: true, NoAuth: true,
		HarnessAuth: "vertex-ai", Env: map[string]string{},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	suppressed := map[string]bool{}
	for _, k := range captured.SuppressedFallbackKeys {
		suppressed[k] = true
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "GOOGLE_CLOUD_PROJECT"} {
		if !suppressed[k] {
			t.Errorf("SuppressedFallbackKeys = %v, want %s (every auth-candidate key, used or not)", captured.SuppressedFallbackKeys, k)
		}
	}
	env := effectiveContainerEnv(t, captured)
	if v, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("template credential ANTHROPIC_API_KEY=%q reached a vertex-ai agent's container", v)
	}
	if v, ok := env["GOOGLE_CLOUD_PROJECT"]; ok {
		t.Errorf("template GOOGLE_CLOUD_PROJECT=%q (a credential key of the selected method) reached the container", v)
	}
	assertEnv(t, env, "TPL_TOOL", "ok")
}

// B1: a hosted agent's start or restart leaves opts.Template unset; the
// broker passes the hydrated template in opts.TemplateEnvSource, which
// supplies the template env without a "not found" warning. Without it, the
// env fallback is not applied and Start warns.
func TestStart_TemplateEnvFromTemplateEnvSource(t *testing.T) {
	project := templateEnvStart{}.fixture(t)
	hashDir := filepath.Join(os.Getenv("HOME"), "template-cache", testContentHash)
	if err := os.MkdirAll(hashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hashDir, "scion-agent.json"),
		[]byte(`{"default_harness_config": "test-harness", "env": {"TPL_HOSTED": "cached"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create from the hydrated cache dir, as the broker does on create.
	env, _ := startTemplateEnv(t, context.Background(), project, "hosted", func(o *api.StartOptions) {
		o.Template, o.TemplateName = hashDir, "web-dev"
	})
	assertEnv(t, env, "TPL_HOSTED", "cached")

	// Restart with opts.Template unset and the env source set.
	env, info := startTemplateEnv(t, context.Background(), project, "hosted", func(o *api.StartOptions) {
		o.Template, o.TemplateEnvSource = "", hashDir
	})
	assertEnv(t, env, "TPL_HOSTED", "cached")
	if w := startWarnings(info); strings.Contains(w, "its env fallback is not applied") {
		t.Errorf("unexpected env-fallback warning with a template env source:\n%s", w)
	}

	// Restart without an env source: no template env, and a warning.
	env, info = startTemplateEnv(t, context.Background(), project, "hosted", func(o *api.StartOptions) { o.Template = "" })
	if v, ok := env["TPL_HOSTED"]; ok {
		t.Errorf("TPL_HOSTED=%q applied without a template env source", v)
	}
	if w := startWarnings(info); !strings.Contains(w, "template web-dev not found; its env fallback is not applied") {
		t.Errorf("missing the env-fallback warning:\n%s", w)
	}
}

// writeStoredAgentConfig overwrites an agent's scion-agent.json with cfg.
func writeStoredAgentConfig(t *testing.T, agentDir string, cfg *api.ScionConfig) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
