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

package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net/rpc"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	goplugin "github.com/hashicorp/go-plugin"
)

// BrokerPluginName is the name used to dispense broker plugins via go-plugin.
const BrokerPluginName = "broker"

// hostCallbacksConfigKey is the config map key used to signal that the host
// supports the HostCallbacks reverse channel. Set to "true" by the manager
// when HostCallbacks are available.
const hostCallbacksConfigKey = "_host_callbacks"

// --- RPC argument/response types ---

// ConfigureArgs holds arguments for the Configure RPC call.
type ConfigureArgs struct {
	Config map[string]string
}

// PublishArgs holds arguments for the Publish RPC call.
type PublishArgs struct {
	Topic string
	Msg   *messages.StructuredMessage
}

// SubscribeArgs holds arguments for the Subscribe RPC call.
type SubscribeArgs struct {
	Pattern string
}

// UnsubscribeArgs holds arguments for the Unsubscribe RPC call.
type UnsubscribeArgs struct {
	Pattern string
}

// GetInfoResponse holds the response from GetInfo RPC call.
type GetInfoResponse struct {
	Info PluginInfo
}

// HealthCheckResponse holds the response from HealthCheck RPC call.
type HealthCheckResponse struct {
	Status HealthStatus
}

// HealthStatus represents the runtime health of a plugin.
type HealthStatus struct {
	// Status is the overall health: "healthy", "degraded", or "unhealthy".
	Status string

	// Message is a human-readable description of the current state.
	Message string

	// Details contains plugin-specific health details (e.g., connection state,
	// last send/receive timestamps, buffer utilization).
	Details map[string]string
}

// hostCallbacksMuxID is the well-known MuxBroker stream ID used to establish
// the reverse RPC channel for host callbacks. The host accepts on this ID
// and the plugin dials it during Configure.
const hostCallbacksMuxID uint32 = 1

// --- Host callback interface (host → plugin reverse channel) ---

// HostCallbacks is an interface the plugin can call back to the host.
// It allows plugins to dynamically request or cancel subscriptions on the
// host's message broker. Provided to the plugin during Configure via the
// MuxBroker reverse channel.
type HostCallbacks interface {
	RequestSubscription(pattern string) error
	CancelSubscription(pattern string) error
}

// HostCallbacksAware is an optional interface that plugin implementations
// can implement to receive host callbacks. If a plugin's Impl satisfies
// this interface, the RPC server will inject a HostCallbacks instance
// after the reverse channel is established during Configure.
type HostCallbacksAware interface {
	SetHostCallbacks(HostCallbacks)
}

// HostCallbacksRPCServer wraps a HostCallbacks implementation to serve
// RPC requests from the plugin process. This runs in the host process.
type HostCallbacksRPCServer struct {
	Impl HostCallbacks
}

func (s *HostCallbacksRPCServer) RequestSubscription(args *SubscribeArgs, _ *struct{}) error {
	return s.Impl.RequestSubscription(args.Pattern)
}

func (s *HostCallbacksRPCServer) CancelSubscription(args *UnsubscribeArgs, _ *struct{}) error {
	return s.Impl.CancelSubscription(args.Pattern)
}

// HostCallbacksRPCClient implements HostCallbacks by making RPC calls
// to the host process. This runs in the plugin process.
type HostCallbacksRPCClient struct {
	client *rpc.Client
}

func (c *HostCallbacksRPCClient) RequestSubscription(pattern string) error {
	return c.client.Call("Plugin.RequestSubscription", &SubscribeArgs{Pattern: pattern}, &struct{}{})
}

func (c *HostCallbacksRPCClient) CancelSubscription(pattern string) error {
	return c.client.Call("Plugin.CancelSubscription", &UnsubscribeArgs{Pattern: pattern}, &struct{}{})
}

// --- Plugin interface (implemented by the plugin binary) ---

// MessageBrokerPluginInterface defines the methods that a broker plugin must implement.
// This is the interface that plugin authors implement on the plugin side.
//
// Subscribe pattern conventions:
//
// The pattern parameter uses NATS-style wildcards ("*" matches one token,
// ">" matches the remainder). When the host calls Subscribe(">") or
// Subscribe("*"), this means "start all inbound delivery." Plugins that
// operate on non-pub/sub transports (WebSocket streams, webhooks, polling
// APIs) and do not support topic filtering should accept any pattern and
// start their global listener. The pattern is a hint — plugins may ignore
// it when their transport does not support filtering.
type MessageBrokerPluginInterface interface {
	Configure(config map[string]string) error
	Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error
	Subscribe(pattern string) error
	Unsubscribe(pattern string) error
	Close() error
	GetInfo() (*PluginInfo, error)
	// HealthCheck returns the runtime health of the plugin.
	// Plugins that do not support health checks may return a nil HealthStatus.
	// The host gracefully handles plugins that do not implement this method
	// (pre-HealthCheck plugins) by returning a default "unknown" status.
	HealthCheck() (*HealthStatus, error)
}

