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

// ptone/scion#3810: a restart whose resolution carries only ambient env (a
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
// env (ptone/scion#3810: it used to exit 1 with "no valid auth method").
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
