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

// Hermetic checks of image-build/scripts/check-image-size.sh: it reports the
// size delta of two images (docker: summed layer sizes from history; podman:
// image inspect) and fails only when the growth exceeds the budget. docker
// and podman are fakes on PATH that answer from a table.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCheckImageSize(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	script, err := filepath.Abs(filepath.Join("..", "image-build", "scripts", "check-image-size.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeFake := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Invoked as: docker history --human=false --format {{.Size}} <ref>;
	// prints one size per layer, empty layers included.
	fakeDocker := `#!/bin/sh
[ "$1" = history ] && [ "$2" = --human=false ] || { echo "fake docker: unexpected call: $*" >&2; exit 3; }
case "$5" in
base) printf '0\n1000000000\n48576000\n' ;;  # 1000 MiB over three layers
blanks) printf '0\n\n1000000000\n\n48576000\n' ;;
grew200) printf '0\n1000000000\n48576000\n209715200\n' ;;
warned) echo "WARNING: image platform does not match" >&2; printf '1258291200\n' ;;
grew600) printf '1048576000\n629145600\n' ;;
junk) printf '1000000000\n493MB\n' ;;
shim) printf '328.6MB\n36.9kB\n' ;;  # podman-docker ignores --human=false
nolayers) ;;
*) echo "Error: No such image: $5" >&2; exit 1 ;;
esac
`
	// Invoked as: podman image inspect --format {{.Size}} <ref>. podman
	// history prints sizes human-formatted only, so it must never be called.
	fakePodman := `#!/bin/sh
[ "$1" = image ] && [ "$2" = inspect ] && [ "$3" = --format ] || { echo "fake podman: unexpected call: $*" >&2; exit 3; }
case "$5" in
base) echo 1048576000 ;;
grew200) echo 1258291200 ;;
human) echo "1.05GB" ;;
*) echo "Error: base: image not known: $5" >&2; exit 125 ;;
esac
`
	// Both engines, then the system tools.
	both := t.TempDir()
	writeFake(both, "docker", fakeDocker)
	writeFake(both, "podman", fakePodman)
	bothPath := both + ":/usr/bin:/bin"

	// PATH-restricted directories: only the given fakes plus links to the
	// few tools the script runs, so an engine installed on the test host
	// cannot be found. "" when a tool is missing here; cases using such a
	// directory are then skipped, the others still run.
	restricted := func(fakes map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		for _, tool := range []string{"awk", "basename", "cat", "mktemp", "rm"} {
			p, err := exec.LookPath(tool)
			if err != nil {
				return ""
			}
			if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
				t.Fatal(err)
			}
		}
		for name, body := range fakes {
			writeFake(dir, name, body)
		}
		return dir
	}
	podmanOnly := restricted(map[string]string{"podman": fakePodman})
	noEngine := restricted(nil)
	const restrictedUnavailable = "restricted-path-unavailable"
	if podmanOnly == "" {
		podmanOnly, noEngine = restrictedUnavailable, restrictedUnavailable
	}

	run := func(path string, env []string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Env = append([]string{"PATH=" + path, "LC_ALL=C"}, env...)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("run: %v", err)
			}
			code = ee.ExitCode()
		}
		return string(out), code
	}

	cases := []struct {
		name     string
		path     string // PATH; bothPath when empty
		env      []string
		args     []string
		wantCode int
		wants    []string
	}{
		{"sums every layer", "", nil, []string{"base", "grew200"}, 0,
			[]string{"sum of uncompressed layer sizes (docker history):", "1000.0 MiB  base", "1200.0 MiB  grew200"}},
		{"blank lines between layers", "", nil, []string{"blanks", "grew200"}, 0,
			[]string{"1000.0 MiB  blanks", "200.0 MiB", "ok: within budget"}},
		{"engine stderr is not parsed", "", nil, []string{"base", "warned"}, 0,
			[]string{"1200.0 MiB  warned", "ok: within budget"}},
		{"within default budget", "", nil, []string{"base", "grew200"}, 0,
			[]string{"delta", "200.0 MiB", "(budget 550 MiB)", "ok: within budget"}},
		{"over default budget", "", nil, []string{"base", "grew600"}, 1,
			[]string{"600.0 MiB", "FAIL: grew600 grew by 600.0 MiB over base, above the 550 MiB budget"}},
		{"flag raises budget", "", nil, []string{"--budget-mib", "700", "base", "grew600"}, 0,
			[]string{"(budget 700 MiB)", "ok: within budget"}},
		{"env sets budget", "", []string{"IMAGE_SIZE_BUDGET_MIB=100"}, []string{"base", "grew200"}, 1,
			[]string{"above the 100 MiB budget"}},
		{"shrinking passes", "", nil, []string{"grew600", "base"}, 0,
			[]string{"-600.0 MiB", "ok: within budget"}},
		{"missing image", "", nil, []string{"base", "nope"}, 2,
			[]string{"cannot read the layers of nope with docker: Error: No such image: nope"}},
		{"non-numeric layer size", "", nil, []string{"base", "junk"}, 2,
			[]string{"unexpected size for junk from docker: '493MB'"}},
		{"docker that is podman's shim", "", nil, []string{"base", "shim"}, 2,
			[]string{"unexpected size for shim from docker: '328.6MB' (if docker is podman's docker shim, set IMAGE_TOOL=podman)"}},
		{"no layers", "", nil, []string{"base", "nolayers"}, 2,
			[]string{"no size reported for nolayers by docker"}},
		{"leading-zero budget is decimal", "", nil, []string{"--budget-mib", "0300", "base", "grew200"}, 0,
			[]string{"(budget 300 MiB)", "ok: within budget"}},
		{"budget 08 is not octal", "", nil, []string{"--budget-mib", "08", "base", "grew200"}, 1,
			[]string{"above the 8 MiB budget"}},
		{"bad budget", "", nil, []string{"--budget-mib", "lots", "base", "grew200"}, 2,
			[]string{"budget must be a whole number of MiB"}},
		{"budget digits bounded", "", nil, []string{"--budget-mib", "1234567", "base", "grew200"}, 2,
			[]string{"budget must be at most 6 digits of MiB"}},
		{"six-digit budget", "", nil, []string{"--budget-mib", "999999", "base", "grew600"}, 0,
			[]string{"(budget 999999 MiB)", "ok: within budget"}},
		{"wrong arg count", "", nil, []string{"base"}, 2,
			[]string{"usage:"}},
		{"podman via IMAGE_TOOL", "", []string{"IMAGE_TOOL=podman"}, []string{"base", "grew200"}, 0,
			[]string{"unpacked image size (podman image inspect):", "1000.0 MiB  base", "1200.0 MiB  grew200", "ok: within budget"}},
		{"podman when docker is absent", podmanOnly, nil, []string{"base", "grew200"}, 0,
			[]string{"unpacked image size (podman image inspect):", "200.0 MiB", "ok: within budget"}},
		{"podman missing image", podmanOnly, nil, []string{"base", "nope"}, 2,
			[]string{"cannot inspect nope with podman: Error: base: image not known: nope"}},
		{"podman non-numeric size", podmanOnly, nil, []string{"base", "human"}, 2,
			[]string{"unexpected size for human from podman: '1.05GB'"}},
		{"no engine", noEngine, nil, []string{"base", "grew200"}, 2,
			[]string{"neither docker nor podman found; set IMAGE_TOOL"}},
		{"unknown IMAGE_TOOL", "", []string{"IMAGE_TOOL=nerdctl"}, []string{"base", "grew200"}, 2,
			[]string{"IMAGE_TOOL must be docker or podman, got 'nerdctl'"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := c.path
			if path == restrictedUnavailable {
				t.Skip("a tool the PATH-restricted cases link to is missing on this host")
			}
			if path == "" {
				path = bothPath
			}
			out, code := run(path, c.env, c.args...)
			expectOutput(t, c.name, out, code, c.wantCode, c.wants...)
		})
	}
}
