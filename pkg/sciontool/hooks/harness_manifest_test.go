/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadHarnessManifestRequirement_NoManifestNotRequired(t *testing.T) {
	got, err := LoadHarnessManifestRequirement(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Required {
		t.Fatal("expected Required=false when manifest absent")
	}
}

func TestLoadHarnessManifestRequirement_ContainerScriptIsRequired(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
		"schema_version": 1,
		"harness_config": {
			"provisioner": {"type": "container-script", "interface_version": 1}
		},
		"outputs": {"env": "$HOME/.scion/harness/outputs/env.json"}
	}`
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadHarnessManifestRequirement(home)
	if err != nil {
		t.Fatalf("LoadHarnessManifestRequirement: %v", err)
	}
	if !got.Required {
		t.Fatal("expected Required=true for container-script provisioner")
	}
	if got.EnvOverlayPath != "$HOME/.scion/harness/outputs/env.json" {
		t.Fatalf("unexpected env overlay path: %q", got.EnvOverlayPath)
	}
	if got.BundleDir != bundle {
		t.Fatalf("expected BundleDir=%q, got %q", bundle, got.BundleDir)
	}
}

func TestLoadHarnessManifestRequirement_LifecycleEventsHonored(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	// Provisioner explicitly opts out of pre-start.
	manifest := `{
		"harness_config": {
			"provisioner": {"type": "container-script", "lifecycle_events": ["post-start"]}
		}
	}`
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadHarnessManifestRequirement(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Required {
		t.Fatal("expected Required=false when pre-start not in lifecycle_events")
	}
}

func TestLoadHarnessManifestRequirement_BuiltinRequiredWithError(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"harness_config": {"harness": "claude", "provisioner": {"type": "builtin"}}}`
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadHarnessManifestRequirement(home)
	if err == nil {
		t.Fatal("expected an error for the unusable builtin provisioner")
	}
	if !got.Required {
		t.Fatal("expected Required=true for builtin provisioner")
	}
	for _, want := range []string{`harness type "claude"`, `"builtin"`, "scion harness-config upgrade <harness-config> --activate-script", "harnesses/claude"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// A manifest without a harness type does not print an empty type.
func TestLoadHarnessManifestRequirement_BuiltinWithoutHarnessType(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"harness_config": {"provisioner": {"type": "builtin"}}}`
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadHarnessManifestRequirement(home)
	if err == nil || !got.Required {
		t.Fatalf("expected Required=true and an error, got %+v, %v", got, err)
	}
	if !strings.Contains(err.Error(), "the agent's harness-config is staged") || strings.Contains(err.Error(), `""`) || strings.Contains(err.Error(), "harnesses/,") {
		t.Errorf("unexpected message: %v", err)
	}
}

func TestLoadHarnessManifestRequirement_MalformedManifestFails(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadHarnessManifestRequirement(home)
	if err == nil {
		t.Fatal("expected parse error for malformed manifest")
	}
}

func TestResolveContainerPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		home string
		want string
	}{
		{"home prefix", "$HOME/.scion/harness/outputs/env.json", "/agent", "/agent/.scion/harness/outputs/env.json"},
		{"absolute path", "/tmp/x", "/agent", "/tmp/x"},
		{"empty path", "", "/agent", ""},
		{"home alone", "$HOME", "/agent", "/agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveContainerPath(tc.path, tc.home); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
