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

// TestToolchainAptCommonIncludesFfmpeg verifies step_apt_common installs system
// ffmpeg with --no-install-recommends on every base image chain (core-base and
// thick-prep) so Playwright video recording does not download ffmpeg at runtime.
func TestToolchainAptCommonIncludesFfmpeg(t *testing.T) {
	e := newToolchainEnv(t)
	const stubs = `exec 3>&1
apt-get() { echo "APT $*" >&3; return 0; }
apt_cleanup() { echo "APT-CLEANUP"; }
`
	out, code := e.run(systemPath, stubs+`step_apt_common`)
	expectOutput(t, "apt-common includes ffmpeg", out, code, 0,
		"APT install -s --no-install-recommends",
		"APT install -y --no-install-recommends",
		" ffmpeg ",
		"APT-CLEANUP")
}

// TestToolchainPlaywrightFfmpeg verifies step_playwright_ffmpeg populates an
// immutable 0755 root:root seed tree (/usr/local/share/scion-playwright-ffmpeg,
// overridable via SCION_PLAYWRIGHT_FFMPEG_SEED_DIR in tests) with symlinks to
// the system ffmpeg binary, installation markers, browsers.json, and
// package.json across known Playwright ffmpeg revisions plus any revision
// declared by the globally installed @playwright/cli package, without creating
// a world-writable PLAYWRIGHT_BROWSERS_PATH registry.
func TestToolchainPlaywrightFfmpeg(t *testing.T) {
	t.Run("seeds baseline and dynamic cli revisions", func(t *testing.T) {
		e := newToolchainEnv(t)
		ffmpegBin := e.fake(e.bin, "ffmpeg", `if [ "${1:-}" = "-version" ]; then echo "ffmpeg version 7.1.5"; exit 0; fi; exit 0`)

		seedDir := filepath.Join(t.TempDir(), "scion-playwright-ffmpeg")
		npmPrefix := filepath.Join(t.TempDir(), "npm-global")
		cliCoreDir := filepath.Join(npmPrefix, "lib", "node_modules", "@playwright", "cli", "node_modules", "playwright-core")
		if err := os.MkdirAll(cliCoreDir, 0o755); err != nil {
			t.Fatal(err)
		}
		browsersJSON := `{"comment":"test","browsers":[{"name":"chromium","revision":"1200"},{"name":"ffmpeg","revision":"1099"}]}`
		if err := os.WriteFile(filepath.Join(cliCoreDir, "browsers.json"), []byte(browsersJSON), 0o644); err != nil {
			t.Fatal(err)
		}

		snippet := `export SCION_PLAYWRIGHT_FFMPEG_SEED_DIR="` + seedDir + `"
export NPM_CONFIG_PREFIX="` + npmPrefix + `"
step_playwright_ffmpeg`
		out, code := e.run(systemPath, snippet)
		expectOutput(t, "playwright-ffmpeg", out, code, 0, "seeding Playwright ffmpeg")

		fi, err := os.Stat(seedDir)
		if err != nil {
			t.Fatalf("stat %s: %v", seedDir, err)
		}
		if got := fi.Mode() & (os.ModeSticky | os.ModePerm); got != 0o755 {
			t.Errorf("%s mode = %v, want 0755", seedDir, got)
		}
		if _, err := os.Lstat(filepath.Join(seedDir, ".links")); !os.IsNotExist(err) {
			t.Errorf("seed dir should not contain .links (found err=%v); .links are seeded into the persistent home registry by sciontool init", err)
		}

		for _, rev := range []string{"1009", "1010", "1011", "1012", "1013", "1099"} {
			revDir := filepath.Join(seedDir, "ffmpeg-"+rev)
			rfi, err := os.Stat(revDir)
			if err != nil {
				t.Fatalf("stat %s: %v", revDir, err)
			}
			if got := rfi.Mode() & (os.ModeSticky | os.ModePerm); got != 0o755 {
				t.Errorf("%s mode = %v, want 0755", revDir, got)
			}
			target, err := os.Readlink(filepath.Join(revDir, "ffmpeg-linux"))
			if err != nil {
				t.Fatalf("readlink ffmpeg-%s/ffmpeg-linux: %v", rev, err)
			}
			if target != ffmpegBin {
				t.Errorf("ffmpeg-%s/ffmpeg-linux -> %q, want %q", rev, target, ffmpegBin)
			}
			for _, marker := range []string{"INSTALLATION_COMPLETE", "DEPENDENCIES_VALIDATED"} {
				if _, err := os.Stat(filepath.Join(revDir, marker)); err != nil {
					t.Errorf("ffmpeg-%s/%s missing: %v", rev, marker, err)
				}
			}
			descBytes, err := os.ReadFile(filepath.Join(revDir, "browsers.json"))
			if err != nil {
				t.Fatalf("read ffmpeg-%s/browsers.json: %v", rev, err)
			}
			if !strings.Contains(string(descBytes), `"name":"ffmpeg"`) || !strings.Contains(string(descBytes), `"revision":"`+rev+`"`) {
				t.Errorf("ffmpeg-%s/browsers.json = %q, want ffmpeg descriptor for %s", rev, descBytes, rev)
			}
			// `playwright install --list` calls require(path.join(referenceDir, "package.json"))
			// and sorts by version.split(".").
			pkgBytes, err := os.ReadFile(filepath.Join(revDir, "package.json"))
			if err != nil {
				t.Fatalf("read ffmpeg-%s/package.json: %v", rev, err)
			}
			if !strings.Contains(string(pkgBytes), `"version":"0.0.0"`) {
				t.Errorf("ffmpeg-%s/package.json = %q, want semver version field", rev, pkgBytes)
			}
		}
	})

	t.Run("works when @playwright/cli browsers.json is absent", func(t *testing.T) {
		e := newToolchainEnv(t)
		e.fake(e.bin, "ffmpeg", `echo "ffmpeg version 7.1.5"`)
		seedDir := filepath.Join(t.TempDir(), "scion-playwright-ffmpeg")
		npmPrefix := filepath.Join(t.TempDir(), "empty-npm")

		out, code := e.run(systemPath, `export SCION_PLAYWRIGHT_FFMPEG_SEED_DIR="`+seedDir+`"
export NPM_CONFIG_PREFIX="`+npmPrefix+`"
step_playwright_ffmpeg`)
		expectOutput(t, "without cli browsers.json", out, code, 0, "seeding Playwright ffmpeg")
		for _, rev := range []string{"1011", "1013"} {
			if _, err := os.Stat(filepath.Join(seedDir, "ffmpeg-"+rev, "ffmpeg-linux")); err != nil {
				t.Errorf("ffmpeg-%s/ffmpeg-linux missing: %v", rev, err)
			}
		}
	})

	t.Run("fails when ffmpeg is missing or broken", func(t *testing.T) {
		e := newToolchainEnv(t)
		seedDir := filepath.Join(t.TempDir(), "scion-playwright-ffmpeg")
		// Restrict PATH to fake bin + basic coreutils without /usr/bin/ffmpeg:
		// place symlinks to needed utilities in a clean dir so host ffmpeg is hidden.
		cleanSys := filepath.Join(t.TempDir(), "sys")
		if err := os.MkdirAll(cleanSys, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, util := range []string{"mkdir", "chmod", "ln", "rm", "touch", "cat", "tr", "sort"} {
			if p, err := exec.LookPath(util); err == nil {
				_ = os.Symlink(p, filepath.Join(cleanSys, util))
			}
		}

		out, code := e.run(":"+cleanSys, `export SCION_PLAYWRIGHT_FFMPEG_SEED_DIR="`+seedDir+`"; step_playwright_ffmpeg`)
		expectOutput(t, "missing ffmpeg", out, code, 1, "FAIL: ffmpeg not found")

		e.fake(e.bin, "ffmpeg", broken)
		out, code = e.run(":"+cleanSys, `export SCION_PLAYWRIGHT_FFMPEG_SEED_DIR="`+seedDir+`"; step_playwright_ffmpeg`)
		expectOutput(t, "broken ffmpeg", out, code, 1, "FAIL: ffmpeg")
	})

	t.Run("dispatches via main", func(t *testing.T) {
		e := newToolchainEnv(t)
		e.fake(e.bin, "ffmpeg", `echo "ffmpeg version 7.1.5"`)
		seedDir := filepath.Join(t.TempDir(), "scion-playwright-ffmpeg")
		cmd := exec.Command("bash", e.script, "playwright-ffmpeg")
		cmd.Env = []string{
			"PATH=" + e.bin + systemPath,
			"SCION_PLAYWRIGHT_FFMPEG_SEED_DIR=" + seedDir,
			"NPM_CONFIG_PREFIX=" + filepath.Join(t.TempDir(), "empty-npm"),
			"LC_ALL=C",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bash %s playwright-ffmpeg failed: %v\n%s", e.script, err, out)
		}
		if _, err := os.Stat(filepath.Join(seedDir, "ffmpeg-1013", "ffmpeg-linux")); err != nil {
			t.Errorf("dispatched run did not create ffmpeg-1013/ffmpeg-linux: %v", err)
		}
	})
}

// TestToolchainPinnedKubectlAndGolangciLint verifies optional version+SHA256
// pinning for step_kubectl and step_golangci_lint:
//   - exact pins work for amd64 and arm64, verify SHA256 before extraction and
//     installation, and override an already-present binary;
//   - partial pins, floating/malformed versions, SHA256 mismatches, download
//     errors, extraction errors, and unsupported architectures fail closed
//     without invoking install;
//   - when all pin env vars are unset, the legacy skip-if-present and floating
//     install paths remain unchanged.
func TestToolchainPinnedKubectlAndGolangciLint(t *testing.T) {
	e := newToolchainEnv(t)

	// Build a real tar.gz fixture for golangci-lint and a dummy kubectl binary
	// so sha256sum and tar run unmodified against real inputs.
	fixtureDir := t.TempDir()
	kubectlFixture := filepath.Join(fixtureDir, "kubectl")
	if err := os.WriteFile(kubectlFixture, []byte("pinned kubectl\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		dir := filepath.Join(fixtureDir, "golangci-lint-1.2.3-linux-"+arch)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "golangci-lint"), []byte("pinned lint "+arch+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lintFixture := filepath.Join(fixtureDir, "lint.tar.gz")
	tarCmd := exec.Command("tar", "-czf", lintFixture, "-C", fixtureDir,
		"golangci-lint-1.2.3-linux-amd64", "golangci-lint-1.2.3-linux-arm64")
	if out, err := tarCmd.CombinedOutput(); err != nil {
		t.Fatalf("create lint fixture tar.gz: %v\n%s", err, out)
	}

	shaOf := func(p string) string {
		t.Helper()
		out, err := exec.Command("sha256sum", p).CombinedOutput()
		if err != nil {
			t.Fatalf("sha256sum %s: %v\n%s", p, err, out)
		}
		return strings.Fields(string(out))[0]
	}
	kubectlSHA := shaOf(kubectlFixture)
	lintSHA := shaOf(lintFixture)

	t.Run("exact pins succeed for amd64 and arm64 even when tool is already present", func(t *testing.T) {
		e.fake(e.bin, "kubectl", `echo "existing kubectl"`)
		e.fake(e.bin, "golangci-lint", `echo "existing golangci-lint"`)

		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(arch, func(t *testing.T) {
				snippet := `
deb_arch() { printf '%s\n' "` + arch + `"; }
curl() {
  local url="$2" dest="$4"
  echo "CURL $url"
  case "$url" in
  */kubectl) cp "` + kubectlFixture + `" "$dest" ;;
  */golangci-lint-*.tar.gz) cp "` + lintFixture + `" "$dest" ;;
  *) echo "unexpected URL: $url" >&2; return 1 ;;
  esac
}
install() {
  test -s "$7"
  echo "INSTALL $8"
}
KUBECTL_VERSION=1.2.3
KUBECTL_SHA256_AMD64=` + kubectlSHA + `
KUBECTL_SHA256_ARM64=` + kubectlSHA + `
GOLANGCI_LINT_VERSION=1.2.3
GOLANGCI_LINT_SHA256_AMD64=` + lintSHA + `
GOLANGCI_LINT_SHA256_ARM64=` + lintSHA + `
step_kubectl
step_golangci_lint
`
				out, code := e.run(systemPath, snippet)
				expectOutput(t, "pinned "+arch, out, code, 0,
					"CURL https://dl.k8s.io/release/v1.2.3/bin/linux/"+arch+"/kubectl",
					"INSTALL /usr/local/bin/kubectl",
					"CURL https://github.com/golangci/golangci-lint/releases/download/v1.2.3/golangci-lint-1.2.3-linux-"+arch+".tar.gz",
					"INSTALL /usr/local/bin/golangci-lint",
				)
			})
		}
	})

	t.Run("fail closed on partial, malformed, mismatched, download, extraction, and arch errors", func(t *testing.T) {
		zeroSHA := strings.Repeat("0", 64)
		cases := []struct {
			name    string
			tool    string
			arch    string
			env     string
			curlErr bool
			badTar  bool
			wantMsg string
		}{
			{
				name:    "kubectl partial (missing version)",
				tool:    "step_kubectl",
				arch:    "amd64",
				env:     "KUBECTL_SHA256_AMD64=" + kubectlSHA,
				wantMsg: "FAIL: pinned kubectl requires a concrete version and architecture SHA256",
			},
			{
				name:    "kubectl partial (missing arch sha)",
				tool:    "step_kubectl",
				arch:    "arm64",
				env:     "KUBECTL_VERSION=1.2.3\nKUBECTL_SHA256_AMD64=" + kubectlSHA,
				wantMsg: "FAIL: pinned kubectl requires a concrete version and architecture SHA256",
			},
			{
				name:    "kubectl floating version",
				tool:    "step_kubectl",
				arch:    "amd64",
				env:     "KUBECTL_VERSION=latest\nKUBECTL_SHA256_AMD64=" + kubectlSHA,
				wantMsg: "FAIL: pinned kubectl requires a concrete version and architecture SHA256",
			},
			{
				name:    "kubectl sha256 mismatch",
				tool:    "step_kubectl",
				arch:    "amd64",
				env:     "KUBECTL_VERSION=1.2.3\nKUBECTL_SHA256_AMD64=" + zeroSHA,
				wantMsg: "FAIL: download or SHA256 verification failed for pinned kubectl 1.2.3",
			},
			{
				name:    "kubectl download error",
				tool:    "step_kubectl",
				arch:    "amd64",
				env:     "KUBECTL_VERSION=1.2.3\nKUBECTL_SHA256_AMD64=" + kubectlSHA,
				curlErr: true,
				wantMsg: "FAIL: download or SHA256 verification failed for pinned kubectl 1.2.3",
			},
			{
				name:    "kubectl unsupported arch",
				tool:    "step_kubectl",
				arch:    "s390x",
				env:     "KUBECTL_VERSION=1.2.3\nKUBECTL_SHA256_AMD64=" + kubectlSHA,
				wantMsg: "FAIL: unsupported architecture for pinned kubectl",
			},
			{
				name:    "golangci-lint partial (missing version)",
				tool:    "step_golangci_lint",
				arch:    "amd64",
				env:     "GOLANGCI_LINT_SHA256_AMD64=" + lintSHA,
				wantMsg: "FAIL: pinned golangci-lint requires a concrete version and architecture SHA256",
			},
			{
				name:    "golangci-lint floating version",
				tool:    "step_golangci_lint",
				arch:    "amd64",
				env:     "GOLANGCI_LINT_VERSION=latest\nGOLANGCI_LINT_SHA256_AMD64=" + lintSHA,
				wantMsg: "FAIL: pinned golangci-lint requires a concrete version and architecture SHA256",
			},
			{
				name:    "golangci-lint sha256 mismatch",
				tool:    "step_golangci_lint",
				arch:    "amd64",
				env:     "GOLANGCI_LINT_VERSION=1.2.3\nGOLANGCI_LINT_SHA256_AMD64=" + zeroSHA,
				wantMsg: "FAIL: download or SHA256 verification failed for pinned golangci-lint 1.2.3",
			},
			{
				name:    "golangci-lint extraction failure",
				tool:    "step_golangci_lint",
				arch:    "amd64",
				env:     "GOLANGCI_LINT_VERSION=1.2.3\nGOLANGCI_LINT_SHA256_AMD64=" + kubectlSHA,
				badTar:  true,
				wantMsg: "FAIL: extracting pinned golangci-lint 1.2.3",
			},
			{
				name:    "golangci-lint unsupported arch",
				tool:    "step_golangci_lint",
				arch:    "s390x",
				env:     "GOLANGCI_LINT_VERSION=1.2.3\nGOLANGCI_LINT_SHA256_AMD64=" + lintSHA,
				wantMsg: "FAIL: unsupported architecture for pinned golangci-lint",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				curlBody := `cp "` + lintFixture + `" "$4"`
				if tc.curlErr {
					curlBody = `return 9`
				} else if tc.tool == "step_kubectl" || tc.badTar {
					curlBody = `cp "` + kubectlFixture + `" "$4"`
				}
				snippet := `
deb_arch() { printf '%s\n' "` + tc.arch + `"; }
curl() { ` + curlBody + `; }
install() { echo "UNEXPECTED-INSTALL $*"; }
` + tc.env + `
` + tc.tool + `
`
				out, code := e.run(systemPath, snippet)
				expectOutput(t, tc.name, out, code, 1, tc.wantMsg)
				if strings.Contains(out, "UNEXPECTED-INSTALL") {
					t.Errorf("%s: install was called on failure; output:\n%s", tc.name, out)
				}
			})
		}
	})

	t.Run("unpinned skips when already present", func(t *testing.T) {
		e2 := newToolchainEnv(t)
		e2.fake(e2.bin, "kubectl", `echo "existing"`)
		e2.fake(e2.bin, "golangci-lint", `echo "existing"`)

		out, code := e2.run(systemPath, `
unset KUBECTL_VERSION KUBECTL_SHA256_AMD64 KUBECTL_SHA256_ARM64
unset GOLANGCI_LINT_VERSION GOLANGCI_LINT_SHA256_AMD64 GOLANGCI_LINT_SHA256_ARM64
curl() { echo "UNEXPECTED-CURL $*"; exit 1; }
step_kubectl
step_golangci_lint
`)
		expectOutput(t, "unpinned skip", out, code, 0,
			"skip: kubectl already present",
			"skip: golangci-lint already present",
		)
	})

	t.Run("unpinned uses original stable/latest install paths when absent", func(t *testing.T) {
		e2 := newToolchainEnv(t)
		out, code := e2.run(systemPath, `
unset KUBECTL_VERSION KUBECTL_SHA256_AMD64 KUBECTL_SHA256_ARM64
unset GOLANGCI_LINT_VERSION GOLANGCI_LINT_SHA256_AMD64 GOLANGCI_LINT_SHA256_ARM64
command() {
  if [ "${1:-}" = -v ] && { [ "${2:-}" = kubectl ] || [ "${2:-}" = golangci-lint ]; }; then
    return 1
  fi
  builtin command "$@"
}
deb_arch() { echo arm64; }
curl() {
  case "$*" in
  *stable.txt*) echo v1.32.0 ;;
  *HEAD/install.sh*) echo "fixture-installer" ;;
  *) echo "LEGACY-CURL $*" ;;
  esac
}
install() { echo "LEGACY-INSTALL $*"; }
rm() { :; }
sh() { echo "LEGACY-SH $*"; cat >/dev/null; }
step_kubectl
step_golangci_lint
`)
		expectOutput(t, "unpinned install", out, code, 0,
			"LEGACY-CURL -fsSLo /tmp/kubectl https://dl.k8s.io/release/v1.32.0/bin/linux/arm64/kubectl",
			"LEGACY-INSTALL -o root -g root -m 0755 /tmp/kubectl /usr/local/bin/kubectl",
			"LEGACY-SH -s -- -b /usr/local/bin",
		)
	})
}