// --- go-plugin Plugin definition ---

// BrokerPlugin implements hashicorp/go-plugin's Plugin interface for broker plugins.
// It defines how to create the RPC client and server for broker plugin communication.
type BrokerPlugin struct {
	// Impl is set only on the plugin side (the server).
	Impl MessageBrokerPluginInterface

	// HostCallbacks is set on the host side only. When non-nil, the host
	// serves a reverse RPC channel via MuxBroker so the plugin can call
	// RequestSubscription / CancelSubscription back to the host.
	HostCallbacks HostCallbacks
}

func (p *BrokerPlugin) Server(broker *goplugin.MuxBroker) (interface{}, error) {
	return &BrokerRPCServer{Impl: p.Impl, muxBroker: broker}, nil
}

func (p *BrokerPlugin) Client(broker *goplugin.MuxBroker, c *rpc.Client) (interface{}, error) {
	if p.HostCallbacks != nil {
		// Start serving host callbacks on the MuxBroker reverse channel.
		// The plugin will Dial this channel during Configure.
		slog.Debug("starting host callbacks AcceptAndServe", "mux_id", hostCallbacksMuxID)
		go broker.AcceptAndServe(hostCallbacksMuxID, &HostCallbacksRPCServer{Impl: p.HostCallbacks})
	}
	return &BrokerRPCClient{client: c, hostCallbacksAvailable: p.HostCallbacks != nil}, nil
}

// --- RPC Server (runs in the plugin process) ---

// BrokerRPCServer wraps a MessageBrokerPluginInterface to serve RPC requests.
// This runs inside the plugin binary.
type BrokerRPCServer struct {
	Impl      MessageBrokerPluginInterface
	muxBroker *goplugin.MuxBroker
}

func (s *BrokerRPCServer) Configure(args *ConfigureArgs, _ *struct{}) error {
	// If the host indicated that callbacks are available, establish the
	// reverse RPC channel and inject it into the plugin implementation.
	if args.Config != nil && args.Config[hostCallbacksConfigKey] == "true" && s.muxBroker != nil {
		conn, err := s.muxBroker.Dial(hostCallbacksMuxID)
		if err != nil {
			slog.Warn("failed to dial host callbacks reverse channel",
				"error", err, "mux_id", hostCallbacksMuxID)
		} else {
			callbacks := &HostCallbacksRPCClient{client: rpc.NewClient(conn)}
			if aware, ok := s.Impl.(HostCallbacksAware); ok {
				aware.SetHostCallbacks(callbacks)
			}
		}
	} else if args.Config != nil && args.Config[hostCallbacksConfigKey] == "true" && s.muxBroker == nil {
		slog.Warn("host callbacks requested but muxBroker is nil on plugin side")
	}
	return s.Impl.Configure(args.Config)
}

func (s *BrokerRPCServer) Publish(args *PublishArgs, _ *struct{}) error {
	return s.Impl.Publish(context.Background(), args.Topic, args.Msg)
}

func (s *BrokerRPCServer) Subscribe(args *SubscribeArgs, _ *struct{}) error {
	return s.Impl.Subscribe(args.Pattern)
}

func (s *BrokerRPCServer) Unsubscribe(args *UnsubscribeArgs, _ *struct{}) error {
	return s.Impl.Unsubscribe(args.Pattern)
}

func (s *BrokerRPCServer) Close(_ struct{}, _ *struct{}) error {
	return s.Impl.Close()
}

func (s *BrokerRPCServer) GetInfo(_ struct{}, resp *GetInfoResponse) error {
	info, err := s.Impl.GetInfo()
	if err != nil {
		return err
	}
	if info != nil {
		resp.Info = *info
	}
	return nil
}

func (s *BrokerRPCServer) HealthCheck(_ struct{}, resp *HealthCheckResponse) error {
	status, err := s.Impl.HealthCheck()
	if err != nil {
		return err
	}
	if status != nil {
		resp.Status = *status
	}
	return nil
}

// --- RPC Client (runs in the host process) ---

// BrokerRPCClient implements MessageBrokerPluginInterface by making RPC calls
// to the plugin process. This is used on the host side.
type BrokerRPCClient struct {
	client *rpc.Client
	// hostCallbacksAvailable indicates that the host started a reverse RPC
	// channel for HostCallbacks. The manager reads this flag to include
	// the _host_callbacks key in the config map passed to Configure.
	hostCallbacksAvailable bool
}

