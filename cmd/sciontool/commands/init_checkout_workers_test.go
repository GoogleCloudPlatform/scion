/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

type checkoutCloneFixture struct {
	root    string
	realGit string
	bare    string
	seed    string
	binDir  string
	argvLog string
}

func newCheckoutCloneFixture(t *testing.T) *checkoutCloneFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if os.Geteuid() == 0 {
		t.Skip("runs as root; privilege-drop path differs")
	}

	root := t.TempDir()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gcPath := filepath.Join(root, "gc")
	if err := os.WriteFile(gcPath, []byte("[user]\n\temail=a@b.c\n\tname=A\n[init]\n\tdefaultBranch=main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &checkoutCloneFixture{
		root:    root,
		realGit: realGit,
		bare:    filepath.Join(root, "remote.git"),
		seed:    filepath.Join(root, "seed"),
		binDir:  filepath.Join(root, "bin"),
		argvLog: filepath.Join(root, "argv.log"),
	}

	f.runGit(root, "init", "-q", "--bare", f.bare)
	f.runGit(root, "init", "-q", "-b", "main", f.seed)
	if err := os.WriteFile(filepath.Join(f.seed, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runGit(f.seed, "add", ".")
	f.runGit(f.seed, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "init")
	f.runGit(f.seed, "push", "-q", f.bare, "main")

	if err := os.MkdirAll(f.binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Record each invocation as NUL-separated arguments terminated by a newline
	// (no test argument contains NUL or LF), skipping internal checkout--worker
	// subprocesses if git ever resolves them via PATH.
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do if [ \"$a\" = 'checkout--worker' ]; then exec '" + realGit + "' \"$@\"; fi; done\n" +
		"{\n" +
		"  for a in \"$@\"; do printf '%s\\000' \"$a\"; done\n" +
		"  printf '\\n'\n" +
		"} >> '" + f.argvLog + "'\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(f.binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return f
}

func (f *checkoutCloneFixture) runGit(dir string, args ...string) {
	c := exec.Command(f.realGit, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(f.root, "gc"), "GIT_CONFIG_SYSTEM="+os.DevNull)
	if out, err := c.CombinedOutput(); err != nil {
		panic("git " + strings.Join(args, " ") + ": " + err.Error() + "\n" + string(out))
	}
}

func (f *checkoutCloneFixture) setCommonEnv(t *testing.T, workspace string) {
	t.Helper()
	t.Setenv("PATH", f.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", f.root)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(f.root, "gc"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("SCION_GIT_CLONE_URL", "file://"+f.bare)
	t.Setenv(api.EnvGitTokenRemote, "")
	t.Setenv("SCION_WORKSPACE_PATH", workspace)
	t.Setenv("SCION_GIT_BRANCH", "main")
	t.Setenv("SCION_GIT_DEPTH", "0")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("SCION_AGENT_BRANCH", "")
	t.Setenv("SCION_AGENT_NAME", "test-agent")
	t.Setenv("SCION_GIT_CHECKOUT_WORKERS", "")
}

func (f *checkoutCloneFixture) recordedCheckoutCalls(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(f.argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(line, "\x00"), "\x00")
		for _, p := range parts {
			if p == "checkout" {
				out = append(out, parts)
				break
			}
		}
	}
	return out
}

// TestGitCloneWorkspace_DefaultParallelCheckoutArgv pins that every git
// checkout invocation in gitCloneWorkspace's default clone + new-agent-branch
// flow carries "-c checkout.workers=16" ahead of "checkout", and that
// checkout.workers is not persisted into .git/config.
func TestGitCloneWorkspace_DefaultParallelCheckoutArgv(t *testing.T) {
	f := newCheckoutCloneFixture(t)
	workspace := filepath.Join(f.root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	f.setCommonEnv(t, workspace)

	if err := gitCloneWorkspace(0, 0, f.root, false); err != nil {
		t.Fatalf("gitCloneWorkspace: %v", err)
	}

	got := f.recordedCheckoutCalls(t)
	want := [][]string{
		{"-c", "checkout.workers=16", "-C", workspace, "checkout", "-b", "main", "origin/main"},
		{"-c", "checkout.workers=16", "-C", workspace, "checkout", "scion/test-agent"},
		{"-c", "checkout.workers=16", "-C", workspace, "checkout", "-b", "scion/test-agent"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkout calls mismatch:\n got: %q\nwant: %q", got, want)
	}

	repoConfig, err := os.ReadFile(filepath.Join(workspace, ".git", "config"))
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	if strings.Contains(string(repoConfig), "checkout") {
		t.Errorf("checkout.workers must be passed via -c, not persisted in .git/config:\n%s", repoConfig)
	}
}

// TestGitCloneWorkspace_RemoteAgentBranchFallbackUsesParallelCheckout exercises
// the fallback path where SCION_AGENT_BRANCH is unset (so main is fetched and
// checked out first) and scion/<agentName> already exists on origin with
// divergent files, ensuring the tracking checkout also receives
// "-c checkout.workers=16".
func TestGitCloneWorkspace_RemoteAgentBranchFallbackUsesParallelCheckout(t *testing.T) {
	f := newCheckoutCloneFixture(t)
	f.runGit(f.seed, "checkout", "-q", "-b", "scion/existing-agent")
	if err := os.WriteFile(filepath.Join(f.seed, "branch-only.txt"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runGit(f.seed, "add", ".")
	f.runGit(f.seed, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "agent branch")
	f.runGit(f.seed, "push", "-q", f.bare, "scion/existing-agent")

	workspace := filepath.Join(f.root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	f.setCommonEnv(t, workspace)
	t.Setenv("SCION_AGENT_NAME", "existing-agent")

	if err := gitCloneWorkspace(0, 0, f.root, false); err != nil {
		t.Fatalf("gitCloneWorkspace: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace, "branch-only.txt")); err != nil {
		t.Fatalf("expected branch-only.txt from remote agent branch: %v", err)
	}

	got := f.recordedCheckoutCalls(t)
	want := [][]string{
		{"-c", "checkout.workers=16", "-C", workspace, "checkout", "-b", "main", "origin/main"},
		{"-c", "checkout.workers=16", "-C", workspace, "checkout", "scion/existing-agent"},
		{"-c", "checkout.workers=16", "-C", workspace, "checkout", "-b", "scion/existing-agent", "origin/scion/existing-agent"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkout calls mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestGitCloneWorkspace_ExplicitAgentBranchUsesParallelCheckout covers when
// SCION_AGENT_BRANCH names an existing remote branch fetched directly on the
// pre-attempt path.
func TestGitCloneWorkspace_ExplicitAgentBranchUsesParallelCheckout(t *testing.T) {
	f := newCheckoutCloneFixture(t)
	f.runGit(f.seed, "checkout", "-q", "-b", "scion/feat")
	if err := os.WriteFile(filepath.Join(f.seed, "feat.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.runGit(f.seed, "add", ".")
	f.runGit(f.seed, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "feat")
	f.runGit(f.seed, "push", "-q", f.bare, "scion/feat")

	workspace := filepath.Join(f.root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	f.setCommonEnv(t, workspace)
	t.Setenv("SCION_AGENT_BRANCH", "scion/feat")

	if err := gitCloneWorkspace(0, 0, f.root, false); err != nil {
		t.Fatalf("gitCloneWorkspace: %v", err)
	}

	got := f.recordedCheckoutCalls(t)
	want := [][]string{
		{"-c", "checkout.workers=16", "-C", workspace, "checkout", "-b", "scion/feat", "origin/scion/feat"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkout calls mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestGitCloneWorkspace_CheckoutWorkersEnvOverride verifies valid overrides
// (including 1 for sequential and 0 for CPU count) and fallback + warning on
// invalid or out-of-range SCION_GIT_CHECKOUT_WORKERS values.
func TestGitCloneWorkspace_CheckoutWorkersEnvOverride(t *testing.T) {
	cases := []struct {
		name     string
		envVal   string
		wantArg  string
		wantWarn string
	}{
		{
			name:    "explicit 8 workers",
			envVal:  "8",
			wantArg: "checkout.workers=8",
		},
		{
			name:    "sequential 1 worker",
			envVal:  "1",
			wantArg: "checkout.workers=1",
		},
		{
			name:    "auto 0 workers",
			envVal:  "0",
			wantArg: "checkout.workers=0",
		},
		{
			name:    "whitespace trimmed max",
			envVal:  " 64\n",
			wantArg: "checkout.workers=64",
		},
		{
			name:     "non-numeric falls back to 16",
			envVal:   "abc",
			wantArg:  "checkout.workers=16",
			wantWarn: `[WARN] SCION_GIT_CHECKOUT_WORKERS is not a valid integer in 0..64 ("abc"), defaulting to 16`,
		},
		{
			name:     "negative falls back to 16",
			envVal:   "-1",
			wantArg:  "checkout.workers=16",
			wantWarn: `[WARN] SCION_GIT_CHECKOUT_WORKERS is not a valid integer in 0..64 ("-1"), defaulting to 16`,
		},
		{
			name:     "above max falls back to 16",
			envVal:   "65",
			wantArg:  "checkout.workers=16",
			wantWarn: `[WARN] SCION_GIT_CHECKOUT_WORKERS is not a valid integer in 0..64 ("65"), defaulting to 16`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readLog := captureSciontoolLog(t)
			f := newCheckoutCloneFixture(t)
			workspace := filepath.Join(f.root, "workspace")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			f.setCommonEnv(t, workspace)
			t.Setenv("SCION_GIT_CHECKOUT_WORKERS", tc.envVal)

			if err := gitCloneWorkspace(0, 0, f.root, false); err != nil {
				t.Fatalf("gitCloneWorkspace: %v", err)
			}

			calls := f.recordedCheckoutCalls(t)
			if len(calls) != 3 {
				t.Fatalf("expected 3 checkout calls, got %d: %q", len(calls), calls)
			}
			for i, call := range calls {
				if len(call) < 2 || call[0] != "-c" || call[1] != tc.wantArg {
					t.Errorf("call[%d] = %q, want leading [-c %s]", i, call, tc.wantArg)
				}
			}

			logged := readLog()
			if tc.wantWarn != "" {
				if !strings.Contains(logged, tc.wantWarn) {
					t.Errorf("log missing %q; got:\n%s", tc.wantWarn, logged)
				}
				if strings.Contains(logged, "WARNING:") {
					t.Errorf("log must not include redundant WARNING: prefix; got:\n%s", logged)
				}
			} else if strings.Contains(logged, "SCION_GIT_CHECKOUT_WORKERS") {
				t.Errorf("unexpected SCION_GIT_CHECKOUT_WORKERS log for valid input %q:\n%s", tc.envVal, logged)
			}
		})
	}
}

// TestGitCloneWorkspace_ParallelCheckoutPopulatesAboveThreshold seeds more
// than Git's default checkout.thresholdForParallelism (100 files) so git
// actually spawns checkout--worker subprocesses under CombinedOutputManaged.
func TestGitCloneWorkspace_ParallelCheckoutPopulatesAboveThreshold(t *testing.T) {
	f := newCheckoutCloneFixture(t)
	const fileCount = 128
	if err := os.MkdirAll(filepath.Join(f.seed, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < fileCount; i++ {
		name := filepath.Join(f.seed, "pkg", fmt.Sprintf("file_%03d.txt", i))
		if err := os.WriteFile(name, []byte("content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.runGit(f.seed, "add", ".")
	f.runGit(f.seed, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "many files")
	f.runGit(f.seed, "push", "-q", f.bare, "main")

	workspace := filepath.Join(f.root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	f.setCommonEnv(t, workspace)

	if err := gitCloneWorkspace(0, 0, f.root, false); err != nil {
		t.Fatalf("gitCloneWorkspace: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(workspace, "pkg"))
	if err != nil {
		t.Fatalf("read workspace/pkg: %v", err)
	}
	if len(entries) != fileCount {
		t.Fatalf("expected %d checked-out files in pkg/, got %d", fileCount, len(entries))
	}
}
