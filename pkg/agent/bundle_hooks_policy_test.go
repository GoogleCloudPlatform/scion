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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

var bundleHookTestRelPaths = []string{
	".scion/hooks/pre-start",
	".scion/hooks/pre-start.sh",
	".scion/hooks/pre-start.d/10-bundle",
	".scion/hooks/post-start.d/10-bundle",
	".scion/hooks/pre-stop.d/10-bundle",
	".scion/hooks/session-end.d/10-bundle",
}

func writeDeclBundleHooksHC(t *testing.T, e *policyTestEnv, name string) string {
	t.Helper()
	dir := filepath.Join(e.scion, "harness-configs", name)
	writePolicyHC(t, dir, policyTestDecl)
	for _, rel := range bundleHookTestRelPaths {
		tag := filepath.Base(filepath.Dir(rel)) + "/" + filepath.Base(rel)
		p := filepath.Join(dir, "home", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\nprintf '%s\\n' '" + tag + "' >> \"$HOOK_MARKER_FILE\"\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runAllLifecycleEvents(t *testing.T, agentHome string) string {
	t.Helper()
	t.Setenv("SCION_HOOKS_DIR", t.TempDir())
	marker := filepath.Join(t.TempDir(), "ran.txt")
	t.Setenv("HOOK_MARKER_FILE", marker)

	lm := hooks.NewLifecycleManager()
	lm.AgentHome = agentHome
	lm.AddHooksDir(filepath.Join(agentHome, ".scion", "hooks"))
	if err := lm.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}
	if err := lm.RunPostStart(); err != nil {
		t.Fatalf("RunPostStart: %v", err)
	}
	if err := lm.RunPreStop(); err != nil {
		t.Fatalf("RunPreStop: %v", err)
	}
	if err := lm.RunSessionEnd(); err != nil {
		t.Fatalf("RunSessionEnd: %v", err)
	}
	data, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHarnessConfigPolicy_DisallowedSuppressesBundleHomeHooks(t *testing.T) {
	assertBundleHooksAbsent := func(t *testing.T, e *policyTestEnv, agentName string) {
		t.Helper()
		home := config.GetAgentHomePath(e.scion, agentName)
		for _, rel := range bundleHookTestRelPaths {
			p := filepath.Join(home, filepath.FromSlash(rel))
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("bundle hook %s should not exist in agent home (stat err=%v)", rel, err)
			}
		}
		if runtime.GOOS != "windows" {
			if got := runAllLifecycleEvents(t, home); got != "" {
				t.Errorf("bundle hooks executed under restrictive policy:\n%s", got)
			}
		}
	}

	t.Run("ProvisionAgent disallowed", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
		ctx := (&recordingPolicy{refuse: true}).ctx()
		if _, _, _, err := ProvisionAgent(ctx, "prov-disallowed", "", "", "hc-decl-hooks", e.scion, "", "", "", ""); err != nil {
			t.Fatalf("ProvisionAgent: %v", err)
		}
		assertBundleHooksAbsent(t, e, "prov-disallowed")
	})

	t.Run("Start disallowed", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
		runs := 0
		ctx := (&recordingPolicy{refuse: true}).ctx()
		if _, err := policyTestManager(&runs).Start(ctx, api.StartOptions{
			Name: "start-disallowed", ProjectPath: e.scion, HarnessConfig: "hc-decl-hooks", NoAuth: true,
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if runs != 1 {
			t.Errorf("runs = %d, want 1", runs)
		}
		assertBundleHooksAbsent(t, e, "start-disallowed")
	})

	t.Run("Relaunch disallowed clears previously staged bundle hooks", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
		runs := 0
		mgr := policyTestManager(&runs)
		opts := api.StartOptions{Name: "relaunch-disallowed", ProjectPath: e.scion, HarnessConfig: "hc-decl-hooks", NoAuth: true}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("first Start (allowed): %v", err)
		}
		home := config.GetAgentHomePath(e.scion, "relaunch-disallowed")
		for _, rel := range bundleHookTestRelPaths {
			if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(rel))); err != nil {
				t.Fatalf("expected bundle hook %s after allowed start: %v", rel, err)
			}
		}

		if _, err := mgr.Start((&recordingPolicy{refuse: true}).ctx(), opts); err != nil {
			t.Fatalf("relaunch under restrictive policy: %v", err)
		}
		if runs != 2 {
			t.Errorf("runs = %d, want 2", runs)
		}
		assertBundleHooksAbsent(t, e, "relaunch-disallowed")
	})

	t.Run("Relaunch disallowed without prior record clears bundle hooks from resolved dir", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
		runs := 0
		mgr := policyTestManager(&runs)
		opts := api.StartOptions{Name: "legacy-relaunch", ProjectPath: e.scion, HarnessConfig: "hc-decl-hooks", NoAuth: true}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("first Start (allowed): %v", err)
		}
		agentDir := filepath.Join(e.scion, "agents", "legacy-relaunch")
		_ = os.Remove(filepath.Join(agentDir, bundleHooksRecordFileName))

		if _, err := mgr.Start((&recordingPolicy{refuse: true}).ctx(), opts); err != nil {
			t.Fatalf("relaunch without prior record: %v", err)
		}
		assertBundleHooksAbsent(t, e, "legacy-relaunch")
	})

	t.Run("Corrupt bundle hooks record under restrictive policy fails closed", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
		runs := 0
		mgr := policyTestManager(&runs)
		opts := api.StartOptions{Name: "corrupt-record", ProjectPath: e.scion, HarnessConfig: "hc-decl-hooks", NoAuth: true}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("first Start (allowed): %v", err)
		}
		agentDir := filepath.Join(e.scion, "agents", "corrupt-record")
		if err := os.WriteFile(filepath.Join(agentDir, bundleHooksRecordFileName), []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := mgr.Start((&recordingPolicy{refuse: true}).ctx(), opts); err == nil {
			t.Fatal("expected Start to fail closed when bundle hooks record cannot be parsed")
		}
		if runs != 1 {
			t.Errorf("container must not launch when record check fails closed: runs=%d, want 1", runs)
		}
	})

	t.Run("Uncertain stat on bundle home under restrictive policy fails closed", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-stat-err", policyTestDecl)
		hcDir, err := config.LoadHarnessConfigDir(filepath.Join(e.scion, "harness-configs", "hc-stat-err"))
		if err != nil {
			t.Fatal(err)
		}
		notADir := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		hcDir.Path = filepath.Join(notADir, "hc")
		home := t.TempDir()
		ctx := (&recordingPolicy{refuse: true}).ctx()

		if _, _, err := reconcileBundleHooksOnProvision(ctx, "stat-prov", "hc-stat-err", hcDir, home); err == nil {
			t.Fatal("expected reconcileBundleHooksOnProvision to fail closed on uncertain bundle home stat")
		}
		if err := clearDisallowedBundleHooksOnStart(ctx, "stat-start", "hc-stat-err", hcDir, nil, t.TempDir(), home, nil); err == nil {
			t.Fatal("expected clearDisallowedBundleHooksOnStart to fail closed on uncertain bundle home stat")
		}
	})
}

