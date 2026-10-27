/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// runProvisionCaptureRemote runs runProvision for a shared-plain workspace
// with provision.ProvisionShared replaced, with token in GITHUB_TOKEN and
// tokenRemote in api.EnvGitTokenRemote ("" leaves it unset), and returns the
// input ProvisionShared was given.
func runProvisionCaptureRemote(t *testing.T, cloneURL, token, tokenRemote string) provision.ProvisionInput {
	t.Helper()
	wsDir := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(wsDir, 0o770); err != nil {
		t.Fatal(err)
	}
	oldWorkspace, oldMode, oldUID, oldGID := provisionWorkspace, provisionMode, provisionUID, provisionGID
	oldShared := provisionShared
	t.Cleanup(func() {
		provisionWorkspace, provisionMode, provisionUID, provisionGID = oldWorkspace, oldMode, oldUID, oldGID
		provisionShared = oldShared
	})
	provisionWorkspace = wsDir
	provisionMode = "shared-plain"
	provisionUID = os.Getuid()
	provisionGID = os.Getgid()

	var got provision.ProvisionInput
	called := false
	provisionShared = func(in provision.ProvisionInput) error {
		got = in
		called = true
		return nil
	}
	t.Setenv("SCION_CLONE_URL", cloneURL)
	t.Setenv("SCION_CLONE_BRANCH", "main")
	t.Setenv("SCION_WORKSPACE_MODE", "")
	t.Setenv("SCION_PROVISION_STATE_DIR", "")
	t.Setenv("SCION_PROJECT_ID", "test-proj")
	t.Setenv(provision.GitTokenEnv, token)
	t.Setenv(api.EnvGitTokenRemote, tokenRemote)
	if tokenRemote == "" {
		if err := os.Unsetenv(api.EnvGitTokenRemote); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_COUNT", "")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if !called {
		t.Fatal("runProvision did not call ProvisionShared")
	}
	// The credential helper is given to the clone command only, never to
	// this process (later steps must not inherit it).
	if v := os.Getenv("GIT_CONFIG_COUNT"); v != "" {
		t.Errorf("GIT_CONFIG_COUNT set in the provision process env: %q", v)
	}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GIT_CONFIG_VALUE_") && strings.Contains(e, "credential") {
			t.Errorf("credential helper entry in the provision process env: %s", strings.SplitN(e, "=", 2)[0])
		}
	}
	return got
}

// The init container's clone gets the token only through CloneToken, bound
// to the project's recorded remote (CloneTokenRemote): never the unbound
// CloneWithToken, and none at all without a token remote or for a clone URL
// that does not name it.
func TestRunProvision_CloneTokenBoundToRemote(t *testing.T) {
	const remote = "github.com/org/private"
	for _, tc := range []struct {
		name, url, token, remote string
		want                     bool
	}{
		{"token, clone URL and matching remote", "https://github.com/org/private.git", "test-token-value", remote, true},
		{"token and clone URL, no remote", "https://github.com/org/private.git", "test-token-value", "", false},
		{"label naming another host", "https://mirror.example/org/private.git", "test-token-value", remote, false},
		{"no token", "https://github.com/org/private.git", "", remote, false},
		{"token without clone URL", "", "test-token-value", remote, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := runProvisionCaptureRemote(t, tc.url, tc.token, tc.remote)
			if in.CloneWithToken {
				t.Error("CloneWithToken (unbound) must never be set")
			}
			if got := in.CloneToken != ""; got != tc.want {
				t.Errorf("CloneToken set = %v, want %v", got, tc.want)
			}
			if in.CloneTokenRemote != tc.remote {
				t.Errorf("CloneTokenRemote = %q, want %q", in.CloneTokenRemote, tc.remote)
			}
			if in.GitClone != nil && strings.Contains(in.GitClone.URL, "test-token-value") {
				t.Error("the token must not be added to the clone URL")
			}
		})
	}
}
