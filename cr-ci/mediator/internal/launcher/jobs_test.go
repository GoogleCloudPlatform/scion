package launcher

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/fakes"
)

const jobName = "projects/example-project/locations/us-central1/jobs/cr-gh-runner-8c32g"

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func newLauncher(t *testing.T) (*JobsLauncher, *fakes.CloudRun) {
	t.Helper()
	cr := fakes.NewCloudRun(jobName)
	t.Cleanup(cr.Close)
	return &JobsLauncher{
		BaseURL: cr.Server.URL, Tokens: staticToken("ya29.test"),
		Project: "example-project", Region: "us-central1", Job: "cr-gh-runner-8c32g",
		MediatorURL: "https://mediator.example.com",
	}, cr
}

func TestLaunchEnvOverrides(t *testing.T) {
	l, cr := newLauncher(t)
	ref, err := l.Launch(context.Background(), LaunchSpec{JobID: 77, RunID: 7, Repo: "o/r", ServedRepos: []string{"o/r", "o/s"}, RunnerName: "cr-job-77", ClaimToken: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, jobName+"/executions/") {
		t.Errorf("ref = %q", ref)
	}
	ex := cr.Executions()
	if len(ex) != 1 {
		t.Fatalf("executions = %d", len(ex))
	}
	want := map[string]string{
		EnvClaimToken: "0123456789abcdef0123456789abcdef", EnvMediatorURL: "https://mediator.example.com",
		EnvJobID: "77", EnvRunnerName: "cr-job-77", EnvRepos: "o/r,o/s",
	}
	if len(ex[0].Env) != len(want) {
		t.Errorf("env = %v, want exactly %v", ex[0].Env, want)
	}
	for k, v := range want {
		if ex[0].Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, ex[0].Env[k], v)
		}
	}
	if strings.Contains(strings.ToUpper(cr.RunBodies[0]), "JIT") {
		t.Errorf("run request mentions a JIT config: %s", cr.RunBodies[0])
	}
}

func TestLaunchRejectsIncompleteSpec(t *testing.T) {
	l, cr := newLauncher(t)
	if _, err := l.Launch(context.Background(), LaunchSpec{JobID: 1, RunnerName: "cr-job-1"}); err == nil {
		t.Error("launch without a claim token succeeded")
	}
	// The runner's job guard binds to GH_REPOS, so a launch must carry a well-formed list:
	// each entry as entrypoint.sh accepts it, at most maxServedRepos of them.
	tooMany := make([]string, maxServedRepos+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("o/r%d", i)
	}
	for _, served := range [][]string{nil, {""}, {"o"}, {"o/"}, {"/r"}, {"o/r,x/y"}, {"o/r", "o/ s"},
		{"o/r/x"}, {"o!/r"}, {"o/r$"}, {"\u00f6/r"}, {"o_x/r"}, {"o/r\n"}, tooMany} {
		if _, err := l.Launch(context.Background(), LaunchSpec{JobID: 1, RunnerName: "cr-job-1", ClaimToken: "0123456789abcdef0123456789abcdef", ServedRepos: served}); err == nil {
			t.Errorf("launch with served repositories %q succeeded", served)
		}
	}
	if len(cr.RunBodies) != 0 {
		t.Error("incomplete spec reached Cloud Run")
	}
}

