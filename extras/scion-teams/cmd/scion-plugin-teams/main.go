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

// scion-plugin-teams is the Microsoft Teams message broker plugin for Scion.
// It can run as:
//   - A go-plugin subprocess (when launched by the scion plugin manager)
//   - A standalone gRPC service (--standalone flag or TEAMS_STANDALONE=true)
//   - A standalone binary that prints usage information
//
// Plugin mode is auto-detected via the SCION_PLUGIN magic cookie environment variable.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/GoogleCloudPlatform/scion/extras/scion-teams/internal/teams"
	"github.com/GoogleCloudPlatform/scion/pkg/integration/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	goplugin "github.com/hashicorp/go-plugin"
)

func main() {
	// If the magic cookie is set, run as a go-plugin subprocess.
	if os.Getenv(plugin.MagicCookieKey) == plugin.MagicCookieValue {
		servePlugin()
		return
	}

	if os.Getenv("TEAMS_STANDALONE") == "true" || hasFlag("--standalone") {
		serveStandalone()
		return
	}

	// Otherwise, print usage information.
	fmt.Println("scion-plugin-teams: Microsoft Teams message broker plugin for Scion")
	fmt.Println()
	fmt.Println("This binary is intended to be launched by the Scion plugin manager.")
	fmt.Println("It receives Activities via HTTP webhook from the Bot Framework Service")
	fmt.Println("and provides bidirectional messaging between Teams and Scion agents.")
	fmt.Println()
	fmt.Println("Modes:")
	fmt.Println("  (default)      Plugin mode — launched by hub plugin manager")
	fmt.Println("  --standalone   Standalone gRPC service")
	fmt.Println()
	fmt.Println("Configuration keys:")
	fmt.Println("  app_id           (required) Azure App Registration client ID")
	fmt.Println("  app_secret       (required) Azure App Registration client secret")
	fmt.Println("  tenant_id        (required) Azure AD tenant ID")
	fmt.Println("  listen_address   Webhook server bind address (default: :3978)")
	fmt.Println("  hub_url          Hub API URL for inbound message delivery")
	fmt.Println("  hmac_key         Base64-encoded HMAC key for hub authentication")
	fmt.Println("  broker_id        Broker ID for HMAC signing")
	fmt.Println("  db_path          Path to SQLite database (default: teams.db)")
	fmt.Println("  mention_routing  Enable @-mention routing (default: true)")
	os.Exit(0)
}

func hasFlag(flag string) bool {
	for _, arg := range os.Args[1:] {
		if arg == flag {
			return true
		}
	}
	return false
}

func servePlugin() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	impl := teams.NewBroker(log)
	log.Info("Starting Teams broker plugin")

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  plugin.BrokerPluginProtocolVersion,
			MagicCookieKey:   plugin.MagicCookieKey,
			MagicCookieValue: plugin.MagicCookieValue,
		},
		Plugins: map[string]goplugin.Plugin{
			plugin.BrokerPluginName: &plugin.BrokerPlugin{
				Impl: impl,
			},
		},
	})
}

func serveStandalone() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log.Info("Starting Teams bot in standalone mode")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	grpcPort := 50051
	if p := os.Getenv("GRPC_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			grpcPort = parsed
		}
	}

	// Create broker and gRPC server early so health probes work.
	broker := teams.NewBroker(log)
	brokerServer := grpcbroker.NewServer(broker)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", grpcPort))
	if err != nil {
		log.Error("Failed to listen for gRPC", "error", err, "port", grpcPort)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	brokerv1.RegisterBrokerServiceServer(grpcServer, brokerServer)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

	go func() {
		log.Info("gRPC server listening", "port", grpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			log.Error("gRPC server error", "error", err)
		}
	}()

	// Start the integration runtime (config layering + signal listener).
	rt := runtime.New(runtime.Options{
		Integration: "teams",
		ConfigFile:  os.Getenv("CONFIG_FILE"),
		EnvPrefix:   "TEAMS",
		EnvKeys: []string{
			"app_id", "app_secret", "tenant_id",
			"listen_address",
			"hub_url", "hmac_key", "broker_id",
			"db_path", "mention_routing",
		},
		UpdateHook: os.Getenv("UPDATE_HOOK"),
		Log:        log,
	})

	rctx, err := rt.Start(ctx)
	if err != nil {
		log.Error("Failed to start integration runtime", "error", err)
		os.Exit(1)
	}
	defer rt.Stop()

	cfg := rt.Config()

	if err := broker.Configure(cfg); err != nil {
		log.Error("Failed to configure Teams broker", "error", err)
		os.Exit(1)
	}

	rt.SetReconfigure(func(newCfg map[string]string) error {
		return broker.Configure(newCfg)
	})

	// Start the webhook server via Subscribe.
	if err := broker.Subscribe(">"); err != nil {
		log.Error("Failed to subscribe", "error", err)
		os.Exit(1)
	}
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	// Block until signal or update-triggered shutdown.
	select {
	case <-rctx.Done():
	case updateID := <-rt.ShutdownRequested():
		log.Info("Update-triggered shutdown", "update_id", updateID)
		stop()
	}

	log.Info("Shutting down standalone Teams bot")

	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

	grpcDone := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(grpcDone)
	}()
	select {
	case <-grpcDone:
	case <-time.After(5 * time.Second):
		grpcServer.Stop()
	}

	if err := broker.Close(); err != nil {
		log.Warn("Failed to close Teams broker", "error", err)
	}

	log.Info("Standalone Teams bot stopped")
}
