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

//go:build unix

package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type homeFilesTestFixture struct {
	projectScionDir string
	tplDir          string
	mgr             Manager
	runCalls        int
	onRun           func(cfg runtime.RunConfig)
}

func newHomeFilesTestFixture(t *testing.T) *homeFilesTestFixture {
	t.Helper()

	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	mkdirAll(t, tplDir)
	writeFile(t, filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	mkdirAll(t, hcDir)
	writeFile(t, filepath.Join(hcDir, "config.yaml"), "harness: gemini\nuser: scion\nimage: test-image:latest\n")

	writeFile(t, filepath.Join(globalScionDir, "settings.yaml"),
		"schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	mkdirAll(t, projectScionDir)

	fx := &homeFilesTestFixture{
		projectScionDir: projectScionDir,
		tplDir:          tplDir,
	}
	fx.mgr = NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			fx.runCalls++
			if fx.onRun != nil {
				fx.onRun(cfg)
			}
			return "mock-container-id", nil
		},
	})
	return fx
}

func (fx *homeFilesTestFixture) agentHome(name string) string {
	return filepath.Join(fx.projectScionDir, "agents", name, "home")
}

func (fx *homeFilesTestFixture) agentWorkspace(name string) string {
	return filepath.Join(fx.projectScionDir, "agents", name, "workspace")
}

func TestHomeFiles_WrittenOnCreateBeforeContainerStarts(t *testing.T) {
	fx := newHomeFilesTestFixture(t)

	eventPayload := []byte("{\"type\":\"trigger.event\",\"event_id\":\"ev-123\",\"fire_id\":\"fi-456\"}\n")
	files := []api.HomeFile{
		{Path: api.TriggerEventHomeFilePath, Mode: 0o600, Content: eventPayload},
	}

	verifiedBeforeRun := false
	fx.onRun = func(cfg runtime.RunConfig) {
		gotEvent, err := os.ReadFile(filepath.Join(cfg.HomeDir, ".scion", "trigger-event.json"))
		if err != nil {
			t.Errorf("before container Run: read trigger-event.json: %v", err)
			return
		}
		if !bytes.Equal(gotEvent, eventPayload) {
			t.Errorf("before container Run: trigger-event.json = %q, want %q", gotEvent, eventPayload)
		}
		verifiedBeforeRun = true
	}

	opts := api.StartOptions{
		Name:           "triggered-agent",
		ProjectPath:    fx.projectScionDir,
		NoAuth:         true,
		FreshProvision: true,
		HomeFiles:      files,
		GitClone:       &api.GitCloneConfig{URL: "https://github.com/example/repo.git", Branch: "main"},
	}
	if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if !verifiedBeforeRun {
		t.Fatal("expected MockRuntime.RunFunc to observe HomeFiles before launch")
	}

	home := fx.agentHome("triggered-agent")
	for _, want := range files {
		full := filepath.Join(home, filepath.FromSlash(want.Path))
		info, err := os.Lstat(full)
		if err != nil {
			t.Fatalf("Lstat(%s): %v", want.Path, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file: mode=%v", want.Path, info.Mode())
		}
		if gotPerm := info.Mode().Perm(); gotPerm != os.FileMode(want.Mode) {
			t.Errorf("%s mode = %#o, want %#o", want.Path, gotPerm, want.Mode)
		}
		gotBytes, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", want.Path, err)
		}
		if !bytes.Equal(gotBytes, want.Content) {
			t.Errorf("%s content = %q, want %q", want.Path, gotBytes, want.Content)
		}
	}
}

func TestHomeFiles_WrittenOnProvisionOnlyCreate(t *testing.T) {
	fx := newHomeFilesTestFixture(t)

	payload := []byte(`{"event_id":"prov-only"}`)
	opts := api.StartOptions{
		Name:           "prov-agent",
		ProjectPath:    fx.projectScionDir,
		NoAuth:         true,
		FreshProvision: true,
		HomeFiles: []api.HomeFile{
			{Path: ".scion/trigger-event.json", Mode: 0o600, Content: payload},
		},
		GitClone: &api.GitCloneConfig{URL: "https://github.com/example/repo.git", Branch: "main"},
	}
	if _, err := fx.mgr.Provision(context.Background(), opts); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}

	full := filepath.Join(fx.agentHome("prov-agent"), ".scion", "trigger-event.json")
	info, err := os.Lstat(full)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if gotPerm := info.Mode().Perm(); gotPerm != 0o600 {
		t.Errorf("mode = %#o, want 0600", gotPerm)
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content = %q, want %q", got, payload)
	}
}

