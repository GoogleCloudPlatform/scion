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

package firewalltest_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type digResult struct {
	ips      []string
	exitCode int
}

type firewallRunResult struct {
	err      error
	stdout   string
	stderr   string
	digLog   string
	ipsetLog string
}

func defaultDigResponses() map[string]digResult {
	return map[string]digResult{
		"api.anthropic.com":            {ips: []string{"160.79.104.10"}},
		"registry.npmjs.org":           {ips: []string{"104.16.25.34"}},
		"sentry.io":                    {ips: []string{"34.120.195.249"}},
		"statsig.com":                  {ips: []string{"34.117.186.192"}},
		"marketplace.visualstudio.com": {ips: []string{"13.107.42.18"}},
		"vscode.blob.core.windows.net": {ips: []string{"20.60.201.1"}},
		"update.code.visualstudio.com": {ips: []string{"13.107.246.40"}},
	}
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

func runInitFirewall(t *testing.T, digResponses map[string]digResult) firewallRunResult {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("init-firewall.sh is a Linux bash script")
	}

	scriptPath, err := filepath.Abs(filepath.Join("..", "..", "..", "harnesses", "claude", "init-firewall.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("stat %s: %v", scriptPath, err)
	}

	binDir := t.TempDir()
	logDir := t.TempDir()
	digLogPath := filepath.Join(logDir, "dig.log")
	ipsetLogPath := filepath.Join(logDir, "ipset.log")
	ipsetSeenPath := filepath.Join(logDir, "ipset-seen.log")

	writeStub(t, binDir, "iptables-save", `exit 0`)
	writeStub(t, binDir, "iptables", `exit 0`)
	writeStub(t, binDir, "ip6tables", `exit 0`)
	writeStub(t, binDir, "ipset", fmt.Sprintf(`printf '%%s\n' "$*" >> %q
exist=0
args=()
for arg in "$@"; do
  if [ "$arg" = "-exist" ] || [ "$arg" = "-!" ]; then
    exist=1
  else
    args+=("$arg")
  fi
done
if [ "${args[0]:-}" = "add" ] && [ "${#args[@]}" -ge 3 ]; then
  key="${args[1]} ${args[2]}"
  if [ -f %q ] && grep -Fxq "$key" %q; then
    if [ "$exist" -eq 0 ]; then
      echo "ipset: Element cannot be added to the set: it's already added" >&2
      exit 1
    fi
  else
    printf '%%s\n' "$key" >> %q
  fi
fi
exit 0`, ipsetLogPath, ipsetSeenPath, ipsetSeenPath, ipsetSeenPath))
	writeStub(t, binDir, "ip", `echo "default via 172.17.0.1 dev eth0"`)
	writeStub(t, binDir, "aggregate", `cat`)
	writeStub(t, binDir, "jq", `case "$1" in
  -e) exit 0 ;;
  -r) echo "140.82.112.0/20" ;;
  *) exit 1 ;;
esac`)
	writeStub(t, binDir, "curl", `for arg in "$@"; do
  case "$arg" in
    https://api.github.com/meta)
      echo '{"web":["140.82.112.0/20"],"api":["140.82.112.0/20"],"git":["140.82.112.0/20"]}'
      exit 0
      ;;
    https://example.com)
      exit 7
      ;;
    https://api.github.com/zen)
      echo "ok"
      exit 0
      ;;
  esac
done
exit 1`)

	var getentCases strings.Builder
	for domain, res := range digResponses {
		fmt.Fprintf(&getentCases, "  %q)\n", domain)
		for _, ip := range res.ips {
			fmt.Fprintf(&getentCases, "    printf '%%s STREAM %%s\\n' %q %q\n", ip, domain)
		}
		fmt.Fprintf(&getentCases, "    exit %d\n    ;;\n", res.exitCode)
	}
	writeStub(t, binDir, "getent", fmt.Sprintf(`if [ "$1" != "ahostsv4" ]; then
  exit 2
fi
domain="$2"
printf '%%s\n' "$domain" >> %q
case "$domain" in
%s  *)
    exit 2
    ;;
esac`, digLogPath, getentCases.String()))

	procRoot := filepath.Join(logDir, "proc")
	eth0Dir := filepath.Join(procRoot, "sys", "net", "ipv6", "conf", "eth0")
	if err := os.MkdirAll(eth0Dir, 0o755); err != nil {
		t.Fatalf("mkdir proc eth0: %v", err)
	}
	if err := os.WriteFile(filepath.Join(eth0Dir, "disable_ipv6"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write disable_ipv6: %v", err)
	}
	resolvPath := filepath.Join(logDir, "resolv.conf")
	if err := os.WriteFile(resolvPath, []byte("nameserver 127.0.0.11\n"), 0o644); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"SCION_FW_PROC_ROOT=" + procRoot,
		"SCION_FW_RESOLV_CONF=" + resolvPath,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	digLogBytes, _ := os.ReadFile(digLogPath)
	ipsetLogBytes, _ := os.ReadFile(ipsetLogPath)

	return firewallRunResult{
		err:      runErr,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		digLog:   string(digLogBytes),
		ipsetLog: string(ipsetLogBytes),
	}
}

func TestInitFirewall_AllDomainsResolve_StatsigAnthropicRemoved(t *testing.T) {
	res := runInitFirewall(t, defaultDigResponses())
	if res.err != nil {
		t.Fatalf("expected init-firewall.sh to succeed, got %v\nstdout:\n%s\nstderr:\n%s", res.err, res.stdout, res.stderr)
	}
	if strings.Contains(res.digLog, "statsig.anthropic.com") {
		t.Errorf("expected statsig.anthropic.com to be removed from allowlist, but dig queried it:\n%s", res.digLog)
	}
	if !strings.Contains(res.ipsetLog, "add allowed-domains 160.79.104.10") {
		t.Errorf("expected api.anthropic.com IP in ipset log, got:\n%s", res.ipsetLog)
	}
}

func TestInitFirewall_UnresolvableOptionalHostWarnsAndSucceeds(t *testing.T) {
	responses := defaultDigResponses()
	// Simulate NXDOMAIN (empty answer, exit 0) and DNS failure (non-zero exit).
	responses["sentry.io"] = digResult{ips: nil, exitCode: 0}
	responses["statsig.com"] = digResult{ips: nil, exitCode: 9}

	res := runInitFirewall(t, responses)
	if res.err != nil {
		t.Fatalf("expected init-firewall.sh to succeed when optional hosts fail to resolve, got %v\nstdout:\n%s\nstderr:\n%s", res.err, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "WARNING") || !strings.Contains(res.stderr, "sentry.io") {
		t.Errorf("expected stderr warning for unresolvable optional host sentry.io, got:\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "WARNING") || !strings.Contains(res.stderr, "statsig.com") {
		t.Errorf("expected stderr warning for failed optional host statsig.com, got:\n%s", res.stderr)
	}
	if !strings.Contains(res.ipsetLog, "add allowed-domains 160.79.104.10") {
		t.Errorf("expected required host api.anthropic.com to be added to ipset, got:\n%s", res.ipsetLog)
	}
	if !strings.Contains(res.ipsetLog, "add allowed-domains 104.16.25.34") {
		t.Errorf("expected other resolved optional host registry.npmjs.org to be added to ipset, got:\n%s", res.ipsetLog)
	}
}

func TestInitFirewall_UnresolvableRequiredHostFails(t *testing.T) {
	tests := []struct {
		name   string
		result digResult
	}{
		{name: "nxdomain_empty", result: digResult{ips: nil, exitCode: 0}},
		{name: "dig_nonzero_exit", result: digResult{ips: nil, exitCode: 9}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			responses := defaultDigResponses()
			responses["api.anthropic.com"] = tc.result

			res := runInitFirewall(t, responses)
			if res.err == nil {
				t.Fatalf("expected init-firewall.sh to fail when required host api.anthropic.com is unresolvable\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
			}
			combined := res.stdout + "\n" + res.stderr
			if !strings.Contains(combined, "ERROR") || !strings.Contains(combined, "api.anthropic.com") {
				t.Errorf("expected error mentioning api.anthropic.com, got stdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
			}
		})
	}
}

func TestInitFirewall_InvalidIPFailsClosed(t *testing.T) {
	responses := defaultDigResponses()
	responses["sentry.io"] = digResult{ips: []string{"not-an-ip"}, exitCode: 0}

	res := runInitFirewall(t, responses)
	if res.err == nil {
		t.Fatalf("expected init-firewall.sh to fail closed on invalid IP from optional host\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	}
	combined := res.stdout + "\n" + res.stderr
	if !strings.Contains(combined, "Invalid IP") || !strings.Contains(combined, "sentry.io") {
		t.Errorf("expected Invalid IP error for sentry.io, got stdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	}
	if strings.Contains(res.ipsetLog, "not-an-ip") {
		t.Errorf("invalid IP must not be added to ipset, got:\n%s", res.ipsetLog)
	}
}

func TestInitFirewall_DuplicateIPAcrossDomainsSucceeds(t *testing.T) {
	responses := defaultDigResponses()
	// Simulate two domains resolving to the same shared CDN IP.
	responses["sentry.io"] = digResult{ips: []string{"104.16.25.34"}, exitCode: 0}

	res := runInitFirewall(t, responses)
	if res.err != nil {
		t.Fatalf("expected init-firewall.sh to succeed when domains share an IP, got %v\nstdout:\n%s\nstderr:\n%s", res.err, res.stdout, res.stderr)
	}
	if strings.Count(res.ipsetLog, "add allowed-domains 104.16.25.34") < 2 {
		t.Errorf("expected both domains to add shared IP with -exist, got:\n%s", res.ipsetLog)
	}
}
