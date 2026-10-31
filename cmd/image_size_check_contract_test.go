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
// size delta between two images and fails only when the growth exceeds the
// budget. docker is a fake on PATH that answers `image inspect` from a table.

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
	bin := t.TempDir()
	// Invoked as: docker image inspect --format {{.Size}} <ref>
	fakeDocker := `#!/bin/sh
case "$5" in
base) echo 1048576000 ;;  # 1000 MiB
grew200) echo 1258291200 ;;
grew600) echo 1677721600 ;;
junk) echo "not-a-number" ;;
*) echo "Error: No such image: $5" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(env []string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "LC_ALL=C"}, env...)
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
		env      []string
		args     []string
		wantCode int
		wants    []string
	}{
		{"within default budget", nil, []string{"base", "grew200"}, 0,
			[]string{"delta", "200.0 MiB", "(budget 500 MiB)", "ok: within budget"}},
		{"over default budget", nil, []string{"base", "grew600"}, 1,
			[]string{"600.0 MiB", "FAIL: grew600 grew by 600.0 MiB over base, above the 500 MiB budget"}},
		{"flag raises budget", nil, []string{"--budget-mib", "700", "base", "grew600"}, 0,
			[]string{"(budget 700 MiB)", "ok: within budget"}},
		{"env sets budget", []string{"IMAGE_SIZE_BUDGET_MIB=100"}, []string{"base", "grew200"}, 1,
			[]string{"above the 100 MiB budget"}},
		{"shrinking passes", nil, []string{"grew600", "base"}, 0,
			[]string{"-600.0 MiB", "ok: within budget"}},
		{"missing image", nil, []string{"base", "nope"}, 2,
			[]string{"cannot inspect nope with docker", "No such image"}},
		{"unparseable size", nil, []string{"base", "junk"}, 2,
			[]string{"unexpected size for junk"}},
		{"bad budget", nil, []string{"--budget-mib", "lots", "base", "grew200"}, 2,
			[]string{"budget must be a whole number of MiB"}},
		{"wrong arg count", nil, []string{"base"}, 2,
			[]string{"usage:"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := run(c.env, c.args...)
			expectOutput(t, c.name, out, code, c.wantCode, c.wants...)
		})
	}
}
