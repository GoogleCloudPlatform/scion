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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureListStdout redirects os.Stdout to a pipe for the duration of fn and
// discards everything written to it, draining concurrently so fn can never
// block on a full pipe buffer.
func captureListStdout(fn func()) {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(done)
	}()
	fn()
	_ = w.Close()
	os.Stdout = oldStdout
	<-done
	_ = r.Close()
}

func TestFormatLastActivity(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		status   string
		t        time.Time
		expected string
	}{
		{"activity with time", "thinking", now.Add(-30 * time.Second), "thinking, just now"},
		{"phase with time", "stopped", now.Add(-2 * time.Hour), "stopped, 2h ago"},
		{"empty status with time", "", now.Add(-5 * time.Minute), "5m ago"},
		{"WORKING status with time", "WORKING", now.Add(-5 * time.Minute), "5m ago"},
		{"working status with time", "working", now.Add(-5 * time.Minute), "5m ago"},
		{"activity with zero time", "running", time.Time{}, "running"},
		{"empty status with zero time", "", time.Time{}, "-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatLastActivity(tt.status, tt.t)
			if result != tt.expected {
				t.Errorf("formatLastActivity(%q, ...) = %q, want %q", tt.status, result, tt.expected)
			}
		})
	}
}

func TestDisplayAgentsLocalMode(t *testing.T) {
	agents := []api.AgentInfo{
		{
			Name:            tid("agent-1"),
			Template:        "default",
			HarnessConfig:   "claude",
			Runtime:         "docker",
			Project:         "my-project",
			Phase:           "running",
			Activity:        "thinking",
			ContainerStatus: "Up 2 hours",
			LastSeen:        time.Now().Add(-30 * time.Second),
		},
		{
			Name:            "agent-2",
			Template:        "research",
			Runtime:         "docker",
			Project:         "my-project",
			Phase:           "stopped",
			ContainerStatus: "created",
			// No HarnessConfig, no LastSeen
		},
	}

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	// Verify header contains all expected columns
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines (header + 2 agents), got %d: %s", len(lines), output)
	}

	header := lines[0]
	for _, col := range []string{"NAME", "TEMPLATE", "HARNESS-CFG", "RUNTIME", "PROJECT", "PHASE", "CONTAINER", "LAST ACTIVITY"} {
		if !strings.Contains(header, col) {
			t.Errorf("header missing column %q: %s", col, header)
		}
	}

	// Verify first agent row has harness config value and phase column shows "running"
	if !strings.Contains(lines[1], "claude") {
		t.Errorf("agent-1 row should contain harness config 'claude': %s", lines[1])
	}
	if !strings.Contains(lines[1], "running") {
		t.Errorf("agent-1 row should contain phase 'running': %s", lines[1])
	}
	if !strings.Contains(lines[1], "thinking, just now") {
		t.Errorf("agent-1 row should contain 'thinking, just now': %s", lines[1])
	}

	// Verify second agent row shows "-" for missing harness config
	if !strings.Contains(lines[2], "-") {
		t.Errorf("agent-2 row should contain '-' for missing values: %s", lines[2])
	}
}

func TestDisplayAgentsHubMode(t *testing.T) {
	agents := []api.AgentInfo{
		{
			Name:              "hub-agent",
			Template:          "default",
			HarnessConfig:     "gemini",
			Runtime:           "docker",
			Project:           "hub-project",
			RuntimeBrokerName: "local-broker",
			Phase:             "running",
			ContainerStatus:   "Up 5 minutes",
			LastSeen:          time.Now().Add(-2 * time.Minute),
		},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, true)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}

	header := lines[0]
	// Hub mode should have BROKER column
	for _, col := range []string{"NAME", "TEMPLATE", "HARNESS-CFG", "RUNTIME", "PROJECT", "BROKER", "PHASE", "CONTAINER", "LAST ACTIVITY"} {
		if !strings.Contains(header, col) {
			t.Errorf("hub mode header missing column %q: %s", col, header)
		}
	}

	// Verify agent row shows phase "running" and activity is not mixed in
	if !strings.Contains(lines[1], "gemini") {
		t.Errorf("hub agent row should contain harness config 'gemini': %s", lines[1])
	}
	if !strings.Contains(lines[1], "local-broker") {
		t.Errorf("hub agent row should contain broker name: %s", lines[1])
	}
	if !strings.Contains(lines[1], "running") {
		t.Errorf("hub agent row should contain phase 'running': %s", lines[1])
	}
	// No activity set, so last activity should show just the timestamp
	if !strings.Contains(lines[1], "2m ago") {
		t.Errorf("hub agent row should contain '2m ago': %s", lines[1])
	}
}

func TestDisplayAgentsSortByTime(t *testing.T) {
	now := time.Now()
	agents := []api.AgentInfo{
		{
			Name:     "old-agent",
			Template: "default",
			Runtime:  "docker",
			Project:  "my-project",
			LastSeen: now.Add(-10 * time.Minute),
		},
		{
			Name:     "new-agent",
			Template: "default",
			Runtime:  "docker",
			Project:  "my-project",
			LastSeen: now.Add(-1 * time.Minute),
		},
		{
			Name:     "mid-agent",
			Template: "default",
			Runtime:  "docker",
			Project:  "my-project",
			LastSeen: now.Add(-5 * time.Minute),
		},
	}

	// Enable sort-by-time flag
	sortByTime = true
	defer func() { sortByTime = false }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected 4 lines (header + 3 agents), got %d: %s", len(lines), output)
	}

	// Most recent first: new-agent, mid-agent, old-agent
	if !strings.Contains(lines[1], "new-agent") {
		t.Errorf("first agent should be 'new-agent' (most recent), got: %s", lines[1])
	}
	if !strings.Contains(lines[2], "mid-agent") {
		t.Errorf("second agent should be 'mid-agent', got: %s", lines[2])
	}
	if !strings.Contains(lines[3], "old-agent") {
		t.Errorf("third agent should be 'old-agent' (oldest), got: %s", lines[3])
	}
}

func TestDisplayAgentsEmpty(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(nil, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "No active agents found in the current project.") {
		t.Errorf("expected empty project message, got: %s", output)
	}
}

func TestDisplayAgentsEmptyAll(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(nil, true, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "No active agents found across any projects.") {
		t.Errorf("expected all-projects empty message, got: %s", output)
	}
}

func TestDisplayAgentsFriendlyTemplateName(t *testing.T) {
	agents := []api.AgentInfo{
		{
			Name:            "agent-cache-path",
			Template:        "/home/user/.scion/templates/cache/abc123/claude",
			Runtime:         "docker",
			Project:         "my-project",
			Phase:           "running",
			ContainerStatus: "Up 1 hour",
		},
		{
			Name:            "agent-simple",
			Template:        "gemini",
			Runtime:         "docker",
			Project:         "my-project",
			Phase:           "running",
			ContainerStatus: "Up 2 hours",
		},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines, got %d: %s", len(lines), output)
	}

	// Cache path should be resolved to friendly name "claude"
	if strings.Contains(lines[1], "/home/user") {
		t.Errorf("agent row should NOT contain cache path, got: %s", lines[1])
	}
	if !strings.Contains(lines[1], "claude") {
		t.Errorf("agent row should contain friendly template name 'claude': %s", lines[1])
	}

	// Simple name should pass through unchanged
	if !strings.Contains(lines[2], "gemini") {
		t.Errorf("agent row should contain template name 'gemini': %s", lines[2])
	}
}

func TestHubAgentPhaseActivity_PrefersPhaseField(t *testing.T) {
	// When Phase is set, it should be used directly regardless of Status
	phase, activity := hubAgentPhaseActivity("running", "thinking", "")
	if phase != "running" {
		t.Errorf("phase = %q, want %q", phase, "running")
	}
	if activity != "thinking" {
		t.Errorf("activity = %q, want %q", activity, "thinking")
	}
}

func TestHubAgentPhaseActivity_FallsBackToStatus(t *testing.T) {
	// When Phase is empty, fall back to deriving from Status
	phase, activity := hubAgentPhaseActivity("", "", "waiting_for_input")
	if phase != "running" {
		t.Errorf("phase = %q, want %q (derived from status activity)", phase, "running")
	}
	if activity != "waiting_for_input" {
		t.Errorf("activity = %q, want %q", activity, "waiting_for_input")
	}
}

func TestHubAgentPhaseActivity_EmptyAll(t *testing.T) {
	// When all fields are empty, returns empty
	phase, activity := hubAgentPhaseActivity("", "", "")
	if phase != "" {
		t.Errorf("phase = %q, want empty", phase)
	}
	if activity != "" {
		t.Errorf("activity = %q, want empty", activity)
	}
}

