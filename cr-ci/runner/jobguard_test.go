package runner

import (
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
	out := t.TempDir()
	markerPath := filepath.Join(out, "marker")
	script := `sleep 30 & echo $! > "$OUT/sleeper"
"$HOOK"; echo $? > "$OUT/hook-rc"
touch "$OUT/after"
kill "$(cat "$OUT/sleeper")"
`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "OUT=" + out, "HOOK=" + guardHookPath(t), "CR_JOB_GUARD_MARKER=" + markerPath, "LC_ALL=C"}
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
		{"pull_request, payload unparsable", "pull_request", `{not json`, testRepoFull, "reason=payload_unparsable"},
		{"pull_request, payload missing", "pull_request", "-", testRepoFull, "reason=payload_missing"},
		{"pull_request_target", "pull_request_target", prPayload(testRepoFull), testRepoFull, "reason=event_not_allowed event=pull_request_target"},
		{"workflow_run", "workflow_run", "", testRepoFull, "reason=event_not_allowed event=workflow_run"},
		{"issue_comment", "issue_comment", "", testRepoFull, "reason=event_not_allowed event=issue_comment"},
		{"workflow_dispatch", "workflow_dispatch", "", testRepoFull, "reason=event_not_allowed event=workflow_dispatch"},
		{"unknown event", "some_future_event", "", testRepoFull, "reason=event_not_allowed event=some_future_event"},
		{"no event", "", "", testRepoFull, "reason=event_not_allowed event= "},
		{"no repository", "push", "", "", "reason=repository_missing"},
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
func guardRunSh(event, payload string) string {
	return stubRunShWith(`payload="$out/event.json"
printf '%s' '` + payload + `' > "$payload"
GITHUB_EVENT_NAME=` + event + ` GITHUB_REPOSITORY=` + testRepoFull + ` GITHUB_EVENT_PATH="$payload" "$ACTIONS_RUNNER_HOOK_JOB_STARTED"
touch "$out/always-step-ran"
exit 0
`)
}

// TestEntrypointJobGuardRefusal: a disallowed job is stopped before its next step;
// the execution exits 5 with the guard log line, which carries no secret (h.run
// checks the claim token, ID token and JIT config never reach the output).
func TestEntrypointJobGuardRefusal(t *testing.T) {
	h := newSuccessHarness(t)
	h.env["CR_JOB_GUARD_HOOK"] = guardHookPath(t)
	h.setRunSh(guardRunSh("pull_request", prPayload("contributor/example-repo")))
	res := h.run()
	if res.exitCode != 5 {
		t.Errorf("exit code = %d, want 5 (job refused); stderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "ERROR job_guard_refused: the job-started hook refused the job assigned to this runner (reason=head_repository_differs event=pull_request head=contributor/example-repo repository=example-owner/example-repo); runner stopped before any step; exiting 5") {
		t.Errorf("stderr lacks the guard line:\n%s", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.stubOut, "always-step-ran")); err == nil {
		t.Error("a step after the refused hook ran")
	}
}

// TestEntrypointJobGuardAllows: an allowed job runs on; no guard line.
func TestEntrypointJobGuardAllows(t *testing.T) {
	h := newSuccessHarness(t)
	h.env["CR_JOB_GUARD_HOOK"] = guardHookPath(t)
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
