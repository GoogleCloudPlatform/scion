//go:build !windows

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

package firewalltest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type stubConfig struct {
	omitIP6Tables      bool
	ip6TablesProbeFail bool
	ipv6Ifaces         map[string]string // iface -> disable_ipv6 content ("0" or "1")
	resolvConf         string
	gatewayIP          string
	hostAliases        map[string]string   // alias -> IP
	domainIPs          map[string][]string // domain -> IPs (overrides default)
	curlIPv6Succeeds   bool
	extraEnv           []string
}

type runResult struct {
	exitCode int
	output   string
	calls    []string
}

func scriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "harnesses", "claude", "init-firewall.sh"))
	if err != nil {
		t.Fatalf("resolve init-firewall.sh: %v", err)
	}
	return p
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runInitFirewall(t *testing.T, cfg stubConfig) runResult {
	t.Helper()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	logFile := filepath.Join(tmp, "calls.log")

	// Symlink core POSIX utilities needed by init-firewall.sh so PATH can be
	// strictly restricted to binDir without pulling in the host's real ip6tables.
	for _, util := range []string{"awk", "basename", "cat", "dirname", "grep", "sort", "xargs"} {
		p, err := exec.LookPath(util)
		if err != nil {
			t.Fatalf("required host utility %q not found: %v", util, err)
		}
		if err := os.Symlink(p, filepath.Join(binDir, util)); err != nil {
			t.Fatalf("symlink %s: %v", util, err)
		}
	}

	// Use real jq if available; otherwise install a minimal stub for the two jq calls.
	if jqPath, err := exec.LookPath("jq"); err == nil {
		if err := os.Symlink(jqPath, filepath.Join(binDir, "jq")); err != nil {
			t.Fatalf("symlink jq: %v", err)
		}
	} else {
		writeExecutable(t, filepath.Join(binDir, "jq"), fmt.Sprintf(`#!/bin/sh
echo "jq $*" >> %q
if [ "$1" = "-e" ]; then
    exit 0
fi
printf '140.82.112.0/20\n'
`, logFile))
	}

	// Stub: iptables
	writeExecutable(t, filepath.Join(binDir, "iptables"), fmt.Sprintf(`#!/bin/sh
echo "iptables $*" >> %q
`, logFile))

	// Stub: ip6tables
	if !cfg.omitIP6Tables {
		probeExit := ""
		if cfg.ip6TablesProbeFail {
			probeExit = `if [ "$1" = "-w" ] && [ "$2" = "-S" ]; then exit 1; fi`
		}
		writeExecutable(t, filepath.Join(binDir, "ip6tables"), fmt.Sprintf(`#!/bin/sh
echo "ip6tables $*" >> %q
%s
`, logFile, probeExit))
	}

	// Stub: iptables-save
	writeExecutable(t, filepath.Join(binDir, "iptables-save"), fmt.Sprintf(`#!/bin/sh
echo "iptables-save $*" >> %q
`, logFile))

	// Stub: ipset (tracks existing members to enforce -exist semantics)
	ipsetState := filepath.Join(tmp, "ipset.state")
	writeExecutable(t, filepath.Join(binDir, "ipset"), fmt.Sprintf(`#!/bin/sh
echo "ipset $*" >> %q
state=%q
exist=0
if [ "$1" = "-exist" ] || [ "$1" = "-!" ]; then
    exist=1
    shift
fi
case "$1" in
    destroy)
        rm -f "$state"
        ;;
    create)
        : > "$state"
        ;;
    add)
        shift
        if [ "$1" = "-exist" ] || [ "$1" = "-!" ]; then
            exist=1
            shift
        fi
        setname="$1"
        member="$2"
        entry="$setname $member"
        if [ -f "$state" ] && grep -Fxq "$entry" "$state"; then
            if [ "$exist" -eq 0 ]; then
                echo "ipset: Element cannot be added to the set: it's already added" >&2
                exit 1
            fi
        else
            echo "$entry" >> "$state"
        fi
        ;;
esac
`, logFile, ipsetState))

	// Stub: aggregate (pass-through)
	writeExecutable(t, filepath.Join(binDir, "aggregate"), fmt.Sprintf(`#!/bin/sh
echo "aggregate $*" >> %q
cat
`, logFile))

	// Stub: ip
	gw := cfg.gatewayIP
	if gw == "" {
		gw = "172.17.0.1"
	}
	writeExecutable(t, filepath.Join(binDir, "ip"), fmt.Sprintf(`#!/bin/sh
echo "ip $*" >> %q
echo "default via %s dev eth0"
`, logFile, gw))

	// Stub: getent
	var getentCases strings.Builder
	defaultDomains := map[string][]string{
		"registry.npmjs.org":           {"104.16.0.1"},
		"api.anthropic.com":            {"160.79.104.10"},
		"sentry.io":                    {"160.79.104.20"},
		"statsig.com":                  {"160.79.104.20"}, // intentional duplicate IP
		"marketplace.visualstudio.com": {"13.107.42.18"},
		"vscode.blob.core.windows.net": {"20.60.178.33"},
		"update.code.visualstudio.com": {"13.107.246.40"},
	}
	for k, v := range cfg.domainIPs {
		defaultDomains[k] = v
	}
	for alias, ip := range cfg.hostAliases {
		if ip != "" {
			defaultDomains[alias] = []string{ip}
		}
	}
	for name, ips := range defaultDomains {
		fmt.Fprintf(&getentCases, "    %q)\n", name)
		for _, ip := range ips {
			fmt.Fprintf(&getentCases, "        printf '%%s STREAM %%s\\n' %q %q\n", ip, name)
		}
		getentCases.WriteString("        ;;\n")
	}
	writeExecutable(t, filepath.Join(binDir, "getent"), fmt.Sprintf(`#!/bin/sh
echo "getent $*" >> %q
if [ "$1" != "ahostsv4" ]; then
    exit 2
fi
case "$2" in
%s
    *)
        exit 2
        ;;
esac
`, logFile, getentCases.String()))

	// Stub: curl
	curlV6Exit := "7"
	if cfg.curlIPv6Succeeds {
		curlV6Exit = "0"
	}
	writeExecutable(t, filepath.Join(binDir, "curl"), fmt.Sprintf(`#!/bin/sh
echo "curl $*" >> %q
has_v6=0
url=""
for arg in "$@"; do
    case "$arg" in
        -6) has_v6=1 ;;
        http://*|https://*) url="$arg" ;;
    esac
done
case "$url" in
    https://api.github.com/meta)
        printf '{"web":["140.82.112.0/20"],"api":["140.82.112.0/20"],"git":["192.30.252.0/22"]}\n'
        exit 0
        ;;
    https://api.github.com/zen)
        printf 'Keep it logically awesome.\n'
        exit 0
        ;;
    https://example.com)
        if [ "$has_v6" -eq 1 ]; then
            exit %s
        fi
        exit 7
        ;;
    *)
        exit 7
        ;;
esac
`, logFile, curlV6Exit))

	// Fake /proc root
	procRoot := filepath.Join(tmp, "proc")
	for iface, val := range cfg.ipv6Ifaces {
		dir := filepath.Join(procRoot, "sys", "net", "ipv6", "conf", iface)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir proc iface %s: %v", iface, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "disable_ipv6"), []byte(val+"\n"), 0o644); err != nil {
			t.Fatalf("write disable_ipv6 for %s: %v", iface, err)
		}
	}

	// Fake resolv.conf
	resolvPath := filepath.Join(tmp, "resolv.conf")
	if err := os.WriteFile(resolvPath, []byte(cfg.resolvConf), 0o644); err != nil {
		t.Fatalf("write resolv.conf: %v", err)
	}

	cmd := exec.Command("bash", scriptPath(t))
	env := []string{
		"PATH=" + binDir,
		"SCION_FW_PROC_ROOT=" + procRoot,
		"SCION_FW_RESOLV_CONF=" + resolvPath,
	}
	env = append(env, cfg.extraEnv...)
	cmd.Env = env

	outBytes, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("exec bash init-firewall.sh: %v", err)
		}
	}

	var calls []string
	if logBytes, readErr := os.ReadFile(logFile); readErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(logBytes)), "\n") {
			if line != "" {
				calls = append(calls, line)
			}
		}
	}

	return runResult{
		exitCode: exitCode,
		output:   string(outBytes),
		calls:    calls,
	}
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func hasCallPrefix(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func hasCallContaining(calls []string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func TestInitFirewall_DefaultHardening(t *testing.T) {
	res := runInitFirewall(t, stubConfig{
		ipv6Ifaces: map[string]string{
			"lo":      "0",
			"all":     "0",
			"default": "0",
			"eth0":    "0",
		},
		gatewayIP: "172.18.0.1",
		// Host alias matches gateway -> must not add duplicate host rule
		hostAliases: map[string]string{
			"host.docker.internal": "172.18.0.1",
		},
	})
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", res.exitCode, res.output)
	}

	// IPv6 DROP policies on all three chains and final OUTPUT REJECT.
	for _, want := range []string{
		"ip6tables -w -P INPUT DROP",
		"ip6tables -w -P FORWARD DROP",
		"ip6tables -w -P OUTPUT DROP",
		"ip6tables -w -A OUTPUT -j REJECT --reject-with icmp6-adm-prohibited",
	} {
		if !hasCall(res.calls, want) {
			t.Errorf("missing expected call %q\ncalls:\n%s", want, strings.Join(res.calls, "\n"))
		}
	}

	// Verify final ip6tables call is the OUTPUT REJECT rule.
	lastIP6 := ""
	for _, c := range res.calls {
		if strings.HasPrefix(c, "ip6tables ") {
			lastIP6 = c
		}
	}
	if lastIP6 != "ip6tables -w -A OUTPUT -j REJECT --reject-with icmp6-adm-prohibited" {
		t.Errorf("last ip6tables call = %q, want OUTPUT REJECT", lastIP6)
	}

	// Gateway rules use /32; no /24 anywhere.
	if !hasCall(res.calls, "iptables -A INPUT -s 172.18.0.1/32 -j ACCEPT") {
		t.Errorf("missing gateway /32 INPUT rule")
	}
	if !hasCall(res.calls, "iptables -A OUTPUT -d 172.18.0.1/32 -j ACCEPT") {
		t.Errorf("missing gateway /32 OUTPUT rule")
	}
	if hasCallContaining(res.calls, "/24") {
		t.Errorf("unexpected /24 rule found in calls:\n%s", strings.Join(res.calls, "\n"))
	}

	// Gateway /32 rule should appear only once even when host.docker.internal matches gateway.
	gwInputCount := 0
	for _, c := range res.calls {
		if c == "iptables -A INPUT -s 172.18.0.1/32 -j ACCEPT" {
			gwInputCount++
		}
	}
	if gwInputCount != 1 {
		t.Errorf("gateway INPUT rule count = %d, want 1", gwInputCount)
	}

	// No --dport 22 or --sport 22 rule by default.
	if hasCallContaining(res.calls, "--dport 22") || hasCallContaining(res.calls, "--sport 22") {
		t.Errorf("unexpected SSH port 22 rule by default:\n%s", strings.Join(res.calls, "\n"))
	}

	// Duplicate IP (sentry.io & statsig.com both 160.79.104.20) succeeded via -exist.
	if !hasCall(res.calls, "ipset -exist add allowed-domains 160.79.104.20") {
		t.Errorf("expected ipset -exist add for duplicate IP")
	}

	// IPv6 verification check ran because IPV6_FILTERED=1 and eth0 disable_ipv6=0.
	if !hasCall(res.calls, "curl -6 --connect-timeout 5 https://example.com") {
		t.Errorf("expected IPv6 verification curl -6 call")
	}
}

func TestInitFirewall_IPv6MissingIP6Tables(t *testing.T) {
	t.Run("fails closed when IPv6 enabled", func(t *testing.T) {
		res := runInitFirewall(t, stubConfig{
			omitIP6Tables: true,
			ipv6Ifaces: map[string]string{
				"lo":   "0",
				"eth0": "0",
			},
		})
		if res.exitCode == 0 {
			t.Fatalf("expected non-zero exit when ip6tables is missing and IPv6 is enabled\noutput:\n%s", res.output)
		}
		if hasCallPrefix(res.calls, "iptables ") {
			t.Errorf("expected fail-closed before IPv4 iptables calls, got:\n%s", strings.Join(res.calls, "\n"))
		}
	})

	t.Run("skips cleanly when IPv6 disabled", func(t *testing.T) {
		res := runInitFirewall(t, stubConfig{
			omitIP6Tables: true,
			ipv6Ifaces: map[string]string{
				"all":     "0",
				"default": "0",
				"lo":      "0",
				"eth0":    "1",
			},
		})
		if res.exitCode != 0 {
			t.Fatalf("expected exit 0 when ip6tables is missing and IPv6 is disabled, got %d\noutput:\n%s", res.exitCode, res.output)
		}
		if hasCallPrefix(res.calls, "ip6tables ") {
			t.Errorf("expected no ip6tables calls, got:\n%s", strings.Join(res.calls, "\n"))
		}
	})

	t.Run("probe failure fails closed when IPv6 enabled", func(t *testing.T) {
		res := runInitFirewall(t, stubConfig{
			ip6TablesProbeFail: true,
			ipv6Ifaces: map[string]string{
				"eth0": "0",
			},
		})
		if res.exitCode == 0 {
			t.Fatalf("expected non-zero exit when ip6tables -w -S fails and IPv6 is enabled")
		}
	})

	t.Run("probe failure skips cleanly when IPv6 disabled", func(t *testing.T) {
		res := runInitFirewall(t, stubConfig{
			ip6TablesProbeFail: true,
			ipv6Ifaces: map[string]string{
				"eth0": "1",
			},
		})
		if res.exitCode != 0 {
			t.Fatalf("expected exit 0 when ip6tables -w -S fails and IPv6 is disabled, got %d\noutput:\n%s", res.exitCode, res.output)
		}
		for _, c := range res.calls {
			if strings.HasPrefix(c, "ip6tables ") && c != "ip6tables -w -S" {
				t.Errorf("unexpected ip6tables call after failed probe: %q", c)
			}
		}
	})
}

func TestInitFirewall_IPv6ResolvConfNameservers(t *testing.T) {
	// Intentionally omit trailing newline to verify the last line is still processed.
	res := runInitFirewall(t, stubConfig{
		ipv6Ifaces: map[string]string{"eth0": "0"},
		resolvConf: strings.Join([]string{
			"# comment",
			"nameserver 127.0.0.11",
			"nameserver 2001:4860:4860::8888",
			"nameserver 2001:0db8:0000:0000:0000:0000:0000:0001",
			"nameserver ::1:2:3:4:5:6:7",
			"nameserver ::",
			"nameserver 1:2:3:4:5:6:7:8:9",
			"nameserver 1:2:3:4:5:6:7::8",
			"nameserver 12345::1",
			"nameserver :1:2:3:4:5:6:7",
			"nameserver 1:2:3:4:5:6:7:",
			"nameserver 1:2:3:4:5:6:7",
			"nameserver bad;inject",
			"nameserver fd00::10 # inline comment without trailing newline",
		}, "\n"),
	})
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", res.exitCode, res.output)
	}
	for _, valid := range []string{
		"2001:4860:4860::8888",
		"2001:0db8:0000:0000:0000:0000:0000:0001",
		"::1:2:3:4:5:6:7",
		"fd00::10",
	} {
		want := "ip6tables -w -A OUTPUT -p udp -d " + valid + " --dport 53 -j ACCEPT"
		if !hasCall(res.calls, want) {
			t.Errorf("missing IPv6 nameserver rule for %s", valid)
		}
	}
	for _, invalid := range []string{
		"::",
		"1:2:3:4:5:6:7:8:9",
		"1:2:3:4:5:6:7::8",
		"12345::1",
		":1:2:3:4:5:6:7",
		"1:2:3:4:5:6:7:",
		"1:2:3:4:5:6:7",
		"bad;inject",
	} {
		if hasCallContaining(res.calls, " -d "+invalid+" ") {
			t.Errorf("invalid IPv6 nameserver %q leaked into ip6tables calls:\n%s", invalid, strings.Join(res.calls, "\n"))
		}
	}
}

