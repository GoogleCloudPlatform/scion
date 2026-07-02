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

package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const (
	AppleDNSHostname = "host.containers.internal"
	AppleDNSIP       = "203.0.113.1"
)

// AppleDNSRuleExists checks whether a DNS rule for the given hostname is
// configured in Apple Container. Does not require sudo.
func AppleDNSRuleExists(ctx context.Context, hostname string) (bool, error) {
	out, err := exec.CommandContext(ctx, "container", "system", "dns", "list").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == hostname {
			return true, nil
		}
	}
	return false, nil
}

// EnsureAppleDNS always attempts to create (or recreate) the DNS rule.
// This is necessary because PF rules do not persist across macOS reboots
// even though DNS entries do, so we must re-run the create command on
// every server startup to restore the packet filter rule.
func EnsureAppleDNS(ctx context.Context, hostname, ip string) (bool, error) {
	cmd := exec.CommandContext(ctx, "sudo", "container", "system", "dns", "create", hostname, "--localhost", ip)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Create failed — likely the entry already exists (possibly with a
		// different IP). Delete it and retry.
		delCmd := exec.CommandContext(ctx, "sudo", "container", "system", "dns", "delete", hostname)
		if delOut, delErr := delCmd.CombinedOutput(); delErr != nil {
			return false, fmt.Errorf("sudo container system dns delete failed: %w (output: %s); original create error: %v (output: %s)", delErr, string(delOut), err, string(out))
		}
		retryCmd := exec.CommandContext(ctx, "sudo", "container", "system", "dns", "create", hostname, "--localhost", ip)
		if retryOut, retryErr := retryCmd.CombinedOutput(); retryErr != nil {
			return false, fmt.Errorf("sudo container system dns create (retry after delete) failed: %w (output: %s)", retryErr, string(retryOut))
		}
	}
	return true, nil
}
