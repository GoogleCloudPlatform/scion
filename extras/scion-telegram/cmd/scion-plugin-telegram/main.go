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
//   - A standalone binary that prints usage information
//
// Plugin mode is auto-detected via the SCION_PLUGIN magic cookie environment variable.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/extras/scion-telegram/internal/telegram"
	goplugin "github.com/hashicorp/go-plugin"
)

func main() {
	// If the magic cookie is set, run as a go-plugin subprocess
	if os.Getenv(plugin.MagicCookieKey) == plugin.MagicCookieValue {
		servePlugin()
		return
	}

	// Otherwise, print usage information
	fmt.Println("scion-plugin-telegram: Telegram message broker plugin for Scion")
	fmt.Println()
	fmt.Println("This binary is intended to be launched by the Scion plugin manager.")
	fmt.Println("It communicates with the Telegram Bot API to provide bidirectional")
	fmt.Println("messaging between Telegram chats and Scion agents.")
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
