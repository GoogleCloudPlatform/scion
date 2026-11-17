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
	metaPayload := []byte("{\"source\":\"webhook\"}\n")
	files := []api.HomeFile{
		{Path: ".scion/trigger-event.json", Mode: 0o600, Content: eventPayload},
		{Path: ".scion/triggers/meta.json", Mode: 0o644, Content: metaPayload},
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
	t.Run("writeProvisionHomeFiles refuses symlinked .scion and symlinked subdir and never writes through leaf symlink", func(t *testing.T) {
		outsideDir := t.TempDir()
		victim := filepath.Join(outsideDir, "victim.json")
		if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Case A: .scion itself is a symlink outside agentHome -> refused.
		homeA := t.TempDir()
		if err := os.Symlink(outsideDir, filepath.Join(homeA, ".scion")); err != nil {
			t.Fatal(err)
		}
		err := writeProvisionHomeFiles(homeA, []api.HomeFile{
			{Path: ".scion/victim.json", Mode: 0o600, Content: []byte("attacker")},
		})
		if err == nil {
			t.Fatal("expected refusal when .scion is a symlink, got nil")
		}

		// Case B: .scion/sub is a symlink outside agentHome -> refused.
		homeB := t.TempDir()
		mkdirAll(t, filepath.Join(homeB, ".scion"))
		if err := os.Symlink(outsideDir, filepath.Join(homeB, ".scion", "sub")); err != nil {
			t.Fatal(err)
		}
		err = writeProvisionHomeFiles(homeB, []api.HomeFile{
			{Path: ".scion/sub/victim.json", Mode: 0o600, Content: []byte("attacker")},
		})
		if err == nil {
			t.Fatal("expected refusal when .scion/sub is a symlink, got nil")
		}

		// Case C: leaf .scion/trigger-event.json is a symlink to victim -> replaced atomically, victim untouched.
		homeC := t.TempDir()
		mkdirAll(t, filepath.Join(homeC, ".scion"))
		leafPath := filepath.Join(homeC, ".scion", "trigger-event.json")
		if err := os.Symlink(victim, leafPath); err != nil {
			t.Fatal(err)
		}
		if err := writeProvisionHomeFiles(homeC, []api.HomeFile{
			{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte("safe-content")},
		}); err != nil {
			t.Fatalf("leaf replacement failed: %v", err)
		}
		info, err := os.Lstat(leafPath)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("expected replaced leaf to be regular file, stat err=%v mode=%v", err, info.Mode())
		}

		gotVictim, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotVictim) != "untouched" {
			t.Errorf("outside victim file modified: got %q", gotVictim)
		}
	})

	t.Run("Start refuses symlinked subdir under .scion before container launch", func(t *testing.T) {
		fx := newHomeFilesTestFixture(t)

		outsideDir := t.TempDir()
		victim := filepath.Join(outsideDir, "event.json")
		if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}

		// Seed a leftover agent dir where .scion is a real directory (so earlier
		// pre-start hook cleanup succeeds) and .scion/escaped is a symlink outside home,
		// ensuring Start reaches writeProvisionHomeFiles and refuses there.
		agentDir := filepath.Join(fx.projectScionDir, "agents", "leftover-link-agent")
		agentHome := filepath.Join(agentDir, "home")
		mkdirAll(t, filepath.Join(agentDir, "workspace"))
		mkdirAll(t, filepath.Join(agentHome, ".scion"))
		writeFile(t, filepath.Join(agentDir, "scion-agent.json"), `{"harness":"gemini","default_harness_config":"test-harness"}`)
		if err := os.Symlink(outsideDir, filepath.Join(agentHome, ".scion", "escaped")); err != nil {
			t.Fatal(err)
		}

		opts := api.StartOptions{
			Name:           "leftover-link-agent",
			ProjectPath:    fx.projectScionDir,
			NoAuth:         true,
			FreshProvision: true,
			HomeFiles: []api.HomeFile{
				{Path: ".scion/escaped/event.json", Mode: 0o600, Content: []byte("attacker")},
			},
			GitClone: &api.GitCloneConfig{URL: "https://github.com/example/repo.git", Branch: "main"},
		}
		if _, err := fx.mgr.Start(context.Background(), opts); err == nil {
			t.Fatal("expected Start() to refuse writing HomeFile when .scion/escaped is a symlink, got nil")
		}
		if fx.runCalls != 0 {
			t.Errorf("expected container not to start, got %d Run calls", fx.runCalls)
		}
		gotVictim, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotVictim) != "untouched" {
			t.Errorf("outside victim file modified: got %q", gotVictim)
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
		".scion/hooks",
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
				t.Fatalf("ValidateHomeFiles(%q) = nil, want reserved-path refusal", rel)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("ValidateHomeFiles(%q) error = %q, want substring 'reserved'", rel, err.Error())
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("ValidateHomeFiles(%q) leaked content: %q", rel, err.Error())
			}

			home := t.TempDir()
			if werr := writeProvisionHomeFiles(home, files); werr == nil {
				t.Fatalf("writeProvisionHomeFiles(%q) = nil, want reserved-path refusal", rel)
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
