/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

// openScriptForTest is a small helper that opens path (via the same
// symlink-safe walk executeScriptEnforced uses) and returns its file and
// chain, for tests that want to drive buildEnforcedCmd directly without
// invoking cmd.Run() (which would require real privilege to exercise the
// dropped branch's setgroups(2) call in this test environment).
func openScriptForTest(t *testing.T, path string) (f *os.File, chain []NodeOwnership) {
	t.Helper()
	dirFd, chain, err := openChainNoFollow(filepath.Dir(path))
	if err != nil {
		t.Fatalf("openChainNoFollow: %v", err)
	}
	f, _, err = openScriptNoFollow(dirFd, filepath.Base(path))
	_ = closeFd(dirFd)
	if err != nil {
		t.Fatalf("openScriptNoFollow: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, chain
}

func mustWriteExecutableScript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestOpenChainNoFollow_WalksToRoot verifies the chain returned by
// openChainNoFollow starts at "/" and ends at the target directory's own
// ownership, entirely from fstat results on already-open fds.
func TestOpenChainNoFollow_WalksToRoot(t *testing.T) {
	dir := t.TempDir()
	leaf := filepath.Join(dir, "pre-start.d")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, chain, err := openChainNoFollow(leaf)
	if err != nil {
		t.Fatalf("openChainNoFollow: %v", err)
	}
	defer func() { _ = closeFd(fd) }()

	if len(chain) < 2 {
		t.Fatalf("expected at least root + leaf in chain, got %d entries", len(chain))
	}
	// "/" is always uid 0 in any real filesystem.
	if chain[0].UID != 0 {
		t.Errorf("expected chain[0] (\"/\") to be uid 0, got %d", chain[0].UID)
	}
	// The leaf directory (under t.TempDir()) is owned by whoever is running
	// the test, not necessarily root.
	self := uint32(os.Getuid())
	if chain[len(chain)-1].UID != self {
		t.Errorf("expected chain leaf to be owned by the test's own uid %d, got %d", self, chain[len(chain)-1].UID)
	}
}

// TestOpenChainNoFollow_RefusesSymlinkedDir asserts a symlinked directory
// anywhere in the chain is refused outright — never silently followed, and
// never treated as "not existing" (which would let the hook silently not
// run instead of failing loudly).
func TestOpenChainNoFollow_RefusesSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "pre-start.d")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := openChainNoFollow(link); err == nil {
		t.Fatal("expected openChainNoFollow to refuse a symlinked directory component")
	}
}

// TestOpenScriptNoFollow_RefusesSymlinkedScript asserts a symlinked hook
// script is refused, never executed as either root or the workload, and that
// the refusal is ErrScriptRefused so runScriptHooks' skip-not-abort logic
// can recognize it.
func TestOpenScriptNoFollow_RefusesSymlinkedScript(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-script")
	mustWriteExecutableScript(t, real, "#!/bin/sh\nexit 0\n")
	link := filepath.Join(dir, "20-harness-provision")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	dirFd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer func() { _ = closeFd(dirFd) }()

	_, _, err = openScriptNoFollow(dirFd, "20-harness-provision")
	if err == nil {
		t.Fatal("expected openScriptNoFollow to refuse a symlinked script")
	}
	if !errors.Is(err, ErrScriptRefused) {
		t.Errorf("error = %v, want it to wrap ErrScriptRefused", err)
	}
}

// callWithTimeout runs fn in a goroutine and fails the test if it does not
// return within d, rather than letting a hang in fn (e.g. a regression back
// to a blocking open) hang the whole test binary. A regression is then a
// clean, fast test FAILURE instead of a CI timeout with no useful signal.
func callWithTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not return within %s — likely a regression to a blocking open", d)
	}
}

// TestOpenScriptNoFollow_RefusesWriterlessFIFOWithoutBlocking proves a
// FIFO nobody has open for writing is refused promptly rather than hanging
// hook processing: a plain O_RDONLY open(2) of such a FIFO blocks forever,
// which would hang all hook processing if a workload ever planted one under
// a hooks directory. openScriptNoFollow's O_NONBLOCK open plus the
// fstat-and-reject-non-regular check must refuse it instead — wrapped in
// ErrScriptRefused, exactly like a symlink — and must do so promptly. Run
// under callWithTimeout so a regression back to blocking behavior is a fast
// test failure, never a hung test binary.
func TestOpenScriptNoFollow_RefusesWriterlessFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "not-a-script")
	if err := unix.Mkfifo(fifoPath, 0o755); err != nil {
		t.Skipf("mkfifo unavailable in this environment: %v", err)
	}

	dirFd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer func() { _ = closeFd(dirFd) }()

	var gotErr error
	callWithTimeout(t, 5*time.Second, func() {
		_, _, gotErr = openScriptNoFollow(dirFd, "not-a-script")
	})
	if gotErr == nil {
		t.Fatal("expected openScriptNoFollow to refuse a writerless FIFO, got nil error")
	}
	if !errors.Is(gotErr, ErrScriptRefused) {
		t.Errorf("error = %v, want it to wrap ErrScriptRefused", gotErr)
	}
}

// TestOpenScriptNoFollow_RefusesUnixSocket covers the other non-regular type
// cheap to create in a test: a bound AF_UNIX socket special file. Unlike a
// FIFO, opening a socket special file via plain open(2) fails immediately
// (ENXIO) rather than blocking, but it must still be refused via
// ErrScriptRefused, not treated as a generic open error.
func TestOpenScriptNoFollow_RefusesUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "not-a-script")

	sockFd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("AF_UNIX socket unavailable: %v", err)
	}
	defer func() { _ = closeFd(sockFd) }()
	if err := unix.Bind(sockFd, &unix.SockaddrUnix{Name: sockPath}); err != nil {
		t.Skipf("bind unavailable in this environment: %v", err)
	}

	dirFd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer func() { _ = closeFd(dirFd) }()

	var gotErr error
	callWithTimeout(t, 5*time.Second, func() {
		_, _, gotErr = openScriptNoFollow(dirFd, "not-a-script")
	})
	if gotErr == nil {
		t.Fatal("expected openScriptNoFollow to refuse a socket special file, got nil error")
	}
	if !errors.Is(gotErr, ErrScriptRefused) {
		t.Errorf("error = %v, want it to wrap ErrScriptRefused", gotErr)
	}
}

// TestPrepareEnforcedExec_WorkloadWritableChainYieldsDropped feeds
// prepareEnforcedExec a script under a REAL, unprivileged filesystem
// fixture — t.TempDir(), which sits under the world-writable os.TempDir()
// ("/tmp", mode 1777 on every Linux system) — and asserts the DECISION it
// derives from real fstat results is "drop", not a fabricated NodeOwnership
// value. This is the direct, unprivileged proof that a workload-planted
// hook (the scenario this whole mechanism exists to close) is dropped: it
// needs no privilege at all, because dropping requires no privilege — only
// running the process as a genuinely different uid does.
func TestPrepareEnforcedExec_WorkloadWritableChainYieldsDropped(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "pre-start.d", "30-project-custom")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")

	prep, err := prepareEnforcedExec(script)
	if err != nil {
		t.Fatalf("prepareEnforcedExec: %v", err)
	}
	defer func() { _ = prep.file.Close() }()

	if !prep.executable {
		t.Error("expected the script to be reported executable")
	}
	if prep.asRoot {
		t.Error("expected a script under a world-writable ancestor (t.TempDir()/os.TempDir()) to be dropped, got asRoot=true")
	}
}

