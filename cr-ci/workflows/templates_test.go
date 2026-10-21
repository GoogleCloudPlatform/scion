package workflows

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// routingExpr is the one runs-on expression that routes CI jobs to cr-ci. It
// must appear byte for byte in the snippet and in every doc that shows it.
const routingExpr = "${{ (github.event_name == 'push' || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)) && vars.SCION_LARGE_RUNNER || 'ubuntu-latest' }}"

const (
	forkGuard = "github.event.pull_request.head.repo.full_name == github.repository"
	fallback  = "|| 'ubuntu-latest' }}"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lines(s string) []string { return strings.Split(s, "\n") }

// configDefault returns KEY's value in ../config.example.env.
func configDefault(t *testing.T, key string) string {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(readFile(t, "../config.example.env")))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && k == key {
			return v
		}
	}
	t.Fatalf("%s not found in config.example.env", key)
	return ""
}

func TestRoutingExpressionShape(t *testing.T) {
	for _, want := range []string{
		forkGuard,
		"github.event_name == 'push'",
		"github.event_name == 'pull_request' && " + forkGuard,
		"&& vars." + configDefault(t, "ROUTING_VARIABLE_NAME") + " ",
	} {
		if !strings.Contains(routingExpr, want) {
			t.Errorf("routing expression lacks %q", want)
		}
	}
	if !strings.HasSuffix(routingExpr, fallback) {
		t.Errorf("routing expression must end with the fallback %q", fallback)
	}
	// The allow-list must not route the events the mediator refuses.
	for _, banned := range []string{"pull_request_target", "workflow_run", "!="} {
		if strings.Contains(routingExpr, banned) {
			t.Errorf("routing expression contains %q", banned)
		}
	}
}

// TestRoutingExpressionEverywhere keeps the snippet and the docs in step: every
// line that shows a routing runs-on carries exactly routingExpr.
func TestRoutingExpressionEverywhere(t *testing.T) {
	files := []string{
		"ci-lint-routing.snippet.yml",
		"README.md",
		"../docs/architecture.md",
		"../docs/github-setup.md",
	}
	for _, f := range files {
		found := 0
		for i, l := range lines(readFile(t, f)) {
			if !strings.Contains(l, "runs-on:") || !strings.Contains(l, "vars.") {
				continue
			}
			found++
			if got := strings.TrimSpace(l); got != "runs-on: "+routingExpr {
				t.Errorf("%s:%d: routing runs-on differs from the canonical expression:\n got %s\nwant runs-on: %s", f, i+1, got, routingExpr)
			}
		}
		if found == 0 {
			t.Errorf("%s: no routing runs-on line found", f)
		}
	}
}

func TestSnippetLintEnv(t *testing.T) {
	s := readFile(t, "ci-lint-routing.snippet.yml")
	for i, l := range lines(s) {
		if strings.Contains(l, "-c=1") && !strings.HasPrefix(strings.TrimSpace(l), "#") &&
			!strings.Contains(l, "runner.environment != 'self-hosted' && ") {
			t.Errorf("snippet:%d: the -c=1 throttle must apply only off the self-hosted runner: %s", i+1, l)
		}
	}
	for _, want := range []string{
		"GOFLAGS: ${{ runner.environment != 'self-hosted' && '-gcflags=github.com/GoogleCloudPlatform/scion/pkg/hub=-c=1' || '' }}",
		`--concurrency="$LINT_CONCURRENCY"`,
		"LINT_CONCURRENCY: ${{ runner.environment == 'self-hosted' && '4' || '2' }}",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("snippet lacks %q", want)
		}
	}
}

func TestPoCWorkflow(t *testing.T) {
	s := readFile(t, "cr-runner-poc.yml")
	ls := lines(s)

	// Triggers: the only key under the top-level on: is workflow_dispatch.
	var events []string
	inOn := false
	for _, l := range ls {
		switch {
		case l == "on:":
			inOn = true
		case inOn && l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#"):
			inOn = false
		case inOn && regexp.MustCompile(`^  [a-z_]+:`).MatchString(l):
			events = append(events, strings.TrimSuffix(strings.TrimSpace(l), ":"))
		}
	}
	if !reflect.DeepEqual(events, []string{"workflow_dispatch"}) {
		t.Errorf("PoC triggers = %v, want [workflow_dispatch]", events)
	}

	var runsOn []string
	for _, l := range ls {
		if strings.HasPrefix(strings.TrimSpace(l), "runs-on:") {
			runsOn = append(runsOn, strings.TrimSpace(l))
		}
	}
	if want := []string{"runs-on: ${{ inputs.runner-label }}"}; !reflect.DeepEqual(runsOn, want) {
		t.Errorf("PoC runs-on = %v, want %v (the label alone)", runsOn, want)
	}

	// The runner-label input's default equals RUNNER_LABEL's default.
	label := configDefault(t, "RUNNER_LABEL")
	m := regexp.MustCompile(`(?m)^      runner-label:\n(?:        .*\n)*?        default: (\S+)$`).FindStringSubmatch(s)
	if m == nil {
		t.Fatal("PoC: no default for the runner-label input")
	}
	if m[1] != label {
		t.Errorf("PoC runner-label default = %q, want RUNNER_LABEL default %q", m[1], label)
	}

	for _, want := range []string{"/sys/fs/cgroup/memory.peak", "df -h /scratch", "golangci-lint run "} {
		if !strings.Contains(s, want) {
			t.Errorf("PoC lacks %q", want)
		}
	}
	for _, banned := range []string{"-c=1", "self-hosted,", "pull_request", "secrets."} {
		for i, l := range ls {
			if strings.Contains(l, banned) && !strings.HasPrefix(strings.TrimSpace(l), "#") {
				t.Errorf("cr-runner-poc.yml:%d: contains %q: %s", i+1, banned, l)
			}
		}
	}
}

