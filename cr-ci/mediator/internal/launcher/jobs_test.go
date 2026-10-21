package launcher

import (
	"context"
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
	ref, err := l.Launch(context.Background(), LaunchSpec{JobID: 77, RunID: 7, Repo: "o/r", RunnerName: "cr-job-77", ClaimToken: "0123456789abcdef0123456789abcdef"})
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
		EnvJobID: "77", EnvRunnerName: "cr-job-77",
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
	if len(cr.RunBodies) != 0 {
		t.Error("incomplete spec reached Cloud Run")
	}
}

func TestActiveTerminateReap(t *testing.T) {
	l, cr := newLauncher(t)
	now := time.Unix(1_700_000_000, 0)
	l.Now = func() time.Time { return now }
	cr.AddExecution(fakes.Execution{Created: now.Add(-time.Hour), Completed: true, Env: map[string]string{EnvJobID: "1"}})
	cr.AddExecution(fakes.Execution{Created: now.Add(-2 * time.Minute), Env: map[string]string{EnvJobID: "2"}})
	cr.AddExecution(fakes.Execution{Created: now.Add(-2 * time.Hour), Env: map[string]string{EnvJobID: "3"}})
	ctx := context.Background()
	active, err := l.Active(ctx)
	if err != nil || len(active) != 2 {
		t.Fatalf("Active = %+v %v, want 2", active, err)
	}
	if active[0].JobID != 2 || active[1].JobID != 3 {
		t.Errorf("job IDs from env = %d %d", active[0].JobID, active[1].JobID)
	}
	// Terminate by job ID lookup.
	if err := l.Terminate(ctx, 2, ""); err != nil {
		t.Fatal(err)
	}
	// Already finished, and unknown job: both fine.
	if err := l.Terminate(ctx, 2, active[0].Ref); err != nil {
		t.Errorf("terminating a finished execution: %v", err)
	}
	if err := l.Terminate(ctx, 999, ""); err != nil {
		t.Errorf("terminating an unknown job: %v", err)
	}
	if err := l.Terminate(ctx, 1, "projects/other/locations/x/jobs/y/executions/z"); err == nil {
		t.Error("cancelled an execution of another job")
	}
	n, err := l.ReapOrphans(ctx, 50*time.Minute)
	if err != nil || n != 1 {
		t.Errorf("ReapOrphans = %d %v, want 1", n, err)
	}
	if active, _ := l.Active(ctx); len(active) != 0 {
		t.Errorf("active after reap = %v", active)
	}
}
