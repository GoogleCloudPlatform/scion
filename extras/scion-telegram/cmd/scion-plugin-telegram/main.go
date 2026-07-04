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
//   - A standalone service with Postgres-backed state (HA/Mode 3)
//   - A migration tool (SQLite → Postgres)
//   - A standalone binary that prints usage information
//
// Plugin mode is auto-detected via the SCION_PLUGIN magic cookie environment variable.
// Standalone mode is selected via the --standalone flag or SCION_TELEGRAM_STANDALONE=1.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-telegram/internal/telegram"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	goplugin "github.com/hashicorp/go-plugin"
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
	if os.Getenv("SCION_TELEGRAM_STANDALONE") == "1" {
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
	fmt.Println("Configuration keys:")
	fmt.Println("  bot_token       (required) Telegram Bot API token")
	fmt.Println("  hub_url         Hub API URL for inbound message delivery")
	fmt.Println("  hmac_key        Base64-encoded HMAC key for hub authentication")
	fmt.Println("  broker_id       Broker ID for HMAC signing")
	fmt.Println("  chat_routes     JSON map of chat IDs to topic patterns (inbound routing)")
	fmt.Println("  outbound_routes JSON map of topic patterns to chat IDs (outbound routing)")
	fmt.Println("  user_mappings   JSON map of Telegram user IDs to scion user emails/IDs")
	fmt.Println("  register_addr   HTTP listen address for registration server (e.g., :9093)")
	fmt.Println("  register_url    External URL for registration links (e.g., https://example.com)")
	fmt.Println("  mappings_file   Path to persist user mappings JSON file")
	fmt.Println("  api_base_url    Override Telegram API base URL (for testing)")
	fmt.Println()
	fmt.Println("Standalone mode environment variables:")
	fmt.Println("  SCION_TELEGRAM_STANDALONE=1   Enable standalone mode")
	fmt.Println("  DATABASE_URL                  Postgres connection URL (required)")
	fmt.Println("  BOT_TOKEN                     Telegram bot token (required)")
	fmt.Println("  WEBHOOK_URL                   Public webhook URL (required)")
	fmt.Println("  WEBHOOK_SECRET                Secret token for webhook validation")
	fmt.Println("  WEBHOOK_LISTEN                Listen address for webhook (default :9094)")
	fmt.Println("  HUB_URL                       Hub API URL")
	fmt.Println("  HMAC_KEY                      HMAC key for hub auth")
	fmt.Println("  BROKER_ID                     Broker identifier")
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

// standaloneConfig holds validated configuration for standalone mode.
type standaloneConfig struct {
	DatabaseURL    string
	BotToken       string
	WebhookURL     string
	WebhookSecret  string
	WebhookListen  string
	HubURL         string
	HMACKey        string
	BrokerID       string
	InboundMode    string
	APIBaseURL     string
	AgentCacheTTL  string
	SendQueueSize  string
	SendMinDelay   string
}

func loadStandaloneConfig() (*standaloneConfig, error) {
	cfg := &standaloneConfig{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		BotToken:      os.Getenv("BOT_TOKEN"),
		WebhookURL:    os.Getenv("WEBHOOK_URL"),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
		WebhookListen: os.Getenv("WEBHOOK_LISTEN"),
		HubURL:        os.Getenv("HUB_URL"),
		HMACKey:       os.Getenv("HMAC_KEY"),
		BrokerID:      os.Getenv("BROKER_ID"),
		InboundMode:   os.Getenv("INBOUND_MODE"),
		APIBaseURL:    os.Getenv("API_BASE_URL"),
		AgentCacheTTL: os.Getenv("AGENT_CACHE_TTL"),
		SendQueueSize: os.Getenv("SEND_QUEUE_SIZE"),
		SendMinDelay:  os.Getenv("SEND_MIN_DELAY"),
	}

	if cfg.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required for standalone mode")
	}
	if cfg.BotToken == "" {
		return nil, errors.New("BOT_TOKEN is required for standalone mode")
	}

	// HA/standalone Telegram is webhook-only (design decision D8).
	if cfg.InboundMode != "" && strings.EqualFold(cfg.InboundMode, "poll") {
		return nil, errors.New("HA/standalone Telegram requires webhook mode. Long-poll is supported only in plugin (Mode 1/2) deployments")
	}
	cfg.InboundMode = "webhook"

	if cfg.WebhookURL == "" {
		return nil, errors.New("WEBHOOK_URL is required for standalone/HA Telegram (webhook-only mode)")
	}

	if cfg.WebhookListen == "" {
		cfg.WebhookListen = ":9094"
	}

	return cfg, nil
}

