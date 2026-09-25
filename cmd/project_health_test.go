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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
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