// NewBrokerRPCClient creates a BrokerRPCClient wrapping the given RPC client.
// This is primarily used by integration tests that set up their own RPC server.
func NewBrokerRPCClient(client *rpc.Client) *BrokerRPCClient {
	return &BrokerRPCClient{client: client}
}

func (c *BrokerRPCClient) Configure(config map[string]string) error {
	return c.client.Call("Plugin.Configure", &ConfigureArgs{Config: config}, &struct{}{})
}

func (c *BrokerRPCClient) Publish(_ context.Context, topic string, msg *messages.StructuredMessage) error {
	return c.client.Call("Plugin.Publish", &PublishArgs{Topic: topic, Msg: msg}, &struct{}{})
}

func (c *BrokerRPCClient) Subscribe(pattern string) error {
	return c.client.Call("Plugin.Subscribe", &SubscribeArgs{Pattern: pattern}, &struct{}{})
}

func (c *BrokerRPCClient) Unsubscribe(pattern string) error {
	return c.client.Call("Plugin.Unsubscribe", &UnsubscribeArgs{Pattern: pattern}, &struct{}{})
}

func (c *BrokerRPCClient) Close() error {
	return c.client.Call("Plugin.Close", struct{}{}, &struct{}{})
}

func (c *BrokerRPCClient) GetInfo() (*PluginInfo, error) {
	var resp GetInfoResponse
	err := c.client.Call("Plugin.GetInfo", struct{}{}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp.Info, nil
}

// HealthCheck returns the runtime health of the plugin.
// If the plugin does not implement HealthCheck (older protocol), a default
// "unknown" status is returned instead of an error.
func (c *BrokerRPCClient) HealthCheck() (*HealthStatus, error) {
	var resp HealthCheckResponse
	err := c.client.Call("Plugin.HealthCheck", struct{}{}, &resp)
	if err != nil {
		// Gracefully handle plugins that don't implement HealthCheck.
		// net/rpc returns an error for unknown methods.
		return &HealthStatus{
			Status:  "unknown",
			Message: "plugin does not support health checks",
		}, nil
	}
	return &resp.Status, nil
}

// --- Host-side adapter: wraps BrokerRPCClient as broker.MessageBroker ---

// BrokerPluginAdapter wraps a BrokerRPCClient to satisfy the eventbus.EventBus interface.
// Subscribe's EventHandler callback is not forwarded to the plugin — inbound messages
// arrive via the hub API instead (see broker-plugins.md design doc).
type BrokerPluginAdapter struct {
	rpcClient *BrokerRPCClient
	mu        sync.Mutex
	subs      map[string]*pluginSubscription
}

// NewBrokerPluginAdapter creates a new adapter wrapping the given RPC client.
func NewBrokerPluginAdapter(client *BrokerRPCClient) *BrokerPluginAdapter {
	return &BrokerPluginAdapter{
		rpcClient: client,
		subs:      make(map[string]*pluginSubscription),
	}
}

func (a *BrokerPluginAdapter) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	return a.rpcClient.Publish(ctx, topic, msg)
}

// Subscribe tells the plugin to start listening on the external broker for the given pattern.
// The handler callback is stored locally but not forwarded — inbound delivery happens
// via the hub API endpoint (POST /api/v1/broker/inbound).
func (a *BrokerPluginAdapter) Subscribe(pattern string, handler eventbus.EventHandler) (eventbus.Subscription, error) {
	if err := a.rpcClient.Subscribe(pattern); err != nil {
		return nil, fmt.Errorf("plugin subscribe failed: %w", err)
	}
	sub := &pluginSubscription{
		adapter: a,
		pattern: pattern,
	}
	a.mu.Lock()
	a.subs[pattern] = sub
	a.mu.Unlock()
	return sub, nil
}

func (a *BrokerPluginAdapter) Close() error {
	return a.rpcClient.Close()
}

// unsubscribePattern sends the RPC unsubscribe call and removes the pattern
// from the adapter's tracking map. Both pluginSubscription and
// reconnectingSub use this to avoid leaking entries in the subs map.
func (a *BrokerPluginAdapter) unsubscribePattern(pattern string) error {
	err := a.rpcClient.Unsubscribe(pattern)
	a.mu.Lock()
	delete(a.subs, pattern)
	a.mu.Unlock()
	return err
}

// pluginSubscription implements eventbus.Subscription for plugin brokers.
type pluginSubscription struct {
	adapter *BrokerPluginAdapter
	pattern string
}