func TestHomeFiles_SymlinkEscapeRefusedByConfinement(t *testing.T) {
	t.Run("writeProvisionHomeFiles refuses symlinked .scion and unlinks leaf symlink without following", func(t *testing.T) {
		outsideDir := t.TempDir()
		victim := filepath.Join(outsideDir, "trigger-event.json")
		if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Case A: .scion itself is a symlink outside agentHome -> refused.
		homeA := t.TempDir()
		if err := os.Symlink(outsideDir, filepath.Join(homeA, ".scion")); err != nil {
			t.Fatal(err)
		}
		err := writeProvisionHomeFiles(homeA, []api.HomeFile{
			{Path: api.TriggerEventHomeFilePath, Mode: 0o600, Content: []byte("attacker")},
		})
		if err == nil {
			t.Fatal("expected refusal when .scion is a symlink, got nil")
		}

		// Case B: leaf .scion/trigger-event.json is a symlink to victim -> symlink
		// is unlinked (not followed), regular file is created exclusively, and
		// outside victim is untouched.
		homeB := t.TempDir()
		mkdirAll(t, filepath.Join(homeB, ".scion"))
		leafPath := filepath.Join(homeB, ".scion", "trigger-event.json")
		if err := os.Symlink(victim, leafPath); err != nil {
			t.Fatal(err)
		}
		if err := writeProvisionHomeFiles(homeB, []api.HomeFile{
			{Path: api.TriggerEventHomeFilePath, Mode: 0o600, Content: []byte("safe-content")},
		}); err != nil {
			t.Fatalf("writeProvisionHomeFiles over leaf symlink failed: %v", err)
		}
		info, err := os.Lstat(leafPath)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("expected regular file after unlinking leaf symlink, stat err=%v mode=%v", err, info.Mode())
		}
		gotLeaf, err := os.ReadFile(leafPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotLeaf) != "safe-content" {
			t.Errorf("leaf content = %q, want %q", gotLeaf, "safe-content")
		}
		gotVictim, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotVictim) != "untouched" {
			t.Errorf("outside victim file modified: got %q", gotVictim)
		}

		// Case C: writeHomeFileExclusive directly still refuses an existing file with os.ErrExist.
		if err := writeHomeFileExclusive(homeB, api.TriggerEventHomeFilePath, []byte("overwrite"), 0o600); !errors.Is(err, os.ErrExist) {
			t.Fatalf("writeHomeFileExclusive over existing file = %v, want os.ErrExist", err)
		}
	})

	t.Run("Start replaces leftover existing HomeFile in reused agent dir with new event body", func(t *testing.T) {
		fx := newHomeFilesTestFixture(t)

		// Seed a leftover agent dir where .scion/trigger-event.json already exists
		// (e.g. reused name under soft_delete_retain_files).
		agentDir := filepath.Join(fx.projectScionDir, "agents", "leftover-file-agent")
		agentHome := filepath.Join(agentDir, "home")
		mkdirAll(t, filepath.Join(agentDir, "workspace"))
		mkdirAll(t, filepath.Join(agentHome, ".scion"))
		writeFile(t, filepath.Join(agentDir, "scion-agent.json"), `{"harness":"gemini","default_harness_config":"test-harness"}`)
		existingPath := filepath.Join(agentHome, ".scion", "trigger-event.json")
		if err := os.WriteFile(existingPath, []byte("leftover-content"), 0o600); err != nil {
			t.Fatal(err)
		}

		opts := api.StartOptions{
			Name:           "leftover-file-agent",
			ProjectPath:    fx.projectScionDir,
			NoAuth:         true,
			FreshProvision: true,
			HomeFiles: []api.HomeFile{
				{Path: api.TriggerEventHomeFilePath, Mode: 0o600, Content: []byte("new-content")},
			},
			GitClone: &api.GitCloneConfig{URL: "https://github.com/example/repo.git", Branch: "main"},
		}
		if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("expected Start() to succeed on reused agent dir with leftover trigger-event.json, got: %v", err)
		}
		if fx.runCalls != 1 {
			t.Errorf("expected 1 container Run call, got %d", fx.runCalls)
		}
		got, err := os.ReadFile(existingPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new-content" {
			t.Errorf("trigger-event.json = %q, want %q", got, "new-content")
		}
	})

	t.Run("Start removes planted symlink at HomeFile path without following target", func(t *testing.T) {
		fx := newHomeFilesTestFixture(t)

		outsideDir := t.TempDir()
		victim := filepath.Join(outsideDir, "victim.json")
		if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}

		agentDir := filepath.Join(fx.projectScionDir, "agents", "leftover-symlink-agent")
		agentHome := filepath.Join(agentDir, "home")
		mkdirAll(t, filepath.Join(agentDir, "workspace"))
		mkdirAll(t, filepath.Join(agentHome, ".scion"))
		writeFile(t, filepath.Join(agentDir, "scion-agent.json"), `{"harness":"gemini","default_harness_config":"test-harness"}`)
		leafPath := filepath.Join(agentHome, ".scion", "trigger-event.json")
		if err := os.Symlink(victim, leafPath); err != nil {
			t.Fatal(err)
		}

		opts := api.StartOptions{
			Name:           "leftover-symlink-agent",
			ProjectPath:    fx.projectScionDir,
			NoAuth:         true,
			FreshProvision: true,
			HomeFiles: []api.HomeFile{
				{Path: api.TriggerEventHomeFilePath, Mode: 0o600, Content: []byte("fresh-event")},
			},
			GitClone: &api.GitCloneConfig{URL: "https://github.com/example/repo.git", Branch: "main"},
		}
		if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start() failed over planted leaf symlink: %v", err)
		}
		info, err := os.Lstat(leafPath)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("expected regular file at %s, err=%v mode=%v", leafPath, err, info.Mode())
		}
		got, err := os.ReadFile(leafPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "fresh-event" {
			t.Errorf("leaf content = %q, want %q", got, "fresh-event")
		}
		gotVictim, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotVictim) != "untouched" {
			t.Errorf("symlink target modified: got %q, want %q", gotVictim, "untouched")
		}
	})
}

