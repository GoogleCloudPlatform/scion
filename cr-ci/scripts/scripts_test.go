// Package scripts holds the test harness for the cr-ci deploy scripts. The
// scripts run under bash with stub gcloud, docker and gh executables
// (testdata/stubs) first on PATH; the stubs record their argv and never
// contact anything. Dry-run transcripts are compared with committed goldens
// (testdata/golden; regenerate with go test ./scripts -update).
package scripts

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden dry-run transcripts")

// Throwaway values only. The markers make leaks easy to find.
const (
	fakePEM        = "-----BEGIN RSA PRIVATE KEY-----\nFAKE-PEM-MARKER-7f3a91\n-----END RSA PRIVATE KEY-----\n"
	fakeHookSecret = "FAKE-HOOK-MARKER-c0ffee"
	fakeToken      = "FAKE-ACCESS-TOKEN-d2f81c"
	project        = "example-project"
	mediatorURL    = "https://cr-runner-mediator-abc123-uc.a.run.app"
)

// baseConfig is a complete, valid config with fake values.
func baseConfig() map[string]string {
	return map[string]string{
		"TARGET_REPOS":                  "example-owner/example-repo,example-owner/other-repo",
		"GITHUB_APP_ID":                 "123456",
		"GITHUB_APP_INSTALLATION_SCOPE": "repo",
		"GCP_PROJECT":                   project,
		"REGION":                        "us-central1",
		"MEDIATOR_URL":                  mediatorURL,
	}
}

type env struct {
	t       *testing.T
	dir     string // working directory and HOME
	stubDir string // STUB_DIR: calls/, exists, deny
	binDir  string
	config  string
	extra   []string // extra environment
}

func newEnv(t *testing.T, cfg map[string]string) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, stubDir: filepath.Join(dir, "stub"), binDir: filepath.Join(dir, "bin")}
	for _, d := range []string{e.stubDir, e.binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"gcloud", "docker", "gh"} {
		b, err := os.ReadFile(filepath.Join("testdata", "stubs", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.binDir, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.config = e.writeConfig("config.env", cfg)
	return e
}

func (e *env) writeConfig(name string, cfg map[string]string) string {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# test config\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, cfg[k])
	}
	return e.writeFile(name, b.String(), 0o644)
}

func (e *env) writeFile(name, content string, mode os.FileMode) string {
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// state lists resource IDs (the argument after "describe") that exist or
// whose describe is denied.
func (e *env) state(file string, ids ...string) {
	e.writeFile(filepath.Join("stub", file), strings.Join(ids, "\n")+"\n", 0o644)
}

type result struct {
	stdout, stderr string
	code           int
	calls          []call
}

type call struct {
	argv  []string
	stdin string
	has   bool   // stdin recorded
	env   string // recorded environment (docker build: DOCKER_BUILDKIT)
}

func (c call) String() string { return quote(c.argv) }

// run runs a script with the stubs first on PATH, from e.dir.
func (e *env) run(script string, stdin string, args ...string) result {
	e.t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		e.t.Fatal(err)
	}
	abs, err := filepath.Abs(script)
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command(bash, append([]string{abs}, args...)...)
	cmd.Dir = e.dir
	cmd.Env = append([]string{
		"PATH=" + e.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + e.dir,
		"STUB_DIR=" + e.stubDir,
		"LC_ALL=C",
		// git in the scripts (default image tags) must not find a stray
		// repository at or above the temp root, e.g. /tmp/.git.
		"GIT_CEILING_DIRECTORIES=" + os.TempDir(),
	}, e.extra...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatalf("running %s: %v", script, err)
		}
		code = ee.ExitCode()
	}
	r := result{stdout: out.String(), stderr: errb.String(), code: code, calls: e.calls()}
	_ = os.RemoveAll(filepath.Join(e.stubDir, "calls"))
	return r
}

func (e *env) calls() []call {
	e.t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.stubDir, "calls", "*.args"))
	sort.Strings(files)
	var cs []call
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			e.t.Fatal(err)
		}
		argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		c := call{argv: argv}
		if in, err := os.ReadFile(strings.TrimSuffix(f, ".args") + ".stdin"); err == nil {
			c.stdin, c.has = string(in), true
		}
		if env, err := os.ReadFile(strings.TrimSuffix(f, ".args") + ".env"); err == nil {
			c.env = string(env)
		}
		cs = append(cs, c)
	}
	return cs
}

