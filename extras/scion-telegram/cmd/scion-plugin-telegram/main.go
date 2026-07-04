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

// scion-plugin-telegram is the Telegram message broker plugin for scion.
// It can run as:
//   - A go-plugin subprocess (when launched by the scion plugin manager)
//   - A standalone gRPC service with HA support (--standalone flag or TELEGRAM_STANDALONE=true)
//   - A migration tool (SQLite → Postgres)
//   - A standalone binary that prints usage information
//
// Plugin mode is auto-detected via the SCION_PLUGIN magic cookie environment variable.
// Standalone mode is selected via the --standalone flag or TELEGRAM_STANDALONE=true.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/GoogleCloudPlatform/scion/extras/scion-telegram/internal/telegram"
	"github.com/GoogleCloudPlatform/scion/pkg/integration/lockloop"
	"github.com/GoogleCloudPlatform/scion/pkg/integration/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	goplugin "github.com/hashicorp/go-plugin"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	// If the magic cookie is set, run as a go-plugin subprocess
	if os.Getenv(plugin.MagicCookieKey) == plugin.MagicCookieValue {
		servePlugin()
		return
	}

	// Check for subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			runMigrate()
			return
		case "--standalone":
			serveStandalone()
			return
		}
	}

	// Check env var for standalone mode
	if os.Getenv("TELEGRAM_STANDALONE") == "true" || os.Getenv("SCION_TELEGRAM_STANDALONE") == "1" {
		serveStandalone()
		return
	}

	// Otherwise, print usage information
	fmt.Println("scion-plugin-telegram: Telegram message broker plugin for Scion")
	fmt.Println()
	fmt.Println("This binary is intended to be launched by the Scion plugin manager.")
	fmt.Println("It communicates with the Telegram Bot API to provide bidirectional")
	fmt.Println("messaging between Telegram chats and Scion agents.")
	fmt.Println()
	fmt.Println("Modes:")
	fmt.Println("  (default)      Run as go-plugin subprocess (requires SCION_PLUGIN cookie)")
	fmt.Println("  --standalone   Run as standalone HA service with Postgres")
	fmt.Println("  migrate        Migrate data from SQLite to Postgres")
	fmt.Println()
	fmt.Println("Standalone mode environment variables:")
	fmt.Println("  TELEGRAM_STANDALONE=true    Enable standalone mode")
	fmt.Println("  DATABASE_URL                Postgres connection URL (required)")
	fmt.Println("  TELEGRAM_BOT_TOKEN          Telegram bot token (required)")
	fmt.Println("  TELEGRAM_WEBHOOK_URL        Public webhook URL (required)")
	fmt.Println("  TELEGRAM_WEBHOOK_SECRET     Secret token for webhook validation")
	fmt.Println("  TELEGRAM_WEBHOOK_LISTEN     Listen address for webhook (default :9094)")
	fmt.Println("  TELEGRAM_HUB_URL            Hub API URL")
	fmt.Println("  TELEGRAM_HMAC_KEY           HMAC key for hub auth")
	fmt.Println("  TELEGRAM_BROKER_ID          Broker identifier")
	fmt.Println("  GRPC_PORT                   gRPC port (default 50051)")
	os.Exit(0)
}

