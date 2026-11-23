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
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// plantFakeIPTablesOnPATH points $PATH at a directory containing a fake
// "iptables" that, if ever executed, creates a marker file — then returns a
// function that reports whether it ran. This is the attack shape a planted
// binary first on $PATH would take: an attacker-owned directory placed
// first on PATH, standing in for substrate's real "/usr/local/share/
// npm-global/bin", made hermetic by using t.Setenv instead of the real
// npm-global directory.
func plantFakeIPTablesOnPATH(t *testing.T) (ran func() bool) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "planted-ran")
	fake := filepath.Join(dir, "iptables")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write planted iptables: %v", err)
	}
	t.Setenv("PATH", dir)
	return func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}
}

// TestIPTablesCmd_NeverConsultsPATH covers all four call sites in this file
// (setupIPTablesRedirect, cleanupIPTablesRedirect, setupMetadataBlock,
// cleanupMetadataBlock all go through the shared iptablesCmd helper): with a
// planted "iptables" as the only entry on $PATH, none of them may run it.
// Each call is still expected to fail in this unprivileged test environment
// (the real, resolved iptables binary refuses without CAP_NET_ADMIN) — the
// assertion that matters is that the PLANTED one never ran. This is
// necessarily a negative-only assertion (every call here fails for a
// reason unrelated to the property under test), which is exactly the shape
// that let a resolution regression hide undetected — see
// TestIPTablesCmd_RealBinaryDispatchesCorrectly below for the paired
// positive-path assertion that would have caught it.
func TestIPTablesCmd_NeverConsultsPATH(t *testing.T) {
	t.Run("setupIPTablesRedirect", func(t *testing.T) {
		ran := plantFakeIPTablesOnPATH(t)
		_ = setupIPTablesRedirect(18382)
		if ran() {
			t.Fatal("setupIPTablesRedirect executed a planted iptables from $PATH")
		}
	})
	t.Run("cleanupIPTablesRedirect", func(t *testing.T) {
		ran := plantFakeIPTablesOnPATH(t)
		cleanupIPTablesRedirect(18382)
		if ran() {
			t.Fatal("cleanupIPTablesRedirect executed a planted iptables from $PATH")
		}
	})
	t.Run("setupMetadataBlock", func(t *testing.T) {
		ran := plantFakeIPTablesOnPATH(t)
		method, _ := setupMetadataBlock()
		if ran() {
			t.Fatal("setupMetadataBlock executed a planted iptables from $PATH")
		}
		if method == blockIPTables {
			t.Fatal("setupMetadataBlock reported success via a planted iptables from $PATH")
		}
	})
	t.Run("cleanupMetadataBlock", func(t *testing.T) {
		ran := plantFakeIPTablesOnPATH(t)
		cleanupMetadataBlock(blockIPTables)
		if ran() {
			t.Fatal("cleanupMetadataBlock executed a planted iptables from $PATH")
		}
	})
}

// TestIPTablesCmd_RealBinaryDispatchesCorrectly is
// TestIPTablesCmd_NeverConsultsPATH's required positive-path pair: it
// proves the real, resolved iptables binary actually runs and dispatches
// correctly, not merely that a planted one never runs. On a real Debian
// image, iptables resolves through a symlink chain to a multi-call binary
// that refuses to run at all if handed the wrong argv[0] — a regression in
// that resolution would leave every call in the test above failing for a
// completely different, no-less-negative-looking reason (this environment
// also lacks CAP_NET_ADMIN), so nothing there would ever have caught it.
func TestIPTablesCmd_RealBinaryDispatchesCorrectly(t *testing.T) {
	cmd, err := iptablesCmd("-V")
	if err != nil {
		t.Skipf("iptables not resolvable as a trusted binary in this environment: %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the real, resolved iptables -V: %v, output=%s", err, out)
	}
	if !strings.Contains(string(out), "iptables") {
		t.Errorf("iptables -V output = %q, want it to mention \"iptables\" (a multi-call dispatch failure prints \"No valid subcommand given\" instead)", out)
	}
}

// TestIptablesCmd_EnvIsFromScratch proves iptablesCmd's returned *exec.Cmd
// never inherits this process's own ambient environment: cmd.Env left nil
// (Go's exec.Cmd default) would silently fall back to os.Environ(), letting
// an inherited LD_PRELOAD/BASH_ENV/etc. reach the root-context iptables
// invocation even though its argv[0]/path is already a verified absolute
// path unaffected by $PATH itself.
func TestIptablesCmd_EnvIsFromScratch(t *testing.T) {
	t.Setenv("LD_PRELOAD", "/should/not/leak.so")

	cmd, err := iptablesCmd("-V")
	if err != nil {
		t.Skipf("iptables not resolvable as a trusted binary in this environment: %v", err)
	}
	if cmd.Env == nil {
		t.Fatal("iptablesCmd's cmd.Env is nil, which defaults to inheriting this process's full ambient environment")
	}
	for _, kv := range cmd.Env {
		if strings.Contains(kv, "should/not/leak") {
			t.Errorf("cmd.Env = %v, leaked an ambient environment value: %q", cmd.Env, kv)
		}
	}
}

