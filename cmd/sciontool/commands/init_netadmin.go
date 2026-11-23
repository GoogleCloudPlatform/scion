/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
//  3. the drop of CAP_NET_ADMIN (dropNetAdmin), whenever a metadata
//     server was configured (assign or block mode) or the firewall was
//     installed.
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
	errNetAdminDropFailed = errors.New("could not drop " + netcap.CapabilityName + " after installing network controls; refusing to start the harness")

	// errMetadataProtectionUnavailable is the Hub-visible refusal when
	// the metadata interception is required but cannot be set up. The
	// text is fixed and pinned by a golden test.
	//nolint:staticcheck // ST1005: fixed user-facing refusal text, pinned verbatim by the golden test.
	errMetadataProtectionUnavailable = errors.New(metadata.ProtectionUnavailableRefusal)

	// errMetadataInterceptionMarkerMissing is the Hub-visible refusal when
	// interception is required, the runtime-managed source marker is missing
	// or unrecognized, and the legacy in-container fallback cannot run. The
	// text is fixed and pinned by a golden test.
	//nolint:staticcheck // ST1005: fixed user-facing refusal text, pinned verbatim by the golden test.
	errMetadataInterceptionMarkerMissing = errors.New(metadata.InterceptionMarkerMissingRefusal)
)