func TestHubAgentToAgentInfo_PhaseFromPhaseField(t *testing.T) {
	// When the Hub returns phase and activity fields directly, use them
	a := hubclient.Agent{
		ID:              "agent-phase",
		Name:            "test-agent",
		Phase:           "running",
		Activity:        "thinking",
		ContainerStatus: "running",
	}
	info := hubAgentToAgentInfo(a)
	if info.Phase != "running" {
		t.Errorf("Phase = %q, want %q", info.Phase, "running")
	}
	if info.Activity != "thinking" {
		t.Errorf("Activity = %q, want %q", info.Activity, "thinking")
	}
}

func TestHubAgentToAgentInfo_PhaseFromStatusFallback(t *testing.T) {
	// When Phase is empty but Status has a value, derive from it
	a := hubclient.Agent{
		ID:     "agent-legacy",
		Name:   "test-agent",
		Status: "running",
	}
	info := hubAgentToAgentInfo(a)
	if info.Phase != "running" {
		t.Errorf("Phase = %q, want %q (derived from Status)", info.Phase, "running")
	}
}

func TestHubAgentToAgentInfo_HarnessConfigFromTopLevel(t *testing.T) {
	// When the Hub returns harnessConfig at the top level, use it directly
	a := hubclient.Agent{
		ID:            tid("agent-1"),
		Name:          "test-agent",
		HarnessConfig: "gemini",
	}
	info := hubAgentToAgentInfo(a)
	if info.HarnessConfig != "gemini" {
		t.Errorf("HarnessConfig = %q, want %q", info.HarnessConfig, "gemini")
	}
}

func TestHubAgentToAgentInfo_HarnessConfigFallbackToAppliedConfig(t *testing.T) {
	// When the Hub does NOT return harnessConfig at the top level (older Hub),
	// fall back to AppliedConfig.HarnessConfig
	a := hubclient.Agent{
		ID:   "agent-2",
		Name: "test-agent-2",
		AppliedConfig: &hubclient.AgentConfig{
			HarnessConfig: "claude",
		},
	}
	info := hubAgentToAgentInfo(a)
	if info.HarnessConfig != "claude" {
		t.Errorf("HarnessConfig = %q, want %q (should fall back to AppliedConfig.HarnessConfig)", info.HarnessConfig, "claude")
	}
}

func TestFilterRunningAgents(t *testing.T) {
	agents := []api.AgentInfo{
		{Name: "running-agent", Phase: "running"},
		{Name: "stopped-agent", Phase: "stopped"},
		{Name: "error-agent", Phase: "error"},
		{Name: "starting-agent", Phase: "starting"},
		{Name: "created-agent", Phase: "created"},
		{Name: "provisioning-agent", Phase: "provisioning"},
		{Name: "unknown-agent", Phase: "unknown"},
		{Name: "empty-phase-agent", Phase: ""},
	}

	filtered := filterRunningAgents(agents)

	// Should exclude stopped and error, keep everything else
	expected := map[string]bool{
		"running-agent":      true,
		"starting-agent":     true,
		"created-agent":      true,
		"provisioning-agent": true,
		"unknown-agent":      true,
		"empty-phase-agent":  true,
	}

	if len(filtered) != len(expected) {
		t.Fatalf("expected %d agents, got %d", len(expected), len(filtered))
	}
	for _, a := range filtered {
		if !expected[a.Name] {
			t.Errorf("unexpected agent in filtered list: %s (phase=%s)", a.Name, a.Phase)
		}
	}
}

func TestDisplayAgentsRunningFlag(t *testing.T) {
	agents := []api.AgentInfo{
		{
			Name:            "active-agent",
			Template:        "default",
			Runtime:         "docker",
			Project:         "my-project",
			Phase:           "running",
			ContainerStatus: "Up 1 hour",
		},
		{
			Name:            "stopped-agent",
			Template:        "default",
			Runtime:         "docker",
			Project:         "my-project",
			Phase:           "stopped",
			ContainerStatus: "Exited",
		},
	}

	listRunning = true
	defer func() { listRunning = false }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "active-agent") {
		t.Errorf("output should contain running agent 'active-agent': %s", output)
	}
	if strings.Contains(output, "stopped-agent") {
		t.Errorf("output should NOT contain stopped agent 'stopped-agent': %s", output)
	}
}

func TestHubAgentToAgentInfo_HarnessConfigTopLevelTakesPrecedence(t *testing.T) {
	// When both are set, top-level harnessConfig takes precedence
	a := hubclient.Agent{
		ID:            "agent-3",
		Name:          "test-agent-3",
		HarnessConfig: "gemini",
		AppliedConfig: &hubclient.AgentConfig{
			HarnessConfig: "claude",
		},
	}
	info := hubAgentToAgentInfo(a)
	if info.HarnessConfig != "gemini" {
		t.Errorf("HarnessConfig = %q, want %q (top-level should take precedence)", info.HarnessConfig, "gemini")
	}
}

func TestFilterAgentsByPhase(t *testing.T) {
	agents := []api.AgentInfo{
		{Name: "running-1", Phase: "running", Template: "default", Runtime: "docker", Project: "p"},
		{Name: "stopped-1", Phase: "stopped", Template: "default", Runtime: "docker", Project: "p"},
		{Name: "running-2", Phase: "running", Template: "claude", Runtime: "docker", Project: "p"},
		{Name: "error-1", Phase: "error", Template: "default", Runtime: "docker", Project: "p"},
	}

	filterPhase = "running"
	defer func() { filterPhase = "" }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "running-1") {
		t.Errorf("output should contain 'running-1': %s", output)
	}
	if !strings.Contains(output, "running-2") {
		t.Errorf("output should contain 'running-2': %s", output)
	}
	if strings.Contains(output, "stopped-1") {
		t.Errorf("output should NOT contain 'stopped-1': %s", output)
	}
	if strings.Contains(output, "error-1") {
		t.Errorf("output should NOT contain 'error-1': %s", output)
	}
}

func TestFilterAgentsByActivity(t *testing.T) {
	agents := []api.AgentInfo{
		{Name: "thinking-agent", Phase: "running", Activity: "thinking", Template: "default", Runtime: "docker", Project: "p"},
		{Name: "waiting-agent", Phase: "running", Activity: "waiting_for_input", Template: "default", Runtime: "docker", Project: "p"},
		{Name: "no-activity", Phase: "stopped", Template: "default", Runtime: "docker", Project: "p"},
	}

	filterActivity = "thinking"
	defer func() { filterActivity = "" }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "thinking-agent") {
		t.Errorf("output should contain 'thinking-agent': %s", output)
	}
	if strings.Contains(output, "waiting-agent") {
		t.Errorf("output should NOT contain 'waiting-agent': %s", output)
	}
	if strings.Contains(output, "no-activity") {
		t.Errorf("output should NOT contain 'no-activity': %s", output)
	}
}

func TestFilterAgentsByTemplate(t *testing.T) {
	agents := []api.AgentInfo{
		{Name: "claude-agent", Phase: "running", Template: "claude", Runtime: "docker", Project: "p"},
		{Name: "gemini-agent", Phase: "running", Template: "gemini", Runtime: "docker", Project: "p"},
	}

	filterTemplate = "claude"
	defer func() { filterTemplate = "" }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "claude-agent") {
		t.Errorf("output should contain 'claude-agent': %s", output)
	}
	if strings.Contains(output, "gemini-agent") {
		t.Errorf("output should NOT contain 'gemini-agent': %s", output)
	}
}

func TestFilterAgentsCombined(t *testing.T) {
	agents := []api.AgentInfo{
		{Name: "match", Phase: "running", Activity: "thinking", Template: "claude", Runtime: "docker", Project: "p"},
		{Name: "wrong-phase", Phase: "stopped", Activity: "thinking", Template: "claude", Runtime: "docker", Project: "p"},
		{Name: "wrong-activity", Phase: "running", Activity: "executing", Template: "claude", Runtime: "docker", Project: "p"},
		{Name: "wrong-template", Phase: "running", Activity: "thinking", Template: "gemini", Runtime: "docker", Project: "p"},
	}

	filterPhase = "running"
	filterActivity = "thinking"
	filterTemplate = "claude"
	defer func() { filterPhase = ""; filterActivity = ""; filterTemplate = "" }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (header + 1 agent), got %d: %s", len(lines), output)
	}
	if !strings.Contains(lines[1], "match") {
		t.Errorf("only 'match' agent should appear: %s", lines[1])
	}
}