// skipUnlessFdExecSupported skips a test that runs a hook script through
// execViaFd on a platform where execViaFd refuses to build a command (any
// platform other than Linux; see UnsupportedPlatformError).
func skipUnlessFdExecSupported(t *testing.T) {
	t.Helper()
	if execViaFdGOOS != "linux" {
		t.Skipf("running a hook script by file descriptor is supported only on linux, not %s", execViaFdGOOS)
	}
}

// TestExecViaFd_RefusesNonLinux proves execViaFd fails closed on any
// platform other than Linux: it returns an *UnsupportedPlatformError naming
// the platform and no command, rather than exec'ing the script some other
// way.
func TestExecViaFd_RefusesNonLinux(t *testing.T) {
	orig := execViaFdGOOS
	execViaFdGOOS = "darwin"
	t.Cleanup(func() { execViaFdGOOS = orig })

	dir := t.TempDir()
	script := filepath.Join(dir, "hook")
	mustWriteExecutableScript(t, script, "#!/bin/sh\necho -n hello\n")
	f, err := os.Open(script)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	cmd, err := execViaFd(f, script)
	if cmd != nil {
		t.Errorf("execViaFd returned a command on darwin; want nil")
	}
	var upe *UnsupportedPlatformError
	if !errors.As(err, &upe) {
		t.Fatalf("execViaFd error = %v, want an *UnsupportedPlatformError", err)
	}
	if upe.GOOS != "darwin" {
		t.Errorf("UnsupportedPlatformError.GOOS = %q, want %q", upe.GOOS, "darwin")
	}

	m := &LifecycleManager{EnforcePrivilegeDrop: true, WorkloadUID: os.Getuid(), WorkloadGID: os.Getgid()}
	if _, err := m.buildEnforcedCmd(f, script, EventPostStart, true); !errors.As(err, &upe) {
		t.Fatalf("buildEnforcedCmd error = %v, want an *UnsupportedPlatformError", err)
	}
}

