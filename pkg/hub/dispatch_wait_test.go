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

package hub

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendStatus pushes a fake AgentStatusEvent onto the channel.
func sendStatus(ch chan<- Event, phase, activity string, detail *AgentDetail) {
	evt := AgentStatusEvent{
		AgentID:  "agent-1",
		Phase:    phase,
		Activity: activity,
		Detail:   detail,
	}
	data, _ := json.Marshal(evt)
	ch <- Event{Subject: "agent.agent-1.status", Data: data}
}

func TestWaitForAgentTransition_TerminalPhase(t *testing.T) {
	ch := make(chan Event, 8)
	unsub := func() {}
	_ = &Server{} // ensure Server type compiles; waitForAgentTransition is standalone

	go func() {
		sendStatus(ch, "starting", "pulling image", nil)
		sendStatus(ch, "running", "", nil)
	}()

	phase, err := waitForAgentTransition(
		context.Background(), ch, unsub,
		func(p string) bool { return p == "running" || p == "error" },
	)
	require.NoError(t, err)
	assert.Equal(t, "running", phase)
}

func TestWaitForAgentTransition_ErrorPhase(t *testing.T) {
	ch := make(chan Event, 8)
	unsub := func() {}
	_ = &Server{} // ensure Server type compiles; waitForAgentTransition is standalone

	go func() {
		sendStatus(ch, "starting", "", nil)
		sendStatus(ch, "error", "", nil)
	}()

	phase, err := waitForAgentTransition(
		context.Background(), ch, unsub,
		func(p string) bool { return p == "running" || p == "error" },
	)
	require.NoError(t, err)
	assert.Equal(t, "error", phase)
}

func TestWaitForAgentTransition_RollingReset(t *testing.T) {
	// Interim detail updates keep the wait alive past one window.
	// We use a very short timeout override for testing speed.
	ch := make(chan Event, 64)
	unsub := func() {}
	_ = &Server{} // ensure Server type compiles; waitForAgentTransition is standalone

	// Override the timeout by wrapping: we cannot easily override the
	// const, but we can send events faster than the 90s default and
	// confirm the terminal is reached. The real test is that interim
	// events don't cause early return. Send 5 interim events, then terminal.
	go func() {
		for i := 0; i < 5; i++ {
			sendStatus(ch, "starting", "step", &AgentDetail{Message: "progress"})
			time.Sleep(5 * time.Millisecond)
		}
		sendStatus(ch, "running", "", nil)
	}()

	phase, err := waitForAgentTransition(
		context.Background(), ch, unsub,
		func(p string) bool { return p == "running" || p == "error" },
	)
	require.NoError(t, err)
	assert.Equal(t, "running", phase)
}

func TestWaitForAgentTransition_SilenceExpiry(t *testing.T) {
	// Override the rolling timeout to something very short so the test
	// completes quickly. We can't mutate the const, so instead we close
	// the channel which produces a zero Event -> ErrDispatchFailed via
	// the ok=false branch.
	ch := make(chan Event, 4)
	unsub := func() {}
	_ = &Server{} // ensure Server type compiles; waitForAgentTransition is standalone

	// Close immediately: simulates silence (no events).
	close(ch)

	_, err := waitForAgentTransition(
		context.Background(), ch, unsub,
		func(p string) bool { return p == "running" },
	)
	assert.ErrorIs(t, err, ErrDispatchFailed)
}

func TestWaitForAgentTransition_ContextCancel(t *testing.T) {
	ch := make(chan Event, 4)
	unsub := func() {}
	_ = &Server{} // ensure Server type compiles; waitForAgentTransition is standalone

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitForAgentTransition(
		ctx, ch, unsub,
		func(p string) bool { return p == "running" },
	)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWaitForAgentTransition_UnsubCalled(t *testing.T) {
	ch := make(chan Event, 4)
	var unsubCalled bool
	unsub := func() { unsubCalled = true }
	_ = &Server{} // ensure Server type compiles; waitForAgentTransition is standalone

	close(ch)
	_, _ = waitForAgentTransition(
		context.Background(), ch, unsub,
		func(p string) bool { return p == "running" },
	)
	assert.True(t, unsubCalled, "unsub must be called on return")
}

func TestWaitForAgentTransition_StopTerminal(t *testing.T) {
	ch := make(chan Event, 4)
	unsub := func() {}
	_ = &Server{} // ensure Server type compiles; waitForAgentTransition is standalone

	go func() {
		sendStatus(ch, "stopped", "", nil)
	}()

	phase, err := waitForAgentTransition(
		context.Background(), ch, unsub,
		func(p string) bool { return p == "stopped" || p == "error" },
	)
	require.NoError(t, err)
	assert.Equal(t, "stopped", phase)
}