func serveStandalone() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("Starting Telegram broker in standalone/HA mode")

	cfg, err := loadStandaloneConfig()
	if err != nil {
		log.Error("Configuration error", "error", err)
		os.Exit(1)
	}

	// Open Postgres store.
	pgStore, err := telegram.NewPostgresStore(cfg.DatabaseURL)
	if err != nil {
		log.Error("Failed to open Postgres store", "error", err)
		os.Exit(1)
	}
	log.Info("Postgres store opened")

	// Cast to access advisory lock methods.
	lockStore, ok := pgStore.(telegram.AdvisoryLocker)
	if !ok {
		log.Error("Postgres store does not support advisory locks")
		pgStore.Close()
		os.Exit(1)
	}

	// Create the broker with the Postgres store.
	broker := telegram.NewV2(log)
	brokerConfig := map[string]string{
		"bot_token":       cfg.BotToken,
		"hub_url":         cfg.HubURL,
		"hmac_key":        cfg.HMACKey,
		"broker_id":       cfg.BrokerID,
		"inbound_mode":    cfg.InboundMode,
		"webhook_url":     cfg.WebhookURL,
		"webhook_secret":  cfg.WebhookSecret,
		"webhook_listen":  cfg.WebhookListen,
		"api_base_url":    cfg.APIBaseURL,
		"agent_cache_ttl": cfg.AgentCacheTTL,
		"send_queue_size": cfg.SendQueueSize,
		"send_min_delay":  cfg.SendMinDelay,
		"database_url":    cfg.DatabaseURL,
	}

	// Set up signal handling for graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Webhook registration lock loop: only the lock holder calls setWebhook.
	// All instances process incoming webhook updates concurrently.
	go webhookLockLoop(ctx, log, lockStore, cfg, broker, brokerConfig)

	// Wait for shutdown signal.
	sig := <-sigCh
	log.Info("Received signal, shutting down", "signal", sig)
	cancel()

	// Graceful shutdown: close broker first, then store.
	if closeErr := broker.Close(); closeErr != nil {
		log.Warn("Error closing broker", "error", closeErr)
	}
	if closeErr := pgStore.Close(); closeErr != nil {
		log.Warn("Error closing store", "error", closeErr)
	}

	log.Info("Shutdown complete")
}

func webhookLockLoop(ctx context.Context, log *slog.Logger, lockStore telegram.AdvisoryLocker, cfg *standaloneConfig, broker *telegram.TelegramBrokerV2, brokerConfig map[string]string) {
	const (
		lockRetryInterval = 30 * time.Second
		takeoverDelay     = 2 // consecutive ticks before takeover
	)

	var lockHandle *telegram.AdvisoryLockHandle
	consecutiveLocks := 0
	configured := false

	defer func() {
		if lockHandle != nil {
			if err := lockHandle.Release(context.Background()); err != nil {
				log.Warn("Error releasing advisory lock", "error", err)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		acquired, handle, err := lockStore.TryAdvisoryLock(ctx, int64(store.LockTelegramWebhook))
		if err != nil {
			log.Error("Advisory lock attempt failed", "error", err)
			sleepOrCancel(ctx, lockRetryInterval)
			continue
		}

		if !acquired {
			if lockHandle != nil {
				log.Info("Lost webhook registration lock, entering standby")
				if err := lockHandle.Release(ctx); err != nil {
					log.Warn("Error releasing old lock handle", "error", err)
				}
				lockHandle = nil
			}
			consecutiveLocks = 0
			log.Debug("Webhook lock not acquired, standby", "retry_in", lockRetryInterval)
			sleepOrCancel(ctx, lockRetryInterval)
			continue
		}

		// Release old handle if we re-acquired.
		if lockHandle != nil {
			lockHandle.Release(ctx)
		}
		lockHandle = handle
		consecutiveLocks++

		if consecutiveLocks < takeoverDelay {
			log.Info("Lock acquired, waiting for takeover confirmation", "tick", consecutiveLocks, "of", takeoverDelay)
			sleepOrCancel(ctx, lockRetryInterval)
			continue
		}

		if !configured {
			log.Info("Webhook lock confirmed, configuring broker and registering webhook")
			if err := broker.Configure(brokerConfig); err != nil {
				log.Error("Failed to configure broker", "error", err)
				sleepOrCancel(ctx, lockRetryInterval)
				continue
			}
			configured = true
			log.Info("Broker configured and webhook registered")
		}

		// Stay in lock-holding state, periodically verify.
		sleepOrCancel(ctx, lockRetryInterval)
	}
}

func sleepOrCancel(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
