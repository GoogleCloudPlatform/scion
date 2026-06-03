// scion-plugin-discord is the Discord message broker plugin for scion.
// It can run as:
//   - A go-plugin subprocess (when launched by the scion plugin manager)
//   - A standalone binary that prints usage information
//
// Plugin mode is auto-detected via the SCION_PLUGIN magic cookie environment variable.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/GoogleCloudPlatform/scion/extras/scion-discord/internal/discord"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	goplugin "github.com/hashicorp/go-plugin"
)

func main() {
	// If the magic cookie is set, run as a go-plugin subprocess.
	if os.Getenv(plugin.MagicCookieKey) == plugin.MagicCookieValue {
		servePlugin()
		return
	}

	// Otherwise, print usage information.
	fmt.Println("scion-plugin-discord: Discord message broker plugin for Scion")
	fmt.Println()
	fmt.Println("This binary is intended to be launched by the Scion plugin manager.")
	fmt.Println("It communicates with the Discord Gateway API to provide bidirectional")
	fmt.Println("messaging between Discord channels and Scion agents.")
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
