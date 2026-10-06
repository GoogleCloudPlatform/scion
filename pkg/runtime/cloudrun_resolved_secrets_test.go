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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// Placeholder values only; none of these are real credentials.
const (
	crTestEnvSecretValue  = "placeholder-env-secret-value"
	crTestFileSecretValue = "placeholder-file-secret-content"
	crTestFileTarget      = "/home/scion/.config/example/creds.json"
	crTestVarSecretValue  = "placeholder-variable-secret-value"
)

func crTestResolvedSecrets() []api.ResolvedSecret {
	return []api.ResolvedSecret{
		{Name: "api-key", Type: "environment", Target: "EXAMPLE_API_KEY", Value: crTestEnvSecretValue},
		{Name: "creds", Type: "file", Target: crTestFileTarget, Value: crTestFileSecretValue},
		{Name: "region", Type: "variable", Target: "EXAMPLE_REGION", Value: crTestVarSecretValue},
		// Collides with a key already in cfg.Env: cfg.Env must win.
		{Name: "dup", Type: "environment", Target: "PRESET_KEY", Value: "from-secret"},
	}
}

// assertResolvedSecretsInEnv checks that env carries the env-type secret as a
// plain variable, the file- and variable-type secrets inside the
// staged-secrets blob, and
// that a secret never overrides a key already present in cfg.Env.
func assertResolvedSecretsInEnv(t *testing.T, env map[string]string) {
	t.Helper()
	if got := env["EXAMPLE_API_KEY"]; got != crTestEnvSecretValue {
		t.Errorf("env-type secret EXAMPLE_API_KEY not delivered (present=%v)", got != "")
	}
	if got := env["PRESET_KEY"]; got != "from-env" {
		t.Errorf("PRESET_KEY = %q, want cfg.Env value %q", got, "from-env")
	}
	blob, ok := env[stagedsecrets.EnvVar]
	if !ok {
		t.Fatalf("%s not set; file-type secret not delivered", stagedsecrets.EnvVar)
	}
	staged, err := stagedsecrets.Decode(blob)
	if err != nil {
		t.Fatalf("decode %s: %v", stagedsecrets.EnvVar, err)
	}
	if len(staged.FileSecrets) != 1 || staged.FileSecrets[0].Target != crTestFileTarget {
		t.Fatalf("staged file secrets: %d entries, want one entry for %s", len(staged.FileSecrets), crTestFileTarget)
	}
	if got := staged.VariableSecrets["EXAMPLE_REGION"]; got != crTestVarSecretValue {
		t.Errorf("variable-type secret EXAMPLE_REGION not staged (present=%v)", got != "")
	}
	if _, ok := env["EXAMPLE_REGION"]; ok {
		t.Error("variable-type secret EXAMPLE_REGION must not be a plain env var")
	}
}

// crInstanceEnv returns the env of the single created instance, failing the
// test if any name appears more than once.
func crInstanceEnv(t *testing.T, fake *fakeInstancesClient) map[string]string {
	t.Helper()
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
	}
	env := make(map[string]string)
	count := make(map[string]int)
	for _, ev := range fake.createReqs[0].Instance.Containers[0].Env {
		count[ev.Name]++
		if v, ok := ev.GetValues().(*runpb.EnvVar_Value); ok {
			env[ev.Name] = v.Value
		}
	}
	for name, n := range count {
		if n > 1 {
			t.Errorf("env var %s appears %d times in the instance spec", name, n)
		}
	}
	return env
}

// sandboxSecretsHarness is a fake sandbox CLI setup. The mock binary marks
// every invocation and records the argv of "run".
type sandboxSecretsHarness struct {
	rt        *CloudRunSandboxRuntime
	cfg       RunConfig
	argsFile  string
	invokedAt string
}

