// scion-plugin-slack is the Slack message broker plugin for scion.
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

	"github.com/GoogleCloudPlatform/scion/extras/scion-slack/internal/slack"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	goplugin "github.com/hashicorp/go-plugin"
)

func main() {
	if os.Getenv(plugin.MagicCookieKey) == plugin.MagicCookieValue {
		servePlugin()
		return
	}

	fmt.Println("scion-plugin-slack: Slack message broker plugin for Scion")
	fmt.Println()
	fmt.Println("This binary is intended to be launched by the Scion plugin manager.")
	fmt.Println("It communicates with the Slack API to provide bidirectional")
	fmt.Println("messaging between Slack channels and Scion agents.")
	fmt.Println()
	fmt.Println("Configuration keys:")
	fmt.Println("  bot_token        (required) Slack bot token (xoxb-...)")
	fmt.Println("  app_token        Slack app-level token (xapp-..., for Socket Mode)")
	fmt.Println("  signing_secret   Slack signing secret (for HTTP mode verification)")
	fmt.Println("  socket_mode      Use Socket Mode instead of HTTP (default: false)")
	fmt.Println("  listen_address   HTTP listen address (default: :3000)")
	fmt.Println("  hub_url          Hub API URL for inbound message delivery")
	fmt.Println("  hmac_key         Base64-encoded HMAC key for hub authentication")
	fmt.Println("  broker_id        Broker ID for HMAC signing")
	fmt.Println("  db_path          Path to SQLite database (default: slack.db)")
	fmt.Println("  agent_cache_ttl  TTL for cached agent list (default: 5m)")
	os.Exit(0)
}

func servePlugin() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	impl := slack.NewBroker(log)
	log.Info("Starting Slack broker plugin")

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
