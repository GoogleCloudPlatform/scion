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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatLastSeen(t *testing.T) {
	tests := []struct {
		name     string
		offset   time.Duration
		expected string
	}{
		{"zero time", 0, "-"},
		{"just now", 0 * time.Second, "just now"},
		{"1 second ago", 1 * time.Second, "just now"},
		{"30 seconds ago", 30 * time.Second, "30 seconds ago"},
		{"59 seconds ago", 59 * time.Second, "59 seconds ago"},
		{"1 minute ago", 1 * time.Minute, "1 minute ago"},
		{"5 minutes ago", 5 * time.Minute, "5 minutes ago"},
		{"59 minutes ago", 59 * time.Minute, "59 minutes ago"},
		{"1 hour ago", 1 * time.Hour, "1 hour ago"},
		{"3 hours ago", 3 * time.Hour, "3 hours ago"},
		{"23 hours ago", 23 * time.Hour, "23 hours ago"},
		{"1 day ago", 24 * time.Hour, "1 day ago"},
		{"7 days ago", 7 * 24 * time.Hour, "7 days ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input time.Time
			if tt.name == "zero time" {
				input = time.Time{}
			} else {
				input = time.Now().Add(-tt.offset)
			}

			result := formatLastSeen(input)
			if result != tt.expected {
				t.Errorf("formatLastSeen() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestFormatLastSeenFutureTime(t *testing.T) {
	future := time.Now().Add(10 * time.Second)
	result := formatLastSeen(future)
	if result != "just now" {
		t.Errorf("formatLastSeen(future) = %q, want %q", result, "just now")
	}
}

func TestFormatLastActivity(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		status   string
		t        time.Time
		expected string
	}{
		{"activity with time", "thinking", now.Add(-30 * time.Second), "thinking, 30 seconds ago"},
		{"phase with time", "stopped", now.Add(-2 * time.Hour), "stopped, 2 hours ago"},
		{"empty status with time", "", now.Add(-5 * time.Minute), "5 minutes ago"},
		{"WORKING status with time", "WORKING", now.Add(-5 * time.Minute), "5 minutes ago"},
		{"working status with time", "working", now.Add(-5 * time.Minute), "5 minutes ago"},
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
	if !strings.Contains(lines[1], "thinking, 30 seconds ago") {
		t.Errorf("agent-1 row should contain 'thinking, 30 seconds ago': %s", lines[1])
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
	if !strings.Contains(lines[1], "2 minutes ago") {
		t.Errorf("hub agent row should contain '2 minutes ago': %s", lines[1])
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

// TestRejectHubOnlyFiltersInLocalMode is the ptone/scion#2146 review R1-5
// regression: a Hub-only filter set while listing locally must error rather
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

// TestListCmd_Args_RejectsPositionalArguments is the ptone/scion#2146 review
// R1-2 regression: `scion list --descendants foo` (space, not "=") must not
// silently drop "foo" as an ignored positional argument. Because
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

func TestResolveLineageRootID(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		ancestry []string
		want     string
	}{
		{
			name: "no ancestry: self is root (covers a user reference, which has no Ancestry at all)",
			id:   "self-id",
			want: "self-id",
		},
		{
			name:     "single ancestry entry (a top-level, user-created agent): the user is the direct parent",
			id:       "child-id",
			ancestry: []string{"user-id"},
			want:     "user-id",
		},
		{
			name:     "multi-entry ancestry: the LAST entry is the direct parent, not the topmost ancestor",
			id:       "grandchild-id",
			ancestry: []string{"user-id", "parent-id", "immediate-parent-id"},
			want:     "immediate-parent-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveLineageRootID(tt.id, tt.ancestry))
		})
	}
}

func TestResolveOwnerID(t *testing.T) {
	const meID = "11111111-1111-1111-1111-111111111111"
	const directID = "22222222-2222-2222-2222-222222222222"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: meID, Email: "me@example.com"})
		case r.URL.Path == "/api/v1/users/"+directID:
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: directID, Email: "direct@example.com"})
		case r.URL.Path == "/api/v1/users/by-name" || r.URL.Path == "/api/v1/users/alice" || r.URL.Path == "/api/v1/users/ambiguous" || r.URL.Path == "/api/v1/users/nobody":
			// These are name/email lookups mis-tried as direct IDs — 404 so
			// resolveOwnerID falls back to the search branch below.
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		case r.URL.Path == "/api/v1/users":
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

// TestResolveReferenceAgent_FallsBackOn403 is the ptone/scion#2146 review
// R1-3 regression: many agent identities are denied a single-resource GET on
// any agent other than themselves with a plain 403 (verified against a real
// Hub in TestR1_3_ListEndpointResolvesAgentIdentityCantGetOnPeer,
// pkg/hub/rs2_r1_fixes_test.go), even though the identical agent is visible
// through the authorized list endpoint. Before this fix, resolveReferenceAgent
// only fell through to list-based resolution on 404, so --descendants=<peer>
// failed outright for exactly the audience (agents naming a sibling) it is
// built for. This test proves the CLIENT-side fallback: given a GET that
// returns 403, resolution must still succeed via the list endpoint.
func TestResolveReferenceAgent_FallsBackOn403(t *testing.T) {
	const peerID = "66666666-6666-6666-6666-666666666666"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agents/"+peerID:
			// A single-resource GET on a peer is forbidden for this identity
			// (agent.read has no AgentScopes mapping — see
			// TestBypassAgents_LegitimateFlowsStillWork), even though the
			// agent genuinely exists and is listable.
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
		case r.URL.Path == "/api/v1/agents":
			// The list endpoint, by contrast, is authorized and includes the
			// peer — whether narrowed by id[] or returned in a bare page.
			ids := r.URL.Query()["id"]
			if len(ids) > 0 {
				require.Equal(t, []string{peerID}, ids)
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
		switch {
		case r.URL.Path == "/api/v1/agents/"+refID:
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/api/v1/agents":
			ids := r.URL.Query()["id"]
			if len(ids) == 0 {
				bareListCalled = true
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
				return
			}
			require.Equal(t, []string{refID}, ids)
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

// TestResolveReferenceAgent_PagesThroughNameMatches is the ptone/scion#2146
// review R1-8 regression: a name/slug match must not be missed just because
// it falls on a later page of the authorized list.
func TestResolveReferenceAgent_PagesThroughNameMatches(t *testing.T) {
	const targetID = "88888888-8888-8888-8888-888888888888"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agents/target-name":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/api/v1/agents":
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
		switch {
		case r.URL.Path == "/api/v1/users/"+ownerID:
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: ownerID})
		case r.URL.Path == "/api/v1/runtime-brokers/"+brokerID:
			_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{ID: brokerID, Name: "broker-x"})
		case r.URL.Path == "/api/v1/agents":
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

	oldStdout := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	err = listAgentsViaHub(hubCtx)
	_ = w.Close()
	os.Stdout = oldStdout

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
		switch {
		case r.URL.Path == "/api/v1/agents/"+refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent", Name: "ref-agent"})
		case r.URL.Path == "/api/v1/agents":
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
	defer func() {
		listAll, filterDescendants, outputFormat = oldListAll, oldDescendants, oldOutputFormat
	}()

	oldStdout := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	err = listAgentsViaHub(hubCtx)
	_ = w.Close()
	os.Stdout = oldStdout

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
		switch {
		case r.URL.Path == "/api/v1/agents/"+refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent", Ancestry: []string{"anc-1", "anc-2"}})
		case r.URL.Path == "/api/v1/agents/"+emptyRefID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: emptyRefID, Slug: "empty-ref-agent"})
		case r.URL.Path == "/api/v1/agents":
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
	defer func() {
		listAll, filterAncestors, outputFormat = oldListAll, oldAncestors, oldOutputFormat
	}()

	t.Run("non-empty ancestry is sent as id[] filter", func(t *testing.T) {
		agentsCalled, gotIDs = false, nil
		filterAncestors = refID

		oldStdout := os.Stdout
		_, w, _ := os.Pipe()
		os.Stdout = w
		err := listAgentsViaHub(hubCtx)
		_ = w.Close()
		os.Stdout = oldStdout

		require.NoError(t, err)
		assert.True(t, agentsCalled)
		assert.ElementsMatch(t, []string{"anc-1", "anc-2"}, gotIDs)
	})

	t.Run("empty ancestry short-circuits without querying agents", func(t *testing.T) {
		agentsCalled, gotIDs = false, nil
		filterAncestors = emptyRefID

		oldStdout := os.Stdout
		_, w, _ := os.Pipe()
		os.Stdout = w
		err := listAgentsViaHub(hubCtx)
		_ = w.Close()
		os.Stdout = oldStdout

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
	const rootlessRefID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	var gotLineageRootID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agents/"+refID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: refID, Slug: "ref-agent", Ancestry: []string{"user-id", "parent-id"}})
		case r.URL.Path == "/api/v1/agents/"+rootlessRefID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: rootlessRefID, Slug: "rootless-ref-agent"})
		case r.URL.Path == "/api/v1/agents":
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

	oldListAll, oldLineage, oldOutputFormat := listAll, filterLineage, outputFormat
	listAll = true
	outputFormat = "json"
	defer func() {
		listAll, filterLineage, outputFormat = oldListAll, oldLineage, oldOutputFormat
	}()

	run := func() {
		t.Helper()
		oldStdout := os.Stdout
		_, w, _ := os.Pipe()
		os.Stdout = w
		err := listAgentsViaHub(hubCtx)
		_ = w.Close()
		os.Stdout = oldStdout
		require.NoError(t, err)
	}

	t.Run("root is the direct parent (last ancestry entry), not the topmost ancestor", func(t *testing.T) {
		gotLineageRootID = ""
		filterLineage = refID
		run()
		assert.Equal(t, "parent-id", gotLineageRootID)
	})

	t.Run("ancestry-less reference is its own root", func(t *testing.T) {
		gotLineageRootID = ""
		filterLineage = rootlessRefID
		run()
		assert.Equal(t, rootlessRefID, gotLineageRootID)
	})
}

// TestListAgentsViaHub_AllMode_ReferenceResolutionUsesProjectScopedEndpoint is
// the ptone/scion#2146 review R1-3 regression for `--all`: an agent identity
// generally has no hub-wide list authority (only the project-scoped
// endpoint's same-project carve-out — see
// TestR1_3_ListEndpointResolvesAgentIdentityCantGetOnPeer,
// pkg/hub/rs2_r1_fixes_test.go), so if reference-agent resolution used the
// --all-selected global agentSvc, `--all --descendants` would silently find
// nothing for an agent caller, even for itself. refAgentSvc must resolve the
// reference through the project-scoped endpoint whenever a project is
// resolvable, regardless of --all, while the final listing still spans every
// project via the global endpoint.
func TestListAgentsViaHub_AllMode_ReferenceResolutionUsesProjectScopedEndpoint(t *testing.T) {
	const projectID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	const selfID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

	var globalAgentsCalled bool
	var gotAncestorID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects/"+projectID+"/agents/"+selfID:
			// Simulates the real Hub's denial of a single-resource GET for
			// an agent identity on any agent, including itself.
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
			// The project-scoped list endpoint succeeds — this is what
			// refAgentSvc must use for reference resolution.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{{ID: selfID, Slug: "self", Name: "self"}},
			})
		case r.URL.Path == "/api/v1/agents":
			// The global endpoint drives the final --all listing. If
			// reference resolution incorrectly fell back to this endpoint,
			// it would 404 here (not stubbed to resolve selfID) and the
			// test would fail with an error instead of asserting on
			// ancestorId.
			globalAgentsCalled = true
			gotAncestorID = r.URL.Query().Get("ancestorId")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	// ProjectID set directly on HubContext so GetProjectID resolves
	// deterministically without touching git/settings in this test.
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	oldListAll, oldDescendants, oldOutputFormat := listAll, filterDescendants, outputFormat
	listAll = true
	filterDescendants = scopeInferSentinel
	outputFormat = "json"
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_AGENT_ID", selfID)
	defer func() {
		listAll, filterDescendants, outputFormat = oldListAll, oldDescendants, oldOutputFormat
	}()

	oldStdout := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	err = listAgentsViaHub(hubCtx)
	_ = w.Close()
	os.Stdout = oldStdout

	require.NoError(t, err)
	assert.True(t, globalAgentsCalled, "the final --all listing must still hit the global endpoint")
	assert.Equal(t, selfID, gotAncestorID,
		"reference resolution must succeed via the project-scoped endpoint even though the global one can't see this identity")
}

// TestListAgentsViaHub_BareRelationshipFlag_ModeDefaults covers
// ptone/scion#2146 Q2: a bare relationship flag is never an error. In agent
// mode it resolves to the calling agent (unchanged); in human/assistant mode
// it resolves to the calling user, with --ancestors correctly reporting the
// user-has-no-ancestry case as an empty list rather than an error.
func TestListAgentsViaHub_BareRelationshipFlag_ModeDefaults(t *testing.T) {
	const callingUserID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const callingAgentID = "dddddddd-dddd-dddd-dddd-dddddddddddd"

	var gotAncestorID, gotLineageRootID string
	var agentsCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(hubclient.User{ID: callingUserID, Email: "me@example.com"})
		case r.URL.Path == "/api/v1/agents/"+callingAgentID:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{ID: callingAgentID, Slug: "self", Ancestry: []string{"user-id", "parent-id"}})
		case r.URL.Path == "/api/v1/agents":
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

	oldListAll, oldOutputFormat := listAll, outputFormat
	oldDescendants, oldAncestors, oldLineage := filterDescendants, filterAncestors, filterLineage
	listAll = true
	outputFormat = "json"
	defer func() {
		listAll, outputFormat = oldListAll, oldOutputFormat
		filterDescendants, filterAncestors, filterLineage = oldDescendants, oldAncestors, oldLineage
	}()

	run := func() error {
		t.Helper()
		oldStdout := os.Stdout
		_, w, _ := os.Pipe()
		os.Stdout = w
		err := listAgentsViaHub(hubCtx)
		_ = w.Close()
		os.Stdout = oldStdout
		return err
	}

	reset := func() {
		filterDescendants, filterAncestors, filterLineage = "", "", ""
		gotAncestorID, gotLineageRootID, agentsCalled = "", "", false
	}

	t.Run("agent mode: bare --descendants resolves to the calling agent (unchanged)", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "agent")
		t.Setenv("SCION_AGENT_ID", callingAgentID)
		filterDescendants = scopeInferSentinel

		require.NoError(t, run())
		assert.Equal(t, callingAgentID, gotAncestorID)
	})

	t.Run("human mode: bare --descendants resolves to the calling user, not an error", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "")
		filterDescendants = scopeInferSentinel

		require.NoError(t, run())
		assert.Equal(t, callingUserID, gotAncestorID,
			"Ancestry records the creator user directly, so ancestorId=<user> works unchanged")
	})

	t.Run("assistant mode: bare --descendants resolves to the calling user, not an error", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "assistant")
		filterDescendants = scopeInferSentinel

		require.NoError(t, run())
		assert.Equal(t, callingUserID, gotAncestorID)
	})

	t.Run("human mode: bare --ancestors returns an empty list (a user has no ancestry), not an error", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "")
		filterAncestors = scopeInferSentinel

		require.NoError(t, run())
		assert.False(t, agentsCalled, "a user has no Ancestry chain — nothing to query")
	})

	t.Run("human mode: bare --lineage roots at the calling user (no parent to walk to)", func(t *testing.T) {
		reset()
		t.Setenv("SCION_CLI_MODE", "")
		filterLineage = scopeInferSentinel

		require.NoError(t, run())
		assert.Equal(t, callingUserID, gotLineageRootID)
	})
}
