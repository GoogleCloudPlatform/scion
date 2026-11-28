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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config/clicontextseed"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// resolveShellStartCmdArgs resolves a FUSE start wrapper's "$SCION_START_CMD"
// reference (state.FUSEStartCmdExecSuffix) from PID 1's own environment before
// sup.Run launches the child with WorkloadIsolation enabled (where all SCION_*
// variables, including SCION_START_CMD, are stripped from the child
// environment).
//
// Passing the value as positional argv ($1) via `exec sh -c "$1" sh <cmd>`
// avoids shell-quoting issues, keeps supervisor.stripScionControlEnv's
// empty-allowlist invariant intact across all env builders, works identically
// with old and new runtime wrappers (no version skew), and leaves non-FUSE,
// Kubernetes, and Cloud Run argv untouched.
func resolveShellStartCmdArgs(childArgs []string, getenv func(string) string) []string {
	if getenv == nil || len(childArgs) != 3 || childArgs[0] != "sh" || childArgs[1] != "-c" {
		return childArgs
	}
	if !strings.HasSuffix(childArgs[2], state.FUSEStartCmdExecSuffix) {
		return childArgs
	}
	startCmd := getenv("SCION_START_CMD")
	rewritten := strings.TrimSuffix(childArgs[2], state.FUSEStartCmdExecSuffix) + `exec sh -c "$1"`
	return []string{"sh", "-c", rewritten, "sh", startCmd}
}

// hasUnresolvedScionEnvRef reports whether any element of args still contains
// a shell reference to a SCION_* environment variable ($SCION_ or ${SCION_}).
// Used under shellIsolation after resolveShellStartCmdArgs to detect wrapper
// drift that would otherwise expand to empty strings once SCION_* is stripped.
func hasUnresolvedScionEnvRef(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, "$SCION_") || strings.Contains(a, "${SCION_") {
			return true
		}
	}
	return false
}

// beforeSupervisorRunForTest, when non-nil, fires synchronously in runInit
// immediately before sup.Run is launched so tests can inspect filesystem state
// and supervisor.Config at the exact pre-workload boundary. Always nil in
// production.
var beforeSupervisorRunForTest func(cfg supervisor.Config)

// beforeVerifyHandoffAbsentForTest, when non-nil, fires synchronously in
// runInit immediately before verifyShellHandoffTokenAbsent (after
// setupShellIsolatedTokenStore has already run) so tests can independently
// verify the pre-sup.Run fail-closed check. Always nil in production.
var beforeVerifyHandoffAbsentForTest func()

// shellContextSeedChown is the fd-based chown callback used when seeding
// context.yaml. Replaced only in unit tests so ownership handoff can be
// verified without CAP_CHOWN.
var shellContextSeedChown = dirfd.Fchown

// runtimeTokenPathForInit is the root-only runtime token file path used in
// isolated mode on root-init runtimes. Replaced only in unit tests so tests
// never touch /run/scion.
var runtimeTokenPathForInit = hub.DefaultRuntimeTokenPath

// initEffectiveUID returns the process's effective UID. Replaced only in unit
// tests to exercise root-init vs rootless token-store selection.
var initEffectiveUID = os.Geteuid

// configureShellTokenStoreMode selects the isolated runtime token store mode
// (TokenStoreRuntimeFile on root-init runtimes when /run/scion is writable,
// otherwise TokenStoreMemory with a warning; never TokenStoreHome). Safe to
// call idempotently before and after re-exec.
func configureShellTokenStoreMode(targetUID int, rootless bool) hub.TokenStoreMode {
	if hub.IsIsolatedTokenStore() {
		return hub.CurrentTokenStoreMode()
	}
	euid := initEffectiveUID()
	rootInit := euid == 0 && targetUID > 0 && !rootless
	if rootInit {
		if err := hub.ConfigureIsolatedTokenStore(hub.TokenStoreRuntimeFile, runtimeTokenPathForInit); err != nil {
			log.Warn("Failed to initialize runtime token store at %s (%v); falling back to in-memory token store", runtimeTokenPathForInit, err)
			_ = hub.ConfigureIsolatedTokenStore(hub.TokenStoreMemory, "")
			return hub.TokenStoreMemory
		}
		return hub.TokenStoreRuntimeFile
	}
	log.Warn("Running without root-to-workload UID boundary (euid=%d, targetUID=%d, rootless=%v); keeping runtime token in memory only", euid, targetUID, rootless)
	_ = hub.ConfigureIsolatedTokenStore(hub.TokenStoreMemory, "")
	return hub.TokenStoreMemory
}

