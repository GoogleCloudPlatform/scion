//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
)

// TestAgentInfoHostAndContainerWritersLoseNoField runs the host writer
// (updateSavedAgentInfo via SetSavedRunID, as the broker does after
// Runtime.Run) against the container writer (sciontool's StatusHandler) on
// one bind-mounted agent-info.json (ptone/scion#4681). The host only sets
// runId and the container only sets detail.message, each to an increasing
// counter. Neither may ever go back below what its writer has finished:
// that would be the other side writing back a stale read.
func TestAgentInfoHostAndContainerWritersLoseNoField(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), config.DotScion)
	agentName := "race-agent"
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(agentHome, "agent-info.json")
	if err := os.WriteFile(path, []byte(`{"template":"tpl","phase":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	container := &handlers.StatusHandler{StatusPath: path}

	const n = 300
	var doneHost, doneCtr atomic.Int64
	doneHost.Store(-1)
	doneCtr.Store(-1)
	var lost atomic.Bool

	counter := func(v any, prefix string) int64 {
		s, _ := v.(string)
		var c int64
		if _, err := fmt.Sscanf(s, prefix+"-%d", &c); err != nil {
			return -1
		}
		return c
	}
	check := func(who string) {
		wantHost, wantCtr := doneHost.Load(), doneCtr.Load()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", who, err)
			return
		}
		var info map[string]any
		if err := json.Unmarshal(data, &info); err != nil {
			t.Errorf("%s: parse: %v", who, err)
			return
		}
		detail, _ := info["detail"].(map[string]any)
		gotHost := counter(info["runId"], "run")
		gotCtr := counter(detail["message"], "ctr")
		if (gotHost < wantHost || gotCtr < wantCtr || info["template"] != "tpl") && lost.CompareAndSwap(false, true) {
			t.Errorf("%s saw a lost update: runId run-%d (finished run-%d), message ctr-%d (finished ctr-%d), template %v",
				who, gotHost, wantHost, gotCtr, wantCtr, info["template"])
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := int64(0); i < n && !lost.Load(); i++ {
			if err := SetSavedRunID(agentName, projectDir, fmt.Sprintf("run-%d", i)); err != nil {
				t.Errorf("host: %v", err)
				return
			}
			doneHost.Store(i)
			check("host")
		}
	}()
	go func() {
		defer wg.Done()
		for i := int64(0); i < n && !lost.Load(); i++ {
			if err := container.SetMessage(fmt.Sprintf("ctr-%d", i)); err != nil {
				t.Errorf("container: %v", err)
				return
			}
			doneCtr.Store(i)
			check("container")
		}
	}()
	wg.Wait()
	check("final")
}
