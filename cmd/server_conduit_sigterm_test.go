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
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterConduitShutdownDrain_DrainsBeforeCancel (design §3.3,
// ptone/scion#5054 C1): the relay drain is a pre-cancel step of the one
// graceful shutdown: it runs after the mark and before cancel (which is
// what closes the listeners), and the conduit subscribes to no signal of
// its own.
func TestRegisterConduitShutdownDrain_DrainsBeforeCancel(t *testing.T) {
	captureStdLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var order []string
	var mu sync.Mutex
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	q := newTestShutdown(func() {
		record("cancel")
		cancel()
	})
	registerConduitShutdownDrain(q, time.Minute, func() {
		assert.NoError(t, ctx.Err(), "the server context must still be live while GoAways are sent")
		record("drain")
	})
	q.addStep(shutdownPhaseMark, func() { record("mark") })

	q.begin(syscall.SIGTERM)
	assert.Equal(t, []string{"mark", "drain", "cancel"}, order, "conduit drain after the mark and before cancel")
	assert.Error(t, ctx.Err())
}

// TestRegisterConduitShutdownDrain_RefusedAfterBegin: a drain registered
// once the shutdown has begun is refused and never runs from the step (the
// teardown's relay shutdown covers the relay).
func TestRegisterConduitShutdownDrain_RefusedAfterBegin(t *testing.T) {
	captureStdLog(t)
	q := newTestShutdown(func() {})
	q.begin(syscall.SIGTERM)
	var drained atomic.Bool
	assert.False(t, registerConduitShutdownDrain(q, time.Minute, func() { drained.Store(true) }), "a drain registered after the shutdown began must be refused")
	q.begin(syscall.SIGTERM)
	assert.False(t, drained.Load())
}

// TestRegisterConduitShutdownDrain_BoundedWait (conduit review F4): a drain
// step that blocks (a stalled GoAway send) holds the shutdown only until
// its bound on the shutdown clock; cancel then runs.
func TestRegisterConduitShutdownDrain_BoundedWait(t *testing.T) {
	captureStdLog(t)
	clk := clock.NewFake(shutdownTestT0)
	cancelled := make(chan time.Time, 1)
	q := newServerShutdownSequence(func() { cancelled <- clk.Now() }, clk)
	release := make(chan struct{})
	defer close(release)
	require.True(t, registerConduitShutdownDrain(q, 2*time.Second, func() { <-release }))
	go q.begin(syscall.SIGTERM)
	require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(2*time.Second)), "drain bound not armed")
	clk.Advance(2 * time.Second)
	select {
	case at := <-cancelled:
		assert.Equal(t, shutdownTestT0.Add(2*time.Second), at, "cancel at the drain bound")
	case <-time.After(shutdownTestWait):
		t.Fatal("a blocked drain held the shutdown past its bound")
	}
}

// TestConduitDrainStepBound: min(grace, S/4) with shutdown_grace set (S =
// max(G-2s, G/2)); 10s unset.
func TestConduitDrainStepBound(t *testing.T) {
	assert.Equal(t, conduitDrainStepFallback, conduitDrainStepBound(serverShutdownBudget{}))
	assert.Equal(t, 2*time.Second, conduitDrainStepBound(serverShutdownBudget{grace: 10 * time.Second, set: true}))
	assert.Equal(t, 4500*time.Millisecond, conduitDrainStepBound(serverShutdownBudget{grace: 20 * time.Second, set: true}))
	assert.Equal(t, 125*time.Millisecond, conduitDrainStepBound(serverShutdownBudget{grace: time.Second, set: true}))
}

// TestHubServerPathSubscribesToSignalsOnce (conduit review F2,
// ptone/scion#5105): on the hub server path, installServerShutdownSignals
// is the only SIGINT/SIGTERM subscriber that can cancel; no non-test
// cmd/server_*.go file calls signal.Notify or signal.NotifyContext, except
// the listed ones. (cmd/broker.go's foreground SIGTERM handler is the known
// exception outside these files: it removes the broker record and calls the
// same run-once shutdown.)
func TestHubServerPathSubscribesToSignalsOnce(t *testing.T) {
	// Files allowed their own subscription, with the reason. Empty today;
	// the relay role (server_relay.go, a separate process mode dispatched
	// before installServerShutdownSignals) joins it after its rebase.
	exceptions := map[string]string{}
	call := regexp.MustCompile(`signal\.Notify(Context)?\(`)
	files, err := filepath.Glob("server_*.go")
	require.NoError(t, err)
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if _, ok := exceptions[f]; ok {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		scanned++
		assert.Empty(t, call.FindAllString(string(src), -1), "%s subscribes to signals; only installServerShutdownSignals may (ptone/scion#5054)", f)
	}
	assert.GreaterOrEqual(t, scanned, 4, "server_*.go files scanned")
	src, err := os.ReadFile("server_foreground.go")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(src), "installServerShutdownSignals(signalsCtx, signal.Notify, "), "the server subscribes exactly once, through installServerShutdownSignals")
}

// TestConduitShutdownDrainSetting: shutdown_grace has no default, so unset
// leaves the drain deadline as before (no cap); a set grace caps it.
func TestConduitShutdownDrainSetting(t *testing.T) {
	cfg := &config.GlobalConfig{}
	grace, capDrain := conduitShutdownDrainSetting(cfg)
	assert.Zero(t, grace)
	assert.False(t, capDrain)

	cfg.Hub.Conduit.ShutdownGrace = "10s"
	grace, capDrain = conduitShutdownDrainSetting(cfg)
	assert.Equal(t, 10*time.Second, grace)
	assert.True(t, capDrain)
}