func (s *pluginSubscription) Unsubscribe() error {
	return s.adapter.unsubscribePattern(s.pattern)
}

// --- Reconnecting adapter for self-managed broker plugins ---

// reconnectingBrokerAdapter wraps a BrokerPluginAdapter for self-managed broker
// plugins and automatically reconnects when RPC calls fail. This handles the
// case where the external plugin process restarts while the Hub is still running.
type reconnectingBrokerAdapter struct {
	manager    *Manager
	name       string
	logger     *slog.Logger
	mu         sync.Mutex
	current    *BrokerPluginAdapter
	activeSubs map[string]eventbus.EventHandler // tracked for re-subscribe after reconnect
	closed     bool
}

func newReconnectingBrokerAdapter(manager *Manager, name string, initial *BrokerPluginAdapter, logger *slog.Logger) *reconnectingBrokerAdapter {
	return &reconnectingBrokerAdapter{
		manager:    manager,
		name:       name,
		logger:     logger.With("component", "reconnecting-broker", "plugin", name),
		current:    initial,
		activeSubs: make(map[string]eventbus.EventHandler),
	}
}

// tryReconnect re-establishes the connection to the self-managed plugin and
// re-subscribes to all active subscription patterns.
func (a *reconnectingBrokerAdapter) tryReconnect() error {
	a.logger.Info("Attempting reconnect to broker plugin")

	if err := a.manager.Reconnect(PluginTypeBroker, a.name); err != nil {
		return fmt.Errorf("reconnect failed: %w", err)
	}

	raw, err := a.manager.Get(PluginTypeBroker, a.name)
	if err != nil {
		return fmt.Errorf("get after reconnect failed: %w", err)
	}
	rpcClient, ok := raw.(*BrokerRPCClient)
	if !ok {
		return fmt.Errorf("reconnected plugin is not a BrokerRPCClient")
	}

	a.current = NewBrokerPluginAdapter(rpcClient)

	// Re-establish subscriptions on the new connection.
	for pattern, handler := range a.activeSubs {
		if _, err := a.current.Subscribe(pattern, handler); err != nil {
			a.logger.Warn("Failed to re-subscribe after reconnect",
				"pattern", pattern, "error", err)
		}
	}

	a.logger.Info("Successfully reconnected to broker plugin",
		"resubscribed", len(a.activeSubs))
	return nil
}

func (a *reconnectingBrokerAdapter) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return fmt.Errorf("adapter is closed")
	}

	err := a.current.Publish(ctx, topic, msg)
	if err == nil {
		return nil
	}

	a.logger.Warn("Publish failed, attempting reconnect", "topic", topic, "error", err)
	if reconnErr := a.tryReconnect(); reconnErr != nil {
		return fmt.Errorf("publish failed: %w (reconnect also failed: %v)", err, reconnErr)
	}
	return a.current.Publish(ctx, topic, msg)
}

func (a *reconnectingBrokerAdapter) Subscribe(pattern string, handler eventbus.EventHandler) (eventbus.Subscription, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, fmt.Errorf("adapter is closed")
	}

	sub, err := a.current.Subscribe(pattern, handler)
	if err == nil {
		a.activeSubs[pattern] = handler
		return &reconnectingSub{adapter: a, pattern: pattern}, nil
	}

	a.logger.Warn("Subscribe failed, attempting reconnect", "pattern", pattern, "error", err)
	if reconnErr := a.tryReconnect(); reconnErr != nil {
		return nil, fmt.Errorf("subscribe failed: %w (reconnect also failed: %v)", err, reconnErr)
	}
	sub, err = a.current.Subscribe(pattern, handler)
	if err != nil {
		return nil, err
	}
	a.activeSubs[pattern] = handler
	_ = sub // tracked by current adapter internally
	return &reconnectingSub{adapter: a, pattern: pattern}, nil
}

func (a *reconnectingBrokerAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return a.current.Close()
}

// reconnectingSub implements eventbus.Subscription and tracks unsubscribes in
// the reconnecting adapter's activeSubs map.
type reconnectingSub struct {
	adapter *reconnectingBrokerAdapter
	pattern string
}

func (s *reconnectingSub) Unsubscribe() error {
	s.adapter.mu.Lock()
	defer s.adapter.mu.Unlock()
	delete(s.adapter.activeSubs, s.pattern)
	// Best-effort unsubscribe on current connection; if it's dead, the
	// subscription is already gone. Uses unsubscribePattern to also clean
	// up the BrokerPluginAdapter's subs map, preventing leaked entries.
	_ = s.adapter.current.unsubscribePattern(s.pattern)
	return nil
}
