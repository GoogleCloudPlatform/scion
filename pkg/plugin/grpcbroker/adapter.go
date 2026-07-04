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

package grpcbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// PerCallAuthenticator is a hook for per-call authentication (e.g., IAM tokens
// on Cloud Run). Implementations provide credentials that are attached to each
// gRPC call. This is a placeholder interface that 5E or a follow-up can implement.
type PerCallAuthenticator interface {
	// GetRequestMetadata returns metadata to attach to each RPC call.
	GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error)
	// RequireTransportSecurity indicates whether transport security is required.
	RequireTransportSecurity() bool
}

// TLSConfig holds TLS configuration for the gRPC connection.
type TLSConfig struct {
	CertFile   string
	KeyFile    string
	CAFile     string
	SkipVerify bool
}

// AdapterConfig holds configuration for creating a GRPCBrokerAdapter.
type AdapterConfig struct {
	Address   string
	TLS       *TLSConfig
	Logger    *slog.Logger
	Authenticator PerCallAuthenticator
}

// GRPCBrokerAdapter implements eventbus.EventBus by wrapping gRPC client calls
// to a remote BrokerService. It includes reconnection logic modeled on
// reconnectingBrokerAdapter: maintains an activeSubs map and re-subscribes
// after reconnect.
type GRPCBrokerAdapter struct {
	address string
	tlsCfg  *TLSConfig
	logger  *slog.Logger
	auth    PerCallAuthenticator

	mu         sync.Mutex
	conn       *grpc.ClientConn
	client     brokerv1.BrokerServiceClient
	activeSubs map[string]eventbus.EventHandler
	closed     bool

	info   *plugin.PluginInfo
	health *plugin.HealthStatus

	reconnectCallbacks []func()
}

// NewGRPCBrokerAdapter creates a new adapter that connects to the remote
// broker service at the given address. The connection is established lazily
// on the first operation.
func NewGRPCBrokerAdapter(cfg AdapterConfig) *GRPCBrokerAdapter {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &GRPCBrokerAdapter{
		address:    cfg.Address,
		tlsCfg:     cfg.TLS,
		logger:     cfg.Logger.With("component", "grpc-broker-adapter", "address", cfg.Address),
		auth:       cfg.Authenticator,
		activeSubs: make(map[string]eventbus.EventHandler),
	}
}

// OnReconnect registers a callback that is invoked after a successful
// reconnect to the remote broker (not on initial connect). Callbacks are
// called with the adapter lock released so they may call GetInfo/HealthCheck.
func (a *GRPCBrokerAdapter) OnReconnect(fn func()) {
	a.mu.Lock()
	a.reconnectCallbacks = append(a.reconnectCallbacks, fn)
	a.mu.Unlock()
}

// dialOpts returns gRPC dial options based on the adapter's TLS configuration.
// For localhost/127.0.0.1 addresses, defaults to insecure unless TLS config
// is explicitly provided. For all other addresses, requires TLS.
func (a *GRPCBrokerAdapter) dialOpts() ([]grpc.DialOption, error) {
	var opts []grpc.DialOption

	if a.tlsCfg != nil && (a.tlsCfg.CertFile != "" || a.tlsCfg.CAFile != "" || a.tlsCfg.SkipVerify) {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: a.tlsCfg.SkipVerify,
		}
		if a.tlsCfg.CertFile != "" && a.tlsCfg.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(a.tlsCfg.CertFile, a.tlsCfg.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("load client cert: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
		if a.tlsCfg.CAFile != "" {
			caCert, err := os.ReadFile(a.tlsCfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read CA file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caCert) {
				return nil, fmt.Errorf("failed to parse CA certificate")
			}
			tlsConfig.RootCAs = pool
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else if isLocalAddress(a.address) {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	}

	if a.auth != nil {
		opts = append(opts, grpc.WithPerRPCCredentials(a.auth))
	}

	return opts, nil
}

// connect establishes the gRPC connection. Caller must hold a.mu.
func (a *GRPCBrokerAdapter) connect() error {
	if a.conn != nil {
		a.conn.Close()
	}

	opts, err := a.dialOpts()
	if err != nil {
		return fmt.Errorf("dial options: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, a.address, opts...)
	if err != nil {
		return fmt.Errorf("dial %s: %w", a.address, err)
	}

	a.conn = conn
	a.client = brokerv1.NewBrokerServiceClient(conn)
	return nil
}

// ensureConnected establishes a connection if one doesn't exist. If there are
// active subscriptions and no connection, this is treated as a reconnect and
// all subscriptions are re-established on the new connection. Caller must hold a.mu.
func (a *GRPCBrokerAdapter) ensureConnected() error {
	if a.client != nil {
		return nil
	}
	if err := a.connect(); err != nil {
		return err
	}
	isReconnect := len(a.activeSubs) > 0
	if isReconnect {
		a.resubscribeAll()
		a.fireReconnectCallbacks()
	}
	return nil
}

// resubscribeAll re-establishes all active subscriptions on the current
// connection. Caller must hold a.mu.
func (a *GRPCBrokerAdapter) resubscribeAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for pattern := range a.activeSubs {
		if _, err := a.client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: pattern}); err != nil {
			a.logger.Warn("failed to re-subscribe after reconnect",
				"pattern", pattern, "error", err)
		}
	}
	a.logger.Info("re-subscribed after connection reset", "count", len(a.activeSubs))
}

// tryReconnect re-establishes the gRPC connection and re-subscribes all active
// patterns. Caller must hold a.mu.
func (a *GRPCBrokerAdapter) tryReconnect() error {
	a.logger.Info("attempting reconnect to gRPC broker")

	if err := a.connect(); err != nil {
		return fmt.Errorf("reconnect failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for pattern := range a.activeSubs {
		if _, err := a.client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: pattern}); err != nil {
			a.logger.Warn("failed to re-subscribe after reconnect",
				"pattern", pattern, "error", err)
		}
	}

	a.logger.Info("successfully reconnected to gRPC broker",
		"resubscribed", len(a.activeSubs))

	a.fireReconnectCallbacks()

	return nil
}

// fireReconnectCallbacks dispatches all registered reconnect callbacks
// on a separate goroutine so the caller's lock is never released.
// Caller must hold a.mu.
func (a *GRPCBrokerAdapter) fireReconnectCallbacks() {
	if len(a.reconnectCallbacks) == 0 {
		return
	}
	cbs := make([]func(), len(a.reconnectCallbacks))
	copy(cbs, a.reconnectCallbacks)
	go func() {
		for _, cb := range cbs {
			cb()
		}
	}()
}

// Publish sends a message to the remote broker.
func (a *GRPCBrokerAdapter) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return fmt.Errorf("adapter is closed")
	}

	if err := a.ensureConnected(); err != nil {
		return err
	}

	req := &brokerv1.PublishRequest{
		Topic:   topic,
		Message: StructuredMessageToProto(msg),
	}

	_, err := a.client.Publish(ctx, req)
	if err == nil {
		return nil
	}

	a.logger.Warn("publish failed, attempting reconnect", "topic", topic, "error", err)
	if reconnErr := a.tryReconnect(); reconnErr != nil {
		return fmt.Errorf("publish failed: %w (reconnect also failed: %v)", err, reconnErr)
	}
	if a.client == nil {
		return fmt.Errorf("publish failed: client nil after reconnect")
	}
	_, err = a.client.Publish(ctx, req)
	return err
}

