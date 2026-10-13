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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

// fakeVSCode replaces the preflight and the VS Code CLI for one test.
type fakeVSCode struct {
	preflightErr   error
	preflightCalls []string // "<endpoint> <project>/<agent>"
	lookPathErr    error
	runErr         func() error
	runs           [][]string // argv after the binary
}

func stubVSCode(t *testing.T) *fakeVSCode {
	t.Helper()
	f := &fakeVSCode{}
	origPreflight, origLook, origRun := vscodePreflight, vscodeLookPath, vscodeRunCode
	t.Cleanup(func() { vscodePreflight, vscodeLookPath, vscodeRunCode = origPreflight, origLook, origRun })
	vscodePreflight = func(_ context.Context, endpoint, project, agent string, _ io.Writer) error {
		f.preflightCalls = append(f.preflightCalls, endpoint+" "+project+"/"+agent)
		return f.preflightErr
	}
	vscodeLookPath = func(name string) (string, error) {
		if f.lookPathErr != nil {
			return "", f.lookPathErr
		}
		return "/opt/vscode/bin/" + name, nil
	}
	vscodeRunCode = func(_ context.Context, bin string, args []string, _, _ io.Writer) error {
		if bin != "/opt/vscode/bin/code" {
			return fmt.Errorf("unexpected binary %s", bin)
		}
		f.runs = append(f.runs, args)
		if f.runErr != nil {
			return f.runErr()
		}
		return nil
	}
	return f
}

// newVSCodeTest returns a fake hub with projects a-b (agent c) and a (agent
// b-c), an ssh env for it in a fresh home, and the stubbed outside effects.
func newVSCodeTest(t *testing.T) (*fakeSSHHub, *sshCommandEnv, *fakeVSCode) {
	t.Helper()
	hub := twoProjectHub(t, "tok")
	env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
	return hub, env, stubVSCode(t)
}

func vscodeOut(env *sshCommandEnv) string { return env.stdout.(fmt.Stringer).String() }

// One run writes the agent's block and pin with scion-owned modes, sets
// remote.SSH.configFile in the scion user data directory, and runs code
// with the fixed mechanism flag. Nothing under ~/.ssh or the ssh-config
// known-hosts directory is written.
func TestVSCode_WritesBlockPinSettingsAndRunsCode(t *testing.T) {
	hub, env, fake := newVSCodeTest(t)
	p := vscodePaths{home: env.home}
	label := hub.label(t)
	host := "scion." + label + ".a.b-c"

	require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false))

	assert.Equal(t, []string{hub.endpoint(t) + " a/b-c"}, fake.preflightCalls)
	require.Len(t, fake.runs, 1)
	assert.Equal(t, []string{"--user-data-dir", p.userData(), "--remote", "ssh-remote+" + host, "/workspace"}, fake.runs[0])

	cfg := readTestFile(t, p.sshConfig())
	assert.True(t, strings.HasPrefix(cfg, vscodeHeader+"\n"))
	want, err := renderSSHHostBlockPinned(sshTestExe, hub.endpoint(t), label, "a", "b-c", "~/.scion/vscode/known_hosts", p.knownHosts())
	require.NoError(t, err)
	assert.Contains(t, cfg, want)
	assert.Contains(t, cfg, "--hub "+hub.endpoint(t)+" --project a --known-hosts "+p.knownHosts()+" --host-key-alias "+host+" b-c")
	assert.Contains(t, cfg, "StrictHostKeyChecking yes\n")
	assert.Equal(t, host+" "+mustNormKey(t, hub.agents[1].HostKey)+"\n", readTestFile(t, p.knownHosts()))
	assert.JSONEq(t, `{"remote.SSH.configFile": "`+p.sshConfig()+`"}`, readTestFile(t, p.settings()))

	for path, mode := range map[string]os.FileMode{
		p.dir(): 0o700, filepath.Join(p.userData(), "User"): 0o700,
		p.sshConfig(): 0o600, p.knownHosts(): 0o600, p.settings(): 0o600,
	} {
		fi, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, mode, fi.Mode().Perm(), path)
	}
	for _, untouched := range []string{
		filepath.Join(env.home, ".ssh"),
		filepath.Join(env.home, ".scion", "ssh"),
		filepath.Join(env.home, ".config", "Code"),
	} {
		_, err := os.Stat(untouched)
		assert.True(t, errors.Is(err, os.ErrNotExist), "%s must not be created", untouched)
	}
	assert.Empty(t, vscodeOut(env))
}

