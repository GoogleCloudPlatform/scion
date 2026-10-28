/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
)

// TestRunInit_MetadataServerStartFailure drives the real RunInit with the
// metadata server's Start replaced, and checks which Start failures stop
// the agent before the harness runs (exit 1, error phase reported) and which
// let it continue: assign fails closed in every network mode, block fails
// closed outside host networking, a required interception that is not in
// place fails closed, and block under host networking continues.
func TestRunInit_MetadataServerStartFailure(t *testing.T) {
	listenErr := errors.New("metadata server listen: address already in use")
	interceptErr := fmt.Errorf("%w: redirect: exit status 4", metadata.ErrInterceptionUnavailable)

	tests := []struct {
		name        string
		mode        string
		networkMode string
		startErr    error
		wantHarness bool
	}{
		{name: "assign bridge listen failure", mode: "assign", startErr: listenErr, wantHarness: false},
		{name: "assign host listen failure", mode: "assign", networkMode: "host", startErr: listenErr, wantHarness: false},
		{name: "block bridge listen failure", mode: "block", startErr: listenErr, wantHarness: false},
		{name: "block bridge interception unavailable", mode: "block", startErr: interceptErr, wantHarness: false},
		{name: "assign bridge interception unavailable", mode: "assign", startErr: interceptErr, wantHarness: false},
		{name: "block host listen failure", mode: "block", networkMode: "host", startErr: listenErr, wantHarness: true},
		{name: "block bridge started", mode: "block", startErr: nil, wantHarness: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentHome := t.TempDir()
			setupRunInitAsRootlessScion(t, agentHome)
			t.Setenv("SCION_METADATA_MODE", tt.mode)
			if tt.networkMode != "" {
				t.Setenv("SCION_NETWORK_MODE", tt.networkMode)
			}

			withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error { return nil })
			withRunFetchSecretOverrides(t, func(*hub.Client, []string) map[string]string { return nil })
			var startCalled bool
			withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error {
				startCalled = true
				return tt.startErr
			})

			sentinel := filepath.Join(t.TempDir(), "harness-started")
			got := RunInit([]string{"sh", "-c", "touch " + sentinel}, InitRunOptions{DisableTermSignalForwarding: true})

			if !startCalled {
				t.Fatal("the metadata server start seam was never called")
			}
			_, statErr := os.Stat(sentinel)
			harnessRan := statErr == nil
			if harnessRan != tt.wantHarness {
				t.Fatalf("harness ran = %v, want %v (RunInit() = %d)", harnessRan, tt.wantHarness, got)
			}
			if tt.wantHarness {
				return
			}
			if got != 1 {
				t.Errorf("RunInit() = %d, want 1", got)
			}
			raw, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
			if err != nil {
				t.Fatalf("expected agent-info.json to be written: %v", err)
			}
			var info struct {
				Phase  string `json:"phase"`
				Detail struct {
					Message string `json:"message"`
				} `json:"detail"`
			}
			if err := json.Unmarshal(raw, &info); err != nil {
				t.Fatalf("unmarshal agent-info.json %q: %v", raw, err)
			}
			if info.Phase != string(state.PhaseError) {
				t.Errorf("agent-info.json phase = %q, want %q", info.Phase, state.PhaseError)
			}
			if !strings.Contains(info.Detail.Message, tt.startErr.Error()) {
				t.Errorf("agent-info.json detail.message = %q, want it to contain %q", info.Detail.Message, tt.startErr.Error())
			}
		})
	}
}