// TestExecViaFd_RunsShebangScript proves execViaFd's /proc/self/fd/<n> exec
// actually runs a shebang script end to end, unprivileged (the as-root
// branch — no SysProcAttr.Credential — needs no capability to run).
func TestExecViaFd_RunsShebangScript(t *testing.T) {
	skipUnlessFdExecSupported(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "hook")
	mustWriteExecutableScript(t, script, "#!/bin/sh\necho -n hello\n")

	f, err := os.Open(script)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	cmd, err := execViaFd(f, script)
	if err != nil {
		t.Fatalf("execViaFd(...): %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("execViaFd(...).Output(): %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
}

// TestExecViaFd_SwapAfterOpenRunsOriginalInode is the TOCTOU probe for
// execViaFd: it opens a script, then REPLACES the directory entry at
// that same path with a brand-new inode (remove, then create — not a
// truncate-in-place, which would rewrite the already-open fd's own
// content), and asserts execViaFd still runs the ORIGINAL content. This is
// what proves the file DecideExecAsRoot inspects (via the fd this test
// keeps open across the swap) is provably the file that runs, matching
// executeScriptEnforced's own no-TOCTOU construction.
func TestExecViaFd_SwapAfterOpenRunsOriginalInode(t *testing.T) {
	skipUnlessFdExecSupported(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "hook")
	mustWriteExecutableScript(t, script, "#!/bin/sh\necho -n original\n")

	f, err := os.Open(script)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	mustWriteExecutableScript(t, script, "#!/bin/sh\necho -n replaced\n")

	cmd, err := execViaFd(f, script)
	if err != nil {
		t.Fatalf("execViaFd(...): %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("execViaFd(...).Output(): %v", err)
	}
	if string(out) != "original" {
		t.Errorf("output = %q, want %q (the original inode, not the swapped-in replacement)", out, "original")
	}
}

// findPython3 locates python3 for the shebang-exec tests, skipping them
// (not failing) if it is not installed in this environment.
func findPython3(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found in PATH")
	}
	return path
}

// TestExecViaFd_RunsPython3ShebangScript is TestExecViaFd_RunsShebangScript's
// `#!/usr/bin/env python3` counterpart: the fd-3/ExtraFiles construction
// (execViaFd) must work identically regardless of which interpreter the
// kernel's binfmt_script handler re-execs — a `/bin/sh` shebang and a
// `/usr/bin/env python3` one go through two different re-exec paths (env(1)
// itself execs python3 as a second hop), so both are exercised explicitly.
func TestExecViaFd_RunsPython3ShebangScript(t *testing.T) {
	skipUnlessFdExecSupported(t)
	findPython3(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "hook")
	mustWriteExecutableScript(t, script, "#!/usr/bin/env python3\nimport sys\nsys.stdout.write('hello')\n")

	f, err := os.Open(script)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	cmd, err := execViaFd(f, script)
	if err != nil {
		t.Fatalf("execViaFd(...): %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("execViaFd(...).Output(): %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
}

// TestBuildEnforcedCmd_AsRootRunsShellAndPythonShebangs runs the actual
// as-root branch end to end (no SysProcAttr.Credential is set on this
// branch, so it needs no privilege) for both a shell and a python3 hook, at
// both a pre-start event (hookEnv, unhardened) and a post-workload event
// (hardenedRootHookEnv's allowlisted env and fixed PATH) — proving the
// fd-3 mechanics and the hardened/allowlisted environment do not, between
// them, break either interpreter's own shebang re-exec.
func TestBuildEnforcedCmd_AsRootRunsShellAndPythonShebangs(t *testing.T) {
	skipUnlessFdExecSupported(t)
	findPython3(t)
	dir := t.TempDir()

	scripts := map[string]string{
		"shell.sh": "#!/bin/sh\necho -n shell-ok\n",
		"py.py":    "#!/usr/bin/env python3\nimport sys\nsys.stdout.write('python-ok')\n",
	}
	want := map[string]string{"shell.sh": "shell-ok", "py.py": "python-ok"}

	for _, event := range []string{EventPreStart, EventPostStart} {
		for name, content := range scripts {
			script := filepath.Join(dir, event, name)
			mustWriteExecutableScript(t, script, content)
			f, chain := openScriptForTest(t, script)
			_ = chain

			m := &LifecycleManager{EnforcePrivilegeDrop: true, AgentHome: dir}
			cmd, err := m.buildEnforcedCmd(f, script, event, true)
			if err != nil {
				t.Fatalf("event %s script %s: buildEnforcedCmd: %v", event, name, err)
			}
			// buildEnforcedCmd already sets cmd.Stdout (to os.Stderr, so the
			// hook's own output surfaces in the caller's log); override it
			// here to capture output instead, since cmd.Output() refuses to
			// run when Stdout is already set.
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Run(); err != nil {
				t.Fatalf("event %s script %s: cmd.Run(): %v", event, name, err)
			}
			if out.String() != want[name] {
				t.Errorf("event %s script %s: output = %q, want %q", event, name, out.String(), want[name])
			}
		}
	}
}

// TestExecuteScriptEnforced_NonExecutableScriptIsSkipped proves the
// not-executable check runs on the already-open fd (fdIsExecutable) and
// results in a skip, not an error and not an exec attempt.
func TestExecuteScriptEnforced_NonExecutableScriptIsSkipped(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "pre-start.d", "not-executable")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &LifecycleManager{EnforcePrivilegeDrop: true, WorkloadUID: os.Getuid(), WorkloadGID: os.Getgid()}
	if err := m.executeScriptEnforced(script, EventPreStart); err != nil {
		t.Fatalf("executeScriptEnforced: %v, want nil (skip, not error, for a non-executable script)", err)
	}
}

// TestBuildEnforcedCmd_AsRoot verifies the "as root" branch at pre-start
// still runs a project/hub pre-start hook (any root-eligible script other
// than the harness-provision wrapper) via the calling process's own
// credentials (no Credential override), under the same hardened,
// allowlisted environment hardenedRootHookEnv builds for every other
// root-eligible event: HOME=/root, never AgentHome, with AgentHome exposed
// instead via SCION_AGENT_HOME so the hook can still find what was staged
// there. Dir is hardened to "/" here exactly as it is on the post-workload
// root branch (TestBuildEnforcedCmd_AsRootPostWorkloadEvent): PID 1's
// inherited cwd is the workload's own, workload-writable git workspace, not
// root's, whether or not the workload exists yet.
func TestBuildEnforcedCmd_AsRoot(t *testing.T) {
	skipUnlessFdExecSupported(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "30-project-custom")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
	f, _ := openScriptForTest(t, script)

	m := &LifecycleManager{EnforcePrivilegeDrop: true, AgentHome: "/home/scion"}
	cmd, err := m.buildEnforcedCmd(f, script, EventPreStart, true)
	if err != nil {
		t.Fatalf("buildEnforcedCmd: %v", err)
	}

	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
		t.Fatal("expected no Credential override for the as-root branch")
	}
	if cmd.Dir != "/" {
		t.Errorf("Dir = %q, want \"/\": never inherit init's own cwd, even at pre-start", cmd.Dir)
	}
	if got := findEnvVar(cmd.Env, "HOME"); got != "/root" {
		t.Errorf("HOME = %q, want /root (never the workload-owned AgentHome, even at pre-start)", got)
	}
	if got := findEnvVar(cmd.Env, "SCION_AGENT_HOME"); got != "/home/scion" {
		t.Errorf("SCION_AGENT_HOME = %q, want /home/scion (how a pre-start hook finds staged content without HOME pointing at it)", got)
	}
	if got := findEnvVar(cmd.Env, "PYTHONNOUSERSITE"); got != "1" {
		t.Errorf("PYTHONNOUSERSITE = %q, want \"1\"", got)
	}
	if got := findEnvVar(cmd.Env, "SCION_HOOK_PATH"); got != script {
		t.Errorf("SCION_HOOK_PATH = %q, want %q", got, script)
	}
}

// TestBuildEnforcedCmd_AsRootPreStartHardensPathAndStripsDangerousVars
// proves the pre-start-as-root branch never simply inherits this process's
// own environment: a workload-owned directory sitting ahead of the trusted
// system directories on this process's own inherited PATH (see the
// rootexec package doc comment) must never reach a root pre-start hook's
// own PATH-based lookups, and neither must an LD_PRELOAD/BASH_ENV-style
// variable, nor an inherited HOME.
func TestBuildEnforcedCmd_AsRootPreStartHardensPathAndStripsDangerousVars(t *testing.T) {
	skipUnlessFdExecSupported(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "30-project-custom")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
	f, _ := openScriptForTest(t, script)

	t.Setenv("PATH", "/workload-owned/bin:/usr/bin")
	t.Setenv("HOME", "/workload-owned/home")
	t.Setenv("LD_PRELOAD", "/workload-owned/evil.so")
	t.Setenv("LD_LIBRARY_PATH", "/workload-owned")
	t.Setenv("BASH_ENV", "/workload-owned/evil.sh")
	t.Setenv("ENV", "/workload-owned/evil.sh")

	m := &LifecycleManager{EnforcePrivilegeDrop: true, AgentHome: "/home/scion"}
	cmd, err := m.buildEnforcedCmd(f, script, EventPreStart, true)
	if err != nil {
		t.Fatalf("buildEnforcedCmd: %v", err)
	}

	wantPath := "PATH=" + strings.Join(rootexec.SearchPath, ":")
	gotPath := ""
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PATH=") {
			gotPath = kv
		}
	}
	if gotPath != wantPath {
		t.Errorf("PATH entry = %q, want %q (never the inherited, workload-influenceable value)", gotPath, wantPath)
	}
	for _, key := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "ENV"} {
		if got := findEnvVar(cmd.Env, key); got != "" {
			t.Errorf("%s = %q, want stripped entirely", key, got)
		}
	}
	if got := findEnvVar(cmd.Env, "HOME"); got != "/root" {
		t.Errorf("HOME = %q, want /root, never the inherited workload-owned value", got)
	}
}

// TestBuildEnforcedCmd_HarnessProvisionRunsDroppedNotRoot proves the one
// carve-out in the as-root pre-start branch: a script named exactly
// harness.HarnessProvisionHookFilename is still opened via the same
// fd-anchored, root-owned-chain-verified path every other asRoot script
// uses (this test's own f came from that same helper), but the command
// buildEnforcedCmd hands back for it runs under the workload's own uid/gid,
// with supplementary groups cleared, never as root — because what that
// wrapper execs (`sciontool harness provision`, and the harness's own
// provisioner script) reads and writes $HOME and /workspace, both fully
// workload-controlled, unlike a project/hub hook's own root-eligible use of
// pre-start (TestBuildEnforcedCmd_AsRoot above). This test would fail if the
// carve-out were removed (the wrapper would run with no Credential, i.e. as
// root) or if the name check were inverted (a project/hub hook would then be
// dropped instead of the provisioner).
func TestBuildEnforcedCmd_HarnessProvisionRunsDroppedNotRoot(t *testing.T) {
	skipUnlessFdExecSupported(t)
	dir := t.TempDir()
	script := filepath.Join(dir, harness.HarnessProvisionHookFilename)
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
	f, _ := openScriptForTest(t, script)

	m := &LifecycleManager{
		EnforcePrivilegeDrop: true,
		AgentHome:            "/home/scion",
		WorkloadUID:          1000,
		WorkloadGID:          1000,
		WorkloadUsername:     "scion",
	}
	// asRoot=true: DecideExecAsRoot's own classification for this script,
	// exactly as it would be for the real, root-owned staged wrapper. The
	// carve-out applies to that classification's result, not instead of it.
	cmd, err := m.buildEnforcedCmd(f, script, EventPreStart, true)
	if err != nil {
		t.Fatalf("buildEnforcedCmd: %v", err)
	}

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("expected a Credential override dropping the harness-provision wrapper off root")
	}
	cred := cmd.SysProcAttr.Credential
	if cred.Uid != 1000 || cred.Gid != 1000 {
		t.Errorf("Credential = %+v, want uid=gid=1000", cred)
	}
	if cred.Groups == nil || len(cred.Groups) != 0 {
		t.Errorf("Credential.Groups = %v, want an empty (not nil) slice — supplementary groups cleared explicitly", cred.Groups)
	}
	if got := findEnvVar(cmd.Env, "HOME"); got != "/home/scion" {
		t.Errorf("HOME = %q, want /home/scion (the provisioner still needs its own agent home)", got)
	}
	if got := findEnvVar(cmd.Env, "PYTHONNOUSERSITE"); got != "1" {
		t.Errorf("PYTHONNOUSERSITE = %q, want \"1\"", got)
	}
	if got := findEnvVar(cmd.Env, "SCION_HOOK_PATH"); got != script {
		t.Errorf("SCION_HOOK_PATH = %q, want %q", got, script)
	}
}

