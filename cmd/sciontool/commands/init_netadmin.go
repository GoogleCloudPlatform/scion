/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/netcap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Network setup ordering in RunInit
//
// Before any credential is written or staged, metadataProtectionPreflight
// refuses the start when the runtime marked the metadata interception
// required (block or assign mode) but this container cannot install it.
//
// Everything that needs CAP_NET_ADMIN runs first, in init itself, as root:
//
//  1. the opt-in egress firewall (SCION_INIT_FIREWALL, runInitFirewall);
//  2. the metadata server start, which installs the interception rules
//     (metadata.Server.Start);
//  3. the drop of CAP_NET_ADMIN (dropNetAdminForMetadata).
//
// Only then do the git clone, pre-start hooks, sidecar services and the
// harness run, none of which need network administration. Because the
// capability is gone from the bounding set, processes started after step 3
// do not have it, including setuid-root programs. The firewall runs before
// the metadata start because init-firewall.sh resets the nat and filter
// tables, which would otherwise remove the interception rules.

// initFirewallEnv opts in to running the egress firewall script at init.
// It is parsed with strconv.ParseBool; any value that does not parse is a
// fatal error rather than a silent "off", because the operator asked for a
// restriction.
const initFirewallEnv = "SCION_INIT_FIREWALL"

// initFirewallScript is resolved through rootexec.Resolve (root-owned
// system directories only), never through the ambient PATH.
const initFirewallScript = "init-firewall.sh"

// initFirewallTimeout bounds the script: it resolves DNS names and fetches
// GitHub's published address ranges.
const initFirewallTimeout = 2 * time.Minute

// errInitFirewallFailed and errNetAdminDropFailed are the fixed, secret-free
// causes RunInit hands to reportInitFailure.
var (
	errInitFirewallFailed = errors.New("opt-in init firewall (" + initFirewallEnv + ") failed; refusing to start the harness")
	errNetAdminDropFailed = errors.New("could not drop " + netcap.CapabilityName + " after installing metadata interception; refusing to start the harness")

	// errMetadataProtectionUnavailable is the Hub-visible refusal when
	// the metadata interception is required but cannot be set up. The
	// text is fixed and pinned by a golden test.
	//nolint:staticcheck // ST1005: fixed user-facing refusal text, pinned verbatim by the golden test.
	errMetadataProtectionUnavailable = errors.New("agent start refused: the configured metadata protection could not be set up in this container. Provide the required host capabilities, or set the metadata mode to passthrough explicitly to opt out.")
)

// initFirewallRequested reports whether SCION_INIT_FIREWALL asks for the
// firewall. Unset or empty means no.
func initFirewallRequested() (bool, error) {
	raw := strings.TrimSpace(os.Getenv(initFirewallEnv))
	if raw == "" {
		return false, nil
	}
	on, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s is not a boolean", initFirewallEnv)
	}
	return on, nil
}

// runInitFirewall runs the egress firewall script as root. A package-level
// func var only so tests can observe ordering and inject failure.
var runInitFirewall = func() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s requires init to run as root", initFirewallScript)
	}
	path, err := rootexec.Resolve(initFirewallScript)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", initFirewallScript, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), initFirewallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Env = rootexec.Env()
	out, err := procreap.CombinedOutputManaged(cmd)
	if err != nil {
		log.Error("%s output:\n%s", initFirewallScript, string(out))
		return fmt.Errorf("%s: %w", initFirewallScript, err)
	}
	log.Info("Egress firewall installed by %s", initFirewallScript)
	return nil
}

// setupInitFirewall runs the firewall when requested. A non-nil return is
// fatal: RunInit must not start anything.
func setupInitFirewall() error {
	on, err := initFirewallRequested()
	if err != nil {
		log.Error("%v", err)
		return errInitFirewallFailed
	}
	if !on {
		return nil
	}
	if err := runInitFirewall(); err != nil {
		log.Error("Opt-in init firewall failed: %v", err)
		return errInitFirewallFailed
	}
	return nil
}

// runDropNetAdmin is netcap.DropNetAdmin behind a seam for tests. The
// mutation tests replace it with a no-op and expect failures.
var runDropNetAdmin = netcap.DropNetAdmin

// metadataInterceptionInstalled is (*metadata.Server).InterceptionInstalled
// behind a seam: tests stub the metadata server start, so the real method
// would always report false there.
var metadataInterceptionInstalled = (*metadata.Server).InterceptionInstalled

