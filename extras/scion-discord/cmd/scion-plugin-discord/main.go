// scion-plugin-discord is the Discord message broker plugin for scion.
// It can run as:
//   - A go-plugin subprocess (when launched by the scion plugin manager)
//   - A standalone gRPC service with HA support (--standalone flag or DISCORD_STANDALONE=true)
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

	"github.com/GoogleCloudPlatform/scion/extras/scion-discord/internal/discord"
	"github.com/GoogleCloudPlatform/scion/pkg/integration/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	goplugin "github.com/hashicorp/go-plugin"

	_ "github.com/jackc/pgx/v5/stdlib"
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
	fmt.Println("Modes:")
	fmt.Println("  (default)      Plugin mode — launched by hub plugin manager")
	fmt.Println("  --standalone   Standalone gRPC service with HA advisory lock")
	fmt.Println()
	fmt.Println("Configuration keys:")
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
	log.Info("Starting Discord bot in standalone mode")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Read essential config from environment.
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

	// Start the integration runtime (config layering + signal listener).
	rt := runtime.New(runtime.Options{
		Integration: "discord",
		DatabaseURL: databaseURL,
		ConfigFile:  os.Getenv("CONFIG_FILE"),
		EnvPrefix:   "DISCORD",
		EnvKeys: []string{
			"bot_token", "application_id", "public_key", "guild_id",
			"hub_url", "hmac_key", "broker_id",
			"mention_routing", "send_queue_size", "send_min_delay",
			"agent_cache_ttl",
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

	// Open the Postgres store for Discord-specific data.
	discordStore, err := discord.NewPostgresStore(databaseURL)
	if err != nil {
		log.Error("Failed to open Discord Postgres store", "error", err)
		os.Exit(1)
	}
	defer discordStore.Close()

	// Create the Discord broker.
	broker := discord.NewBroker(log)

	// Build the config map from runtime + env direct overrides.
	cfg := rt.Config()
	cfg["database_driver"] = "postgres"
	cfg["database_url"] = databaseURL

	if err := broker.Configure(cfg); err != nil {
		log.Error("Failed to configure Discord broker", "error", err)
		os.Exit(1)
	}

	// Wire runtime reconfigure callback to re-configure the broker.
	rt.SetReconfigure(func(newCfg map[string]string) error {
		newCfg["database_driver"] = "postgres"
		newCfg["database_url"] = databaseURL
		return broker.Configure(newCfg)
	})

	// Start gRPC server with the 5A scaffolding.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", grpcPort))
	if err != nil {
		log.Error("Failed to listen for gRPC", "error", err, "port", grpcPort)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	brokerServer := grpcbroker.NewServer(broker)
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

	// Lock loop: acquire advisory lock → open Gateway; not acquired → standby.
	lockKey := int64(store.LockDiscordGateway)
	gatewayActive := false

	go func() {
		const lockInterval = 30 * time.Second
		ticker := time.NewTicker(lockInterval)
		defer ticker.Stop()

		tryLock := func() {
			acquired, lockErr := discordStore.TryAdvisoryLock(rctx, lockKey)
			if lockErr != nil {
				log.Error("Advisory lock error", "error", lockErr)
				return
			}

			if acquired && !gatewayActive {
				log.Info("Advisory lock acquired, opening Discord Gateway")
				if subErr := broker.Subscribe(">"); subErr != nil {
					log.Error("Failed to subscribe/open Gateway", "error", subErr)
					_ = discordStore.ReleaseAdvisoryLock(rctx, lockKey)
					return
				}
				gatewayActive = true
				healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
			} else if !acquired && !gatewayActive {
				log.Debug("Standby — another instance holds the Gateway lock")
			}
		}

		// Try immediately on startup.
		tryLock()

		for {
			select {
			case <-rctx.Done():
				return
			case <-ticker.C:
				if !gatewayActive {
					tryLock()
				}
			}
		}
	}()

	// Block until signal.
	<-rctx.Done()
	log.Info("Shutting down standalone Discord bot")

	// Graceful shutdown sequence.
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

	if gatewayActive {
		if closeErr := broker.Close(); closeErr != nil {
			log.Warn("Failed to close Discord broker", "error", closeErr)
		}
		if releaseErr := discordStore.ReleaseAdvisoryLock(context.Background(), lockKey); releaseErr != nil {
			log.Warn("Failed to release advisory lock", "error", releaseErr)
		}
	}

	grpcServer.GracefulStop()
	log.Info("Standalone Discord bot stopped")
}
