package chatapp

import (
	"context"
	"log/slog"
	"net"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	goplugin "github.com/hashicorp/go-plugin"
)

// TestBrokerServer_Serve_SelfManaged verifies that the broker plugin RPC
// server starts successfully and can be connected to by a go-plugin client
// using the self-managed (reattach) pattern — without requiring the magic
// cookie environment variable.
func TestBrokerServer_Serve_SelfManaged(t *testing.T) {
	log := slog.Default()
	broker := NewBrokerServer(nil, log)

	// Let the OS pick a free port.
	ps, err := broker.Serve("localhost:0")
	if err != nil {
		t.Fatalf("Serve failed: %v", err)
	}
	defer ps.Close()

	addr := ps.Addr()
	if addr == "" {
		t.Fatal("expected non-empty address")
	}

	// Connect with a go-plugin client using the Reattach config,
	// exactly as the hub's plugin manager does for self-managed plugins.
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("failed to resolve addr %q: %v", addr, err)
	}

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  plugin.BrokerPluginProtocolVersion,
			MagicCookieKey:   plugin.MagicCookieKey,
			MagicCookieValue: plugin.MagicCookieValue,
		},
		Plugins: map[string]goplugin.Plugin{
			plugin.BrokerPluginName: &plugin.BrokerPlugin{},
		},
		Reattach: &goplugin.ReattachConfig{
			Protocol:        goplugin.ProtocolNetRPC,
			ProtocolVersion: plugin.BrokerPluginProtocolVersion,
			Addr:            tcpAddr,
			Test:            true,
		},
	})
	defer client.Kill()

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	raw, err := rpcClient.Dispense(plugin.BrokerPluginName)
	if err != nil {
		t.Fatalf("failed to dispense broker plugin: %v", err)
	}

	brokerClient, ok := raw.(*plugin.BrokerRPCClient)
	if !ok {
		t.Fatalf("dispensed type %T is not *plugin.BrokerRPCClient", raw)
	}

	// Configure should succeed.
	if err := brokerClient.Configure(map[string]string{"test": "1"}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	// GetInfo should return the chat app metadata.
	info, err := brokerClient.GetInfo()
	if err != nil {
		t.Fatalf("GetInfo failed: %v", err)
	}
	if info.Name != "scion-chat-app" {
		t.Errorf("expected name %q, got %q", "scion-chat-app", info.Name)
	}

	// HealthCheck should return degraded (not yet configured by hub after our Configure
	// because we passed a simple config — but it should not error).
	status, err := brokerClient.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
	if status.Status != "healthy" {
		t.Errorf("expected status %q, got %q", "healthy", status.Status)
	}

	// Publish a test message.
	var received bool
	broker.SetHandler(func(_ context.Context, topic string, msg *messages.StructuredMessage) error {
		received = true
		if topic != "test.topic" {
			t.Errorf("expected topic %q, got %q", "test.topic", topic)
		}
		return nil
	})

	err = brokerClient.Publish(context.Background(), "test.topic", &messages.StructuredMessage{
		Type:   "test",
		Sender: "unit-test",
	})
	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}
	if !received {
		t.Error("handler was not called")
	}
}
