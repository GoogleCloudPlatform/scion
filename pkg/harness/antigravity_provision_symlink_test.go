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

package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

// TestAntigravityProvisionRefusesSymlinkedAgentsDir reproduces, end to end
// against the real antigravity/provision.py and its staged scion_harness.py,
// a workload committing ".agents" as a symlink to a directory it does not
// own. _generate_hooks_json must not create anything inside that target
// directory: it computes the hooks.json path underneath the symlinked
// ".agents" and hands it to scion_harness.atomic_write_json, which now
// refuses to open a symlinked parent (O_DIRECTORY|O_NOFOLLOW) instead of
// creating hooks.json inside whatever ".agents" points at. provision.py's
// own try/except around this call turns that refusal into a logged warning,
// not a crash — this test asserts the outward-visible half of that contract
// (the sentinel directory stays empty) and would fail if the guard were
// removed (the file would appear inside sentinelDir).
func TestAntigravityProvisionRefusesSymlinkedAgentsDir(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := t.TempDir()
	if err := config.SeedHarnessConfigFromDir(dir, harnessesEmbed.FS, "antigravity", false); err != nil {
		t.Fatalf("SeedHarnessConfigFromDir: %v", err)
	}

	workspace := t.TempDir()
	sentinelDir := filepath.Join(workspace, "sentinel")
	if err := os.MkdirAll(sentinelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentsLink := filepath.Join(workspace, ".agents")
	if err := os.Symlink(sentinelDir, agentsLink); err != nil {
		t.Fatal(err)
	}

	code := "import sys\n" +
		"sys.path.insert(0, " + pyQuote(dir) + ")\n" +
		"import provision\n" +
		"provision._generate_hooks_json(" + pyQuote(dir) + ")\n"

	cmd := exec.Command(pyPath, "-c", code)
	cmd.Env = append(os.Environ(), "SCION_WORKSPACE_PATH="+workspace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("_generate_hooks_json: %v\noutput:\n%s", err, out)
	}

	entries, err := os.ReadDir(sentinelDir)
	if err != nil {
		t.Fatalf("read sentinel dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("sentinel dir got %d entries, want 0 (hooks.json must never land inside the symlink target): %v", len(entries), entries)
	}

	// The symlink itself must also be left exactly as the workload planted
	// it — never replaced with a real directory (which would itself be a
	// silent, surprising side effect distinct from the write-through this
	// test is about, even though it would not leak into the sentinel dir).
	info, err := os.Lstat(agentsLink)
	if err != nil {
		t.Fatalf("lstat .agents: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf(".agents is no longer a symlink (mode %v) — provision.py replaced it", info.Mode())
	}
}

// TestAntigravityProvisionRefusesSymlinkedHooksJsonTmp reproduces, end to
// end against the real antigravity/provision.py and its staged
// scion_harness.py, a workload committing a symlink to a file it does not
// own at the exact name scion_harness.atomic_write_json is about to create
// its temp file at. _generate_hooks_json's call into atomic_write_json must
// never write through that symlink: the temp file is opened
// O_CREAT|O_EXCL|O_NOFOLLOW, so a pre-existing symlink there is refused
// rather than followed. provision.py's own try/except around the call turns
// that refusal into a logged warning, not a crash — this test asserts the
// sentinel file's content is byte-for-byte unchanged, and would fail if the
// guard were removed (the sentinel would be overwritten with the generated
// hooks.json content instead).
//
// The real temp name is unique per call (pid + a monotonic timestamp + a
// counter — see scion_harness._atomic_tmp_name), precisely so a workload
// cannot predict and pre-plant a symlink at it the way it could when the
// name was the fixed "hooks.json.tmp". This test's injected Python
// monkeypatches _atomic_tmp_name to a fixed, known name before importing
// provision, so it can still plant the symlink at the exact path this call
// will use — mirroring how scion_harness_test.py's own unit tests prove the
// same O_EXCL guard.
func TestAntigravityProvisionRefusesSymlinkedHooksJsonTmp(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := t.TempDir()
	if err := config.SeedHarnessConfigFromDir(dir, harnessesEmbed.FS, "antigravity", false); err != nil {
		t.Fatalf("SeedHarnessConfigFromDir: %v", err)
	}

	workspace := t.TempDir()
	agentsDir := filepath.Join(workspace, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinelFile := filepath.Join(workspace, "sentinel.txt")
	const sentinelContent = "original contents\n"
	if err := os.WriteFile(sentinelFile, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}
	const fixedTmpName = "hooks.json.tmp-fixed-for-test"
	tmpLink := filepath.Join(agentsDir, fixedTmpName)
	if err := os.Symlink(sentinelFile, tmpLink); err != nil {
		t.Fatal(err)
	}

	code := "import sys\n" +
		"sys.path.insert(0, " + pyQuote(dir) + ")\n" +
		"import scion_harness\n" +
		"scion_harness._atomic_tmp_name = lambda name: " + pyQuote(fixedTmpName) + "\n" +
		"import provision\n" +
		"provision._generate_hooks_json(" + pyQuote(dir) + ")\n"

	cmd := exec.Command(pyPath, "-c", code)
	cmd.Env = append(os.Environ(), "SCION_WORKSPACE_PATH="+workspace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("_generate_hooks_json: %v\noutput:\n%s", err, out)
	}

	got, err := os.ReadFile(sentinelFile)
	if err != nil {
		t.Fatalf("read sentinel file: %v", err)
	}
	if string(got) != sentinelContent {
		t.Errorf("sentinel file = %q, want %q (hooks.json content must never be written through the symlink)", got, sentinelContent)
	}

	// The planted symlink itself must still be exactly what the workload
	// planted, and hooks.json must never have been created via a rename over
	// or alongside it.
	info, err := os.Lstat(tmpLink)
	if err != nil {
		t.Fatalf("lstat %s: %v", fixedTmpName, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink (mode %v) — provision.py replaced it", fixedTmpName, info.Mode())
	}
	if _, err := os.Lstat(filepath.Join(agentsDir, "hooks.json")); err == nil {
		t.Errorf("hooks.json was created in %s despite the refused write", agentsDir)
	}
}

// TestAntigravityProvisionUnaffectedByStaleFixedNameTmpFile proves a stale
// file at hooks.json.tmp has no effect because the temp name is randomized:
// a leftover file that happens to sit at that fixed name — e.g. left over
// from an older container image, or a workload artifact that happens to
// collide with it — plays no part in a real provisioning run. hooks.json is
// created normally, with real content, and the stale file is left
// untouched.
func TestAntigravityProvisionUnaffectedByStaleFixedNameTmpFile(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := t.TempDir()
	if err := config.SeedHarnessConfigFromDir(dir, harnessesEmbed.FS, "antigravity", false); err != nil {
		t.Fatalf("SeedHarnessConfigFromDir: %v", err)
	}

	workspace := t.TempDir()
	agentsDir := filepath.Join(workspace, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleTmp := filepath.Join(agentsDir, "hooks.json.tmp")
	const staleContent = "leftover from an older image\n"
	if err := os.WriteFile(staleTmp, []byte(staleContent), 0o644); err != nil {
		t.Fatal(err)
	}

	code := "import sys\n" +
		"sys.path.insert(0, " + pyQuote(dir) + ")\n" +
		"import provision\n" +
		"provision._generate_hooks_json(" + pyQuote(dir) + ")\n"

	cmd := exec.Command(pyPath, "-c", code)
	cmd.Env = append(os.Environ(), "SCION_WORKSPACE_PATH="+workspace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("_generate_hooks_json: %v\noutput:\n%s", err, out)
	}

	if _, err := os.Lstat(filepath.Join(agentsDir, "hooks.json")); err != nil {
		t.Errorf("hooks.json was not created in %s: %v (a stale legacy-named temp file must not block a real write)", agentsDir, err)
	}
	staleGot, err := os.ReadFile(staleTmp)
	if err != nil {
		t.Fatalf("read stale tmp file: %v", err)
	}
	if string(staleGot) != staleContent {
		t.Errorf("stale legacy-named temp file was modified: %q", staleGot)
	}
}

// pyQuote renders s as a single-quoted Python string literal, escaping the
// only two characters that matter for a path under a Go t.TempDir() (a
// single quote or a backslash never appear there in practice, but this
// stays correct if that ever changes).
func pyQuote(s string) string {
	out := "'"
	for _, r := range s {
		switch r {
		case '\\', '\'':
			out += `\` + string(r)
		default:
			out += string(r)
		}
	}
	return out + "'"
}
