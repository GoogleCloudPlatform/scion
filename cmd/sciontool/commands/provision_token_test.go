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

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// runProvisionCapture runs runProvision for a shared-plain workspace with
// provision.ProvisionShared replaced, and returns the input it was given.
func runProvisionCapture(t *testing.T, cloneURL, token string) provision.ProvisionInput {
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

func TestRunProvision_CloneWithToken(t *testing.T) {
	for _, tc := range []struct {
		name, url, token string
		want             bool
	}{
		{"token and clone URL", "https://github.com/org/private.git", "test-token-value", true},
		{"no token", "https://github.com/org/public.git", "", false},
		{"token without clone URL", "", "test-token-value", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := runProvisionCapture(t, tc.url, tc.token)
			if in.CloneWithToken != tc.want {
				t.Errorf("CloneWithToken = %v, want %v", in.CloneWithToken, tc.want)
			}
			if in.GitClone != nil && strings.Contains(in.GitClone.URL, "test-token-value") {
				t.Error("the token must not be added to the clone URL")
			}
		})
	}
}
