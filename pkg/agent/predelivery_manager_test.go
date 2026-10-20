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

package agent

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/require"
)

// Pre-delivery work through the agent manager itself (ptone/scion#4596,
// design 8.4 step 2): the interrupt path and the buffered flush that
// NewManager wires both run it once, against the resolved container,
// before the text is injected, and append its text.

const preDeliveryLine = "\n\nAttachment not delivered: ~/.scion/inbox/m/a.txt (1 bytes): failed verification. Ask the sender to resend it."

// managerPreDelivery records its call: the runtime and container it got,
// and how many runtime commands had run before it.
type managerPreDelivery struct {
	before func() int
	mu     sync.Mutex
	calls  int
	rt     runtime.Runtime
	cid    string
	cmdsAt int
}

func (p *managerPreDelivery) BeforeText(_ context.Context, rt runtime.Runtime, containerID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.rt, p.cid = rt, containerID
	if p.before != nil {
		p.cmdsAt = p.before()
	}
	return preDeliveryLine
}

func (p *managerPreDelivery) Discard() {}

func (p *managerPreDelivery) snapshot() (int, runtime.Runtime, string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.rt, p.cid, p.cmdsAt
}

// textReadyRuntime is fakeReadyRuntime (always ready) that also keeps the
// text each stdin exec carried.
func textReadyRuntime() (*fakeReadyRuntime, func() []string) {
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbe(true), nil })
	var mu sync.Mutex
	var texts []string
	rt.MockRuntime.ExecWithStdinFunc = func(_ context.Context, _ string, cmd []string, stdin io.Reader) (string, error) {
		b, _ := io.ReadAll(stdin)
		rt.mu.Lock()
		rt.cmds = append(rt.cmds, strings.Join(cmd, " "))
		rt.mu.Unlock()
		mu.Lock()
		texts = append(texts, string(b))
		mu.Unlock()
		return "", nil
	}
	return rt, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), texts...)
	}
}

// injections counts the runtime commands that inject text (everything but
// readiness probes).
func injections(rt *fakeReadyRuntime) func() int {
	return func() int {
		_, cmds := rt.snapshot()
		n := 0
		for _, c := range cmds {
			if c != "probe" {
				n++
			}
		}
		return n
	}
}

func TestManager_InterruptRunsPreDeliveryBeforeText(t *testing.T) {
	rt, texts := textReadyRuntime()
	mgr := newReadyTestManager(rt)
	defer mgr.Close()
	pd := &managerPreDelivery{before: injections(rt)}
	require.NoError(t, mgr.Message(WithPreDelivery(context.Background(), pd), "test-agent", "", "stop", true))
	calls, gotRT, cid, cmdsAt := pd.snapshot()
	require.Equal(t, 1, calls)
	require.Equal(t, runtime.Runtime(rt), gotRT)
	require.Equal(t, "agent-1", cid, "the resolved container")
	require.Zero(t, cmdsAt, "it runs before any text is injected")
	require.Contains(t, strings.Join(texts(), ""), "stop"+preDeliveryLine, "its text follows the message's own")
}

func TestManager_BufferedFlushRunsPreDeliveryBeforeText(t *testing.T) {
	rt, texts := textReadyRuntime()
	mgr := NewManager(rt).(*AgentManager)
	mgr.readyTimings = fastReadyTimings
	defer mgr.Close()
	pd := &managerPreDelivery{before: injections(rt)}
	require.NoError(t, mgr.Message(WithPreDelivery(context.Background(), pd), "test-agent", "", "hello", false))
	waitFor(t, 10*time.Second, func() bool {
		return strings.Contains(strings.Join(texts(), ""), "hello"+preDeliveryLine)
	})
	calls, gotRT, cid, cmdsAt := pd.snapshot()
	require.Equal(t, 1, calls)
	require.Equal(t, runtime.Runtime(rt), gotRT)
	require.Equal(t, "agent-1", cid)
	require.Zero(t, cmdsAt, "it runs before any text is injected")
}

// TestManager_PreDeliveryRefusesAmbiguousOwnedTarget: on an owned manager, a
// name that matches agents of several projects with no project given gets
// no write (no exec into either container, no runtime handed over), and
// the work reports its files as not written; with a project it gets that
// project's container.
func TestManager_PreDeliveryRefusesAmbiguousOwnedTarget(t *testing.T) {
	rt, execs := twoProjectWorkerRuntime()
	m := NewManager(rt).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	defer m.Close()

	pd := &managerPreDelivery{}
	require.Equal(t, preDeliveryLine, m.runPreDelivery(context.Background(), "worker", "", pd))
	calls, gotRT, cid, _ := pd.snapshot()
	require.Equal(t, 1, calls)
	require.Nil(t, gotRT, "no runtime for an ambiguous name")
	require.Empty(t, cid)
	require.Empty(t, execs(), "no exec into either agent")

	scoped := &managerPreDelivery{}
	m.runPreDelivery(context.Background(), "worker", "proj-2", scoped)
	_, gotRT, cid, _ = scoped.snapshot()
	require.NotNil(t, gotRT)
	require.Equal(t, "c-2", cid, "the project's own container")
}