func TestSetupIPTablesRedirect_NoIPTables(t *testing.T) {
	// This test verifies that setupIPTablesRedirect returns an error when
	// iptables is not available (which is the case in most test environments).
	err := setupIPTablesRedirect(18380)
	if err == nil {
		// If it succeeded, we're running in a privileged environment with iptables.
		// Clean up the rule we just created.
		cleanupIPTablesRedirect(18380)
		t.Skip("iptables available in test environment, skipping error path test")
	}
	// Expected: error because iptables is not available
	t.Logf("Expected iptables failure: %v", err)
}

func TestCleanupIPTablesRedirect_NoIPTables(t *testing.T) {
	// Cleanup should be a no-op when iptables is not available
	cleanupIPTablesRedirect(18380)
}

func TestSetupMetadataBlock_NoPrivileges(t *testing.T) {
	// In a non-privileged test environment, both iptables and ip route
	// should fail, and setupMetadataBlock should return an error.
	method, err := setupMetadataBlock()
	if err == nil {
		// If it succeeded, clean up and skip.
		cleanupMetadataBlock(method)
		t.Skip("metadata block succeeded in test environment, skipping error path test")
	}
	if method != blockNone {
		t.Fatalf("expected blockNone on failure, got %v", method)
	}
	t.Logf("Expected metadata block failure: %v", err)
}

func TestCleanupMetadataBlock_Noop(t *testing.T) {
	// Cleanup with blockNone should be a silent no-op
	cleanupMetadataBlock(blockNone)
}

func stubRunIPTables(t *testing.T, fn func(args ...string) ([]byte, error)) {
	t.Helper()
	orig := runIPTables
	runIPTables = fn
	t.Cleanup(func() { runIPTables = orig })
}

func TestInstallInterceptionRules_IPTablesArgs(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		port      int
		wantCalls [][]string
	}{
		{
			name: "block installs nat redirect, filter output accept at pos 1, and metadata block",
			mode: modeBlock,
			port: 18380,
			wantCalls: [][]string{
				{"-t", "nat", "-A", "OUTPUT", "-d", metadataIP, "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", "18380"},
				{"-I", "OUTPUT", "1", "-d", loopbackIP, "-p", "tcp", "--dport", "18380", "-j", "ACCEPT"},
				{"-A", "OUTPUT", "-d", metadataIP, "-p", "tcp", "--dport", "80", "-j", "REJECT", "--reject-with", "icmp-port-unreachable"},
			},
		},
		{
			name: "assign installs nat redirect and filter output accept at pos 1 without block rule",
			mode: modeAssign,
			port: 18381,
			wantCalls: [][]string{
				{"-t", "nat", "-A", "OUTPUT", "-d", metadataIP, "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", "18381"},
				{"-I", "OUTPUT", "1", "-d", loopbackIP, "-p", "tcp", "--dport", "18381", "-j", "ACCEPT"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got [][]string
			stubRunIPTables(t, func(args ...string) ([]byte, error) {
				cp := append([]string(nil), args...)
				got = append(got, cp)
				return nil, nil
			})

			if err := InstallInterceptionRules(tt.mode, tt.port); err != nil {
				t.Fatalf("InstallInterceptionRules(%q, %d) = %v, want nil", tt.mode, tt.port, err)
			}
			if !reflect.DeepEqual(got, tt.wantCalls) {
				t.Errorf("iptables calls:\ngot:  %q\nwant: %q", got, tt.wantCalls)
			}
		})
	}
}

func TestSetupIPTablesRedirect_FilterAcceptFailureRollsBackNat(t *testing.T) {
	var got [][]string
	stubRunIPTables(t, func(args ...string) ([]byte, error) {
		cp := append([]string(nil), args...)
		got = append(got, cp)
		if len(args) >= 2 && args[0] == "-I" && args[1] == "OUTPUT" {
			return []byte("iptables: filter insert failed"), errors.New("exit status 1")
		}
		return nil, nil
	})

	err := InstallInterceptionRules(modeBlock, 18380)
	if !errors.Is(err, ErrInterceptionUnavailable) {
		t.Fatalf("InstallInterceptionRules() error = %v, want ErrInterceptionUnavailable", err)
	}

	wantCalls := [][]string{
		{"-t", "nat", "-A", "OUTPUT", "-d", metadataIP, "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", "18380"},
		{"-I", "OUTPUT", "1", "-d", loopbackIP, "-p", "tcp", "--dport", "18380", "-j", "ACCEPT"},
		{"-t", "nat", "-D", "OUTPUT", "-d", metadataIP, "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", "18380"},
	}
	if !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("iptables calls on filter accept failure:\ngot:  %q\nwant: %q", got, wantCalls)
	}
}