// stageTransportTokenForShell stages the bootstrap SCION_TRANSPORT_TOKEN under
// shell isolation: on root-init runtimes it writes directly to the root-only
// runtime path (/run/scion/transport-token, 0600 root:root) and clears the env
// var so re-exec purges /proc/1/environ; in memory mode it never writes to
// $HOME and leaves the env var for post-re-exec memory ingestion.
func stageTransportTokenForShell(targetUID int, rootless bool) bool {
	mode := configureShellTokenStoreMode(targetUID, rootless)
	if mode != hub.TokenStoreRuntimeFile {
		return false
	}
	return stageTransportToken(0, 0)
}

// setupShellIsolatedTokenStore configures the isolated runtime token store
// (root-only /run/scion/scion-token on root-init runtimes, or memory-only on
// rootless/shared-UID runtimes or unwritable /run), takes and unconditionally
// unlinks the broker handoff files (~/.scion/scion-token and
// ~/.scion/transport-token), and removes any stale .tmp files.
//
// It returns a non-nil error only when a handoff file or stale temp file
// cannot be unlinked (fail-closed: init must refuse to start the workload).
func setupShellIsolatedTokenStore(targetUID int, rootless bool) error {
	configureShellTokenStoreMode(targetUID, rootless)

	tok, err := hub.TakeHandoffToken()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No handoff token present (e.g., local mode or no hub configured).
		} else if errors.Is(err, hub.ErrHandoffUnlinkFailed) {
			return fmt.Errorf("cannot isolate handoff token: %w", err)
		} else {
			// Entry was refused (symlink, FIFO, oversize, hardlink) and was
			// already unlinked by TakeHandoffToken.
			log.Warn("Ignored and removed invalid handoff token entry: %v", err)
		}
	} else if tok != "" {
		if werr := hub.WriteTokenFile(tok, 0, 0); werr != nil {
			if hub.CurrentTokenStoreMode() == hub.TokenStoreRuntimeFile {
				log.Warn("Failed to write runtime token file at %s (%v); falling back to in-memory token store", runtimeTokenPathForInit, werr)
				_ = hub.ConfigureIsolatedTokenStore(hub.TokenStoreMemory, "")
				_ = hub.WriteTokenFile(tok, 0, 0)
			} else {
				return fmt.Errorf("store isolated runtime token: %w", werr)
			}
		}
	}

	if err := hub.RemoveStaleHandoffTmpFile(); err != nil {
		return fmt.Errorf("cannot remove stale handoff token temp file: %w", err)
	}

	// Transport token take-and-store (IAP / Cloud Run invoker): apply the same
	// no-follow take + unconditional unlink to ~/.scion/transport-token and
	// persist only to /run/scion/transport-token (0600 root:root) or memory.
	if hub.CurrentTokenStoreMode() == hub.TokenStoreMemory {
		if envTok := os.Getenv(transportauth.EnvTransportToken); envTok != "" {
			_, _ = hub.SeedTransportTokenFile(envTok, 0, 0)
			_ = os.Unsetenv(transportauth.EnvTransportToken)
			_ = os.Unsetenv(transportauth.EnvTransportTokenExpiry)
		}
	}

	tTok, tErr := hub.TakeHandoffTransportToken()
	if tErr != nil {
		if errors.Is(tErr, fs.ErrNotExist) {
			// No handoff transport token present.
		} else if errors.Is(tErr, hub.ErrHandoffUnlinkFailed) {
			return fmt.Errorf("cannot isolate handoff transport token: %w", tErr)
		} else {
			log.Warn("Ignored and removed invalid handoff transport token entry: %v", tErr)
		}
	} else if tTok != "" {
		if _, werr := hub.SeedTransportTokenFile(tTok, 0, 0); werr != nil {
			if hub.CurrentTokenStoreMode() == hub.TokenStoreRuntimeFile {
				log.Warn("Failed to write runtime transport token file (%v); falling back to in-memory token store", werr)
				curAgentTok := hub.ReadTokenFile()
				_ = hub.ConfigureIsolatedTokenStore(hub.TokenStoreMemory, "")
				if curAgentTok != "" {
					_ = hub.WriteTokenFile(curAgentTok, 0, 0)
				}
				_ = hub.WriteTransportTokenFile(tTok, 0, 0)
			} else {
				return fmt.Errorf("store isolated transport token: %w", werr)
			}
		}
	}

	if err := hub.RemoveStaleHandoffTransportTmpFile(); err != nil {
		return fmt.Errorf("cannot remove stale handoff transport token temp file: %w", err)
	}
	return nil
}

