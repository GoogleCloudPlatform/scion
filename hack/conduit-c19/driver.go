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

// Package c19driver implements the Criterion 19 Conduit load profile shared by
// the CI functional smoke test (pkg/hub/conduit_ha_c19_test.go) and the live
// load-driver binary (hack/conduit-c19/cmd/conduit-c19). It is test/validation
// tooling and is not shipped in production binaries.
package c19driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/conduittest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// LargeBodyBytes is the 512 KiB payload size exercised by the C19 profile.
const LargeBodyBytes = 512 * 1024

// AgentTarget identifies one fake agent to launch in the C19 load profile.
type AgentTarget struct {
	AgentID   string `json:"agent_id"`
	ProjectID string `json:"project_id"`
	LaunchID  string `json:"launch_id"`
	Token     string `json:"token"`
	HubURL    string `json:"hub_url,omitempty"`
}

// Config configures a C19 load run.
type Config struct {
	// Agents lists the N fake agents to start concurrently (e.g. N=50).
	Agents []AgentTarget
	// DefaultHubURL is used for any AgentTarget whose HubURL is empty.
	DefaultHubURL string
	// Duration is the active load phase length (default 5s).
	Duration time.Duration
	// DeliverInterval is the per-agent delivery cadence (default 2s).
	DeliverInterval time.Duration
	// LargeBodyCount is the number of 512 KiB deliver-message calls to inject
	// across the run (default 4).
	LargeBodyCount int
	// RotateTokenCount is the number of rotate-token control RPCs to inject
	// when RotateToken is non-nil (default 10).
	RotateTokenCount int
	// BrokerRPCCount is the number of broker RPCs (list/status churn) to
	// inject when BrokerRPC is non-nil (default 20).
	BrokerRPCCount int
	// EnableBulkTunnelOnAgent0 installs an in-memory bulk TCP producer on
	// Agents[0]'s DialLocal so StalledTunnel fills the stream receive window.
	EnableBulkTunnelOnAgent0 bool

	// Deliver sends one deliver-message flush to agentID with body and returns
	// the HTTP status code, outcome string, and any error. Required.
	Deliver func(ctx context.Context, agentID, body string) (status int, outcome string, err error)
	// RotateToken invokes POST /v1/control/rotate-token on agentID. Optional.
	RotateToken func(ctx context.Context, agentID string) error
	// BrokerRPC invokes one hub->broker list/status RPC. Optional.
	BrokerRPC func(ctx context.Context, seq int) error
	// StalledTunnel opens a bulk TCP tunnel stream to Agents[0] and leaves its
	// reader completely stalled until the returned close function is called.
	// Optional.
	StalledTunnel func(ctx context.Context, agentID string) (closeFn func(), err error)
}

// LatencyStats summarizes a slice of recorded durations.
type LatencyStats struct {
	Count int           `json:"count"`
	P50   time.Duration `json:"p50"`
	P99   time.Duration `json:"p99"`
	Max   time.Duration `json:"max"`
}

// SummarizeLatencies computes count, p50, p99, and max over samples.
func SummarizeLatencies(samples []time.Duration) LatencyStats {
	if len(samples) == 0 {
		return LatencyStats{}
	}
	cp := append([]time.Duration(nil), samples...)
	slices.Sort(cp)
	pct := func(p float64) time.Duration {
		idx := int(float64(len(cp)-1) * p)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return cp[idx]
	}
	return LatencyStats{
		Count: len(cp),
		P50:   pct(0.50),
		P99:   pct(0.99),
		Max:   cp[len(cp)-1],
	}
}

// Result holds counters, errors, and latencies from a C19 load run.
type Result struct {
	AgentCount              int
	DeliverAttempts         int64
	DeliverSucceeded        int64
	Deliver429s             int64
	DeliverErrors           int64
	LargeBodySucceeded      int64
	LargeBodyErrors         int64
	RotateTokenSucceeded    int64
	RotateTokenErrors       int64
	BrokerRPCSucceeded      int64
	BrokerRPCErrors         int64
	StalledBulkBytesWritten int64

	DeliverLatencies   []time.Duration
	RotateLatencies    []time.Duration
	BrokerRPCLatencies []time.Duration
	FirstError         error
}

