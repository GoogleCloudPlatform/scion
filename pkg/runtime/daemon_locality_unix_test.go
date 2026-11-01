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

//go:build !windows

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
)

// fakeCLI writes an executable shell script that prints stdout and stderr
// and exits with code.
func fakeCLI(t *testing.T, stdout, stderr string, code int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakecli")
	script := "#!/bin/sh\nprintf '%s' '" + stderr + "' >&2\nprintf '%s' '" + stdout + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunStdoutCommand_IgnoresStderr(t *testing.T) {
	cli := fakeCLI(t, "unix:///var/run/docker.sock\n", "WARNING: config file has unknown keys\n", 0)
	out, err := runStdoutCommand(context.Background(), cli)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != "unix:///var/run/docker.sock" {
		t.Fatalf("out = %q: stderr leaked into the parsed output", out)
	}
}

func TestRunStdoutCommand_FailsOnNonZeroExit(t *testing.T) {
	cli := fakeCLI(t, "unix:///var/run/docker.sock\n", "error\n", 1)
	if _, err := runStdoutCommand(context.Background(), cli); err == nil {
		t.Fatal("a non-zero exit was not an error")
	}
}

// TestDockerDaemonLocal_StderrWarningStillLocal runs the real command path
// (no scripted runner): a CLI warning on stderr does not make a local
// endpoint unresolvable.
func TestDockerDaemonLocal_StderrWarningStillLocal(t *testing.T) {
	prevEnv := daemonLocalityEnv
	daemonLocalityEnv = func(string) string { return "" }
	t.Cleanup(func() { daemonLocalityEnv = prevEnv })
	cli := fakeCLI(t, "unix:///var/run/docker.sock\n", "WARNING: Error loading config file\n", 0)
	local, err := dockerDaemonLocal(context.Background(), cli)
	if err != nil || !local {
		t.Fatalf("got (%v, %v), want local", local, err)
	}
	// Unparseable stdout still fails closed.
	bad := fakeCLI(t, "unix:///a.sock\nunix:///b.sock\n", "", 0)
	if _, err := dockerDaemonLocal(context.Background(), bad); err == nil {
		t.Fatal("multi-line stdout was accepted")
	}
}

// TestPodmanDaemonLocal_StderrWarningStillParsed: rootless Podman's
// WARN lines on stderr do not break the connection list parse.
func TestPodmanDaemonLocal_StderrWarningStillParsed(t *testing.T) {
	prevEnv := daemonLocalityEnv
	daemonLocalityEnv = func(string) string { return "" }
	t.Cleanup(func() { daemonLocalityEnv = prevEnv })
	cli := fakeCLI(t, "", "WARN[0000] The cgroupv2 manager is set to systemd but there is no systemd user session available\n", 0)
	local, err := podmanDaemonLocal(context.Background(), cli)
	if err != nil || !local {
		t.Fatalf("got (%v, %v), want the local engine", local, err)
	}
}

// TestInspectContainerBinds_StderrWarningStillParses: a CLI warning on
// stderr (rootless Podman, a Docker config warning) does not make every
// container's inspect unverifiable.
func TestInspectContainerBinds_StderrWarningStillParses(t *testing.T) {
	cli := fakeCLI(t, `{"Mounts":[{"Type":"bind","Source":"/a","Destination":"/x"}],"Labels":{"scion.name":"a"}}`,
		"WARN[0000] The cgroupv2 manager is set to systemd but there is no systemd user session available\n", 0)
	ib, err := inspectContainerBinds(context.Background(), cli, "c1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ib.ReportedKnown || len(ib.Reported) != 1 || ib.Labels["scion.name"] != "a" {
		t.Fatalf("inspected = %+v", ib)
	}
}

// TestRunStdoutCommand_TimesOut: a hung CLI returns an error within the
// bound instead of blocking (and the locality check then fails closed).
func TestRunStdoutCommand_TimesOut(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hangcli")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := stdoutCommandTimeout
	stdoutCommandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { stdoutCommandTimeout = prev })
	start := time.Now()
	if _, err := runStdoutCommand(context.Background(), p); err == nil {
		t.Fatal("a hung command returned no error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("returned after %v, want within the bound", d)
	}
	prevEnv := daemonLocalityEnv
	daemonLocalityEnv = func(string) string { return "" }
	t.Cleanup(func() { daemonLocalityEnv = prevEnv })
	if local, err := dockerDaemonLocal(context.Background(), p); err == nil || local {
		t.Fatalf("a hung CLI gave (%v, %v), want not local with an error", local, err)
	}
}

func TestVerifyRunningContainerBinds_HostMntSymlinkedParentMissingTail(t *testing.T) {
	_, global, outside := controlPathFixture(t)
	if err := os.MkdirAll(filepath.Join(global, "hub-credentials"), 0o755); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(outside, "host_mnt")
	clear := filepath.Join(outside, "clear")
	for _, d := range []string{clear, filepath.Dir(prefix + clear)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := dockerDesktopHostPrefix
	dockerDesktopHostPrefix = prefix
	t.Cleanup(func() { dockerDesktopHostPrefix = prev })
	// <prefix><clear> -> <global>/hub-credentials; the tail "gone" exists
	// under neither.
	if err := os.Symlink(filepath.Join(global, "hub-credentials"), prefix+clear); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ib := InspectedBinds{ReportedKnown: true, DaemonLocal: func() (bool, error) { return true, nil },
		Reported: []BindMount{{Source: prefix + clear + "/gone", Destination: "/x"}}}
	if err := VerifyRunningContainerBinds(ib); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want the prefixed path refused", err)
	}
}

func TestVerifyRunningContainerBinds_HostMntUnreadablePrefixRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	_, _, outside := controlPathFixture(t)
	prefix := filepath.Join(outside, "host_mnt")
	clear := filepath.Join(outside, "clear")
	if err := os.MkdirAll(filepath.Dir(prefix+clear), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(clear, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(prefix, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(prefix, 0o755) })
	prev := dockerDesktopHostPrefix
	dockerDesktopHostPrefix = prefix
	t.Cleanup(func() { dockerDesktopHostPrefix = prev })
	ib := InspectedBinds{ReportedKnown: true, DaemonLocal: func() (bool, error) { return true, nil },
		Reported: []BindMount{{Source: prefix + clear, Destination: "/x"}}}
	if err := VerifyRunningContainerBinds(ib); !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want a refusal when the prefixed path cannot be resolved", err)
	}
}
