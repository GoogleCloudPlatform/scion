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
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// inlineResetHomePurge runs the background purge of a moved-aside home
// inline for the test.
func inlineResetHomePurge(t *testing.T) {
	t.Helper()
	orig := purgeResetHomeResidueAsync
	purgeResetHomeResidueAsync = func(_ context.Context, homeParent string) {
		if err := purgeResetHomeResidue(homeParent); err != nil {
			t.Errorf("purge moved-aside home: %v", err)
		}
	}
	t.Cleanup(func() { purgeResetHomeResidueAsync = orig })
}

// homeEntries lists the relative paths of every file and directory in home.
func homeEntries(t *testing.T, home string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(home, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == home {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", home, err)
	}
	slices.Sort(out)
	return out
}

func provisionCloneAgent(t *testing.T, scionDir, name string, gc *api.GitCloneConfig) string {
	t.Helper()
	ctx := api.ContextWithGitClone(context.Background(), gc)
	if _, _, _, err := ProvisionAgent(ctx, name, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("ProvisionAgent %s: %v", name, err)
	}
	ws := filepath.Join(scionDir, "agents", name, "workspace")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

// TestReprovision_ResetHome_FreshHome: a reprovision with ResetHome leaves a
// home holding exactly what a freshly created agent with the same config
// gets. Nothing from the previous home survives; the workspace, prompt.md
// and the agent's records do; the reset is recorded; and the moved-aside
// home is purged (ptone/scion#7757).
func TestReprovision_ResetHome_FreshHome(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	inlineResetHomePurge(t)
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	agentName := "reset-agent"
	ws := provisionCloneAgent(t, scionDir, agentName, gc)
	work := filepath.Join(ws, "uncommitted-work.txt")
	if err := os.WriteFile(work, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := config.GetAgentHomePath(scionDir, agentName)
	agentDir := filepath.Dir(home)
	for _, f := range []string{".claude/projects/session.jsonl", "notes.txt", ".bash_history"} {
		p := filepath.Join(home, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
		ResetHome: true, ResetHomeID: "r-1",
	}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}

	for _, f := range []string{".claude", "notes.txt", ".bash_history"} {
		if _, err := os.Lstat(filepath.Join(home, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the home reset (err=%v)", f, err)
		}
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("workspace file lost: %v", err)
	}
	for _, f := range []string{"prompt.md", "scion-agent.json", homeStorageRecordFile} {
		if _, err := os.Stat(filepath.Join(agentDir, f)); err != nil {
			t.Errorf("agent record %s lost: %v", f, err)
		}
	}

	// The same file set as a fresh agent with the same config.
	freshName := "fresh-agent"
	provisionCloneAgent(t, scionDir, freshName, gc)
	if got, want := homeEntries(t, home), homeEntries(t, config.GetAgentHomePath(scionDir, freshName)); !slices.Equal(got, want) {
		t.Errorf("reset home entries = %v, want the fresh agent's %v", got, want)
	}

	data, err := os.ReadFile(filepath.Join(agentDir, homeResetRecordFile))
	if err != nil {
		t.Fatalf("home reset record: %v", err)
	}
	var rec homeResetRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.ResetID != "r-1" {
		t.Errorf("home reset record = %s (err %v), want reset_id r-1", data, err)
	}
	if fi, err := os.Stat(filepath.Join(agentDir, homeResetRecordFile)); err == nil && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("home reset record mode = %v, want private", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(agentDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), removingHomePrefix) {
			t.Errorf("moved-aside home %s was not purged", e.Name())
		}
	}
}

// TestReprovision_ResetHome_RefusedLeavesHomeIntact: a precondition refusal
// (a running container) happens before the home is moved aside.
func TestReprovision_ResetHome_RefusedLeavesHomeIntact(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	agentName := "running-agent"
	provisionCloneAgent(t, scionDir, agentName, gc)
	home := config.GetAgentHomePath(scionDir, agentName)
	note := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(note, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{Name: agentName, Phase: "running"}}, nil
		},
	})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
		ResetHome: true, ResetHomeID: "r-1",
	})
	if !errors.Is(err, ErrReprovisionRefused) || errors.Is(err, ErrHomeResetApplied) {
		t.Fatalf("Reprovision err = %v, want ErrReprovisionRefused without ErrHomeResetApplied", err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Errorf("home file lost on a refused reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(home), homeResetRecordFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("home reset recorded on a refused reset (err=%v)", err)
	}
}

func TestReprovision_ResetHome_InvalidIDRefused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	agentName := "bad-id-agent"
	provisionCloneAgent(t, scionDir, agentName, gc)
	mgr := NewManager(&runtime.MockRuntime{})
	for _, id := range []string{"", "../x", strings.Repeat("a", 65), "a/b", "a b"} {
		_, err := mgr.Reprovision(context.Background(), api.StartOptions{
			Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
			ResetHome: true, ResetHomeID: id,
		})
		if !errors.Is(err, ErrReprovisionRefused) {
			t.Errorf("ResetHomeID %q: err = %v, want ErrReprovisionRefused", id, err)
		}
	}
}