func TestActiveTerminateReap(t *testing.T) {
	l, cr := newLauncher(t)
	now := time.Unix(1_700_000_000, 0)
	l.Now = func() time.Time { return now }
	cr.AddExecution(fakes.Execution{Created: now.Add(-time.Hour), Completed: true, Env: map[string]string{EnvJobID: "1", EnvRunnerName: "cr-job-1"}})
	cr.AddExecution(fakes.Execution{Created: now.Add(-2 * time.Minute), Env: map[string]string{EnvJobID: "2", EnvRunnerName: "cr-job-2"}})
	cr.AddExecution(fakes.Execution{Created: now.Add(-2 * time.Hour), Env: map[string]string{EnvJobID: "3", EnvRunnerName: "cr-job-9"}})
	ctx := context.Background()
	active, err := l.Active(ctx)
	if err != nil || len(active) != 2 {
		t.Fatalf("Active = %+v %v, want 2", active, err)
	}
	if active[0].JobID != 2 || active[1].JobID != 3 || active[1].RunnerName != "cr-job-9" {
		t.Errorf("env not parsed: %+v", active)
	}
	// Terminate looks the execution up by runner name, not job ID.
	if err := l.Terminate(ctx, "cr-job-3", ""); err != nil {
		t.Fatal(err)
	}
	if len(cr.CancelCalls) != 0 {
		t.Errorf("cancelled by job ID: %v", cr.CancelCalls)
	}
	if err := l.Terminate(ctx, "cr-job-2", ""); err != nil {
		t.Fatal(err)
	}
	// Already finished, and unknown runner: both fine.
	if err := l.Terminate(ctx, "cr-job-2", active[0].Ref); err != nil {
		t.Errorf("terminating a finished execution: %v", err)
	}
	if err := l.Terminate(ctx, "cr-job-999", ""); err != nil {
		t.Errorf("terminating an unknown runner: %v", err)
	}
	if err := l.Terminate(ctx, "cr-job-1", "projects/other/locations/x/jobs/y/executions/z"); err == nil {
		t.Error("cancelled an execution of another job")
	}
	all, err := l.Executions(ctx)
	if err != nil || len(all) != 3 || !all[0].Done || !all[1].Done || all[2].Done {
		t.Errorf("Executions = %+v %v, want 3, only the last running", all, err)
	}
	n, err := l.ReapOrphans(ctx, 50*time.Minute)
	if err != nil || n != 1 {
		t.Errorf("ReapOrphans = %d %v, want 1", n, err)
	}
	if active, _ := l.Active(ctx); len(active) != 0 {
		t.Errorf("active after reap = %v", active)
	}
}

// TestLaunchAcceptsServedRepos: names with . _ - and a list at the cap launch.
func TestLaunchAcceptsServedRepos(t *testing.T) {
	l, cr := newLauncher(t)
	full := make([]string, maxServedRepos)
	for i := range full {
		full[i] = fmt.Sprintf("example-owner/repo.%d_x-y", i)
	}
	for _, served := range [][]string{{"o/r"}, {"Example-Owner/a.b_c-d"}, full} {
		if _, err := l.Launch(context.Background(), LaunchSpec{JobID: 1, RunnerName: "cr-job-1", ClaimToken: "0123456789abcdef0123456789abcdef", ServedRepos: served}); err != nil {
			t.Errorf("launch with %d served repositories (%q...): %v", len(served), served[0], err)
		}
	}
	if got := len(cr.RunBodies); got != 3 {
		t.Errorf("run requests = %d, want 3", got)
	}
}

// TestServedReposMatchEntrypoint: the launcher's GH_REPOS entry pattern and cap are
// the ones cr-ci/runner/entrypoint.sh enforces, so a list the launcher accepts never
// makes the runner exit 2 at startup.
func TestServedReposMatchEntrypoint(t *testing.T) {
	b, err := os.ReadFile("../../../runner/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "[[ $GH_REPOS =~ "
	var got string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, prefix); i >= 0 {
			if got != "" {
				t.Fatalf("entrypoint.sh checks GH_REPOS more than once")
			}
			got = strings.TrimSuffix(strings.TrimSpace(line[i+len(prefix):]), " ]] ||")
		}
	}
	entry := strings.TrimSuffix(strings.TrimPrefix(servedRepoRE.String(), "^"), "$")
	want := fmt.Sprintf("^%s(,%s){0,%d}$", entry, entry, maxServedRepos-1)
	if got != want {
		t.Errorf("entrypoint.sh GH_REPOS pattern = %q, want %q (servedRepoRE and maxServedRepos)", got, want)
	}
}
