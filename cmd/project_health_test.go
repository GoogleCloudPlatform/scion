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
	"io"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
)

func TestProjectHealthCmdRegistration(t *testing.T) {
	found := false
	for _, c := range projectCmd.Commands() {
		if c.Name() == "health" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected 'health' subcommand to be registered under projectCmd")
}

func TestPrintProjectHealthReports(t *testing.T) {
	reports := []ProjectHealthReport{
		{
			ID:   "proj-123",
			Name: "my-project",
			Slug: "my-project",
			Summary: ProjectHealthSummary{
				Total:     3,
				Running:   2,
				Error:     1,
				Working:   1,
				Thinking:  0,
				Blocked:   1,
				Completed: 0,
				Stalled:   0,
			},
			Agents: []hubclient.Agent{
				{
					ID:            "agent-1",
					Name:          "lead-dev",
					Template:      "developer",
					HarnessConfig: "claude",
					Phase:         "running",
					Activity:      "working",
				},
				{
					ID:            "agent-2",
					Name:          "code-rev",
					Template:      "code-reviewer",
					HarnessConfig: "claude",
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
			},
		},
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printProjectHealthReports(reports)

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	assert.Contains(t, output, "PROJECT HEALTH & AGENT METRICS")
	assert.Contains(t, output, "Project: my-project (slug: my-project, id: proj-123)")
	assert.Contains(t, output, "Total=3 | Running=2 | Error=1 | Working/Thinking=1 | Blocked=1 | Completed=0")
	assert.Contains(t, output, "lead-dev")
	assert.Contains(t, output, "code-rev")
	assert.Contains(t, output, "failing-agent")
	assert.Contains(t, output, "1 agent(s) are blocked")
	assert.Contains(t, output, "1 agent(s) are in error phase")
}