// TestHomeCachesOnScratch: on the cr-ci runner, $HOME is in memory, so both
// templates link ~/.cache to /scratch in their first step, and only there.
func TestHomeCachesOnScratch(t *testing.T) {
	for _, f := range []string{"cr-runner-poc.yml", "ci-lint-routing.snippet.yml"} {
		s := readFile(t, f)
		first := regexp.MustCompile(`(?m)^ *steps:\n *- name: (.*)\n *if: (.*)$`).FindStringSubmatch(s)
		if first == nil || first[1] != "Keep caches under HOME on /scratch (cr-ci runner only)" ||
			first[2] != "runner.environment == 'self-hosted'" {
			t.Errorf("%s: the first step must be the self-hosted-only ~/.cache link step, got %q", f, first)
		}
		for _, want := range []string{
			`ln -sfn /scratch/cache "$HOME/.cache"`,
			`[ ! -L "$HOME/.cache" ]`,
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q", f, want)
			}
		}
		// Linking the cache dir itself would be replaced by tar on restore.
		if regexp.MustCompile(`ln -sfn? \S+ "\$HOME/\.cache/golangci-lint"`).MatchString(s) {
			t.Errorf("%s links ~/.cache/golangci-lint itself; link the parent", f)
		}
	}
}

var pinnedUses = regexp.MustCompile(`^uses: (\./\S+|[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40})( #.*)?$`)

func TestTemplatesHygiene(t *testing.T) {
	files, err := filepath.Glob("*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, f := range files {
		for i, l := range lines(readFile(t, f)) {
			if strings.Contains(l, "\t") {
				t.Errorf("%s:%d: tab character (YAML forbids tabs for indentation)", f, i+1)
			}
			tl := strings.TrimPrefix(strings.TrimSpace(l), "- ")
			if strings.HasPrefix(tl, "uses:") && !pinnedUses.MatchString(tl) {
				t.Errorf("%s:%d: action not pinned to a full commit SHA: %s", f, i+1, tl)
			}
		}
	}
}

// TestManifestMatchesMatrix: the App manifest requests exactly the matrix's
// repo-scope permission set and event, no more and no less.
func TestManifestMatchesMatrix(t *testing.T) {
	var m struct {
		HookAttributes struct {
			URL    string `json:"url"`
			Active *bool  `json:"active"`
		} `json:"hook_attributes"`
		RedirectURL           string            `json:"redirect_url"`
		RequestOAuthOnInstall *bool             `json:"request_oauth_on_install"`
		DefaultPermissions    map[string]string `json:"default_permissions"`
		DefaultEvents         []string          `json:"default_events"`
	}
	dec := json.NewDecoder(strings.NewReader(readFile(t, "../scripts/github-app-manifest.json")))
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}

	wantPerms := map[string]string{"actions": "read", "administration": "write", "metadata": "read"}
	if !reflect.DeepEqual(m.DefaultPermissions, wantPerms) {
		t.Errorf("manifest permissions = %v, want exactly %v", m.DefaultPermissions, wantPerms)
	}
	if !reflect.DeepEqual(m.DefaultEvents, []string{"workflow_job"}) {
		t.Errorf("manifest events = %v, want [workflow_job]", m.DefaultEvents)
	}
	if m.HookAttributes.Active == nil || !*m.HookAttributes.Active || !strings.HasSuffix(m.HookAttributes.URL, "/webhook") {
		t.Errorf("manifest webhook must be active and end in /webhook: %+v", m.HookAttributes)
	}
	if m.RequestOAuthOnInstall == nil || *m.RequestOAuthOnInstall {
		t.Error("manifest must set request_oauth_on_install to false")
	}
	// The manifest code arrives in the redirect; it must never leave the
	// operator's machine.
	if u, err := url.Parse(m.RedirectURL); err != nil || u.Hostname() != "127.0.0.1" {
		t.Errorf("manifest redirect_url %q must point at 127.0.0.1", m.RedirectURL)
	}

	// The matrix still states the same set (whitespace-normalised).
	matrix := strings.Join(strings.Fields(readFile(t, "../docs/permissions-matrix.md")), " ")
	const want = "- **`repo`:** Repository *Actions*: read; Repository *Administration*: read and write; " +
		"Repository *Metadata*: read (mandatory). No organization permissions. Subscribed event: `workflow_job`."
	if !strings.Contains(matrix, want) {
		t.Errorf("permissions-matrix.md no longer states the repo-scope set the manifest encodes:\n%s", want)
	}
}
