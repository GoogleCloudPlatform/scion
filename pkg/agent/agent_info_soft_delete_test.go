/*
Copyright 2026 The Scion Authors.
*/

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestMarkAgentSoftDeleted(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), config.DotScion)
	agentName := "soft-agent"
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), []byte(`{"phase":"running","extra":"kept"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MarkAgentSoftDeleted(agentName, projectDir, time.Time{}); err != nil {
		t.Fatal(err)
	}
	st := getSavedAgentInfo(agentName, projectDir)
	if st == nil || st.Phase != "deleted" || !st.DeletedAt.IsZero() {
		t.Fatalf("after mark without time: %+v", st)
	}

	at := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	if err := MarkAgentSoftDeleted(agentName, projectDir, at); err != nil {
		t.Fatal(err)
	}
	st = getSavedAgentInfo(agentName, projectDir)
	if st == nil || st.Phase != "deleted" || !st.DeletedAt.Equal(at) {
		t.Fatalf("after mark with time: %+v", st)
	}
	data, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if err != nil || !strings.Contains(string(data), `"extra": "kept"`) && !strings.Contains(string(data), `"extra":"kept"`) {
		t.Fatalf("unknown key lost: %s %v", data, err)
	}
}