// Run starts all configured fake agents, optionally opens a stalled bulk TCP
// tunnel on agent 0, executes the mixed deliver-message + 512 KiB + rotate-token
// + broker RPC workload for cfg.Duration, tears everything down cleanly, and
// returns the aggregated Result.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if len(cfg.Agents) == 0 {
		return nil, errors.New("c19driver: at least one agent target is required")
	}
	if cfg.Deliver == nil {
		return nil, errors.New("c19driver: Deliver callback is required")
	}
	if cfg.Duration <= 0 {
		cfg.Duration = 5 * time.Second
	}
	if cfg.DeliverInterval <= 0 {
		cfg.DeliverInterval = 2 * time.Second
	}
	if cfg.LargeBodyCount < 0 {
		cfg.LargeBodyCount = 0
	}
	if cfg.RotateTokenCount < 0 {
		cfg.RotateTokenCount = 0
	}
	if cfg.BrokerRPCCount < 0 {
		cfg.BrokerRPCCount = 0
	}

	var bulkWritten atomic.Int64
	agentOpts := make([]conduittest.AgentOptions, len(cfg.Agents))
	for i, target := range cfg.Agents {
		hubURL := target.HubURL
		if hubURL == "" {
			hubURL = cfg.DefaultHubURL
		}
		tok := target.Token
		opt := conduittest.AgentOptions{
			HubURL:    hubURL,
			AgentID:   target.AgentID,
			ProjectID: target.ProjectID,
			LaunchID:  target.LaunchID,
			Token:     func() string { return tok },
		}
		if i == 0 && cfg.EnableBulkTunnelOnAgent0 {
			opt.DialLocal = func(_ context.Context, _, _ string) (net.Conn, error) {
				serverConn, clientConn := net.Pipe()
				go func() {
					defer func() { _ = serverConn.Close() }()
					chunk := make([]byte, 16*1024)
					for j := range chunk {
						chunk[j] = byte(j)
					}
					for {
						n, err := serverConn.Write(chunk)
						if n > 0 {
							bulkWritten.Add(int64(n))
						}
						if err != nil {
							return
						}
					}
				}()
				return clientConn, nil
			}
		}
		agentOpts[i] = opt
	}

	admitCtx, admitCancel := context.WithTimeout(ctx, 30*time.Second)
	agents, err := conduittest.StartAgents(admitCtx, agentOpts)
	admitCancel()
	if err != nil {
		return nil, fmt.Errorf("c19driver: start agents: %w", err)
	}
	defer func() {
		var closeWg sync.WaitGroup
		for _, a := range agents {
			closeWg.Add(1)
			go func(ag *conduittest.Agent) {
				defer closeWg.Done()
				_ = ag.Close()
			}(a)
		}
		closeWg.Wait()
	}()

	// Open the stalled bulk tunnel stream before the load window starts so its
	// receive window fills and remains backpressured throughout the workload.
	if cfg.StalledTunnel != nil {
		closeTunnel, err := cfg.StalledTunnel(ctx, cfg.Agents[0].AgentID)
		if err != nil {
			return nil, fmt.Errorf("c19driver: open stalled tunnel: %w", err)
		}
		defer closeTunnel()

		if cfg.EnableBulkTunnelOnAgent0 {
			deadline := time.Now().Add(10 * time.Second)
			for bulkWritten.Load() < conduit.DefaultStreamWindow {
				if time.Now().After(deadline) {
					return nil, fmt.Errorf("c19driver: stalled tunnel only wrote %d bytes (< %d)", bulkWritten.Load(), conduit.DefaultStreamWindow)
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(15 * time.Millisecond):
				}
			}
		}
	}

	res := &Result{AgentCount: len(agents)}
	var (
		mu             sync.Mutex
		deliverAttempt atomic.Int64
		deliverOK      atomic.Int64
		deliver429     atomic.Int64
		deliverErr     atomic.Int64
		largeOK        atomic.Int64
		largeErr       atomic.Int64
		rotateOK       atomic.Int64
		rotateErr      atomic.Int64
		brokerOK       atomic.Int64
		brokerErr      atomic.Int64
	)
	recordErr := func(e error) {
		if e == nil {
			return
		}
		mu.Lock()
		if res.FirstError == nil {
			res.FirstError = e
		}
		mu.Unlock()
	}

	loadCtx, loadCancel := context.WithTimeout(ctx, cfg.Duration)
	defer loadCancel()

	var wg sync.WaitGroup

	// 1. Per-agent periodic delivery loops (1 delivery every DeliverInterval,
	//    staggered across agents so they spread smoothly over the window).
	for i, target := range cfg.Agents {
		wg.Add(1)
		go func(idx int, agentID string) {
			defer wg.Done()
			stagger := time.Duration(idx) * cfg.DeliverInterval / time.Duration(len(cfg.Agents))
			if stagger > 0 {
				select {
				case <-loadCtx.Done():
					return
				case <-time.After(stagger):
				}
			}
			seq := 0
			sendOne := func() {
				callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				start := time.Now()
				deliverAttempt.Add(1)
				status, _, err := cfg.Deliver(callCtx, agentID, fmt.Sprintf("c19-agent-%d-msg-%d", idx, seq))
				elapsed := time.Since(start)
				seq++
				if status == http.StatusTooManyRequests {
					deliver429.Add(1)
				}
				if err != nil || status != http.StatusOK {
					deliverErr.Add(1)
					if err != nil {
						recordErr(fmt.Errorf("deliver agent %s: %w", agentID, err))
					} else {
						recordErr(fmt.Errorf("deliver agent %s: status %d", agentID, status))
					}
					return
				}
				deliverOK.Add(1)
				mu.Lock()
				res.DeliverLatencies = append(res.DeliverLatencies, elapsed)
				mu.Unlock()
			}
			sendOne()
			tick := time.NewTicker(cfg.DeliverInterval)
			defer tick.Stop()
			for {
				select {
				case <-loadCtx.Done():
					return
				case <-tick.C:
					sendOne()
				}
			}
		}(i, target.AgentID)
	}

	// 2. Large (512 KiB) deliver-message bodies mixed across agents.
	if cfg.LargeBodyCount > 0 {
		largePayload := strings.Repeat("X", LargeBodyBytes)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < cfg.LargeBodyCount; i++ {
				target := cfg.Agents[i%len(cfg.Agents)]
				callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				start := time.Now()
				deliverAttempt.Add(1)
				status, _, err := cfg.Deliver(callCtx, target.AgentID, largePayload)
				elapsed := time.Since(start)
				cancel()
				if status == http.StatusTooManyRequests {
					deliver429.Add(1)
				}
				if err != nil || status != http.StatusOK {
					largeErr.Add(1)
					deliverErr.Add(1)
					if err != nil {
						recordErr(fmt.Errorf("large deliver agent %s: %w", target.AgentID, err))
					} else {
						recordErr(fmt.Errorf("large deliver agent %s: status %d", target.AgentID, status))
					}
					continue
				}
				largeOK.Add(1)
				deliverOK.Add(1)
				mu.Lock()
				res.DeliverLatencies = append(res.DeliverLatencies, elapsed)
				mu.Unlock()
			}
		}()
	}

	// 3. Control RPCs (rotate-token) mixed across agents, starting with agent 0
	//    (which shares its session with the stalled bulk tunnel).
	if cfg.RotateToken != nil && cfg.RotateTokenCount > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < cfg.RotateTokenCount; i++ {
				target := cfg.Agents[i%len(cfg.Agents)]
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				start := time.Now()
				err := cfg.RotateToken(callCtx, target.AgentID)
				elapsed := time.Since(start)
				cancel()
				if err != nil {
					rotateErr.Add(1)
					recordErr(fmt.Errorf("rotate-token agent %s: %w", target.AgentID, err))
					continue
				}
				rotateOK.Add(1)
				mu.Lock()
				res.RotateLatencies = append(res.RotateLatencies, elapsed)
				mu.Unlock()
			}
		}()
	}

	// 4. Broker RPC churn (list/status) over the broker's conduit session.
	if cfg.BrokerRPC != nil && cfg.BrokerRPCCount > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < cfg.BrokerRPCCount; i++ {
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				start := time.Now()
				err := cfg.BrokerRPC(callCtx, i)
				elapsed := time.Since(start)
				cancel()
				if err != nil {
					brokerErr.Add(1)
					recordErr(fmt.Errorf("broker RPC %d: %w", i, err))
					continue
				}
				brokerOK.Add(1)
				mu.Lock()
				res.BrokerRPCLatencies = append(res.BrokerRPCLatencies, elapsed)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	res.DeliverAttempts = deliverAttempt.Load()
	res.DeliverSucceeded = deliverOK.Load()
	res.Deliver429s = deliver429.Load()
	res.DeliverErrors = deliverErr.Load()
	res.LargeBodySucceeded = largeOK.Load()
	res.LargeBodyErrors = largeErr.Load()
	res.RotateTokenSucceeded = rotateOK.Load()
	res.RotateTokenErrors = rotateErr.Load()
	res.BrokerRPCSucceeded = brokerOK.Load()
	res.BrokerRPCErrors = brokerErr.Load()
	res.StalledBulkBytesWritten = bulkWritten.Load()
	return res, nil
}