// Subscribe tells the remote broker to start listening for the given pattern.
// The handler is stored locally for re-subscription after reconnect — inbound
// delivery happens via the hub API endpoint.
func (a *GRPCBrokerAdapter) Subscribe(pattern string, handler eventbus.EventHandler) (eventbus.Subscription, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, fmt.Errorf("adapter is closed")
	}

	if err := a.ensureConnected(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := a.client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: pattern})
	if err == nil {
		a.activeSubs[pattern] = handler
		return &grpcSub{adapter: a, pattern: pattern}, nil
	}

	a.logger.Warn("subscribe failed, attempting reconnect", "pattern", pattern, "error", err)
	if reconnErr := a.tryReconnect(); reconnErr != nil {
		return nil, fmt.Errorf("subscribe failed: %w (reconnect also failed: %v)", err, reconnErr)
	}
	if a.client == nil {
		return nil, fmt.Errorf("subscribe failed: client nil after reconnect")
	}

	_, err = a.client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: pattern})
	if err != nil {
		return nil, err
	}
	a.activeSubs[pattern] = handler
	return &grpcSub{adapter: a, pattern: pattern}, nil
}

// Close shuts down the gRPC connection.
func (a *GRPCBrokerAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.closed = true
	if a.conn != nil {
		err := a.conn.Close()
		a.conn = nil
		a.client = nil
		return err
	}
	return nil
}

// Configure sends a Configure RPC to the remote broker.
func (a *GRPCBrokerAdapter) Configure(config map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensureConnected(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := a.client.Configure(ctx, &brokerv1.ConfigureRequest{Config: config})
	return err
}

// GetInfo retrieves plugin metadata from the remote broker.
func (a *GRPCBrokerAdapter) GetInfo() (*plugin.PluginInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensureConnected(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := a.client.GetInfo(ctx, &brokerv1.GetInfoRequest{})
	if err != nil {
		return nil, err
	}
	return ProtoToPluginInfo(resp), nil
}

// HealthCheck retrieves health status from the remote broker.
func (a *GRPCBrokerAdapter) HealthCheck() (*plugin.HealthStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensureConnected(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := a.client.HealthCheck(ctx, &brokerv1.HealthCheckRequest{})
	if err != nil {
		return &plugin.HealthStatus{
			Status:  "unknown",
			Message: "gRPC health check failed: " + err.Error(),
		}, nil
	}
	return ProtoToHealthStatus(resp), nil
}

// grpcSub implements eventbus.Subscription for the gRPC adapter.
type grpcSub struct {
	adapter *GRPCBrokerAdapter
	pattern string
}

func (s *grpcSub) Unsubscribe() error {
	s.adapter.mu.Lock()
	defer s.adapter.mu.Unlock()

	delete(s.adapter.activeSubs, s.pattern)

	if s.adapter.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.adapter.client.Unsubscribe(ctx, &brokerv1.UnsubscribeRequest{Pattern: s.pattern})
	return err
}

// isLocalAddress returns true if the address refers to localhost.
func isLocalAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if host == "localhost" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