// TestBuildEnforcedCmd_HarnessProvisionFailsClosedWithoutWorkloadUID proves
// the carve-out above never falls back to running the provisioner as root
// when no valid workload uid/gid is on hand to drop to — it refuses to
// build a runnable command at all. This test would fail if that guard were
// removed (buildEnforcedCmd would instead hand back a runnable root
// command, uid 0, for the zero-value WorkloadUID/WorkloadGID below).
func TestBuildEnforcedCmd_HarnessProvisionFailsClosedWithoutWorkloadUID(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, harness.HarnessProvisionHookFilename)
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
	f, _ := openScriptForTest(t, script)

	m := &LifecycleManager{EnforcePrivilegeDrop: true, AgentHome: "/home/scion"}
	if _, err := m.buildEnforcedCmd(f, script, EventPreStart, true); err == nil {
		t.Fatal("expected an error refusing to run the harness-provision wrapper with no valid workload uid, got nil")
	}
}

// TestHarnessProvisionHookFilenameMatchesWriter proves this package's own
// harnessProvisionHookFilename constant — duplicated rather than imported;
// see its own doc comment for why — never drifts from the name
// pkg/harness.ContainerScriptHarness actually stages the wrapper under. A
// silent mismatch here would reopen the exact hole the carve-out above
// exists to close: DecideExecAsRoot would still classify the real, staged
// wrapper asRoot, but buildEnforcedCmd's name check would no longer match
// it, so it would fall straight through to running fully as root again.
func TestHarnessProvisionHookFilenameMatchesWriter(t *testing.T) {
	if harnessProvisionHookFilename != harness.HarnessProvisionHookFilename {
		t.Fatalf("harnessProvisionHookFilename = %q, pkg/harness.HarnessProvisionHookFilename = %q; these must stay equal",
			harnessProvisionHookFilename, harness.HarnessProvisionHookFilename)
	}
}

// TestBuildEnforcedCmd_AsRootPostWorkloadEvent is the hardening every
// root-eligible hook needs, pre-start included: it must never run with HOME
// pointed at the workload's own home directory, and it must never run with
// init's own cwd, which — since nothing in sciontool ever chdirs — is
// whatever the image sets (e.g. a Dockerfile WORKDIR), the workload's own
// writable git workspace. Either one left unguarded lets a root-run
// python/bash/git/pip/node hook load workload-planted content and execute
// it as root, the same escalation class DecideExecAsRoot exists to close.
func TestBuildEnforcedCmd_AsRootPostWorkloadEvent(t *testing.T) {
	skipUnlessFdExecSupported(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "10-post-start")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
	f, _ := openScriptForTest(t, script)

	m := &LifecycleManager{EnforcePrivilegeDrop: true, AgentHome: "/home/scion"}
	for _, event := range []string{EventPreStart, EventPostStart, EventPreStop, EventSessionEnd} {
		cmd, err := m.buildEnforcedCmd(f, script, event, true)
		if err != nil {
			t.Fatalf("event %s: buildEnforcedCmd: %v", event, err)
		}
		if got := findEnvVar(cmd.Env, "HOME"); got != "/root" {
			t.Errorf("event %s: HOME = %q, want /root (never the workload-owned home)", event, got)
		}
		if got := findEnvVar(cmd.Env, "PYTHONNOUSERSITE"); got != "1" {
			t.Errorf("event %s: PYTHONNOUSERSITE = %q, want \"1\"", event, got)
		}
		if got := findEnvVar(cmd.Env, "PATH"); got != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
			t.Errorf("event %s: PATH = %q, want the fixed, minimal PATH", event, got)
		}
		if cmd.Dir != "/" {
			t.Errorf("event %s: Dir = %q, want \"/\" (never init's own cwd, which is the workload-writable image WORKDIR)", event, cmd.Dir)
		}
	}
}

// TestHardenedRootHookEnv_DropsInterpreterAndLoaderRedirectors proves the
// hardened root env is built from an allowlist, not inherited wholesale:
// even when init's own process environment carries interpreter/loader
// redirector variables (which substrate-serve's bootstrap can set from
// harness/operator/auth env via os.Setenv), none of them reach a root hook
// at a post-pre-start event. HOME/PATH/PYTHONNOUSERSITE alone would not
// stop a tool that reads one of these directly instead of resolving through
// HOME or PATH.
func TestHardenedRootHookEnv_DropsInterpreterAndLoaderRedirectors(t *testing.T) {
	skipUnlessFdExecSupported(t)
	redirectors := map[string]string{
		"PYTHONPATH":        "/home/scion/lib",
		"PYTHONSTARTUP":     "/home/scion/.pythonrc",
		"BASH_ENV":          "/home/scion/.bashenv",
		"ENV":               "/home/scion/.shrc",
		"LD_PRELOAD":        "/home/scion/evil.so",
		"LD_LIBRARY_PATH":   "/home/scion/lib",
		"NODE_OPTIONS":      "--require /home/scion/evil.js",
		"NODE_PATH":         "/home/scion/node_modules",
		"PERL5LIB":          "/home/scion/perl5",
		"PERL5OPT":          "-Mevil",
		"RUBYLIB":           "/home/scion/ruby",
		"RUBYOPT":           "-revil",
		"XDG_CONFIG_HOME":   "/home/scion/.config",
		"GIT_CONFIG_GLOBAL": "/home/scion/.gitconfig",
	}
	for key, value := range redirectors {
		t.Setenv(key, value)
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "10-post-start")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
	f, _ := openScriptForTest(t, script)

	m := &LifecycleManager{EnforcePrivilegeDrop: true, AgentHome: "/home/scion"}
	cmd, err := m.buildEnforcedCmd(f, script, EventPostStart, true)
	if err != nil {
		t.Fatalf("buildEnforcedCmd: %v", err)
	}

	for key := range redirectors {
		if got := findEnvVar(cmd.Env, key); got != "" {
			t.Errorf("%s = %q, want absent from a hardened root hook's environment", key, got)
		}
	}
	// Control: the hardening's own overrides must still be present.
	if got := findEnvVar(cmd.Env, "HOME"); got != "/root" {
		t.Errorf("HOME = %q, want /root", got)
	}
}

