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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
)

// warnRemovedLegacyEnvOnce guards the CLI's legacy-env warning so it prints
// at most once per process, no matter how many commands PersistentPreRunE
// runs for (e.g. help text triggering a re-parse) or how many callers
// invoke maybeWarnRemovedLegacyEnv (Execute() and rootCmd.PersistentPreRunE
// both do).
var warnRemovedLegacyEnvOnce sync.Once

// warnRemovedLegacyEnv reports legacy environment variables that scion no
// longer reads, via stderrReporter. This is the CLI half of the
// legacy-migration hook points: hub server boot and runtime
// broker boot call config.WarnRemovedLegacyEnvOnce the same way with
// config.NewSlogReporter() instead, at their own boot hook points
// (cmd/server_foreground.go:runServerStart and
// pkg/runtimebroker/server.go:(*Server).Start).
func warnRemovedLegacyEnv() {
	warnRemovedLegacyEnvOnce.Do(func() {
		config.WarnRemovedLegacyEnv(os.Getenv, stderrReporter{})
	})
}

// bootsServerInProcess reports whether cmd is the "start" command inside the
// "server" or "runtime-broker" subtree (nil is treated as neither). Those
// are the only two commands that boot a pkg/hub.Server and/or a
// pkg/runtimebroker.Server in-process; every other command under either
// subtree (status, stop, restart, install, migrate, register, provide, ...)
// never boots a server and must keep warning on stderr like any other CLI
// command, since it has no other hook to report through. `server restart`
// only ever spawns a *new* `server start --foreground` process rather than
// booting in-process itself, so it falls through here too (and that new
// process's own Execute() call resolves to "start" and is correctly caught
// there instead). `runtime-broker start` is always caught by name, in
// daemon mode too: its `--foreground` path calls serverStartCmd.RunE
// directly (cmd/broker.go), which bypasses PersistentPreRunE entirely but
// not Execute()'s check, since the top-level invoked command is still
// "runtime-broker start".
//
// The two boot commands report the legacy-env warning via their own
// slog-based hook (config.WarnRemovedLegacyEnvOnce) instead of this one. If
// the CLI hook also fired there, a combined `scion server start --enable-hub
// --enable-runtime-broker` process would warn on stderr *and* slog for the
// same variable, and — once real on-disk migration also runs through this
// same hook — the CLI hook would perform the migration and report it on
// stderr before the slog-based hooks ever ran, so the structured log records
// operators are told to watch for (subsystem=layout-migration) would never
// contain it.
func bootsServerInProcess(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	return cmd.Name() == "start" &&
		(commandInSubtree(cmd, "server") || commandInSubtree(cmd, "runtime-broker"))
}

// maybeWarnRemovedLegacyEnv calls warnRemovedLegacyEnv unless
// bootsServerInProcess(cmd) is true. Called from two places: Execute() (the
// real entry point, before any settings/project resolution) and
// rootCmd.PersistentPreRunE (the deduplicated path for callers — including
// tests — that invoke rootCmd directly without going through Execute()).
func maybeWarnRemovedLegacyEnv(cmd *cobra.Command) {
	if bootsServerInProcess(cmd) {
		return
	}
	warnRemovedLegacyEnv()
}

// stderrReporter implements config.Reporter for the CLI: every event is one
// "scion: ..." line on stderr, never stdout, so --json and piped output
// stay clean.
type stderrReporter struct{}

func (stderrReporter) Migrated(old, new string, tracked bool) {
	msg := fmt.Sprintf("scion: migrated %s -> %s", old, new)
	if tracked {
		msg += " (file is tracked by git; commit the rename)"
	}
	fmt.Fprintln(os.Stderr, msg)
}

func (stderrReporter) Conflict(old, new, detail string) {
	fmt.Fprintf(os.Stderr, "scion: warning: %s\n", detail)
}

func (stderrReporter) Skipped(old, reason, manual string) {
	fmt.Fprintf(os.Stderr, "scion: warning: cannot migrate %s (%s); run: %s\n", old, reason, manual)
}

func (stderrReporter) EnvIgnored(name, replacement string) {
	fmt.Fprintf(os.Stderr, "scion: %s is no longer read; set %s instead\n", name, replacement)
}
