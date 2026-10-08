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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3602: stop --all and suspend --all via the Hub keep
// at most maxFanOutConcurrency agents in flight, and still report every
// agent's result and the same exit error as before. The local
// stopAllAgents/suspendAllAgents paths use the same boundedFanOut call but
// are not cap-tested here because they need a real runtime.

const lifecycleFanOutTotal = 40

// lifecycleFailAgents are answered with a 500 so the partial-failure
// reporting is exercised alongside the cap.
var lifecycleFailAgents = map[string]bool{"agent-03": true, "agent-17": true, "agent-39": true}

// runLifecycleAll runs fn (a stop/suspend --all entry point) against a gate
// Hub that holds every request for action, drives the gate, and returns the
// Hub plus captured stdout, stderr and the returned error.
func runLifecycleAll(t *testing.T, action, format string, fn func(*HubContext) error) (*gateHub, string, string, error) {
	t.Helper()
	origFormat, origHook, origRm := outputFormat, lifecycleFanOutQueuedHook, stopRm
	t.Cleanup(func() { outputFormat, lifecycleFanOutQueuedHook, stopRm = origFormat, origHook, origRm })
	outputFormat, stopRm = format, false

	h := newGateHub(t, fanOutNames(lifecycleFanOutTotal))
	h.gatedAction = "/" + action
	h.failAgents = lifecycleFailAgents
	queued := make(chan struct{}, lifecycleFanOutTotal)
	lifecycleFanOutQueuedHook = func() { queued <- struct{}{} }

	done := make(chan struct{})
	var runErr error
	var stdout, stderr string
	go func() {
		defer close(done)
		stdout, stderr = captureStdoutStderr(t, func() { runErr = fn(h.hubCtx(t)) })
	}()
	driveGate(t, h, queued, lifecycleFanOutTotal, done)
	return h, stdout, stderr, runErr
}

// assertLifecycleText checks the text report: one line per agent, errors for
// exactly the failing agents, and the aggregate error naming them.
func assertLifecycleText(t *testing.T, stderr string, err error, okLine, errPrefix string) {
	t.Helper()
	for _, name := range fanOutNames(lifecycleFanOutTotal) {
		if lifecycleFailAgents[name] {
			assert.Equal(t, 1, strings.Count(stderr, fmt.Sprintf("Agent '%s': error: ", name)), "error line for %s\n%s", name, stderr)
			assert.NotContains(t, stderr, fmt.Sprintf(okLine, name))
		} else {
			assert.Equal(t, 1, strings.Count(stderr, fmt.Sprintf(okLine, name)), "success line for %s\n%s", name, stderr)
		}
	}
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), errPrefix), err.Error())
	for name := range lifecycleFailAgents {
		assert.Contains(t, err.Error(), "\n  "+name+": ")
	}
	assert.Equal(t, len(lifecycleFailAgents), strings.Count(err.Error(), "\n  "))
}

// assertLifecycleJSON checks the JSON report: one entry per agent, with the
// failing agents marked as errors and an overall "partial" status.
func assertLifecycleJSON(t *testing.T, stdout, command string) {
	t.Helper()
	var got struct {
		Status  string `json:"status"`
		Command string `json:"command"`
		Results []struct {
			Agent  string `json:"agent"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
	assert.Equal(t, "partial", got.Status)
	assert.Equal(t, command, got.Command)
	require.Len(t, got.Results, lifecycleFanOutTotal)
	seen := map[string]bool{}
	for _, r := range got.Results {
		assert.False(t, seen[r.Agent], "duplicate result for %s", r.Agent)
		seen[r.Agent] = true
		if lifecycleFailAgents[r.Agent] {
			assert.Equal(t, "error", r.Status, r.Agent)
			assert.NotEmpty(t, r.Error, r.Agent)
		} else {
			assert.Equal(t, "success", r.Status, r.Agent)
			assert.Empty(t, r.Error, r.Agent)
		}
	}
}

func TestStopAllViaHub3602_FanOutIsCapped(t *testing.T) {
	h, _, stderr, err := runLifecycleAll(t, "stop", "", stopAllAgentsViaHub)
	assert.Equal(t, maxFanOutConcurrency, h.peakInFlight(), "at most maxFanOutConcurrency stops may be in flight")
	assertLifecycleText(t, stderr, err, "Agent '%s' stopped via Hub.\n", "failed to stop some agents via Hub:\n  ")
}

func TestStopAllViaHub3602_FanOutIsCappedJSON(t *testing.T) {
	h, stdout, _, err := runLifecycleAll(t, "stop", "json", stopAllAgentsViaHub)
	assert.Equal(t, maxFanOutConcurrency, h.peakInFlight(), "at most maxFanOutConcurrency stops may be in flight")
	var reported *jsonReportedError
	require.ErrorAs(t, err, &reported)
	assert.Equal(t, "failed to stop some agents via Hub", err.Error())
	assertLifecycleJSON(t, stdout, "stop")
}

func TestSuspendAllViaHub3602_FanOutIsCapped(t *testing.T) {
	h, _, stderr, err := runLifecycleAll(t, "suspend", "", suspendAllAgentsViaHub)
	assert.Equal(t, maxFanOutConcurrency, h.peakInFlight(), "at most maxFanOutConcurrency suspends may be in flight")
	assertLifecycleText(t, stderr, err, "Agent '%s' suspended via Hub.\n", "failed to suspend some agents via Hub:\n  ")
}

func TestSuspendAllViaHub3602_FanOutIsCappedJSON(t *testing.T) {
	h, stdout, _, err := runLifecycleAll(t, "suspend", "json", suspendAllAgentsViaHub)
	assert.Equal(t, maxFanOutConcurrency, h.peakInFlight(), "at most maxFanOutConcurrency suspends may be in flight")
	// suspend --all --format json reports a partial failure in the document
	// only; its nil error is existing behaviour, kept as is.
	require.NoError(t, err)
	assertLifecycleJSON(t, stdout, "suspend")
}