func TestHarnessConfigPolicy_AllowedPreservesBundleHomeHooks(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
	}{
		{"no policy", context.Background},
		{"permissive policy", func() context.Context { return (&recordingPolicy{refuse: false}).ctx() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPolicyTestEnv(t)
			writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
			if _, _, _, err := ProvisionAgent(tc.ctx(), "prov-ok", "", "", "hc-decl-hooks", e.scion, "", "", "", ""); err != nil {
				t.Fatalf("ProvisionAgent: %v", err)
			}
			provHome := config.GetAgentHomePath(e.scion, "prov-ok")
			for _, rel := range bundleHookTestRelPaths {
				info, err := os.Stat(filepath.Join(provHome, filepath.FromSlash(rel)))
				if err != nil || info.Mode()&0o111 == 0 {
					t.Errorf("ProvisionAgent(%s): expected executable %s, got info=%v err=%v", tc.name, rel, info, err)
				}
			}

			runs := 0
			mgr := policyTestManager(&runs)
			opts := api.StartOptions{Name: "start-ok", ProjectPath: e.scion, HarnessConfig: "hc-decl-hooks", NoAuth: true}
			for i := 1; i <= 2; i++ {
				if _, err := mgr.Start(tc.ctx(), opts); err != nil {
					t.Fatalf("Start #%d: %v", i, err)
				}
			}
			if runs != 2 {
				t.Errorf("runs = %d, want 2", runs)
			}
			startHome := config.GetAgentHomePath(e.scion, "start-ok")
			for _, rel := range bundleHookTestRelPaths {
				info, err := os.Stat(filepath.Join(startHome, filepath.FromSlash(rel)))
				if err != nil || info.Mode()&0o111 == 0 {
					t.Errorf("Start(%s): expected executable %s, got info=%v err=%v", tc.name, rel, info, err)
				}
			}
		})
	}
}