func TestSortAgentsByName(t *testing.T) {
	agents := []api.AgentInfo{
		{Name: "charlie", Template: "default", Runtime: "docker", Project: "p", Phase: "running"},
		{Name: "alice", Template: "default", Runtime: "docker", Project: "p", Phase: "running"},
		{Name: "bob", Template: "default", Runtime: "docker", Project: "p", Phase: "running"},
	}

	sortField = "name"
	defer func() { sortField = "" }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected 4 lines, got %d: %s", len(lines), output)
	}
	if !strings.Contains(lines[1], "alice") {
		t.Errorf("first agent should be 'alice': %s", lines[1])
	}
	if !strings.Contains(lines[2], "bob") {
		t.Errorf("second agent should be 'bob': %s", lines[2])
	}
	if !strings.Contains(lines[3], "charlie") {
		t.Errorf("third agent should be 'charlie': %s", lines[3])
	}
}

func TestSortAgentsByCreated(t *testing.T) {
	now := time.Now()
	agents := []api.AgentInfo{
		{Name: "oldest", Template: "default", Runtime: "docker", Project: "p", Phase: "running", Created: now.Add(-3 * time.Hour)},
		{Name: "newest", Template: "default", Runtime: "docker", Project: "p", Phase: "running", Created: now.Add(-1 * time.Hour)},
		{Name: "middle", Template: "default", Runtime: "docker", Project: "p", Phase: "running", Created: now.Add(-2 * time.Hour)},
	}

	sortField = "created"
	defer func() { sortField = "" }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected 4 lines, got %d: %s", len(lines), output)
	}
	// Timestamps default to descending (newest first)
	if !strings.Contains(lines[1], "newest") {
		t.Errorf("first agent should be 'newest': %s", lines[1])
	}
	if !strings.Contains(lines[2], "middle") {
		t.Errorf("second agent should be 'middle': %s", lines[2])
	}
	if !strings.Contains(lines[3], "oldest") {
		t.Errorf("third agent should be 'oldest': %s", lines[3])
	}
}

func TestSortAgentsReverse(t *testing.T) {
	now := time.Now()
	agents := []api.AgentInfo{
		{Name: "oldest", Template: "default", Runtime: "docker", Project: "p", Phase: "running", Created: now.Add(-3 * time.Hour)},
		{Name: "newest", Template: "default", Runtime: "docker", Project: "p", Phase: "running", Created: now.Add(-1 * time.Hour)},
		{Name: "middle", Template: "default", Runtime: "docker", Project: "p", Phase: "running", Created: now.Add(-2 * time.Hour)},
	}

	sortField = "created"
	sortReverse = true
	defer func() { sortField = ""; sortReverse = false }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected 4 lines, got %d: %s", len(lines), output)
	}
	// --reverse on timestamp: ascending (oldest first)
	if !strings.Contains(lines[1], "oldest") {
		t.Errorf("first agent should be 'oldest': %s", lines[1])
	}
	if !strings.Contains(lines[2], "middle") {
		t.Errorf("second agent should be 'middle': %s", lines[2])
	}
	if !strings.Contains(lines[3], "newest") {
		t.Errorf("third agent should be 'newest': %s", lines[3])
	}
}

func TestDisplayAgentsFilteredEmpty(t *testing.T) {
	agents := []api.AgentInfo{
		{Name: "running-agent", Phase: "running", Template: "default", Runtime: "docker", Project: "p"},
	}

	filterPhase = "error"
	defer func() { filterPhase = "" }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)
	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "No active agents") {
		t.Errorf("expected empty message when filter matches nothing, got: %s", output)
	}
	if strings.Contains(output, "running-agent") {
		t.Errorf("output should NOT contain filtered-out agent: %s", output)
	}
}

