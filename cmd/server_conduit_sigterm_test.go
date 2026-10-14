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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
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
	registerConduitShutdownDrain(q, func() {
		assert.NoError(t, ctx.Err(), "the server context must still be live while GoAways are sent")
		record("drain")
	})
	q.addStep(shutdownPhaseMark, func() { record("mark") })

	q.begin(syscall.SIGTERM)
	assert.Equal(t, []string{"mark", "drain", "cancel"}, order, "conduit drain after the mark and before cancel")
	assert.Error(t, ctx.Err())
}

// TestRegisterConduitShutdownDrain_NotRunWhenServerStopsOtherwise: when
// the server stops for another reason (a fatal error cancels the server
// context without the graceful shutdown), the drain step never runs; the
// teardown's relay shutdown covers the relay.
func TestRegisterConduitShutdownDrain_NotRunWhenServerStopsOtherwise(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	drained := false
	q := newTestShutdown(cancel)
	registerConduitShutdownDrain(q, func() { drained = true })
	cancel()
	<-ctx.Done()
	assert.False(t, drained)
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