// Other agents' blocks and pins are kept; a changed pin is replaced with
// one line, an unchanged one silently.
func TestVSCode_KeepsOtherEntriesAndReportsHostKeyChange(t *testing.T) {
	hub, env, _ := newVSCodeTest(t)
	p := vscodePaths{home: env.home}
	label := hub.label(t)
	host := "scion." + label + ".a.b-c"
	other := "scion.elsewhere.p.q"
	otherBlock, err := renderSSHHostBlockKnown(sshTestExe, "https://elsewhere.example", "elsewhere", "p", "q", vscodeKnownHostsRef)
	require.NoError(t, err)
	otherKey := mustNormKey(t, testSSHPublicKey(t))
	oldKey := mustNormKey(t, testSSHPublicKey(t))
	require.NoError(t, ensureSSHDir(env.home, p.dir()))
	require.NoError(t, os.WriteFile(p.sshConfig(), []byte(renderVSCodeSSHConfig(map[string]string{other: otherBlock})), 0o600))
	require.NoError(t, os.WriteFile(p.knownHosts(), []byte(renderKnownHosts(map[string]string{other: otherKey, host: oldKey})), 0o600))

	require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false))
	assert.Equal(t, vscodeRecreatedLine+"\n", vscodeOut(env))
	blocks := parseSSHHostBlocks(readTestFile(t, p.sshConfig()))
	assert.Equal(t, otherBlock, blocks[other])
	assert.Contains(t, blocks, host)
	keys := parseKnownHosts(readTestFile(t, p.knownHosts()))
	assert.Equal(t, otherKey, keys[other])
	assert.Equal(t, mustNormKey(t, hub.agents[1].HostKey), keys[host])

	env.stdout.(interface{ Reset() }).Reset()
	require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false))
	assert.Empty(t, vscodeOut(env), "an unchanged pin prints nothing")
}

// The hub label recorded by an existing scion-<hub> include file is reused,
// and the include file is only read.
func TestVSCode_ReusesIncludeFileLabel(t *testing.T) {
	hub, env, fake := newVSCodeTest(t)
	sp := sshPaths{home: env.home}
	require.NoError(t, ensureSSHDir(env.home, sp.includeDir()))
	include := renderSSHIncludeFile(hub.endpoint(t), "custom", nil)
	require.NoError(t, os.WriteFile(sp.includeFile("custom"), []byte(include), 0o600))
	before := sshFilesSnapshot(t, filepath.Join(env.home, ".ssh"))

	require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false))
	require.Len(t, fake.runs, 1)
	assert.Contains(t, fake.runs[0], "ssh-remote+scion.custom.a.b-c")
	assert.Contains(t, parseSSHHostBlocks(readTestFile(t, vscodePaths{home: env.home}.sshConfig())), "scion.custom.a.b-c")
	assert.Equal(t, before, sshFilesSnapshot(t, filepath.Join(env.home, ".ssh")), "~/.ssh must not change")
}

// Concurrent updates under the directory lock keep every entry. All
// writers start together at a barrier and no step sleeps or times out.
// With the lock, no entry can be lost; without it, eight writers released
// at once lose entries with high probability, so this test catches a
// missing lock with high probability, not certainty.
func TestUpdateVSCodeFiles_ConcurrentWritersKeepAllEntries(t *testing.T) {
	p := vscodePaths{home: t.TempDir()}
	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	hosts := make([]string, n)
	for i := 0; i < n; i++ {
		agent := fmt.Sprintf("ag%d", i)
		hosts[i] = "scion.h.p." + agent
		block, err := renderSSHHostBlockKnown(sshTestExe, "https://h.example", "h", "p", agent, vscodeKnownHostsRef)
		require.NoError(t, err)
		key := mustNormKey(t, testSSHPublicKey(t))
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			<-start
			_, err := updateVSCodeFiles(p, vscodeUpdate{host: host, block: block, key: key, endpoint: tunnelclient.NormalizeEndpoint("https://h.example")})
			errs <- err
		}(hosts[i])
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	blocks := parseSSHHostBlocks(readTestFile(t, p.sshConfig()))
	keys := parseKnownHosts(readTestFile(t, p.knownHosts()))
	for _, h := range hosts {
		assert.Contains(t, blocks, h)
		assert.Contains(t, keys, h)
	}
	assert.Len(t, blocks, n)
	assert.Len(t, keys, n)
}