// TestHardenedRootHookEnv_EnvIsExactlyAllowlistPlusOverrides proves the
// asRoot post-workload environment is a closed set: nothing beyond the
// explicit overrides this hardening itself sets (HOME, PATH,
// PYTHONNOUSERSITE, PYTHONDONTWRITEBYTECODE, SCION_HOOK_PATH) and the exact
// names in rootHookEnvAllowlist. It seeds the inherited process environment
// with allowlisted vars (LANG, TERM), a var that LOOKS safe but is
// deliberately not allowlisted (TZ), a non-allowlisted SCION_* var (no hook
// consumes any SCION_* value, so none is allowlisted by name or by a
// blanket prefix — see rootHookEnvAllowlist's own doc comment), and a
// redirector (PYTHONPATH), and asserts the first two are the ONLY ones that
// survive.
func TestHardenedRootHookEnv_EnvIsExactlyAllowlistPlusOverrides(t *testing.T) {
	skipUnlessFdExecSupported(t)
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TZ", "UTC")
	t.Setenv("SCION_RUNTIME", "test-runtime")
	t.Setenv("PYTHONPATH", "/home/scion/lib")

	dir := t.TempDir()
	script := filepath.Join(dir, "10-post-start")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
	f, _ := openScriptForTest(t, script)

	m := &LifecycleManager{EnforcePrivilegeDrop: true}
	cmd, err := m.buildEnforcedCmd(f, script, EventPostStart, true)
	if err != nil {
		t.Fatalf("buildEnforcedCmd: %v", err)
	}

	allowed := map[string]bool{
		"HOME":                    true,
		"PATH":                    true,
		"PYTHONNOUSERSITE":        true,
		"PYTHONDONTWRITEBYTECODE": true,
		"SCION_HOOK_PATH":         true,
		"SCION_AGENT_HOME":        true,
	}
	for name := range rootHookEnvAllowlist {
		allowed[name] = true
	}
	for _, e := range cmd.Env {
		key, _, _ := strings.Cut(e, "=")
		if !allowed[key] {
			t.Errorf("unexpected env var %q reached a hardened root hook's environment", key)
		}
	}

	if got := findEnvVar(cmd.Env, "LANG"); got != "en_US.UTF-8" {
		t.Errorf("LANG = %q, want %q", got, "en_US.UTF-8")
	}
	if got := findEnvVar(cmd.Env, "TERM"); got != "xterm-256color" {
		t.Errorf("TERM = %q, want %q", got, "xterm-256color")
	}
	if got := findEnvVar(cmd.Env, "TZ"); got != "" {
		t.Errorf("TZ = %q, want absent (looks safe but is not allowlisted)", got)
	}
	if got := findEnvVar(cmd.Env, "SCION_RUNTIME"); got != "" {
		t.Errorf("SCION_RUNTIME = %q, want absent (no hook consumes it, so no SCION_* var is allowlisted by name or prefix)", got)
	}
}

// TestBuildEnforcedCmd_Dropped verifies the dropped branch sets Credential
// to WorkloadUID/GID, rewrites USER/LOGNAME/HOME to match the harness's own
// dropped identity, and sets Dir to WorkloadWorkingDir — "env and cwd
// handled the same way as the harness process".
func TestBuildEnforcedCmd_Dropped(t *testing.T) {
	skipUnlessFdExecSupported(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "session-end")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
	f, _ := openScriptForTest(t, script)

	m := &LifecycleManager{
		EnforcePrivilegeDrop: true,
		AgentHome:            "/home/scion",
		WorkloadUID:          1000,
		WorkloadGID:          1000,
		WorkloadUsername:     "scion",
		WorkloadWorkingDir:   "/workspace",
	}
	cmd, err := m.buildEnforcedCmd(f, script, EventSessionEnd, false)
	if err != nil {
		t.Fatalf("buildEnforcedCmd: %v", err)
	}

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("expected a Credential override for the dropped branch")
	}
	if cmd.SysProcAttr.Credential.Uid != 1000 || cmd.SysProcAttr.Credential.Gid != 1000 {
		t.Errorf("Credential = %+v, want uid=gid=1000", cmd.SysProcAttr.Credential)
	}
	if cmd.Dir != "/workspace" {
		t.Errorf("Dir = %q, want /workspace", cmd.Dir)
	}
	for _, tc := range []struct{ key, want string }{
		{"HOME", "/home/scion"},
		{"USER", "scion"},
		{"LOGNAME", "scion"},
		{"SCION_HOOK_PATH", script},
	} {
		if got := findEnvVar(cmd.Env, tc.key); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestBuildEnforcedCmd_DroppedFailsClosedWithoutWorkloadUIDOrGID proves the
// dropped (!asRoot) branch never builds a runnable *exec.Cmd — and so never
// execs anything — when EnforcePrivilegeDrop is set but WorkloadUID or
// WorkloadGID is <= 0: mirroring buildDroppedProvisionCmd's own fail-closed
// guard, a future caller that constructs a LifecycleManager with
// EnforcePrivilegeDrop set but never resolves a workload identity must
// refuse rather than fall through to a zero-value Credential, which the
// kernel would treat as uid/gid 0 (root).
func TestBuildEnforcedCmd_DroppedFailsClosedWithoutWorkloadUIDOrGID(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "session-end")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")

	cases := []struct {
		name     string
		uid, gid int
	}{
		{name: "uid0", uid: 0, gid: 1000},
		{name: "gid0", uid: 1000, gid: 0},
		{name: "both0", uid: 0, gid: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := openScriptForTest(t, script)
			m := &LifecycleManager{
				EnforcePrivilegeDrop: true,
				AgentHome:            "/home/scion",
				WorkloadUID:          tc.uid,
				WorkloadGID:          tc.gid,
			}
			cmd, err := m.buildEnforcedCmd(f, script, EventSessionEnd, false)
			if err == nil {
				t.Fatal("expected an error refusing to build the dropped command with no valid workload uid/gid, got nil")
			}
			if cmd != nil {
				t.Errorf("expected a nil *exec.Cmd on failure, got %+v — nothing must be left runnable", cmd)
			}
		})
	}
}