func TestValidateListFlags(t *testing.T) {
	tests := []struct {
		name       string
		phase      string
		activity   string
		sort       string
		wantErr    bool
		errContain string
	}{
		{"valid phase", "running", "", "", false, ""},
		{"valid activity", "", "thinking", "", false, ""},
		{"valid sort", "", "", "name", false, ""},
		{"invalid phase", "bogus", "", "", true, "invalid phase"},
		{"invalid activity", "", "bogus", "", true, "invalid activity"},
		{"invalid sort", "", "", "bogus", true, "invalid sort field"},
		{"all empty", "", "", "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filterPhase = tt.phase
			filterActivity = tt.activity
			sortField = tt.sort
			defer func() { filterPhase = ""; filterActivity = ""; sortField = "" }()

			err := validateListFlags()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContain)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateListFlagsNegativeCount(t *testing.T) {
	oldListCount := listCount
	listCount = -5
	defer func() { listCount = oldListCount }()

	err := validateListFlags()
	if err == nil {
		t.Fatal("expected error for negative --count, got nil")
	}
	if !strings.Contains(err.Error(), "non-negative") {
		t.Errorf("error %q should mention non-negative", err.Error())
	}
}

// TestRejectHubOnlyFiltersInLocalMode covers: a Hub-only filter set while
// listing locally must error rather
// than silently listing everything (a narrowing filter that narrows nothing
// makes the output wider than asked for, with no indication anything was
// ignored).
func TestRejectHubOnlyFiltersInLocalMode(t *testing.T) {
	reset := func() {
		filterOwner, filterBroker, filterHarness = "", "", ""
		filterDescendants, filterAncestors, filterLineage = "", "", ""
	}
	defer reset()

	t.Run("no Hub-only filters set: no error", func(t *testing.T) {
		reset()
		assert.NoError(t, rejectHubOnlyFiltersInLocalMode())
	})

	tests := []struct {
		flagName string
		set      func()
	}{
		{"owner", func() { filterOwner = "alice" }},
		{"broker", func() { filterBroker = "my-broker" }},
		{"harness", func() { filterHarness = "claude" }},
		{"descendants", func() { filterDescendants = "agent-a" }},
		{"ancestors", func() { filterAncestors = "agent-a" }},
		{"lineage", func() { filterLineage = "agent-a" }},
	}
	for _, tt := range tests {
		t.Run(tt.flagName, func(t *testing.T) {
			reset()
			tt.set()
			err := rejectHubOnlyFiltersInLocalMode()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--"+tt.flagName)
			assert.Contains(t, err.Error(), "Hub mode")
		})
	}
}

// TestListCmd_Args_RejectsPositionalArguments covers: `scion list
// --descendants foo` (space, not "=") must not silently drop "foo" as an
// ignored positional argument. Because
// --descendants has NoOptDefVal, the flag consumes no value without "=", so
// "foo" would otherwise parse as a positional arg that listCmd's RunE never
// reads — the command would then run with --descendants inferring the
// caller instead of naming "foo", a silent wrong answer rather than a
// visible error.
func TestListCmd_Args_RejectsPositionalArguments(t *testing.T) {
	require.NotNil(t, listCmd.Args, "listCmd must validate positional arguments")

	err := listCmd.Args(listCmd, []string{"foo"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--descendants=")

	assert.NoError(t, listCmd.Args(listCmd, nil), "no positional args must still be accepted")
	assert.NoError(t, listCmd.Args(listCmd, []string{}), "an empty args slice must still be accepted")
}

func TestListCountFlag(t *testing.T) {
	var receivedLimit string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedLimit = r.URL.Query().Get("limit")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agents": [], "totalCount": 0}`))
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("hubclient.New failed: %v", err)
	}

	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	// Save and restore global flags
	oldListAll := listAll
	oldListCount := listCount
	oldOutputFormat := outputFormat
	listAll = true // avoid project ID lookup
	listCount = 10
	outputFormat = ""
	defer func() {
		listAll = oldListAll
		listCount = oldListCount
		outputFormat = oldOutputFormat
	}()

	// Capture stdout (displayAgents writes there)
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err = listAgentsViaHub(hubCtx)

	_ = w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	if err != nil {
		t.Fatalf("listAgentsViaHub returned error: %v", err)
	}

	if receivedLimit != "10" {
		t.Errorf("expected limit=10 in API request, got limit=%q", receivedLimit)
	}
}

func TestListTruncationWarning(t *testing.T) {
	// Server returns 2 agents but totalCount=5, indicating truncation
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"agents": []map[string]interface{}{
				{"id": "a1", "name": "agent-1", "phase": "running"},
				{"id": "a2", "name": "agent-2", "phase": "running"},
			},
			"totalCount": 5,
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("hubclient.New failed: %v", err)
	}

	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	// Save and restore global flags
	oldListAll := listAll
	oldListCount := listCount
	oldOutputFormat := outputFormat
	listAll = true
	listCount = 0
	outputFormat = ""
	defer func() {
		listAll = oldListAll
		listCount = oldListCount
		outputFormat = oldOutputFormat
	}()

	// Capture stderr for the truncation warning
	oldStderr := os.Stderr
	stderrR, stderrW, _ := os.Pipe()
	os.Stderr = stderrW

	// Capture stdout (displayAgents writes table output there)
	oldStdout := os.Stdout
	stdoutR, stdoutW, _ := os.Pipe()
	os.Stdout = stdoutW

	err = listAgentsViaHub(hubCtx)

	_ = stderrW.Close()
	_ = stdoutW.Close()
	os.Stderr = oldStderr
	os.Stdout = oldStdout

	var stderrBuf bytes.Buffer
	_, _ = stderrBuf.ReadFrom(stderrR)
	// drain stdout
	var stdoutBuf bytes.Buffer
	_, _ = stdoutBuf.ReadFrom(stdoutR)

	if err != nil {
		t.Fatalf("listAgentsViaHub returned error: %v", err)
	}

	stderrOutput := stderrBuf.String()
	expectedWarning := fmt.Sprintf("Warning: showing %d of %d agents. Use --count %d to see all.", 2, 5, 5)
	if !strings.Contains(stderrOutput, expectedWarning) {
		t.Errorf("expected truncation warning %q in stderr, got: %q", expectedWarning, stderrOutput)
	}
}

func TestListJSONAlwaysBareArray(t *testing.T) {
	agents := []api.AgentInfo{
		{Name: "agent-1", Phase: "running", Template: "default", Runtime: "docker", Project: "p", ProjectID: "p-id", ProjectPath: "/p/path"},
		{Name: "agent-2", Phase: "running", Template: "default", Runtime: "docker", Project: "p", ProjectID: "p-id", ProjectPath: "/p/path"},
	}

	// Save and restore global flags
	oldOutputFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldOutputFormat }()

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := displayAgents(agents, false, false)

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("displayAgents returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	// JSON output must always be a bare array, never an envelope
	output := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(output, "[") {
		t.Errorf("expected bare JSON array, got: %s", output)
	}

	var arr []api.AgentInfo
	if err := json.Unmarshal(buf.Bytes(), &arr); err != nil {
		t.Fatalf("failed to decode bare JSON array: %v\noutput: %s", err, buf.String())
	}
	if len(arr) != 2 {
		t.Errorf("expected 2 agents in array, got %d", len(arr))
	}

	// The output must carry the canonical project fields and must not carry
	// any legacy grove key.
	var raw []map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("failed to decode raw JSON array: %v\noutput: %s", err, buf.String())
	}
	for i, entry := range raw {
		if entry["project"] != "p" {
			t.Errorf("entry %d: project = %v, want %q", i, entry["project"], "p")
		}
		if entry["projectId"] != "p-id" {
			t.Errorf("entry %d: projectId = %v, want %q", i, entry["projectId"], "p-id")
		}
		if entry["projectPath"] != "/p/path" {
			t.Errorf("entry %d: projectPath = %v, want %q", i, entry["projectPath"], "/p/path")
		}
		for _, legacyKey := range []string{"grove", "groveId", "grovePath"} {
			if _, ok := entry[legacyKey]; ok {
				t.Errorf("entry %d: legacy key %q present in JSON output, want absent: %v", i, legacyKey, entry[legacyKey])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// ptone/scion#2146: --owner/--broker/--harness attribute filters and the
// --descendants/--ancestors relationship filters.
// ---------------------------------------------------------------------------

func TestListCmd_RelationshipFlagsNoOptDefVal(t *testing.T) {
	for _, name := range []string{"descendants", "ancestors", "lineage"} {
		f := listCmd.Flags().Lookup(name)
		require.NotNilf(t, f, "list command should have a --%s flag", name)
		assert.Equalf(t, scopeInferSentinel, f.NoOptDefVal,
			"--%s should have NoOptDefVal set to the sentinel so bare usage works", name)
	}
}

func TestListCmd_RelationshipFlagsMutuallyExclusive(t *testing.T) {
	tests := [][2]string{
		{"descendants", "ancestors"},
		{"descendants", "lineage"},
		{"ancestors", "lineage"},
	}
	for _, pair := range tests {
		t.Run(pair[0]+"+"+pair[1], func(t *testing.T) {
			require.NoError(t, listCmd.Flags().Set(pair[0], "agent-a"))
			require.NoError(t, listCmd.Flags().Set(pair[1], "agent-b"))
			defer func() {
				for _, name := range []string{"descendants", "ancestors", "lineage"} {
					_ = listCmd.Flags().Set(name, "")
					listCmd.Flags().Lookup(name).Changed = false
				}
			}()

			err := listCmd.ValidateFlagGroups()
			require.Errorf(t, err, "--%s and --%s together must be rejected", pair[0], pair[1])
			assert.Contains(t, err.Error(), pair[0])
			assert.Contains(t, err.Error(), pair[1])
		})
	}
}

func TestResolveRelationshipReference(t *testing.T) {
	const meID = "99999999-9999-9999-9999-999999999999"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: meID, Email: "me@example.com"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	tests := []struct {
		name        string
		flagValue   string
		cliMode     string
		agentID     string
		wantAgentID string
		wantUserID  string
		wantErr     string
	}{
		{
			name:        "explicit value is always an agent reference, regardless of mode",
			flagValue:   "some-agent",
			cliMode:     "",
			wantAgentID: "some-agent",
		},
		{
			name:        "explicit value in agent mode is still an agent reference",
			flagValue:   "some-agent",
			cliMode:     "agent",
			agentID:     "self-id",
			wantAgentID: "some-agent",
		},
		{
			name:        "bare flag in agent mode resolves to the calling agent via SCION_AGENT_ID",
			flagValue:   scopeInferSentinel,
			cliMode:     "agent",
			agentID:     "agent-self-id",
			wantAgentID: "agent-self-id",
		},
		{
			name:      "bare flag in agent mode with no SCION_AGENT_ID errors",
			flagValue: scopeInferSentinel,
			cliMode:   "agent",
			agentID:   "",
			wantErr:   "SCION_AGENT_ID is not set",
		},
		{
			name:       "bare flag in human mode resolves to the calling user",
			flagValue:  scopeInferSentinel,
			cliMode:    "",
			wantUserID: meID,
		},
		{
			name:       "bare flag in assistant mode resolves to the calling user",
			flagValue:  scopeInferSentinel,
			cliMode:    "assistant",
			wantUserID: meID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SCION_CLI_MODE", tt.cliMode)
			t.Setenv("SCION_AGENT_ID", tt.agentID)

			agentRef, userID, err := resolveRelationshipReference(context.Background(), client, tt.flagValue)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantAgentID, agentRef)
			assert.Equal(t, tt.wantUserID, userID)
		})
	}
}

// TestResolveLineageRootID covers ptone/scion#2146: a length-1 Ancestry
// entry is not always a user. It can also be another AGENT's ID, if that
// agent's own Ancestry was itself empty when it created the reference (see
// resolveLineageRootID's doc for the three ways that happens). Only
// resolving what the entry actually names — through the same authorized-list
// mechanism as --descendants/--ancestors, never a bare per-ID fetch — can
// tell the two apart, so this test runs against a real stub Hub rather than
// a purely local table.
func TestResolveLineageRootID(t *testing.T) {
	const ancestryLessCreatorID = "11111111-1111-1111-1111-111111111111"
	// A queried ID that gets back an agent whose ID does NOT match (e.g. a
	// pre-#2146 Hub that ignores `id` and returns an arbitrary agent).
	const mismatchedIDEntry = "22222222-2222-2222-2222-222222222222"
	const unexpectedAgentID = "33333333-3333-3333-3333-333333333333"
	// A queried ID that gets back two elements both claiming that exact ID
	// — should never happen against a real Hub (IDs is a single-element
	// set), but the code must fail loud rather than guess.
	const duplicateMatchEntry = "44444444-4444-4444-4444-444444444444"

	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.URL.Path != "/api/v1/agents" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ids := r.URL.Query()["id"]
		switch {
		case len(ids) == 1 && ids[0] == ancestryLessCreatorID:
			// ancestryLessCreatorID names a real, visible agent — the edge
			// case where a length-1 Ancestry entry is an AGENT, not a user,
			// because its own Ancestry was itself empty when it created the
			// reference below.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{{ID: ancestryLessCreatorID, Slug: "ancestry-less-creator"}},
			})
		case len(ids) == 1 && ids[0] == mismatchedIDEntry:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{{ID: unexpectedAgentID, Slug: "unexpected-agent"}},
			})
		case len(ids) == 1 && ids[0] == duplicateMatchEntry:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{
					{ID: duplicateMatchEntry, Slug: "dup-1"},
					{ID: duplicateMatchEntry, Slug: "dup-2"},
				},
			})
		default:
			// Any other queried ID (e.g. a plain user ID) names no agent.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	agentSvc := client.Agents()

	tests := []struct {
		name        string
		id          string
		ancestry    []string
		want        string
		wantNoCalls bool // len 0 and len>=2 must make zero requests
	}{
		{
			name:        "no ancestry: self is root (covers a user reference, which has no Ancestry at all)",
			id:          "self-id",
			want:        "self-id",
			wantNoCalls: true,
		},
		{
			name:     "single ancestry entry naming a user (the common top-level-agent case): parent is a user, so self is root, not the user (ptone/scion#2146 --lineage option (i))",
			id:       "child-id",
			ancestry: []string{"a-user-id"},
			want:     "child-id",
		},
		{
			name:     "single ancestry entry naming an AGENT: root at that agent, not self",
			id:       "child-of-ancestry-less-creator",
			ancestry: []string{ancestryLessCreatorID},
			want:     ancestryLessCreatorID,
		},
		{
			name:        "multi-entry ancestry: the LAST entry is the direct parent, not the topmost ancestor — no lookup needed, since it's guaranteed to be an agent ID by construction",
			id:          "grandchild-id",
			ancestry:    []string{"user-id", "parent-id", "immediate-parent-id"},
			want:        "immediate-parent-id",
			wantNoCalls: true,
		},
		{
			// Guards the exact-ID-match hardening in isAncestryEntryAnAgent,
			// tested directly. Mutation-verified: replacing the ID-equality
			// check with `true` made this case return unexpectedAgentID
			// instead of self, failing.
			name:     "single ancestry entry whose lookup returns a DIFFERENT ID: must not trust it — root at self, not the mismatched agent",
			id:       "child-with-mismatched-lookup",
			ancestry: []string{mismatchedIDEntry},
			want:     "child-with-mismatched-lookup",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCount = 0
			got, err := resolveLineageRootID(context.Background(), agentSvc, tt.id, tt.ancestry)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			if tt.wantNoCalls {
				assert.Zero(t, requestCount, "this ancestry length must resolve locally, with no Hub call at all")
			} else {
				assert.Equal(t, 1, requestCount, "a length-1 ancestry must resolve via exactly one authorized-list call")
			}
		})
	}

	// Guards the >1-exact-match branch, tested directly: removing the
	// `found != nil` check makes this case fail. (It does not distinguish
	// the ID-equality mutant; the mismatched-ID case above does.)
	t.Run("single ancestry entry whose lookup returns TWO exact-ID matches: must fail loud, not guess", func(t *testing.T) {
		requestCount = 0
		_, err := resolveLineageRootID(context.Background(), agentSvc, "child-id", []string{duplicateMatchEntry})
		require.Error(t, err)
	})
}

