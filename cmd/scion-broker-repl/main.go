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

// scion-broker-repl is the reference message broker plugin for scion.
// It can run as:
//   - A go-plugin subprocess (when launched by the scion plugin manager)
//   - A standalone REPL for manual message send/receive during development
//
// Plugin mode is auto-detected via the SCION_PLUGIN magic cookie environment variable.
//
// REPL usage:
//
//	scion-broker-repl --hub-url http://localhost:8080
//	repl> sub scion.grove.mygrove.agent.*.messages
//	repl> pub scion.grove.mygrove.agent.alice.messages hello world
//	repl> unsub scion.grove.mygrove.agent.*.messages
//	repl> quit
package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker"
	goplugin "github.com/hashicorp/go-plugin"
)

func main() {
	// If the magic cookie is set, run as a go-plugin subprocess
	if os.Getenv(plugin.MagicCookieKey) == plugin.MagicCookieValue {
		servePlugin()
		return
	}

	// Otherwise, run as a standalone REPL
	runREPL()
}

func servePlugin() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	impl := refbroker.New(log)

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

func runREPL() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	b := refbroker.New(log)
	defer func() { _ = b.Close() }()

	// Parse flags from args
	hubURL := ""
	for i, arg := range os.Args[1:] {
		if arg == "--hub-url" && i+1 < len(os.Args[1:]) {
			hubURL = os.Args[i+2]
		}
	}

	config := map[string]string{}
	if hubURL != "" {
		config["hub_url"] = hubURL
	}

	// Set up inbound handler to print received messages to stdout
	b.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		fmt.Printf("[inbound] %s: sender=%s msg=%s\n", topic, msg.Sender, msg.Msg)
	}

	if err := b.Configure(config); err != nil {
		fmt.Fprintf(os.Stderr, "configure error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("scion-broker-repl (reference message broker)")
	fmt.Println("Commands: sub <pattern>, unsub <pattern>, pub <topic> <message>, quit")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("repl> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("repl> ")
			continue
		}

		parts := strings.SplitN(line, " ", 3)
		cmd := parts[0]

		switch cmd {
		case "quit", "exit":
			fmt.Println("Bye.")
			return

		case "sub":
			if len(parts) < 2 {
				fmt.Println("Usage: sub <pattern>")
			} else {
				if err := b.Subscribe(parts[1]); err != nil {
					fmt.Printf("Error: %v\n", err)
				} else {
					fmt.Printf("Subscribed to: %s\n", parts[1])
				}
			}

		case "unsub":
			if len(parts) < 2 {
				fmt.Println("Usage: unsub <pattern>")
			} else {
				if err := b.Unsubscribe(parts[1]); err != nil {
					fmt.Printf("Error: %v\n", err)
				} else {
					fmt.Printf("Unsubscribed from: %s\n", parts[1])
				}
			}

		case "pub":
			if len(parts) < 3 {
				fmt.Println("Usage: pub <topic> <message>")
			} else {
				msg := messages.NewInstruction("repl:user", "broker:topic", parts[2])
				if err := b.PublishExternal(parts[1], msg); err != nil {
					fmt.Printf("Error: %v\n", err)
				} else {
					fmt.Printf("Published to: %s\n", parts[1])
				}
			}

		default:
			fmt.Printf("Unknown command: %s\n", cmd)
			fmt.Println("Commands: sub <pattern>, unsub <pattern>, pub <topic> <message>, quit")
		}

		fmt.Print("repl> ")
	}
}