func TestHomeFiles_NotRewrittenOnResumeOrReprovision(t *testing.T) {
	fx := newHomeFilesTestFixture(t)

	const agentName = "lifecycle-agent"
	gitClone := &api.GitCloneConfig{URL: "https://github.com/example/repo.git", Branch: "main"}

	// 1. Initial create writes .scion/trigger-event.json.
	createOpts := api.StartOptions{
		Name:           agentName,
		ProjectPath:    fx.projectScionDir,
		NoAuth:         true,
		FreshProvision: true,
		GitClone:       gitClone,
		HomeFiles: []api.HomeFile{
			{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte("created-payload")},
		},
	}
	if _, err := fx.mgr.Start(context.Background(), createOpts); err != nil {
		t.Fatalf("create Start() failed: %v", err)
	}

	targetPath := filepath.Join(fx.agentHome(agentName), ".scion", "trigger-event.json")
	// Simulate an in-container edit or persisted state and ensure a real git clone
	// exists in workspace so Reprovision's clone-per-agent precondition holds.
	if err := os.WriteFile(targetPath, []byte("persisted-in-home"), 0o600); err != nil {
		t.Fatal(err)
	}
	mkdirAll(t, filepath.Join(fx.agentWorkspace(agentName), ".git"))

	// 2. Start/resume (FreshProvision: false) must NOT rewrite HomeFiles even if
	// a caller accidentally populates opts.HomeFiles.
	resumeOpts := api.StartOptions{
		Name:           agentName,
		ProjectPath:    fx.projectScionDir,
		NoAuth:         true,
		Resume:         true,
		FreshProvision: false,
		GitClone:       gitClone,
		HomeFiles: []api.HomeFile{
			{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte("resume-overwrite")},
		},
	}
	if _, err := fx.mgr.Start(context.Background(), resumeOpts); err != nil {
		t.Fatalf("resume Start() failed: %v", err)
	}
	gotAfterResume, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAfterResume) != "persisted-in-home" {
		t.Errorf("after resume: content = %q, want %q", gotAfterResume, "persisted-in-home")
	}

	// 2b. Plain non-resume start/restart (FreshProvision: false, Resume: false)
	// must ALSO NOT write HomeFiles (catches dropping opts.FreshProvision &&).
	plainStartOpts := api.StartOptions{
		Name:           agentName,
		ProjectPath:    fx.projectScionDir,
		NoAuth:         true,
		Resume:         false,
		FreshProvision: false,
		GitClone:       gitClone,
		HomeFiles: []api.HomeFile{
			{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte("plain-start-overwrite")},
		},
	}
	if _, err := fx.mgr.Start(context.Background(), plainStartOpts); err != nil {
		t.Fatalf("plain non-resume Start() failed: %v", err)
	}
	gotAfterPlainStart, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAfterPlainStart) != "persisted-in-home" {
		t.Errorf("after plain non-resume start: content = %q, want %q", gotAfterPlainStart, "persisted-in-home")
	}

	// 3. Reprovision without ResetHome preserves home and must NOT rewrite HomeFiles.
	reprovOpts := api.StartOptions{
		Name:           agentName,
		ProjectPath:    fx.projectScionDir,
		NoAuth:         true,
		FreshProvision: false,
		GitClone:       gitClone,
		HomeFiles: []api.HomeFile{
			{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte("reprovision-overwrite")},
		},
	}
	if _, err := fx.mgr.Reprovision(context.Background(), reprovOpts); err != nil {
		t.Fatalf("Reprovision() failed: %v", err)
	}
	gotAfterReprov, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAfterReprov) != "persisted-in-home" {
		t.Errorf("after Reprovision: content = %q, want %q", gotAfterReprov, "persisted-in-home")
	}

	// 4. Reprovision with ResetHome rebuilds home fresh and does NOT re-inject create-time HomeFiles.
	resetOpts := reprovOpts
	resetOpts.ResetHome = true
	resetOpts.ResetHomeID = "reincarnate-reset-1"
	if _, err := fx.mgr.Reprovision(context.Background(), resetOpts); err != nil {
		t.Fatalf("Reprovision(ResetHome) failed: %v", err)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Errorf("after ResetHome Reprovision: expected trigger-event.json to be absent, got err=%v", err)
	}
}