// Seams for metadataProtectionPreflight and legacy in-container rule install.
var (
	preflightEUID               = os.Geteuid
	preflightNetAdminUsable     = netcap.Usable
	runInstallInterceptionRules = metadata.InstallInterceptionRules
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

// setupInitFirewall runs the firewall when requested and reports whether
// it was installed. A non-nil error is fatal: RunInit must not start
// anything.
func setupInitFirewall() (installed bool, err error) {
	on, err := initFirewallRequested()
	if err != nil {
		log.Error("%v", err)
		return false, errInitFirewallFailed
	}
	if !on {
		return false, nil
	}
	if err := runInitFirewall(); err != nil {
		log.Error("Opt-in init firewall failed: %v", err)
		return false, errInitFirewallFailed
	}
	return true, nil
}

// runDropNetAdmin is netcap.DropNetAdmin behind a seam for tests. The
// mutation tests replace it with a no-op and expect failures.
var runDropNetAdmin = netcap.DropNetAdmin

// dropNetAdmin drops CAP_NET_ADMIN from init and everything it starts
// later. RunInit calls it after network setup whenever a metadata server
// was configured (assign or block mode) or the opt-in firewall was
// installed.
//
// netcap.DropNetAdmin is a no-op when no thread holds the capability, so a
// start that was never granted it (gVisor, Cloud Run, restricted pods)
// passes here without making any prctl or capset call.
//
// required is true when init installed network controls: interception
// rules or the firewall. Then a failed drop is fatal, keeping that setup
// and the drop all-or-nothing. Otherwise (interception legitimately
// skipped for non-root or host networking, and no firewall) the drop is
// still attempted as hardening, but a failure only warns.
func dropNetAdmin(required bool) error {
	err := runDropNetAdmin()
	if err == nil {
		return nil
	}
	if required {
		log.Error("Network controls are installed but %v", err)
		return errNetAdminDropFailed
	}
	log.Warn("Could not drop %s (no network controls are installed; continuing): %v", netcap.CapabilityName, err)
	return nil
}

// metadataProtectionPreflight refuses the start, before any credential is
// written or staged, when cfg requires metadata interception
// (SCION_METADATA_INTERCEPTION=required) but:
//   - the container shares the host network namespace;
//   - SCION_METADATA_INTERCEPTION_SOURCE is absent (legacy pre-#5246 broker)
//     and init does not have root + usable CAP_NET_ADMIN for the in-container
//     fallback; or
//   - SCION_METADATA_INTERCEPTION_SOURCE holds an unrecognized value.
//
// When SCION_METADATA_INTERCEPTION_SOURCE=runtime, the broker installs rules
// from a short-lived setup container and init verifies them via probe, so no
// in-container CAP_NET_ADMIN is required.
//
// A nil cfg (passthrough, or no metadata mode) and a cfg without the
// required marker (Kubernetes, rootless Podman, the Apple runtime, host
// networking) pass unchanged.
func metadataProtectionPreflight(cfg *metadata.Config) error {
	if cfg == nil || !cfg.InterceptionRequired {
		return nil
	}
	if cfg.NetworkMode == "host" {
		log.Error("Metadata protection (mode=%s) cannot be set up: the container shares the host network namespace", cfg.Mode)
		return errMetadataProtectionUnavailable
	}
	switch cfg.InterceptionSource {
	case metadata.InterceptionSourceRuntime:
		return nil
	case "":
		// Compatibility fallback for older (pre-#5246) brokers that grant
		// CAP_NET_ADMIN to the agent container instead of running a
		// netns-setup sidecar. Require root and usable CAP_NET_ADMIN before
		// staging any credentials; fail closed otherwise.
		if preflightEUID() != 0 {
			log.Error("Metadata protection (mode=%s) cannot be set up: runtime-managed interception marker (%s=%s) is not set and init is not root (euid=%d)",
				cfg.Mode, metadata.InterceptionSourceEnvVar, metadata.InterceptionSourceRuntime, preflightEUID())
			return errMetadataInterceptionMarkerMissing
		}
		usable, err := preflightNetAdminUsable()
		if err != nil {
			log.Error("Metadata protection (mode=%s) cannot be set up: runtime-managed interception marker (%s=%s) is not set and %s check failed: %v",
				cfg.Mode, metadata.InterceptionSourceEnvVar, metadata.InterceptionSourceRuntime, netcap.CapabilityName, err)
			return errMetadataInterceptionMarkerMissing
		}
		if !usable {
			log.Error("Metadata protection (mode=%s) cannot be set up: runtime-managed interception marker (%s=%s) is not set and %s is not usable",
				cfg.Mode, metadata.InterceptionSourceEnvVar, metadata.InterceptionSourceRuntime, netcap.CapabilityName)
			return errMetadataInterceptionMarkerMissing
		}
		return nil
	default:
		log.Error("Metadata protection (mode=%s) cannot be set up: unrecognized interception source %q (expected %s=%s)",
			cfg.Mode, cfg.InterceptionSource, metadata.InterceptionSourceEnvVar, metadata.InterceptionSourceRuntime)
		return errMetadataInterceptionMarkerMissing
	}
}

// maxProbeBodyBytes bounds the response body read during redirect probe
// verification so an unexpected responder cannot cause unbounded allocation.
const maxProbeBodyBytes = 128

// Seams and parameters for waitForRedirect and verifyIPv6MetadataBlocked.
var (
	redirectProbeBaseURL        = "http://169.254.169.254"
	redirectProbeAttemptTimeout = time.Second
	// redirectProbeDeadline must exceed pkg/runtime's netnsSetupTimeout (3m)
	// so init does not time out before the setup container finishes installing
	// rules.
	redirectProbeDeadline = 210 * time.Second
	// legacyRedirectProbeDeadline bounds the redirect probe on the legacy
	// in-container path, where rules are installed synchronously before probing.
	legacyRedirectProbeDeadline = 10 * time.Second
	redirectProbeInterval       = 100 * time.Millisecond
	runWaitForRedirect          = waitForRedirect

	ipv6ProbeAddrs   = defaultIPv6ProbeAddrs()
	ipv6ProbeTimeout = 2 * time.Second
	ipv6ProbeDial    = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	runNetnsHasIPv6      = metadata.NetnsHasIPv6
	runVerifyIPv6Blocked = verifyIPv6MetadataBlocked
)

func defaultIPv6ProbeAddrs() []string {
	addrs := metadata.IPv6MetadataAddrs()
	targets := make([]string, len(addrs))
	for i, addr := range addrs {
		targets[i] = net.JoinHostPort(addr, "80")
	}
	return targets
}

// waitForRedirect polls the metadata IP on the emulator's per-boot random
// probe path until the emulator answers HTTP 200 OK with the expected per-boot
// probe token, proving that the nat OUTPUT REDIRECT rule reaches this emulator
// instance. Returns errMetadataProtectionUnavailable if the deadline expires
// or ctx is cancelled.
func waitForRedirect(ctx context.Context, probePath, probeToken string) error {
	if probePath == "" || probeToken == "" {
		log.Error("Metadata redirect probe path or token is empty")
		return errMetadataProtectionUnavailable
	}
	wantToken := []byte(probeToken)
	client := &http.Client{
		Timeout: redirectProbeAttemptTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	url := redirectProbeBaseURL + probePath
	deadline := time.Now().Add(redirectProbeDeadline)
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			log.Error("Metadata redirect probe request build failed: %v", err)
			return errMetadataProtectionUnavailable
		}
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProbeBodyBytes+1))
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode != http.StatusOK:
				lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
			case readErr != nil:
				lastErr = fmt.Errorf("read probe body: %w", readErr)
			case len(body) > maxProbeBodyBytes:
				lastErr = fmt.Errorf("probe response body exceeded %d bytes", maxProbeBodyBytes)
			case subtle.ConstantTimeCompare(body, wantToken) == 1:
				log.Info("Metadata redirect verified via probe")
				return nil
			default:
				lastErr = errors.New("probe response token mismatch")
			}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			log.Error("Metadata redirect verification cancelled: %v", ctx.Err())
			return errMetadataProtectionUnavailable
		}
		if time.Now().After(deadline) {
			log.Error("Metadata redirect verification timed out after %s: %v", redirectProbeDeadline, lastErr)
			return errMetadataProtectionUnavailable
		}
		timer := time.NewTimer(redirectProbeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Error("Metadata redirect verification cancelled: %v", ctx.Err())
			return errMetadataProtectionUnavailable
		case <-timer.C:
		}
	}
}

// verifyIPv6MetadataBlocked performs a negative TCP connect probe against the
// IPv6 cloud metadata endpoints when IPv6 is enabled in the network namespace.
// Connection refusal, network/host unreachability, and timeout all pass; a
// successful TCP connection fails closed with errMetadataProtectionUnavailable.
func verifyIPv6MetadataBlocked(ctx context.Context) error {
	if !runNetnsHasIPv6() {
		return nil
	}
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		reachable string
	)
	for _, addr := range ipv6ProbeAddrs {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			dialCtx, cancel := context.WithTimeout(ctx, ipv6ProbeTimeout)
			defer cancel()
			conn, err := ipv6ProbeDial(dialCtx, "tcp", target)
			if err == nil {
				_ = conn.Close()
				mu.Lock()
				if reachable == "" {
					reachable = target
				}
				mu.Unlock()
			}
		}(addr)
	}
	wg.Wait()
	if reachable != "" {
		log.Error("IPv6 metadata endpoint %s is reachable; refusing to start the harness", reachable)
		return errMetadataProtectionUnavailable
	}
	log.Info("IPv6 metadata endpoints verified unreachable")
	return nil
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
