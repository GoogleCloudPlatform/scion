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

package metadata

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

const (
	metadataIP = "169.254.169.254"
	loopbackIP = "127.0.0.1"
)

// runIPTables invokes iptables with the given arguments and returns combined
// output. Overridden in unit tests to inspect exact rule arguments and
// ordering without requiring CAP_NET_ADMIN.
var runIPTables = execIPTables

func execIPTables(args ...string) ([]byte, error) {
	cmd, err := iptablesCmd(args...)
	if err != nil {
		return nil, err
	}
	return procreap.CombinedOutputManaged(cmd)
}

// ipv6MetadataAddrs are the cloud metadata service IPv6 endpoints (GCE, AWS)
// blocked on TCP/80 whenever IPv6 is enabled in the container network namespace.
var ipv6MetadataAddrs = []string{"fd20:ce::254", "fd00:ec2::254"}

// IPv6MetadataAddrs returns a copy of the cloud metadata service IPv6
// endpoints blocked on TCP/80 when IPv6 is enabled in the network namespace.
func IPv6MetadataAddrs() []string {
	return append([]string(nil), ipv6MetadataAddrs...)
}

// procRoot is the mount path of procfs; overridden in tests.
var procRoot = "/proc"

// NetnsHasIPv6 reports whether any non-loopback network interface in the
// current network namespace has IPv6 enabled (disable_ipv6 == 0).
func NetnsHasIPv6() bool {
	return netnsHasIPv6InRoot(procRoot)
}

func netnsHasIPv6InRoot(root string) bool {
	confDir := filepath.Join(root, "sys", "net", "ipv6", "conf")
	entries, err := os.ReadDir(confDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "all" || name == "default" || name == "lo" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(confDir, name, "disable_ipv6"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(raw)) == "0" {
			return true
		}
	}
	return false
}

