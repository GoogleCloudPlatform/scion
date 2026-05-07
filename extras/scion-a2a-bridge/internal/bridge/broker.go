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

package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	goplugin "github.com/hashicorp/go-plugin"
)

// MessageHandler is called when a message is received from the Hub via the broker plugin.
type MessageHandler func(ctx context.Context, topic string, msg *messages.StructuredMessage) error

// BrokerServer implements the MessageBrokerPluginInterface and serves it via go-plugin RPC.
type BrokerServer struct {
	handler       MessageHandler
	hostCallbacks plugin.HostCallbacks
	log           *slog.Logger
	shutdownCtx   context.Context

	mu            sync.RWMutex
	subscriptions map[string]bool
	configured    bool
}

var _ plugin.MessageBrokerPluginInterface = (*BrokerServer)(nil)
var _ plugin.HostCallbacksAware = (*BrokerServer)(nil)

// NewBrokerServer creates a new broker plugin server.
func NewBrokerServer(handler MessageHandler, log *slog.Logger, shutdownCtx context.Context) *BrokerServer {
	return &BrokerServer{
		handler:       handler,
		log:           log,
		shutdownCtx:   shutdownCtx,
		subscriptions: make(map[string]bool),
	}
}

// SetHandler replaces the message handler after construction.
func (b *BrokerServer) SetHandler(handler MessageHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handler = handler
}

// Configure is called by the Hub plugin manager during initialization.
func (b *BrokerServer) Configure(config map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.configured = true
	b.log.Info("broker plugin configured", "config_keys", len(config))
	return nil
}

// Publish receives a message from the Hub and routes it to the handler.
func (b *BrokerServer) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.log.Debug("received message via broker",
		"topic", topic,
		"sender", msg.Sender,
		"type", msg.Type,
	)
	b.mu.RLock()
	h := b.handler
	b.mu.RUnlock()
	if h != nil {
		return h(ctx, topic, msg)
	}
	return nil
}

// Subscribe registers a topic pattern for receiving messages.
func (b *BrokerServer) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscriptions[pattern] = true
	b.log.Info("subscribed to pattern", "pattern", pattern)
	return nil
}

// Unsubscribe removes a topic pattern subscription.
func (b *BrokerServer) Unsubscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscriptions, pattern)
	b.log.Info("unsubscribed from pattern", "pattern", pattern)
	return nil
}

// Close gracefully shuts down the broker plugin.
func (b *BrokerServer) Close() error {
	b.log.Info("broker plugin closing")
	return nil
}

// GetInfo returns plugin metadata.
func (b *BrokerServer) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:         "scion-a2a-bridge",
		Version:      "1.0.0",
		Capabilities: []string{"a2a-bridge"},
	}, nil
}

// HealthCheck returns the plugin's health status.
func (b *BrokerServer) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	status := "healthy"
	msg := "a2a bridge broker plugin operational"
	if !b.configured {
		status = "degraded"
		msg = "not yet configured by hub"
	}

	return &plugin.HealthStatus{
		Status:  status,
		Message: msg,
	}, nil
}

// SetHostCallbacks is called by the go-plugin framework to provide the reverse channel.
func (b *BrokerServer) SetHostCallbacks(hc plugin.HostCallbacks) {
	b.mu.Lock()
	b.hostCallbacks = hc
	subs := make([]string, 0, len(b.subscriptions))
	for p := range b.subscriptions {
		subs = append(subs, p)
	}
	b.mu.Unlock()

	b.log.Info("host callbacks connected")

	go func() {
		for _, pattern := range subs {
			for i := 0; i < 10; i++ {
				err := hc.RequestSubscription(pattern)
				if err == nil {
					b.log.Info("subscribed to deferred pattern", "pattern", pattern)
					break
				}
				if err.Error() == "host callbacks not yet available" {
					b.log.Debug("host callbacks not ready yet, retrying...", "pattern", pattern, "attempt", i+1)
					select {
					case <-time.After(time.Second):
					case <-b.shutdownCtx.Done():
						return
					}
				} else {
					b.log.Error("failed to request deferred subscription", "pattern", pattern, "error", err)
					break
				}
			}
		}
	}()
}

