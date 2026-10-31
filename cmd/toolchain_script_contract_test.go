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

// Hermetic checks of image-build/lib/install-core-toolchain.sh, which the
// image builds run under `set -euo pipefail`:
//   - a broken, already-present go, gh or helm is treated as "not installed"
//     and leads to a reinstall, instead of aborting the build in the version
//     probe;
//   - verify_installed, the post-install check, fails when another copy
//     shadows the installed one on PATH, or when the installed binary reports
//     the wrong version or does not run.
//
// The script is sourced (its main only runs when executed) with fake tools on
// a restricted PATH. Nothing is downloaded or installed: curl is replaced by a
// shell function that exits with a sentinel status, so "reached the download"
// is observable without network access.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const toolchainScript = "image-build/lib/install-core-toolchain.sh"

// curlSentinel is the exit status of the stubbed curl: the step got past its
// version probe and started the reinstall.
const curlSentinel = 42

type toolchainEnv struct {
	t      *testing.T
	script string
	bin    string // fake tools, first on PATH
}

func newToolchainEnv(t *testing.T) *toolchainEnv {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	script, err := filepath.Abs(filepath.Join("..", toolchainScript))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return &toolchainEnv{t: t, script: script, bin: bin}
}

// fake writes an executable shell script named name into dir.
func (e *toolchainEnv) fake(dir, name, body string) string {
	e.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// broken is the body of a binary that exists but cannot run: it prints
// something on stderr and stdout and exits non-zero, like a wrong-architecture
// binary or one with a missing shared library.
const broken = `echo "exec format error" >&2; echo garbage; exit 126`

// run sources the script and runs the shell snippet with PATH set to the fake
// bin directory followed by pathTail. curl and dpkg are stubbed.
func (e *toolchainEnv) run(pathTail, snippet string) (string, int) {
	e.t.Helper()
	driver := `source "$1"
curl() { echo "STUB-CURL $*"; exit ` + strconv.Itoa(curlSentinel) + `; }
dpkg() { echo amd64; }
` + snippet
	cmd := exec.Command("bash", "-c", driver, "driver", e.script)
	cmd.Env = []string{"PATH=" + e.bin + pathTail, "HOME=" + e.t.TempDir(), "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatalf("run: %v", err)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// systemPath is appended after the fake bin so the script's own utilities
// (awk, sed, sort, head, ...) resolve.
const systemPath = ":/usr/bin:/bin"

func expectOutput(t *testing.T, name, out string, code, wantCode int, wants ...string) {
	t.Helper()
	if code != wantCode {
		t.Errorf("%s: exit %d, want %d; output:\n%s", name, code, wantCode, out)
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("%s: output lacks %q; output:\n%s", name, w, out)
		}
	}
}

func TestToolchainProbeVersionNeverAborts(t *testing.T) {
	e := newToolchainEnv(t)
	e.fake(e.bin, "brokentool", broken)
	e.fake(e.bin, "goodtool", `echo "tool 1.2.3"; echo "second line"`)

	out, code := e.run(systemPath, `v="$(probe_version brokentool --version)"; echo "probe=[$v]"`)
	expectOutput(t, "broken tool", out, code, 0, "probe=[]")

	out, code = e.run(systemPath, `v="$(probe_version goodtool --version)"; echo "probe=[$v]"`)
	expectOutput(t, "good tool", out, code, 0, "probe=[tool 1.2.3]")

	out, code = e.run(systemPath, `v="$(probe_version no-such-tool)"; echo "probe=[$v]"`)
	expectOutput(t, "missing tool", out, code, 0, "probe=[]")
}

func TestToolchainBrokenToolIsReinstalled(t *testing.T) {
	cases := []struct{ tool, step, want string }{
		{"go", "step_go 1.26.1 1.26.1", "go at "},
		{"gh", "step_gh 2.100.0", "gh at "},
		{"helm", "step_helm 3.17.3 aaaa bbbb", "helm at "},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			e := newToolchainEnv(t)
			e.fake(e.bin, c.tool, broken)
			out, code := e.run(systemPath, c.step)
			// Exit curlSentinel means the probe did not abort the step and it
			// went on to download the pinned version.
			expectOutput(t, c.tool, out, code, curlSentinel, c.want, "does not run", "STUB-CURL")
		})
	}
}

func TestToolchainAdequateToolIsKept(t *testing.T) {
	cases := []struct{ tool, body, step, want string }{
		{"go", `echo "go version go1.27.1 linux/amd64"`, "step_go 1.26.1 1.26.1", "go 1.27.1 already satisfies"},
		{"helm", `echo "v3.17.3"`, "step_helm 3.17.3 aaaa bbbb", "helm 3.17.3 already present"},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			e := newToolchainEnv(t)
			e.fake(e.bin, c.tool, c.body)
			out, code := e.run(systemPath, c.step)
			expectOutput(t, c.tool, out, code, 0, c.want)
		})
	}

	// An older but working tool is replaced (reaches the download) and says why.
	e := newToolchainEnv(t)
	e.fake(e.bin, "gh", `echo "gh version 2.0.0 (2020-01-01)"`)
	out, code := e.run(systemPath, "step_gh 2.100.0")
	expectOutput(t, "old gh", out, code, curlSentinel, "gh 2.0.0 is below 2.100.0", "STUB-CURL")
}

