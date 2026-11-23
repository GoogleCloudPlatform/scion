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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectHealthCmdRegistration(t *testing.T) {
	foundStatus := false
	hasHealthAlias := false
	for _, c := range projectCmd.Commands() {
		if c.Name() == "status" {
			foundStatus = true
			for _, alias := range c.Aliases {
				if alias == "health" {
					hasHealthAlias = true
				}
			}
			break
		}
	}
	assert.True(t, foundStatus, "expected 'status' subcommand to be registered under projectCmd")
	assert.True(t, hasHealthAlias, "expected 'health' alias on 'status' subcommand")
}

func TestPrintProjectHealthReports(t *testing.T) {
	reports := []ProjectHealthReport{
		{
			ID:   "proj-123",
			Name: "my-project",
			Slug: "my-project",
			Summary: ProjectHealthSummary{
				Total:     4,
				Starting:  1,
				Running:   2,
				Error:     1,
				Working:   1,
				Thinking:  0,
				Executing: 1,
				Blocked:   1,
				Completed: 0,
				Stalled:   0,
			},
			Agents: []hubclient.Agent{
				{
					ID:            "agent-1",
					Name:          "lead-dev",
					Template:      "https://hub.example.com/templates/developer@v1",
					HarnessConfig: "claude",
					Phase:         "running",
					Activity:      "working",
				},
				{
					ID:            "agent-2",
					Name:          "code-rev",
					Template:      "code-reviewer",
					HarnessConfig: "",
					Phase:         "running",
					Activity:      "blocked",
				},
				{
					ID:            "agent-3",
					Name:          "failing-agent",
					Template:      "default",
					HarnessConfig: "gemini-cli",
					Phase:         "error",
				},
				{
					ID:            "agent-4",
					Name:          "booting-agent",
					Template:      "default",
					HarnessConfig: "claude",
					Phase:         "starting",
					Activity:      "executing",
				},
			},
		},
	}

	var buf bytes.Buffer
	printProjectHealthReports(&buf, reports)
	output := buf.String()

	assert.Contains(t, output, "PROJECT STATUS & AGENT METRICS")
	assert.Contains(t, output, "Project: my-project (slug: my-project, id: proj-123)")
	assert.Contains(t, output, "Phases:   Total=4 | Running=2 | Error=1 | Stopped=0 | Starting=1")
	assert.Contains(t, output, "Activity: Working=1 | Thinking=0 | Blocked=1 | Completed=0 | Executing=1")
	assert.Contains(t, output, "lead-dev")
	assert.Contains(t, output, "developer")
	assert.Contains(t, output, "code-rev")
	assert.Contains(t, output, "failing-agent")
	assert.Contains(t, output, "1 agent(s) are blocked — run 'scion look <agent>' to see the block reason.")
	assert.Contains(t, output, "1 agent(s) are in error phase")
}

// TestRunProjectHealth_EmptyProjectsListKeepsProxyHint pins that an empty
// 200 from the projects list still carries the reverse-proxy hint. The list
// call now fails with apiclient.ErrNoContent rather than a JSON decode error,
// and HintProxyError must still recognise it.
func TestRunProjectHealth_EmptyProjectsListKeepsProxyHint(t *testing.T) {
	origHome := os.Getenv("HOME")
	origProjectPath, origAll, origJSON, origFormat := projectPath, projectHealthAll, projectHealthJSON, outputFormat
	defer func() {
		_ = os.Setenv("HOME", origHome)
		projectPath, projectHealthAll, projectHealthJSON, outputFormat = origProjectPath, origAll, origJSON, origFormat
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK) // empty body
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "")
	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, server.URL, "")
	projectHealthAll = true
	projectHealthJSON = false

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runProjectHealth(cmd, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apiclient.ErrNoContent), "got %v", err)
	assert.Contains(t, err.Error(), "failed to list projects from Hub")
	assert.Contains(t, err.Error(), "Hint: a reverse proxy may be intercepting /healthz and /health")
}

func TestFormatActivitySummary_Starting(t *testing.T) {
	out := formatActivitySummary(ProjectHealthSummary{Working: 1, Initializing: 2})
	assert.Contains(t, out, "Starting=2")
	// The phase count of the same name is not part of the activity line.
	assert.NotContains(t, formatActivitySummary(ProjectHealthSummary{Working: 1, Starting: 3}), "Starting")
}

