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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fastWaitReadyPoll(t *testing.T) {
	t.Helper()
	orig := waitReadyPollInterval
	waitReadyPollInterval = time.Millisecond
	t.Cleanup(func() { waitReadyPollInterval = orig })
}

// probeSequence returns each probe in turn, repeating the last one.
func probeSequence(probes ...readyProbe) func(context.Context) (readyProbe, error) {
	i := 0
	return func(context.Context) (readyProbe, error) {
		p := probes[i]
		if i < len(probes)-1 {
			i++
		}
		return p, nil
	}
}

func TestWaitForAgentAcceptsInput_Success(t *testing.T) {
	fastWaitReadyPoll(t)
	var progress bytes.Buffer
	p, err := waitForAgentAcceptsInput(context.Background(), waitReadyOptions{
		AgentName: "a1",
		Get: probeSequence(
			readyProbe{Phase: "starting"},
			readyProbe{Phase: "running", Activity: "initializing"},
			readyProbe{Phase: "running", Activity: "initializing"},
			readyProbe{Phase: "running", Activity: "working"},
		),
		Timeout:  5 * time.Second,
		Progress: &progress,
	})
	require.NoError(t, err)
	assert.Equal(t, "working", p.Activity)
	assert.Equal(t, 1, strings.Count(progress.String(), "Waiting for agent 'a1' to accept input"), "progress is printed once")
}

// An image that predates the initializing activity reports working at once.
func TestWaitForAgentAcceptsInput_OldImageReadyAtOnce(t *testing.T) {
	fastWaitReadyPoll(t)
	var progress bytes.Buffer
	_, err := waitForAgentAcceptsInput(context.Background(), waitReadyOptions{
		AgentName: "a1",
		Get:       probeSequence(readyProbe{Phase: "running", Activity: "working"}),
		Timeout:   5 * time.Second,
		Progress:  &progress,
	})
	require.NoError(t, err)
	assert.Empty(t, progress.String())
}

// A Hub that only reports the legacy Status string gives no activity; a
// running agent then counts as ready.
func TestWaitForAgentAcceptsInput_LegacyStatusRunning(t *testing.T) {
	fastWaitReadyPoll(t)
	p := hubReadyProbe(&hubclient.Agent{Status: "running"})
	require.True(t, p.LegacyStatus)
	_, err := waitForAgentAcceptsInput(context.Background(), waitReadyOptions{
		AgentName: "a1", Get: probeSequence(p), Timeout: time.Second,
	})
	require.NoError(t, err)
}

func TestWaitForAgentAcceptsInput_Timeout(t *testing.T) {
	fastWaitReadyPoll(t)
	_, err := waitForAgentAcceptsInput(context.Background(), waitReadyOptions{
		AgentName: "a1",
		Get:       probeSequence(readyProbe{Phase: "running", Activity: "initializing"}),
		Timeout:   50 * time.Millisecond,
	})
	var te *waitReadyTimeoutError
	require.ErrorAs(t, err, &te)
	assert.Equal(t, exitCodeNotReady, exitCodeFor(err))
	assert.Equal(t, 3, exitCodeFor(err))
	assert.Contains(t, err.Error(), "status: starting")
	assert.Contains(t, err.Error(), "left running")
	assert.False(t, isReportedInJSON(err), "text mode prints the error")
}

func TestWaitForAgentAcceptsInput_FetchErrorsRetried(t *testing.T) {
	fastWaitReadyPoll(t)
	calls := 0
	_, err := waitForAgentAcceptsInput(context.Background(), waitReadyOptions{
		AgentName: "a1",
		Get: func(context.Context) (readyProbe, error) {
			calls++
			if calls < 3 {
				return readyProbe{}, errors.New("transient")
			}
			return readyProbe{Phase: "running", Activity: "thinking"}, nil
		},
		Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
}

func TestWaitForAgentAcceptsInput_AgentStops(t *testing.T) {
	fastWaitReadyPoll(t)
	_, err := waitForAgentAcceptsInput(context.Background(), waitReadyOptions{
		AgentName: "a1",
		Get: probeSequence(
			readyProbe{Phase: "running", Activity: "initializing"},
			readyProbe{Phase: "stopped"},
		),
		Timeout: 5 * time.Second,
	})
	require.Error(t, err)
	var te *waitReadyTimeoutError
	assert.False(t, errors.As(err, &te))
	assert.Equal(t, 1, exitCodeFor(err))
}

func TestMarkWaitReadyReported(t *testing.T) {
	assert.NoError(t, markWaitReadyReported(nil))

	err := markWaitReadyReported(&waitReadyTimeoutError{Agent: "a1", Timeout: time.Second})
	assert.True(t, isReportedInJSON(err), "JSON mode prints no banner")
	assert.Equal(t, exitCodeNotReady, exitCodeFor(err))

	err = markWaitReadyReported(errors.New("agent stopped"))
	assert.True(t, isReportedInJSON(err))
	assert.Equal(t, 1, exitCodeFor(err))
}

func TestValidateWaitReadyFlags(t *testing.T) {
	origReady, origNoWait := startWaitReady, startNoWait
	t.Cleanup(func() { startWaitReady, startNoWait = origReady, origNoWait })

	startWaitReady, startNoWait = true, true
	assert.ErrorContains(t, validateWaitReadyFlags(), "--wait-ready cannot be used with --no-wait")
	startWaitReady, startNoWait = true, false
	assert.NoError(t, validateWaitReadyFlags())
	startWaitReady, startNoWait = false, true
	assert.NoError(t, validateWaitReadyFlags())
}

func TestRemainingWaitReadyTimeout(t *testing.T) {
	orig := startWaitTimeout
	t.Cleanup(func() { startWaitTimeout = orig })

	startWaitTimeout = 0
	assert.Equal(t, 120*time.Second, remainingWaitReadyTimeout(time.Now()))
	startWaitTimeout = time.Minute
	got := remainingWaitReadyTimeout(time.Now().Add(-20 * time.Second))
	assert.InDelta(t, float64(40*time.Second), float64(got), float64(time.Second))
	assert.Equal(t, minWaitReadyTimeout, remainingWaitReadyTimeout(time.Now().Add(-2*time.Minute)))
}

type fakeAgentLister struct{ agents []api.AgentInfo }

func (f fakeAgentLister) List(context.Context, map[string]string) ([]api.AgentInfo, error) {
	return f.agents, nil
}

func TestLocalReadyProbe(t *testing.T) {
	get := localReadyProbe(fakeAgentLister{agents: []api.AgentInfo{
		{Name: "other", Phase: "running", Activity: "working"},
		{Name: "a1", Phase: "running", Activity: "initializing"},
	}}, "a1")
	p, err := get(context.Background())
	require.NoError(t, err)
	assert.Equal(t, readyProbe{Phase: "running", Activity: "initializing"}, p)

	_, err = localReadyProbe(fakeAgentLister{}, "a1")(context.Background())
	assert.Error(t, err)
}

func TestPrintBroadcastAccepted_Starting(t *testing.T) {
	out := captureStdout(t, func() {
		printBroadcastAccepted(&hubclient.BroadcastResponse{Targeted: 3, Starting: 1})
	})
	assert.Contains(t, out, "Broadcast accepted (3 running agents targeted).")
	assert.Contains(t, out, "1 targeted agent(s) still starting")

	out = captureStdout(t, func() {
		printBroadcastAccepted(&hubclient.BroadcastResponse{Targeted: 3})
	})
	assert.NotContains(t, out, "starting")
}
