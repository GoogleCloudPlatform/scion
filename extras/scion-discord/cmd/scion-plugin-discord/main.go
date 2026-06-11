// scion-plugin-discord is the Discord message broker plugin for scion.
// It can run as:
//   - A go-plugin subprocess (when launched by the scion plugin manager)
//   - A standalone gRPC server with HA advisory-lock-based leader election
//
// Plugin mode is auto-detected via the SCION_PLUGIN magic cookie environment variable.
// Standalone mode is activated via --standalone flag or DISCORD_STANDALONE=true env var.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-discord/internal/discord"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	// If the magic cookie is set, run as a go-plugin subprocess.
	if os.Getenv(plugin.MagicCookieKey) == plugin.MagicCookieValue {
		servePlugin()
		return
	}

	if os.Getenv("DISCORD_STANDALONE") == "true" || hasFlag("--standalone") {
		serveStandalone()
		return
	}

	// Otherwise, print usage information.
	fmt.Println("scion-plugin-discord: Discord message broker plugin for Scion")
	fmt.Println()
	fmt.Println("This binary is intended to be launched by the Scion plugin manager.")
	fmt.Println("It communicates with the Discord Gateway API to provide bidirectional")
	fmt.Println("messaging between Discord channels and Scion agents.")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  scion-plugin-discord --standalone    Run as standalone gRPC server")
	fmt.Println()
	fmt.Println("Environment variables (standalone mode):")
	fmt.Println("  DISCORD_STANDALONE=true   Enable standalone mode")
	fmt.Println("  DISCORD_BOT_TOKEN         (required) Discord bot token")
	fmt.Println("  DISCORD_APPLICATION_ID    Discord application ID")
	fmt.Println("  DISCORD_PUBLIC_KEY        Discord public key")
	fmt.Println("  DISCORD_GUILD_ID          Guild ID for guild-scoped commands")
	fmt.Println("  HUB_URL                   Hub API URL for inbound message delivery")
	fmt.Println("  HMAC_KEY                  Base64-encoded HMAC key")
	fmt.Println("  BROKER_ID                 Broker ID for HMAC signing")
	fmt.Println("  DATABASE_URL              PostgreSQL connection string")
	fmt.Println("  GRPC_LISTEN_ADDRESS       gRPC listen address (default :50051)")
	fmt.Println()
	fmt.Println("Configuration keys (plugin mode):")
	fmt.Println("  bot_token        (required) Discord bot token")
	fmt.Println("  application_id   Discord application ID (for slash commands)")
	fmt.Println("  public_key       Discord public key (for interaction verification)")
	fmt.Println("  guild_id         Guild ID for guild-scoped commands (empty = global)")
	fmt.Println("  hub_url          Hub API URL for inbound message delivery")
	fmt.Println("  hmac_key         Base64-encoded HMAC key for hub authentication")
	fmt.Println("  broker_id        Broker ID for HMAC signing")
	fmt.Println("  db_path          Path to SQLite database (default: discord.db)")
	fmt.Println("  mention_routing  Enable @-mention routing (default: true)")
	fmt.Println("  send_queue_size  Max queued messages per channel (default: 100)")
	fmt.Println("  send_min_delay   Minimum delay between sends (default: 50ms)")
	fmt.Println("  agent_cache_ttl  TTL for cached agent list (default: 5m)")
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

	impl := discord.NewBroker(log)
	log.Info("Starting Discord broker plugin")

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
	log.Info("Starting Discord broker in standalone mode")

	botToken := os.Getenv("DISCORD_BOT_TOKEN")
	if botToken == "" {
		log.Error("DISCORD_BOT_TOKEN is required")
		os.Exit(1)
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Error("DATABASE_URL is required for standalone mode")
		os.Exit(1)
	}

	listenAddr := os.Getenv("GRPC_LISTEN_ADDRESS")
	if listenAddr == "" {
		listenAddr = ":50051"
	}

	broker := discord.NewBroker(log)

	config := map[string]string{
		"bot_token":       botToken,
		"application_id":  os.Getenv("DISCORD_APPLICATION_ID"),
		"public_key":      os.Getenv("DISCORD_PUBLIC_KEY"),
		"guild_id":        os.Getenv("DISCORD_GUILD_ID"),
		"hub_url":         os.Getenv("HUB_URL"),
		"hmac_key":        os.Getenv("HMAC_KEY"),
		"broker_id":       os.Getenv("BROKER_ID"),
		"database_driver": "postgres",
		"database_url":    databaseURL,
	}

	if err := broker.Configure(config); err != nil {
		log.Error("Failed to configure broker", "error", err)
		os.Exit(1)
	}

	lockStore, err := discord.NewPostgresStore(databaseURL)
	if err != nil {
		log.Error("Failed to open lock store", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Error("Failed to listen", "address", listenAddr, "error", err)
		os.Exit(1)
	}

	var lockHeld atomic.Bool

	grpcServer := grpc.NewServer()
	brokerv1.RegisterBrokerServiceServer(grpcServer, discord.NewBrokerGRPCServer(broker, log, &lockHeld))

	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	go runAdvisoryLockLoop(ctx, log, lockStore, broker, &lockHeld, healthServer)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-sigCh
		log.Info("Received shutdown signal")
		cancel()
		grpcServer.GracefulStop()
	}()

	log.Info("gRPC server listening", "address", listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Error("gRPC server failed", "error", err)
	}

	// Cleanup after Serve() returns (triggered by GracefulStop or error).
	if err := broker.Close(); err != nil {
		log.Warn("Error closing broker", "error", err)
	}

	if lockHeld.Load() {
		if err := lockStore.ReleaseAdvisoryLock(context.Background(), int64(store.LockDiscordGateway)); err != nil {
			log.Warn("Error releasing advisory lock", "error", err)
		}
	}

	if err := lockStore.Close(); err != nil {
		log.Warn("Error closing lock store", "error", err)
	}
}

func runAdvisoryLockLoop(ctx context.Context, log *slog.Logger, lockStore discord.Store, broker *discord.DiscordBroker, lockHeld *atomic.Bool, healthServer *health.Server) {
	const retryInterval = 30 * time.Second
	const pingInterval = 30 * time.Second

	for {
		acquired, err := lockStore.TryAdvisoryLock(ctx, int64(store.LockDiscordGateway))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("Failed to try advisory lock", "error", err)
		} else if acquired {
			lockHeld.Store(true)
			log.Info("Acquired gateway lock, starting as primary")
			if err := broker.Subscribe(">"); err != nil {
				log.Error("Failed to subscribe (start gateway)", "error", err)
				lockHeld.Store(false)
				goto waitRetry
			}
			healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

			// Monitor the lock connection; demote if it drops.
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(pingInterval):
				}
				if err := lockStore.PingLockConn(ctx); err != nil {
					log.Warn("Lock connection lost, demoting to standby", "error", err)
					if closeErr := broker.Close(); closeErr != nil {
						log.Warn("Error closing broker after lock loss", "error", closeErr)
					}
					_ = lockStore.ReleaseAdvisoryLock(context.Background(), int64(store.LockDiscordGateway))
					lockHeld.Store(false)
					healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
					break
				}
			}
		} else {
			log.Info("Another instance holds gateway lock, standing by")
		}

	waitRetry:
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryInterval):
		}
	}
}
