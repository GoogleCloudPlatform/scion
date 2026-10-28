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
		{"mediator image", "deploy-mediator.sh", []string{"--tag", "golden"}, "us-central1-docker.pkg.dev/example-project/cr-runners/cr-runner-mediator:golden", []string{"cr-runner-mediator"}, "builds submit ."},
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
		cmd.Env = minimalGitEnv(dir)
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

// TestBootstrapEnablesOnlyMissingAPIs: C1 lists the enabled APIs and enables
// only the missing ones; with all enabled it makes no enable call (so the
// deployer does not need serviceusage.services.enable), and a list error stops.
func TestBootstrapEnablesOnlyMissingAPIs(t *testing.T) {
	all := "run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com " +
		"cloudscheduler.googleapis.com cloudbuild.googleapis.com iam.googleapis.com storage.googleapis.com"
	enableCalls := func(r result) []string {
		var out []string
		for _, c := range r.calls {
			if s := c.String(); strings.Contains(s, "services enable") {
				out = append(out, s)
			}
		}
		return out
	}
	e := newEnv(t, baseConfig())

	e.extra = []string{"STUB_ENABLED_APIS=" + all + " compute.googleapis.com"}
	r := e.run("bootstrap-gcp.sh", "", "--config", "config.env", "--apply")
	wantCode(t, r, 0)
	if got := enableCalls(r); len(got) != 0 {
		t.Errorf("all APIs enabled, but enable was called: %v", got)
	}
	if !strings.Contains(r.stdout, "all required APIs are enabled") {
		t.Errorf("no note for the skipped enable:\n%s", r.stdout)
	}

	e.extra = []string{"STUB_ENABLED_APIS=run.googleapis.com iam.googleapis.com storage.googleapis.com"}
	r = e.run("bootstrap-gcp.sh", "", "--config", "config.env", "--apply")
	wantCode(t, r, 0)
	want := "gcloud services enable artifactregistry.googleapis.com secretmanager.googleapis.com " +
		"cloudscheduler.googleapis.com cloudbuild.googleapis.com --project=example-project"
	if got := enableCalls(r); len(got) != 1 || got[0] != want {
		t.Errorf("enable calls = %v, want only the missing APIs:\n%s", got, want)
	}

	e.extra = []string{"STUB_SERVICES_LIST_DENIED=1"}
	r = e.run("bootstrap-gcp.sh", "", "--config", "config.env", "--apply")
	if r.code == 0 || !strings.Contains(r.stderr, "query failed") {
		t.Errorf("a failed API list did not stop the script: %d %q", r.code, r.stderr)
	}
	if got := enableCalls(r); len(got) != 0 {
		t.Errorf("enabled APIs after a failed list: %v", got)
	}
}

// TestBuildKitEverywhere: both images, on both build paths, are built with
// BuildKit (runner/Dockerfile uses COPY --chmod, which the legacy builder of
// gcr.io/cloud-builders/docker rejects).
func TestBuildKitEverywhere(t *testing.T) {
	cb, err := os.ReadFile("cloudbuild.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cb), "env: ['DOCKER_BUILDKIT=1']") {
		t.Errorf("scripts/cloudbuild.yaml does not set DOCKER_BUILDKIT=1 on the docker step:\n%s", cb)
	}
	for _, s := range []struct{ script, image string }{
		{"build-runner-image.sh", "gh-runner"},
		{"deploy-mediator.sh", "cr-runner-mediator"},
	} {
		e := newEnv(t, baseConfig())
		e.state("exists", "cr-runner-mediator", "cr-gh-runner-8c32g")
		// Cloud Build: the shared config (with BuildKit) is the only one used.
		r := e.run(s.script, "", "--config", "config.env", "--apply", "--tag", "golden")
		wantCode(t, r, 0)
		n := 0
		for _, c := range r.calls {
			if str := c.String(); strings.Contains(str, "builds submit") {
				n++
				if !strings.Contains(str, "--config=scripts/cloudbuild.yaml") {
					t.Errorf("%s: Cloud Build without the shared config: %s", s.script, str)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d Cloud Build submissions, want 1", s.script, n)
		}
		// Local: docker build runs with DOCKER_BUILDKIT=1.
		r = e.run(s.script, "", "--config", "config.env", "--apply", "--tag", "golden", "--local")
		wantCode(t, r, 0)
		builds := 0
		for _, c := range r.calls {
			if c.argv[0] == "docker" && len(c.argv) > 1 && c.argv[1] == "build" {
				builds++
				if strings.TrimSpace(c.env) != "DOCKER_BUILDKIT=1" {
					t.Errorf("%s --local: docker build ran with %q, want DOCKER_BUILDKIT=1", s.script, c.env)
				}
				if !strings.Contains(c.String(), "/"+s.image+":golden") {
					t.Errorf("%s --local: built %s", s.script, c)
				}
			}
		}
		if builds != 1 {
			t.Errorf("%s --local: %d docker builds, want 1", s.script, builds)
		}
	}
}

// TestMediatorBuildContext: mediator/Dockerfile copies its whole build context
// (so hack/check-image-go-dirs.sh, which guards root-module Dockerfiles,
// accepts it), which is safe only because both context filters are
// allowlists: .gcloudignore (Cloud Build upload, TestGcloudIgnoreAllowlist)
// and .dockerignore (local builds), each allowing only go.mod and mediator/.
func TestMediatorBuildContext(t *testing.T) {
	df, err := os.ReadFile(filepath.Join("..", "mediator", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var copies []string
	for _, l := range strings.Split(string(df), "\n") {
		if f := strings.Fields(l); len(f) > 0 && strings.EqualFold(f[0], "COPY") && !strings.Contains(l, "--from=") {
			copies = append(copies, strings.Join(f, " "))
		}
	}
	if strings.Join(copies, "|") != "COPY . ./" {
		t.Errorf("mediator/Dockerfile context COPYs = %q, want only the whole-context COPY . ./", copies)
	}
	// A Dockerfile-specific ignore file would replace .dockerignore under
	// BuildKit and bypass the allowlist.
	for _, f := range []string{"Dockerfile.dockerignore", ".dockerignore"} {
		if _, err := os.Stat(filepath.Join("..", "mediator", f)); err == nil {
			t.Errorf("mediator/%s exists; it would override the cr-ci/.dockerignore allowlist", f)
		}
	}
	di, err := os.ReadFile(filepath.Join("..", ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, l := range strings.Split(string(di), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			rules = append(rules, l)
		}
	}
	want := []string{"*", "!go.mod", "!mediator", "mediator/**/*_test.go"}
	if strings.Join(rules, "\n") != strings.Join(want, "\n") {
		t.Errorf(".dockerignore rules = %q, want the allowlist %q", rules, want)
	}
}
