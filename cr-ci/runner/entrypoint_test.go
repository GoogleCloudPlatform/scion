// Package runner holds the offline test harness for entrypoint.sh. The tests
// run the real script with bash against a local fake claim server
// (httptest, loopback only) and a stub run.sh. They need bash, curl, jq and
// awk on PATH, and no network or GCP access.
package runner

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// stubRunSh records how it was started, then exits 0. It writes its args
// one per line, its PID, its working directory and selected env vars.
const stubRunSh = `#!/usr/bin/env bash
set -euo pipefail
out=${STUB_OUT:?}
printf '%s\n' "$@" > "$out/args"
echo "$$" > "$out/pid"
pwd > "$out/cwd"
{
  echo "RUNNER_WORKSPACE=${RUNNER_WORKSPACE:-}"
  echo "RUNNER_TEMP=${RUNNER_TEMP:-}"
  echo "TMPDIR=${TMPDIR:-}"
  echo "GOPATH=${GOPATH:-}"
  echo "GOCACHE=${GOCACHE:-}"
  echo "GOMODCACHE=${GOMODCACHE:-}"
  echo "GOLANGCI_LINT_CACHE=${GOLANGCI_LINT_CACHE:-}"
  echo "TOKEN_SET=${RUNNER_CLAIM_TOKEN+yes}"
} > "$out/env"
echo "stub run.sh started"
`

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "jq", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("entrypoint harness needs %s on PATH: %v", tool, err)
		}
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// fakeJITConfig returns a throwaway value shaped like an encoded JIT config.
func fakeJITConfig(t *testing.T) string {
	t.Helper()
	b := make([]byte, 600)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

type claimRequest struct {
	method, path, auth string
	bodyLen            int
}

// claimServer replies with the scripted responses in order and repeats the
// last one when the script runs out.
type claimServer struct {
	*httptest.Server
	mu       sync.Mutex
	script   []func(http.ResponseWriter)
	requests []claimRequest
}

func newClaimServer(t *testing.T, script ...func(http.ResponseWriter)) *claimServer {
	t.Helper()
	cs := &claimServer{script: script}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		n := len(cs.requests)
		cs.requests = append(cs.requests, claimRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), len(body)})
		cs.mu.Unlock()
		cs.script[min(n, len(cs.script)-1)](w)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *claimServer) recorded() []claimRequest {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]claimRequest(nil), cs.requests...)
}

func respondJIT(cfg string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `{"jit_config":%q}`, cfg)
	}
}

func respondStatus(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { http.Error(w, http.StatusText(code), code) }
}

func respondRaw(contentType, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, body)
	}
}

// harness is one prepared entrypoint run: a fake scratch mount, a mount
// table that lists it, a runner dir with the stub run.sh, and the env.
type harness struct {
	t                  *testing.T
	scratch, mountinfo string
	runnerDir, stubOut string
	token, jit         string
	env                map[string]string
}

