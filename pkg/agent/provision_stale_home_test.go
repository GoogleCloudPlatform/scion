package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// setupNoConfigAgentProject builds a non-git project with a global default
// template and returns the project's .scion dir plus the agent's dir and
// home paths. The agent dir is created without a scion-agent config file.
func setupNoConfigAgentProject(t *testing.T, agentName string) (scionDir, agentDir, agentHome string) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "project")
	scionDir = filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "templates"), 0755); err != nil {
		t.Fatal(err)
	}
	globalScionDir := filepath.Join(tmpDir, ".scion")
	_ = os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "generic", "generic")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"generic"}`), 0644)

	agentDir = filepath.Join(scionDir, "agents", agentName)
	agentHome = config.GetAgentHomePath(scionDir, agentName)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	return scionDir, agentDir, agentHome
}

// An agent dir with a populated home and agent-info.json but no
// scion-agent.json (an agent created before that file existed) is kept:
// GetAgent writes a minimal scion-agent.json from agent-info.json and leaves
// the home contents in place (ptone/scion#4438).
func TestGetAgent_NoConfigPopulatedHomeWithRecordIsKept(t *testing.T) {
	agentName := "legacy-agent"
	scionDir, agentDir, agentHome := setupNoConfigAgentProject(t, agentName)

	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	homeFile := filepath.Join(agentHome, ".history")
	if err := os.WriteFile(homeFile, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"),
		[]byte(`{"name":"legacy-agent","template":"default","harnessConfig":"generic"}`), 0644); err != nil {
		t.Fatal(err)
	}

	gotDir, gotHome, _, cfg, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}
	if gotDir != agentDir || gotHome != agentHome {
		t.Fatalf("GetAgent returned dir=%q home=%q, want %q and %q", gotDir, gotHome, agentDir, agentHome)
	}

	data, err := os.ReadFile(homeFile)
	if err != nil {
		t.Fatalf("home file was not kept: %v", err)
	}
	if string(data) != "keep me" {
		t.Fatalf("home file content = %q, want %q", data, "keep me")
	}

	cfgData, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		t.Fatalf("expected scion-agent.json to be written: %v", err)
	}
	var written api.ScionConfig
	if err := json.Unmarshal(cfgData, &written); err != nil {
		t.Fatalf("written scion-agent.json does not parse: %v", err)
	}
	if written.HarnessConfig != "generic" {
		t.Errorf("written harness_config = %q, want %q", written.HarnessConfig, "generic")
	}

	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.HarnessConfig != "generic" {
		t.Errorf("config harness_config = %q, want %q", cfg.HarnessConfig, "generic")
	}
	if cfg.Info == nil || cfg.Info.Template != "default" {
		t.Errorf("config Info = %+v, want template %q from agent-info.json", cfg.Info, "default")
	}
}

// An agent dir with no config file and no home state is stale: GetAgent
// still removes it and provisions the agent afresh.
func TestGetAgent_NoConfigNoHomeIsRemoved(t *testing.T) {
	agentName := "stale-nohome-agent"
	scionDir, agentDir, _ := setupNoConfigAgentProject(t, agentName)

	leftover := filepath.Join(agentDir, "leftover.txt")
	if err := os.WriteFile(leftover, []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	_, _, _, cfg, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("expected the stale dir to be removed (leftover file still present, stat err=%v)", err)
	}
	if !config.ScionAgentConfigExists(agentDir) {
		t.Fatal("expected the agent to be re-provisioned with a config file")
	}
}

// An agent dir with a populated home but no config file and no
// agent-info.json is never removed: GetAgent returns an error wrapping
// config.ErrAgentStateConflict and leaves every file in place.
func TestGetAgent_NoConfigPopulatedHomeWithoutRecordIsRefused(t *testing.T) {
	agentName := "no-record-agent"
	scionDir, agentDir, agentHome := setupNoConfigAgentProject(t, agentName)

	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	homeFile := filepath.Join(agentHome, ".history")
	if err := os.WriteFile(homeFile, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	dirFile := filepath.Join(agentDir, "prompt.md")
	if err := os.WriteFile(dirFile, []byte("task"), 0644); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected an error for a populated home with no stored record")
	}
	if !errors.Is(err, config.ErrAgentStateConflict) {
		t.Fatalf("error = %v, want it to wrap config.ErrAgentStateConflict", err)
	}
	for _, f := range []string{homeFile, dirFile} {
		if _, statErr := os.Stat(f); statErr != nil {
			t.Errorf("expected %s to be kept: %v", f, statErr)
		}
	}
	if config.ScionAgentConfigExists(agentDir) {
		t.Error("expected no scion-agent config to be written without a stored record")
	}
}

// An agent dir whose home/ exists but is empty holds no home state: it is
// stale, so GetAgent removes it and provisions the agent afresh.
func TestGetAgent_NoConfigEmptyHomeIsRemoved(t *testing.T) {
	agentName := "stale-emptyhome-agent"
	scionDir, agentDir, agentHome := setupNoConfigAgentProject(t, agentName)

	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(agentDir, "leftover.txt")
	if err := os.WriteFile(leftover, []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	_, _, _, cfg, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("expected the stale dir to be removed (leftover file still present, stat err=%v)", err)
	}
	if !config.ScionAgentConfigExists(agentDir) {
		t.Fatal("expected the agent to be re-provisioned with a config file")
	}
}

// The conflict error is returned to clients by brokers, so it must not
// name a host path.
func TestGetAgent_NoConfigRefusalNamesNoHostPath(t *testing.T) {
	agentName := "no-record-path-agent"
	scionDir, _, agentHome := setupNoConfigAgentProject(t, agentName)

	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, ".history"), []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if !errors.Is(err, config.ErrAgentStateConflict) {
		t.Fatalf("error = %v, want it to wrap config.ErrAgentStateConflict", err)
	}
	if strings.Contains(err.Error(), string(filepath.Separator)) {
		t.Errorf("error text %q contains a path", err.Error())
	}
}

// A home that cannot be read may hold data: the dir is kept and the call
// returns the conflict error instead of removing it.
func TestHandleAgentDirWithoutConfig_UnreadableHomeIsKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 directory")
	}
	tmp := t.TempDir()
	agentDir := filepath.Join(tmp, "agents", "a")
	agentHome := filepath.Join(agentDir, "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	homeFile := filepath.Join(agentHome, ".history")
	if err := os.WriteFile(homeFile, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(agentHome, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(agentHome, 0755) })

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, filepath.Join(agentDir, "workspace"))
	if !errors.Is(err, config.ErrAgentStateConflict) {
		t.Fatalf("error = %v, want it to wrap config.ErrAgentStateConflict", err)
	}
	if strings.Contains(err.Error(), tmp) {
		t.Errorf("error text %q contains a host path", err.Error())
	}
	if err := os.Chmod(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(homeFile); statErr != nil {
		t.Fatalf("expected the home file to be kept: %v", statErr)
	}
}

// splitLayout returns an agent dir and an agent home that sits outside it,
// as with an external agents directory.
func splitLayout(t *testing.T) (agentDir, agentHome string) {
	t.Helper()
	tmp := t.TempDir()
	agentDir = filepath.Join(tmp, "project", ".scion", "agents", "a")
	agentHome = filepath.Join(tmp, "external", "a", "home")
	for _, d := range []string{agentDir, agentHome} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agentHome, ".history"), []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	return agentDir, agentHome
}

// Split layout: a populated external home with a record is adopted.
func TestHandleAgentDirWithoutConfig_SplitLayoutWithRecordIsAdopted(t *testing.T) {
	agentDir, agentHome := splitLayout(t)
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"),
		[]byte(`{"name":"a","template":"default","harnessConfig":"generic"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, filepath.Join(agentDir, "workspace")); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if !config.ScionAgentConfigExists(agentDir) {
		t.Fatal("expected scion-agent.json to be written in the agent dir")
	}
	if _, err := os.Stat(filepath.Join(agentHome, ".history")); err != nil {
		t.Fatalf("expected the home file to be kept: %v", err)
	}
}

