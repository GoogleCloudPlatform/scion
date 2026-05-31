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

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/spf13/cobra"
)

func runServerInstall(cmd *cobra.Command, args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find scion executable: %w", err)
	}

	// Resolve to absolute path
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	switch goos := goruntime.GOOS; goos {
	case "linux":
		return generateSystemdUnit(executable, serverInstallHosted)
	case "darwin":
		return generateLaunchdPlist(executable, serverInstallHosted)
	default:
		return fmt.Errorf("unsupported platform %q; only linux (systemd) and darwin (launchd) are supported", goos)
	}
}

func generateSystemdUnit(executable string, hosted bool) error {
	args := "server start --foreground"
	if hosted {
		args = "server start --foreground --hosted"
	}

	description := "Scion Workstation Server"
	if hosted {
		description = "Scion Server (Hosted)"
	}

	unit := fmt.Sprintf(`[Unit]
Description=%s
After=network.target docker.service

[Service]
Type=simple
ExecStart=%s %s
ExecStop=%s server stop
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, description, executable, args, executable)

	fmt.Print(unit)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To install as a systemd user service:")
	fmt.Fprintln(os.Stderr, "  mkdir -p ~/.config/systemd/user")
	fmt.Fprintln(os.Stderr, "  scion server install > ~/.config/systemd/user/scion-server.service")
	fmt.Fprintln(os.Stderr, "  systemctl --user daemon-reload")
	fmt.Fprintln(os.Stderr, "  systemctl --user enable --now scion-server")
	return nil
}

func generateLaunchdPlist(executable string, hosted bool) error {
	args := []string{executable, "server", "start", "--foreground"}
	if hosted {
		args = append(args, "--hosted")
	}

	// Build ProgramArguments XML entries
	var argEntries string
	for _, arg := range args {
		argEntries += fmt.Sprintf("        <string>%s</string>\n", arg)
	}

	label := "io.scion.server"
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/scion-server.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/scion-server.log</string>
</dict>
</plist>
`, label, argEntries)

	fmt.Print(plist)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To install as a launchd user agent:")
	fmt.Fprintln(os.Stderr, "  scion server install > ~/Library/LaunchAgents/io.scion.server.plist")
	fmt.Fprintln(os.Stderr, "  launchctl load ~/Library/LaunchAgents/io.scion.server.plist")
	return nil
}