func newSandboxSecretsHarness(t *testing.T) *sandboxSecretsHarness {
	t.Helper()
	tmpDir := t.TempDir()
	h := &sandboxSecretsHarness{
		argsFile:  filepath.Join(tmpDir, "sandbox-args"),
		invokedAt: filepath.Join(tmpDir, "sandbox-invoked"),
	}
	mockBin := filepath.Join(tmpDir, "sandbox")
	script := "#!/bin/sh\ntouch " + h.invokedAt + "\n" +
		"if [ \"$1\" = \"run\" ]; then\n  printf '%s\\n' \"$@\" > " + h.argsFile + "\nfi\necho sandbox-ok\n"
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(tmpDir, "agent-home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}
	h.rt = &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(filepath.Join(tmpDir, "state.json")),
		rootDir:      filepath.Join(tmpDir, "scion"),
		watchCancels: make(map[string]context.CancelFunc),
	}
	t.Cleanup(func() {
		h.rt.watchMu.Lock()
		for _, cancel := range h.rt.watchCancels {
			cancel()
		}
		h.rt.watchMu.Unlock()
	})
	h.cfg = RunConfig{
		Name:         "secrets-agent",
		HomeDir:      homeDir,
		Workspace:    filepath.Join(tmpDir, "workspace"),
		Image:        "omni-image",
		UnixUsername: "scion",
		Harness:      &mockHarness{command: []string{"gemini"}, env: map[string]string{}},
	}
	if err := os.MkdirAll(h.cfg.Workspace, 0755); err != nil {
		t.Fatal(err)
	}
	return h
}

// runEnv returns the --env pairs the mock binary received for "run".
func (h *sandboxSecretsHarness) runEnv(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(h.argsFile)
	if err != nil {
		t.Fatalf("read mock binary args: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	env := make(map[string]string)
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] == "--" {
			break
		}
		if lines[i] == "--env" {
			if k, v, ok := strings.Cut(lines[i+1], "="); ok {
				env[k] = v
			}
			i++
		}
	}
	return env
}

// invoked reports whether the mock binary ran at all.
func (h *sandboxSecretsHarness) invoked() bool {
	_, err := os.Stat(h.invokedAt)
	return err == nil
}

// checkSizeError checks that err names the secret and does not carry the
// value. It reports only whether the value was present, never the error
// text itself, so a regression cannot print a large value.
func checkSizeError(t *testing.T, err error, wantName, valueProbe string) {
	t.Helper()
	if err == nil {
		t.Fatal("Run succeeded; want an error for the oversized secret")
	}
	msg := err.Error()
	if !strings.Contains(msg, wantName) {
		t.Errorf("error does not name secret %s (error length %d)", wantName, len(msg))
	}
	if strings.Contains(msg, valueProbe) {
		t.Error("error message contains the secret value")
	}
}

func TestCloudRunRun_DeliversResolvedSecrets(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)

	cfg := runConfigForTest()
	cfg.UnixUsername = "scion"
	cfg.Env = []string{"PRESET_KEY=from-env"}
	cfg.ResolvedSecrets = crTestResolvedSecrets()

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertResolvedSecretsInEnv(t, crInstanceEnv(t, fake))
}

// Env-type secrets targeting names the runtime sets itself (staged keys,
// SCION_HOST_UID/GID) are skipped, so the spec never repeats a name.
func TestCloudRunRun_SecretsDoNotDuplicateRuntimeEnv(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)

	cfg := runConfigForTest()
	cfg.UnixUsername = "scion"
	cfg.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "creds", Type: "file", Target: crTestFileTarget, Value: crTestFileSecretValue},
		{Name: "s1", Type: "environment", Target: stagedsecrets.EnvVar, Value: "from-secret"},
		{Name: "s2", Type: "environment", Target: telemetryGCPCredentialsEnvVar, Value: "from-secret"},
		{Name: "s3", Type: "environment", Target: "SCION_HOST_UID", Value: "from-secret"},
		{Name: "s4", Type: "", Target: "SCION_HOST_GID", Value: "from-secret"},
	}
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := crInstanceEnv(t, fake)
	for _, key := range []string{stagedsecrets.EnvVar, telemetryGCPCredentialsEnvVar, "SCION_HOST_UID", "SCION_HOST_GID"} {
		if env[key] == "from-secret" {
			t.Errorf("%s taken from an env-type secret; the runtime value must win", key)
		}
	}
	if _, err := stagedsecrets.Decode(env[stagedsecrets.EnvVar]); err != nil {
		t.Errorf("%s is not the staged blob: decode failed", stagedsecrets.EnvVar)
	}
}

func TestCloudRunSandboxRun_DeliversResolvedSecrets(t *testing.T) {
	h := newSandboxSecretsHarness(t)
	h.cfg.Env = []string{"PRESET_KEY=from-env"}
	h.cfg.ResolvedSecrets = crTestResolvedSecrets()

	if _, err := h.rt.Run(context.Background(), h.cfg); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertResolvedSecretsInEnv(t, h.runEnv(t))
}

