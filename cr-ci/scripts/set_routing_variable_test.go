package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// routingConfig is a valid config with two target repos (fake values only).
const routingConfig = `TARGET_REPOS=ptone/scion, GoogleCloudPlatform/scion
GITHUB_APP_ID=1
GITHUB_APP_INSTALLATION_SCOPE=repo
GCP_PROJECT=example-project
REGION=us-central1
`

// ghStub logs every call to $GH_LOG. "variable get" succeeds for repos listed
// in $GH_PRESENT, fails with GitHub's not-found text otherwise, or with a 403
// when $GH_DENY is set. $GH_VERSION overrides the reported version.
const ghStub = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GH_LOG"
case "$1 $2" in
  "--version ") echo "gh version ${GH_VERSION:-2.60.0} (2024-01-01)" ;;
  "variable get")
    if [[ -n ${GH_DENY:-} ]]; then echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; fi
    if [[ " ${GH_PRESENT:-} " == *" $5 "* ]]; then echo scion-cr-8c32g; exit 0; fi
    echo "variable $3 was not found" >&2; exit 1 ;;
  "variable set"|"variable delete")
    if [[ -n ${GH_WRITE_FAIL:-} ]]; then echo "HTTP 403: Must have admin rights to Repository." >&2; exit 1; fi
    exit 0 ;;
  *) echo "unexpected gh call" >&2; exit 9 ;;
esac
`

type routingRun struct {
	out   string
	rc    int
	calls []string
}

func runRouting(t *testing.T, env []string, args ...string) routingRun {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.env")
	if err := os.WriteFile(cfg, []byte(routingConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghStub), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "gh.log")
	cmd := exec.Command("bash", append([]string{"set-routing-variable.sh", "--config", cfg}, args...)...)
	cmd.Env = append([]string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GH_LOG=" + log,
	}, env...)
	out, err := cmd.CombinedOutput()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var calls []string
	if b, err := os.ReadFile(log); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" && l != "--version" {
				calls = append(calls, l)
			}
		}
	}
	return routingRun{out: string(out), rc: rc, calls: calls}
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w+"\n") {
			t.Errorf("output lacks line %q:\n%s", w, out)
		}
	}
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("gh calls = %q, want %q", got, want)
	}
}

func TestRoutingDryRunSetsEveryTargetWithoutCallingGh(t *testing.T) {
	r := runRouting(t, nil)
	if r.rc != 0 {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
	wantLines(t, r.out,
		"+ gh variable set SCION_LARGE_RUNNER --repo ptone/scion --body scion-cr-8c32g",
		"+ gh variable set SCION_LARGE_RUNNER --repo GoogleCloudPlatform/scion --body scion-cr-8c32g")
	wantCalls(t, r.calls)
}

func TestRoutingDryRunClearPrintsDelete(t *testing.T) {
	r := runRouting(t, nil, "--repo", "ptone/scion", "--clear")
	if r.rc != 0 {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
	wantLines(t, r.out,
		"? gh variable get SCION_LARGE_RUNNER --repo ptone/scion",
		"+ gh variable delete SCION_LARGE_RUNNER --repo ptone/scion")
	wantCalls(t, r.calls)
}

func TestRoutingApplySet(t *testing.T) {
	r := runRouting(t, nil, "--repo", "GoogleCloudPlatform/scion", "--apply")
	if r.rc != 0 {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
	wantCalls(t, r.calls, "variable set SCION_LARGE_RUNNER --repo GoogleCloudPlatform/scion --body scion-cr-8c32g")
}

func TestRoutingApplySetCustomValue(t *testing.T) {
	r := runRouting(t, nil, "--repo", "ptone/scion", "--value", "ubuntu-latest-8-cores", "--apply")
	if r.rc != 0 {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
	wantCalls(t, r.calls, "variable set SCION_LARGE_RUNNER --repo ptone/scion --body ubuntu-latest-8-cores")
}

func TestRoutingApplyClearIsIdempotent(t *testing.T) {
	r := runRouting(t, []string{"GH_PRESENT=ptone/scion"}, "--clear", "--apply")
	if r.rc != 0 {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
	wantCalls(t, r.calls,
		"variable get SCION_LARGE_RUNNER --repo ptone/scion",
		"variable delete SCION_LARGE_RUNNER --repo ptone/scion",
		"variable get SCION_LARGE_RUNNER --repo GoogleCloudPlatform/scion")
	wantLines(t, r.out, "# GoogleCloudPlatform/scion: SCION_LARGE_RUNNER is not set (or the repo was not found: check the name); nothing to clear")
}

func TestRoutingClearAcceptsRepoOutsideTargets(t *testing.T) {
	r := runRouting(t, []string{"GH_PRESENT=other/repo"}, "--repo", "other/repo", "--clear", "--apply")
	if r.rc != 0 {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
	wantCalls(t, r.calls,
		"variable get SCION_LARGE_RUNNER --repo other/repo",
		"variable delete SCION_LARGE_RUNNER --repo other/repo")
}

func TestRoutingApplyStopsOnPermissionDenied(t *testing.T) {
	r := runRouting(t, []string{"GH_DENY=1"}, "--repo", "ptone/scion", "--clear", "--apply")
	if r.rc == 0 {
		t.Fatalf("rc=0 on a 403\n%s", r.out)
	}
	wantCalls(t, r.calls, "variable get SCION_LARGE_RUNNER --repo ptone/scion")
}

func TestRoutingApplyStopsWhenSetFails(t *testing.T) {
	r := runRouting(t, []string{"GH_WRITE_FAIL=1"}, "--apply")
	if r.rc == 0 || !strings.Contains(r.out, "step failed") {
		t.Fatalf("rc=%d, want a failure naming the step:\n%s", r.rc, r.out)
	}
	// It stops at the first failed repo instead of carrying on.
	wantCalls(t, r.calls, "variable set SCION_LARGE_RUNNER --repo ptone/scion --body scion-cr-8c32g")
}

func TestRoutingRejects(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		args []string
		msg  string
	}{
		{"repo outside TARGET_REPOS", nil, []string{"--repo", "other/repo"}, "is not in TARGET_REPOS"},
		{"malformed repo", nil, []string{"--repo", "no-slash", "--clear"}, "is not OWNER/REPO"},
		{"label with comma", nil, []string{"--value", "a,b"}, "is not a single runner label"},
		{"label with space", nil, []string{"--value", "a b"}, "is not a single runner label"},
		{"clear and value", nil, []string{"--clear", "--value", "x"}, "exclusive"},
		{"unknown flag", nil, []string{"--force"}, "unknown argument"},
		{"old gh", []string{"GH_VERSION=2.30.0"}, []string{"--apply"}, "older than the required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runRouting(t, tt.env, tt.args...)
			if r.rc == 0 || !strings.Contains(r.out, tt.msg) {
				t.Errorf("rc=%d, want non-zero with %q:\n%s", r.rc, tt.msg, r.out)
			}
			for _, c := range r.calls {
				if strings.HasPrefix(c, "variable set") || strings.HasPrefix(c, "variable delete") {
					t.Errorf("mutating gh call despite the error: %s", c)
				}
			}
		})
	}
}