func TestSummarizeAgents_CountsHarnessErrorWithErrors(t *testing.T) {
	sum := summarizeAgents([]hubclient.Agent{
		{Name: "a", Phase: "running", Activity: "harness_error"},
		{Name: "b", Phase: "running", Activity: "working"},
		{Name: "c", Phase: "error"},
	})
	assert.Equal(t, 3, sum.Total)
	assert.Equal(t, 2, sum.Running)
	assert.Equal(t, 1, sum.Error)
	assert.Equal(t, 1, sum.HarnessError)
	assert.Equal(t, 0, sum.OtherActivity)
	assert.Contains(t, formatActivitySummary(sum), "HarnessError=1")

	var buf bytes.Buffer
	printProjectHealthReports(&buf, []ProjectHealthReport{{
		ID: "p", Name: "p", Slug: "p", Summary: sum,
		Agents: []hubclient.Agent{{Name: "a", Phase: "running", Activity: "harness_error"}},
	}})
	assert.Contains(t, buf.String(), "1 agent(s) have a harness error")
}

// idle is counted on its own and shown in the activity line; it is a
// healthy state, so it is not lumped into Other.
func TestProjectHealthSummary_IdleCount(t *testing.T) {
	var s ProjectHealthSummary
	for _, a := range []string{"idle", "idle", "blocked", "working"} {
		s.addActivity(a)
	}
	assert.Equal(t, 2, s.Idle)
	assert.Equal(t, 1, s.Blocked)
	assert.Equal(t, 0, s.OtherActivity)
	assert.Contains(t, formatActivitySummary(s), "Idle=2")
	assert.NotContains(t, formatActivitySummary(ProjectHealthSummary{Working: 1}), "Idle")
}

func TestSummarizeAgents_SuspendedExcludedFromActivityCounts(t *testing.T) {
	agents := []hubclient.Agent{
		{ID: "a1", Name: "run-work", Phase: "running", Activity: "working"},
		{ID: "a2", Name: "run-idle", Phase: "running", Activity: "idle"},
		{ID: "a3", Name: "susp-done", Phase: "suspended", Activity: "completed"},
		{ID: "a4", Name: "susp-idle", Phase: "suspended", Activity: "idle"},
		{ID: "a5", Name: "susp-harness-err", Phase: "suspended", Activity: "harness_error"},
		{ID: "a6", Name: "susp-blocked", Phase: "suspended", Activity: "blocked"},
		{ID: "a7", Name: "stop-crash", Phase: "stopped", Activity: "crashed"},
	}

	s := summarizeAgents(agents)
	assert.Equal(t, 7, s.Total)
	assert.Equal(t, 2, s.Running)
	assert.Equal(t, 4, s.Suspended)
	assert.Equal(t, 1, s.Stopped)

	assert.Equal(t, 1, s.Working)
	assert.Equal(t, 1, s.Idle, "suspended+idle must not inflate live Idle count")
	assert.Equal(t, 0, s.Completed, "suspended+completed must not inflate live Completed count")
	assert.Equal(t, 0, s.HarnessError, "suspended+harness_error must not inflate live HarnessError count")
	assert.Equal(t, 0, s.Blocked, "suspended row with legacy blocked must not trigger blocked count")
	assert.Equal(t, 1, s.Crashed)

	var buf bytes.Buffer
	printProjectHealthReports(&buf, []ProjectHealthReport{{
		ID:      "p1",
		Name:    "proj",
		Slug:    "proj",
		Summary: s,
		Agents:  agents,
	}})
	out := buf.String()
	assert.Contains(t, out, "Suspended=4")
	assert.Contains(t, out, "Idle=1")
	assert.Contains(t, out, "Completed=0")
	assert.Contains(t, out, "Blocked=0")
	assert.NotContains(t, out, "agent(s) are blocked")
	assert.NotContains(t, out, "agent(s) have a harness error")
}

func TestSummarizeAgents_StoppedAndHaltedRetainedActivityExcluded(t *testing.T) {
	agents := []hubclient.Agent{
		{ID: "a1", Name: "run-done", Phase: "running", Activity: "completed"},
		{ID: "a2", Name: "stop-done", Phase: "stopped", Activity: "completed"},
		{ID: "a3", Name: "halt-done", Phase: "halted", Activity: "completed"},
		{ID: "a4", Name: "stop-crash", Phase: "stopped", Activity: "crashed"},
		{ID: "a5", Name: "stop-limits", Phase: "stopped", Activity: "limits_exceeded"},
	}

	s := summarizeAgents(agents)
	assert.Equal(t, 5, s.Total)
	assert.Equal(t, 1, s.Running)
	assert.Equal(t, 3, s.Stopped)
	assert.Equal(t, 1, s.Error)
	assert.Equal(t, 1, s.Completed, "stopped+completed and halted+completed must not inflate live Completed count")
	assert.Equal(t, 1, s.Crashed, "stopped+crashed terminal activity must still be counted")
	assert.Equal(t, 1, s.LimitsExceeded, "stopped+limits_exceeded terminal activity must still be counted")
}