func newHarness(t *testing.T, mediatorURL string) *harness {
	t.Helper()
	requireTools(t)
	root := t.TempDir()
	h := &harness{
		t:         t,
		scratch:   filepath.Join(root, "scratch"),
		mountinfo: filepath.Join(root, "mountinfo"),
		runnerDir: filepath.Join(root, "actions-runner"),
		stubOut:   filepath.Join(root, "stub-out"),
		token:     randomHex(t, 16),
		jit:       fakeJITConfig(t),
	}
	for _, d := range []string{h.scratch, h.runnerDir, h.stubOut} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.writeMountinfo("ext4")
	// A hostile ~/.curlrc: without curl -q this prints the request headers,
	// bearer token included, and the leak assertions in run() fail.
	if err := os.WriteFile(filepath.Join(root, ".curlrc"), []byte("verbose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.runnerDir, "run.sh"), []byte(stubRunSh), 0o755); err != nil {
		t.Fatal(err)
	}
	h.env = map[string]string{
		"PATH":                     os.Getenv("PATH"),
		"HOME":                     root,
		"RUNNER_CLAIM_TOKEN":       h.token,
		"MEDIATOR_URL":             mediatorURL,
		"GH_JOB_ID":                "4242",
		"RUNNER_NAME":              "cr-job-4242",
		"CR_SCRATCH_DIR":           h.scratch,
		"CR_MOUNTINFO":             h.mountinfo,
		"CR_RUNNER_DIR":            h.runnerDir,
		"CR_CLAIM_BACKOFF_SECONDS": "0",
		"STUB_OUT":                 h.stubOut,
	}
	return h
}

// stubCommand puts an executable named name with the given script first on
// the entrypoint's PATH.
func (h *harness) stubCommand(name, script string) {
	h.t.Helper()
	bin := filepath.Join(filepath.Dir(h.scratch), "stub-bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if !strings.HasPrefix(h.env["PATH"], bin+":") {
		h.env["PATH"] = bin + ":" + h.env["PATH"]
	}
}

func (h *harness) writeMountinfo(fstype string) {
	h.t.Helper()
	content := "22 1 0:21 / / rw,relatime - overlay overlay rw\n" +
		fmt.Sprintf("36 22 8:16 / %s rw,relatime shared:7 - %s /dev/sdb rw\n", h.scratch, fstype)
	if err := os.WriteFile(h.mountinfo, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

type result struct {
	exitCode       int
	pid            int
	stdout, stderr string
}

func (h *harness) run() result {
	h.t.Helper()
	cmd := exec.Command("bash", "entrypoint.sh")
	cmd.Env = make([]string, 0, len(h.env))
	for k, v := range h.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := result{pid: cmd.Process.Pid, stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.exitCode = exitErr.ExitCode()
	default:
		h.t.Fatalf("running entrypoint.sh: %v", err)
	}
	// The secrets must never reach the logs, whatever the outcome.
	for name, secret := range map[string]string{"claim token": h.token, "JIT config": h.jit} {
		if strings.Contains(res.stdout, secret) || strings.Contains(res.stderr, secret) {
			h.t.Errorf("%s leaked into entrypoint output", name)
		}
	}
	return res
}

func (h *harness) stubStarted() bool {
	_, err := os.Stat(filepath.Join(h.stubOut, "args"))
	return err == nil
}

func (h *harness) stubFile(name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.stubOut, name))
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func expectFailure(t *testing.T, h *harness, res result, wantCode int, wantMsg string) {
	t.Helper()
	if res.exitCode != wantCode {
		t.Errorf("exit code = %d, want %d; stderr:\n%s", res.exitCode, wantCode, res.stderr)
	}
	if h.stubStarted() {
		t.Error("run.sh was started, want not started")
	}
	if !strings.Contains(res.stderr, wantMsg) {
		t.Errorf("stderr does not mention %q:\n%s", wantMsg, res.stderr)
	}
}

func TestEntrypointSuccess(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)

	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if got, want := h.stubFile("args"), "--jitconfig\n"+h.jit; got != want {
		t.Errorf("run.sh args = %q, want --jitconfig and the claimed config", got)
	}
	if got := h.stubFile("pid"); got != strconv.Itoa(res.pid) {
		t.Errorf("run.sh pid = %s, entrypoint pid = %d: run.sh was not exec'd in place", got, res.pid)
	}
	if got := h.stubFile("cwd"); got != h.runnerDir {
		t.Errorf("run.sh cwd = %s, want %s", got, h.runnerDir)
	}
	if !strings.Contains(res.stdout, "stub run.sh started") {
		t.Errorf("stdout lacks the stub's output: %q", res.stdout)
	}

	env := h.stubFile("env")
	for _, want := range []string{
		"RUNNER_WORKSPACE=" + h.scratch + "/_work",
		"RUNNER_TEMP=" + h.scratch + "/_temp",
		"TMPDIR=" + h.scratch + "/_temp",
		"GOPATH=" + h.scratch + "/gopath",
		"GOCACHE=" + h.scratch + "/gocache",
		"GOMODCACHE=" + h.scratch + "/gomodcache",
		"GOLANGCI_LINT_CACHE=" + h.scratch + "/golangci-lint-cache",
		"TOKEN_SET=",
	} {
		if !strings.Contains(env+"\n", want+"\n") {
			t.Errorf("run.sh env lacks %q; got:\n%s", want, env)
		}
	}
	for _, d := range []string{"_work", "_temp", "_diag", "gopath", "gocache", "gomodcache", "golangci-lint-cache"} {
		if fi, err := os.Stat(filepath.Join(h.scratch, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s/%s was not created: %v", h.scratch, d, err)
		}
	}

	reqs := cs.recorded()
	if len(reqs) != 1 {
		t.Fatalf("claim requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost || r.path != "/internal/claim" || r.bodyLen != 0 {
		t.Errorf("claim request = %s %s (body %d bytes), want POST /internal/claim with empty body", r.method, r.path, r.bodyLen)
	}
	if r.auth != "Bearer "+h.token {
		t.Error("claim request did not carry Authorization: Bearer <RUNNER_CLAIM_TOKEN>")
	}
}

func TestEntrypointTrailingSlashURL(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL+"/")
	if res := h.run(); res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if reqs := cs.recorded(); len(reqs) != 1 || reqs[0].path != "/internal/claim" {
		t.Errorf("requests = %+v, want one to /internal/claim", reqs)
	}
}

func TestEntrypointClaimGone(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusGone))
	h := newHarness(t, cs.URL)
	res := h.run()
	expectFailure(t, h, res, 3, "410")
	if n := len(cs.recorded()); n != 1 {
		t.Errorf("claim requests = %d, want 1 (410 is not retried)", n)
	}
}

