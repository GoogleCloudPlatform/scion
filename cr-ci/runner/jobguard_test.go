package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testRepoFull = "example-owner/example-repo"

func guardHookPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("cr-ci-job-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// installGuard copies the hook and its committed sha256 into root/guard-bin and
// root/guard-etc as the image has them (0755 and 0444, directories 0755), owned by the
// test's uid (CR_JOB_GUARD_OWNER_UID), and returns their paths.
func installGuard(t *testing.T, root string) (hook, shaFile string) {
	t.Helper()
	hook = filepath.Join(root, "guard-bin", "cr-ci-job-guard.sh")
	shaFile = filepath.Join(root, "guard-etc", "job-guard.sha256")
	for _, f := range []struct {
		src, dst string
		mode     os.FileMode
	}{{"cr-ci-job-guard.sh", hook, 0o755}, {"cr-ci-job-guard.sh.sha256", shaFile, 0o444}} {
		b, err := os.ReadFile(f.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(f.dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.dst, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	return hook, shaFile
}

func writePayload(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "event.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func prPayload(head string) string {
	return `{"pull_request":{"head":{"repo":{"full_name":"` + head + `"}}}}`
}

// runGuardInGroup runs the hook inside a stand-in process group (a bash leader with a
// background sleeper, as run.sh's group holds Runner.Listener and Runner.Worker). It
// returns whether the code after the hook ran (the group survived), the hook's exit
// code when it did, and the marker's content.
func runGuardInGroup(t *testing.T, env map[string]string) (survived bool, hookRC string, marker string) {
	t.Helper()
	return runGuard(t, env, `"$HOOK_SHELL" -e "$HOOK"`)
}

// runGuard runs the hook with invoke (a shell fragment) from a stand-in worker. The
// runner runs a hook as "bash -e <file>" (ScriptHandler.cs L213-216 with
// ScriptHandlerHelpers.cs L19), so the hook's commands run under errexit.
func runGuard(t *testing.T, env map[string]string, invoke string) (survived bool, hookRC string, marker string) {
	t.Helper()
	out := t.TempDir()
	markerPath := filepath.Join(out, "marker")
	script := `sleep 30 & echo $! > "$OUT/sleeper"
` + invoke + `; echo $? > "$OUT/hook-rc"
touch "$OUT/after"
kill "$(cat "$OUT/sleeper")"
`
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "OUT=" + out, "HOOK=" + guardHookPath(t), "HOOK_SHELL=" + bash,
		"CR_JOB_GUARD_MARKER=" + markerPath, "CR_JOB_GUARD_REPOS=" + testRepoFull, "LC_ALL=C"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatal("guard did not finish within 30 s")
	}
	// Whatever happened, the sleeper must not outlive the test.
	if b, err := os.ReadFile(filepath.Join(out, "sleeper")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			if processAlive(pid) && !survivedFile(out) {
				t.Errorf("the sleeper in the refused job's process group is still alive")
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if b, err := os.ReadFile(filepath.Join(out, "hook-rc")); err == nil {
		hookRC = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(markerPath); err == nil {
		marker = strings.TrimSpace(string(b))
	}
	return survivedFile(out), hookRC, marker
}

// processAlive: the process exists and is not a zombie (a killed child whose parent
// is gone stays a zombie until something reaps it).
func processAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

func survivedFile(out string) bool {
	_, err := os.Stat(filepath.Join(out, "after"))
	return err == nil
}

// TestJobGuardAllows: same-repo push and pull_request jobs pass the hook (exit 0, no
// marker, the process group untouched).
func TestJobGuardAllows(t *testing.T) {
	requireTools(t)
	for _, tc := range []struct {
		name, event, payload string
	}{
		{"push", "push", ""},
		{"same-repo pull_request", "pull_request", prPayload(testRepoFull)},
		{"same-repo pull_request, other case", "pull_request", prPayload("Example-Owner/Example-Repo")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GITHUB_EVENT_NAME": tc.event, "GITHUB_REPOSITORY": testRepoFull}
			if tc.payload != "" {
				env["GITHUB_EVENT_PATH"] = writePayload(t, t.TempDir(), tc.payload)
			}
			survived, rc, marker := runGuardInGroup(t, env)
			if !survived || rc != "0" || marker != "" {
				t.Errorf("survived=%v hook rc=%q marker=%q, want the job allowed", survived, rc, marker)
			}
		})
	}
}

// TestJobGuardRefuses: anything else is refused with a hard stop. The hook writes the
// marker and kills its process group, so nothing after it runs (no later step, even one
// with if: always()). Fail closed on a missing or unparsable payload.
func TestJobGuardRefuses(t *testing.T) {
	requireTools(t)
	for _, tc := range []struct {
		name, event, payload, repo, reason string
	}{
		{"fork pull_request", "pull_request", prPayload("contributor/example-repo"), testRepoFull, "reason=head_repository_differs"},
		{"pull_request without head", "pull_request", `{"pull_request":{}}`, testRepoFull, "reason=head_repository_missing"},
		{"pull_request, head repository null (fork deleted)", "pull_request", `{"pull_request":{"head":{"repo":null}}}`, testRepoFull, "reason=head_repository_missing"},
		{"pull_request, payload unparsable", "pull_request", `{not json`, testRepoFull, "reason=payload_unparsable"},
		{"pull_request, payload missing", "pull_request", "-", testRepoFull, "reason=payload_missing"},
		{"pull_request_target", "pull_request_target", prPayload(testRepoFull), testRepoFull, "reason=event_not_allowed event=pull_request_target"},
		{"workflow_run", "workflow_run", "", testRepoFull, "reason=event_not_allowed event=workflow_run"},
		{"issue_comment", "issue_comment", "", testRepoFull, "reason=event_not_allowed event=issue_comment"},
		{"workflow_dispatch", "workflow_dispatch", "", testRepoFull, "reason=event_not_allowed event=workflow_dispatch"},
		{"unknown event", "some_future_event", "", testRepoFull, "reason=event_not_allowed event=some_future_event"},
		{"no event", "", "", testRepoFull, "reason=event_not_allowed event= "},
		{"no repository", "push", "", "", "reason=repository_missing"},
		{"another repository's push", "push", "", "other-owner/example-repo", "reason=repository_not_served event=push head= repository=other-owner/example-repo"},
		{"another repository's same-repo pull_request", "pull_request", prPayload("other-owner/other-repo"), "other-owner/other-repo", "reason=repository_not_served"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GITHUB_EVENT_NAME": tc.event, "GITHUB_REPOSITORY": tc.repo}
			switch tc.payload {
			case "":
			case "-":
				env["GITHUB_EVENT_PATH"] = filepath.Join(t.TempDir(), "absent.json")
			default:
				env["GITHUB_EVENT_PATH"] = writePayload(t, t.TempDir(), tc.payload)
			}
			survived, rc, marker := runGuardInGroup(t, env)
			if survived {
				t.Errorf("the process group survived the refusal (hook rc %q): later steps could run", rc)
			}
			if !strings.Contains(marker+" ", tc.reason) {
				t.Errorf("marker %q does not contain %q", marker, tc.reason)
			}
		})
	}
}

// TestJobGuardSanitizesMarker: a hostile head repository name ends up in the marker
// (and the entrypoint's log line) only as safe characters on one line.
func TestJobGuardSanitizesMarker(t *testing.T) {
	requireTools(t)
	env := map[string]string{
		"GITHUB_EVENT_NAME": "pull_request", "GITHUB_REPOSITORY": testRepoFull,
		"GITHUB_EVENT_PATH": writePayload(t, t.TempDir(), prPayload("evil$(id)`x`\\nINJECT")),
	}
	survived, _, marker := runGuardInGroup(t, env)
	if survived {
		t.Fatal("the process group survived the refusal")
	}
	if !strings.Contains(marker, "head=evil__id__x__INJECT") || strings.ContainsAny(marker, "$`()\n") {
		t.Errorf("marker %q is not sanitized", marker)
	}
}

// guardRunSh is a stub run.sh "job": it calls the job-started hook the way
// Runner.Worker does (a child in run.sh's process group, with the job's GitHub env) and
// then runs a "step" that would be if: always().
func guardRunSh(event, payload string) string { return guardRunShRepo(event, payload, testRepoFull) }

func guardRunShRepo(event, payload, repo string) string {
	return stubRunShWith(`payload="$out/event.json"
printf '%s' '` + payload + `' > "$payload"
GITHUB_EVENT_NAME=` + event + ` GITHUB_REPOSITORY=` + repo + ` GITHUB_EVENT_PATH="$payload" bash -e "$ACTIONS_RUNNER_HOOK_JOB_STARTED"
touch "$out/always-step-ran"
exit 0
`)
}

// TestEntrypointJobGuardRefusal: a disallowed job is stopped before its next step;
// the execution exits 5 with the guard log line, which carries no secret (h.run
// checks the claim token, ID token and JIT config never reach the output).
func TestEntrypointJobGuardRefusal(t *testing.T) {
	h := newSuccessHarness(t)
	h.setRunSh(guardRunSh("pull_request", prPayload("contributor/example-repo")))
	res := h.run()
	if res.exitCode != 5 {
		t.Errorf("exit code = %d, want 5 (job refused); stderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "ERROR job_guard_refused: the job-started hook refused the job assigned to this runner (reason=head_repository_differs event=pull_request head=contributor/example-repo repository=example-owner/example-repo served=example-owner/example-repo); runner stopped before any step; exiting 5") {
		t.Errorf("stderr lacks the guard line:\n%s", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.stubOut, "always-step-ran")); err == nil {
		t.Error("a step after the refused hook ran")
	}
}

// TestEntrypointJobGuardAllows: an allowed job runs on; no guard line.
func TestEntrypointJobGuardAllows(t *testing.T) {
	h := newSuccessHarness(t)
	h.setRunSh(guardRunSh("push", "{}"))
	res := h.run()
	if res.exitCode != 0 || strings.Contains(res.stderr, "job_guard") {
		t.Errorf("exit code %d, stderr:\n%s; want 0 and no guard line", res.exitCode, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.stubOut, "always-step-ran")); err != nil {
		t.Error("the allowed job's step did not run")
	}
}

// TestEntrypointJobGuardEnforcer: fail closed. If the hook's own kill did not take
// effect (simulated: the stub writes the marker and keeps running steps), the
// entrypoint's enforcer SIGKILLs run.sh's process group, and the execution exits 5.
func TestEntrypointJobGuardEnforcer(t *testing.T) {
	h := newSuccessHarness(t)
	h.setRunSh(stubRunShWith(`printf 'reason=test_enforcer event=x head= repository=y\n' > "$CR_JOB_GUARD_MARKER"
for _ in $(seq 1 60); do sleep 0.1; done
touch "$out/late-step"
exit 0
`))
	start := time.Now()
	res := h.run()
	if res.exitCode != 5 || !strings.Contains(res.stderr, "job_guard_refused") {
		t.Errorf("exit code %d, want 5 with the guard line; stderr:\n%s", res.exitCode, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.stubOut, "late-step")); err == nil {
		t.Error("the stub kept running after the marker: the enforcer did not stop it")
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("took %s", d)
	}
}

// TestJobGuardServedRepository (R4, R5): the hook requires GITHUB_REPOSITORY to be one
// of the repositories the runner serves (CR_JOB_GUARD_REPOS, from GH_REPOS): one in repo
// scope, every target repository of the owner in org scope. Fail closed on an empty or
// malformed list.
func TestJobGuardServedRepository(t *testing.T) {
	requireTools(t)
	const orgList = "example-owner/second-repo,example-owner/example-repo,example-owner/third.repo"
	for _, tc := range []struct {
		name, repo, served, reason string // reason "" = allowed
	}{
		{"repo scope", testRepoFull, testRepoFull, ""},
		{"repo scope, other case", testRepoFull, "Example-Owner/Example-Repo", ""},
		{"repo scope, another repository", "example-owner/second-repo", testRepoFull, "reason=repository_not_served"},
		{"org scope, a member", testRepoFull, orgList, ""},
		{"org scope, another member", "example-owner/third.repo", orgList, ""},
		{"org scope, a non-member of the owner", "example-owner/fourth-repo", orgList, "reason=repository_not_served"},
		{"org scope, another owner", "other-owner/example-repo", orgList, "reason=repository_not_served"},
		{"org scope, a prefix is not a member", "example-owner/example", orgList, "reason=repository_not_served"},
		{"list empty", testRepoFull, "", "reason=served_repositories_missing"},
		{"list with an empty entry", testRepoFull, testRepoFull + ",", "reason=served_repositories_malformed"},
		{"list with a leading comma", testRepoFull, "," + testRepoFull, "reason=served_repositories_malformed"},
		{"list with spaces", testRepoFull, "example-owner/second-repo, " + testRepoFull, "reason=served_repositories_malformed"},
		{"list entry without owner", testRepoFull, "example-repo", "reason=served_repositories_malformed"},
		{"list entry with a path", testRepoFull, testRepoFull + "/x", "reason=served_repositories_malformed"},
		{"list with a wildcard", testRepoFull, "example-owner/*", "reason=served_repositories_malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			survived, rc, marker := runGuardInGroup(t, map[string]string{
				"GITHUB_EVENT_NAME": "push", "GITHUB_REPOSITORY": tc.repo, "CR_JOB_GUARD_REPOS": tc.served,
			})
			if tc.reason == "" {
				if !survived || rc != "0" || marker != "" {
					t.Errorf("survived=%v rc=%q marker=%q, want allowed", survived, rc, marker)
				}
				return
			}
			if survived || !strings.Contains(marker, tc.reason) {
				t.Errorf("survived=%v marker=%q, want refused with %s", survived, marker, tc.reason)
			}
		})
	}
}

// TestJobGuardStopsParentFromOwnGroup (R1): the stop must not depend on the hook sharing
// the worker's process group. Run as the runner runs hooks (bash -e, here with
// --noprofile --norc) but in a session and group of its own (setsid), the hook still
// kills its parent, the stand-in worker, and the parent's group (the sleeper), so the
// worker's next "step" never runs.
func TestJobGuardStopsParentFromOwnGroup(t *testing.T) {
	requireTools(t)
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Fatalf("needs setsid on PATH: %v", err)
	}
	for _, tc := range []struct{ name, invoke string }{
		{"own group, refused", `setsid "$HOOK_SHELL" --noprofile --norc -e "$HOOK"`},
		{"worker's group, refused", `"$HOOK_SHELL" --noprofile --norc -e "$HOOK"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			survived, rc, marker := runGuard(t, map[string]string{
				"GITHUB_EVENT_NAME": "pull_request_target", "GITHUB_REPOSITORY": testRepoFull,
			}, tc.invoke)
			if survived {
				t.Errorf("the stand-in worker survived the refusal (hook rc %q): later steps could run", rc)
			}
			if !strings.Contains(marker, "reason=event_not_allowed") {
				t.Errorf("marker %q", marker)
			}
		})
	}
}

// TestJobGuardPinnedSHA (R2a): the committed sha256 pin is the hook's, and the image and
// the entrypoint use the same fixed paths for both.
func TestJobGuardPinnedSHA(t *testing.T) {
	hook, err := os.ReadFile("cr-ci-job-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	pin, err := os.ReadFile("cr-ci-job-guard.sh.sha256")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(hook)
	if got, want := string(pin), hex.EncodeToString(sum[:])+"\n"; got != want {
		t.Errorf("cr-ci-job-guard.sh.sha256 = %q, want %q (update it with: sha256sum cr-ci-job-guard.sh | cut -d' ' -f1 > cr-ci-job-guard.sh.sha256)", got, want)
	}
	if fi, err := os.Stat("cr-ci-job-guard.sh"); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("cr-ci-job-guard.sh must be executable in the repository (%v)", err)
	}
	for file, wants := range map[string][]string{
		"Dockerfile": {
			"COPY --chmod=0755 cr-ci-job-guard.sh /usr/local/bin/cr-ci-job-guard.sh\n",
			"COPY --chmod=0444 cr-ci-job-guard.sh.sha256 /etc/cr-ci/job-guard.sha256\n",
			`sha256sum -c -`,
		},
		"entrypoint.sh": {
			"job_guard_hook=${CR_JOB_GUARD_HOOK:-/usr/local/bin/cr-ci-job-guard.sh}\n",
			"job_guard_sha_file=${CR_JOB_GUARD_SHA_FILE:-/etc/cr-ci/job-guard.sha256}\n",
			"job_guard_owner=${CR_JOB_GUARD_OWNER_UID:-0}\n",
		},
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range wants {
			if !strings.Contains(string(b), w) {
				t.Errorf("%s lacks %q", file, w)
			}
		}
	}
}

// TestEntrypointJobGuardIntegrity (R2a): no fail-open. The entrypoint refuses to start
// run.sh (exit 2, before any claim) unless the hook is the pinned, protected file.
func TestEntrypointJobGuardIntegrity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, h *harness)
		want   string
	}{
		{"hook missing", func(t *testing.T, h *harness) { mustDo(t, os.Remove(h.guardHook)) },
			"is missing or of the wrong type"},
		{"hook not executable", func(t *testing.T, h *harness) { mustDo(t, os.Chmod(h.guardHook, 0o644)) },
			"is not executable"},
		{"hook sha256 mismatch", func(t *testing.T, h *harness) {
			mustDo(t, os.Chmod(h.guardHook, 0o644))
			f, err := os.OpenFile(h.guardHook, os.O_APPEND|os.O_WRONLY, 0)
			mustDo(t, err)
			_, err = f.WriteString("exit 0\n")
			mustDo(t, err)
			mustDo(t, f.Close())
			mustDo(t, os.Chmod(h.guardHook, 0o755))
		}, "(pinned at image build)"},
		{"hook group-writable", func(t *testing.T, h *harness) { mustDo(t, os.Chmod(h.guardHook, 0o775)) },
			"is group- or world-writable (mode 775)"},
		{"hook world-writable", func(t *testing.T, h *harness) { mustDo(t, os.Chmod(h.guardHook, 0o757)) },
			"is group- or world-writable (mode 757)"},
		{"hook directory writable", func(t *testing.T, h *harness) { mustDo(t, os.Chmod(filepath.Dir(h.guardHook), 0o777)) },
			"the directory of the job-started hook"},
		{"hook is a symlink", func(t *testing.T, h *harness) {
			real := h.guardHook + ".real"
			mustDo(t, os.Rename(h.guardHook, real))
			mustDo(t, os.Symlink(real, h.guardHook))
		}, "is a symlink"},
		{"hook owned by another uid", func(t *testing.T, h *harness) {
			h.env["CR_JOB_GUARD_OWNER_UID"] = strconv.Itoa(os.Getuid() + 1)
		}, "is owned by uid"},
		{"sha256 file missing", func(t *testing.T, h *harness) { mustDo(t, os.Remove(h.guardSHAFile)) },
			"the job guard sha256 file"},
		{"sha256 file writable", func(t *testing.T, h *harness) { mustDo(t, os.Chmod(h.guardSHAFile, 0o666)) },
			"the job guard sha256 file "},
		{"sha256 file malformed", func(t *testing.T, h *harness) {
			mustDo(t, os.Chmod(h.guardSHAFile, 0o644))
			mustDo(t, os.WriteFile(h.guardSHAFile, []byte("not-a-sha\n"), 0o444))
		}, "does not hold one sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := newClaimServer(t, respondStatus(http.StatusInternalServerError))
			h := newHarness(t, cs.URL)
			tc.mutate(t, h)
			res := h.run()
			expectFailure(t, h, res, 2, tc.want)
			if !strings.Contains(res.stderr, "refusing to start the runner") {
				t.Errorf("stderr lacks the refusal:\n%s", res.stderr)
			}
			if n := len(cs.recorded()) + len(h.meta.recorded()); n != 0 {
				t.Errorf("%d claim/metadata requests, want none (token must not be spent)", n)
			}
		})
	}
	t.Run("checked in warm-up too", func(t *testing.T) {
		h, cs := warmupHarness(t)
		mustDo(t, os.Chmod(h.guardHook, 0o644))
		expectFailure(t, h, h.run(), 2, "is not executable")
		if n := len(cs.recorded()); n != 0 {
			t.Errorf("%d claim requests, want none", n)
		}
	})
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestEntrypointTestOverridesNeedTestMode (R2a): outside test mode (CR_TEST_MODE unset
// or not 1) every CR_* override, the hook path included, is ignored, so the fixed image
// paths are checked. (Production is also PID 1, which disables test mode outright.)
func TestEntrypointTestOverridesNeedTestMode(t *testing.T) {
	for _, mode := range []string{"", "0", "true"} {
		t.Run("CR_TEST_MODE="+mode, func(t *testing.T) {
			cs := newClaimServer(t, respondStatus(http.StatusInternalServerError))
			h := newHarness(t, cs.URL)
			if mode == "" {
				delete(h.env, "CR_TEST_MODE")
			} else {
				h.env["CR_TEST_MODE"] = mode
			}
			res := h.run()
			if res.exitCode != 2 || h.stubStarted() {
				t.Errorf("exit code %d (run.sh started: %v), want 2 and not started; stderr:\n%s", res.exitCode, h.stubStarted(), res.stderr)
			}
			for _, want := range []string{
				"ignoring CR_JOB_GUARD_HOOK: test overrides take effect only in test mode",
				"ignoring CR_JOB_GUARD_SHA_FILE",
				"ignoring CR_JOB_GUARD_OWNER_UID",
				"ignoring CR_SCRATCH_DIR",
				"job guard: ",
			} {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, res.stderr)
				}
			}
			if strings.Contains(res.stderr, h.guardHook) {
				t.Errorf("the overridden hook path was used outside test mode:\n%s", res.stderr)
			}
			if n := len(cs.recorded()) + len(h.meta.recorded()); n != 0 {
				t.Errorf("%d claim/metadata requests, want none", n)
			}
		})
	}
}

// TestEntrypointGHRepos (R4, R5): GH_REPOS, the repositories the runner serves, is
// required: 1-100 comma-separated owner/name entries. A malformed value is not echoed.
func TestEntrypointGHRepos(t *testing.T) {
	const bad = "GH_REPOS must be 1-100 comma-separated owner/name entries"
	many := strings.TrimSuffix(strings.Repeat("o/r,", 101), ",")
	for _, tc := range []struct{ name, value, want string }{
		{"missing", "", "required environment variable GH_REPOS is not set"},
		{"no owner", "example-repo", bad},
		{"extra path", "owner/repo/x", bad},
		{"shell characters", "owner/re$(id)", bad},
		{"empty entry", testRepoFull + ",", bad},
		{"space after comma", testRepoFull + ", owner/b", bad},
		{"101 entries", many, bad},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := newClaimServer(t, respondStatus(http.StatusInternalServerError))
			h := newHarness(t, cs.URL)
			if tc.value == "" {
				delete(h.env, "GH_REPOS")
			} else {
				h.env["GH_REPOS"] = tc.value
			}
			res := h.run()
			expectFailure(t, h, res, 2, tc.want)
			if tc.value != "" && strings.Contains(res.stderr, tc.value) {
				t.Errorf("the malformed value was echoed:\n%s", res.stderr)
			}
			if n := len(cs.recorded()); n != 0 {
				t.Errorf("%d claim requests, want none", n)
			}
		})
	}
}

// TestEntrypointJobGuardRepositoryBinding (R4): a job of a repository other than GH_REPOS
// is refused (hard stop, exit 5), even a same-repo push there.
func TestEntrypointJobGuardRepositoryBinding(t *testing.T) {
	h := newSuccessHarness(t)
	h.setRunSh(guardRunShRepo("push", "{}", "other-owner/other-repo"))
	res := h.run()
	if res.exitCode != 5 || !strings.Contains(res.stderr, "reason=repository_not_served event=push head= repository=other-owner/other-repo served=example-owner/example-repo") {
		t.Errorf("exit code %d, want 5 with repository_not_served; stderr:\n%s", res.exitCode, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.stubOut, "always-step-ran")); err == nil {
		t.Error("a step after the refused hook ran")
	}
}

// TestEntrypointJobGuardOrgScope (R5): with an org-scope list, a job of any listed
// repository runs on (org runners legitimately take any of them); a job of an unlisted
// repository of the same owner is refused.
func TestEntrypointJobGuardOrgScope(t *testing.T) {
	const list = "example-owner/second-repo,example-owner/example-repo"
	t.Run("member", func(t *testing.T) {
		h := newSuccessHarness(t)
		h.env["GH_REPOS"] = list
		h.setRunSh(guardRunShRepo("push", "{}", "example-owner/second-repo"))
		res := h.run()
		if res.exitCode != 0 || strings.Contains(res.stderr, "job_guard") {
			t.Errorf("exit code %d, want 0 and no guard line; stderr:\n%s", res.exitCode, res.stderr)
		}
		if _, err := os.Stat(filepath.Join(h.stubOut, "always-step-ran")); err != nil {
			t.Error("the member repository's job did not run")
		}
		if env := h.stubFile("env"); !strings.Contains(env+"\n", "CR_JOB_GUARD_REPOS="+list+"\n") {
			t.Errorf("run.sh env lacks the served list; got:\n%s", env)
		}
	})
	t.Run("non-member", func(t *testing.T) {
		h := newSuccessHarness(t)
		h.env["GH_REPOS"] = list
		h.setRunSh(guardRunShRepo("push", "{}", "example-owner/third-repo"))
		res := h.run()
		if res.exitCode != 5 || !strings.Contains(res.stderr, "reason=repository_not_served event=push head= repository=example-owner/third-repo served=example-owner/second-repo,example-owner/example-repo") {
			t.Errorf("exit code %d, want 5 with repository_not_served; stderr:\n%s", res.exitCode, res.stderr)
		}
		if _, err := os.Stat(filepath.Join(h.stubOut, "always-step-ran")); err == nil {
			t.Error("a step after the refused hook ran")
		}
	})
}

// TestEntrypointJobGuardEnvFixed (R2a): values for the hook's settings in the
// entrypoint's own environment are replaced: run.sh (and so Runner.Worker) gets the
// verified hook path and GH_REPOS.
func TestEntrypointJobGuardEnvFixed(t *testing.T) {
	h := newSuccessHarness(t)
	h.env["ACTIONS_RUNNER_HOOK_JOB_STARTED"] = "/bin/true"
	h.env["CR_JOB_GUARD_REPOS"] = "other-owner/other-repo"
	h.env["CR_JOB_GUARD_MARKER"] = filepath.Join(t.TempDir(), "elsewhere")
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code %d; stderr:\n%s", res.exitCode, res.stderr)
	}
	env := h.stubFile("env")
	for _, want := range []string{"ACTIONS_RUNNER_HOOK_JOB_STARTED=" + h.guardHook, "CR_JOB_GUARD_REPOS=" + testRepoFull} {
		if !strings.Contains(env+"\n", want+"\n") {
			t.Errorf("run.sh env lacks %q; got:\n%s", want, env)
		}
	}
}

// TestJobGuardJobLogShowsOnlyReason: what the hook prints goes to the refused job's log,
// which may be public (a fork pull request). It carries only the reason code, no
// repository name. The served list (under org scope it can name private repositories) is
// in neither the job log nor the marker; the entrypoint adds it from GH_REPOS to its own
// host-side line after run.sh has died (TestEntrypointJobGuardServedListHostOnly).
func TestJobGuardJobLogShowsOnlyReason(t *testing.T) {
	requireTools(t)
	const served = "example-owner/example-repo,example-owner/private-repo"
	for _, tc := range []struct {
		name   string
		env    map[string]string
		reason string
	}{
		{"fork pull request", map[string]string{
			"GITHUB_EVENT_NAME": "pull_request", "GITHUB_REPOSITORY": testRepoFull, "CR_JOB_GUARD_REPOS": served,
		}, "head_repository_differs"},
		{"org scope non-member", map[string]string{
			"GITHUB_EVENT_NAME": "push", "GITHUB_REPOSITORY": "example-owner/other-repo", "CR_JOB_GUARD_REPOS": served,
		}, "repository_not_served"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobLog := filepath.Join(t.TempDir(), "job-log")
			tc.env["HOOK_OUT"] = jobLog
			if tc.env["GITHUB_EVENT_NAME"] == "pull_request" {
				tc.env["GITHUB_EVENT_PATH"] = writePayload(t, t.TempDir(), prPayload("contributor/example-repo"))
			}
			survived, _, marker := runGuard(t, tc.env, `"$HOOK_SHELL" -e "$HOOK" >"$HOOK_OUT" 2>&1`)
			if survived {
				t.Fatal("the stand-in worker survived the refusal")
			}
			b, err := os.ReadFile(jobLog)
			if err != nil {
				t.Fatal(err)
			}
			out := string(b)
			if want := "::error title=cr-ci job guard::job refused by the cr-ci runner's job guard (reason=" + tc.reason + ")\n"; !strings.HasPrefix(out, want) {
				t.Errorf("job log = %q, want it to start with %q", out, want)
			}
			for _, leak := range []string{"private-repo", "served=", "repository=", "head=", "event=", "contributor", "other-repo", "example-owner"} {
				if strings.Contains(out, leak) {
					t.Errorf("job log %q contains %q", out, leak)
				}
			}
			// The marker (read by the entrypoint, host side) has the reason but no served
			// list either: the entrypoint adds that from its own GH_REPOS.
			if !strings.Contains(marker, "reason="+tc.reason) || strings.Contains(marker, "served=") || strings.Contains(marker, "private-repo") {
				t.Errorf("marker %q: want the reason and no served list", marker)
			}
		})
	}
}

// TestEntrypointJobGuardServedListHostOnly: the served list appears only in the
// entrypoint's own stderr (container log), written after run.sh has died; the job's
// output (run.sh's stdout, where the hook's job-log line goes) has the reason code only.
func TestEntrypointJobGuardServedListHostOnly(t *testing.T) {
	const list = "example-owner/example-repo,example-owner/private-repo"
	h := newSuccessHarness(t)
	h.env["GH_REPOS"] = list
	h.setRunSh(guardRunShRepo("push", "{}", "example-owner/other-repo"))
	res := h.run()
	if res.exitCode != 5 {
		t.Fatalf("exit code %d, want 5; stderr:\n%s", res.exitCode, res.stderr)
	}
	// stdout is what run.sh (and so the job) printed, relayed by the watcher.
	if !strings.Contains(res.stdout, "::error title=cr-ci job guard::job refused by the cr-ci runner's job guard (reason=repository_not_served)") {
		t.Errorf("job output lacks the reason-only line:\n%s", res.stdout)
	}
	for _, leak := range []string{"private-repo", "served="} {
		if strings.Contains(res.stdout, leak) {
			t.Errorf("job output contains %q:\n%s", leak, res.stdout)
		}
	}
	if !strings.Contains(res.stderr, "ERROR job_guard_refused: the job-started hook refused the job assigned to this runner (reason=repository_not_served event=push head= repository=example-owner/other-repo served="+list+"); runner stopped") {
		t.Errorf("entrypoint log lacks the host-side line with the served list:\n%s", res.stderr)
	}
	// The entrypoint writes that line only after its wait for run.sh returns (section 5);
	// here, at least, it comes after run.sh was started.
	if i, j := strings.Index(res.stderr, "starting run.sh"), strings.Index(res.stderr, "ERROR job_guard_refused"); i < 0 || j < i {
		t.Errorf("guard line not after run.sh start:\n%s", res.stderr)
	}
}
