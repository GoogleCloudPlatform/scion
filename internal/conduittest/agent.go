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

package conduittest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// RecordingTmuxRunner implements tmuxdeliver.Runner and records every argv
// and stdin payload it executes.
type RecordingTmuxRunner struct {
	mu        sync.Mutex
	calls     [][]string
	stdins    []string
	flushes   int
	beforeRun func([]string)
}

// SetBeforeRun installs an optional hook invoked before each Run call (nil clears it).
func (r *RecordingTmuxRunner) SetBeforeRun(fn func([]string)) {
	r.mu.Lock()
	r.beforeRun = fn
	r.mu.Unlock()
}

// Run implements tmuxdeliver.Runner.
func (r *RecordingTmuxRunner) Run(_ context.Context, argv []string, stdin io.Reader) (string, error) {
	cp := append([]string(nil), argv...)
	r.mu.Lock()
	hook := r.beforeRun
	r.mu.Unlock()
	if hook != nil {
		hook(cp)
	}
	var stdinStr string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		stdinStr = string(b)
	}
	r.mu.Lock()
	r.calls = append(r.calls, cp)
	r.stdins = append(r.stdins, stdinStr)
	if len(cp) >= 2 && cp[0] == "tmux" && cp[1] == "paste-buffer" {
		r.flushes++
	}
	r.mu.Unlock()
	return "", nil
}

// Calls returns a deep copy of all recorded argv slices.
func (r *RecordingTmuxRunner) Calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// Stdins returns a copy of the stdin payload recorded for each Run call,
// aligned by index with Calls().
func (r *RecordingTmuxRunner) Stdins() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stdins...)
}

// Flushes returns the number of tmux paste-buffer calls recorded.
func (r *RecordingTmuxRunner) Flushes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes
}

// AgentOptions configures a fake Conduit agent.
type AgentOptions struct {
	HubURL     string
	AgentID    string
	ProjectID  string
	LaunchID   string
	Token      func() string
	HTTPClient *http.Client
	DialLocal  func(ctx context.Context, network, addr string) (net.Conn, error)
	Policy     conduit.Policy
	// EnablePTY installs a FakePTYSpawner (exposed on Agent.PTYs) when
	// SpawnPTY is nil. When both EnablePTY is false and SpawnPTY is nil,
	// the agent sets NoPTY: true and does not advertise pty.
	EnablePTY bool
	SpawnPTY  sconduit.PTYSpawner
	Backoff   *conduit.Backoff
	// Session configures the underlying conduit session. Leaving
	// Session.RPCHandler nil installs the default control.Handler (serving
	// deliver-message and rotate-token and recording RPCCount/RPCPaths/
	// KickCount). Setting Session.RPCHandler replaces the control handler, so
	// deliver-message, rotate-token, and RPCCount/RPCPaths/KickCount stop
	// working.
	Session   conduit.Config
	Clock     clock.Clock
	OnSession func(*conduitv1.Welcome)
}

// Agent is a fake agent wired with the real pkg/sciontool/conduit session
// client, the real control.Handler (with deliver-message and rotate-token
// enabled), a RecordingTmuxRunner, and an optional FakePTYSpawner.
type Agent struct {
	AgentID   string
	ProjectID string
	Runner    *RecordingTmuxRunner
	PTYs      *FakePTYSpawner

	ws       *FaultWSDialer
	admitted chan *conduitv1.Welcome

	mu        sync.Mutex
	lastWel   *conduitv1.Welcome
	rpcPaths  []string
	rpcCount  atomic.Int64
	kickCount atomic.Int64

	stopOnce sync.Once
	stopFn   func() error
}

