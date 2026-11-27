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
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// BrokerOptions configures a fake Conduit broker.
type BrokerOptions struct {
	HubURL       string
	BrokerID     string
	BrokerSecret []byte
	Version      string
	Incarnation  string
	RPCHandler   conduit.RPCHandler
	Rand         func(int64) int64
}

// Broker is a fake broker wired with the real runtimebroker.ConduitDialer and
// a real HMAC-authenticated hubclient.Client (the same HTTP client path
// hubConduitDeliverer calls).
type Broker struct {
	BrokerID  string
	Dialer    *runtimebroker.ConduitDialer
	HubClient hubclient.Client
	ws        *FaultWSDialer

	stopOnce sync.Once
	stopFn   func() error
}

// StartBroker starts a fake broker dialing opts.HubURL. Call WaitHealthy to
// wait until the conduit command path becomes healthy, and Close to stop it.
func StartBroker(opts BrokerOptions) (*Broker, error) {
	version := opts.Version
	if version == "" {
		version = "test"
	}
	incarnation := opts.Incarnation
	if incarnation == "" {
		incarnation = "broker-inc-" + uuid.NewString()[:8]
	}
	randFn := opts.Rand
	if randFn == nil {
		randFn = func(int64) int64 { return 0 }
	}

	wsDialer := NewFaultWSDialer()
	d, err := runtimebroker.NewConduitDialer(runtimebroker.ConduitDialConfig{
		HubEndpoint: opts.HubURL,
		BrokerID:    opts.BrokerID,
		SecretKey:   opts.BrokerSecret,
		Version:     version,
		Incarnation: incarnation,
		RPCHandler:  opts.RPCHandler,
		WS:          wsDialer.Dialer(),
		Rand:        randFn,
	})
	if err != nil {
		return nil, fmt.Errorf("conduittest: NewConduitDialer: %w", err)
	}

	hc, err := hubclient.New(opts.HubURL, hubclient.WithHMACAuth(opts.BrokerID, opts.BrokerSecret))
	if err != nil {
		return nil, fmt.Errorf("conduittest: hubclient.New: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = d.Run(runCtx)
	}()

	fb := &Broker{
		BrokerID:  opts.BrokerID,
		Dialer:    d,
		HubClient: hc,
		ws:        wsDialer,
	}
	fb.stopFn = func() error {
		var stopErr error
		fb.stopOnce.Do(func() {
			wsDialer.Restore()
			cancel()
			wsDialer.CloseCurrent()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				stopErr = fmt.Errorf("conduittest: fake broker %s did not stop within 10s", opts.BrokerID)
			}
		})
		return stopErr
	}
	return fb, nil
}

// WaitHealthy polls until the broker's conduit command path reaches healthy
// state or ctx expires.
func (b *Broker) WaitHealthy(ctx context.Context) error {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if b.Dialer.CommandPathState() == api.BrokerHealthHealthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("broker %s command path state = %q: %w", b.BrokerID, b.Dialer.CommandPathState(), ctx.Err())
		case <-tick.C:
		}
	}
}

// DeliverMessage sends a deliver-message flush over the real broker->hub HTTP
// endpoint (POST /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/deliver).
func (b *Broker) DeliverMessage(ctx context.Context, agentID string, req *agentcontrol.DeliverRequest) (int, agentcontrol.DeliverResponse, error) {
	return b.HubClient.RuntimeBrokers().DeliverMessage(ctx, b.BrokerID, agentID, req)
}

// Close stops the fake broker cleanly.
func (b *Broker) Close() error { return b.stopFn() }

// CloseSession abruptly closes the broker's current WebSocket TCP connection.
func (b *Broker) CloseSession() { b.ws.CloseCurrent() }

// SilentDrop stalls reads and writes on the broker's WebSocket TCP conn without
// sending a close frame. Call Restore to resume and deliver pending frames.
func (b *Broker) SilentDrop() { b.ws.SilentDrop() }

// Restore resumes reads and writes on the broker's WebSocket TCP conn.
func (b *Broker) Restore() { b.ws.Restore() }