var bareRE = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,^-]+$`)

// quote mirrors crci_quote in lib.sh.
func quote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if bareRE.MatchString(a) {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(parts, " ")
}

// lines returns the transcript lines with the given prefix ("+ " or "? "),
// prefix removed; a "+ a | b" pipeline yields two commands.
func lines(out, prefix string) []string {
	var ls []string
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		l = strings.TrimPrefix(l, prefix)
		if i := strings.Index(l, " < "); i >= 0 && prefix == "+ " {
			l = l[:i] // input redirection is shown, not part of argv
		}
		for _, p := range strings.Split(l, " | ") {
			// "env VAR=value cmd ..." runs cmd; the stubs record cmd.
			for strings.HasPrefix(p, "env ") {
				f := strings.SplitN(p, " ", 3)
				if len(f) < 3 || !strings.Contains(f[1], "=") {
					break
				}
				p = f[2]
			}
			ls = append(ls, p)
		}
	}
	return ls
}

// nonVersion drops the tool version checks of apply mode.
func nonVersion(cs []call) []call {
	var out []call
	for _, c := range cs {
		if len(c.argv) >= 2 && (c.argv[1] == "--version" || (c.argv[0] == "docker" && c.argv[1] == "version")) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func callStrings(cs []call) []string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = c.String()
	}
	return s
}

func wantCode(t *testing.T, r result, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, code, r.stdout, r.stderr)
	}
}

// ---- dry-run goldens ------------------------------------------------------------

type dryCase struct {
	name   string // golden file name
	script string
	args   []string
	setup  func(e *env)
}

func secretFiles(e *env) {
	e.writeFile("app-key.pem", fakePEM, 0o600)
	e.writeFile("hook-secret", fakeHookSecret+"\n", 0o600)
}

var dryCases = []dryCase{
	{"check-config", "check-config.sh", nil, nil},
	{"bootstrap-gcp", "bootstrap-gcp.sh", nil, nil},
	{"build-runner-image", "build-runner-image.sh", []string{"--tag", "golden"}, nil},
	{"build-runner-image-local", "build-runner-image.sh", []string{"--tag", "golden", "--local"}, nil},
	{"deploy-job", "deploy-job.sh", []string{"--tag", "golden"}, nil},
	{"deploy-job-warm-up", "deploy-job.sh", []string{"--tag", "golden", "--warm-up"}, nil},
	{"deploy-mediator", "deploy-mediator.sh", []string{"--tag", "golden"}, nil},
	{"deploy-mediator-no-invoker-iam-check", "deploy-mediator.sh", []string{"--tag", "golden", "--no-invoker-iam-check"}, nil},
	{"deploy-scheduler", "deploy-scheduler.sh", nil, nil},
	{"set-secrets", "set-secrets.sh", []string{"--app-key-file", "app-key.pem", "--webhook-secret-file", "hook-secret"}, secretFiles},
	{"set-secrets-generate", "set-secrets.sh", []string{"--generate-webhook-secret", "new-webhook-secret"}, nil},
	{"teardown", "teardown.sh", nil, nil},
	{"teardown-keep", "teardown.sh", []string{"--keep-secrets", "--keep-images"}, nil},
	{"teardown-regional-only", "teardown.sh", []string{"--regional-only"}, nil},
}

// TestDryRunGolden: every script's dry-run output equals its committed
// transcript, and dry-run calls no tool at all (the stubs record nothing).
func TestDryRunGolden(t *testing.T) {
	for _, tc := range dryCases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, baseConfig())
			if tc.setup != nil {
				tc.setup(e)
			}
			r := e.run(tc.script, "", append([]string{"--config", "config.env"}, tc.args...)...)
			wantCode(t, r, 0)
			if len(r.calls) != 0 {
				t.Errorf("dry-run called tools: %v", callStrings(r.calls))
			}
			golden := filepath.Join("testdata", "golden", tc.name+".txt")
			if *update {
				if err := os.WriteFile(golden, []byte(r.stdout), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if r.stdout != string(want) {
				t.Errorf("dry-run transcript differs from %s:\n--- got\n%s\n--- want\n%s", golden, r.stdout, want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join("testdata", "golden", "set-secrets-generate.txt")); err == nil {
		// Dry-run must not create the generated-secret file.
		e := newEnv(t, baseConfig())
		e.run("set-secrets.sh", "", "--config", "config.env", "--generate-webhook-secret", "new-webhook-secret")
		if _, err := os.Stat(filepath.Join(e.dir, "new-webhook-secret")); err == nil {
			t.Error("dry-run created the generated-secret file")
		}
	}
}

// ---- apply with stubs ---------------------------------------------------------------

// TestApplyRunsExactlyTheDryRunCommands: with nothing existing, --apply runs
// the "?" checks and the "+" commands of the dry-run, in the same order.
func TestApplyRunsExactlyTheDryRunCommands(t *testing.T) {
	cases := []dryCase{
		{"bootstrap-gcp", "bootstrap-gcp.sh", nil, nil},
		{"build-runner-image", "build-runner-image.sh", []string{"--tag", "golden"}, nil},
		{"build-runner-image-local", "build-runner-image.sh", []string{"--tag", "golden", "--local"}, nil},
		{"deploy-job", "deploy-job.sh", []string{"--tag", "golden"}, func(e *env) {
			// apply refuses a missing image; dry-run only shows the check.
			e.state("exists", "us-central1-docker.pkg.dev/example-project/cr-runners/gh-runner:golden")
		}},
		{"deploy-job-warm-up", "deploy-job.sh", []string{"--tag", "golden", "--warm-up"}, func(e *env) {
			e.state("exists", "us-central1-docker.pkg.dev/example-project/cr-runners/gh-runner:golden")
		}},
		{"deploy-mediator", "deploy-mediator.sh", []string{"--tag", "golden"}, func(e *env) {
			e.state("exists", "cr-runner-mediator")
		}},
		{"deploy-scheduler", "deploy-scheduler.sh", nil, nil},
		{"set-secrets", "set-secrets.sh", []string{"--app-key-file", "app-key.pem", "--webhook-secret-file", "hook-secret"}, secretFiles},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, baseConfig())
			if tc.setup != nil {
				tc.setup(e)
			}
			args := append([]string{"--config", "config.env"}, tc.args...)
			dry := e.run(tc.script, "", args...)
			wantCode(t, dry, 0)
			app := e.run(tc.script, "", append(args, "--apply")...)
			wantCode(t, app, 0)
			// In apply the "?" lines are printed too; compare the calls with
			// the dry-run's "?" and "+" lines in transcript order.
			var want []string
			for _, l := range strings.Split(dry.stdout, "\n") {
				if strings.HasPrefix(l, "? ") || strings.HasPrefix(l, "+ ") {
					want = append(want, lines(l, l[:2])...)
				}
			}
			// deploy-job and deploy-mediator: existing resources take the
			// other branch in apply; compare against the apply transcript.
			if tc.setup != nil && tc.name != "set-secrets" {
				want = nil
				for _, l := range strings.Split(app.stdout, "\n") {
					if strings.HasPrefix(l, "? ") || strings.HasPrefix(l, "+ ") {
						want = append(want, lines(l, l[:2])...)
					}
				}
			}
			got := callStrings(nonVersion(app.calls))
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("apply calls differ from the transcript:\n--- calls\n%s\n--- transcript\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// allExisting is every resource ID the scripts describe.
var allExisting = []string{
	"cr-runner-build-sa@example-project.iam.gserviceaccount.com",
	"cr-runner-mediator-sa@example-project.iam.gserviceaccount.com",
	"cr-runner-scheduler-sa@example-project.iam.gserviceaccount.com",
	"cr-runner-exec-sa@example-project.iam.gserviceaccount.com",
	"cr-runners",
	"cr-runner-gh-app-private-key",
	"cr-runner-gh-webhook-secret",
	"us-central1-docker.pkg.dev/example-project/cr-runners/gh-runner:golden",
	"us-central1-docker.pkg.dev/example-project/cr-runners/cr-runner-mediator:golden",
	"cr-gh-runner-8c32g",
	"cr-runner-mediator",
	"cr-runner-reconciler",
	"gs://example-project-cr-ci-staging",
}

// TestIdempotentSecondRun: with everything in place, nothing is created or
// rebuilt; updates and (idempotent) bindings still run.
func TestIdempotentSecondRun(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.state("exists", allExisting...)
	e.extra = []string{"STUB_ENABLED_APIS=run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com " +
		"cloudscheduler.googleapis.com cloudbuild.googleapis.com iam.googleapis.com storage.googleapis.com"}
	for _, s := range []struct {
		script string
		args   []string
		want   []string // substrings that must appear in some call
	}{
		{"bootstrap-gcp.sh", nil, []string{"add-iam-policy-binding"}},
		{"build-runner-image.sh", []string{"--tag", "golden"}, nil},
		{"deploy-job.sh", []string{"--tag", "golden"}, []string{"run jobs update cr-gh-runner-8c32g", "--clear-volumes --clear-volume-mounts --add-volume=name=scratch,type=ephemeral-disk,size=10Gi --add-volume-mount=volume=scratch,mount-path=/home/runner"}},
		{"deploy-mediator.sh", []string{"--tag", "golden"}, []string{"run deploy cr-runner-mediator"}},
		{"deploy-scheduler.sh", nil, []string{"scheduler jobs update http cr-runner-reconciler"}},
	} {
		r := e.run(s.script, "", append([]string{"--config", "config.env", "--apply"}, s.args...)...)
		wantCode(t, r, 0)
		all := strings.Join(callStrings(r.calls), "\n")
		for _, bad := range []string{" create ", "builds submit", "docker build", "services enable"} {
			if strings.Contains(all, bad) {
				t.Errorf("%s re-created something (%q) on a second run:\n%s", s.script, bad, all)
			}
		}
		for _, w := range s.want {
			if !strings.Contains(all, w) {
				t.Errorf("%s: no call contains %q:\n%s", s.script, w, all)
			}
		}
	}
}

// TestPermissionDeniedStops: a denied check stops the script, names the
// failing step, and nothing after it runs.
func TestPermissionDeniedStops(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.state("deny", "cr-runner-mediator-sa@example-project.iam.gserviceaccount.com")
	r := e.run("bootstrap-gcp.sh", "", "--config", "config.env", "--apply")
	if r.code == 0 {
		t.Fatal("bootstrap succeeded despite a permission denial")
	}
	if !strings.Contains(r.stderr, "permission denied or other error") || !strings.Contains(r.stderr, "cr-runner-mediator-sa@") {
		t.Errorf("stderr does not name the denied step:\n%s", r.stderr)
	}
	all := callStrings(r.calls)
	last := all[len(all)-1]
	if !strings.Contains(last, "describe cr-runner-mediator-sa@") {
		t.Errorf("calls continued after the denial; last call: %s", last)
	}
	for _, c := range all {
		if strings.Contains(c, "create cr-runner-mediator-sa") {
			t.Error("created the account it could not check")
		}
	}
}

// ---- secrets ------------------------------------------------------------------------

// TestSecretsNeverLeak: secret values reach gcloud on stdin only, exactly as
// intended, and never appear in argv, output or the goldens.
func TestSecretsNeverLeak(t *testing.T) {
	e := newEnv(t, baseConfig())
	secretFiles(e)
	r := e.run("set-secrets.sh", "", "--config", "config.env", "--apply", "--app-key-file", "app-key.pem", "--webhook-secret-file", "hook-secret")
	wantCode(t, r, 0)
	var stdins []string
	for _, c := range r.calls {
		if c.has {
			stdins = append(stdins, c.stdin)
		}
	}
	if len(stdins) != 2 || stdins[0] != fakePEM || stdins[1] != fakeHookSecret {
		t.Fatalf("secret stdins = %q, want the PEM as is and the webhook secret without its newline", stdins)
	}

	// From stdin.
	r2 := e.run("set-secrets.sh", fakeHookSecret+"\r\n", "--config", "config.env", "--apply", "--webhook-secret-file", "-")
	wantCode(t, r2, 0)
	if len(r2.calls) < 2 || r2.calls[len(r2.calls)-1].stdin != fakeHookSecret {
		t.Errorf("stdin webhook secret not passed exactly: %+v", r2.calls)
	}

	// Generated.
	r3 := e.run("set-secrets.sh", "", "--config", "config.env", "--apply", "--generate-webhook-secret", "gen-secret")
	wantCode(t, r3, 0)
	gen, err := os.ReadFile(filepath.Join(e.dir, "gen-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).Match(gen) {
		t.Errorf("generated secret %q is not 64 hex chars without a newline", gen)
	}
	if fi, _ := os.Stat(filepath.Join(e.dir, "gen-secret")); fi.Mode().Perm() != 0o600 {
		t.Errorf("generated secret file mode %v, want 0600", fi.Mode().Perm())
	}
	last := r3.calls[len(r3.calls)-1]
	if last.stdin != string(gen) {
		t.Error("the added version differs from the generated file")
	}

	// Local image build: the access token goes to docker login on stdin.
	r4 := e.run("build-runner-image.sh", "", "--config", "config.env", "--apply", "--tag", "golden", "--local")
	wantCode(t, r4, 0)
	var login *call
	for i, c := range r4.calls {
		if c.argv[0] == "docker" && len(c.argv) > 1 && c.argv[1] == "login" {
			login = &r4.calls[i]
		}
	}
	if login == nil || strings.TrimSpace(login.stdin) != fakeToken {
		t.Errorf("docker login did not get the token on stdin: %+v", login)
	}

	secrets := []string{"FAKE-PEM-MARKER-7f3a91", fakeHookSecret, fakeToken, string(gen)}
	for i, rr := range []result{r, r2, r3, r4} {
		for _, s := range secrets {
			if strings.Contains(rr.stdout, s) || strings.Contains(rr.stderr, s) {
				t.Errorf("run %d: a secret value appears in the output", i)
			}
			for _, c := range rr.calls {
				if strings.Contains(strings.Join(c.argv, " "), s) {
					t.Errorf("run %d: a secret value appears in argv: %s", i, c)
				}
			}
		}
	}
	goldens, _ := filepath.Glob(filepath.Join("testdata", "golden", "*.txt"))
	for _, g := range goldens {
		b, _ := os.ReadFile(g)
		for _, s := range secrets[:3] {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contains a secret value", g)
			}
		}
	}
}

func TestSetSecretsRefusals(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.writeFile("loose.pem", fakePEM, 0o644)
	e.writeFile("not-a-key", "hello\n", 0o600)
	e.writeFile("exists", "x", 0o600)
	e.writeFile("empty-hook", "\n", 0o600)
	for name, args := range map[string][]string{
		"group-readable file": {"--app-key-file", "loose.pem"},
		"not a PEM":           {"--app-key-file", "not-a-key"},
		"generated exists":    {"--generate-webhook-secret", "exists"},
		"two stdins":          {"--app-key-file", "-", "--webhook-secret-file", "-"},
		"file and generate":   {"--webhook-secret-file", "empty-hook", "--generate-webhook-secret", "new"},
		"nothing to do":       nil,
	} {
		r := e.run("set-secrets.sh", "", append([]string{"--config", "config.env", "--apply"}, args...)...)
		if r.code == 0 {
			t.Errorf("%s: accepted", name)
		}
		for _, c := range r.calls {
			if strings.Contains(c.String(), "versions add") {
				t.Errorf("%s: added a version anyway", name)
			}
		}
	}
	r := e.run("set-secrets.sh", "", "--config", "config.env", "--apply", "--webhook-secret-file", "empty-hook")
	if r.code == 0 || !strings.Contains(r.stderr, "empty") {
		t.Errorf("empty webhook secret: code %d, stderr %q", r.code, r.stderr)
	}
}

// ---- mediator -----------------------------------------------------------------------

func TestMediatorDeploy(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.state("exists", allExisting...)
	r := e.run("deploy-mediator.sh", "", "--config", "config.env", "--apply", "--tag", "golden")
	wantCode(t, r, 0)
	var deploy string
	for _, c := range r.calls {
		if strings.Contains(c.String(), "run deploy ") {
			deploy = c.String()
		}
	}
	for _, w := range []string{"--max-instances=1 ", "--min-instances=0 ", "--allow-unauthenticated", "--service-account=cr-runner-mediator-sa@",
		"RECONCILE_SCHEDULE=*/5 * * * *;", "TARGET_REPOS=example-owner/example-repo,example-owner/other-repo;", "MEDIATOR_URL=" + mediatorURL + ";"} {
		if !strings.Contains(deploy, w) {
			t.Errorf("deploy call lacks %q:\n%s", w, deploy)
		}
	}

	// A service that does not end up at max-instances=1 fails the run.
	e.extra = []string{"STUB_MAXSCALE=3"}
	r = e.run("deploy-mediator.sh", "", "--config", "config.env", "--apply", "--tag", "golden")
	if r.code == 0 || !strings.Contains(r.stderr, "must be 1") {
		t.Errorf("max-instances 3 accepted: code %d, stderr %q", r.code, r.stderr)
	}

	// URL mismatch warns; empty MEDIATOR_URL tells the operator what to do.
	e.extra = []string{"STUB_URL=https://cr-runner-mediator-other-uc.a.run.app",
		`STUB_URLS=["https://cr-runner-mediator-other-uc.a.run.app"]`}
	r = e.run("deploy-mediator.sh", "", "--config", "config.env", "--apply", "--tag", "golden")
	wantCode(t, r, 0)
	if !strings.Contains(r.stderr, "WARNING: MEDIATOR_URL=") {
		t.Errorf("no warning for a URL mismatch:\n%s", r.stderr)
	}
	// MEDIATOR_URL may be any of the service's URLs (deterministic form).
	e.extra = []string{"STUB_URL=https://cr-runner-mediator-hash-uc.a.run.app",
		`STUB_URLS=["https://cr-runner-mediator-hash-uc.a.run.app","` + mediatorURL + `"]`}
	r = e.run("deploy-mediator.sh", "", "--config", "config.env", "--apply", "--tag", "golden")
	wantCode(t, r, 0)
	if strings.Contains(r.stderr, "WARNING: MEDIATOR_URL") {
		t.Errorf("warned although MEDIATOR_URL is one of the service URLs:\n%s", r.stderr)
	}
	cfg := baseConfig()
	delete(cfg, "MEDIATOR_URL")
	e.extra = nil
	e.config = e.writeConfig("config.env", cfg)
	r = e.run("deploy-mediator.sh", "", "--config", "config.env", "--apply", "--tag", "golden")
	wantCode(t, r, 0)
	if !strings.Contains(r.stdout, "set MEDIATOR_URL="+mediatorURL) {
		t.Errorf("first deploy does not say how to set MEDIATOR_URL:\n%s", r.stdout)
	}
	r = e.run("deploy-scheduler.sh", "", "--config", "config.env")
	if r.code == 0 || !strings.Contains(r.stderr, "MEDIATOR_URL is empty") {
		t.Errorf("scheduler deployed without MEDIATOR_URL: %d %q", r.code, r.stderr)
	}
}

// ---- IAM ------------------------------------------------------------------------------

// TestIAMBindingsMatchMatrix: the bindings the scripts make are exactly those
// of docs/permissions-matrix.md section 2.2 (C4, C6, C7, C10, C11, C13, C19), each
// on the resource the matrix names; EXEC_SA_NAME is never granted anything.
func TestIAMBindingsMatchMatrix(t *testing.T) {
	const (
		build     = "serviceAccount:cr-runner-build-sa@example-project.iam.gserviceaccount.com"
		mediator  = "serviceAccount:cr-runner-mediator-sa@example-project.iam.gserviceaccount.com"
		scheduler = "serviceAccount:cr-runner-scheduler-sa@example-project.iam.gserviceaccount.com"
		exec      = "cr-runner-exec-sa@example-project.iam.gserviceaccount.com"
	)
	want := []string{
		"artifacts repositories cr-runners " + build + " roles/artifactregistry.writer",               // C4
		"projects example-project " + build + " roles/logging.logWriter",                              // C7
		"secrets cr-runner-gh-app-private-key " + mediator + " roles/secretmanager.secretAccessor",    // C6
		"secrets cr-runner-gh-webhook-secret " + mediator + " roles/secretmanager.secretAccessor",     // C6
		"iam service-accounts " + exec + " " + mediator + " roles/iam.serviceAccountUser",             // C11
		"run jobs cr-gh-runner-8c32g " + mediator + " roles/run.developer",                            // C10
		"run services cr-runner-mediator " + scheduler + " roles/run.invoker",                         // C13
		"storage buckets gs://example-project-cr-ci-staging " + build + " roles/storage.objectViewer", // C19
	}
	var got []string
	for _, s := range []struct {
		script string
		args   []string
	}{{"bootstrap-gcp.sh", nil}, {"deploy-job.sh", []string{"--tag", "x"}}, {"deploy-mediator.sh", []string{"--tag", "x"}}, {"deploy-scheduler.sh", nil}, {"build-runner-image.sh", []string{"--tag", "x"}}} {
		e := newEnv(t, baseConfig())
		r := e.run(s.script, "", append([]string{"--config", "config.env"}, s.args...)...)
		wantCode(t, r, 0)
		for _, l := range lines(r.stdout, "+ ") {
			f := strings.Fields(l)
			for i, w := range f {
				if w != "add-iam-policy-binding" {
					continue
				}
				var member, role string
				for _, a := range f {
					if v, ok := strings.CutPrefix(a, "--member="); ok {
						member = v
					}
					if v, ok := strings.CutPrefix(a, "--role="); ok {
						role = v
					}
				}
				got = append(got, strings.Join(f[1:i], " ")+" "+f[i+1]+" "+member+" "+role)
			}
			if strings.Contains(l, "--member=serviceAccount:"+exec) {
				t.Errorf("EXEC_SA_NAME is granted something: %s", l)
			}
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("IAM bindings differ from the matrix:\n--- got\n%s\n--- want\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The EXEC_SA_NAME zero-roles check fails the run if it holds a role.
	e := newEnv(t, baseConfig())
	e.extra = []string{"STUB_EXEC_ROLES=roles/viewer"}
	r := e.run("bootstrap-gcp.sh", "", "--config", "config.env", "--apply")
	if r.code == 0 || !strings.Contains(r.stderr, "it must hold none") {
		t.Errorf("EXEC_SA_NAME with a role accepted: %d %q", r.code, r.stderr)
	}
}

// ---- config ----------------------------------------------------------------------------

func TestConfigParsing(t *testing.T) {
	e := newEnv(t, baseConfig())
	cfg := func(extra string) string {
		var b strings.Builder
		for k, v := range baseConfig() {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
		b.WriteString(extra)
		return e.writeFile("c.env", b.String(), 0o644)
	}
	check := func(extra string) result {
		cfg(extra)
		return e.run("check-config.sh", "", "--config", "c.env")
	}

	r := check("RECONCILE_SCHEDULE=0 */2 * * *\r\n  # indented comment\n\nRUNNER_LABEL=  my-label  \n")
	wantCode(t, r, 0)
	for _, w := range []string{"RECONCILE_SCHEDULE=0 */2 * * *\n", "RUNNER_LABEL=my-label\n", "RUNNER_CPU=8\n", "MEDIATOR_URL=" + mediatorURL + "\n"} {
		if !strings.Contains(r.stdout, w) {
			t.Errorf("resolved config lacks %q:\n%s", w, r.stdout)
		}
	}

	pwned := filepath.Join(e.dir, "pwned")
	for name, extra := range map[string]string{
		"unknown key":      "NOT_A_KEY=1\n",
		"duplicate key":    "GITHUB_APP_ID=7\n",
		"not KEY=value":    "export RUNNER_CPU=8\n",
		"quotes are kept":  "RUNNER_LABEL=\"quoted\"\n",
		"no substitution":  "RUNNER_LABEL=$(touch " + pwned + ")\n",
		"lowercase key":    "runner_cpu=8\n",
		"invalid value":    "RUNNER_CPU=3\n",
		"memory above cpu": "RUNNER_CPU=4\nRUNNER_MEMORY=24Gi\n",
	} {
		r := check(extra)
		if r.code == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("a config value was executed")
	}

	cfgNoApp := baseConfig()
	delete(cfgNoApp, "GITHUB_APP_ID")
	e.writeConfig("noapp.env", cfgNoApp)
	r = e.run("bootstrap-gcp.sh", "", "--config", "noapp.env")
	if r.code == 0 || !strings.Contains(r.stderr, "GITHUB_APP_ID: required") {
		t.Errorf("missing required key: %d %q", r.code, r.stderr)
	}
	r = e.run("bootstrap-gcp.sh", "", "--config", "missing.env")
	if r.code == 0 {
		t.Error("a missing config file was accepted")
	}
}

func TestToolVersionAndArgs(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.extra = []string{"STUB_GCLOUD_VERSION=400.0.0"}
	r := e.run("bootstrap-gcp.sh", "", "--config", "config.env", "--apply")
	if r.code == 0 || !strings.Contains(r.stderr, "older than the required") {
		t.Errorf("old gcloud accepted: %d %q", r.code, r.stderr)
	}
	for _, s := range []string{"bootstrap-gcp.sh", "build-runner-image.sh", "deploy-job.sh", "deploy-mediator.sh", "deploy-scheduler.sh", "set-secrets.sh", "teardown.sh", "check-config.sh"} {
		r := e.run(s, "", "--help")
		if r.code != 0 || !strings.Contains(r.stdout, "Usage: "+s) || !strings.Contains(r.stdout, "--apply") {
			t.Errorf("%s --help: %d %q", s, r.code, r.stdout)
		}
		if r := e.run(s, "", "--config", "config.env", "--bogus"); r.code == 0 {
			t.Errorf("%s accepted an unknown flag", s)
		}
	}
}

// ---- teardown ---------------------------------------------------------------------------

func TestTeardown(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.state("exists", allExisting...)
	for name, in := range map[string]string{"no answer": "", "wrong answer": "other-project\n"} {
		r := e.run("teardown.sh", in, "--config", "config.env", "--apply")
		if r.code == 0 {
			t.Errorf("%s: teardown ran", name)
		}
		for _, c := range r.calls {
			if strings.Contains(c.String(), " delete ") {
				t.Errorf("%s: deleted %s", name, c)
			}
		}
	}
	r := e.run("teardown.sh", project+"\n", "--config", "config.env", "--apply")
	wantCode(t, r, 0)
	var deletes []string
	for _, c := range r.calls {
		s := c.String()
		if strings.Contains(s, " delete ") || strings.Contains(s, "remove-iam-policy-binding") {
			deletes = append(deletes, strings.Join(c.argv[1:4], " "))
		} else if strings.Contains(s, "storage rm ") {
			deletes = append(deletes, strings.Join(c.argv[1:4], " "))
		}
	}
	want := []string{
		"scheduler jobs delete", "run services delete", "run jobs delete", "artifacts repositories delete",
		"storage rm --recursive",
		"secrets delete cr-runner-gh-app-private-key", "secrets delete cr-runner-gh-webhook-secret",
		"projects remove-iam-policy-binding example-project",
		"iam service-accounts delete", "iam service-accounts delete", "iam service-accounts delete", "iam service-accounts delete",
	}
	if strings.Join(deletes, "\n") != strings.Join(want, "\n") {
		t.Errorf("teardown order:\n--- got\n%s\n--- want\n%s", strings.Join(deletes, "\n"), strings.Join(want, "\n"))
	}

	// --confirm, and nothing left (bindings gone too): every delete is
	// skipped and the run still succeeds, with and without --keep-*.
	e.state("exists")
	e.extra = []string{"STUB_REMOVE_MISSING=1"}
	r = e.run("teardown.sh", "", "--config", "config.env", "--apply", "--confirm", project, "--keep-secrets", "--keep-images")
	wantCode(t, r, 0)
	r = e.run("teardown.sh", "", "--config", "config.env", "--apply", "--confirm", project)
	wantCode(t, r, 0)
	for _, c := range r.calls {
		if strings.Contains(c.String(), " delete ") {
			t.Errorf("deleted a missing resource: %s", c)
		}
	}
}

// TestTeardownRegionalOnly: only the stack in REGION goes; everything
// project-wide (service accounts, secrets, staging bucket, project binding)
// stays.
func TestTeardownRegionalOnly(t *testing.T) {
	e := newEnv(t, baseConfig())
	e.state("exists", allExisting...)
	r := e.run("teardown.sh", project+"\n", "--config", "config.env", "--apply", "--regional-only")
	wantCode(t, r, 0)
	if !strings.Contains(r.stderr, "the cr-ci stack in region us-central1 of project "+project) {
		t.Errorf("confirmation prompt does not name the region:\n%s", r.stderr)
	}
	var deletes []string
	for _, c := range r.calls {
		s := c.String()
		if strings.Contains(s, " delete ") || strings.Contains(s, "remove-iam-policy-binding") || strings.Contains(s, "storage rm ") {
			deletes = append(deletes, strings.Join(c.argv[1:4], " "))
		}
	}
	want := []string{"scheduler jobs delete", "run services delete", "run jobs delete", "artifacts repositories delete"}
	if strings.Join(deletes, "\n") != strings.Join(want, "\n") {
		t.Errorf("regional teardown:\n--- got\n%s\n--- want\n%s", strings.Join(deletes, "\n"), strings.Join(want, "\n"))
	}

	r = e.run("teardown.sh", "", "--config", "config.env", "--apply", "--confirm", project, "--regional-only", "--keep-images")
	wantCode(t, r, 0)
	for _, c := range r.calls {
		if s := c.String(); strings.Contains(s, "artifacts repositories delete") || strings.Contains(s, "remove-iam-policy-binding") {
			t.Errorf("--keep-images: %s", c)
		}
	}

	r = e.run("teardown.sh", "", "--config", "config.env", "--apply", "--confirm", project, "--regional-only", "--keep-secrets")
	if r.code == 0 || !strings.Contains(r.stderr, "--keep-secrets has no effect with --regional-only") {
		t.Errorf("--regional-only --keep-secrets: code %d, want refused; stderr:\n%s", r.code, r.stderr)
	}
	for _, c := range r.calls {
		if s := c.String(); strings.Contains(s, " delete ") || strings.Contains(s, " describe ") {
			t.Errorf("--regional-only --keep-secrets: called %s, want refused before any resource call", c)
		}
	}
}

// minimalGitEnv is the whole environment for the tests' own git calls: nothing
// is inherited, so no GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_CONFIG_* or
// hooks setting can point git at another repository (such as the real checkout)
// or change its behaviour. home holds the throwaway repository.
func minimalGitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(home),
	}
}

// ---- default image tag ------------------------------------------------------------------

// TestDefaultTag builds a throwaway git repo holding the module files the
// scripts need, so it does not depend on the checkout the tests run in.
func TestDefaultTag(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git is required")
	}
	root := t.TempDir()
	mod := filepath.Join(root, "cr-ci")
	for _, f := range []string{"config.example.env", "go.mod", "scripts/lib.sh", "scripts/build-runner-image.sh", "scripts/deploy-mediator.sh", "runner/Dockerfile", "mediator/Dockerfile"} {
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
	g := func(args ...string) string {
		cmd := exec.Command(git, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		cmd.Env = minimalGitEnv(root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	g("init", "-q")
	g("add", ".")
	g("commit", "-q", "-m", "test")
	tree := g("rev-parse", "HEAD:cr-ci/runner")

	e := newEnv(t, baseConfig())
	script := filepath.Join(mod, "scripts", "build-runner-image.sh")
	r := e.run(script, "", "--config", "config.env")
	wantCode(t, r, 0)
	if !strings.Contains(r.stdout, "gh-runner:git-"+tree[:12]) {
		t.Errorf("default tag is not git-%s:\n%s", tree[:12], r.stdout)
	}
	// Uncommitted runner changes are refused in apply mode.
	if err := os.WriteFile(filepath.Join(mod, "runner", "Dockerfile"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = e.run(script, "", "--config", "config.env", "--apply")
	if r.code == 0 || !strings.Contains(r.stderr, "uncommitted changes") {
		t.Errorf("dirty runner/ accepted: %d %q", r.code, r.stderr)
	}
	// The mediator tag covers mediator/ and go.mod.
	med := filepath.Join(mod, "scripts", "deploy-mediator.sh")
	r = e.run(med, "", "--config", "config.env")
	wantCode(t, r, 0)
	if !regexp.MustCompile(`cr-runner-mediator:git-[0-9a-f]{12} `).MatchString(r.stdout) {
		t.Errorf("mediator default tag missing:\n%s", r.stdout)
	}

	// Upgrade order: deploy-mediator notes when the job runs another runner image
	// than the current runner/ tree (the mediator goes first, the job next).
	e.state("exists", allExisting...)
	img := "us-central1-docker.pkg.dev/example-project/cr-runners/gh-runner:"
	e.extra = []string{"STUB_JOB_IMAGE=" + img + "git-" + tree[:12]}
	r = e.run(med, "", "--config", "config.env", "--apply", "--tag", "golden")
	wantCode(t, r, 0)
	if strings.Contains(r.stderr, "not the current runner tag") {
		t.Errorf("warned although the job runs the current runner image:\n%s", r.stderr)
	}
	e.extra = []string{"STUB_JOB_IMAGE=" + img + "git-000000000000"}
	r = e.run(med, "", "--config", "config.env", "--apply", "--tag", "golden")
	wantCode(t, r, 0)
	if !strings.Contains(r.stderr, "not the current runner tag git-"+tree[:12]) {
		t.Errorf("no warning for an older runner image on the job:\n%s", r.stderr)
	}
	e.state("exists", "cr-runner-mediator")
	r = e.run(med, "", "--config", "config.env", "--apply", "--tag", "golden")
	if !strings.Contains(r.stderr, "runner job cr-gh-runner-8c32g not found") {
		t.Errorf("no warning for a missing runner job:\n%s", r.stderr)
	}
}

// TestDeployMediatorMinInstances: MEDIATOR_MIN_INSTANCES reaches gcloud run
// deploy; max-instances stays 1.
func TestDeployMediatorMinInstances(t *testing.T) {
	cfg := baseConfig()
	cfg["MEDIATOR_MIN_INSTANCES"] = "1"
	e := newEnv(t, cfg)
	r := e.run("deploy-mediator.sh", "", "--config", "config.env", "--tag", "golden")
	wantCode(t, r, 0)
	if !strings.Contains(r.stdout, " --min-instances=1 --max-instances=1 ") {
		t.Errorf("dry-run lacks --min-instances=1 --max-instances=1:\n%s", r.stdout)
	}
}

// TestDeployJobDiskLimitHint: a RUNNER_DISK_GI above the region's limit fails at
// the job create, and the script says what the error means.
func TestDeployJobDiskLimitHint(t *testing.T) {
	cfg := baseConfig()
	cfg["RUNNER_DISK_GI"] = "25"
	e := newEnv(t, cfg)
	e.state("exists", allExisting...)
	e.extra = []string{"STUB_DISK_LIMIT_GI=23"}
	r := e.run("deploy-job.sh", "", "--config", "config.env", "--tag", "golden", "--apply")
	if r.code == 0 {
		t.Fatal("deploy-job.sh succeeded over the disk limit")
	}
	for _, want := range []string{
		"RUNNER_DISK_GI=25 is above Cloud Run's 10 GiB per-instance default",
		"N is the limit in us-central1: set RUNNER_DISK_GI to at most N",
	} {
		if !strings.Contains(r.stdout+r.stderr, want) {
			t.Errorf("output lacks %q:\nstdout:\n%s\nstderr:\n%s", want, r.stdout, r.stderr)
		}
	}
	cfg["RUNNER_DISK_GI"] = "23"
	e = newEnv(t, cfg)
	e.state("exists", allExisting...)
	e.extra = []string{"STUB_DISK_LIMIT_GI=23"}
	r = e.run("deploy-job.sh", "", "--config", "config.env", "--tag", "golden", "--apply")
	wantCode(t, r, 0)
}