func TestHarnessConfigPolicy_BundleHomeHooksLeaveTemplateAndProjectHooksIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("project pre-start hook staging requires unix confined home writes")
	}

	setupFixture := func(t *testing.T) *policyTestEnv {
		t.Helper()
		e := newPolicyTestEnv(t)
		hcDir := writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
		sharedInHC := filepath.Join(hcDir, "home", ".scion", "hooks", "pre-start.d", "15-shared")
		if err := os.WriteFile(sharedInHC, []byte("#!/bin/sh\nprintf 'bundle-shared\\n' >> \"$HOOK_MARKER_FILE\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		tplDir := e.template(t, "tpl-hooks", "harness_config: hc-decl-hooks\n")
		for name, tag := range map[string]string{
			"15-shared": "tpl-shared",
			"20-tpl":    "tpl-only",
		} {
			p := filepath.Join(tplDir, "home", ".scion", "hooks", "pre-start.d", name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' '"+tag+"' >> \"$HOOK_MARKER_FILE\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return e
	}

	assertOnlyTemplateAndProjectHooks := func(t *testing.T, e *policyTestEnv, agentName string) {
		t.Helper()
		home := config.GetAgentHomePath(e.scion, agentName)
		for _, rel := range bundleHookTestRelPaths {
			if _, err := os.Lstat(filepath.Join(home, filepath.FromSlash(rel))); !os.IsNotExist(err) {
				t.Errorf("bundle hook %s should be absent, got err=%v", rel, err)
			}
		}
		for _, keep := range []string{
			".scion/hooks/pre-start.d/15-shared",
			".scion/hooks/pre-start.d/20-tpl",
			".scion/hooks/pre-start.d/30-project-custom",
		} {
			info, err := os.Stat(filepath.Join(home, filepath.FromSlash(keep)))
			if err != nil || info.Mode()&0o111 == 0 {
				t.Fatalf("expected executable %s, got info=%v err=%v", keep, info, err)
			}
		}
		got := runAllLifecycleEvents(t, home)
		want := "tpl-shared\ntpl-only\nproject-custom\n"
		if got != want {
			t.Errorf("lifecycle execution order/output = %q, want %q", got, want)
		}
	}

	projectHook := "#!/bin/sh\nprintf 'project-custom\\n' >> \"$HOOK_MARKER_FILE\"\n"

	t.Run("Provision and Start under restrictive policy", func(t *testing.T) {
		e := setupFixture(t)
		runs := 0
		mgr := policyTestManager(&runs)
		ctx := (&recordingPolicy{refuse: true}).ctx()
		opts := api.StartOptions{
			Name:                      "direct-disallowed",
			ProjectPath:               e.scion,
			Template:                  "tpl-hooks",
			NoAuth:                    true,
			ProjectPreStartHookScript: projectHook,
		}
		if _, err := mgr.Provision(ctx, opts); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		assertOnlyTemplateAndProjectHooks(t, e, "direct-disallowed")

		if _, err := mgr.Start(ctx, opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		assertOnlyTemplateAndProjectHooks(t, e, "direct-disallowed")
	})

	t.Run("Allowed Start then restrictive relaunch", func(t *testing.T) {
		e := setupFixture(t)
		runs := 0
		mgr := policyTestManager(&runs)
		opts := api.StartOptions{
			Name:                      "relaunch-preserved",
			ProjectPath:               e.scion,
			Template:                  "tpl-hooks",
			NoAuth:                    true,
			ProjectPreStartHookScript: projectHook,
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("first Start (allowed): %v", err)
		}
		if _, err := mgr.Start((&recordingPolicy{refuse: true}).ctx(), opts); err != nil {
			t.Fatalf("second Start (restrictive): %v", err)
		}
		assertOnlyTemplateAndProjectHooks(t, e, "relaunch-preserved")
	})
}

func TestHarnessConfigPolicy_BundleHomeHooksConfinedToAgentHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix symlink confinement test")
	}

	t.Run("linked parent component fails closed", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
		runs := 0
		mgr := policyTestManager(&runs)
		opts := api.StartOptions{Name: "confined-parent", ProjectPath: e.scion, HarnessConfig: "hc-decl-hooks", NoAuth: true}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("first Start: %v", err)
		}

		outside := t.TempDir()
		outsideFile := filepath.Join(outside, "10-bundle")
		if err := os.WriteFile(outsideFile, []byte("keep"), 0o755); err != nil {
			t.Fatal(err)
		}
		home := config.GetAgentHomePath(e.scion, "confined-parent")
		postStartDir := filepath.Join(home, ".scion", "hooks", "post-start.d")
		if err := os.RemoveAll(postStartDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, postStartDir); err != nil {
			t.Fatal(err)
		}

		_, err := mgr.Start((&recordingPolicy{refuse: true}).ctx(), opts)
		if err == nil || !strings.Contains(err.Error(), agenthome.ErrLinkComponent.Error()) {
			t.Fatalf("expected ErrLinkComponent refusal, got %v", err)
		}
		if runs != 1 {
			t.Errorf("container must not run on confinement refusal: runs=%d, want 1", runs)
		}
		if _, statErr := os.Stat(outsideFile); statErr != nil {
			t.Errorf("outside file was modified/removed: %v", statErr)
		}
	})

	t.Run("linked leaf is unlinked without touching target", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		writeDeclBundleHooksHC(t, e, "hc-decl-hooks")
		runs := 0
		mgr := policyTestManager(&runs)
		opts := api.StartOptions{Name: "confined-leaf", ProjectPath: e.scion, HarnessConfig: "hc-decl-hooks", NoAuth: true}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("first Start: %v", err)
		}

		outside := t.TempDir()
		outsideFile := filepath.Join(outside, "target.sh")
		if err := os.WriteFile(outsideFile, []byte("keep"), 0o755); err != nil {
			t.Fatal(err)
		}
		home := config.GetAgentHomePath(e.scion, "confined-leaf")
		leaf := filepath.Join(home, ".scion", "hooks", "post-start.d", "10-bundle")
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideFile, leaf); err != nil {
			t.Fatal(err)
		}

		if _, err := mgr.Start((&recordingPolicy{refuse: true}).ctx(), opts); err != nil {
			t.Fatalf("relaunch with linked leaf: %v", err)
		}
		if _, lstatErr := os.Lstat(leaf); !os.IsNotExist(lstatErr) {
			t.Errorf("linked leaf was not unlinked: %v", lstatErr)
		}
		if _, statErr := os.Stat(outsideFile); statErr != nil {
			t.Errorf("outside target file was deleted: %v", statErr)
		}
	})
}