// verifyShellHandoffTokenAbsent asserts that neither the agent token nor the
// transport token handoff path exists before the workload starts. Returns an
// error wrapping hub.ErrHandoffUnlinkFailed if either exists or cannot be
// inspected safely.
func verifyShellHandoffTokenAbsent() error {
	if err := hub.VerifyHandoffTokenAbsent(); err != nil {
		return err
	}
	return hub.VerifyHandoffTransportTokenAbsent()
}

// isShellExecContainer reports whether PID 1 is running inside a shell
// execution container (ptone/scion#9743), delegating to the canonical
// state.IsShellExecContainerEnv predicate shared with control.ExecEnabled.
// Evaluate once early in runInit and pass explicitly.
func isShellExecContainer(getenv func(string) string) bool {
	return state.IsShellExecContainerEnv(getenv)
}

// removeEnvFileNoFollow unlinks <agentHome>/.scion/scion-env through a
// no-follow parent directory fd so a symlinked .scion directory is refused and
// a symlink at scion-env is removed without following it. Missing .scion or
// missing scion-env is a no-op.
func removeEnvFileNoFollow(agentHome string) error {
	envPath := filepath.Join(agentHome, ".scion", "scion-env")
	dirFd, leaf, err := dirfd.OpenParentNoFollow(envPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open .scion dir for scion-env unlink: %w", err)
	}
	defer func() { _ = dirfd.Close(dirFd) }()

	if err := dirfd.UnlinkAt(dirFd, leaf); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("unlink %s: %w", envPath, err)
	}
	log.Debug("Removed stale scion-env file at %s", envPath)
	return nil
}

// seedShellCLIContext writes <agentHome>/.scion/context.yaml with the
// container's hub endpoint (SCION_HUB_ENDPOINT, falling back to SCION_HUB_URL)
// if and only if the file does not already exist. Writes use dirfd no-follow +
// O_EXCL with mode 0600 and owner uid:gid so a pre-existing file (on a
// persistent home) or planted symlink is never overwritten or followed.
func seedShellCLIContext(agentHome string, uid, gid int, getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	if mode := strings.TrimSpace(getenv(transportauth.EnvTransportMode)); transportauth.IsProxyMode(mode) {
		log.Debug("Skipping plain-endpoint CLI context seed in proxy transport mode %q", mode)
		return nil
	}
	rawEndpoint := strings.TrimSpace(getenv("SCION_HUB_ENDPOINT"))
	if rawEndpoint == "" {
		rawEndpoint = strings.TrimSpace(getenv("SCION_HUB_URL"))
	}
	if rawEndpoint == "" {
		log.Debug("Skipping CLI context seed: no SCION_HUB_ENDPOINT or SCION_HUB_URL")
		return nil
	}

	data, err := clicontextseed.Render(clicontextseed.Options{Endpoint: rawEndpoint})
	if err != nil {
		return err
	}

	scionDir := filepath.Join(agentHome, ".scion")
	scionDirFile, err := dirfd.EnsureDirNoFollow(scionDir, 0o755)
	if err != nil {
		return fmt.Errorf("ensure .scion dir for context.yaml seed: %w", err)
	}
	defer func() { _ = scionDirFile.Close() }()

	if uid > 0 {
		st, serr := scionDirOwnerUID(int(scionDirFile.Fd()))
		if serr != nil {
			log.Error("Failed to stat %s: %v", scionDir, serr)
		} else if st == 0 {
			if cerr := scionDirFile.Chown(uid, gid); cerr != nil {
				log.Error("Failed to chown %s: %v", scionDir, cerr)
			}
		}
	}

	contextPath := filepath.Join(scionDir, "context.yaml")
	if err := dirfd.WriteAtNoFollowWithChown(
		int(scionDirFile.Fd()),
		"context.yaml",
		contextPath,
		data,
		0o600,
		uid,
		gid,
		dirfd.CreateExclusive,
		shellContextSeedChown,
	); err != nil {
		if errors.Is(err, os.ErrExist) {
			log.Debug("CLI context already exists at %s; leaving untouched", contextPath)
			return nil
		}
		return fmt.Errorf("seed %s: %w", contextPath, err)
	}
	log.Debug("Seeded CLI context at %s", contextPath)
	return nil
}
