// Copyright 2026 The Scion Authors.

package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// restartAuthEnvKeys are host env vars GatherAuthWithEnv may read outside
// broker mode; they are blanked so only opts.Env feeds the resolution.
var restartAuthEnvKeys = []string{
	"SCION_METADATA_MODE",
	"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT", "ANTHROPIC_VERTEX_PROJECT_ID",
	"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION", "CLAUDE_CODE_USE_VERTEX",
	"SCION_HARNESS_SELECTED_AUTH",
}

// newClaudeRestartEnv initializes a project whose "claude" harness-config is
// a copy of the real harnesses/claude bundle.
func newClaudeRestartEnv(t *testing.T) (*policyTestEnv, string) {
	t.Helper()
	for _, k := range restartAuthEnvKeys {
		t.Setenv(k, "")
	}
	src, err := filepath.Abs(filepath.Join("..", "..", "harnesses", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	e := newPolicyTestEnv(t)
	dst := filepath.Join(e.scion, "harness-configs", "claude")
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copy claude harness-config: %v", err)
	}
	return e, dst
}

func readAuthCandidates(t *testing.T, home string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		EnvSecretFiles map[string]string `json:"env_secret_files"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.EnvSecretFiles
}

// A restart whose resolution carries only ambient env (a
// region var, as the hub injects) and not the credential supplied at create
// must restage auth-candidates.json with the recorded credential, so the
// container-side provisioner can still select an auth method.
func TestStart_RestartWithOnlyAmbientEnvKeepsRecordedCredential(t *testing.T) {
	e, _ := newClaudeRestartEnv(t)
	mgr := policyTestManager(nil)
	first := api.StartOptions{Name: "restart-auth", ProjectPath: e.scion, HarnessConfig: "claude",
		Env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test-0123456789abcdefghij", "GOOGLE_CLOUD_LOCATION": "us-east5"}}
	if _, err := mgr.Start(context.Background(), first); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "restart-auth")
	if _, ok := readAuthCandidates(t, home)["ANTHROPIC_API_KEY"]; !ok {
		t.Fatal("fixture: first start should reference ANTHROPIC_API_KEY")
	}

	restart := first
	restart.Env = map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5", "CLOUD_ML_REGION": "us-east5"}
	for i := 0; i < 2; i++ { // twice: the record must keep the credential too
		if _, err := mgr.Start(context.Background(), restart); err != nil {
			t.Fatalf("restart %d: %v", i+1, err)
		}
		got := readAuthCandidates(t, home)
		if got["ANTHROPIC_API_KEY"] != "$HOME/.scion/harness/secrets/ANTHROPIC_API_KEY" {
			t.Fatalf("restart %d: recorded ANTHROPIC_API_KEY not referenced: %v", i+1, got)
		}
		data, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "secrets", "ANTHROPIC_API_KEY"))
		if err != nil || string(data) != "sk-ant-test-0123456789abcdefghij" {
			t.Fatalf("restart %d: restored secret %q (err=%v)", i+1, data, err)
		}
	}
}

// runStagedClaudeProvision runs the staged provision.py against home the
// way the 20-harness-provision hook does (HOME=agent home, minimal env).
func runStagedClaudeProvision(t *testing.T, home string) (string, error) {
	t.Helper()
	cmd := exec.Command("python3",
		filepath.Join(home, ".scion", "harness", "provision.py"),
		"--manifest", filepath.Join(home, ".scion", "harness", "manifest.json"))
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
		"PYTHONDONTWRITEBYTECODE=1",
		"SCION_HARNESS=claude",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The real claude provision.py, staged by a real Start, succeeds on the
// first start and again on a restart whose resolution carries only ambient
// env; it used to exit 1 with "no valid auth method found".
func TestStart_ClaudeProvisionSucceedsTwiceAcrossRestart(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	e, _ := newClaudeRestartEnv(t)
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "restart-prov", ProjectPath: e.scion, HarnessConfig: "claude",
		Env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test-0123456789abcdefghij", "GOOGLE_CLOUD_LOCATION": "us-east5"}}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "restart-prov")
	if out, err := runStagedClaudeProvision(t, home); err != nil {
		t.Fatalf("first provision failed: %v\n%s", err, out)
	}

	opts.Env = map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5", "CLOUD_ML_REGION": "us-east5"}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	out, err := runStagedClaudeProvision(t, home)
	if err != nil {
		t.Fatalf("provision after restart failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "outputs", "resolved-auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resolved struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Method != "api-key" {
		t.Errorf("auth method after restart = %q, want api-key\n%s", resolved.Method, out)
	}
}

func provisionedMethod(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "outputs", "resolved-auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resolved struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &resolved); err != nil {
		t.Fatal(err)
	}
	return resolved.Method
}

// startAndProvision runs Start with env and then the staged claude
// provision.py, returning the auth method it selected.
func startAndProvision(t *testing.T, mgr Manager, opts api.StartOptions, env map[string]string, home string) string {
	t.Helper()
	opts.Env = env
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if out, err := runStagedClaudeProvision(t, home); err != nil {
		t.Fatalf("provision failed: %v\n%s", err, out)
	}
	return provisionedMethod(t, home)
}

// A restart that switches the agent from an API key to Vertex AI with an
// ADC file selects vertex-ai: the stale recorded API key is not carried.
func TestStart_ClaudeRestartSwitchToVertexWithADC(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	e, _ := newClaudeRestartEnv(t)
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "switch-vertex", ProjectPath: e.scion, HarnessConfig: "claude"}
	home := config.GetAgentHomePath(e.scion, "switch-vertex")
	if m := startAndProvision(t, mgr, opts, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test-0123456789abcdefghij"}, home); m != "api-key" {
		t.Fatalf("first start method = %q, want api-key", m)
	}
	m := startAndProvision(t, mgr, opts, map[string]string{
		"GOOGLE_CLOUD_PROJECT":           "p",
		"CLOUD_ML_REGION":                "us-east5",
		"GOOGLE_APPLICATION_CREDENTIALS": adc,
	}, home)
	if m != "vertex-ai" {
		t.Errorf("restart method = %q, want vertex-ai (stale API key carried)", m)
	}
}

// With a GCP service account assigned (project and region, no ADC file), a
// restart selects the same method as the first start with the same inputs:
// the recorded API key (recorded because it was part of the first start's
// inputs) wins over vertex-ai in the provisioner's selection order.
func TestStart_ClaudeRestartWithServiceAccountMatchesFirstStart(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	e, _ := newClaudeRestartEnv(t)
	t.Setenv("SCION_METADATA_MODE", "assign")
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "sa-vertex", ProjectPath: e.scion, HarnessConfig: "claude"}
	home := config.GetAgentHomePath(e.scion, "sa-vertex")
	first := startAndProvision(t, mgr, opts, map[string]string{
		"ANTHROPIC_API_KEY":    "sk-ant-test-0123456789abcdefghij",
		"GOOGLE_CLOUD_PROJECT": "p",
		"CLOUD_ML_REGION":      "us-east5",
	}, home)
	restart := startAndProvision(t, mgr, opts, map[string]string{
		"GOOGLE_CLOUD_PROJECT": "p",
		"CLOUD_ML_REGION":      "us-east5",
	}, home)
	if first != "api-key" || restart != first {
		t.Errorf("first start method = %q, restart method = %q; want both api-key", first, restart)
	}
}