// dropNetAdminForMetadata drops CAP_NET_ADMIN from init and everything it
// starts later. RunInit calls it whenever the metadata mode is assign or
// block (passthrough never reaches it), after the metadata server start.
//
// netcap.DropNetAdmin is a no-op when no thread holds the capability, so a
// start that was never granted it (gVisor, Cloud Run, restricted pods)
// passes here without making any prctl or capset call.
//
// If interception rules were installed, a failed drop is fatal, keeping
// interception setup and the drop all-or-nothing. If interception was
// legitimately skipped (non-root, host networking), the drop is still
// attempted as hardening, but a failure only warns, which keeps today's
// start behaviour for that case.
func dropNetAdminForMetadata(interceptionInstalled bool) error {
	err := runDropNetAdmin()
	if err == nil {
		return nil
	}
	if interceptionInstalled {
		log.Error("Metadata interception is installed but %v", err)
		return errNetAdminDropFailed
	}
	log.Warn("Could not drop %s (metadata interception is not installed; continuing): %v", netcap.CapabilityName, err)
	return nil
}

// Seams for metadataProtectionPreflight. Tests replace them; the defaults
// check the real process.
var (
	preflightEUID = os.Geteuid
	// preflightResolveIPTables resolves iptables the same way the install
	// does (metadata's iptablesCmd: rootexec.Resolve, root-owned system
	// directories only).
	preflightResolveIPTables = func() error {
		_, err := rootexec.Resolve("iptables")
		return err
	}
	preflightNetAdminUsable = netcap.Usable
)

// metadataProtectionPreflight refuses the start, before any credential is
// written or staged, when cfg requires the metadata interception (the
// runtime granted the capability for it and set the required marker, in
// block or assign mode) but this container cannot install it: init is not
// root, the container shares the host network namespace, iptables does not
// resolve, or CAP_NET_ADMIN is not in the effective and bounding sets.
//
// A nil cfg (passthrough, or no metadata mode) and a cfg without the
// marker (Kubernetes, rootless Podman, the Apple runtime, host networking:
// runtimes that never install the interception) pass unchanged.
//
// These are the failures init can see in advance. Anything only the real
// install shows (a kernel without the nat table, say) is still caught by
// the fatal metadata start in RunInit, after staging; see
// removeTransportTokenOnRefusal for what that later exit cleans up.
func metadataProtectionPreflight(cfg *metadata.Config) error {
	if cfg == nil || !cfg.InterceptionRequired {
		return nil
	}
	reason := ""
	switch {
	case preflightEUID() != 0:
		reason = "init is not running as root"
	case cfg.NetworkMode == "host":
		reason = "the container shares the host network namespace"
	default:
		if err := preflightResolveIPTables(); err != nil {
			reason = fmt.Sprintf("iptables is not available: %v", err)
		} else if usable, err := preflightNetAdminUsable(); err != nil {
			reason = fmt.Sprintf("cannot check %s: %v", netcap.CapabilityName, err)
		} else if !usable {
			reason = netcap.CapabilityName + " is not in the effective and bounding sets"
		}
	}
	if reason == "" {
		return nil
	}
	// The reason goes to the local log only; the Hub sees the fixed text.
	log.Error("Metadata protection (mode=%s) cannot be set up: %s", cfg.Mode, reason)
	return errMetadataProtectionUnavailable
}

// removeTransportTokenOnRefusal removes the transport token file and its
// environment pointer when RunInit refuses the start after staging (a
// fatal metadata start). It acts only when this init staged the file:
// stageTransportToken sets the pointer, and it survives the re-exec.
// RunInit calls it after reportInitFailure, which may still need the
// credential to reach the Hub. The staged hub credential is removed
// separately, by finishStagedHubToken, and only when this init staged it.
//
// Deliberately left in place, as on every other init failure after
// staging: file secrets written to their payload targets and the harness
// secrets directory. After the environ-purge re-exec init no longer has
// their names, and on Kubernetes the runtime fills the harness secrets
// directory itself. No workload has run at this point.
func removeTransportTokenOnRefusal() {
	if os.Getenv(transportauth.EnvTransportTokenFile) == "" {
		return
	}
	if removed, err := hub.RemoveTransportTokenFile(); err != nil {
		log.Error("Failed to remove the transport token file after refusing the start: %v", err)
	} else if removed {
		log.Info("Removed the transport token file after refusing the start")
	}
	_ = os.Unsetenv(transportauth.EnvTransportTokenFile)
}