func TestInitFirewall_HostAliasRule(t *testing.T) {
	res := runInitFirewall(t, stubConfig{
		ipv6Ifaces: map[string]string{"eth0": "1"},
		gatewayIP:  "172.17.0.1",
		hostAliases: map[string]string{
			"host.docker.internal":     "192.168.65.254",
			"host.containers.internal": "192.168.65.254", // same as docker alias -> deduped
		},
	})
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d\noutput:\n%s", res.exitCode, res.output)
	}
	if !hasCall(res.calls, "iptables -A INPUT -s 192.168.65.254/32 -j ACCEPT") {
		t.Errorf("missing host-alias INPUT /32 rule")
	}
	if !hasCall(res.calls, "iptables -A OUTPUT -d 192.168.65.254/32 -j ACCEPT") {
		t.Errorf("missing host-alias OUTPUT /32 rule")
	}
	aliasCount := 0
	for _, c := range res.calls {
		if c == "iptables -A OUTPUT -d 192.168.65.254/32 -j ACCEPT" {
			aliasCount++
		}
	}
	if aliasCount != 1 {
		t.Errorf("host-alias OUTPUT rule count = %d, want 1", aliasCount)
	}
}

func TestInitFirewall_AllowSSHOptIn(t *testing.T) {
	for _, val := range []string{"1", "true"} {
		t.Run("ALLOW_SSH="+val, func(t *testing.T) {
			res := runInitFirewall(t, stubConfig{
				ipv6Ifaces: map[string]string{"eth0": "1"},
				extraEnv:   []string{"SCION_INIT_FIREWALL_ALLOW_SSH=" + val},
			})
			if res.exitCode != 0 {
				t.Fatalf("expected exit 0, got %d\noutput:\n%s", res.exitCode, res.output)
			}
			count := 0
			for _, c := range res.calls {
				if c == "iptables -A OUTPUT -p tcp --dport 22 -j ACCEPT" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("expected exactly 1 --dport 22 rule, got %d", count)
			}
			if hasCallContaining(res.calls, "--sport 22") {
				t.Errorf("unexpected --sport 22 rule")
			}
		})
	}

	for _, bad := range []string{"yes", "0", "false", "1;id"} {
		t.Run("invalid_ALLOW_SSH="+bad, func(t *testing.T) {
			res := runInitFirewall(t, stubConfig{
				ipv6Ifaces: map[string]string{"eth0": "1"},
				extraEnv:   []string{"SCION_INIT_FIREWALL_ALLOW_SSH=" + bad},
			})
			if res.exitCode == 0 {
				t.Fatalf("expected non-zero exit for ALLOW_SSH=%q", bad)
			}
			if len(res.calls) != 0 {
				t.Errorf("expected zero external calls on invalid ALLOW_SSH, got:\n%s", strings.Join(res.calls, "\n"))
			}
		})
	}
}