func TestHarnessConfigEnv_ReservedLifecycleKeysIgnored(t *testing.T) {
	e := newPolicyTestEnv(t)
	hcBody := policyTestDecl + strings.Join([]string{
		"env:",
		"  SCION_HOOKS_DIR: /home/scion/.custom-hooks",
		"  SCION_HARNESS_BUNDLE: /tmp/bundle",
		"  SCION_HARNESS_SECRETS_DIR: /tmp/secrets",
		"  SCION_HARNESS_OUTPUTS_DIR: /tmp/outputs",
		"  SCION_PROVISION_STATE_DIR: /tmp/prov",
		"  SCION_AGENT_NAME: \"{{ .AgentName }}\"",
		"  CUSTOM_HARNESS_VAR: kept",
		"env_template:",
		"  SCION_HOOKS_DIR: \"{{ .AgentHome }}/.custom-hooks\"",
		"  SCION_HARNESS_BUNDLE: \"{{ .AgentHome }}/.bundle\"",
		"  TEMPLATE_USER_VAR: \"{{ .UnixUsername }}\"",
		"",
	}, "\n")
	e.projectHC(t, "hc-env", hcBody)

	var captured scionruntime.RunConfig
	mgr := NewManager(&scionruntime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg scionruntime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	})
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "env-agent", ProjectPath: e.scion, HarnessConfig: "hc-env", NoAuth: true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	gotEnv := map[string]string{}
	for _, kv := range captured.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			gotEnv[k] = v
		}
	}
	if captured.ResolvedAuth != nil {
		for k, v := range captured.ResolvedAuth.EnvVars {
			gotEnv[k] = v
		}
	}
	if captured.Harness != nil {
		for k, v := range captured.Harness.GetEnv("env-agent", "/home/scion", "scion") {
			gotEnv[k] = v
		}
	}

	for _, reserved := range []string{
		"SCION_HOOKS_DIR",
		"SCION_HARNESS_BUNDLE",
		"SCION_HARNESS_SECRETS_DIR",
		"SCION_HARNESS_OUTPUTS_DIR",
		"SCION_PROVISION_STATE_DIR",
	} {
		if val, exists := gotEnv[reserved]; exists {
			t.Errorf("reserved lifecycle env key %s reached launch env with value %q", reserved, val)
		}
	}
	if gotEnv["CUSTOM_HARNESS_VAR"] != "kept" {
		t.Errorf("CUSTOM_HARNESS_VAR = %q, want %q", gotEnv["CUSTOM_HARNESS_VAR"], "kept")
	}
	if gotEnv["TEMPLATE_USER_VAR"] != "scion" {
		t.Errorf("TEMPLATE_USER_VAR = %q, want %q", gotEnv["TEMPLATE_USER_VAR"], "scion")
	}

	t.Run("persisted config and host passthrough also drop reserved lifecycle keys", func(t *testing.T) {
		t.Setenv("SCION_HOOKS_DIR", "/host/hooks")
		t.Setenv("HOOKS_KEY_SUFFIX", "HOOKS_DIR")
		cfg := &api.ScionConfig{
			Env: map[string]string{
				"SCION_HOOKS_DIR":           "",
				"SCION_${HOOKS_KEY_SUFFIX}": "/expanded/hooks",
				"SCION_EXTRA_PATH":          "/home/scion/bin",
			},
		}
		env, _, _, _ := buildAgentEnv(cfg, nil, nil, false)
		for _, kv := range env {
			if strings.HasPrefix(kv, "SCION_HOOKS_DIR=") {
				t.Errorf("SCION_HOOKS_DIR leaked through buildAgentEnv: %s", kv)
			}
		}
	})

	t.Run("settings harness-config overlay drops reserved lifecycle keys", func(t *testing.T) {
		vs := &config.VersionedSettings{
			SchemaVersion: "1",
			HarnessConfigs: map[string]config.HarnessConfigEntry{
				"hc-settings": {
					Harness: "generic",
					Env: map[string]string{
						"SCION_HOOKS_DIR": "/settings/hooks",
						"KEEP_ME":         "1",
					},
				},
			},
		}
		opts := api.StartOptions{}
		overlay, _ := resolveAuthEnvOverlay(&opts, vs, "", "hc-settings")
		if _, ok := opts.Env["SCION_HOOKS_DIR"]; ok {
			t.Errorf("opts.Env retained SCION_HOOKS_DIR: %v", opts.Env)
		}
		if _, ok := overlay["SCION_HOOKS_DIR"]; ok {
			t.Errorf("auth overlay retained SCION_HOOKS_DIR: %v", overlay)
		}
		if opts.Env["KEEP_ME"] != "1" {
			t.Errorf("KEEP_ME = %q, want 1", opts.Env["KEEP_ME"])
		}
	})
}