func TestToolchainVerifyInstalled(t *testing.T) {
	e := newToolchainEnv(t)
	root := t.TempDir()
	instDir := filepath.Join(root, "inst")
	shadowDir := filepath.Join(root, "shadow")
	good := e.fake(instDir, "helm", `echo "v3.17.3"`)
	e.fake(shadowDir, "helm", `echo "v3.10.0"`)
	wrong := e.fake(filepath.Join(root, "wrong"), "helm", `echo "v3.0.0"`)
	bad := e.fake(filepath.Join(root, "bad"), "helm", broken)

	check := func(name, pathTail, binary string, wantCode int, wants ...string) {
		t.Helper()
		out, code := e.run(pathTail, `verify_installed helm "`+binary+`" 3.17.3 helm_version_of "`+binary+`"`)
		expectOutput(t, name, out, code, wantCode, wants...)
	}
	check("installed copy first on PATH", ":"+instDir+systemPath, good, 0, "verified helm 3.17.3 at "+good)
	check("shadowed by an earlier copy", ":"+shadowDir+":"+instDir+systemPath, good, 1,
		"helm resolves to '"+filepath.Join(shadowDir, "helm")+"'", "Another copy is earlier on PATH")
	// Without instDir on PATH, helm is either absent or some host copy;
	// either way it is not the installed binary.
	check("not on PATH", systemPath, good, 1, "helm resolves to '", "after installing 3.17.3 to "+good)
	check("wrong version", ":"+filepath.Dir(wrong)+systemPath, wrong, 1, "reports version '3.0.0', expected 3.17.3")
	check("installed binary does not run", ":"+filepath.Dir(bad)+systemPath, bad, 1, "<none: the binary does not run>")
}

// TestToolchainScriptStillRunsWhenExecuted: the source guard must not stop the
// Dockerfiles' `install-core-toolchain.sh <step>` invocations from running.
func TestToolchainScriptStillRunsWhenExecuted(t *testing.T) {
	e := newToolchainEnv(t)
	cmd := exec.Command("bash", e.script, "no-such-step")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "unknown step: no-such-step") {
		t.Errorf("executing the script with an unknown step: err %v, output:\n%s", err, out)
	}
}

// TestToolchainPlaywrightDeps: the playwright-deps step installs Playwright's
// WebKit/Firefox library list only on a suite it has a list for, uses the real
// trixie ALSA package name, and skips any other suite without touching apt.
func TestToolchainPlaywrightDeps(t *testing.T) {
	e := newToolchainEnv(t)
	osRelease := func(body string) string {
		p := filepath.Join(t.TempDir(), "os-release")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// apt-get is stubbed: every call is logged on fd 3, which the step does
	// not redirect, so the simulate pre-flight (run with its output sent to
	// /dev/null) is visible too. The pre-flight succeeds unless APT_SIM_FAIL
	// names a package in the set. apt_cleanup is stubbed too, so the test
	// never removes /var/lib/apt/lists even as root.
	const stubs = `exec 3>&1
apt-get() {
  echo "APT $*" >&3
  if [ "$1" = install ] && [ "$2" = -s ] && [ -n "${APT_SIM_FAIL:-}" ]; then
    case " $* " in *" $APT_SIM_FAIL "*) return 100 ;; esac
  fi
  return 0
}
apt_cleanup() { echo "APT-CLEANUP"; }
`
	trixie := osRelease("PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\n")
	noble := osRelease("PRETTY_NAME=\"Ubuntu 24.04.3 LTS\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n")

	out, code := e.run(systemPath, stubs+`OS_RELEASE_FILE="`+trixie+`"; step_playwright_deps`)
	expectOutput(t, "debian 13", out, code, 0,
		"installing Playwright WebKit and Firefox system libraries",
		"APT install -s --no-install-recommends",
		"APT install -y --no-install-recommends",
		" libasound2t64 ", " libgtk-4-1 ", " gstreamer1.0-plugins-bad ", " libicu76 ", " libavcodec61 ",
		"APT-CLEANUP")
	if strings.Contains(out, " libasound2 ") {
		t.Errorf("debian 13: installs the virtual libasound2 instead of libasound2t64; output:\n%s", out)
	}

	out, code = e.run(systemPath, stubs+`OS_RELEASE_FILE="`+noble+`"; step_playwright_deps`)
	expectOutput(t, "ubuntu 24.04", out, code, 0, "skip: no Playwright WebKit/Firefox library list for Ubuntu 24.04.3 LTS")
	if strings.Contains(out, "APT ") {
		t.Errorf("ubuntu 24.04: touched apt on a suite with no list; output:\n%s", out)
	}

	out, code = e.run(systemPath, stubs+`OS_RELEASE_FILE="`+filepath.Join(t.TempDir(), "missing")+`"; step_playwright_deps`)
	expectOutput(t, "missing os-release", out, code, 0, "skip: no Playwright WebKit/Firefox library list for unknown")
	if strings.Contains(out, "APT ") {
		t.Errorf("missing os-release: touched apt; output:\n%s", out)
	}

	out, code = e.run(systemPath, stubs+`APT_SIM_FAIL=libicu76; apt_installable() { [ "$1" != libicu76 ]; }
OS_RELEASE_FILE="`+trixie+`"; step_playwright_deps`)
	expectOutput(t, "renamed package", out, code, 1, "FAIL: apt cannot install these Playwright libraries: libicu76")
	if strings.Contains(out, "APT install -y") {
		t.Errorf("renamed package: installed despite a failed pre-flight; output:\n%s", out)
	}
}