// As in Docker, an env-type secret wins over harness, auth and synthesised
// env, except the UID/GID keys the sandbox fixes; cfg.Env still wins over
// the secret.
func TestCloudRunSandboxRun_SecretPrecedence(t *testing.T) {
	h := newSandboxSecretsHarness(t)
	h.cfg.Env = []string{"PRESET_KEY=from-env"}
	h.cfg.Harness = &mockHarness{command: []string{"gemini"}, env: map[string]string{
		"HARNESS_KEY": "from-harness",
		"PRESET_KEY":  "from-harness",
	}}
	h.cfg.ResolvedAuth = &api.ResolvedAuth{Method: "api-key", EnvVars: map[string]string{"AUTH_KEY": "from-auth"}}
	h.cfg.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "h", Type: "environment", Target: "HARNESS_KEY", Value: "from-secret"},
		{Name: "a", Type: "environment", Target: "AUTH_KEY", Value: "from-secret"},
		{Name: "w", Type: "environment", Target: "SCION_WORKSPACE_PATH", Value: "from-secret"},
		{Name: "u", Type: "environment", Target: "SCION_HOST_UID", Value: "from-secret"},
		{Name: "p", Type: "environment", Target: "PRESET_KEY", Value: "from-secret"},
	}
	if _, err := h.rt.Run(context.Background(), h.cfg); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	env := h.runEnv(t)
	for _, key := range []string{"HARNESS_KEY", "AUTH_KEY", "SCION_WORKSPACE_PATH"} {
		if env[key] != "from-secret" {
			t.Errorf("%s = %q, want the secret to win", key, env[key])
		}
	}
	if env["SCION_HOST_UID"] == "from-secret" {
		t.Error("SCION_HOST_UID taken from a secret; the sandbox value must win")
	}
	// Pre-existing sandbox behaviour: harness env overrides cfg.Env. The
	// secret must not change that, since cfg.Env already beat it.
	if env["PRESET_KEY"] != "from-harness" {
		t.Errorf("PRESET_KEY = %q, want harness value (secret skipped for cfg.Env key)", env["PRESET_KEY"])
	}
}

func TestCloudRunSandboxRun_OversizedSecretFailsWithName(t *testing.T) {
	big := strings.Repeat("x", sandboxMaxEnvArgBytes+1)
	cases := []struct {
		name   string
		secret api.ResolvedSecret
	}{
		{"environment", api.ResolvedSecret{Name: "big-env", Type: "environment", Target: "BIG_ENV", Value: big}},
		{"file", api.ResolvedSecret{Name: "big-file", Type: "file", Target: "/home/scion/big.bin", Value: big}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSandboxSecretsHarness(t)
			h.cfg.ResolvedSecrets = []api.ResolvedSecret{
				{Name: "small", Type: "environment", Target: "SMALL", Value: "fits"},
				tc.secret,
			}
			_, err := h.rt.Run(context.Background(), h.cfg)
			checkSizeError(t, err, `"`+tc.secret.Name+`"`, "xxxxxxxx")
			if h.invoked() {
				t.Error("sandbox binary was invoked; want Run to fail before any command")
			}
		})
	}
}

// The sandbox limit covers the whole KEY=VALUE argv string plus its NUL.
func TestApplyResolvedSecretsToEnv_SandboxLimitCountsKey(t *testing.T) {
	const key = "LONG_SECRET_TARGET"
	fit := strings.Repeat("x", sandboxMaxEnvArgBytes-len(key)-2)
	for _, tc := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"exactly at limit", fit, false},
		{"one byte over", fit + "x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := RunConfig{
				UnixUsername:    "scion",
				ResolvedSecrets: []api.ResolvedSecret{{Name: "long", Type: "environment", Target: key, Value: tc.value}},
			}
			_, err := applyResolvedSecretsToEnv(&cfg, sandboxEnvLimit, sandboxRuntimeEnvKeys...)
			if tc.wantErr {
				checkSizeError(t, err, `"long"`, "xxxxxxxx")
				return
			}
			if err != nil {
				t.Errorf("value that fits was rejected (error length %d)", len(err.Error()))
			}
		})
	}
}