// Split layout: a populated external home with no record is refused and
// kept.
func TestHandleAgentDirWithoutConfig_SplitLayoutWithoutRecordIsRefused(t *testing.T) {
	agentDir, agentHome := splitLayout(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, filepath.Join(agentDir, "workspace"))
	if !errors.Is(err, config.ErrAgentStateConflict) {
		t.Fatalf("error = %v, want it to wrap config.ErrAgentStateConflict", err)
	}
	if _, statErr := os.Stat(agentDir); statErr != nil {
		t.Fatalf("expected the agent dir to be kept: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(agentHome, ".history")); statErr != nil {
		t.Fatalf("expected the home file to be kept: %v", statErr)
	}
}

// Split layout: a record found only in agentDir/home (not the home GetAgent
// reads) is not used for adoption; the populated dir is refused and kept.
func TestHandleAgentDirWithoutConfig_RecordOnlyInAgentDirHomeIsRefused(t *testing.T) {
	tmp := t.TempDir()
	agentDir := filepath.Join(tmp, "project", ".scion", "agents", "a")
	agentHome := filepath.Join(tmp, "external", "a", "home")
	inDirHome := filepath.Join(agentDir, "home")
	if err := os.MkdirAll(inDirHome, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inDirHome, "agent-info.json"),
		[]byte(`{"name":"a","template":"default","harnessConfig":"generic"}`), 0644); err != nil {
		t.Fatal(err)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, filepath.Join(agentDir, "workspace"))
	if !errors.Is(err, config.ErrAgentStateConflict) {
		t.Fatalf("error = %v, want it to wrap config.ErrAgentStateConflict", err)
	}
	if config.ScionAgentConfigExists(agentDir) {
		t.Error("expected no scion-agent config to be written")
	}
	if _, statErr := os.Stat(filepath.Join(inDirHome, "agent-info.json")); statErr != nil {
		t.Fatalf("expected agentDir/home to be kept: %v", statErr)
	}
}