// TestResolveLineageRootID_LookupError covers: a length-1 ancestry's
// authorized-list lookup can fail (a real HTTP/network error, not just a
// zero-result "not an agent" outcome), and that
// must propagate as a wrapped error, not silently root at self — rooting at
// self on an unknown outcome would be indistinguishable from a legitimate
// "it's a user" result, hiding a real failure from the caller.
func TestResolveLineageRootID_LookupError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	agentSvc := client.Agents()

	_, err = resolveLineageRootID(context.Background(), agentSvc, "child-id", []string{"some-ancestor-id"})
	require.Error(t, err)
}

func TestResolveOwnerID(t *testing.T) {
	const meID = "11111111-1111-1111-1111-111111111111"
	const directID = "22222222-2222-2222-2222-222222222222"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: meID, Email: "me@example.com"})
		case "/api/v1/users/" + directID:
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: directID, Email: "direct@example.com"})
		case "/api/v1/users/by-name", "/api/v1/users/alice", "/api/v1/users/ambiguous", "/api/v1/users/nobody":
			// These are name/email lookups mis-tried as direct IDs — 404 so
			// resolveOwnerID falls back to the search branch below.
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		case "/api/v1/users":
			search := r.URL.Query().Get("search")
			var users []hubclient.User
			switch search {
			case "alice":
				users = []hubclient.User{{ID: "alice-id", Email: "alice@example.com", DisplayName: "Alice"}}
			case "ambiguous":
				users = []hubclient.User{
					{ID: "amb-1", Email: "a1@example.com", DisplayName: "ambiguous"},
					{ID: "amb-2", Email: "a2@example.com", DisplayName: "ambiguous"},
				}
			case "nobody":
				users = nil
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"users": users})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	t.Run("me resolves via auth/me", func(t *testing.T) {
		id, err := resolveOwnerID(context.Background(), client, "me")
		require.NoError(t, err)
		assert.Equal(t, meID, id)
	})

	t.Run("direct ID resolves via Users().Get", func(t *testing.T) {
		id, err := resolveOwnerID(context.Background(), client, directID)
		require.NoError(t, err)
		assert.Equal(t, directID, id)
	})

	t.Run("name/email falls back to search - single match", func(t *testing.T) {
		id, err := resolveOwnerID(context.Background(), client, "alice")
		require.NoError(t, err)
		assert.Equal(t, "alice-id", id)
	})

	t.Run("name/email matching multiple users errors", func(t *testing.T) {
		_, err := resolveOwnerID(context.Background(), client, "ambiguous")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple users")
	})

	t.Run("no matching user errors", func(t *testing.T) {
		_, err := resolveOwnerID(context.Background(), client, "nobody")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no matching user")
	})
}

