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

package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

// A block with a pin file passes --known-hosts and --host-key-alias after
// --project and before the agent, each word quoted like the others; a space
// in the scion path or the home path is single-quoted. Without a pin file
// the block is the ssh-config block.
func TestRenderSSHHostBlockPinned_Quoting(t *testing.T) {
	block, err := renderSSHHostBlockPinned("/opt/my scion/bin/scion", "https://hub.example", "h", "p", "a",
		vscodeKnownHostsRef, "/home/u x/.scion/vscode/known_hosts")
	require.NoError(t, err)
	assert.Contains(t, block, "  ProxyCommand '/opt/my scion/bin/scion' tunnel --stdio --ssh --hub https://hub.example --project p"+
		" --known-hosts '/home/u x/.scion/vscode/known_hosts' --host-key-alias scion.h.p.a a\n")
	assert.Contains(t, block, "  HostKeyAlias scion.h.p.a\n")
	assert.Contains(t, block, "  UserKnownHostsFile ~/.scion/vscode/known_hosts\n")
	assert.Contains(t, block, "  StrictHostKeyChecking yes\n")
	assert.Equal(t, tunnelclient.NormalizeEndpoint("https://hub.example"), vscodeBlockHub(block))

	_, err = renderSSHHostBlockPinned(sshTestExe, "https://hub.example", "h", "p", "a", vscodeKnownHostsRef, "relative/known_hosts")
	assert.Error(t, err)

	plain, err := renderSSHHostBlockPinned(sshTestExe, "https://hub.example", "h", "p", "a", sshKnownHostsRef("h"), "")
	require.NoError(t, err)
	ssh, err := renderSSHHostBlock(sshTestExe, "https://hub.example", "h", "p", "a")
	require.NoError(t, err)
	assert.Equal(t, ssh, plain, "without a pin file the block is byte-identical to ssh-config's")
	assert.NotContains(t, ssh, "--known-hosts")
}

