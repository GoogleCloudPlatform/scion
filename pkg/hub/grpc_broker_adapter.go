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

package hub

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// GRPCBrokerAdapter implements eventbus.EventBus by forwarding calls over gRPC
// to a standalone broker service.
type GRPCBrokerAdapter struct {
	client  brokerv1.BrokerServiceClient
	conn    *grpc.ClientConn
	address string
	channel string
	log     *slog.Logger

	mu              sync.RWMutex
	subs            map[string]eventbus.EventHandler
	closed          bool
	lastReconnectAt time.Time
}

// NewGRPCBrokerAdapter dials the broker gRPC service at address and returns an
// adapter that satisfies eventbus.EventBus.
func NewGRPCBrokerAdapter(address string, channel string, log *slog.Logger) (*GRPCBrokerAdapter, error) {
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", address, err)
	}

	return &GRPCBrokerAdapter{
		client:  brokerv1.NewBrokerServiceClient(conn),
		conn:    conn,
		address: address,
		channel: channel,
		log:     log.With("component", "grpc-broker-adapter", "address", address),
		subs:    make(map[string]eventbus.EventHandler),
	}, nil
}

// Configure sends a configuration map to the remote broker via gRPC.
func (a *GRPCBrokerAdapter) Configure(config map[string]string) error {
	a.mu.RLock()
	if a.closed {
		a.mu.RUnlock()
		return fmt.Errorf("adapter is closed")
	}
	client := a.client
	a.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err := client.Configure(ctx, &brokerv1.ConfigureRequest{Config: config})
	cancel()
	if err != nil {
		a.log.Warn("Configure failed, attempting reconnect", "error", err)
		if reconnErr := a.tryReconnect(); reconnErr != nil {
			return fmt.Errorf("configure failed: %w (reconnect also failed: %v)", err, reconnErr)
		}
		a.mu.RLock()
		client = a.client
		a.mu.RUnlock()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = client.Configure(ctx2, &brokerv1.ConfigureRequest{Config: config})
		cancel2()
	}
	return err
}

// Publish converts the StructuredMessage to its proto representation and sends
// it to the remote broker via gRPC.
func (a *GRPCBrokerAdapter) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	a.mu.RLock()
	if a.closed {
		a.mu.RUnlock()
		return fmt.Errorf("adapter is closed")
	}
	client := a.client
	a.mu.RUnlock()

	req := structuredMessageToPublishRequest(topic, msg)
	_, err := client.Publish(ctx, req)
	if err != nil {
		a.log.Warn("Publish failed", "topic", topic, "error", err)
		if reconnErr := a.tryReconnect(); reconnErr != nil {
			return fmt.Errorf("publish failed: %w (reconnect also failed: %v)", err, reconnErr)
		}
		a.mu.RLock()
		client = a.client
		a.mu.RUnlock()
		_, err = client.Publish(ctx, req)
	}
	return err
}

// Subscribe stores the handler locally and tells the remote broker to start
// listening for the given pattern. Inbound delivery is via HTTP POST to the
// hub API, not via the handler callback.
func (a *GRPCBrokerAdapter) Subscribe(pattern string, handler eventbus.EventHandler) (eventbus.Subscription, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return nil, fmt.Errorf("adapter is closed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err := a.client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: pattern})
	cancel()
	if err != nil {
		a.log.Warn("Subscribe failed, attempting reconnect", "pattern", pattern, "error", err)
		a.mu.Unlock()
		reconnErr := a.tryReconnect()
		a.mu.Lock()
		if reconnErr != nil {
			return nil, fmt.Errorf("subscribe failed: %w (reconnect also failed: %v)", err, reconnErr)
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = a.client.Subscribe(ctx2, &brokerv1.SubscribeRequest{Pattern: pattern})
		cancel2()
		if err != nil {
			return nil, err
		}
	}

	a.subs[pattern] = handler
	return &grpcSubscription{adapter: a, pattern: pattern}, nil
}

// Close shuts down the gRPC connection.
func (a *GRPCBrokerAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return a.conn.Close()
}

// tryReconnect re-establishes the gRPC connection and re-subscribes all active
// patterns on the new connection. It guards against thundering-herd reconnects
// by skipping if another goroutine already reconnected or if the last reconnect
// was within the past 5 seconds.
func (a *GRPCBrokerAdapter) tryReconnect() error {
	a.mu.Lock()
	if time.Since(a.lastReconnectAt) < 5*time.Second {
		a.mu.Unlock()
		a.log.Debug("Skipping reconnect, another goroutine reconnected recently")
		return nil
	}
	failedConn := a.conn
	a.mu.Unlock()

	a.log.Info("Attempting reconnect to broker service")

	conn, err := grpc.NewClient(a.address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("reconnect dial failed: %w", err)
	}

	a.mu.Lock()
	if a.conn != failedConn {
		a.mu.Unlock()
		_ = conn.Close()
		a.log.Debug("Skipping reconnect, connection already replaced by another goroutine")
		return nil
	}
	oldConn := a.conn
	a.conn = conn
	a.client = brokerv1.NewBrokerServiceClient(conn)
	client := a.client
	a.lastReconnectAt = time.Now()
	patterns := make([]string, 0, len(a.subs))
	for p := range a.subs {
		patterns = append(patterns, p)
	}
	a.mu.Unlock()

	if oldConn != nil {
		_ = oldConn.Close()
	}

	for _, pattern := range patterns {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, subErr := client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: pattern}); subErr != nil {
			a.log.Warn("Failed to re-subscribe after reconnect", "pattern", pattern, "error", subErr)
		}
		cancel()
	}

	a.log.Info("Successfully reconnected to broker service", "resubscribed", len(patterns))
	return nil
}