// --prune drops only this hub's agents that a complete listing no longer
// has; a truncated listing changes nothing and runs neither the preflight
// nor code.
func TestVSCode_Prune(t *testing.T) {
	seed := func(t *testing.T, env *sshCommandEnv, hub *fakeSSHHub) (gone, kept, other string) {
		p := vscodePaths{home: env.home}
		label := hub.label(t)
		gone, kept, other = "scion."+label+".a.gone", "scion."+label+".a-b.c", "scion.elsewhere.p.q"
		blocks := map[string]string{}
		keys := map[string]string{}
		for _, h := range []string{gone, kept, other} {
			parts := strings.Split(h, ".")
			endpoint := hub.endpoint(t)
			if h == other {
				endpoint = "https://elsewhere.example"
			}
			b, err := renderSSHHostBlockKnown(sshTestExe, endpoint, parts[1], parts[2], parts[3], vscodeKnownHostsRef)
			require.NoError(t, err)
			blocks[h] = b
			keys[h] = mustNormKey(t, testSSHPublicKey(t))
		}
		require.NoError(t, ensureSSHDir(env.home, p.dir()))
		require.NoError(t, os.WriteFile(p.sshConfig(), []byte(renderVSCodeSSHConfig(blocks)), 0o600))
		require.NoError(t, os.WriteFile(p.knownHosts(), []byte(renderKnownHosts(keys)), 0o600))
		return gone, kept, other
	}

	t.Run("complete listing", func(t *testing.T) {
		hub, env, _ := newVSCodeTest(t)
		gone, kept, other := seed(t, env, hub)
		require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, true))
		p := vscodePaths{home: env.home}
		blocks := parseSSHHostBlocks(readTestFile(t, p.sshConfig()))
		keys := parseKnownHosts(readTestFile(t, p.knownHosts()))
		assert.NotContains(t, blocks, gone)
		assert.NotContains(t, keys, gone)
		for _, h := range []string{kept, other, "scion." + hub.label(t) + ".a.b-c"} {
			assert.Contains(t, blocks, h)
			assert.Contains(t, keys, h)
		}
	})
	t.Run("without --prune nothing is removed", func(t *testing.T) {
		hub, env, _ := newVSCodeTest(t)
		gone, _, _ := seed(t, env, hub)
		require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false))
		assert.Contains(t, parseSSHHostBlocks(readTestFile(t, vscodePaths{home: env.home}.sshConfig())), gone)
	})
	t.Run("truncated listing", func(t *testing.T) {
		hub, env, fake := newVSCodeTest(t)
		hub.truncateAgents = true
		seed(t, env, hub)
		before := sshFilesSnapshot(t, vscodePaths{home: env.home}.dir())
		err := vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, true)
		assert.Equal(t, tunnelclient.ExitError, tunnelclient.CodeOf(err), "%v", err)
		assert.Equal(t, before, sshFilesSnapshot(t, vscodePaths{home: env.home}.dir()))
		assert.Empty(t, fake.preflightCalls)
		assert.Empty(t, fake.runs)
	})
}

// A preflight failure ends the command with that exit code and line,
// before any file is written or code is run.
func TestVSCode_PreflightFailureCodes(t *testing.T) {
	for _, code := range []int{
		tunnelclient.ExitCredential, tunnelclient.ExitAgentNotFound, tunnelclient.ExitForbidden,
		tunnelclient.ExitHubUnavailable, tunnelclient.ExitCapReached, tunnelclient.ExitAgentNotRunning,
		tunnelclient.ExitTargetUnavailable, tunnelclient.ExitNoTunnelSupport, tunnelclient.ExitError,
	} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			_, env, fake := newVSCodeTest(t)
			fake.preflightErr = tunnelclient.Errorf(code, "preflight line %d", code)
			err := vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false)
			assert.Equal(t, code, tunnelclient.CodeOf(err))
			assert.Equal(t, fmt.Sprintf("preflight line %d", code), err.Error())
			assert.Empty(t, fake.runs)
			_, statErr := os.Stat(vscodePaths{home: env.home}.dir())
			assert.True(t, errors.Is(statErr, os.ErrNotExist), "nothing is written")
		})
	}
}