// TestExecuteScriptEnforced_DroppedFailsClosedWithoutWorkloadUIDOrGID is
// TestBuildEnforcedCmd_DroppedFailsClosedWithoutWorkloadUIDOrGID's end-to-end
// counterpart: it drives the full executeScriptEnforced entry point (the
// only caller of buildEnforcedCmd in production) against a script that is
// NOT root-eligible (self-owned, world-writable temp dir, so DecideExecAsRoot
// classifies it dropped), with EnforcePrivilegeDrop set but no workload
// identity resolved. The marker file's absence proves the script was never
// executed — the guard fires before cmd.Run() is ever reached, so this needs
// no CAP_SETGID/root to verify.
func TestExecuteScriptEnforced_DroppedFailsClosedWithoutWorkloadUIDOrGID(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "session-end")
	mustWriteExecutableScript(t, script, "#!/bin/sh\ntouch "+marker+"\n")

	m := &LifecycleManager{EnforcePrivilegeDrop: true, AgentHome: "/home/scion"}
	err := m.executeScript(script, EventSessionEnd)
	if err == nil {
		t.Fatal("expected executeScript to refuse with no valid workload uid/gid, got nil error")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("the script ran (marker exists, stat err=%v); it must not run without a resolved workload identity in enforced mode", statErr)
	}
}