func TestEntrypointRetryThenSuccess(t *testing.T) {
	var h *harness
	cs := newClaimServer(t,
		respondStatus(http.StatusServiceUnavailable),
		respondStatus(http.StatusInternalServerError),
		func(w http.ResponseWriter) { respondJIT(h.jit)(w) },
	)
	h = newHarness(t, cs.URL)
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if n := len(cs.recorded()); n != 3 {
		t.Errorf("claim requests = %d, want 3", n)
	}
	if got := h.stubFile("args"); got != "--jitconfig\n"+h.jit {
		t.Error("run.sh did not receive the claimed config")
	}
}

func TestEntrypointRetriesExhausted(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusBadGateway))
	h := newHarness(t, cs.URL)
	res := h.run()
	expectFailure(t, h, res, 3, "after 5 attempts")
	if n := len(cs.recorded()); n != 5 {
		t.Errorf("claim requests = %d, want 5", n)
	}
}

func TestEntrypointConnectionErrorsExhausted(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusOK))
	url := cs.URL
	cs.Close() // nothing listens on the port any more
	h := newHarness(t, url)
	res := h.run()
	expectFailure(t, h, res, 3, "after 5 attempts (last status 000)")
}

func TestEntrypointNonRetryableResponses(t *testing.T) {
	cases := []struct {
		name    string
		respond func(http.ResponseWriter)
		wantMsg string
	}{
		{"method not allowed", respondStatus(http.StatusMethodNotAllowed), "unexpected status 405"},
		{"wrong content type", respondRaw("text/plain", `{"jit_config":"x"}`), "Content-Type"},
		{"invalid json", respondRaw("application/json", `not json`), "without a non-empty jit_config"},
		{"missing field", respondRaw("application/json", `{}`), "without a non-empty jit_config"},
		{"empty field", respondRaw("application/json", `{"jit_config":""}`), "without a non-empty jit_config"},
		{"non-string field", respondRaw("application/json", `{"jit_config":42}`), "without a non-empty jit_config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newClaimServer(t, tc.respond)
			h := newHarness(t, cs.URL)
			res := h.run()
			expectFailure(t, h, res, 3, tc.wantMsg)
			if n := len(cs.recorded()); n != 1 {
				t.Errorf("claim requests = %d, want 1 (not retried)", n)
			}
		})
	}
}

func TestEntrypointScratchChecks(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(h *harness)
		wantMsg string
	}{
		{"missing", func(h *harness) { os.RemoveAll(h.scratch) }, "does not exist"},
		{"not a mount point", func(h *harness) {
			if err := os.WriteFile(h.mountinfo, []byte("22 1 0:21 / / rw - overlay overlay rw\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "is not a mount point"},
		{"mount table unreadable, same device as parent", func(h *harness) { h.env["CR_MOUNTINFO"] = filepath.Join(h.scratch, "no-such-file") }, "is not a mount point"},
		{"not writable", func(h *harness) {
			if os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			if err := os.Chmod(h.scratch, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(h.scratch, 0o755) })
		}, "not writable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newClaimServer(t, respondStatus(http.StatusOK))
			h := newHarness(t, cs.URL)
			tc.mutate(h)
			res := h.run()
			expectFailure(t, h, res, 2, tc.wantMsg)
			if n := len(cs.recorded()); n != 0 {
				t.Errorf("claim requests = %d, want 0 (token must not be spent)", n)
			}
		})
	}
}

func TestEntrypointTmpfsScratchWarns(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	h.writeMountinfo("tmpfs")
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "is tmpfs") {
		t.Errorf("no tmpfs warning in stderr:\n%s", res.stderr)
	}
}

func TestEntrypointConfigErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(h *harness)
		wantMsg string
	}{
		{"missing RUNNER_CLAIM_TOKEN", func(h *harness) { delete(h.env, "RUNNER_CLAIM_TOKEN") }, "RUNNER_CLAIM_TOKEN is not set"},
		{"missing MEDIATOR_URL", func(h *harness) { delete(h.env, "MEDIATOR_URL") }, "MEDIATOR_URL is not set"},
		{"missing GH_JOB_ID", func(h *harness) { delete(h.env, "GH_JOB_ID") }, "GH_JOB_ID is not set"},
		{"missing RUNNER_NAME", func(h *harness) { delete(h.env, "RUNNER_NAME") }, "RUNNER_NAME is not set"},
		{"empty RUNNER_CLAIM_TOKEN", func(h *harness) { h.env["RUNNER_CLAIM_TOKEN"] = "" }, "RUNNER_CLAIM_TOKEN is not set"},
		{"short token", func(h *harness) { h.env["RUNNER_CLAIM_TOKEN"] = h.token[:31] }, "not 32 lowercase hex"},
		{"uppercase token", func(h *harness) { h.env["RUNNER_CLAIM_TOKEN"] = strings.ToUpper(h.token) }, "not 32 lowercase hex"},
		{"non-numeric job id", func(h *harness) { h.env["GH_JOB_ID"] = "42x" }, "GH_JOB_ID must be numeric"},
		{"http to a remote host", func(h *harness) { h.env["MEDIATOR_URL"] = "http://mediator.example.com" }, "must be an https origin"},
		{"url with a path", func(h *harness) { h.env["MEDIATOR_URL"] = "https://mediator.example.com/x" }, "must be an https origin"},
		{"runner dir without run.sh", func(h *harness) { os.Remove(filepath.Join(h.runnerDir, "run.sh")) }, "run.sh not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newClaimServer(t, respondStatus(http.StatusOK))
			h := newHarness(t, cs.URL)
			tc.mutate(h)
			res := h.run()
			expectFailure(t, h, res, 2, tc.wantMsg)
			if n := len(cs.recorded()); n != 0 {
				t.Errorf("claim requests = %d, want 0 (token must not be spent)", n)
			}
		})
	}
}

// stubStatFakeMount makes `stat` report a device of its own (and an ext4
// filesystem type) for the scratch dir, as for a real mount point, and
// defers to the real stat for every other path.
func (h *harness) stubStatFakeMount() {
	h.t.Helper()
	realStat, err := exec.LookPath("stat")
	if err != nil {
		h.t.Fatal(err)
	}
	h.stubCommand("stat", fmt.Sprintf(`#!/usr/bin/env bash
if [[ ${!#} == %q ]]; then
  if [[ " $* " == *" -f "* ]]; then echo ext2/ext3; else echo 999999; fi
  exit 0
fi
exec %q "$@"
`, h.scratch, realStat))
}

func TestEntrypointScratchDeviceFallback(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(h *harness)
	}{
		{"not in mount table", func(h *harness) {
			if err := os.WriteFile(h.mountinfo, []byte("22 1 0:21 / / rw - overlay overlay rw\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"mount table unreadable", func(h *harness) { h.env["CR_MOUNTINFO"] = filepath.Join(h.scratch, "no-such-file") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h *harness
			cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
			h = newHarness(t, cs.URL)
			tc.mutate(h)
			h.stubStatFakeMount()
			res := h.run()
			if res.exitCode != 0 {
				t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
			}
			if !strings.Contains(res.stderr, "device 999999 differs from parent device") {
				t.Errorf("stderr does not report the device fallback:\n%s", res.stderr)
			}
			if !h.stubStarted() {
				t.Error("run.sh was not started")
			}
		})
	}
}

func TestEntrypointScratchMountTableLogged(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	res := h.run()
	if want := h.scratch + " is a mount (ext4): listed in " + h.mountinfo; !strings.Contains(res.stderr, want) {
		t.Errorf("stderr lacks %q:\n%s", want, res.stderr)
	}
}

func TestEntrypointBackoffDoubling(t *testing.T) {
	cases := []struct {
		name, base string // base "" leaves CR_CLAIM_BACKOFF_SECONDS unset (default 1)
		want       string
	}{
		{"default", "", "1\n2\n4\n8"},
		{"fractional base", "0.5", "0.5\n1\n2\n4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newClaimServer(t, respondStatus(http.StatusServiceUnavailable))
			h := newHarness(t, cs.URL)
			if tc.base == "" {
				delete(h.env, "CR_CLAIM_BACKOFF_SECONDS")
			} else {
				h.env["CR_CLAIM_BACKOFF_SECONDS"] = tc.base
			}
			h.stubCommand("sleep", "#!/usr/bin/env bash\necho \"$*\" >> \"$STUB_OUT/sleeps\"\n")
			res := h.run()
			expectFailure(t, h, res, 3, "after 5 attempts")
			if got := h.stubFile("sleeps"); got != tc.want {
				t.Errorf("sleep calls = %q, want %q", got, tc.want)
			}
		})
	}
}