// structuredMessageToPublishRequest converts a messages.StructuredMessage and
// topic into a brokerv1.PublishRequest.
func structuredMessageToPublishRequest(topic string, msg *messages.StructuredMessage) *brokerv1.PublishRequest {
	if msg == nil {
		return &brokerv1.PublishRequest{Topic: topic}
	}
	return &brokerv1.PublishRequest{
		Topic:   topic,
		Message: structuredMessageToProto(msg),
	}
}

func structuredMessageToProto(msg *messages.StructuredMessage) *brokerv1.StructuredMessage {
	pm := &brokerv1.StructuredMessage{
		Version:      int32(msg.Version),
		Timestamp:    msg.Timestamp,
		Sender:       msg.Sender,
		SenderId:     msg.SenderID,
		Recipient:    msg.Recipient,
		RecipientId:  msg.RecipientID,
		Recipients:   msg.Recipients,
		Msg:          msg.Msg,
		Type:         msg.Type,
		Plain:        msg.Plain,
		Raw:          msg.Raw,
		Urgent:       msg.Urgent,
		Broadcasted:  msg.Broadcasted,
		ObserverOnly: msg.ObserverOnly,
		Status:       msg.Status,
		Channel:      msg.Channel,
		ThreadId:     msg.ThreadID,
		Visibility:   msg.Visibility,
	}
	if len(msg.Attachments) > 0 {
		pm.Attachments = make([]string, len(msg.Attachments))
		copy(pm.Attachments, msg.Attachments)
	}
	if len(msg.Metadata) > 0 {
		pm.Metadata = make(map[string]string, len(msg.Metadata))
		for k, v := range msg.Metadata {
			pm.Metadata[k] = v
		}
	}
	return pm
}

// protoToStructuredMessage converts a brokerv1.StructuredMessage back to the
// internal messages.StructuredMessage type.
func protoToStructuredMessage(pm *brokerv1.StructuredMessage) *messages.StructuredMessage {
	if pm == nil {
		return nil
	}
	msg := &messages.StructuredMessage{
		Version:      int(pm.Version),
		Timestamp:    pm.Timestamp,
		Sender:       pm.Sender,
		SenderID:     pm.SenderId,
		Recipient:    pm.Recipient,
		RecipientID:  pm.RecipientId,
		Recipients:   pm.Recipients,
		Msg:          pm.Msg,
		Type:         pm.Type,
		Plain:        pm.Plain,
		Raw:          pm.Raw,
		Urgent:       pm.Urgent,
		Broadcasted:  pm.Broadcasted,
		ObserverOnly: pm.ObserverOnly,
		Status:       pm.Status,
		Channel:      pm.Channel,
		ThreadID:     pm.ThreadId,
		Visibility:   pm.Visibility,
	}
	if len(pm.Attachments) > 0 {
		msg.Attachments = make([]string, len(pm.Attachments))
		copy(msg.Attachments, pm.Attachments)
	}
	if len(pm.Metadata) > 0 {
		msg.Metadata = make(map[string]string, len(pm.Metadata))
		for k, v := range pm.Metadata {
			msg.Metadata[k] = v
		}
	}
	return msg
}

// grpcSubscription implements eventbus.Subscription for the GRPCBrokerAdapter.
type grpcSubscription struct {
	adapter *GRPCBrokerAdapter
	pattern string
}

func (s *grpcSubscription) Unsubscribe() error {
	s.adapter.mu.Lock()
	defer s.adapter.mu.Unlock()
	delete(s.adapter.subs, s.pattern)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.adapter.client.Unsubscribe(ctx, &brokerv1.UnsubscribeRequest{Pattern: s.pattern})
	return err
}