// StartAgent starts a fake agent dialing opts.HubURL. Call WaitAdmitted to
// wait for its first session admission, and Close to stop it cleanly.
func StartAgent(opts AgentOptions) (*Agent, error) {
	runner := &RecordingTmuxRunner{}
	wsDialer := NewFaultWSDialer()
	fa := &Agent{
		AgentID:   opts.AgentID,
		ProjectID: opts.ProjectID,
		Runner:    runner,
		ws:        wsDialer,
		admitted:  make(chan *conduitv1.Welcome, 8),
	}

	ctl := control.New(control.Options{
		KickTokenRefresh: func() { fa.kickCount.Add(1) },
		Deliver: &control.DeliverOptions{
			Runner:            runner,
			ReadyPollInterval: 10 * time.Millisecond,
		},
	})
	baseRPC := control.RPCHandler(ctl)
	countedRPC := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		fa.rpcCount.Add(1)
		fa.mu.Lock()
		fa.rpcPaths = append(fa.rpcPaths, req.GetPath())
		fa.mu.Unlock()
		return baseRPC.HandleRPC(ctx, req)
	})

	// Setting opts.Session.RPCHandler replaces the default control.Handler
	// (deliver-message, rotate-token, and RPCCount/RPCPaths/KickCount stop
	// working).
	sessCfg := opts.Session
	if sessCfg.RPCHandler == nil {
		sessCfg.RPCHandler = countedRPC
	}

	backoff := opts.Backoff
	if backoff == nil {
		backoff = &conduit.Backoff{Base: 50 * time.Millisecond, Max: 500 * time.Millisecond, Rand: func(int64) int64 { return 0 }}
	}

	spawner := opts.SpawnPTY
	noPTY := false
	if spawner == nil {
		if opts.EnablePTY {
			fa.PTYs = NewFakePTYSpawner()
			spawner = fa.PTYs.Spawn
		} else {
			noPTY = true
		}
	}

	a, err := sconduit.New(sconduit.Options{
		HubURL:     opts.HubURL,
		AgentID:    opts.AgentID,
		ProjectID:  opts.ProjectID,
		LaunchID:   opts.LaunchID,
		Token:      opts.Token,
		HTTPClient: opts.HTTPClient,
		WS:         wsDialer.Dialer(),
		DialLocal:  opts.DialLocal,
		Policy:     opts.Policy,
		SpawnPTY:   spawner,
		NoPTY:      noPTY,
		Clock:      opts.Clock,
		OnSession: func(w *conduitv1.Welcome) {
			fa.mu.Lock()
			fa.lastWel = w
			fa.mu.Unlock()
			select {
			case fa.admitted <- w:
			default:
			}
			if opts.OnSession != nil {
				opts.OnSession(w)
			}
		},
		Backoff:   backoff,
		Session:   sessCfg,
		RPCRoutes: ctl.Routes(),
		Features:  ctl.Features(),
	})
	if err != nil {
		return nil, fmt.Errorf("conduittest: sconduit.New(%s): %w", opts.AgentID, err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = a.Run(runCtx)
	}()

	fa.stopFn = func() error {
		var stopErr error
		fa.stopOnce.Do(func() {
			wsDialer.Restore()
			cancel()
			wsDialer.CloseCurrent()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				stopErr = fmt.Errorf("conduittest: fake agent %s did not stop within 10s", opts.AgentID)
			}
		})
		return stopErr
	}
	return fa, nil
}

// StartAgents starts N fake agents concurrently and waits for all of them to
// be admitted before returning. If any agent fails to start or be admitted
// before ctx expires, all started agents are closed and an error is returned.
func StartAgents(ctx context.Context, opts []AgentOptions) ([]*Agent, error) {
	agents := make([]*Agent, len(opts))
	errs := make([]error, len(opts))
	var wg sync.WaitGroup
	for i, o := range opts {
		wg.Add(1)
		go func(idx int, opt AgentOptions) {
			defer wg.Done()
			a, err := StartAgent(opt)
			if err != nil {
				errs[idx] = err
				return
			}
			agents[idx] = a
			if _, err := a.WaitAdmitted(ctx); err != nil {
				errs[idx] = fmt.Errorf("agent %s admission: %w", opt.AgentID, err)
			}
		}(i, o)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			for _, a := range agents {
				if a != nil {
					_ = a.Close()
				}
			}
			return nil, err
		}
	}
	return agents, nil
}

// WaitAdmitted waits for the next Welcome from the relay or ctx expiration.
func (a *Agent) WaitAdmitted(ctx context.Context) (*conduitv1.Welcome, error) {
	select {
	case w := <-a.admitted:
		return w, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("fake agent %s session not admitted: %w", a.AgentID, ctx.Err())
	}
}

// SessionID returns the session ID from the latest Welcome.
func (a *Agent) SessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastWel.GetSessionId()
}

// RelayInstanceID returns the relay instance ID from the latest Welcome.
func (a *Agent) RelayInstanceID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastWel.GetRelayInstanceId()
}

// RPCCount returns the total number of control RPCs received by the agent.
func (a *Agent) RPCCount() int64 { return a.rpcCount.Load() }

// KickCount returns the number of rotate-token kicks received by the agent.
func (a *Agent) KickCount() int64 { return a.kickCount.Load() }

// RPCPaths returns a copy of all control RPC paths received by the agent.
func (a *Agent) RPCPaths() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.rpcPaths...)
}

// Close stops the fake agent cleanly.
func (a *Agent) Close() error { return a.stopFn() }

// CloseSession abruptly closes the agent's current WebSocket TCP connection.
func (a *Agent) CloseSession() { a.ws.CloseCurrent() }

// SilentDrop stalls reads and writes on the agent's WebSocket TCP conn without
// sending a close frame. Call Restore to resume and deliver pending frames.
func (a *Agent) SilentDrop() { a.ws.SilentDrop() }

// Restore resumes reads and writes on the agent's WebSocket TCP conn.
func (a *Agent) Restore() { a.ws.Restore() }