func TestHomeFiles_ReservedPathsRefused(t *testing.T) {
	const canary = "RESERVED_PATH_SECRET_CANARY_4242"

	reservedCases := []string{
		".scion/scion-agent.json",
		".scion/agent-info.json",
		".scion/scion-services.yaml",
		".scion/resolved-skills.json",
		".scion/task.md",
		".scion/scion-token",
		".scion/scion-token.tmp",
		".scion/transport-token",
		".scion/transport-token.tmp",
		".scion/telemetry-gcp-credentials.json",
		".scion/secrets.json",
		".scion/.home-links.json",
		".scion/settings.yaml",
		".scion/project-id",
		".scion/ssh",
		".scion/ssh/known_hosts-hub",
		".scion/vscode",
		".scion/vscode/tunnel.json",
		".scion/system_prompt.md",
		".scion/hooks/pre-start.d/30-project-custom",
		".scion/hooks/pre-start.d/20-harness-provision",
		".scion/hooks/pre-start.d/99-custom",
		".scion/hooks/pre-start.d",
		".scion/hooks/post-start",
		".scion/hooks/pre-start.sh",
		".scion/hooks/session-end.d/x",
		".scion/hooks",
		".scion/Secrets.json",
		".scion/Project-ID",
		".scion/HOOKS/Post-Start",
		".scion/harness",
		".scion/harness/manifest.json",
		".scion/harness/secrets/API_KEY",
		".scion/harness/inputs/plugins.json",
		".scion/skills",
		".scion/skills/my-skill/SKILL.md",
	}

	for _, rel := range reservedCases {
		t.Run(rel, func(t *testing.T) {
			files := []api.HomeFile{
				{Path: rel, Mode: 0o600, Content: []byte(canary)},
			}
			err := ValidateHomeFiles(files)
			if err == nil {
				t.Fatalf("ValidateHomeFiles(%q) = nil, want refusal", rel)
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("ValidateHomeFiles(%q) leaked content: %q", rel, err.Error())
			}
			if _, collides := collidesWithReservedHomePath(rel); !collides {
				t.Errorf("collidesWithReservedHomePath(%q) = false, want true (defense in depth)", rel)
			}

			home := t.TempDir()
			if werr := writeProvisionHomeFiles(home, files); werr == nil {
				t.Fatalf("writeProvisionHomeFiles(%q) = nil, want refusal", rel)
			}
		})
	}

	t.Run("Start refuses reserved path without launching container", func(t *testing.T) {
		fx := newHomeFilesTestFixture(t)
		opts := api.StartOptions{
			Name:           "reserved-start-agent",
			ProjectPath:    fx.projectScionDir,
			NoAuth:         true,
			FreshProvision: true,
			GitClone:       &api.GitCloneConfig{URL: "https://github.com/example/repo.git", Branch: "main"},
			HomeFiles: []api.HomeFile{
				{Path: ".scion/scion-agent.json", Mode: 0o600, Content: []byte(canary)},
			},
		}
		_, err := fx.mgr.Start(context.Background(), opts)
		if err == nil {
			t.Fatal("expected Start() to refuse reserved .scion/scion-agent.json")
		}
		if fx.runCalls != 0 {
			t.Errorf("expected 0 container Run calls, got %d", fx.runCalls)
		}
	})
}