func TestResolveReferenceAgent(t *testing.T) {
	const directID = "33333333-3333-3333-3333-333333333333"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agents/"+directID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: directID, Slug: "direct-agent", Name: "direct-agent"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/agents/") && r.URL.Path != "/api/v1/agents/":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		case r.URL.Path == "/api/v1/agents":
			agents := []hubclient.Agent{
				{ID: "by-slug-id", Slug: "worker", Name: "Worker Display Name"},
				{ID: "ambiguous-1", Slug: "dup-1", Name: "duplicate"},
				{ID: "ambiguous-2", Slug: "dup-2", Name: "duplicate"},
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": agents})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	agentSvc := client.Agents()

	t.Run("resolves directly by ID", func(t *testing.T) {
		a, err := resolveReferenceAgent(context.Background(), agentSvc, directID)
		require.NoError(t, err)
		assert.Equal(t, directID, a.ID)
	})

	t.Run("falls back to slug match", func(t *testing.T) {
		a, err := resolveReferenceAgent(context.Background(), agentSvc, "worker")
		require.NoError(t, err)
		assert.Equal(t, "by-slug-id", a.ID)
	})

	t.Run("falls back to name match", func(t *testing.T) {
		a, err := resolveReferenceAgent(context.Background(), agentSvc, "Worker Display Name")
		require.NoError(t, err)
		assert.Equal(t, "by-slug-id", a.ID)
	})

	t.Run("ambiguous name/slug errors", func(t *testing.T) {
		_, err := resolveReferenceAgent(context.Background(), agentSvc, "duplicate")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple agents")
	})

	t.Run("no match errors", func(t *testing.T) {
		_, err := resolveReferenceAgent(context.Background(), agentSvc, "does-not-exist")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// TestResolveReferenceAgent_FallsBackOn403 covers: many agent identities are
// denied a single-resource GET on any agent other than themselves with a
// plain 403 (verified against a real Hub in
// TestListAgents_ListEndpointResolvesPeerWhenGetIsForbidden,
// pkg/hub/rs2_r1_fixes_test.go), even though the identical agent is visible
// through the authorized list endpoint. Falling through only on 404 would
// make --descendants=<peer> fail outright for exactly the audience (agents
// naming a sibling) it is built for. This test proves the CLIENT-side
// fallback: given a GET that returns 403, resolution must still succeed via
// the list endpoint.
func TestResolveReferenceAgent_FallsBackOn403(t *testing.T) {
	const peerID = "66666666-6666-6666-6666-666666666666"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + peerID:
			// A single-resource GET on a peer is forbidden for this identity
			// (agent.read has no AgentScopes mapping — see
			// TestBypassAgents_LegitimateFlowsStillWork), even though the
			// agent genuinely exists and is listable.
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
		case "/api/v1/agents":
			// The list endpoint, by contrast, is authorized and includes the
			// peer — whether narrowed by id[] or returned in a bare page.
			ids := r.URL.Query()["id"]
			if len(ids) > 0 {
				assert.Equal(t, []string{peerID}, ids)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{{ID: peerID, Slug: "peer-agent", Name: "peer-agent"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	a, err := resolveReferenceAgent(context.Background(), client.Agents(), peerID)
	require.NoError(t, err, "a 403 on GET must fall through to list-based resolution, not fail outright")
	assert.Equal(t, peerID, a.ID)
}

// TestResolveReferenceAgent_UUIDNarrowsViaIDsFilter verifies that once GET
// fails (404 or 403), a UUID-shaped reference is resolved via the id[]
// filter — a single narrowing query — rather than paging through every
// agent to find a name/slug match that could never occur for a UUID input.
func TestResolveReferenceAgent_UUIDNarrowsViaIDsFilter(t *testing.T) {
	const refID = "77777777-7777-7777-7777-777777777777"
	var bareListCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + refID:
			w.WriteHeader(http.StatusNotFound)
		case "/api/v1/agents":
			ids := r.URL.Query()["id"]
			if len(ids) == 0 {
				bareListCalled = true
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
				return
			}
			assert.Equal(t, []string{refID}, ids)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{{ID: refID, Slug: "ref-agent", Name: "ref-agent"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	a, err := resolveReferenceAgent(context.Background(), client.Agents(), refID)
	require.NoError(t, err)
	assert.Equal(t, refID, a.ID)
	assert.False(t, bareListCalled, "a UUID reference must resolve via id[], not a full-list page scan")
}

// TestResolveReferenceAgent_UUIDMismatchedIDNotFound covers, tested
// directly: a Hub that ignores the `id` query param (a pre-ptone/scion#2146
// Hub) and hands back an arbitrary agent must not be trusted — the response
// element's ID must actually equal ref.
// Mutation-verified: replacing `resp.Agents[i].ID == ref` with `true` makes
// this case pass instead of erroring (it would wrongly resolve to the
// mismatched agent), so it fails as intended against that mutant.
func TestResolveReferenceAgent_UUIDMismatchedIDNotFound(t *testing.T) {
	const ref = "77777777-7777-7777-7777-777777777777"
	const unexpectedAgentID = "88888888-8888-8888-8888-888888888888"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + ref:
			w.WriteHeader(http.StatusNotFound)
		case "/api/v1/agents":
			assert.Equal(t, []string{ref}, r.URL.Query()["id"])
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{{ID: unexpectedAgentID, Slug: "unexpected-agent"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	_, err = resolveReferenceAgent(context.Background(), client.Agents(), ref)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestResolveReferenceAgent_UUIDDuplicateMatchErrors covers: two response
// elements both claiming ID == ref should never happen against a real Hub
// (IDs is a single-element set), but the function must fail loud rather
// than guess which one.
func TestResolveReferenceAgent_UUIDDuplicateMatchErrors(t *testing.T) {
	const ref = "99999999-9999-9999-9999-999999999999"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + ref:
			w.WriteHeader(http.StatusNotFound)
		case "/api/v1/agents":
			assert.Equal(t, []string{ref}, r.URL.Query()["id"])
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{
					{ID: ref, Slug: "dup-1"},
					{ID: ref, Slug: "dup-2"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	_, err = resolveReferenceAgent(context.Background(), client.Agents(), ref)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than one record")
}

// TestResolveReferenceAgent_PagesThroughNameMatches covers: a name/slug
// match must not be missed just because it falls on a later page of the
// authorized list.
func TestResolveReferenceAgent_PagesThroughNameMatches(t *testing.T) {
	const targetID = "88888888-8888-8888-8888-888888888888"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/target-name":
			w.WriteHeader(http.StatusNotFound)
		case "/api/v1/agents":
			if r.URL.Query().Get("cursor") == "" {
				// First page: no match, but says there's more.
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"agents":     []hubclient.Agent{{ID: "other-1", Slug: "other-1", Name: "other-1"}},
					"nextCursor": "page-2",
				})
				return
			}
			// Second page: the actual match, no further cursor.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{{ID: targetID, Slug: "target-name", Name: "target-name"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	a, err := resolveReferenceAgent(context.Background(), client.Agents(), "target-name")
	require.NoError(t, err, "a match on the second page must not be missed")
	assert.Equal(t, targetID, a.ID)
}

// TestListAgentsViaHub_AttributeFilterQueryParams is an end-to-end wiring
// check: --owner/--broker/--harness resolve and land on the outgoing
// /api/v1/agents request as ownerId/runtimeBrokerId/harnessConfig, combined
// with the existing --phase/--label filters (all via AND, per ptone/scion#2146).
func TestListAgentsViaHub_AttributeFilterQueryParams(t *testing.T) {
	const ownerID = "44444444-4444-4444-4444-444444444444"
	const brokerID = "55555555-5555-5555-5555-555555555555"

	var gotQuery map[string][]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/users/" + ownerID:
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: ownerID})
		case "/api/v1/runtime-brokers/" + brokerID:
			_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{ID: brokerID, Name: "broker-x"})
		case "/api/v1/agents":
			gotQuery = map[string][]string(r.URL.Query())
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	oldListAll, oldOwner, oldBroker, oldHarness, oldPhase, oldOutputFormat :=
		listAll, filterOwner, filterBroker, filterHarness, filterPhase, outputFormat
	listAll = true // avoid project ID lookup
	filterOwner = ownerID
	filterBroker = brokerID
	filterHarness = "claude"
	filterPhase = "running"
	outputFormat = "json"
	defer func() {
		listAll, filterOwner, filterBroker, filterHarness, filterPhase, outputFormat =
			oldListAll, oldOwner, oldBroker, oldHarness, oldPhase, oldOutputFormat
	}()

	captureListStdout(func() {
		err = listAgentsViaHub(hubCtx)
	})

	require.NoError(t, err)
	require.NotNil(t, gotQuery, "the /api/v1/agents request should have been made")
	assert.Equal(t, ownerID, gotQuery["ownerId"][0])
	assert.Equal(t, brokerID, gotQuery["runtimeBrokerId"][0])
	assert.Equal(t, "claude", gotQuery["harnessConfig"][0])
	assert.Equal(t, "running", gotQuery["phase"][0])
}

// TestListAgentsViaHub_DescendantsFlag verifies --descendants=<agent> resolves
// the reference agent and sends its ID as ancestorId on the outgoing request.
func TestListAgentsViaHub_DescendantsFlag(t *testing.T) {
	const refID = "66666666-6666-6666-6666-666666666666"

	var gotAncestorID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent", Name: "ref-agent"})
		case "/api/v1/agents":
			gotAncestorID = r.URL.Query().Get("ancestorId")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	oldListAll, oldDescendants, oldOutputFormat := listAll, filterDescendants, outputFormat
	listAll = true
	filterDescendants = refID
	outputFormat = "json"
	// Explicit, not ambient: the --all/agent-mode/relationship-flag guard
	// reads SCION_CLI_MODE, and this test only cares about
	// resolution mechanics, not mode. Do not rely on the ambient
	// environment defaulting to human mode — inside an agent container it
	// does not.
	t.Setenv("SCION_CLI_MODE", "human")
	defer func() {
		listAll, filterDescendants, outputFormat = oldListAll, oldDescendants, oldOutputFormat
	}()

	captureListStdout(func() {
		err = listAgentsViaHub(hubCtx)
	})

	require.NoError(t, err)
	assert.Equal(t, refID, gotAncestorID)
}

// TestListAgentsViaHub_AncestorsFlag verifies --ancestors=<agent> resolves the
// reference agent's Ancestry chain and sends it as the id[] relationship
// filter, and that an agent with an empty Ancestry short-circuits to zero
// results without sending an ambiguous empty id[] query.
func TestListAgentsViaHub_AncestorsFlag(t *testing.T) {
	const refID = "77777777-7777-7777-7777-777777777777"
	const emptyRefID = "88888888-8888-8888-8888-888888888888"

	var gotIDs []string
	var agentsCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent", Ancestry: []string{"anc-1", "anc-2"}})
		case "/api/v1/agents/" + emptyRefID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: emptyRefID, Slug: "empty-ref-agent"})
		case "/api/v1/agents":
			agentsCalled = true
			gotIDs = r.URL.Query()["id"]
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	oldListAll, oldAncestors, oldOutputFormat := listAll, filterAncestors, outputFormat
	listAll = true
	outputFormat = "json"
	// See TestListAgentsViaHub_DescendantsFlag: explicit, not ambient.
	t.Setenv("SCION_CLI_MODE", "human")
	defer func() {
		listAll, filterAncestors, outputFormat = oldListAll, oldAncestors, oldOutputFormat
	}()

	t.Run("non-empty ancestry is sent as id[] filter", func(t *testing.T) {
		agentsCalled, gotIDs = false, nil
		filterAncestors = refID

		var err error
		captureListStdout(func() {
			err = listAgentsViaHub(hubCtx)
		})

		require.NoError(t, err)
		assert.True(t, agentsCalled)
		assert.ElementsMatch(t, []string{"anc-1", "anc-2"}, gotIDs)
	})

	t.Run("empty ancestry short-circuits without querying agents", func(t *testing.T) {
		agentsCalled, gotIDs = false, nil
		filterAncestors = emptyRefID

		var err error
		captureListStdout(func() {
			err = listAgentsViaHub(hubCtx)
		})

		require.NoError(t, err)
		assert.False(t, agentsCalled, "an empty ancestry must not fall through to an unrestricted /api/v1/agents query")
	})
}

// TestListAgentsViaHub_LineageFlag verifies --lineage=<agent> resolves the
// reference agent's direct parent (the last Ancestry entry) and sends it as
// lineageRootId — and that an ancestry-less reference uses itself as the
// root.
func TestListAgentsViaHub_LineageFlag(t *testing.T) {
	const refID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const refProjectID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	const rootlessRefID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const topLevelRefID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const topLevelRefProjectID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	// A length-1 ancestry whose entry resolves to a visible agent, through
	// the full listAgentsViaHub path.
	const agentParentRefID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	const agentParentRefProjectID = "11111111-2222-3333-4444-555555555555"
	const agentParentID = "66666666-7777-8888-9999-aaaaaaaaaaaa"

	var gotLineageRootID, gotProjectID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agents/"+refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent", ProjectID: refProjectID, Ancestry: []string{"user-id", "parent-id"}})
		case r.URL.Path == "/api/v1/agents/"+rootlessRefID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: rootlessRefID, Slug: "rootless-ref-agent"})
		case r.URL.Path == "/api/v1/agents/"+topLevelRefID:
			// A top-level, user-created agent: Ancestry has exactly one
			// entry (the user that created it), so its direct parent is a
			// user, not an agent.
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: topLevelRefID, Slug: "top-level-ref-agent", ProjectID: topLevelRefProjectID, Ancestry: []string{"user-id"}})
		case r.URL.Path == "/api/v1/agents/"+agentParentRefID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: agentParentRefID, Slug: "agent-parent-ref", ProjectID: agentParentRefProjectID, Ancestry: []string{agentParentID}})
		case r.URL.Path == "/api/v1/agents" && r.URL.Query().Get("id") == agentParentID:
			// The isAncestryEntryAnAgent resolution call for the length-1
			// entry above — distinct from the final listing call below,
			// which never sets `id`.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{{ID: agentParentID, Slug: "agent-parent"}},
			})
		case r.URL.Path == "/api/v1/agents":
			gotLineageRootID = r.URL.Query().Get("lineageRootId")
			gotProjectID = r.URL.Query().Get("projectId")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	oldListAll, oldLineage, oldOutputFormat := listAll, filterLineage, outputFormat
	listAll = true
	outputFormat = "json"
	// See TestListAgentsViaHub_DescendantsFlag: explicit, not ambient.
	t.Setenv("SCION_CLI_MODE", "human")
	defer func() {
		listAll, filterLineage, outputFormat = oldListAll, oldLineage, oldOutputFormat
	}()

	run := func() {
		t.Helper()
		var err error
		captureListStdout(func() {
			err = listAgentsViaHub(hubCtx)
		})
		require.NoError(t, err)
	}

	t.Run("root is the direct parent (last ancestry entry), not the topmost ancestor", func(t *testing.T) {
		gotLineageRootID, gotProjectID = "", ""
		filterLineage = refID
		run()
		assert.Equal(t, "parent-id", gotLineageRootID)
		// Project-bounding matters most in exactly this agent-parent case
		// (the top-level-agent subtest below is not the only path that must
		// bound to the reference's project).
		assert.Equal(t, refProjectID, gotProjectID)
	})

	t.Run("ancestry-less reference is its own root", func(t *testing.T) {
		gotLineageRootID = ""
		filterLineage = rootlessRefID
		run()
		assert.Equal(t, rootlessRefID, gotLineageRootID)
	})

	// --lineage explicitly roots at the reference itself when its direct
	// parent is a user (a top-level agent) rather than another agent — this
	// is the CLI-level case (through the full listAgentsViaHub path, not
	// just the resolveLineageRootID unit test above) that exercises exactly
	// that, and also confirms the query is bounded to the reference's own
	// project.
	t.Run("top-level agent (direct parent is a user) is its own root, project-bounded", func(t *testing.T) {
		gotLineageRootID, gotProjectID = "", ""
		filterLineage = topLevelRefID
		run()
		assert.Equal(t, topLevelRefID, gotLineageRootID)
		assert.Equal(t, topLevelRefProjectID, gotProjectID)
	})

	// A length-1 ancestry whose entry resolves to a visible agent, driven
	// through the full listAgentsViaHub path, not just the
	// resolveLineageRootID unit test.
	t.Run("length-1 ancestry entry resolving to a visible agent roots there, project-bounded", func(t *testing.T) {
		gotLineageRootID, gotProjectID = "", ""
		filterLineage = agentParentRefID
		run()
		assert.Equal(t, agentParentID, gotLineageRootID)
		assert.Equal(t, agentParentRefProjectID, gotProjectID)
	})
}

