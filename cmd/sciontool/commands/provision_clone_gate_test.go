/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// The Kubernetes init container's clone goes through the real
// ProvisionShared, which refuses a local path, file://, http://, a
// <transport>::<address> URL and a value starting with "-", and every call
// is made with HostGitHardened set.
func TestRunProvision_RefusesHostCloneURLs(t *testing.T) {
	local := filepath.Join(t.TempDir(), "repo.git")
	for name, cloneURL := range map[string]string{
		"local path":   local,
		"file":         "file://" + local,
		"http":         "http://git.example.com/org/repo.git",
		"transport":    "ext::sh -c touch% /tmp/x",
		"leading dash": "--upload-pack=touch /tmp/x",
	} {
		t.Run(name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			if err := os.MkdirAll(workspace, 0o770); err != nil {
				t.Fatal(err)
			}
			setupProvisionCmd(t, workspace, "shared-plain", cloneURL)
			var inputs []provision.ProvisionInput
			orig := provisionShared
			provisionShared = func(in provision.ProvisionInput) error {
				inputs = append(inputs, in)
				return orig(in)
			}
			t.Cleanup(func() { provisionShared = orig })

			err := runProvision(context.Background())
			if !errors.Is(err, provision.ErrHostCloneURLRefused) {
				t.Fatalf("runProvision error = %v, want the clone URL refused", err)
			}
			if len(inputs) != 1 || !inputs[0].HostGitHardened {
				t.Fatalf("provisioning inputs = %d, want one hardened call", len(inputs))
			}
			if _, statErr := os.Stat(filepath.Join(workspace, ".git")); !os.IsNotExist(statErr) {
				t.Errorf("nothing may be cloned for a refused URL (stat .git: %v)", statErr)
			}
		})
	}
}

// Pins HostGitHardened on the init container's provisioning input.
func TestRunProvision_HostCloneHardened(t *testing.T) {
	in := runProvisionCaptureRemote(t, "https://github.com/org/repo.git", "", "")
	if !in.HostGitHardened {
		t.Error("the init container's clone must be hardened (HostGitHardened)")
	}
}
