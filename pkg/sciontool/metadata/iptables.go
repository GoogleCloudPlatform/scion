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
	"os/exec"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
)

const metadataIP = "169.254.169.254"

// setupIPTablesRedirect configures iptables to redirect traffic destined for
// the GCE metadata server IP (169.254.169.254) to the local metadata sidecar.
// This ensures tools that hardcode the metadata IP (e.g., curl) are intercepted.
// Requires NET_ADMIN capability.
func setupIPTablesRedirect(port int) error {
	portStr := strconv.Itoa(port)

	// Add a DNAT rule: redirect TCP traffic to 169.254.169.254:80 to localhost:port
	args := []string{
		"-t", "nat",
		"-A", "OUTPUT",
		"-d", metadataIP,
		"-p", "tcp",
		"--dport", "80",
		"-j", "REDIRECT",
		"--to-port", portStr,
	}

	cmd := exec.Command("iptables", args...)
	output, err := procreap.CombinedOutputManaged(cmd)
	if err != nil {
		return fmt.Errorf("iptables redirect setup failed: %w (output: %s)", err, string(output))
	}

	log.Info("iptables: redirecting %s:80 -> localhost:%s", metadataIP, portStr)
	return nil
}

// cleanupIPTablesRedirect removes the iptables redirect rule.
func cleanupIPTablesRedirect(port int) {
	portStr := strconv.Itoa(port)
	args := []string{
		"-t", "nat",
		"-D", "OUTPUT",
		"-d", metadataIP,
		"-p", "tcp",
		"--dport", "80",
		"-j", "REDIRECT",
		"--to-port", portStr,
	}

	cmd := exec.Command("iptables", args...)
	if output, err := procreap.CombinedOutputManaged(cmd); err != nil {
		log.Debug("iptables cleanup failed (non-fatal): %v (output: %s)", err, string(output))
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
	cmd := exec.Command("iptables", rejectArgs...)
	output, err := procreap.CombinedOutputManaged(cmd)
	if err == nil {
		log.Info("iptables: blocking TCP/80 traffic to %s (REJECT)", metadataIP)
		return blockIPTables, nil
	}
	log.Debug("iptables REJECT failed: %v (output: %s)", err, string(output))

	return blockNone, fmt.Errorf("metadata blocking failed: iptables: %v", err)
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
		cmd := exec.Command("iptables", args...)
		if output, err := procreap.CombinedOutputManaged(cmd); err != nil {
			log.Debug("iptables block cleanup failed (non-fatal): %v (output: %s)", err, string(output))
		}
	}
}
