/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestNewLifecycleManager_DefaultDir(t *testing.T) {
	t.Setenv("SCION_HOOKS_DIR", "")
	m := NewLifecycleManager()
	if len(m.HooksDirs) != 1 || m.HooksDirs[0] != "/etc/scion/hooks" {
		t.Fatalf("expected default /etc/scion/hooks, got %v", m.HooksDirs)
	}
}

func TestNewLifecycleManager_SciontoolHooksDirParsesColonList(t *testing.T) {
	t.Setenv("SCION_HOOKS_DIR", "/a:/b: :/c")
	m := NewLifecycleManager()
	want := []string{"/a", "/b", "/c"}
	if len(m.HooksDirs) != len(want) {
		t.Fatalf("expected %v, got %v", want, m.HooksDirs)
	}
	for i, d := range want {
		if m.HooksDirs[i] != d {
			t.Errorf("HooksDirs[%d] = %q, want %q", i, m.HooksDirs[i], d)
		}
	}
}

func TestAddHooksDir_AppendsAndDedupes(t *testing.T) {
	m := &LifecycleManager{HooksDirs: []string{"/etc/scion/hooks"}}
	m.AddHooksDir("/agent/.scion/hooks")
	m.AddHooksDir("/agent/.scion/hooks") // duplicate
	m.AddHooksDir("")                    // ignored
	if len(m.HooksDirs) != 2 {
		t.Fatalf("expected 2 dirs, got %v", m.HooksDirs)
	}
	if m.HooksDirs[1] != "/agent/.scion/hooks" {
		t.Fatalf("expected per-agent dir appended, got %v", m.HooksDirs)
	}
}

func TestRunPreStart_ExecutesScriptsAcrossMultipleDirs(t *testing.T) {
	system := t.TempDir()
	agent := t.TempDir()

	// system dir: pre-start.d/10-system writes "system" to a marker file
	marker := filepath.Join(t.TempDir(), "marker")
	mustWriteScript(t, filepath.Join(system, "pre-start.d", "10-system"),
		"#!/bin/sh\necho -n system >> "+marker+"\n")
	mustWriteScript(t, filepath.Join(agent, "pre-start.d", "20-agent"),
		"#!/bin/sh\necho -n :agent >> "+marker+"\n")

	m := &LifecycleManager{
		HooksDirs: []string{system, agent},
		Handlers:  map[string][]Handler{},
	}
	if err := m.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(got) != "system:agent" {
		t.Fatalf("expected system before agent, got %q", string(got))
	}
}

func TestRunPreStart_HandlersRunAfterScriptHooks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	mustWriteScript(t, filepath.Join(dir, "pre-start.d", "10"),
		"#!/bin/sh\necho -n A >> "+marker+"\n")

	var mu sync.Mutex
	m := &LifecycleManager{HooksDirs: []string{dir}, Handlers: map[string][]Handler{}}
	m.RegisterHandler(EventPreStart, func(*Event) error {
		mu.Lock()
		defer mu.Unlock()
		f, _ := os.OpenFile(marker, os.O_APPEND|os.O_WRONLY, 0644)
		_, _ = f.WriteString("B")
		_ = f.Close()
		return nil
	})
	if err := m.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}
	got, _ := os.ReadFile(marker)
	if string(got) != "AB" {
		t.Fatalf("expected scripts before handlers (AB), got %q", string(got))
	}
}

func TestRunPreStart_ScriptDDirSortedLexically(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	mustWriteScript(t, filepath.Join(dir, "pre-start.d", "20"),
		"#!/bin/sh\necho -n 20 >> "+marker+"\n")
	mustWriteScript(t, filepath.Join(dir, "pre-start.d", "10"),
		"#!/bin/sh\necho -n 10 >> "+marker+"\n")
	mustWriteScript(t, filepath.Join(dir, "pre-start.d", "30"),
		"#!/bin/sh\necho -n 30 >> "+marker+"\n")

	m := &LifecycleManager{HooksDirs: []string{dir}, Handlers: map[string][]Handler{}}
	if err := m.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}
	got, _ := os.ReadFile(marker)
	if string(got) != "102030" {
		t.Fatalf("expected lexical order 102030, got %q", string(got))
	}
}

func TestRunPreStart_FailingScriptReturnsError(t *testing.T) {
	dir := t.TempDir()
	mustWriteScript(t, filepath.Join(dir, "pre-start.d", "10-fail"),
		"#!/bin/sh\nexit 7\n")
	m := &LifecycleManager{HooksDirs: []string{dir}, Handlers: map[string][]Handler{}}
	if err := m.RunPreStart(); err == nil {
		t.Fatal("expected error from failing script")
	}
}

func TestRunPreStart_MissingDirsAreSkipped(t *testing.T) {
	// Both dirs missing — should be a no-op, not an error.
	m := &LifecycleManager{
		HooksDirs: []string{"/nope/one", "/nope/two"},
		Handlers:  map[string][]Handler{},
	}
	if err := m.RunPreStart(); err != nil {
		t.Fatalf("expected no error from missing dirs, got %v", err)
	}
}

func mustWriteScript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
}
