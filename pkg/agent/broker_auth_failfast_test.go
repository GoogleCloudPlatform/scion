// Copyright 2026 The Scion Authors.

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Synthetic placeholder credentials; never real values.
const (
	failfastAPIKey     = "sk-ant-test-placeholder-0000000000"
	failfastOAuthToken = "oauth-test-placeholder-0000000000"
)

// stripNoAuth removes the top-level no_auth block from the copied claude
// harness-config, giving a harness that does not allow starting without
// credentials.
func stripNoAuth(t *testing.T, hcDir string) {
	t.Helper()
	path := filepath.Join(hcDir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^no_auth:\n(?:[ \t]+.*\n|\n)*`)
	out := re.ReplaceAll(data, nil)
	if string(out) == string(data) || strings.Contains(string(out), "\nno_auth:") {
		t.Fatal("fixture: no_auth block not removed from claude config.yaml")
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// failfastManager is policyTestManager with a named mock runtime, so tests
// can show that the check does not depend on the runtime.
func failfastManager(runs *int, runtimeName string) Manager {
	return NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			if runs != nil {
				*runs++
			}
			return "mock-id", nil
		},
	})
}

func brokerStart(t *testing.T, scion, name string) api.StartOptions {
	t.Helper()
	return api.StartOptions{Name: name, ProjectPath: scion, HarnessConfig: "claude", BrokerMode: true}
}

func assertNoAuthSatisfied(t *testing.T, err error, runs int, wantInMsg ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("Start succeeded, want a no-auth-satisfied error")
	}
	if !errors.Is(err, harness.ErrNoAuthSatisfied) {
		t.Fatalf("Start error = %v, want ErrNoAuthSatisfied", err)
	}
	for _, s := range wantInMsg {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %q", err, s)
		}
	}
	for _, secret := range []string{failfastAPIKey, failfastOAuthToken} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks a credential value: %q", err)
		}
	}
	if runs != 0 {
		t.Errorf("container ran %d times, want 0 (fail before launch)", runs)
	}
}

// Each kind of satisfying credential lets the broker start proceed, on a
// harness that does not allow starting without credentials, on any runtime.
func TestStart_BrokerStartsWithEachSatisfyingCredential(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		env      map[string]string
		secrets  []api.ResolvedSecret
	}{
		{name: "api-key env", env: map[string]string{"ANTHROPIC_API_KEY": failfastAPIKey}},
		{name: "oauth-token env", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": failfastOAuthToken}},
		{name: "explicit api-key env", explicit: "api-key", env: map[string]string{"ANTHROPIC_API_KEY": failfastAPIKey}},
		{name: "vertex-ai project and region", env: map[string]string{"GOOGLE_CLOUD_PROJECT": "placeholder-project", "GOOGLE_CLOUD_REGION": "us-east5"}},
		{name: "auth-file file secret", secrets: []api.ResolvedSecret{{
			Name: "CLAUDE_AUTH", Type: "file", Target: "/home/scion/.claude/.credentials.json", Value: `{"placeholder":true}`,
		}}},
		{name: "explicit auth-file file secret", explicit: "auth-file", secrets: []api.ResolvedSecret{{
			Name: "CLAUDE_AUTH", Type: "file", Target: "/home/scion/.claude/.credentials.json", Value: `{"placeholder":true}`,
		}}},
	}
	for _, rt := range []string{"docker", "kubernetes"} {
		for _, tc := range cases {
			t.Run(rt+"/"+tc.name, func(t *testing.T) {
				e, hcDir := newClaudeRestartEnv(t)
				stripNoAuth(t, hcDir)
				runs := 0
				opts := brokerStart(t, e.scion, "ff-ok")
				opts.HarnessAuth = tc.explicit
				opts.Env = tc.env
				opts.ResolvedSecrets = tc.secrets
				if _, err := failfastManager(&runs, rt).Start(context.Background(), opts); err != nil {
					t.Fatalf("Start: %v", err)
				}
				if runs != 1 {
					t.Errorf("container ran %d times, want 1", runs)
				}
			})
		}
	}
}

// A harness whose no_auth behaviour allows it starts without credentials.
func TestStart_BrokerStartsWithoutCredentialsWhenNoAuthAllowed(t *testing.T) {
	e, _ := newClaudeRestartEnv(t) // claude ships no_auth: drop-to-shell
	runs := 0
	if _, err := failfastManager(&runs, "docker").Start(context.Background(), brokerStart(t, e.scion, "ff-noauth")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if runs != 1 {
		t.Errorf("container ran %d times, want 1", runs)
	}
}

// Restarting a stopped agent whose restart carries only ambient env (the
// ptone/scion#3810 path) still starts: the recorded credential is restaged
// and satisfies the persisted auth type.
func TestStart_BrokerRestartOfStoppedAgentKeepsRecordedCredential(t *testing.T) {
	e, hcDir := newClaudeRestartEnv(t)
	stripNoAuth(t, hcDir)
	mgr := failfastManager(nil, "docker")
	opts := brokerStart(t, e.scion, "ff-restart")
	opts.Env = map[string]string{"ANTHROPIC_API_KEY": failfastAPIKey, "GOOGLE_CLOUD_LOCATION": "us-east5"}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	restart := opts
	restart.Env = map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5"}
	runs := 0
	if _, err := failfastManager(&runs, "docker").Start(context.Background(), restart); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if runs != 1 {
		t.Errorf("container ran %d times, want 1", runs)
	}
}

// Outside broker mode the check does not run (scope: broker start path).
func TestStart_LocalModeDoesNotFailFast(t *testing.T) {
	e, _ := newClaudeRestartEnv(t)
	runs := 0
	opts := api.StartOptions{Name: "ff-local", ProjectPath: e.scion, HarnessConfig: "claude", HarnessAuth: "api-key"}
	if _, err := failfastManager(&runs, "docker").Start(context.Background(), opts); err != nil {
		t.Fatalf("local Start: %v", err)
	}
	if runs != 1 {
		t.Errorf("container ran %d times, want 1", runs)
	}
}

// An explicit env-only auth type with none of its keys staged fails the
// broker start before any container is created, on any runtime.
func TestStart_BrokerFailsFastWhenEnvOnlyAuthTypeUnsatisfied(t *testing.T) {
	for _, rt := range []string{"docker", "kubernetes"} {
		t.Run(rt, func(t *testing.T) {
			e, _ := newClaudeRestartEnv(t)
			runs := 0
			opts := brokerStart(t, e.scion, "ff-explicit")
			opts.HarnessAuth = "api-key"
			opts.Env = map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5"} // ambient only
			_, err := failfastManager(&runs, rt).Start(context.Background(), opts)
			assertNoAuthSatisfied(t, err, runs, "claude", `"api-key"`, "ANTHROPIC_API_KEY", "api-key, auth-file, oauth-token, vertex-ai")
		})
	}
}

// Auth types that declare required files are never rejected by the broker:
// the file can reach the container by routes the broker cannot see. This
// holds on any runtime, for an explicit file type and for a harness without
// no_auth whose declared types include file types.
func TestStart_BrokerNeverRejectsFileAuthTypes(t *testing.T) {
	for _, rt := range []string{"docker", "kubernetes"} {
		t.Run(rt+"/explicit auth-file", func(t *testing.T) {
			e, _ := newClaudeRestartEnv(t)
			runs := 0
			opts := brokerStart(t, e.scion, "ff-file")
			opts.HarnessAuth = "auth-file"
			if _, err := failfastManager(&runs, rt).Start(context.Background(), opts); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if runs != 1 {
				t.Errorf("container ran %d times, want 1", runs)
			}
		})
		t.Run(rt+"/no no_auth, nothing staged", func(t *testing.T) {
			e, hcDir := newClaudeRestartEnv(t)
			stripNoAuth(t, hcDir)
			runs := 0
			if _, err := failfastManager(&runs, rt).Start(context.Background(), brokerStart(t, e.scion, "ff-forbidden")); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if runs != 1 {
				t.Errorf("container ran %d times, want 1", runs)
			}
		})
	}
}
