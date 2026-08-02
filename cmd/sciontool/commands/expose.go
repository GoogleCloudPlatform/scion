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

package commands

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/autoexpose"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/spf13/cobra"
)

var (
	exposeLabel string
	exposeHost  string
	exposeList  bool
)

var exposeCmd = &cobra.Command{
	Use:   "expose [port]",
	Short: "Expose an agent-local HTTP port through the Hub",
	Long: `Expose an agent-local HTTP port through the Hub so it can be accessed
via the Hub's proxy endpoint.

Ports can also be auto-exposed by setting SCION_AUTO_EXPOSE_PORTS=true in the
agent's environment. When auto-expose is enabled, listening TCP ports are
automatically detected and registered with the Hub. Use SCION_AUTO_EXPOSE_MODE
to control filtering (allowlist or denylist) and SCION_AUTO_EXPOSE_PORTS_LIST
to specify the port list for the active filter mode.

Use --list to see all currently exposed ports and the auto-expose status.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := scionhub.NewClient()
		if client == nil || !client.IsConfigured() {
			return fmt.Errorf("hub client not configured")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		if exposeList {
			// Print auto-expose status header.
			cfg := autoexpose.ConfigFromEnv()
			if cfg.Enabled {
				_, _ = fmt.Fprintf(os.Stdout, "Auto-expose: enabled (mode: %s, interval: %s)\n", cfg.FilterMode, cfg.Interval)
			} else {
				_, _ = fmt.Fprintln(os.Stdout, "Auto-expose: disabled")
			}

			ports, err := client.ListPorts(ctx)
			if err != nil {
				return err
			}
			if len(ports) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "No ports exposed.")
				return nil
			}
			for _, p := range ports {
				label := p.Label
				if label == "" {
					label = "-"
				}
				_, _ = fmt.Fprintf(os.Stdout, "%d\t%s\t%s\n", p.Port, label, p.URL)
			}
			return nil
		}
		if len(args) != 1 {
			return fmt.Errorf("port is required")
		}
		port, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid port %q", args[0])
		}
		resp, err := client.RegisterPort(ctx, scionhub.RegisterPortRequest{
			Port:  port,
			Label: exposeLabel,
			Host:  exposeHost,
		})
		if err != nil {
			// Treat 409 Conflict (already registered) as success — idempotent expose.
			if strings.Contains(err.Error(), "error 409:") {
				// Port is already registered; fetch current info to display.
				ports, listErr := client.ListPorts(ctx)
				if listErr != nil {
					_, _ = fmt.Fprintf(os.Stdout, "Port %d is already exposed.\n", port)
					return nil
				}
				for _, p := range ports {
					if p.Port == port {
						_, _ = fmt.Fprintf(os.Stdout, "Port %d is already exposed.\nURL: %s\nBase path: %s\n", p.Port, p.URL, p.BasePath)
						return nil
					}
				}
				_, _ = fmt.Fprintf(os.Stdout, "Port %d is already exposed.\n", port)
				return nil
			}
			return err
		}
		_, _ = fmt.Fprintf(os.Stdout, "Port %d exposed.\nURL: %s\nBase path: %s\n", resp.Port, resp.URL, resp.BasePath)
		return nil
	},
}

var unexposeCmd = &cobra.Command{
	Use:   "unexpose <port>",
	Short: "Stop exposing an agent-local HTTP port through the Hub",
	Long: `Stop exposing an agent-local HTTP port through the Hub.

If the port is not currently exposed, this command succeeds silently. This
makes it safe to call repeatedly (idempotent).

Note: if auto-expose is enabled (SCION_AUTO_EXPOSE_PORTS=true), an unexposed
port may be re-exposed automatically on the next scan cycle if it is still
listening and passes the filter rules.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := scionhub.NewClient()
		if client == nil || !client.IsConfigured() {
			return fmt.Errorf("hub client not configured")
		}
		port, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid port %q", args[0])
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		if err := client.DeletePort(ctx, port); err != nil {
			// Treat 404 (not found) as success — idempotent unexpose.
			if strings.Contains(err.Error(), "error 404:") {
				_, _ = fmt.Fprintf(os.Stdout, "Port %d is not currently exposed.\n", port)
				return nil
			}
			return err
		}
		_, _ = fmt.Fprintf(os.Stdout, "Port %d unexposed.\n", port)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(exposeCmd)
	rootCmd.AddCommand(unexposeCmd)
	exposeCmd.Flags().StringVar(&exposeLabel, "label", "", "Label for the exposed port")
	exposeCmd.Flags().StringVar(&exposeHost, "host", "127.0.0.1", "Local host to forward to")
	exposeCmd.Flags().BoolVar(&exposeList, "list", false, "List exposed ports")
}