func TestResetAgentHome(t *testing.T) {
	t.Run("retry of the same reset is idempotent", func(t *testing.T) {
		agentDir := t.TempDir()
		home := filepath.Join(agentDir, "home")
		_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
		if err := resetAgentHome(agentDir, home, "r1"); err != nil {
			t.Fatalf("first reset: %v", err)
		}
		if _, err := os.Stat(filepath.Join(agentDir, removingHomePrefix+"r1", ".claude")); err != nil {
			t.Fatalf("old home not moved aside: %v", err)
		}
		// The earlier attempt started composing a new home, then failed.
		_ = os.MkdirAll(filepath.Join(home, "partial"), 0o755)
		if err := resetAgentHome(agentDir, home, "r1"); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if _, err := os.Stat(filepath.Join(agentDir, removingHomePrefix+"r1", ".claude")); err != nil {
			t.Fatalf("moved-aside home replaced on retry: %v", err)
		}
	})
	t.Run("missing home is not an error", func(t *testing.T) {
		agentDir := t.TempDir()
		if err := resetAgentHome(agentDir, filepath.Join(agentDir, "home"), "r1"); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if _, err := os.Stat(filepath.Join(agentDir, homeResetRecordFile)); err != nil {
			t.Fatalf("reset not recorded: %v", err)
		}
	})
	t.Run("symlinked home refused", func(t *testing.T) {
		agentDir := t.TempDir()
		target := t.TempDir()
		home := filepath.Join(agentDir, "home")
		if err := os.Symlink(target, home); err != nil {
			t.Fatal(err)
		}
		if err := resetAgentHome(agentDir, home, "r1"); !errors.Is(err, ErrReprovisionRefused) {
			t.Fatalf("err = %v, want ErrReprovisionRefused", err)
		}
		if fi, err := os.Lstat(home); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlinked home changed (err=%v)", err)
		}
		if _, err := os.Stat(filepath.Join(agentDir, homeResetRecordFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reset recorded for a refused home (err=%v)", err)
		}
	})
}

func TestPurgeResetHomeResidueUnder(t *testing.T) {
	global := t.TempDir()
	hubAgent := filepath.Join(global, "projects", "p1", ".scion", "agents", "a1")
	splitAgent := filepath.Join(global, "project-configs", "p2__id", ".scion", "agents", "a2")
	for _, d := range []string{hubAgent, splitAgent} {
		_ = os.MkdirAll(filepath.Join(d, removingHomePrefix+"r1", ".claude"), 0o755)
		_ = os.MkdirAll(filepath.Join(d, "home"), 0o755)
	}
	PurgeResetHomeResidueUnder(global, nil)
	for _, d := range []string{hubAgent, splitAgent} {
		if _, err := os.Stat(filepath.Join(d, removingHomePrefix+"r1")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: moved-aside home not purged (err=%v)", d, err)
		}
		if _, err := os.Stat(filepath.Join(d, "home")); err != nil {
			t.Errorf("%s: live home removed: %v", d, err)
		}
	}
}

// TestReprovision_ResetHome_FailureAfterResetIsMarked: a failure after the
// home was moved aside carries ErrHomeResetApplied.
func TestReprovision_ResetHome_FailureAfterResetIsMarked(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	inlineResetHomePurge(t)
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	agentName := "fail-after-reset"
	provisionCloneAgent(t, scionDir, agentName, gc)
	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "no-such-template", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
		ResetHome: true, ResetHomeID: "r-2",
	})
	if err == nil || !errors.Is(err, ErrHomeResetApplied) {
		t.Fatalf("Reprovision err = %v, want ErrHomeResetApplied", err)
	}
}