// A missing or failing VS Code CLI is exit 1 with one line naming code.
func TestVSCode_CodeMissingOrFailing(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, env, fake := newVSCodeTest(t)
		fake.lookPathErr = exec.ErrNotFound
		err := vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false)
		assert.Equal(t, tunnelclient.ExitError, tunnelclient.CodeOf(err))
		assert.Equal(t, "VS Code CLI code not found on PATH", err.Error())
	})
	t.Run("nonzero exit", func(t *testing.T) {
		if _, err := os.Stat("/bin/sh"); err != nil {
			t.Skip("needs /bin/sh")
		}
		_, env, fake := newVSCodeTest(t)
		fake.runErr = func() error { return exec.Command("/bin/sh", "-c", "exit 3").Run() }
		err := vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false)
		assert.Equal(t, tunnelclient.ExitError, tunnelclient.CodeOf(err))
		assert.Equal(t, "code exited with status 3", err.Error())
	})
	t.Run("start error", func(t *testing.T) {
		_, env, fake := newVSCodeTest(t)
		fake.runErr = func() error { return errors.New("permission denied") }
		err := vscodeMain(context.Background(), env, "a/b-c", vscodeDefaultFolder, false)
		assert.Equal(t, tunnelclient.ExitError, tunnelclient.CodeOf(err))
		assert.Equal(t, "starting code: permission denied", err.Error())
	})
}

// --path chooses the folder; it must be absolute.
func TestVSCode_FolderAndArgs(t *testing.T) {
	_, env, fake := newVSCodeTest(t)
	require.NoError(t, vscodeMain(context.Background(), env, "a/b-c", "/workspace/sub dir", false))
	require.Len(t, fake.runs, 1)
	assert.Equal(t, "/workspace/sub dir", fake.runs[0][len(fake.runs[0])-1])

	err := vscodeMain(context.Background(), env, "a/b-c", "relative", false)
	assert.Equal(t, tunnelclient.ExitUsage, tunnelclient.CodeOf(err))
	assert.Len(t, fake.runs, 1, "code is not run for an invalid --path")

	assert.Equal(t, tunnelclient.ExitUsage, tunnelclient.CodeOf(vscodeCmd.Args(vscodeCmd, nil)))
	assert.Equal(t, tunnelclient.ExitUsage, tunnelclient.CodeOf(vscodeCmd.Args(vscodeCmd, []string{"a", "b"})))
	assert.NoError(t, vscodeCmd.Args(vscodeCmd, []string{"a"}))
	assert.True(t, plainErrorCommands["vscode"])
	assert.False(t, commandRequiresProject(vscodeCmd))
}

// settings.json is created, merged when it is plain JSON, left alone when
// it has comments and already holds the value, and otherwise refused with
// the file and the key named.
func TestEnsureVSCodeSettings(t *testing.T) {
	setup := func(t *testing.T, content string) vscodePaths {
		p := vscodePaths{home: t.TempDir()}
		if content != "" {
			require.NoError(t, ensureSSHDir(p.home, filepath.Dir(p.settings())))
			require.NoError(t, os.WriteFile(p.settings(), []byte(content), 0o600))
		}
		return p
	}
	t.Run("created", func(t *testing.T) {
		p := setup(t, "")
		require.NoError(t, ensureVSCodeSettings(p))
		assert.JSONEq(t, `{"remote.SSH.configFile": "`+p.sshConfig()+`"}`, readTestFile(t, p.settings()))
	})
	for name, content := range map[string]string{"empty": "", "whitespace": " \n\t\n", "null": "null\n"} {
		t.Run(name+" counts as an empty object", func(t *testing.T) {
			p := vscodePaths{home: t.TempDir()}
			require.NoError(t, ensureSSHDir(p.home, filepath.Dir(p.settings())))
			require.NoError(t, os.WriteFile(p.settings(), []byte(content), 0o600))
			require.NoError(t, ensureVSCodeSettings(p))
			assert.JSONEq(t, `{"remote.SSH.configFile": "`+p.sshConfig()+`"}`, readTestFile(t, p.settings()))
		})
	}
	t.Run("plain JSON is merged", func(t *testing.T) {
		p := setup(t, `{"editor.fontSize": 14, "remote.SSH.configFile": "/old"}`)
		require.NoError(t, ensureVSCodeSettings(p))
		assert.JSONEq(t, `{"editor.fontSize": 14, "remote.SSH.configFile": "`+p.sshConfig()+`"}`, readTestFile(t, p.settings()))
	})
	t.Run("comments with the value are kept", func(t *testing.T) {
		p := vscodePaths{home: t.TempDir()}
		content := "{\n  // mine\n  \"editor.fontSize\": 14,\n  /* scion */ \"remote.SSH.configFile\": \"" + p.sshConfig() + "\",\n}\n"
		require.NoError(t, ensureSSHDir(p.home, filepath.Dir(p.settings())))
		require.NoError(t, os.WriteFile(p.settings(), []byte(content), 0o600))
		require.NoError(t, ensureVSCodeSettings(p))
		assert.Equal(t, content, readTestFile(t, p.settings()))
	})
	t.Run("comments without the value are refused", func(t *testing.T) {
		content := "{\n  // mine\n  \"remote.SSH.configFile\": \"/old\"\n}\n"
		p := setup(t, content)
		err := ensureVSCodeSettings(p)
		require.Error(t, err)
		assert.Contains(t, err.Error(), p.settings())
		assert.Contains(t, err.Error(), "remote.SSH.configFile")
		assert.Equal(t, content, readTestFile(t, p.settings()))
	})
}