func TestCleanupIPTablesRedirect_RemovesBothRules(t *testing.T) {
	var got [][]string
	stubRunIPTables(t, func(args ...string) ([]byte, error) {
		cp := append([]string(nil), args...)
		got = append(got, cp)
		return nil, nil
	})

	cleanupIPTablesRedirect(18380)

	wantCalls := [][]string{
		{"-t", "nat", "-D", "OUTPUT", "-d", metadataIP, "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", "18380"},
		{"-D", "OUTPUT", "-d", loopbackIP, "-p", "tcp", "--dport", "18380", "-j", "ACCEPT"},
	}
	if !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("cleanupIPTablesRedirect calls:\ngot:  %q\nwant: %q", got, wantCalls)
	}
}

// TestFirewallAndInterceptionRuleOrdering models the filter OUTPUT chain
// after init-firewall.sh (which allows -o lo and appends a catch-all REJECT)
// followed by InstallInterceptionRules. It verifies that a packet rewritten
// by nat OUTPUT REDIRECT (dst=127.0.0.1:<port>, out=eth0) is accepted while
// non-allowlisted egress (including external traffic on the metadata port and
// un-redirected metadata traffic) remains rejected.
func TestFirewallAndInterceptionRuleOrdering(t *testing.T) {
	type filterRule struct {
		outIface string
		dst      string
		proto    string
		dport    string
		target   string
	}
	type packet struct {
		name     string
		outIface string
		dst      string
		proto    string
		dport    string
		want     string
	}

	eval := func(rules []filterRule, p packet) string {
		for _, r := range rules {
			if r.outIface != "" && r.outIface != p.outIface {
				continue
			}
			if r.dst != "" && r.dst != p.dst {
				continue
			}
			if r.proto != "" && r.proto != p.proto {
				continue
			}
			if r.dport != "" && r.dport != p.dport {
				continue
			}
			return r.target
		}
		return "DROP"
	}

	parseFilterRule := func(args []string) (pos int, rule filterRule, isFilter bool) {
		// Ignore non-filter tables (e.g., -t nat).
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-t" && args[i+1] != "filter" {
				return 0, filterRule{}, false
			}
		}
		pos = -1 // append
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "-I":
				// -I OUTPUT 1
				if i+2 < len(args) && args[i+1] == "OUTPUT" && args[i+2] == "1" {
					pos = 0
					i += 2
				}
			case "-o":
				if i+1 < len(args) {
					rule.outIface = args[i+1]
					i++
				}
			case "-d":
				if i+1 < len(args) {
					rule.dst = args[i+1]
					i++
				}
			case "-p":
				if i+1 < len(args) {
					rule.proto = args[i+1]
					i++
				}
			case "--dport":
				if i+1 < len(args) {
					rule.dport = args[i+1]
					i++
				}
			case "-j":
				if i+1 < len(args) {
					rule.target = args[i+1]
					i++
				}
			}
		}
		return pos, rule, true
	}

	for _, mode := range []string{modeBlock, modeAssign} {
		t.Run(mode, func(t *testing.T) {
			// Baseline filter OUTPUT chain installed by init-firewall.sh:
			// allows -o lo, rejects everything else not on the allowlist.
			chain := []filterRule{
				{outIface: "lo", target: "ACCEPT"},
				{target: "REJECT"},
			}

			stubRunIPTables(t, func(args ...string) ([]byte, error) {
				pos, r, ok := parseFilterRule(args)
				if !ok {
					return nil, nil
				}
				if pos == 0 {
					chain = append([]filterRule{r}, chain...)
				} else {
					chain = append(chain, r)
				}
				return nil, nil
			})

			if err := InstallInterceptionRules(mode, 18380); err != nil {
				t.Fatalf("InstallInterceptionRules(%q, 18380) = %v", mode, err)
			}

			packets := []packet{
				{name: "redirected metadata probe via eth0", outIface: "eth0", dst: "127.0.0.1", proto: "tcp", dport: "18380", want: "ACCEPT"},
				{name: "direct loopback traffic", outIface: "lo", dst: "127.0.0.1", proto: "tcp", dport: "8080", want: "ACCEPT"},
				{name: "non-allowlisted external HTTPS", outIface: "eth0", dst: "93.184.216.34", proto: "tcp", dport: "443", want: "REJECT"},
				{name: "external host on metadata emulator port", outIface: "eth0", dst: "192.0.2.1", proto: "tcp", dport: "18380", want: "REJECT"},
				{name: "un-redirected metadata HTTP", outIface: "eth0", dst: "169.254.169.254", proto: "tcp", dport: "80", want: "REJECT"},
				{name: "non-HTTP metadata port", outIface: "eth0", dst: "169.254.169.254", proto: "tcp", dport: "443", want: "REJECT"},
				{name: "UDP to loopback metadata port via eth0", outIface: "eth0", dst: "127.0.0.1", proto: "udp", dport: "18380", want: "REJECT"},
			}

			for _, p := range packets {
				if got := eval(chain, p); got != p.want {
					t.Errorf("packet %q: verdict = %s, want %s (chain=%+v)", p.name, got, p.want, chain)
				}
			}
		})
	}
}
