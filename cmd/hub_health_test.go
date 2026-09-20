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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
)

func TestHubHealthCmdRegistration(t *testing.T) {
	found := false
	for _, c := range hubCmd.Commands() {
		if c.Name() == "health" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected 'health' subcommand to be registered under hubCmd")
}

func TestPrintHubHealthSummary(t *testing.T) {
	summary := &hubclient.HealthSummaryResponse{
		Status: "healthy",
		Hub: hubclient.HealthSummaryHub{
			Status:           "healthy",
			Version:          "0c07fdee",
			Uptime:           "25h10m",
			ConnectedBrokers: 1,
			ActiveAgents:     12,
			Projects:         3,
		},
		Database: hubclient.HealthSummaryDB{
			Status:             "healthy",
			PoolActive:         1,
			PoolMax:            5,
			PoolIdle:           4,
			PoolWaitCountTotal: 42,
		},
		Brokers: []hubclient.HealthSummaryBrkr{
			{
				ID:               "broker-1",
				Name:             "Hosted Broker",
				Status:           "online",
				Runtime:          "docker",
				RuntimeAvailable: true,
				AgentCount:       12,
				AgentHealthy:     12,
				LastHeartbeat:    time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
			},
		},
		Agents: hubclient.HealthSummaryAgents{
			Total: 12,
			ByPhase: map[string]int{
				"running": 12,
				"error":   0,
			},
			Stalled: []string{"stalled-agent-1"},
			Errored: []string{"errored-agent-1"},
		},
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printHubHealthSummary(summary, "https://hub.example.com/")

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	assert.Contains(t, output, "SCION HUB HEALTH & METRICS")
	assert.Contains(t, output, "https://hub.example.com/")
	assert.Contains(t, output, "Overall Status:      healthy")
	assert.Contains(t, output, "Active=1 / Max=5")
	assert.Contains(t, output, "Hosted Broker")
	assert.Contains(t, output, "stalled-agent-1")
	assert.Contains(t, output, "errored-agent-1")
}