// TestExecuteScriptEnforced_WorkloadOwnedRunsDropped is an end-to-end
// exercise of the dropped path. It needs CAP_SETGID (root) because Go's
// os/exec calls setgroups(2) whenever SysProcAttr.Credential is set
// (matching supervisor.Supervisor.Run's own Credential shape — no Groups, no
// NoSetGroups override — see buildEnforcedCmd), and setgroups(2) requires
// that capability unconditionally, even to drop to the calling process's own
// current uid/gid.
//
// When it runs (euid == 0), it drops to a genuinely different, non-zero uid
// (65534, traditionally "nobody") rather than self-dropping to the test's
// own uid: euid 0 self-dropping to WorkloadUID = os.Getuid() = 0 would make
// the "it ran as the workload uid" assertion pass whether or not the drop
// actually happened, since 0 == 0 either way. Using 65534 makes the marker
// file's content only match if the credential switch was real.
// TestPrepareEnforcedExec_WorkloadWritableChainYieldsDropped above is the
// unprivileged equivalent for the decision itself.
func TestExecuteScriptEnforced_WorkloadOwnedRunsDropped(t *testing.T) {
	skipUnlessFdExecSupported(t)
	if os.Geteuid() != 0 {
		t.Skip("requires CAP_SETGID (root) to exercise the real setgroups(2)+exec path; see TestPrepareEnforcedExec_WorkloadWritableChainYieldsDropped and TestBuildEnforcedCmd_Dropped for the unprivileged-safe equivalents")
	}
	const dropUID, dropGID = 65534, 65534
	dir := t.TempDir()
	// World-writable so uid 65534 -- distinct from the real root this test
	// runs as -- can create the marker file after the drop.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	script := filepath.Join(dir, "pre-start.d", "30-project-custom")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nid -u > "+marker+"\n")

	m := &LifecycleManager{
		EnforcePrivilegeDrop: true,
		WorkloadUID:          dropUID,
		WorkloadGID:          dropGID,
		WorkloadUsername:     "nobody",
	}
	if err := m.executeScriptEnforced(script, EventPreStart); err != nil {
		t.Fatalf("executeScriptEnforced: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	wantUID := []byte(itoa(dropUID) + "\n")
	if string(got) != string(wantUID) {
		t.Errorf("hook ran as uid %q, want %q (the distinct workload uid, proving the drop actually happened)", got, wantUID)
	}
}

// TestExecuteScriptEnforced_WorkloadOwnedRunsDropped_PythonShebang is
// TestExecuteScriptEnforced_WorkloadOwnedRunsDropped's `#!/usr/bin/env
// python3` counterpart, proving the fd-3 mechanics survive the dropped
// branch's own real setuid/setgid exec for the two-hop env(1)->python3
// re-exec, not just a shell script. Same root/CAP_SETGID gate as that test.
func TestExecuteScriptEnforced_WorkloadOwnedRunsDropped_PythonShebang(t *testing.T) {
	skipUnlessFdExecSupported(t)
	if os.Geteuid() != 0 {
		t.Skip("requires CAP_SETGID (root) to exercise the real setgroups(2)+exec path; see TestExecViaFd_RunsPython3ShebangScript for the unprivileged-safe equivalent")
	}
	findPython3(t)
	const dropUID, dropGID = 65534, 65534
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	script := filepath.Join(dir, "pre-start.d", "30-project-custom")
	mustWriteExecutableScript(t, script,
		"#!/usr/bin/env python3\nimport os\nwith open("+`"`+marker+`"`+", 'w') as f:\n    f.write(str(os.getuid()))\n")

	m := &LifecycleManager{
		EnforcePrivilegeDrop: true,
		WorkloadUID:          dropUID,
		WorkloadGID:          dropGID,
		WorkloadUsername:     "nobody",
	}
	if err := m.executeScriptEnforced(script, EventPreStart); err != nil {
		t.Fatalf("executeScriptEnforced: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(got) != itoa(dropUID) {
		t.Errorf("hook ran as uid %q, want %q (the distinct workload uid, proving the drop actually happened)", got, itoa(dropUID))
	}
}

// TestExecuteScriptEnforced_RootOwnedChainRunsAsRoot is the real-exec
// counterpart of TestBuildEnforcedCmd_AsRoot: it requires an actual
// root-owned, non-writable directory chain outside of any world-writable
// temp dir (t.TempDir() will never do — see
// TestPrepareEnforcedExec_WorkloadWritableChainYieldsDropped's own doc
// comment on why /tmp itself always fails the chain check), which in turn
// requires both root and write access to a location under "/" — this
// container's own "/" is root-owned 0755, so an unprivileged test cannot
// create anything there at all. Skipped unless running as root; when
// skipped, DecideExecAsRoot's own table tests (privilege_test.go) already
// prove the decision logic this real exec depends on, without needing a
// real filesystem at all.
//
// Uses a project/hub hook name (30-project-custom), not the harness-provision
// wrapper: buildEnforcedCmd's carve-out runs that one root-eligible
// pre-start script dropped — see
// TestExecuteScriptEnforced_HarnessProvisionHookRunsDroppedNotRoot
// immediately below for its own real-exec proof.
func TestExecuteScriptEnforced_RootOwnedChainRunsAsRoot(t *testing.T) {
	skipUnlessFdExecSupported(t)
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a root-owned, non-world-writable directory outside /tmp; DecideExecAsRoot's table tests already cover the decision itself")
	}
	dir, err := os.MkdirTemp("/root", "hooks-enforced-test-*")
	if err != nil {
		t.Skipf("could not create a root-owned fixture under /root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(dir, "marker")
	script := filepath.Join(dir, "pre-start.d", "30-project-custom")
	mustWriteExecutableScript(t, script, "#!/bin/sh\nid -u > "+marker+"\n")
	if err := os.Chmod(filepath.Join(dir, "pre-start.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	m := &LifecycleManager{EnforcePrivilegeDrop: true, WorkloadUID: 1000, WorkloadGID: 1000}
	if err := m.executeScriptEnforced(script, EventPreStart); err != nil {
		t.Fatalf("executeScriptEnforced: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(got) != "0\n" {
		t.Errorf("hook ran as uid %q, want \"0\\n\" (root)", got)
	}
}

// TestExecuteScriptEnforced_HarnessProvisionHookRunsDroppedNotRoot is
// TestExecuteScriptEnforced_RootOwnedChainRunsAsRoot's counterpart for the
// one root-eligible pre-start script buildEnforcedCmd's carve-out drops: the same
// root-owned, non-writable directory chain (proving DecideExecAsRoot still
// classifies the genuine, root-owned wrapper asRoot regardless of the
// fd-anchored open), but the script is named exactly
// harness.HarnessProvisionHookFilename and the marker it writes must show
// the workload uid, never 0. This would fail if the carve-out in
// buildEnforcedCmd were removed or its name check broken.
func TestExecuteScriptEnforced_HarnessProvisionHookRunsDroppedNotRoot(t *testing.T) {
	skipUnlessFdExecSupported(t)
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a root-owned, non-world-writable directory outside /tmp; TestBuildEnforcedCmd_HarnessProvisionRunsDroppedNotRoot is the unprivileged-safe equivalent")
	}
	const dropUID, dropGID = 65534, 65534
	dir, err := os.MkdirTemp("/root", "hooks-enforced-test-*")
	if err != nil {
		t.Skipf("could not create a root-owned fixture under /root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The marker lives in a SEPARATE directory, never on the script's own
	// chain: chmoding dir itself (or pre-start.d) world-writable to let
	// dropUID write the marker would make rootProtected's own group/other-
	// write check fail on a chain component, so DecideExecAsRoot would
	// classify this script dropped on chain grounds alone — masking whether
	// the carve-out this test exists to prove (a name-based drop of a
	// script DecideExecAsRoot would otherwise still classify asRoot) is
	// actually what caused the drop. dir and pre-start.d stay root-owned,
	// 0755, for exactly that reason.
	markerDir := t.TempDir()
	if err := os.Chmod(markerDir, 0o777); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(markerDir, "marker")
	script := filepath.Join(dir, "pre-start.d", harness.HarnessProvisionHookFilename)
	mustWriteExecutableScript(t, script, "#!/bin/sh\nid -u > "+marker+"\n")
	if err := os.Chmod(filepath.Join(dir, "pre-start.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	m := &LifecycleManager{EnforcePrivilegeDrop: true, WorkloadUID: dropUID, WorkloadGID: dropGID, WorkloadUsername: "nobody"}
	if err := m.executeScriptEnforced(script, EventPreStart); err != nil {
		t.Fatalf("executeScriptEnforced: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	wantUID := []byte(itoa(dropUID) + "\n")
	if string(got) != string(wantUID) {
		t.Errorf("hook ran as uid %q, want %q (the distinct workload uid, never root, proving the drop actually happened)", got, wantUID)
	}
}

// TestExecuteScriptEnforced_NonEnforcedModeUnchanged verifies executeScript
// runs every hook script unconditionally when EnforcePrivilegeDrop is false
// — including for a script that the enforced decision would drop (a
// world-writable ancestor under t.TempDir()).
func TestExecuteScriptEnforced_NonEnforcedModeUnchanged(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	script := filepath.Join(dir, "pre-start.d", "30-project-custom")
	mustWriteExecutableScript(t, script, "#!/bin/sh\necho -n ran >> "+marker+"\n")

	m := &LifecycleManager{HooksDirs: []string{dir}, Handlers: map[string][]Handler{}}
	if err := m.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(got) != "ran" {
		t.Fatalf("expected the non-enforced exec path to run the script unconditionally, got %q", got)
	}
}

// TestExecuteScript_NonEnforcedPreStartEnvUnchangedByHardening proves a
// non-enforced LifecycleManager (EnforcePrivilegeDrop false, the zero
// value) runs a pre-start hook via the unfiltered m.hookEnv(), never
// hardenedRootHookEnv's allowlist: variables hardenedRootHookEnv strips in
// enforced mode (NODE_OPTIONS, GIT_CONFIG_GLOBAL) still reach the script
// unchanged here, and HOME is the workload-owned AgentHome, not /root.
func TestExecuteScript_NonEnforcedPreStartEnvUnchangedByHardening(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--require /home/scion/evil.js")
	t.Setenv("GIT_CONFIG_GLOBAL", "/home/scion/.gitconfig")
	t.Setenv("HOME", "/should-not-be-used")

	dir := t.TempDir()
	agentHome := t.TempDir()
	out := filepath.Join(dir, "out")
	script := filepath.Join(dir, "pre-start.d", "30-project-custom")
	mustWriteExecutableScript(t, script,
		"#!/bin/sh\nprintenv NODE_OPTIONS > "+out+"\nprintenv GIT_CONFIG_GLOBAL >> "+out+"\nprintenv HOME >> "+out+"\n")

	m := &LifecycleManager{
		HooksDirs: []string{dir},
		Handlers:  map[string][]Handler{},
		AgentHome: agentHome,
		// EnforcePrivilegeDrop left at its zero value (false).
	}
	if err := m.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	want := "--require /home/scion/evil.js\n/home/scion/.gitconfig\n" + agentHome + "\n"
	if string(got) != want {
		t.Errorf("non-enforced pre-start env = %q, want %q (unfiltered hookEnv, AgentHome-owned HOME, never the enforced-mode allowlist)", got, want)
	}
}

// TestRunPreStart_EnforcedModeSkipsRefusedWorkloadEntryButRunsSiblings
// proves the skip-not-abort behavior: a workload-planted symlink under a
// non-EnforcedHooksDir hooks directory must not DoS the event's other,
// legitimate hooks.
func TestRunPreStart_EnforcedModeSkipsRefusedWorkloadEntryButRunsSiblings(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	good := filepath.Join(dir, "pre-start.d", "10-good")
	mustWriteExecutableScript(t, good, "#!/bin/sh\necho -n ran >> "+marker+"\n")
	bad := filepath.Join(dir, "pre-start.d", "05-bad-symlink")
	if err := os.Symlink("/nonexistent", bad); err != nil {
		t.Fatal(err)
	}

	m := &LifecycleManager{
		EnforcePrivilegeDrop: true,
		HooksDirs:            []string{dir},
		Handlers:             map[string][]Handler{},
		WorkloadUID:          os.Getuid(),
		WorkloadGID:          os.Getgid(),
	}
	err := m.RunPreStart()

	// The refused symlink must never abort iteration, regardless of what
	// happens to 10-good next. When this test runs as root, 10-good's own
	// drop-to-self-uid exec fully succeeds and err is nil. When it does not
	// (this container, and CI in general), 10-good's exec still fails, but
	// for an unrelated reason — dropping needs CAP_SETGID even to the
	// calling process's own current uid/gid (see
	// TestExecuteScriptEnforced_WorkloadOwnedRunsDropped's doc comment) — so
	// the resulting error mentions 10-good, never 05-bad-symlink. Asserting
	// on which script the error names, rather than requiring err == nil,
	// makes this test prove the skip-not-abort property in both
	// environments instead of only under root.
	if err == nil {
		got, readErr := os.ReadFile(marker)
		if readErr != nil {
			t.Fatalf("expected the sibling hook to have run: %v", readErr)
		}
		if string(got) != "ran" {
			t.Errorf("marker = %q, want %q", got, "ran")
		}
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "05-bad-symlink") {
		t.Fatalf("RunPreStart error mentions the refused symlink; it must be skipped, not aborted on: %v", err)
	}
	if !strings.Contains(msg, "10-good") {
		t.Fatalf("RunPreStart error does not mention the sibling hook; iteration did not reach it: %v", err)
	}
}

// TestRunPreStart_EnforcedModeHardFailsRefusedEntryUnderEnforcedHooksDir
// proves the exception: a refused entry under EnforcedHooksDir itself (the
// root-owned, broker-delivered directory, never workload-writable) still
// hard-fails the event instead of being silently skipped — an anomaly there
// is worth aborting over, not routine workload nuisance. The directory is
// matched after cleaning both paths, so naming it with a trailing slash —
// in HooksDirs or in EnforcedHooksDir — still hard-fails.
func TestRunPreStart_EnforcedModeHardFailsRefusedEntryUnderEnforcedHooksDir(t *testing.T) {
	tests := []struct {
		name              string
		hooksDirSuffix    string
		enforcedDirSuffix string
	}{
		{name: "identical paths"},
		{name: "hooks dir with trailing slash", hooksDirSuffix: "/"},
		{name: "enforced hooks dir with trailing slash", enforcedDirSuffix: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			orig := EnforcedHooksDir
			EnforcedHooksDir = dir + tt.enforcedDirSuffix
			t.Cleanup(func() { EnforcedHooksDir = orig })

			if err := os.MkdirAll(filepath.Join(dir, "pre-start.d"), 0o755); err != nil {
				t.Fatal(err)
			}
			bad := filepath.Join(dir, "pre-start.d", "05-bad-symlink")
			if err := os.Symlink("/nonexistent", bad); err != nil {
				t.Fatal(err)
			}

			m := &LifecycleManager{
				EnforcePrivilegeDrop: true,
				HooksDirs:            []string{dir + tt.hooksDirSuffix},
				Handlers:             map[string][]Handler{},
				WorkloadUID:          os.Getuid(),
				WorkloadGID:          os.Getgid(),
			}
			if err := m.RunPreStart(); err == nil {
				t.Fatal("expected RunPreStart to hard-fail on a refused entry under the (test's stand-in for the) enforced hooks dir")
			}
		})
	}
}

func findEnvVar(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return e[len(prefix):]
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

var startHooksProcreapReaperOnce sync.Once

// TestExecuteScriptEnforced_AsRootRunsUnderActiveReaperWithoutECHILD is the
// regression test for the property that executeScriptEnforced's root-branch
// exec must go through procreap's managed exec API, not a raw cmd.Run(),
// because sciontool init's real PID-1 reaper (procreap.StartReaper) is
// active for the whole lifetime of every enforced hook script it runs in
// production. A raw cmd.Run() call here is exactly the shape of bug
// TestManagedService_StartSurvivesReaperRace (pkg/sciontool/services) and
// TestConfigureSharedWorkspaceGit_RunsUnderActiveReaperWithoutECHILD
// (cmd/sciontool/commands) guard against elsewhere: the reaper's generic
// wait4(-1, ...) can steal a hook script's exit status from cmd.Wait()
// before Run() gets to it, surfacing as an ECHILD-shaped "wait: no child
// processes" error.
//
// This drives buildEnforcedCmd with asRoot passed explicitly (the same
// unprivileged-safe construction TestBuildEnforcedCmd_AsRoot* uses above),
// then runEnforcedCmd — the exact tail executeScriptEnforced itself calls —
// rather than executeScriptEnforced end to end, because the dropped branch's
// real setgroups(2) call requires CAP_SETGID (see
// TestExecuteScriptEnforced_WorkloadOwnedRunsDropped); the root branch under
// test here needs no privilege at all.
//
// A real, live procreap reaper must run for this to be a faithful
// reproduction — a fake or absent reaper can't race anything (same
// requirement TestConfigureSharedWorkspaceGit_RunsUnderActiveReaperWithoutECHILD
// documents).
//
// Positive control: this test is not vacuously green, but the failure is
// probabilistic, not deterministic — reverting runEnforcedCmd's call back
// to a raw cmd.Run() makes it fail only intermittently (empirically
// clustered in the first iteration right after the reaper starts; later
// iterations in the same process rarely hit the race), so a single `go
// test -race -count=5` run reverted is not guaranteed to show red. The
// deterministic guard for this same revert is the static audit
// cmd/sciontool/commands/execaudit_test.go's TestNoRawExecInPID1Path, which
// fails every time on either this file's raw cmd.Run() or a raw exec call
// anywhere else in the PID-1 path — this test exists alongside it to prove
// the managed call is also race-free in practice, not to replace it as the
// authoritative regression guard.
func TestExecuteScriptEnforced_AsRootRunsUnderActiveReaperWithoutECHILD(t *testing.T) {
	skipUnlessFdExecSupported(t)
	startHooksProcreapReaperOnce.Do(procreap.StartReaper)

	const iterations = 50
	// Pre-create every fixture AND open every script file serially:
	// t.TempDir, t.Fatal and t.Cleanup (all used by mustWriteExecutableScript
	// and openScriptForTest) are not safe to call from multiple goroutines,
	// so none of the concurrent work below may call either helper.
	markers := make([]string, iterations)
	scripts := make([]string, iterations)
	files := make([]*os.File, iterations)
	for i := range markers {
		dir := t.TempDir()
		markers[i] = filepath.Join(dir, "marker")
		scripts[i] = filepath.Join(dir, fmt.Sprintf("post-start-%d", i))
		mustWriteExecutableScript(t, scripts[i], "#!/bin/sh\necho ran > "+markers[i]+"\n")
		files[i], _ = openScriptForTest(t, scripts[i])
	}

	m := &LifecycleManager{EnforcePrivilegeDrop: true}

	var wg sync.WaitGroup
	errs := make([]error, iterations)
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd, err := m.buildEnforcedCmd(files[i], scripts[i], EventPostStart, true)
			if err != nil {
				errs[i] = fmt.Errorf("buildEnforcedCmd: %w", err)
				return
			}
			if err := runEnforcedCmd(cmd); err != nil {
				errs[i] = fmt.Errorf("runEnforcedCmd: %w", err)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("iteration %d: %v", i, err)
			continue
		}
		got, readErr := os.ReadFile(markers[i])
		if readErr != nil {
			t.Errorf("iteration %d: read marker: %v", i, readErr)
			continue
		}
		if string(got) != "ran\n" {
			t.Errorf("iteration %d: marker = %q, want %q", i, got, "ran\n")
		}
	}
}