func servePlugin() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var impl plugin.MessageBrokerPluginInterface
	if os.Getenv("SCION_TELEGRAM_V2") == "1" {
		impl = telegram.NewV2(log)
		log.Info("Using Telegram broker v2")
	} else {
		impl = telegram.New(log)
	}

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
	log.Info("Starting Telegram broker in standalone/HA mode")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Error("DATABASE_URL is required in standalone mode")
		os.Exit(1)
	}

	grpcPort := 50051
	if p := os.Getenv("GRPC_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			grpcPort = parsed
		}
	}

	// F2: Create broker and gRPC server early so health probes work during
	// the runtime's DB-connect retry window.
	broker := telegram.NewV2(log)
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

	// F3: Start the integration runtime (config layering + signal listener).
	rt := runtime.New(runtime.Options{
		Integration: "telegram",
		DatabaseURL: databaseURL,
		ConfigFile:  os.Getenv("CONFIG_FILE"),
		EnvPrefix:   "TELEGRAM",
		EnvKeys: []string{
			"bot_token", "webhook_url", "webhook_secret", "webhook_listen",
			"hub_url", "hmac_key", "broker_id",
			"api_base_url", "agent_cache_ttl", "send_queue_size",
			"send_min_delay",
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

	// Open Telegram Postgres store.
	telegramStore, err := telegram.NewPostgresStore(databaseURL)
	if err != nil {
		log.Error("Failed to open Telegram Postgres store", "error", err)
		os.Exit(1)
	}
	defer telegramStore.Close()

	// F4+F8: Build broker config from runtime. All instances Configure
	// (start webhook HTTP listener) with skip_set_webhook=true.
	// Only the lock holder calls RegisterWebhook().
	cfg := rt.Config()
	cfg["database_url"] = databaseURL
	cfg["skip_set_webhook"] = "true"

	// F8: Validate merged config rejects poll mode before forcing webhook.
	if v, ok := cfg["inbound_mode"]; ok && strings.EqualFold(v, "poll") {
		log.Error("HA/standalone Telegram requires webhook mode; inbound_mode=poll is not supported")
		os.Exit(1)
	}
	cfg["inbound_mode"] = "webhook"

	if err := broker.Configure(cfg); err != nil {
		log.Error("Failed to configure Telegram broker", "error", err)
		os.Exit(1)
	}

	rt.SetReconfigure(func(newCfg map[string]string) error {
		newCfg["database_url"] = databaseURL
		newCfg["skip_set_webhook"] = "true"
		if v, ok := newCfg["inbound_mode"]; ok && strings.EqualFold(v, "poll") {
			return fmt.Errorf("HA/standalone Telegram requires webhook mode; inbound_mode=poll rejected")
		}
		newCfg["inbound_mode"] = "webhook"
		return broker.Configure(newCfg)
	})

	// Resolve webhook URL and secret for RegisterWebhook.
	webhookURL := cfg["webhook_url"]
	if webhookURL == "" {
		webhookURL = os.Getenv("TELEGRAM_WEBHOOK_URL")
	}
	webhookSecret := cfg["webhook_secret"]
	if webhookSecret == "" {
		webhookSecret = os.Getenv("TELEGRAM_WEBHOOK_SECRET")
	}

	// F1+F5: Use the shared lock loop. Only the lock holder calls setWebhook;
	// all instances serve webhook HTTP traffic.
	lockStore, ok := telegramStore.(lockloop.AdvisoryLocker)
	if !ok {
		log.Error("Postgres store does not support advisory locks")
		os.Exit(1)
	}

	lockLoop := lockloop.New(lockStore, int64(store.LockTelegramWebhook), log)
	lockLoop.OnAcquired = func() error {
		if err := broker.RegisterWebhook(webhookURL, webhookSecret); err != nil {
			return err
		}
		healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		return nil
	}
	lockLoop.OnLost = func() {
		healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
		if err := broker.DeregisterWebhook(); err != nil {
			log.Warn("Error deregistering webhook on lock loss", "error", err)
		}
	}

	lockLoopDone := make(chan struct{})
	go func() {
		lockLoop.Run(rctx)
		close(lockLoopDone)
	}()

	// Block until signal or update-triggered shutdown.
	select {
	case <-rctx.Done():
	case updateID := <-rt.ShutdownRequested():
		log.Info("Update-triggered shutdown", "update_id", updateID)
		stop()
	}
	<-lockLoopDone
	log.Info("Shutting down standalone Telegram bot")

	// F6: Correct shutdown ordering:
	// NOT_SERVING → GracefulStop (bounded) → broker.Close → release lock (last).
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

	if lockLoop.Active() {
		if err := broker.Close(); err != nil {
			log.Warn("Failed to close Telegram broker", "error", err)
		}
	}
	lockLoop.ReleaseHandle()

	log.Info("Standalone Telegram bot stopped")
}

var errPollRejected = errors.New("HA/standalone Telegram requires webhook mode; inbound_mode=poll is not supported")

func envOrEmpty(key string) string {
	return os.Getenv(key)
}

func hasFlag(flag string) bool {
	for _, arg := range os.Args[1:] {
		if arg == flag {
			return true
		}
	}
	return false
}
