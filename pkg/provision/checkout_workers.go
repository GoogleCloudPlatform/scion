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

package provision

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

const (
	// DefaultGitCheckoutWorkers is the number of parallel checkout workers
	// passed via `git -c checkout.workers=<N>` to git commands that write a
	// work tree: sciontool init's workspace checkout, and ProvisionShared's
	// shared clone and worktree add. On NFS-backed workspaces (e.g. Cloud Run
	// with Filestore, Kubernetes NFS shares), single-threaded checkout is
	// bottlenecked on per-file create/write/close RPC latency (~4.5x faster
	// at 16 workers on ~13.5k files), while on local disk 16 workers is
	// slightly faster or neutral.
	//
	// checkout.thresholdForParallelism is intentionally left at Git's
	// built-in default (100 files) so small checkouts avoid worker-process
	// spawn overhead while any nontrivial tree gets parallel I/O. Passing
	// checkout.workers via `-c` rather than `git config` keeps .git/config
	// clean under pkg/util/git_config_policy.go.
	DefaultGitCheckoutWorkers = 16
	// MaxGitCheckoutWorkers is the largest accepted override.
	MaxGitCheckoutWorkers = 64
	// EnvGitCheckoutWorkers overrides DefaultGitCheckoutWorkers: 0 uses the
	// CPU count git detects, 1 forces sequential checkout, 2..64 is an
	// explicit worker count.
	EnvGitCheckoutWorkers = "SCION_GIT_CHECKOUT_WORKERS"
)

// ParseGitCheckoutWorkers reads an EnvGitCheckoutWorkers value. An empty or
// blank value gives DefaultGitCheckoutWorkers and ok; a value that is not an
// integer in 0..MaxGitCheckoutWorkers gives DefaultGitCheckoutWorkers and
// !ok, so the caller can warn in its own log.
func ParseGitCheckoutWorkers(raw string) (n int, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultGitCheckoutWorkers, true
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 0 || n > MaxGitCheckoutWorkers {
		return DefaultGitCheckoutWorkers, false
	}
	return n, true
}

// checkoutWorkersConfig returns the checkout.workers=<N> setting, from
// EnvGitCheckoutWorkers in this process's environment, for a host-side git
// command that writes a work tree. It is always passed with -c, never
// written to a repository config.
func checkoutWorkersConfig() string {
	raw := os.Getenv(EnvGitCheckoutWorkers)
	n, ok := ParseGitCheckoutWorkers(raw)
	if !ok {
		slog.Warn("invalid parallel checkout worker count, using the default",
			"env", EnvGitCheckoutWorkers, "value", raw, "max", MaxGitCheckoutWorkers, "default", n)
	}
	return fmt.Sprintf("checkout.workers=%d", n)
}