// TestListAgentsViaHub_AllMode_AgentIdentityWithRelationshipFlag_Errors: an
// agent TOKEN plus --all plus any relationship flag must fail loudly, not
// silently print an empty list. An agent identity has no hub-wide list
// authority at all — the real Hub proves this directly
// (TestListAgents_ListEndpointResolvesPeerWhenGetIsForbidden,
// pkg/hub/rs2_r1_fixes_test.go: the global endpoint returns nothing for a
// bare agent token, even for id=<self>) — so the final --all listing could
// never succeed for an agent-token caller regardless of how the reference
// itself is resolved. The guard fires before any HTTP call at all (there is
// nothing to fake here: the test server would fail the test if it received
// any request), which is the point — no reference resolution, no listing,
// no silent wrong answer.
//
// The guard keys on hubCtx.CredentialKind == CredentialKindAgentToken, not
// on CLI mode — SCION_CLI_MODE is still set to "agent" here because that's
// the realistic pairing (an actual agent container normally does
// authenticate with its agent token), but it is CredentialKind, set
// explicitly below, that the guard actually reads. See
// TestListAgentsViaHub_AllMode_AgentModeWithOAuthCredential_NotBlocked for
// the complementary case: the same agent-mode setup, but a non-agent-token
// credential, which must NOT be blocked.
func TestListAgentsViaHub_AllMode_AgentIdentityWithRelationshipFlag_Errors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP request to %s — the --all/agent-token/relationship-flag guard must fire before any network call", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "some-project-id", CredentialKind: hubsync.CredentialKindAgentToken}

	oldListAll := listAll
	oldDescendants, oldAncestors, oldLineage := filterDescendants, filterAncestors, filterLineage
	listAll = true
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_AGENT_ID", "some-agent-id")
	defer func() {
		listAll = oldListAll
		filterDescendants, filterAncestors, filterLineage = oldDescendants, oldAncestors, oldLineage
	}()

	for _, tt := range []struct {
		name string
		set  func()
	}{
		{"descendants (bare)", func() { filterDescendants = scopeInferSentinel }},
		{"ancestors (bare)", func() { filterAncestors = scopeInferSentinel }},
		{"lineage (bare)", func() { filterLineage = scopeInferSentinel }},
		// The guard must also fire for an explicit reference value, not only
		// the bare (self-inferring) form — the final --all listing is what
		// fails regardless of how the reference was named.
		{"descendants (explicit value)", func() { filterDescendants = "some-other-agent" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			filterDescendants, filterAncestors, filterLineage = "", "", ""
			tt.set()

			err := listAgentsViaHub(hubCtx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--all")
			assert.Contains(t, err.Error(), "agent")
		})
	}
}

// TestListAgentsViaHub_AllMode_AgentModeWithOAuthCredential_NotBlocked
// proves: the SAME SCION_CLI_MODE=agent setup as
// TestListAgentsViaHub_AllMode_AgentIdentityWithRelationshipFlag_Errors, but
// a HubContext whose CredentialKind is OAuth (a human authenticated via
// `scion hub auth login`, running inside an agent container) rather than an
// agent token, is not blocked. The guard keys on the credential, not the CLI
// mode. Dev auth is covered by the sibling test right below — both are
// real, named CredentialKind values a caller might have in agent mode, not
// just "not an agent token" in the abstract.
func TestListAgentsViaHub_AllMode_AgentModeWithOAuthCredential_NotBlocked(t *testing.T) {
	const refID = "88888888-9999-aaaa-bbbb-cccccccccccc"
	var listCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent"})
		case "/api/v1/agents":
			listCalled = true
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, CredentialKind: hubsync.CredentialKindOAuth}

	oldListAll, oldDescendants, oldOutputFormat := listAll, filterDescendants, outputFormat
	listAll = true
	filterDescendants = refID
	outputFormat = "json"
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_AGENT_ID", "some-agent-id")
	defer func() {
		listAll, filterDescendants, outputFormat = oldListAll, oldDescendants, oldOutputFormat
	}()

	captureListStdout(func() {
		err = listAgentsViaHub(hubCtx)
	})

	require.NoError(t, err, "an OAuth credential in agent mode must not trip the agent-token --all guard")
	assert.True(t, listCalled)
}

