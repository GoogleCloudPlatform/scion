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

package chatapp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

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

	mu            sync.RWMutex
	subscriptions map[string]bool
	configured    bool
}

// Compile-time interface checks.
var _ plugin.MessageBrokerPluginInterface = (*BrokerServer)(nil)
var _ plugin.HostCallbacksAware = (*BrokerServer)(nil)

// NewBrokerServer creates a new broker plugin server.
func NewBrokerServer(handler MessageHandler, log *slog.Logger) *BrokerServer {
	return &BrokerServer{
		handler:       handler,
		log:           log,
		subscriptions: make(map[string]bool),
	}
}

// SetHandler replaces the message handler after construction, allowing
// deferred wiring (e.g. to a notification relay created later).
func (b *BrokerServer) SetHandler(handler MessageHandler) {
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
	if b.handler != nil {
		return b.handler(ctx, topic, msg)
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
		Name:         "scion-chat-app",
		Version:      "1.0.0",
		Capabilities: []string{"chat-bridge", "notification-relay"},
	}, nil
}

// HealthCheck returns the plugin's health status.
func (b *BrokerServer) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	status := "healthy"
	msg := "chat app broker plugin operational"
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
	defer b.mu.Unlock()
	b.hostCallbacks = hc
	b.log.Info("host callbacks connected")
}

// HostCallbacks returns the host callbacks interface (for requesting subscriptions).
func (b *BrokerServer) HostCallbacks() plugin.HostCallbacks {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.hostCallbacks
}

// RequestSubscription asks the Hub to subscribe this plugin to a topic pattern.
func (b *BrokerServer) RequestSubscription(pattern string) error {
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
// The Hub's plugin manager connects to this server as a self-managed plugin.
//
// We use goplugin.RPCServer directly instead of goplugin.Serve() because
// Serve() is designed for plugin binaries launched by a parent process — it
// checks for the magic cookie env var and calls os.Exit(1) when it is absent.
// As a self-managed plugin we own our own process lifecycle, so we just need
// to speak the go-plugin net/rpc protocol on a listener we control.
func (b *BrokerServer) Serve(listenAddr string) (*PluginServer, error) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", listenAddr, err)
	}

	pluginMap := map[string]goplugin.Plugin{
		plugin.BrokerPluginName: &plugin.BrokerPlugin{
			Impl: b,
		},
	}

	server := &PluginServer{
		listener: listener,
		broker:   b,
		log:      b.log,
	}

	// Create dummy stdout/stderr readers for the go-plugin stream protocol.
	// Self-managed plugins don't pipe stdio to the host, so these are
	// never-closing readers that the stream copiers will block on harmlessly.
	stdoutR, _ := io.Pipe()
	stderrR, _ := io.Pipe()

	doneCh := make(chan struct{})
	rpcServer := &goplugin.RPCServer{
		Plugins: pluginMap,
		Stdout:  stdoutR,
		Stderr:  stderrR,
		DoneCh:  doneCh,
	}

	go rpcServer.Serve(listener)

	b.log.Info("broker plugin RPC server started", "address", listenAddr)
	server.addr = listener.Addr().String()

	return server, nil
}

// PluginServer wraps the running plugin RPC server.
type PluginServer struct {
	listener net.Listener
	broker   *BrokerServer
	addr     string
	log      *slog.Logger
}

// Addr returns the address the server is listening on.
func (s *PluginServer) Addr() string {
	return s.addr
}

// Close shuts down the plugin server.
func (s *PluginServer) Close() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}