func TestHomeFiles_TriggerEventFileEnvResolvedFromContainerUser(t *testing.T) {
	findEnv := func(env []string, key string) (string, bool) {
		prefix := key + "="
		for _, entry := range env {
			if strings.HasPrefix(entry, prefix) {
				return strings.TrimPrefix(entry, prefix), true
			}
		}
		return "", false
	}

	cases := []struct {
		name     string
		user     string
		hubEnv   map[string]string
		wantPath string
	}{
		{
			name:     "non-scion user sets resolved path when hub omits env",
			user:     "alice",
			hubEnv:   nil,
			wantPath: "/home/alice/.scion/trigger-event.json",
		},
		{
			name: "non-scion user overrides hub hard-coded /home/scion path",
			user: "alice",
			hubEnv: map[string]string{
				api.EnvTriggerEventFile: "/home/scion/.scion/trigger-event.json",
			},
			wantPath: "/home/alice/.scion/trigger-event.json",
		},
		{
			name: "root user resolves to /root/.scion/trigger-event.json",
			user: "root",
			hubEnv: map[string]string{
				api.EnvTriggerEventFile: "/home/scion/.scion/trigger-event.json",
			},
			wantPath: "/root/.scion/trigger-event.json",
		},
		{
			name:     "default scion user resolves to /home/scion/.scion/trigger-event.json",
			user:     "scion",
			hubEnv:   nil,
			wantPath: "/home/scion/.scion/trigger-event.json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHomeFilesTestFixture(t)

			// Override harness-config user for this test case.
			hcPath := filepath.Join(os.Getenv("HOME"), ".scion", "harness-configs", "test-harness", "config.yaml")
			writeFile(t, hcPath, "harness: gemini\nuser: "+tc.user+"\nimage: test-image:latest\n")

			var gotEnv []string
			fx.onRun = func(cfg runtime.RunConfig) {
				gotEnv = append([]string(nil), cfg.Env...)
			}

			opts := api.StartOptions{
				Name:           "trigger-user-" + tc.user,
				ProjectPath:    fx.projectScionDir,
				NoAuth:         true,
				FreshProvision: true,
				Env:            tc.hubEnv,
				HomeFiles: []api.HomeFile{
					{Path: api.TriggerEventHomeFilePath, Mode: 0o600, Content: []byte(`{"event_id":"ev-1"}`)},
				},
				GitClone: &api.GitCloneConfig{URL: "https://github.com/example/repo.git", Branch: "main"},
			}
			if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
				t.Fatalf("Start() failed: %v", err)
			}
			got, ok := findEnv(gotEnv, api.EnvTriggerEventFile)
			if !ok {
				t.Fatalf("expected %s to be set in container env", api.EnvTriggerEventFile)
			}
			if got != tc.wantPath {
				t.Errorf("%s = %q, want %q", api.EnvTriggerEventFile, got, tc.wantPath)
			}
		})
	}
}