func TestStripJSONComments(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"comments, trailing commas and comment text in strings",
			"{\"a\": \"// not a comment\", // c\n \"b\": [1, 2,], /* x */ \"c\": \"/* s */\",\n}",
			`{"a": "// not a comment", "b": [1, 2], "c": "/* s */"}`,
		},
		{
			"a backslash-quote followed by comment text stays in the string",
			`{"a": "x\"// y", "b": "z\"/* w */"}`,
			`{"a": "x\"// y", "b": "z\"/* w */"}`,
		},
		{
			"a value ending in a doubled backslash closes the string",
			`{"c": "d\\" // tail` + "\n" + `, "e": "f\\" /* x */}`,
			`{"c": "d\\", "e": "f\\"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.JSONEq(t, tc.want, string(stripJSONComments([]byte(tc.in))))
		})
	}
}

// Two hubs whose hosts share a first DNS label get distinct labels, so
// their Host names never collide: a label an existing vscode block holds
// for another hub falls back.
func TestVSCodeHubLabel_SharedFirstDNSLabel(t *testing.T) {
	p := vscodePaths{home: t.TempDir()}
	a := tunnelclient.NormalizeEndpoint("https://hub.a.example")
	b := tunnelclient.NormalizeEndpoint("https://hub.b.example")

	labelA, err := vscodeHubLabel(p, &sshHubTarget{endpoint: a})
	require.NoError(t, err)
	assert.Equal(t, "hub", labelA)
	blockA, err := renderSSHHostBlockKnown(sshTestExe, a, labelA, "p", "x", vscodeKnownHostsRef)
	require.NoError(t, err)
	_, err = updateVSCodeFiles(p, vscodeUpdate{host: sshHostName(labelA, "p", "x"), block: blockA, key: mustNormKey(t, testSSHPublicKey(t)), endpoint: a})
	require.NoError(t, err)

	labelB, err := vscodeHubLabel(p, &sshHubTarget{endpoint: b})
	require.NoError(t, err)
	assert.Equal(t, "hub-b-example", labelB)
	assert.NotEqual(t, sshHostName(labelA, "p", "x"), sshHostName(labelB, "p", "x"))

	again, err := vscodeHubLabel(p, &sshHubTarget{endpoint: a})
	require.NoError(t, err)
	assert.Equal(t, labelA, again, "the label a block records for its own hub is reused")
}

// When vscode blocks of other hubs hold both label candidates, the error
// names the vscode file, not an include directory.
func TestVSCodeHubLabel_BothCandidatesHeldInVSCodeFile(t *testing.T) {
	p := vscodePaths{home: t.TempDir()}
	for _, h := range []struct{ endpoint, label, agent string }{
		{"http://hub.example", "hub", "a"},
		{"https://hub.example:8443", "hub-example", "b"},
	} {
		block, err := renderSSHHostBlockKnown(sshTestExe, h.endpoint, h.label, "p", h.agent, vscodeKnownHostsRef)
		require.NoError(t, err)
		_, err = updateVSCodeFiles(p, vscodeUpdate{host: sshHostName(h.label, "p", h.agent), block: block, key: mustNormKey(t, testSSHPublicKey(t)), endpoint: tunnelclient.NormalizeEndpoint(h.endpoint)})
		require.NoError(t, err)
	}
	_, err := vscodeHubLabel(p, &sshHubTarget{endpoint: tunnelclient.NormalizeEndpoint("https://hub.example")})
	require.Error(t, err)
	assert.Equal(t, tunnelclient.ExitError, tunnelclient.CodeOf(err))
	assert.Contains(t, err.Error(), p.sshConfig())
	assert.Contains(t, err.Error(), `"hub" and "hub-example"`)
	assert.NotContains(t, err.Error(), "include files in .")
}

// Pruning selects blocks by the hub their ProxyCommand pins, so another
// hub's blocks and pins survive even under the same label; a host held by
// another hub is never overwritten.
func TestUpdateVSCodeFiles_PruneAndOwnershipByPinnedHub(t *testing.T) {
	p := vscodePaths{home: t.TempDir()}
	a := tunnelclient.NormalizeEndpoint("https://hub.a.example")
	b := tunnelclient.NormalizeEndpoint("https://hub.b.example")
	put := func(endpoint, agent string, keep map[string]bool) error {
		block, err := renderSSHHostBlockKnown(sshTestExe, endpoint, "x", "p", agent, vscodeKnownHostsRef)
		require.NoError(t, err)
		_, err = updateVSCodeFiles(p, vscodeUpdate{host: sshHostName("x", "p", agent), block: block, key: mustNormKey(t, testSSHPublicKey(t)), endpoint: endpoint, keep: keep})
		return err
	}
	require.NoError(t, put(a, "a1", nil))
	require.NoError(t, put(b, "b1", nil))

	require.NoError(t, put(a, "a2", map[string]bool{}))
	blocks := parseSSHHostBlocks(readTestFile(t, p.sshConfig()))
	keys := parseKnownHosts(readTestFile(t, p.knownHosts()))
	assert.NotContains(t, blocks, "scion.x.p.a1", "hub a's unlisted host is pruned")
	assert.NotContains(t, keys, "scion.x.p.a1")
	assert.Contains(t, blocks, "scion.x.p.a2")
	assert.Contains(t, blocks, "scion.x.p.b1", "hub b's block survives hub a's prune")
	assert.Contains(t, keys, "scion.x.p.b1")

	before := sshFilesSnapshot(t, p.dir())
	err := put(a, "b1", nil)
	require.Error(t, err)
	assert.Equal(t, "host scion.x.p.b1 in "+p.sshConfig()+" is pinned to hub "+b+"; remove that block from "+p.sshConfig()+" to use this name", err.Error())
	assert.Equal(t, before, sshFilesSnapshot(t, p.dir()), "nothing is written")
}

func TestVSCodeBlockHub(t *testing.T) {
	rendered, err := renderSSHHostBlockKnown(sshTestExe, "https://hub.example", "h", "p", "a", vscodeKnownHostsRef)
	require.NoError(t, err)
	assert.Equal(t, tunnelclient.NormalizeEndpoint("https://hub.example"), vscodeBlockHub(rendered))
	quoted := "Host scion.h.p.a\n  ProxyCommand /s tunnel --stdio --ssh --hub 'https://Hub.Example:443/' --project p a\n"
	assert.Equal(t, tunnelclient.NormalizeEndpoint("https://hub.example"), vscodeBlockHub(quoted))
	assert.Equal(t, "https://h.example/a b'c%d", sshShellUnquote(sshShellQuote("https://h.example/a b'c%d")))
	assert.Empty(t, vscodeBlockHub("Host scion.h.p.a\n  HostKeyAlias scion.h.p.a\n"))
}

// The preflight result maps to the command's error: tunnel client errors
// pass through unchanged, the timeout is exit 6, anything else exit 1.
func TestVSCodePreflightError(t *testing.T) {
	te := tunnelclient.Errorf(tunnelclient.ExitAgentNotRunning, "agent is not running")
	for _, tc := range []struct {
		name string
		in   error
		code int
		line string
	}{
		{"nil", nil, tunnelclient.ExitOK, ""},
		{"tunnel client error", fmt.Errorf("wrapped: %w", te), tunnelclient.ExitAgentNotRunning, "agent is not running"},
		{"timeout", fmt.Errorf("dial: %w", context.DeadlineExceeded), tunnelclient.ExitHubUnavailable, "checking the SSH tunnel to p/a timed out after 1m0s"},
		{"other", errors.New("boom"), tunnelclient.ExitError, "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vscodePreflightError(tc.in, "p", "a")
			assert.Equal(t, tc.code, tunnelclient.CodeOf(got))
			if tc.in == nil {
				assert.NoError(t, got)
				return
			}
			assert.Equal(t, tc.line, got.Error())
		})
	}
	assert.Same(t, te, vscodePreflightError(te, "p", "a"), "a tunnel client error is returned as it is")
}
