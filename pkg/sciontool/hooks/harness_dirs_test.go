/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveHarnessDirs_UnsetKeepsBundleDirs(t *testing.T) {
	t.Setenv(HarnessOutputsDirEnv, "")
	t.Setenv(HarnessSecretsDirEnv, "")
	bundle := "/home/scion/.scion/harness"
	d, err := ResolveHarnessDirs(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if d.Outputs != bundle+"/outputs" || d.Secrets != bundle+"/secrets" {
		t.Errorf("dirs = %+v, want the bundle's outputs and secrets", d)
	}
	for _, p := range []string{bundle + "/outputs/env.json", bundle + "/secrets/KEY", "/elsewhere/x"} {
		if got := d.OutputPath(p); got != p {
			t.Errorf("OutputPath(%q) = %q, want unchanged", p, got)
		}
		if got := d.SecretPath(p); got != p {
			t.Errorf("SecretPath(%q) = %q, want unchanged", p, got)
		}
	}
	if len(d.ExtraRoots()) != 0 {
		t.Errorf("ExtraRoots = %v, want none", d.ExtraRoots())
	}
}

func TestResolveHarnessDirs_EnvMovesDirs(t *testing.T) {
	t.Setenv(HarnessOutputsDirEnv, "/run/scion/mem/outputs/")
	t.Setenv(HarnessSecretsDirEnv, "/run/scion/mem/harness-secrets")
	bundle := "/home/scion/.scion/harness"
	d, err := ResolveHarnessDirs(bundle)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		fn       func(string) string
		in, want string
	}{
		{d.OutputPath, bundle + "/outputs/env.json", "/run/scion/mem/outputs/env.json"},
		{d.OutputPath, bundle + "/outputs/sub/status.json", "/run/scion/mem/outputs/sub/status.json"},
		{d.OutputPath, bundle + "/outputs", "/run/scion/mem/outputs"},
		{d.OutputPath, bundle + "/outputsx/env.json", bundle + "/outputsx/env.json"},
		{d.OutputPath, bundle + "/inputs/x", bundle + "/inputs/x"},
		{d.OutputPath, "", ""},
		{d.SecretPath, bundle + "/secrets/AGY_TOKEN", "/run/scion/mem/harness-secrets/AGY_TOKEN"},
		{d.SecretPath, bundle + "/outputs/env.json", bundle + "/outputs/env.json"},
	}
	for _, c := range cases {
		if got := c.fn(c.in); got != c.want {
			t.Errorf("remap(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if want := []string{"/run/scion/mem/outputs", "/run/scion/mem/harness-secrets"}; !reflect.DeepEqual(d.ExtraRoots(), want) {
		t.Errorf("ExtraRoots = %v, want %v", d.ExtraRoots(), want)
	}
}

func TestResolveHarnessDirs_RelativeValueRejected(t *testing.T) {
	for _, env := range []string{HarnessOutputsDirEnv, HarnessSecretsDirEnv} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(HarnessOutputsDirEnv, "")
			t.Setenv(HarnessSecretsDirEnv, "")
			t.Setenv(env, "run/outputs")
			_, err := ResolveHarnessDirs("/b")
			if err == nil || !strings.Contains(err.Error(), env) {
				t.Errorf("err = %v, want %s named", err, env)
			}
		})
	}
}

func TestResolveEnvOverlay(t *testing.T) {
	home := "/home/scion"
	req := HarnessManifestRequirement{
		Required:       true,
		EnvOverlayPath: "$HOME/.scion/harness/outputs/env.json",
		BundleDir:      filepath.Join(home, ".scion", "harness"),
	}

	t.Setenv(HarnessOutputsDirEnv, "")
	t.Setenv(HarnessSecretsDirEnv, "")
	p, roots, err := req.ResolveEnvOverlay(home)
	if err != nil {
		t.Fatal(err)
	}
	if p != ResolveContainerPath(req.EnvOverlayPath, home) || p != "/home/scion/.scion/harness/outputs/env.json" {
		t.Errorf("unset: path = %q", p)
	}
	if !reflect.DeepEqual(roots, []string{req.BundleDir, home}) {
		t.Errorf("unset: roots = %v", roots)
	}

	t.Setenv(HarnessOutputsDirEnv, "/run/scion/mem/outputs")
	t.Setenv(HarnessSecretsDirEnv, "/run/scion/mem/harness-secrets")
	p, roots, err = req.ResolveEnvOverlay(home)
	if err != nil {
		t.Fatal(err)
	}
	if p != "/run/scion/mem/outputs/env.json" {
		t.Errorf("set: path = %q", p)
	}
	want := []string{req.BundleDir, home, "/run/scion/mem/outputs", "/run/scion/mem/harness-secrets"}
	if !reflect.DeepEqual(roots, want) {
		t.Errorf("set: roots = %v, want %v", roots, want)
	}

	t.Setenv(HarnessOutputsDirEnv, "rel")
	if _, _, err := req.ResolveEnvOverlay(home); err == nil {
		t.Error("relative outputs dir accepted")
	}
}
