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
	"log/slog"
	"net"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeBrokerServer records calls for assertions.
type fakeBrokerServer struct {
	brokerv1.UnimplementedBrokerServiceServer
	published    []*brokerv1.PublishRequest
	subscribed   []string
	unsubscribed []string
}

func (s *fakeBrokerServer) Publish(_ context.Context, req *brokerv1.PublishRequest) (*brokerv1.PublishResponse, error) {
	s.published = append(s.published, req)
	return &brokerv1.PublishResponse{}, nil
}

func (s *fakeBrokerServer) Subscribe(_ context.Context, req *brokerv1.SubscribeRequest) (*brokerv1.SubscribeResponse, error) {
	s.subscribed = append(s.subscribed, req.GetPattern())
	return &brokerv1.SubscribeResponse{}, nil
}

func (s *fakeBrokerServer) Unsubscribe(_ context.Context, req *brokerv1.UnsubscribeRequest) (*brokerv1.UnsubscribeResponse, error) {
	s.unsubscribed = append(s.unsubscribed, req.GetPattern())
	return &brokerv1.UnsubscribeResponse{}, nil
}

func startFakeServer(t *testing.T) (*fakeBrokerServer, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake := &fakeBrokerServer{}
	brokerv1.RegisterBrokerServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)
	return fake, lis.Addr().String()
}

func newTestAdapter(t *testing.T, addr string) *GRPCBrokerAdapter {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &GRPCBrokerAdapter{
		client:  brokerv1.NewBrokerServiceClient(conn),
		conn:    conn,
		address: addr,
		channel: "test-channel",
		log:     slog.Default(),
		subs:    make(map[string]eventbus.EventHandler),
	}
}

func TestGRPCBrokerAdapter_Publish(t *testing.T) {
	fake, addr := startFakeServer(t)
	adapter := newTestAdapter(t, addr)

	msg := &messages.StructuredMessage{
		Version:     1,
		Sender:      "alice",
		SenderID:    "a1",
		Recipient:   "bob",
		Msg:         "hello",
		Type:        "instruction",
		Plain:       true,
		Channel:     "test-channel",
		Metadata:    map[string]string{"key": "val"},
		Attachments: []string{"att1"},
	}

	if err := adapter.Publish(context.Background(), "chat.message", msg); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if len(fake.published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(fake.published))
	}
	req := fake.published[0]
	if req.Topic != "chat.message" {
		t.Errorf("topic = %q, want %q", req.Topic, "chat.message")
	}
	pm := req.Message
	if pm.Sender != "alice" {
		t.Errorf("sender = %q, want %q", pm.Sender, "alice")
	}
	if pm.SenderId != "a1" {
		t.Errorf("sender_id = %q, want %q", pm.SenderId, "a1")
	}
	if pm.Msg != "hello" {
		t.Errorf("msg = %q, want %q", pm.Msg, "hello")
	}
	if !pm.Plain {
		t.Error("plain should be true")
	}
	if pm.Channel != "test-channel" {
		t.Errorf("channel = %q, want %q", pm.Channel, "test-channel")
	}
	if pm.Metadata["key"] != "val" {
		t.Errorf("metadata[key] = %q, want %q", pm.Metadata["key"], "val")
	}
	if len(pm.Attachments) != 1 || pm.Attachments[0] != "att1" {
		t.Errorf("attachments = %v, want [att1]", pm.Attachments)
	}
}

func TestGRPCBrokerAdapter_Subscribe(t *testing.T) {
	fake, addr := startFakeServer(t)
	adapter := newTestAdapter(t, addr)

	handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) {}
	sub, err := adapter.Subscribe(">", handler)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if len(fake.subscribed) != 1 || fake.subscribed[0] != ">" {
		t.Fatalf("expected subscribe with pattern '>', got %v", fake.subscribed)
	}

	adapter.mu.RLock()
	_, tracked := adapter.subs[">"]
	adapter.mu.RUnlock()
	if !tracked {
		t.Error("handler not tracked in subs map")
	}

	if err := sub.Unsubscribe(); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if len(fake.unsubscribed) != 1 || fake.unsubscribed[0] != ">" {
		t.Fatalf("expected unsubscribe with pattern '>', got %v", fake.unsubscribed)
	}

	adapter.mu.RLock()
	_, stillTracked := adapter.subs[">"]
	adapter.mu.RUnlock()
	if stillTracked {
		t.Error("handler still tracked after Unsubscribe")
	}
}

func TestGRPCBrokerAdapter_Close(t *testing.T) {
	_, addr := startFakeServer(t)
	adapter := newTestAdapter(t, addr)

	if err := adapter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	adapter.mu.RLock()
	closed := adapter.closed
	adapter.mu.RUnlock()
	if !closed {
		t.Error("expected closed flag to be set")
	}

	if err := adapter.Publish(context.Background(), "t", &messages.StructuredMessage{}); err == nil {
		t.Error("expected error publishing on closed adapter")
	}
}