// iptablesCmd resolves "iptables" through rootexec.Resolve — never the
// ambient PATH, which on substrate includes a directory the workload owns
// outright — and builds its environment from scratch: this runs as root,
// in-process in PID 1, and needs nothing beyond a fixed PATH to invoke a
// system binary with fixed arguments. A resolution failure is returned as
// the exec error every caller here already handles.
func iptablesCmd(args ...string) (*exec.Cmd, error) {
	path, err := rootexec.Resolve("iptables")
	if err != nil {
		return nil, fmt.Errorf("resolve iptables: %w", err)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = rootexec.Env()
	return cmd, nil
}

// ip6tablesCmd resolves "ip6tables" through rootexec.Resolve with a clean
// environment, matching iptablesCmd.
func ip6tablesCmd(args ...string) (*exec.Cmd, error) {
	path, err := rootexec.Resolve("ip6tables")
	if err != nil {
		return nil, fmt.Errorf("resolve ip6tables: %w", err)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = rootexec.Env()
	return cmd, nil
}

// setupIPTablesRedirect configures iptables to redirect traffic destined for
// the GCE metadata server IP (169.254.169.254) to the local metadata sidecar.
// This ensures tools that hardcode the metadata IP (e.g., curl) are intercepted.
//
// It also inserts a narrowly-scoped filter OUTPUT ACCEPT rule for the
// post-REDIRECT loopback target (127.0.0.1:<port>). In Linux netfilter's
// NF_INET_LOCAL_OUT hook, nat OUTPUT REDIRECT rewrites the destination to
// 127.0.0.1:<port> and reroutes the skb to lo, but state->out remains the
// pre-NAT egress interface (e.g., eth0) during filter OUTPUT evaluation. An
// egress firewall that only allows `-o lo` and appends a catch-all OUTPUT
// REJECT (such as init-firewall.sh) would otherwise reject the redirected
// metadata traffic with icmp-admin-prohibited ("no route to host"). Inserting
// the rule at position 1 allows only TCP traffic to 127.0.0.1:<port>, leaving
// all non-allowlisted egress (including un-redirected traffic to 169.254.169.254)
// rejected.
//
// Requires NET_ADMIN capability.
func setupIPTablesRedirect(port int) error {
	portStr := strconv.Itoa(port)

	// Add a DNAT rule: redirect TCP traffic to 169.254.169.254:80 to localhost:port
	natArgs := []string{
		"-t", "nat",
		"-A", "OUTPUT",
		"-d", metadataIP,
		"-p", "tcp",
		"--dport", "80",
		"-j", "REDIRECT",
		"--to-port", portStr,
	}

	output, err := runIPTables(natArgs...)
	if err != nil {
		return fmt.Errorf("iptables redirect setup failed: %w (output: %s)", err, string(output))
	}

	// Allow the redirected loopback flow in filter OUTPUT ahead of any
	// catch-all REJECT/DROP rule installed by an egress firewall.
	filterArgs := []string{
		"-I", "OUTPUT", "1",
		"-d", loopbackIP,
		"-p", "tcp",
		"--dport", portStr,
		"-j", "ACCEPT",
	}

	output, err = runIPTables(filterArgs...)
	if err != nil {
		// Roll back the nat rule so partial state is not left behind.
		cleanupNatArgs := []string{
			"-t", "nat",
			"-D", "OUTPUT",
			"-d", metadataIP,
			"-p", "tcp",
			"--dport", "80",
			"-j", "REDIRECT",
			"--to-port", portStr,
		}
		if delOut, delErr := runIPTables(cleanupNatArgs...); delErr != nil {
			log.Debug("iptables redirect rollback failed (non-fatal): %v (output: %s)", delErr, string(delOut))
		}
		return fmt.Errorf("iptables redirect filter accept setup failed: %w (output: %s)", err, string(output))
	}

	log.Info("iptables: redirecting %s:80 -> %s:%s", metadataIP, loopbackIP, portStr)
	return nil
}

// cleanupIPTablesRedirect removes the iptables redirect and filter accept rules.
func cleanupIPTablesRedirect(port int) {
	portStr := strconv.Itoa(port)
	natArgs := []string{
		"-t", "nat",
		"-D", "OUTPUT",
		"-d", metadataIP,
		"-p", "tcp",
		"--dport", "80",
		"-j", "REDIRECT",
		"--to-port", portStr,
	}
	if output, err := runIPTables(natArgs...); err != nil {
		log.Debug("iptables cleanup failed (non-fatal): %v (output: %s)", err, string(output))
	}

	filterArgs := []string{
		"-D", "OUTPUT",
		"-d", loopbackIP,
		"-p", "tcp",
		"--dport", portStr,
		"-j", "ACCEPT",
	}
	if output, err := runIPTables(filterArgs...); err != nil {
		log.Debug("iptables filter accept cleanup failed (non-fatal): %v (output: %s)", err, string(output))
	}
}

// blockMethod tracks which blocking mechanism was successfully applied.
type blockMethod int

const (
	blockNone     blockMethod = iota
	blockIPTables             // iptables REJECT rule in filter OUTPUT chain
)

// setupMetadataBlock blocks outbound HTTP traffic to the GCE metadata server IP.
// Only TCP port 80 is blocked — other traffic (notably DNS on UDP 53) is left
// alone because on GCE the metadata IP doubles as the default DNS resolver.
// It tries iptables first (REJECT rule in the filter table), then falls back to
// an iptables DROP on TCP/80. An ip-route fallback is intentionally NOT used
// because it would block all protocols including DNS.
func setupMetadataBlock() (blockMethod, error) {
	// Try iptables REJECT in the filter OUTPUT chain, scoped to TCP port 80.
	// This gives immediate "connection refused" feedback to the caller.
	rejectArgs := []string{
		"-A", "OUTPUT",
		"-d", metadataIP,
		"-p", "tcp",
		"--dport", "80",
		"-j", "REJECT",
		"--reject-with", "icmp-port-unreachable",
	}
	output, err := runIPTables(rejectArgs...)
	if err == nil {
		log.Info("iptables: blocking TCP/80 traffic to %s (REJECT)", metadataIP)
		return blockIPTables, nil
	}
	log.Debug("iptables REJECT failed: %v (output: %s)", err, string(output))

	return blockNone, fmt.Errorf("metadata blocking failed: iptables: %v", err)
}

// runIP6Tables runs ip6tables via ip6tablesCmd; overridden in unit tests to
// inspect exact arguments without requiring CAP_NET_ADMIN.
var runIP6Tables = func(args ...string) ([]byte, error) {
	cmd, err := ip6tablesCmd(args...)
	if err != nil {
		return nil, err
	}
	return procreap.CombinedOutputManaged(cmd)
}

// setupIPv6MetadataBlock blocks outbound TCP/80 traffic to IPv6 cloud metadata
// endpoints in the filter OUTPUT chain using tcp-reset. Non-HTTP traffic
// (such as DNS) is untouched.
func setupIPv6MetadataBlock() error {
	for _, addr := range ipv6MetadataAddrs {
		args := []string{
			"-A", "OUTPUT",
			"-d", addr + "/128",
			"-p", "tcp",
			"--dport", "80",
			"-j", "REJECT",
			"--reject-with", "tcp-reset",
		}
		output, err := runIP6Tables(args...)
		if err != nil {
			return fmt.Errorf("ipv6 metadata blocking failed for %s: %w (output: %s)", addr, err, string(output))
		}
		log.Info("ip6tables: blocking TCP/80 traffic to %s/128 (REJECT)", addr)
	}
	return nil
}

// InstallInterceptionRules installs the iptables rules for metadata
// interception in the current network namespace:
//  1. When IPv6 is enabled in the network namespace, IPv6 filter OUTPUT REJECT
//     rules for cloud metadata endpoints (in all modes).
//  2. For every mode other than assign, an IPv4 filter OUTPUT REJECT rule for
//     un-redirected metadata traffic.
//  3. A filter OUTPUT ACCEPT rule at position 1 for the redirected loopback
//     destination (so an egress firewall's catch-all OUTPUT REJECT does not
//     block the redirected flow), followed LAST by the IPv4 nat OUTPUT
//     REDIRECT rule to port.
//
// Ordering invariant: the IPv4 nat OUTPUT REDIRECT rule MUST be the final rule
// installed overall. On the runtime-managed interception path, sciontool init's
// IPv4 redirect probe is the synchronization barrier and passes as soon as
// REDIRECT exists. Installing REDIRECT last guarantees that a passing probe
// implies all filter, loopback accept, and IPv6 rules are already in place,
// and that any earlier failure leaves REDIRECT uninstalled so init fails
// closed. Packet semantics are unaffected because nat OUTPUT rewrites
// redirected packets to loopback before filter.
// Intended for the runtime's short-lived netns-setup container and legacy
// in-container setup.
func InstallInterceptionRules(mode string, port int) error {
	if runNetnsHasIPv6() {
		if err := runSetupIPv6MetadataBlock(); err != nil {
			return fmt.Errorf("%w: ipv6 filter rule: %v", ErrInterceptionUnavailable, err)
		}
	} else {
		log.Info("IPv6 not enabled in netns; skipping IPv6 metadata rules")
	}
	if mode != modeAssign {
		if _, err := runSetupMetadataBlock(); err != nil {
			return fmt.Errorf("%w: filter rule: %v", ErrInterceptionUnavailable, err)
		}
	}
	if err := runSetupIPTablesRedirect(port); err != nil {
		return fmt.Errorf("%w: redirect: %v", ErrInterceptionUnavailable, err)
	}
	return nil
}

// cleanupMetadataBlock removes the metadata block installed by setupMetadataBlock.
func cleanupMetadataBlock(method blockMethod) {
	switch method {
	case blockIPTables:
		args := []string{
			"-D", "OUTPUT",
			"-d", metadataIP,
			"-p", "tcp",
			"--dport", "80",
			"-j", "REJECT",
			"--reject-with", "icmp-port-unreachable",
		}
		if output, err := runIPTables(args...); err != nil {
			log.Debug("iptables block cleanup failed (non-fatal): %v (output: %s)", err, string(output))
		}
	}
}
