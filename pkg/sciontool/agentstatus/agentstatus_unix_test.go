//go:build unix

/*
Copyright 2025 The Scion Authors.
*/

package agentstatus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

func readActivity(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read agent info: %v", err)
	}
	var info struct {
		Activity string `json:"activity"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("parse agent info: %v", err)
	}
	return info.Activity
}

// TestRunAskUserStaysWaitingThroughToolEnd: the ask_user update writes
// waiting_for_input to the agent state file, and the tool-end hook that
// follows the command (it runs inside a harness shell tool) keeps it.
func TestRunAskUserStaysWaitingThroughToolEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No Hub: the update stays local.
	t.Setenv(hub.EnvHubEndpoint, "")
	t.Setenv(hub.EnvHubURL, "")
	t.Setenv(hub.EnvHubToken, "")
	t.Setenv(hub.EnvAgentID, "")

	RunAskUser("Decision needed: ship it?")

	statusPath := filepath.Join(home, "agent-info.json")
	if got := readActivity(t, statusPath); got != "waiting_for_input" {
		t.Fatalf("activity after ask_user = %q, want waiting_for_input", got)
	}

	h := &handlers.StatusHandler{StatusPath: statusPath}
	if err := h.Handle(&hooks.Event{Name: hooks.EventToolEnd}); err != nil {
		t.Fatalf("tool-end: %v", err)
	}
	if got := readActivity(t, statusPath); got != "waiting_for_input" {
		t.Fatalf("activity after tool-end = %q, want waiting_for_input (sticky)", got)
	}

	// Same-turn tool-start (e.g., `scion message` after `ask_user`) must
	// keep waiting_for_input.
	if err := h.Handle(&hooks.Event{Name: hooks.EventToolStart}); err != nil {
		t.Fatalf("same-turn tool-start: %v", err)
	}
	if got := readActivity(t, statusPath); got != "waiting_for_input" {
		t.Fatalf("activity after same-turn tool-start = %q, want waiting_for_input", got)
	}

	// End of turn keeps waiting_for_input and arms the turn-end marker.
	if err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd}); err != nil {
		t.Fatalf("agent-end: %v", err)
	}
	if got := readActivity(t, statusPath); got != "waiting_for_input" {
		t.Fatalf("activity after agent-end = %q, want waiting_for_input", got)
	}

	// Positive control: a tool-start on a subsequent turn clears the
	// turn-ended ask-user wait.
	if err := h.Handle(&hooks.Event{Name: hooks.EventToolStart}); err != nil {
		t.Fatalf("next-turn tool-start: %v", err)
	}
	if got := readActivity(t, statusPath); got == "waiting_for_input" {
		t.Fatalf("activity after next-turn tool-start = %q, want it cleared", got)
	}
}