// --known-hosts and --host-key-alias are a usage error unless both are
// given, with --ssh, an absolute path and a Host name; nothing is dialed.
func TestTunnelPinSourceFlags(t *testing.T) {
	base := []string{"tunnel", "--stdio", "--hub", "http://127.0.0.1:1", "--project", "p"}
	for _, tc := range []struct {
		name string
		args []string
		line string
	}{
		{"known-hosts alone", []string{"--ssh", "--known-hosts", "/k"}, "--known-hosts and --host-key-alias must be given together"},
		{"alias alone", []string{"--ssh", "--host-key-alias", "scion.h.p.a"}, "--known-hosts and --host-key-alias must be given together"},
		{"without --ssh", []string{"--port", "22", "--known-hosts", "/k", "--host-key-alias", "scion.h.p.a"}, "--known-hosts and --host-key-alias need --ssh"},
		{"relative path", []string{"--ssh", "--known-hosts", "k", "--host-key-alias", "scion.h.p.a"}, `invalid --known-hosts "k": use an absolute path`},
		{"bad alias", []string{"--ssh", "--known-hosts", "/k", "--host-key-alias", "h.p.a"}, `invalid --host-key-alias "h.p.a": use scion.<hub>.<project>.<agent>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append(append([]string{}, base...), tc.args...), "a")
			code, stderr := tunnelRunCLI(t, nil, args...)
			assert.Equal(t, tunnelclient.ExitUsage, code, "stderr %q", stderr)
			assert.Contains(t, stderr, tc.line)
			tunnelAssertOneLine(t, stderr)
		})
	}
}

// The flags steer only the hint: given the same hub answers, the tunnel's
// exit code and stderr are the same with and without them, and the pin
// file is never written.
func TestTunnelCLI_PinFlagsDoNotChangeOutcome(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	pinDir := t.TempDir()
	pinFile := filepath.Join(pinDir, "known_hosts")
	pin := renderKnownHosts(map[string]string{"scion.h.a.nope": mustNormKey(t, testSSHPublicKey(t))})
	require.NoError(t, os.WriteFile(pinFile, []byte(pin), 0o600))

	for _, ref := range [][2]string{{"nope", "b-c"}, {"a", "nope"}} {
		run := func(extra ...string) (int, string) {
			args := append([]string{"tunnel", "--stdio", "--ssh", "--hub", hub.srv.URL, "--project", ref[0]}, extra...)
			return tunnelRunCLIWith(t, nil, func() { t.Setenv("SCION_HUB_TOKEN", "tok") }, append(args, ref[1])...)
		}
		codeWithout, stderrWithout := run()
		codeWith, stderrWith := run("--known-hosts", pinFile, "--host-key-alias", "scion.h."+ref[0]+"."+ref[1])
		assert.Equal(t, tunnelclient.ExitAgentNotFound, codeWithout, "stderr %q", stderrWithout)
		assert.Equal(t, codeWithout, codeWith, "%s/%s", ref[0], ref[1])
		assert.Equal(t, stderrWithout, stderrWith, "%s/%s", ref[0], ref[1])
		assert.Equal(t, pin, readTestFile(t, pinFile), "the pin file is never written")
	}
}

// The flags steer only the hint also when it fires: for an existing agent
// whose pin differs, the hint is printed with the flags, and the tunnel
// then fails the same way with and without them (the fake hub has no
// tunnel route, a terminal exit 11 with no retry). The pin file is never
// written.
func TestTunnelCLI_PinMismatchDoesNotChangeOutcome(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	pinFile := filepath.Join(t.TempDir(), "known_hosts")
	alias := "scion.h.a.b-c"
	pin := renderKnownHosts(map[string]string{alias: mustNormKey(t, testSSHPublicKey(t))})
	require.NoError(t, os.WriteFile(pinFile, []byte(pin), 0o600))
	run := func(extra ...string) (int, string, string) {
		args := append([]string{"tunnel", "--stdio", "--ssh", "--hub", hub.srv.URL, "--project", "a"}, extra...)
		return tunnelRunCLIOutput(t, nil, func() { t.Setenv("SCION_HUB_TOKEN", "tok") }, append(args, "b-c")...)
	}
	hint := "agent was recreated; run scion vscode --hub " + hub.srv.URL + " --project a b-c"

	codeWithout, stderrWithout, outWithout := run()
	codeWith, stderrWith, outWith := run("--known-hosts", pinFile, "--host-key-alias", alias)

	assert.Equal(t, tunnelclient.ExitNoTunnelSupport, codeWithout, "stderr %q", stderrWithout)
	assert.Equal(t, codeWithout, codeWith)
	assert.Equal(t, stderrWithout, stderrWith)
	assert.Contains(t, outWith, hint, "the mismatch path is reached with the flags")
	assert.NotContains(t, outWithout, "agent was recreated")
	assert.Equal(t, pin, readTestFile(t, pinFile), "the pin file is never written")
}

// With a pin source the hint reads only that file: a mismatch prints a
// scion vscode command with the pinned hub, project and agent; a match, a
// missing pin or a missing file is silent; a stale ssh-config pin for the
// same agent is ignored. Nothing is written.
func TestCheckSSHHostKeyPin_VSCodeFileHint(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	pinFile := filepath.Join(home, "vscode_known_hosts")
	alias := "scion.v.a.b-c"
	writePin := func(key string) {
		require.NoError(t, os.WriteFile(pinFile, []byte(renderKnownHosts(map[string]string{alias: mustNormKey(t, key)})), 0o600))
	}
	check := func(pin sshPinSource) string {
		var stderr bytes.Buffer
		require.NoError(t, checkSSHHostKeyPinIn(context.Background(), &stderr, home, hub.srv.URL, "tok", "a", "b-c", pin))
		return stderr.String()
	}
	source := sshPinSource{knownHosts: pinFile, alias: alias}

	writePin(hub.agents[1].HostKey)
	assert.Empty(t, check(source), "matching pin: silent")

	hub.rotateKey(t, "ag-bc")
	assert.Equal(t, sshRecreatedHint+"\n", check(sshPinSource{}), "without a pin source: the ssh-config pin, as before")
	writePin(hub.agents[1].HostKey)
	before := readTestFile(t, pinFile)
	assert.Empty(t, check(source), "the vscode pin matches the new key: the stale ssh-config pin is ignored")

	hub.rotateKey(t, "ag-bc")
	assert.Equal(t, "agent was recreated; run scion vscode --hub "+hub.srv.URL+" --project a b-c\n", check(source))
	assert.Equal(t, before, readTestFile(t, pinFile), "the hint never writes the pin")

	assert.Empty(t, check(sshPinSource{knownHosts: pinFile, alias: "scion.v.a.other"}), "missing pin: silent")
	assert.Empty(t, check(sshPinSource{knownHosts: filepath.Join(home, "absent"), alias: alias}), "missing file: silent")
}

// scion vscode writes the pin flags into its blocks: the hint then reads
// ~/.scion/vscode/known_hosts under the block's own HostKeyAlias.
func TestVSCode_BlockPassesPinFlags(t *testing.T) {
	hub, env, _ := newVSCodeTest(t)
	require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false))
	p := vscodePaths{home: env.home}
	host := "scion." + hub.label(t) + ".a.b-c"
	block := parseSSHHostBlocks(readTestFile(t, p.sshConfig()))[host]
	want := " --project a --known-hosts " + sshShellQuote(p.knownHosts()) + " --host-key-alias " + host + " b-c\n"
	assert.True(t, strings.Contains(block, want), "block %q lacks %q", block, want)
}