// A first provision that fails after creating the agent dir removes it, so
// the next start provisions afresh instead of meeting a kept, half-written
// dir.
func TestProvisionAgent_FailedFirstProvisionRemovesCreatedDir(t *testing.T) {
	agentName := "failed-first-agent"
	scionDir, agentDir, _ := setupNoConfigAgentProject(t, agentName)
	if err := os.RemoveAll(agentDir); err != nil {
		t.Fatal(err)
	}

	ctx := api.ContextWithGitClone(context.Background(), &api.GitCloneConfig{URL: "https://example.com/o/r.git"})
	_, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "", "", "relative/ws")
	if err == nil {
		t.Fatal("expected ProvisionAgent to fail for a relative workspace with git clone")
	}
	if _, statErr := os.Stat(agentDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the agent dir created by the failed provision to be removed (stat err=%v)", statErr)
	}
}

// A provision that fails on a dir that existed before the call never
// removes it.
func TestProvisionAgent_FailedProvisionKeepsPreexistingDir(t *testing.T) {
	agentName := "failed-existing-agent"
	scionDir, agentDir, _ := setupNoConfigAgentProject(t, agentName)
	marker := filepath.Join(agentDir, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := api.ContextWithGitClone(context.Background(), &api.GitCloneConfig{URL: "https://example.com/o/r.git"})
	_, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "", "", "relative/ws")
	if err == nil {
		t.Fatal("expected ProvisionAgent to fail for a relative workspace with git clone")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("expected the pre-existing agent dir to be kept: %v", statErr)
	}
}
