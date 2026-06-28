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

// EnsureAppleDNS creates the DNS rule if it doesn't already exist.
// Checks without sudo first; only invokes sudo if the rule is missing.
func EnsureAppleDNS(ctx context.Context, hostname, ip string) (bool, error) {
	exists, err := AppleDNSRuleExists(ctx, hostname)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "sudo", "container", "system", "dns", "create", hostname, "--localhost", ip)
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("sudo container system dns create failed: %w (output: %s)", err, string(out))
	}
	return true, nil
}