// Appending must not write into spare capacity of the caller's Env array.
func TestApplyResolvedSecretsToEnv_DoesNotWriteCallerBackingArray(t *testing.T) {
	backing := make([]string, 1, 4)
	backing[0] = "PRESET_KEY=from-env"
	cfg := RunConfig{
		UnixUsername: "scion",
		Env:          backing,
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "api-key", Type: "environment", Target: "EXAMPLE_API_KEY", Value: crTestEnvSecretValue},
		},
	}
	if _, err := applyResolvedSecretsToEnv(&cfg, cloudRunEnvLimit); err != nil {
		t.Fatalf("applyResolvedSecretsToEnv: %v", err)
	}
	if spare := backing[:cap(backing)]; spare[1] != "" {
		t.Error("secret written into the caller's spare Env capacity")
	}
	if len(cfg.Env) != 2 {
		t.Errorf("len(cfg.Env) = %d, want 2", len(cfg.Env))
	}
}

func TestApplyResolvedSecretsToEnv_CollisionKeepsCfgEnv(t *testing.T) {
	cfg := RunConfig{
		UnixUsername: "scion",
		Env:          []string{"PRESET_KEY=from-env"},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "dup", Type: "environment", Target: "PRESET_KEY", Value: "from-secret"},
			{Name: "a1", Type: "environment", Target: "SAME_TARGET", Value: "first-value"},
			{Name: "a2", Type: "", Target: "SAME_TARGET", Value: "second-value"},
		},
	}
	if _, err := applyResolvedSecretsToEnv(&cfg, cloudRunEnvLimit); err != nil {
		t.Fatalf("applyResolvedSecretsToEnv: %v", err)
	}
	want := []string{"PRESET_KEY=from-env", "SAME_TARGET=second-value"}
	if strings.Join(cfg.Env, "\n") != strings.Join(want, "\n") {
		t.Errorf("Env = %q, want %q", cfg.Env, want)
	}
}

func TestCloudRunRun_OversizedSecretFailsWithName(t *testing.T) {
	big := strings.Repeat("x", cloudRunMaxEnvValueBytes+1)
	cases := []struct {
		name    string
		secret  api.ResolvedSecret
		wantErr string
	}{
		{"environment", api.ResolvedSecret{Name: "big-env", Type: "environment", Target: "BIG_ENV", Value: big}, `"big-env"`},
		{"file", api.ResolvedSecret{Name: "big-file", Type: "file", Target: "/home/scion/big.bin", Value: big}, `"big-file"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newFakeCloudRunRuntime(t, fake)
			cfg := runConfigForTest()
			cfg.UnixUsername = "scion"
			cfg.ResolvedSecrets = []api.ResolvedSecret{
				{Name: "small", Type: "environment", Target: "SMALL", Value: "fits"},
				tc.secret,
			}
			_, err := rt.Run(context.Background(), cfg)
			checkSizeError(t, err, tc.wantErr, "xxxxxxxx")
			if len(fake.createReqs) != 0 {
				t.Errorf("CreateInstance called %d times, want 0", len(fake.createReqs))
			}
		})
	}
}

func TestCloudRunSandboxRun_ErrorRedactsResolvedSecretValue(t *testing.T) {
	tmpDir := t.TempDir()
	mockBin := writeFakeSandbox(t, tmpDir, true) // echoes argv to stderr, exits 1
	homeDir := filepath.Join(tmpDir, "agent-home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}
	rt := &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(filepath.Join(tmpDir, "state.json")),
		rootDir:      filepath.Join(tmpDir, "scion"),
		watchCancels: make(map[string]context.CancelFunc),
	}
	cfg := RunConfig{
		Name:         "redact-probe",
		HomeDir:      homeDir,
		Workspace:    filepath.Join(tmpDir, "workspace"),
		Image:        "omni-image",
		UnixUsername: "scion",
		Harness:      &mockHarness{command: []string{"/bin/true"}},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "api-key", Type: "environment", Target: "EXAMPLE_API_KEY", Value: crTestEnvSecretValue},
			{Name: "creds", Type: "file", Target: crTestFileTarget, Value: crTestFileSecretValue},
		},
	}
	if err := os.MkdirAll(cfg.Workspace, 0755); err != nil {
		t.Fatal(err)
	}
	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() returned nil error, but the fake sandbox binary exits 1")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--allow-egress") {
		t.Fatalf("error does not carry the echoed sandbox argv, so the check is vacuous (error length %d)", len(msg))
	}
	if strings.Contains(msg, crTestEnvSecretValue) {
		t.Error("error output contains the env-type secret value")
	}
	if !strings.Contains(msg, "EXAMPLE_API_KEY") {
		t.Error("error output should still name the redacted key EXAMPLE_API_KEY")
	}
	if strings.Contains(msg, stagedsecrets.EnvVar+"=ey") {
		t.Error("error output contains the staged secrets blob")
	}
}