// TestListAgentsViaHub_AllMode_AgentModeWithDevAuthCredential_NotBlocked is
// TestListAgentsViaHub_AllMode_AgentModeWithOAuthCredential_NotBlocked's
// sibling for the other non-agent-token credential: dev auth on a localhost
// Hub for a non-hub-managed agent.
func TestListAgentsViaHub_AllMode_AgentModeWithDevAuthCredential_NotBlocked(t *testing.T) {
	const refID = "99999999-aaaa-bbbb-cccc-dddddddddddd"
	var listCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent"})
		case "/api/v1/agents":
			listCalled = true
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, CredentialKind: hubsync.CredentialKindDevAuto}

	oldListAll, oldDescendants, oldOutputFormat := listAll, filterDescendants, outputFormat
	listAll = true
	filterDescendants = refID
	outputFormat = "json"
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_AGENT_ID", "some-agent-id")
	defer func() {
		listAll, filterDescendants, outputFormat = oldListAll, oldDescendants, oldOutputFormat
	}()

	captureListStdout(func() {
		err = listAgentsViaHub(hubCtx)
	})

	require.NoError(t, err, "a dev-auth credential in agent mode must not trip the agent-token --all guard")
	assert.True(t, listCalled)
}

// TestListAgentsViaHub_AllMode_AssistantModeWithRelationshipFlag_NotBlocked
// covers: the --all guard must NOT fire for a HubContext with no agent-token
// CredentialKind set (this test's HubContext leaves it at its zero value,
// hubsync.CredentialKindUnknown). SCION_CLI_MODE=assistant is still set here
// for realism, although the guard does not read it. Human mode is
// covered by TestListAgentsViaHub_AllMode_HumanCrossProjectReference;
// agent mode with a non-agent-token credential is covered by
// TestListAgentsViaHub_AllMode_AgentModeWithOAuthCredential_NotBlocked and
// its dev-auth sibling, immediately above.
func TestListAgentsViaHub_AllMode_AssistantModeWithRelationshipFlag_NotBlocked(t *testing.T) {
	const refID = "77777777-8888-9999-aaaa-bbbbbbbbbbbb"
	var listCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent"})
		case "/api/v1/agents":
			listCalled = true
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	oldListAll, oldDescendants, oldOutputFormat := listAll, filterDescendants, outputFormat
	listAll = true
	filterDescendants = refID
	outputFormat = "json"
	t.Setenv("SCION_CLI_MODE", "assistant")
	defer func() {
		listAll, filterDescendants, outputFormat = oldListAll, oldDescendants, oldOutputFormat
	}()

	captureListStdout(func() {
		err = listAgentsViaHub(hubCtx)
	})

	require.NoError(t, err, "assistant mode must not trip the agent-mode --all guard")
	assert.True(t, listCalled)
}

// TestListAgentsViaHub_AllMode_HumanCrossProjectReference covers: under
// --all, a HUMAN caller must still be able to name a reference agent in a
// *different* project than the one linked in the current directory.
// Reference resolution for a human/assistant caller under --all goes
// through the same global endpoint the final listing uses, never the
// current directory's project.
func TestListAgentsViaHub_AllMode_HumanCrossProjectReference(t *testing.T) {
	const currentProjectID = "11111111-2222-3333-4444-555555555555"
	const crossProjectRefID = "66666666-7777-8888-9999-000000000000"

	var gotAncestorID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agents/"+crossProjectRefID:
			// The reference agent lives in a different project than
			// currentProjectID, but the global endpoint can still resolve
			// it directly by ID for a human caller.
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: crossProjectRefID, Slug: "cross-project-agent"})
		case r.URL.Path == "/api/v1/agents":
			gotAncestorID = r.URL.Query().Get("ancestorId")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
			t.Errorf("human caller under --all must not be routed through the project-scoped endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	// ProjectID set to a DIFFERENT project than the reference agent lives
	// in, so a test that incorrectly scopes resolution to "the current
	// project" would fail to resolve crossProjectRefID at all.
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: currentProjectID}

	oldListAll, oldDescendants, oldOutputFormat := listAll, filterDescendants, outputFormat
	listAll = true
	filterDescendants = crossProjectRefID
	outputFormat = "json"
	t.Setenv("SCION_CLI_MODE", "human")
	defer func() {
		listAll, filterDescendants, outputFormat = oldListAll, oldDescendants, oldOutputFormat
	}()

	captureListStdout(func() {
		err = listAgentsViaHub(hubCtx)
	})

	require.NoError(t, err)
	assert.Equal(t, crossProjectRefID, gotAncestorID,
		"a human caller under --all must resolve a cross-project reference via the global endpoint")
}

// TestListAgentsViaHub_BareRelationshipFlag_ModeDefaults covers
// ptone/scion#2146 Q2: a bare relationship flag is never an error. In agent
// mode it resolves to the calling agent (unchanged); in human/assistant mode
// it resolves to the calling user, with --ancestors correctly reporting the
// user-has-no-ancestry case as an empty list rather than an error.
func TestListAgentsViaHub_BareRelationshipFlag_ModeDefaults(t *testing.T) {
	const callingUserID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const callingAgentID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	const agentProjectID = "22222222-3333-4444-5555-666666666666"

	var gotAncestorID, gotLineageRootID string
	var agentsCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: callingUserID, Email: "me@example.com"})
		case "/api/v1/agents/" + callingAgentID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: callingAgentID, Slug: "self", Ancestry: []string{"user-id", "parent-id"}})
		case "/api/v1/projects/" + agentProjectID + "/agents/" + callingAgentID:
			// Agent mode's default (non-`--all`) path goes through the
			// project-scoped endpoint.
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: callingAgentID, Slug: "self", Ancestry: []string{"user-id", "parent-id"}})
		case "/api/v1/projects/" + agentProjectID + "/agents":
			agentsCalled = true
			gotAncestorID = r.URL.Query().Get("ancestorId")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		case "/api/v1/agents":
			agentsCalled = true
			gotAncestorID = r.URL.Query().Get("ancestorId")
			gotLineageRootID = r.URL.Query().Get("lineageRootId")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}
	agentHubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: agentProjectID}

	oldListAll, oldOutputFormat := listAll, outputFormat
	oldDescendants, oldAncestors, oldLineage := filterDescendants, filterAncestors, filterLineage
	listAll = true
	outputFormat = "json"
	defer func() {
		listAll, outputFormat = oldListAll, oldOutputFormat
		filterDescendants, filterAncestors, filterLineage = oldDescendants, oldAncestors, oldLineage
	}()

	run := func(ctx *HubContext) error {
		t.Helper()
		var err error
		captureListStdout(func() {
			err = listAgentsViaHub(ctx)
		})
		return err
	}

	reset := func() {
		filterDescendants, filterAncestors, filterLineage = "", "", ""
		gotAncestorID, gotLineageRootID, agentsCalled = "", "", false
	}

	t.Run("agent mode: bare --descendants resolves to the calling agent (unchanged)", func(t *testing.T) {
		reset()
		// Agent mode + a relationship flag is only ever exercised without
		// --all: agent identities have no hub-wide list authority, so --all
		// combined with a relationship flag is a hard error — see
		// TestListAgentsViaHub_AllMode_AgentIdentityWithRelationshipFlag_Errors.
		oldListAllLocal := listAll
		listAll = false
		defer func() { listAll = oldListAllLocal }()
		t.Setenv("SCION_CLI_MODE", "agent")
		t.Setenv("SCION_AGENT_ID", callingAgentID)
		filterDescendants = scopeInferSentinel

		require.NoError(t, run(agentHubCtx))
		assert.Equal(t, callingAgentID, gotAncestorID)
	})

	t.Run("human mode: bare --descendants resolves to the calling user, not an error", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "human")
		filterDescendants = scopeInferSentinel

		require.NoError(t, run(hubCtx))
		assert.Equal(t, callingUserID, gotAncestorID,
			"Ancestry records the creator user directly, so ancestorId=<user> works unchanged")
	})

	t.Run("assistant mode: bare --descendants resolves to the calling user, not an error", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "assistant")
		filterDescendants = scopeInferSentinel

		require.NoError(t, run(hubCtx))
		assert.Equal(t, callingUserID, gotAncestorID)
	})

	t.Run("human mode: bare --ancestors returns an empty list (a user has no ancestry), not an error", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "human")
		filterAncestors = scopeInferSentinel

		require.NoError(t, run(hubCtx))
		assert.False(t, agentsCalled, "a user has no Ancestry chain — nothing to query")
	})

	t.Run("human mode: bare --lineage roots at the calling user (no parent to walk to)", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "human")
		filterLineage = scopeInferSentinel

		require.NoError(t, run(hubCtx))
		assert.Equal(t, callingUserID, gotLineageRootID)
	})
}
