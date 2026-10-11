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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConduitSignals replaces the SIGTERM trap's signal seams; it returns
// the channel the trap registered (nil until it does) and the signals it
// asked for, and whether the trap was removed.
type fakeConduitSignals struct {
	mu      sync.Mutex
	ch      chan<- os.Signal
	signals []os.Signal
	stopped bool
}

func installFakeConduitSignals(t *testing.T) *fakeConduitSignals {
	t.Helper()
	f := &fakeConduitSignals{}
	prevNotify, prevStop := conduitSignalNotify, conduitSignalStop
	conduitSignalNotify = func(c chan<- os.Signal, sig ...os.Signal) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ch, f.signals = c, append(f.signals, sig...)
	}
	conduitSignalStop = func(c chan<- os.Signal) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if c == f.ch {
			f.stopped = true
		}
	}
	t.Cleanup(func() { conduitSignalNotify, conduitSignalStop = prevNotify, prevStop })
	return f
}

// TestTrapConduitSIGTERM_DrainsBeforeCancel: SIGTERM runs the relay drain
// (GoAway to every session) to completion before the server context is
// cancelled, which is what closes the listeners. Only SIGTERM is trapped
// (os.Interrupt keeps its own handler), and the trap is removed afterwards.
func TestTrapConduitSIGTERM_DrainsBeforeCancel(t *testing.T) {
	f := installFakeConduitSignals(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var order []string
	var mu sync.Mutex
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	drain := func() {
		assert.NoError(t, ctx.Err(), "the server context must still be live while GoAways are sent")
		record("drain")
	}
	cancelled := func() {
		record("cancel")
		cancel()
	}
	var wg sync.WaitGroup
	trapConduitSIGTERM(ctx, cancelled, drain, &wg)

	f.mu.Lock()
	require.NotNil(t, f.ch)
	assert.Equal(t, []os.Signal{syscall.SIGTERM}, f.signals, "only SIGTERM is trapped")
	ch := f.ch
	f.mu.Unlock()

	ch <- syscall.SIGTERM
	wg.Wait()
	assert.Equal(t, []string{"drain", "cancel"}, order)
	assert.Error(t, ctx.Err())
	f.mu.Lock()
	assert.True(t, f.stopped, "trap removed")
	f.mu.Unlock()
}

// TestTrapConduitSIGTERM_RemovedOnShutdown: when the server stops for
// another reason (os.Interrupt, a fatal error), the trap is removed and no
// drain runs from it.
func TestTrapConduitSIGTERM_RemovedOnShutdown(t *testing.T) {
	f := installFakeConduitSignals(t)
	ctx, cancel := context.WithCancel(context.Background())
	drained := false
	var wg sync.WaitGroup
	trapConduitSIGTERM(ctx, cancel, func() { drained = true }, &wg)
	cancel()
	wg.Wait()
	assert.False(t, drained)
	f.mu.Lock()
	assert.True(t, f.stopped, "trap removed")
	f.mu.Unlock()
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
