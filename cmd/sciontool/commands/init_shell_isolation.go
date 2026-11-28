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
// never touch /run/sciontool/auth.
var runtimeTokenPathForInit = hub.DefaultRuntimeTokenPath

// initEffectiveUID returns the process's effective UID. Replaced only in unit
// tests to exercise root-init vs rootless token-store selection.
var initEffectiveUID = os.Geteuid

// runChownTokenFile is hub.ChownTokenFile; replaced only in unit tests to
// assert the ChownTokenFile skip under shellIsolation without CAP_CHOWN.
var runChownTokenFile = hub.ChownTokenFile

// chownTokenFileForStartup chowns the canonical home token file to
// targetUID:targetGID in non-isolated mode. Under shellIsolation the token
// lives in /run/sciontool/auth/scion-token (0600 root:root) or memory only,
// so ChownTokenFile is skipped.
func chownTokenFileForStartup(shellIsolation bool, token string, targetUID, targetGID int) {
	if !shellIsolation && token != "" && targetUID > 0 {
		if err := runChownTokenFile(targetUID, targetGID); err != nil {
			log.Error("Failed to chown token file to UID=%d: %v", targetUID, err)
		}
	}
}

// configureShellTokenStoreMode selects the isolated runtime token store mode
// (TokenStoreRuntimeFile on root-init runtimes when /run/sciontool/auth is
// writable, otherwise TokenStoreMemory with a warning; never TokenStoreHome).
// Safe to call idempotently before and after re-exec.
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

// setupShellIsolatedTokenStore configures the isolated runtime token store
// (root-only /run/sciontool/auth/scion-token on root-init runtimes, or
// memory-only on rootless/shared-UID runtimes or unwritable /run), takes and
// unconditionally unlinks the broker handoff files (~/.scion/scion-token and
// ~/.scion/transport-token), and removes any stale .tmp files.
//
// This runs AFTER the environ-purge re-exec so no in-memory state is lost
// across syscall.Exec and any bootstrap SCION_TRANSPORT_TOKEN has already been
// staged to the handoff path and purged from /proc/1/environ.
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

	// Transport token take-and-store (IAP / Cloud Run invoker): always take
	// and unconditionally unlink ~/.scion/transport-token so no stale or
	// workload-visible file remains. Seed it into the runtime store ONLY when
	// this start had a hub-provided transport credential staged pre-exec
	// (signalled by SCION_TRANSPORT_TOKEN_FILE surviving the re-exec); a stale
	// file on a persistent home with no transport env is unlinked and discarded.
	hasStagedTransport := os.Getenv(transportauth.EnvTransportTokenFile) != ""
	tTok, tErr := hub.TakeHandoffTransportToken()
	if tErr != nil {
		if errors.Is(tErr, fs.ErrNotExist) {
			// No handoff transport token present.
		} else if errors.Is(tErr, hub.ErrHandoffUnlinkFailed) {
			return fmt.Errorf("cannot isolate handoff transport token: %w", tErr)
		} else {
			log.Warn("Ignored and removed invalid handoff transport token entry: %v", tErr)
		}
	} else if hasStagedTransport && tTok != "" {
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
		if rtPath := hub.TransportTokenFilePath(); rtPath != "" {
			_ = os.Setenv(transportauth.EnvTransportTokenFile, rtPath)
		} else {
			_ = os.Unsetenv(transportauth.EnvTransportTokenFile)
		}
	} else if !hasStagedTransport && tTok != "" {
		log.Info("Discarded stale handoff transport token file (no transport token provided for this start)")
	}

	if err := hub.RemoveStaleHandoffTransportTmpFile(); err != nil {
		return fmt.Errorf("cannot remove stale handoff transport token temp file: %w", err)
	}
	return nil
}

// reconcileShellHandoffBeforeSupervisorRun re-runs the handoff take step
// immediately before sup.Run and then verifies that both handoff paths are
// absent.
//
// It NEVER adopts a late agent-token handoff directly (sidecar services and
// pre-start hooks have already run as UID scion in this window, so a file-origin
// candidate must still pass the normal file-change hub check + verified-anchor
// check, or be adopted by a broker SIGUSR2). Instead it takes and unlinks the
// file and returns the raw value as pendingAgentToken so authResetter can hold
// it in PID 1 memory as pendingCandidate. Init fails closed only if an unlink
// or absence check fails.
func reconcileShellHandoffBeforeSupervisorRun(hubClient *hub.Client) (string, error) {
	var pendingAgentToken string
	tok, err := hub.TakeHandoffToken()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Normal case: no late handoff arrived.
		} else if errors.Is(err, hub.ErrHandoffUnlinkFailed) {
			return "", fmt.Errorf("cannot remove late handoff token: %w", err)
		} else {
			log.Warn("Ignored and removed invalid late handoff token entry: %v", err)
		}
	} else if tok != "" {
		pendingAgentToken = tok
		log.Info("Deferred late handoff credential to authResetter for validation before adoption")
	}
	if err := hub.RemoveStaleHandoffTmpFile(); err != nil {
		return "", fmt.Errorf("cannot remove late stale handoff token temp file: %w", err)
	}

	// Re-run transport handoff take as well.
	if hubClient != nil && hubClient.HasHubProvidedTransport() {
		if adopted, terr := hubClient.AdoptTransportTokenFile(0, 0); terr != nil {
			if errors.Is(terr, hub.ErrHandoffUnlinkFailed) {
				return "", fmt.Errorf("cannot remove late handoff transport token: %w", terr)
			}
			log.Warn("Ignored and removed invalid late handoff transport token: %v", terr)
		} else if adopted {
			log.Info("Adopted late handoff transport credential before workload start")
		}
	} else {
		if _, terr := hub.TakeHandoffTransportToken(); terr != nil {
			if !errors.Is(terr, fs.ErrNotExist) {
				if errors.Is(terr, hub.ErrHandoffUnlinkFailed) {
					return "", fmt.Errorf("cannot remove late handoff transport token: %w", terr)
				}
				log.Warn("Ignored and removed invalid late handoff transport token entry: %v", terr)
			}
		} else {
			log.Warn("Discarded late handoff transport credential before workload start (no hub-provided transport)")
		}
	}
	if err := hub.RemoveStaleHandoffTransportTmpFile(); err != nil {
		return "", fmt.Errorf("cannot remove late stale handoff transport token temp file: %w", err)
	}

	if err := verifyShellHandoffTokenAbsent(); err != nil {
		return "", err
	}
	return pendingAgentToken, nil
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
// if and only if the file does not already exist. In proxy transport mode
// (SCION_TRANSPORT_MODE in {iap, cloudrun_invoker}), it also seeds
// hubs.<nick>.transport {mode, audience} from PID 1's environment without ever
// copying PID 1's transport token. Writes use dirfd no-follow + O_EXCL with
// mode 0600 and owner uid:gid so a pre-existing file (on a persistent home) or
// planted symlink is never overwritten or followed.
func seedShellCLIContext(agentHome string, uid, gid int, getenv func(string) string) error {
	if getenv == nil {
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

	opts := clicontextseed.Options{Endpoint: rawEndpoint}
	if mode := strings.TrimSpace(getenv(transportauth.EnvTransportMode)); transportauth.IsProxyMode(mode) {
		audience := strings.TrimSpace(getenv(transportauth.EnvTransportAudience))
		if audience == "" {
			audience = strings.TrimSpace(getenv(transportauth.EnvHubOIDCAudience))
		}
		opts.Transport = &clicontextseed.TransportOptions{
			Mode:     mode,
			Audience: audience,
		}
	}

	data, err := clicontextseed.Render(opts)
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