func TestInitFirewall_ExtraDomains(t *testing.T) {
	t.Run("valid extra domains", func(t *testing.T) {
		res := runInitFirewall(t, stubConfig{
			ipv6Ifaces: map[string]string{"eth0": "1"},
			domainIPs: map[string][]string{
				"pypi.org":               {"151.101.0.223"},
				"files.pythonhosted.org": {"151.101.0.223", "151.101.64.223"},
			},
			extraEnv: []string{"SCION_INIT_FIREWALL_ALLOW_DOMAINS=pypi.org,files.pythonhosted.org"},
		})
		if res.exitCode != 0 {
			t.Fatalf("expected exit 0, got %d\noutput:\n%s", res.exitCode, res.output)
		}
		for _, want := range []string{
			"getent ahostsv4 pypi.org",
			"getent ahostsv4 files.pythonhosted.org",
			"ipset -exist add allowed-domains 151.101.0.223",
			"ipset -exist add allowed-domains 151.101.64.223",
		} {
			if !hasCall(res.calls, want) {
				t.Errorf("missing expected call %q", want)
			}
		}
	})

	invalidCases := []struct {
		name  string
		value string
	}{
		{"semicolon", "a;b.com"},
		{"command_sub", "$(id).x.com"},
		{"backtick", "`id`.x.com"},
		{"wildcard", "*.example.com"},
		{"localhost", "localhost"},
		{"sub_localhost", "foo.localhost"},
		{"ipv4_literal", "1.2.3.4"},
		{"single_label", "intranet"},
		{"leading_comma", ",pypi.org"},
		{"trailing_comma", "pypi.org,"},
		{"empty_middle", "pypi.org,,example.com"},
		{"newline", "pypi.org\nevil.com"},
		{"space", "pypi.org evil.com"},
		{"uppercase", "PyPI.org"},
	}

	for _, tc := range invalidCases {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			res := runInitFirewall(t, stubConfig{
				ipv6Ifaces: map[string]string{"eth0": "1"},
				extraEnv:   []string{"SCION_INIT_FIREWALL_ALLOW_DOMAINS=" + tc.value},
			})
			if res.exitCode == 0 {
				t.Fatalf("expected non-zero exit for invalid domain %q", tc.value)
			}
			if hasCallPrefix(res.calls, "getent ") || hasCallPrefix(res.calls, "ipset ") || hasCallPrefix(res.calls, "iptables ") || hasCallPrefix(res.calls, "ip6tables ") {
				t.Errorf("expected failure before any external tool call for %q, got:\n%s", tc.value, strings.Join(res.calls, "\n"))
			}
		})
	}
}

func TestInitFirewall_RejectsInvalidResolvedIP(t *testing.T) {
	res := runInitFirewall(t, stubConfig{
		ipv6Ifaces: map[string]string{"eth0": "1"},
		domainIPs: map[string][]string{
			"registry.npmjs.org": {"999.1.1.1"},
		},
	})
	if res.exitCode == 0 {
		t.Fatalf("expected non-zero exit for invalid resolved IP 999.1.1.1")
	}
	if hasCallContaining(res.calls, "999.1.1.1") {
		t.Errorf("invalid IP 999.1.1.1 should not be passed to ipset:\n%s", strings.Join(res.calls, "\n"))
	}
}

func TestInitFirewall_IPv6VerificationFailureAborts(t *testing.T) {
	res := runInitFirewall(t, stubConfig{
		ipv6Ifaces:       map[string]string{"eth0": "0"},
		curlIPv6Succeeds: true,
	})
	if res.exitCode == 0 {
		t.Fatalf("expected non-zero exit when IPv6 verification curl -6 unexpectedly succeeds")
	}
}