func TestStructuredMessageConversion_RoundTrip(t *testing.T) {
	orig := &messages.StructuredMessage{
		Version:      1,
		Timestamp:    "2026-01-01T00:00:00Z",
		Sender:       "alice",
		SenderID:     "a1",
		Recipient:    "bob",
		RecipientID:  "b2",
		Recipients:   "bob,carol",
		Msg:          "test message",
		Type:         "instruction",
		Plain:        true,
		Raw:          false,
		Urgent:       true,
		Broadcasted:  false,
		ObserverOnly: true,
		Status:       "active",
		Attachments:  []string{"file1.txt", "file2.txt"},
		Metadata:     map[string]string{"k1": "v1", "k2": "v2"},
		Channel:      "discord",
		ThreadID:     "thread-123",
		Visibility:   "normal",
	}

	proto := structuredMessageToProto(orig)
	back := protoToStructuredMessage(proto)

	if back.Version != orig.Version {
		t.Errorf("Version: got %d, want %d", back.Version, orig.Version)
	}
	if back.Timestamp != orig.Timestamp {
		t.Errorf("Timestamp: got %q, want %q", back.Timestamp, orig.Timestamp)
	}
	if back.Sender != orig.Sender {
		t.Errorf("Sender: got %q, want %q", back.Sender, orig.Sender)
	}
	if back.SenderID != orig.SenderID {
		t.Errorf("SenderID: got %q, want %q", back.SenderID, orig.SenderID)
	}
	if back.Recipient != orig.Recipient {
		t.Errorf("Recipient: got %q, want %q", back.Recipient, orig.Recipient)
	}
	if back.RecipientID != orig.RecipientID {
		t.Errorf("RecipientID: got %q, want %q", back.RecipientID, orig.RecipientID)
	}
	if back.Recipients != orig.Recipients {
		t.Errorf("Recipients: got %q, want %q", back.Recipients, orig.Recipients)
	}
	if back.Msg != orig.Msg {
		t.Errorf("Msg: got %q, want %q", back.Msg, orig.Msg)
	}
	if back.Type != orig.Type {
		t.Errorf("Type: got %q, want %q", back.Type, orig.Type)
	}
	if back.Plain != orig.Plain {
		t.Errorf("Plain: got %v, want %v", back.Plain, orig.Plain)
	}
	if back.Raw != orig.Raw {
		t.Errorf("Raw: got %v, want %v", back.Raw, orig.Raw)
	}
	if back.Urgent != orig.Urgent {
		t.Errorf("Urgent: got %v, want %v", back.Urgent, orig.Urgent)
	}
	if back.Broadcasted != orig.Broadcasted {
		t.Errorf("Broadcasted: got %v, want %v", back.Broadcasted, orig.Broadcasted)
	}
	if back.ObserverOnly != orig.ObserverOnly {
		t.Errorf("ObserverOnly: got %v, want %v", back.ObserverOnly, orig.ObserverOnly)
	}
	if back.Status != orig.Status {
		t.Errorf("Status: got %q, want %q", back.Status, orig.Status)
	}
	if back.Channel != orig.Channel {
		t.Errorf("Channel: got %q, want %q", back.Channel, orig.Channel)
	}
	if back.ThreadID != orig.ThreadID {
		t.Errorf("ThreadID: got %q, want %q", back.ThreadID, orig.ThreadID)
	}
	if back.Visibility != orig.Visibility {
		t.Errorf("Visibility: got %q, want %q", back.Visibility, orig.Visibility)
	}
	if len(back.Attachments) != len(orig.Attachments) {
		t.Fatalf("Attachments length: got %d, want %d", len(back.Attachments), len(orig.Attachments))
	}
	for i, a := range orig.Attachments {
		if back.Attachments[i] != a {
			t.Errorf("Attachments[%d]: got %q, want %q", i, back.Attachments[i], a)
		}
	}
	if len(back.Metadata) != len(orig.Metadata) {
		t.Fatalf("Metadata length: got %d, want %d", len(back.Metadata), len(orig.Metadata))
	}
	for k, v := range orig.Metadata {
		if back.Metadata[k] != v {
			t.Errorf("Metadata[%q]: got %q, want %q", k, back.Metadata[k], v)
		}
	}
}

func TestProtoToStructuredMessage_Nil(t *testing.T) {
	if got := protoToStructuredMessage(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestPublishRequest_NilMessage(t *testing.T) {
	req := structuredMessageToPublishRequest("topic", nil)
	if req.Topic != "topic" {
		t.Errorf("topic = %q, want %q", req.Topic, "topic")
	}
	if req.Message != nil {
		t.Errorf("expected nil message, got %v", req.Message)
	}
}

// Verify interface compliance at compile time.
var _ eventbus.EventBus = (*GRPCBrokerAdapter)(nil)