// HostCallbacks returns the host callbacks interface.
func (b *BrokerServer) HostCallbacks() plugin.HostCallbacks {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.hostCallbacks
}

// RequestSubscription asks the Hub to subscribe this plugin to a topic pattern.
// Skips the remote RPC if the pattern is already subscribed locally.
func (b *BrokerServer) RequestSubscription(pattern string) error {
	b.mu.Lock()
	already := b.subscriptions[pattern]
	b.subscriptions[pattern] = true
	b.mu.Unlock()

	if already {
		return nil
	}

	hc := b.HostCallbacks()
	if hc == nil {
		return fmt.Errorf("host callbacks not available")
	}
	return hc.RequestSubscription(pattern)
}

// CancelSubscription asks the Hub to cancel a subscription.
func (b *BrokerServer) CancelSubscription(pattern string) error {
	hc := b.HostCallbacks()
	if hc == nil {
		return fmt.Errorf("host callbacks not available")
	}
	return hc.CancelSubscription(pattern)
}

// Serve starts the go-plugin RPC server on the given address.
// If allowRemote is false, the address must bind to a loopback interface.
func (b *BrokerServer) Serve(listenAddr string, allowRemote bool) (*PluginServer, error) {
	if !allowRemote {
		host, _, err := net.SplitHostPort(listenAddr)
		if err != nil {
			return nil, fmt.Errorf("invalid plugin listen address %q: %w", listenAddr, err)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			return nil, fmt.Errorf("plugin.listen_address %q binds to all interfaces; use localhost or set plugin.allow_remote: true", listenAddr)
		}
		if host != "localhost" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return nil, fmt.Errorf("plugin.listen_address %q is not loopback; set plugin.allow_remote: true to override", listenAddr)
			}
		}
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", listenAddr, err)
	}

	pluginMap := map[string]goplugin.Plugin{
		plugin.BrokerPluginName: &plugin.BrokerPlugin{
			Impl: b,
		},
	}

	// Reader ends are never explicitly closed — they leak for the process lifetime.
	// Acceptable: one BrokerServer per process, and go-plugin holds references internally.
	// Closing the writer side immediately yields EOF on the reader, which is correct:
	// go-plugin's RPCServer in server mode uses the listener for RPC transport, not
	// these stdio pipes. The pipes satisfy the interface but carry no application data.
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	stdoutW.Close()
	stderrW.Close()

	doneCh := make(chan struct{})
	rpcServer := &goplugin.RPCServer{
		Plugins: pluginMap,
		Stdout:  stdoutR,
		Stderr:  stderrR,
		DoneCh:  doneCh,
	}

	go rpcServer.Serve(listener)

	server := &PluginServer{
		listener:  listener,
		rpcDoneCh: doneCh,
		log:       b.log,
		addr:      listener.Addr().String(),
	}

	b.log.Info("broker plugin RPC server started", "address", listenAddr)

	return server, nil
}

// PluginServer wraps the running plugin RPC server.
type PluginServer struct {
	listener  net.Listener
	rpcDoneCh chan struct{}
	addr      string
	log       *slog.Logger
	closeOnce sync.Once
}

// Addr returns the address the server is listening on.
func (s *PluginServer) Addr() string {
	return s.addr
}

// Close shuts down the plugin server. Does not wait for in-flight RPCs spawned
// by go-plugin to drain; the parent Bridge.Shutdown handles goroutine drainage.
func (s *PluginServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.listener != nil {
			err = s.listener.Close()
		}
		if s.rpcDoneCh != nil {
			close(s.rpcDoneCh)
		}
	})
	return err
}