func TestProvisionAgent_EmptyServicesRemovesStaleServicesFile(t *testing.T) {
	t.Run("removes bundle scion-services.yaml when agent defines no services", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		hcDir := filepath.Join(e.scion, "harness-configs", "hc-svc")
		writePolicyHC(t, hcDir, policyTestDecl)
		svcFile := filepath.Join(hcDir, "home", ".scion", servicesFileName)
		if err := os.MkdirAll(filepath.Dir(svcFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(svcFile, []byte("- name: bundle-svc\n  command: [\"/bin/true\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, _, _, err := ProvisionAgent(context.Background(), "no-svc-agent", "", "", "hc-svc", e.scion, "", "", "", ""); err != nil {
			t.Fatalf("ProvisionAgent: %v", err)
		}
		homeSvc := filepath.Join(config.GetAgentHomePath(e.scion, "no-svc-agent"), ".scion", servicesFileName)
		if _, err := os.Lstat(homeSvc); !os.IsNotExist(err) {
			t.Errorf("expected stale %s to be removed when agent defines no services (stat err=%v)", servicesFileName, err)
		}
	})

	t.Run("configured services overwrite bundle scion-services.yaml", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("writeServicesFile requires unix confined home writes")
		}
		e := newPolicyTestEnv(t)
		hcDir := filepath.Join(e.scion, "harness-configs", "hc-svc")
		writePolicyHC(t, hcDir, policyTestDecl)
		svcFile := filepath.Join(hcDir, "home", ".scion", servicesFileName)
		if err := os.MkdirAll(filepath.Dir(svcFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(svcFile, []byte("- name: bundle-svc\n  command: [\"/bin/true\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e.template(t, "tpl-svc", "harness_config: hc-svc\nservices:\n  - name: configured-svc\n    command: [\"/bin/sleep\", \"1\"]\n")

		if _, _, _, err := ProvisionAgent(context.Background(), "with-svc-agent", "tpl-svc", "", "", e.scion, "", "", "", ""); err != nil {
			t.Fatalf("ProvisionAgent: %v", err)
		}
		homeSvc := filepath.Join(config.GetAgentHomePath(e.scion, "with-svc-agent"), ".scion", servicesFileName)
		data, err := os.ReadFile(homeSvc)
		if err != nil {
			t.Fatalf("read %s: %v", servicesFileName, err)
		}
		if strings.Contains(string(data), "bundle-svc") || !strings.Contains(string(data), "configured-svc") {
			t.Errorf("unexpected %s content:\n%s", servicesFileName, data)
		}
	})
}
