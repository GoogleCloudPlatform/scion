package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNotFoundPerResource: for every resource type the scripts check,
// gcloud's real not-found message (the stub prints each command's own
// wording) means "create" (or "skip" in teardown), and the real denial
// message, which says "(or it may not exist)", stops the script.
func TestNotFoundPerResource(t *testing.T) {
	cases := []struct {
		name, script string
		args         []string
		rid          string   // resource ID after "describe"
		exists       []string // preconditions
		wantOnMiss   string   // a call that must follow the not-found
	}{
		{"service account", "bootstrap-gcp.sh", nil, "cr-runner-build-sa@example-project.iam.gserviceaccount.com", nil, "iam service-accounts create cr-runner-build-sa"},
		{"artifact repository", "bootstrap-gcp.sh", nil, "cr-runners", nil, "artifacts repositories create cr-runners"},
		{"staging bucket", "bootstrap-gcp.sh", nil, "gs://example-project-cr-ci-staging", nil, "storage buckets create gs://example-project-cr-ci-staging"},
		{"secret", "bootstrap-gcp.sh", nil, "cr-runner-gh-app-private-key", nil, "secrets create cr-runner-gh-app-private-key"},
		{"image", "build-runner-image.sh", []string{"--tag", "golden"}, "us-central1-docker.pkg.dev/example-project/cr-runners/gh-runner:golden", nil, "builds submit runner"},
		{"run job", "deploy-job.sh", []string{"--tag", "golden"}, "cr-gh-runner-8c32g", []string{"us-central1-docker.pkg.dev/example-project/cr-runners/gh-runner:golden"}, "run jobs create cr-gh-runner-8c32g"},
		{"run service", "deploy-mediator.sh", []string{"--tag", "golden"}, "us-central1-docker.pkg.dev/example-project/cr-runners/cr-runner-mediator:golden", []string{"cr-runner-mediator"}, "builds submit ."},
		{"scheduler job", "deploy-scheduler.sh", nil, "cr-runner-reconciler", nil, "scheduler jobs create http cr-runner-reconciler"},
		{"run service (teardown skips)", "teardown.sh", []string{"--confirm", project}, "cr-runner-mediator", nil, "run jobs describe"},
		{"run job (teardown skips)", "teardown.sh", []string{"--confirm", project}, "cr-gh-runner-8c32g", nil, "artifacts repositories describe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, baseConfig())
			e.state("exists", tc.exists...)
			args := append([]string{"--config", "config.env", "--apply"}, tc.args...)
			r := e.run(tc.script, "", args...)
			wantCode(t, r, 0)
			if !strings.Contains(strings.Join(callStrings(r.calls), "\n"), tc.wantOnMiss) {
				t.Errorf("after not-found, no call contains %q:\n%s", tc.wantOnMiss, strings.Join(callStrings(r.calls), "\n"))
			}
			e.state("deny", tc.rid)
			r = e.run(tc.script, "", args...)
			if r.code == 0 || !strings.Contains(r.stderr, "permission denied or other error") {
				t.Errorf("denied check did not stop the script: %d %q", r.code, r.stderr)
			}
		})
	}
}

// TestGcloudIgnoreAllowlist: deploy-mediator.sh uploads cr-ci/ with
// gcloud builds submit, so .gcloudignore must allow only go.mod and the
// non-test files of mediator/. gcloudignore uses gitignore syntax, so git's
// own ignore engine is the oracle.
func TestGcloudIgnoreAllowlist(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git is required")
	}
	rules, err := os.ReadFile(filepath.Join("..", ".gcloudignore"))
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for _, l := range strings.Split(string(rules), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			first = l
			break
		}
	}
	if first != "/*" {
		t.Errorf(".gcloudignore must start with /* (allowlist), starts with %q", first)
	}
	dir := t.TempDir()
	for _, f := range []string{
		"go.mod", "mediator/main.go", "mediator/Dockerfile", "mediator/internal/x/y.go",
		"mediator/main_test.go", "mediator/internal/x/y_test.go",
		"app-key.pem", "webhook-secret", "new-webhook-secret", "config.env", "notes.txt",
		"scripts/lib.sh", "docs/a.md", "runner/Dockerfile", "workflows/w.yml",
	} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), rules, 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun := func(args ...string) string {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gitRun("init", "-q")
	got := strings.Fields(gitRun("ls-files", "--others", "--exclude-standard"))
	sort.Strings(got)
	want := []string{"go.mod", "mediator/Dockerfile", "mediator/internal/x/y.go", "mediator/main.go"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("build context = %v, want only %v", got, want)
	}
}

// TestSecretFilesOutsideCheckout: set-secrets.sh refuses key and secret files
// inside the cr-ci module (the directory deploy-mediator.sh uploads).
func TestSecretFilesOutsideCheckout(t *testing.T) {
	mod := filepath.Join(t.TempDir(), "cr-ci")
	for _, f := range []string{"config.example.env", "scripts/lib.sh", "scripts/set-secrets.sh"} {
		b, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(mod, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(mod, "app-key.pem")
	if err := os.WriteFile(inside, []byte(fakePEM), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, baseConfig())
	script := filepath.Join(mod, "scripts", "set-secrets.sh")
	for name, args := range map[string][]string{
		"key inside":       {"--app-key-file", inside},
		"generated inside": {"--generate-webhook-secret", filepath.Join(mod, "new-secret")},
	} {
		r := e.run(script, "", append([]string{"--config", "config.env"}, args...)...)
		if r.code == 0 || !strings.Contains(r.stderr, "inside the cr-ci checkout") {
			t.Errorf("%s: %d %q", name, r.code, r.stderr)
		}
	}
	e.writeFile("outside.pem", fakePEM, 0o600)
	r := e.run(script, "", "--config", "config.env", "--app-key-file", "outside.pem")
	wantCode(t, r, 0)
}

// TestMediatorRefusesPlaceholderAppID: GITHUB_APP_ID=1 (the documented
// placeholder) is fine for the steps before the App exists, but
// deploy-mediator.sh --apply refuses it before building or deploying.
func TestMediatorRefusesPlaceholderAppID(t *testing.T) {
	cfg := baseConfig()
	cfg["GITHUB_APP_ID"] = "1"
	e := newEnv(t, cfg)
	e.state("exists", allExisting...)
	r := e.run("deploy-mediator.sh", "", "--config", "config.env", "--apply", "--tag", "golden")
	if r.code == 0 || !strings.Contains(r.stderr, "placeholder") {
		t.Errorf("placeholder App ID deployed: %d %q", r.code, r.stderr)
	}
	for _, c := range r.calls {
		if s := c.String(); strings.Contains(s, "run deploy") || strings.Contains(s, "builds submit") {
			t.Errorf("acted before refusing: %s", s)
		}
	}
	r = e.run("deploy-mediator.sh", "", "--config", "config.env", "--tag", "golden")
	wantCode(t, r, 0)
	if !strings.Contains(r.stdout, "placeholder") {
		t.Error("dry-run does not warn about the placeholder")
	}
	r = e.run("bootstrap-gcp.sh", "", "--config", "config.env", "--apply")
	wantCode(t, r, 0)
}
