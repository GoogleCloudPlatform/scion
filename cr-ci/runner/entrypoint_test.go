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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stubRunSh records how it was started, then exits 0. It writes its args
// one per line, its PID and parent PID, its SIGINT disposition, its working
// directory and selected env vars. Tests append to it (stubRunShWith) to
// change how it ends.
const stubRunSh = `#!/usr/bin/env bash
set -euo pipefail
out=${STUB_OUT:?}
printf '%s\n' "$@" > "$out/args"
echo "$$" > "$out/pid"
echo "$PPID" > "$out/ppid"
trap -p INT > "$out/int_trap"
ls -l /proc/$$/fd > "$out/fds_long" 2>/dev/null || true
pwd > "$out/cwd"
{
  echo "RUNNER_WORKSPACE=${RUNNER_WORKSPACE:-}"
  echo "RUNNER_TEMP=${RUNNER_TEMP:-}"
  echo "TMPDIR=${TMPDIR:-}"
  echo "GOPATH=${GOPATH:-}"
  echo "GOCACHE=${GOCACHE:-}"
  echo "GOMODCACHE=${GOMODCACHE:-}"
  echo "GOLANGCI_LINT_CACHE=${GOLANGCI_LINT_CACHE:-}"
  echo "XDG_CACHE_HOME_SET=${XDG_CACHE_HOME+yes}"
  echo "TOKEN_SET=${RUNNER_CLAIM_TOKEN+yes}"
  echo "ACTIONS_RUNNER_HOOK_JOB_STARTED=${ACTIONS_RUNNER_HOOK_JOB_STARTED:-}"
  echo "CR_JOB_GUARD_REPOS=${CR_JOB_GUARD_REPOS:-}"
  echo "CR_TEST_MODE_SET=${CR_TEST_MODE+yes}"
} > "$out/env"
echo "stub run.sh started"
`

// stubRunShWith returns the stub run.sh with tail appended after its recording.
func stubRunShWith(tail string) string { return stubRunSh + tail }

// setRunSh replaces the harness's run.sh.
func (h *harness) setRunSh(script string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.runnerDir, "run.sh"), []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "jq", "awk", "tar", "sha256sum", "stat"} {
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
	claim, flavor      string // X-Runner-Claim, Metadata-Flavor
	query              url.Values
	bodyLen            int
}

// claimServer replies with the scripted responses in order and repeats the
// last one when the script runs out. It also serves as the fake metadata
// server (see newMetadataServer).
type claimServer struct {
	*httptest.Server
	mu       sync.Mutex
	script   []func(http.ResponseWriter)
	requests []claimRequest
	// guard, if set, can answer a request itself (returning true) before the
	// script is consulted, as the real metadata server does for bad requests.
	guard func(http.ResponseWriter, *http.Request) bool
}

func newClaimServer(t *testing.T, script ...func(http.ResponseWriter)) *claimServer {
	t.Helper()
	cs := &claimServer{script: script}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		n := len(cs.requests)
		cs.requests = append(cs.requests, claimRequest{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			claim: r.Header.Get("X-Runner-Claim"), flavor: r.Header.Get("Metadata-Flavor"),
			query: r.URL.Query(), bodyLen: len(body),
		})
		script, guard := cs.script, cs.guard
		cs.mu.Unlock()
		if guard != nil && guard(w, r) {
			return
		}
		script[min(n, len(script)-1)](w)
	}))
	t.Cleanup(cs.Close)
	return cs
}

const identityPath = "/computeMetadata/v1/instance/service-accounts/default/identity"

// newMetadataServer fakes the GCE/Cloud Run metadata server's identity
// endpoint. Like the real one, it refuses requests without
// "Metadata-Flavor: Google" (403); it also refuses a wrong path, a wrong
// audience or a missing format=full (400). Otherwise it plays the script.
func newMetadataServer(t *testing.T, audience string, script ...func(http.ResponseWriter)) *claimServer {
	t.Helper()
	ms := newClaimServer(t, script...)
	ms.guard = func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		switch {
		case r.Header.Get("Metadata-Flavor") != "Google":
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
		case r.Method != http.MethodGet || r.URL.Path != identityPath:
			http.Error(w, "not found", http.StatusNotFound)
		case q.Get("audience") != audience || q.Get("format") != "full":
			http.Error(w, "bad audience or format", http.StatusBadRequest)
		default:
			return false
		}
		return true
	}
	return ms
}

// setScript replaces the scripted responses (before the entrypoint runs).
func (cs *claimServer) setScript(script ...func(http.ResponseWriter)) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.script = script
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

func respondIDToken(tok string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html") // what the metadata server sends
		io.WriteString(w, tok)
	}
}

// fakeIDToken returns a throwaway JWT-shaped value (three base64url segments).
func fakeIDToken(t *testing.T) string {
	t.Helper()
	seg := func(n int) string {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return seg(30) + "." + seg(200) + "." + seg(256)
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
	idToken, audience  string
	home               string       // HOME of the entrypoint run (and CR_EXPECTED_HOME)
	guardHook          string       // test copy of cr-ci-job-guard.sh (CR_JOB_GUARD_HOOK)
	guardSHAFile       string       // test copy of its pinned sha256 (CR_JOB_GUARD_SHA_FILE)
	meta               *claimServer // fake metadata server, wired by default
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
		idToken:   fakeIDToken(t),
		home:      root,
		audience:  strings.TrimSuffix(mediatorURL, "/"),
	}
	h.meta = newMetadataServer(t, h.audience, respondIDToken(h.idToken))
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
	h.guardHook, h.guardSHAFile = installGuard(t, root)
	h.env = map[string]string{
		// Test mode (the entrypoint is not PID 1 here): the CR_* overrides take effect.
		"CR_TEST_MODE":             "1",
		"CR_JOB_GUARD_HOOK":        h.guardHook,
		"CR_JOB_GUARD_SHA_FILE":    h.guardSHAFile,
		"CR_JOB_GUARD_OWNER_UID":   strconv.Itoa(os.Getuid()),
		"GH_REPOS":                 testRepoFull,
		"PATH":                     os.Getenv("PATH"),
		"HOME":                     root,
		"CR_EXPECTED_HOME":         root,
		"RUNNER_CLAIM_TOKEN":       h.token,
		"MEDIATOR_URL":             mediatorURL,
		"GH_JOB_ID":                "4242",
		"RUNNER_NAME":              "cr-job-4242",
		"CR_SCRATCH_DIR":           h.scratch,
		"CR_MOUNTINFO":             h.mountinfo,
		"CR_RUNNER_DIR":            h.runnerDir,
		"CR_CLAIM_BACKOFF_SECONDS": "0",
		"CR_METADATA_URL":          h.meta.URL,
		"STUB_OUT":                 h.stubOut,
		// The runner-output FIFOs go under the test's own temp dir, not /tmp.
		"CR_FIFO_TMPDIR": root,
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
	cmd := h.entrypointCmd()
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
	for name, secret := range map[string]string{"claim token": h.token, "JIT config": h.jit, "ID token": h.idToken} {
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
	// run.sh is the entrypoint's child (not exec'd), so the entrypoint can report the
	// runner's real exit code and forward signals.
	if got := h.stubFile("ppid"); got != strconv.Itoa(res.pid) {
		t.Errorf("run.sh parent pid = %s, entrypoint pid = %d: run.sh is not the entrypoint's child", got, res.pid)
	}
	// The watcher's done FIFO is closed for the child: no fd of run.sh points at it.
	// (Other inherited fds, such as a build wrapper's lock, are not the entrypoint's.)
	if fds := h.stubFile("fds_long"); strings.Contains(fds, "watcher-done") || strings.Contains(fds, "idle-wait") {
		t.Errorf("run.sh inherited the watcher's done FIFO or the idle timer's FIFO:\n%s", fds)
	}
	// An ignored SIGINT would break run.sh's graceful cancel (RUNNER_MANUALLY_TRAP_SIG).
	if got := h.stubFile("int_trap"); got != "" {
		t.Errorf("run.sh starts with SIGINT disposition %q, want the default (not ignored)", got)
	}
	if got := h.stubFile("cwd"); got != h.runnerDir {
		t.Errorf("run.sh cwd = %s, want %s", got, h.runnerDir)
	}
	if !strings.Contains(res.stdout, "stub run.sh started") {
		t.Errorf("stdout lacks the stub's output: %q", res.stdout)
	}

	env := h.stubFile("env")
	for _, want := range []string{
		"RUNNER_WORKSPACE=" + h.home + "/work",
		"RUNNER_TEMP=" + h.scratch + "/_temp",
		"TMPDIR=" + h.scratch + "/_temp",
		"GOPATH=" + h.home + "/go",
		"GOCACHE=" + h.home + "/.cache/go-build",
		"GOMODCACHE=" + h.home + "/go/pkg/mod",
		"GOLANGCI_LINT_CACHE=" + h.home + "/.cache/golangci-lint",
		"XDG_CACHE_HOME_SET=",
		"TOKEN_SET=",
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=" + h.guardHook,
		"CR_JOB_GUARD_REPOS=" + testRepoFull,
		"CR_TEST_MODE_SET=",
	} {
		if !strings.Contains(env+"\n", want+"\n") {
			t.Errorf("run.sh env lacks %q; got:\n%s", want, env)
		}
	}
	h.checkHomeLinks()
	// The runner's _diag must stay off /scratch (F-D).
	if _, err := os.Lstat(filepath.Join(h.scratch, "_diag")); !os.IsNotExist(err) {
		t.Errorf("%s/_diag exists (err %v); the runner's logs must not go to /scratch", h.scratch, err)
	}
	for _, d := range []string{"_work", "_temp", ".cache/go-build", ".cache/golangci-lint", "go/pkg/mod"} {
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
	if r.auth != "Bearer "+h.idToken {
		t.Error("claim request did not carry Authorization: Bearer <ID token from the metadata server>")
	}
	if r.claim != h.token {
		t.Error("claim request did not carry X-Runner-Claim: <RUNNER_CLAIM_TOKEN>")
	}

	mreqs := h.meta.recorded()
	if len(mreqs) != 1 {
		t.Fatalf("metadata requests = %d, want 1", len(mreqs))
	}
	m := mreqs[0]
	if m.method != http.MethodGet || m.path != identityPath || m.flavor != "Google" ||
		m.query.Get("audience") != cs.URL || m.query.Get("format") != "full" {
		t.Errorf("metadata request = %s %s flavor=%q query=%v, want GET %s with Metadata-Flavor: Google, audience=%s, format=full",
			m.method, m.path, m.flavor, m.query, identityPath, cs.URL)
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
	expectFailure(t, h, res, 3, "after 5 attempts (last: claim status 000)")
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
				h.t.Fatal(err)
			}
		}, "is not a mount point"},
		{"mount table unreadable, same device as parent", func(h *harness) { h.env["CR_MOUNTINFO"] = filepath.Join(h.scratch, "no-such-file") }, "is not a mount point"},
		{"not writable", func(h *harness) {
			if os.Geteuid() == 0 {
				h.t.Skip("root ignores directory permissions")
			}
			if err := os.Chmod(h.scratch, 0o555); err != nil {
				h.t.Fatal(err)
			}
			h.t.Cleanup(func() { os.Chmod(h.scratch, 0o755) })
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
		{"remote metadata url", func(h *harness) { h.env["CR_METADATA_URL"] = "http://metadata.example.com" }, "CR_METADATA_URL must be"},
		{"zero idle timeout", func(h *harness) { h.env["RUNNER_IDLE_TIMEOUT_SECONDS"] = "0" }, "RUNNER_IDLE_TIMEOUT_SECONDS must be"},
		{"idle timeout with a unit", func(h *harness) { h.env["RUNNER_IDLE_TIMEOUT_SECONDS"] = "10m" }, "RUNNER_IDLE_TIMEOUT_SECONDS must be"},
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
				h.t.Fatal(err)
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

func TestEntrypointScratchStatFailure(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusOK))
	h := newHarness(t, cs.URL)
	if err := os.WriteFile(h.mountinfo, []byte("22 1 0:21 / / rw - overlay overlay rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fails for /scratch and its parent only (the job guard's own stat calls, on other
	// paths, run first and must still work).
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	h.stubCommand("stat", fmt.Sprintf(`#!/usr/bin/env bash
case ${!#} in %q | %q) echo 'stat: simulated failure' >&2; exit 1 ;; esac
exec %q "$@"
`, h.scratch, filepath.Dir(h.scratch), realStat))
	res := h.run()
	expectFailure(t, h, res, 2, "cannot stat "+h.scratch+" to verify that it is a mount")
	if n := len(cs.recorded()); n != 0 {
		t.Errorf("claim requests = %d, want 0 (token must not be spent)", n)
	}
}

func TestEntrypointTrailingSlashAudience(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL+"/")
	if res := h.run(); res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if m := h.meta.recorded(); len(m) != 1 || m[0].query.Get("audience") != cs.URL {
		t.Errorf("metadata requests = %+v, want one with audience %s (no trailing slash)", m, cs.URL)
	}
}

func TestEntrypointIdentityRejected(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusUnauthorized))
	h := newHarness(t, cs.URL)
	res := h.run()
	expectFailure(t, h, res, 3, "rejected the runner identity (401)")
	if n := len(cs.recorded()); n != 1 {
		t.Errorf("claim requests = %d, want 1 (401 is not retried)", n)
	}
}

func TestEntrypointMetadataRetryThenSuccess(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	h.meta.setScript(respondStatus(http.StatusServiceUnavailable), respondStatus(http.StatusInternalServerError), respondIDToken(h.idToken))
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if n := len(h.meta.recorded()); n != 3 {
		t.Errorf("metadata requests = %d, want 3", n)
	}
	if reqs := cs.recorded(); len(reqs) != 1 || reqs[0].auth != "Bearer "+h.idToken {
		t.Errorf("want exactly one claim request carrying the ID token, got %d", len(reqs))
	}
	if !strings.Contains(res.stderr, "claim succeeded on attempt 3") {
		t.Errorf("metadata retries did not use the shared attempt budget:\n%s", res.stderr)
	}
}

func TestEntrypointMetadataExhausted(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusOK))
	h := newHarness(t, cs.URL)
	h.meta.setScript(respondStatus(http.StatusServiceUnavailable))
	res := h.run()
	expectFailure(t, h, res, 3, "after 5 attempts (last: metadata server status 503)")
	if n := len(h.meta.recorded()); n != 5 {
		t.Errorf("metadata requests = %d, want 5", n)
	}
	if n := len(cs.recorded()); n != 0 {
		t.Errorf("claim requests = %d, want 0", n)
	}
}

func TestEntrypointMetadataUnreachable(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusOK))
	h := newHarness(t, cs.URL)
	h.meta.Close() // nothing listens on the port any more
	res := h.run()
	expectFailure(t, h, res, 3, "after 5 attempts (last: metadata server status 000)")
	if n := len(cs.recorded()); n != 0 {
		t.Errorf("claim requests = %d, want 0", n)
	}
}

func TestEntrypointMetadataNonRetryable(t *testing.T) {
	cases := []struct {
		name    string
		respond func(http.ResponseWriter)
		wantMsg string
	}{
		{"not found", respondStatus(http.StatusNotFound), "metadata server returned status 404"},
		{"forbidden", respondStatus(http.StatusForbidden), "metadata server returned status 403"},
		{"empty body", respondIDToken(""), "200 without an ID token"},
		{"not a jwt", respondIDToken("not-a-jwt"), "200 without an ID token"},
		{"header injection attempt", respondIDToken("a.b.c\"\nheader = \"X-Evil: 1"), "200 without an ID token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newClaimServer(t, respondStatus(http.StatusOK))
			h := newHarness(t, cs.URL)
			h.meta.setScript(tc.respond)
			res := h.run()
			expectFailure(t, h, res, 3, tc.wantMsg)
			if n := len(h.meta.recorded()); n != 1 {
				t.Errorf("metadata requests = %d, want 1 (not retried)", n)
			}
			if n := len(cs.recorded()); n != 0 {
				t.Errorf("claim requests = %d, want 0", n)
			}
		})
	}
}

func TestEntrypointSharedAttemptBudget(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusBadGateway))
	h := newHarness(t, cs.URL)
	h.meta.setScript(respondStatus(http.StatusServiceUnavailable), respondStatus(http.StatusServiceUnavailable), respondIDToken(h.idToken))
	res := h.run()
	expectFailure(t, h, res, 3, "after 5 attempts (last: claim status 502)")
	if n := len(h.meta.recorded()); n != 3 {
		t.Errorf("metadata requests = %d, want 3", n)
	}
	if n := len(cs.recorded()); n != 3 {
		t.Errorf("claim requests = %d, want 3 (5 attempts minus 2 spent on the metadata server)", n)
	}
}

func TestEntrypointIDTokenReusedOnClaimRetry(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, respondStatus(http.StatusServiceUnavailable), func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if n := len(h.meta.recorded()); n != 1 {
		t.Errorf("metadata requests = %d, want 1 (ID token reused)", n)
	}
	reqs := cs.recorded()
	if len(reqs) != 2 {
		t.Fatalf("claim requests = %d, want 2", len(reqs))
	}
	for i, r := range reqs {
		if r.auth != "Bearer "+h.idToken || r.claim != h.token {
			t.Errorf("claim request %d lacks the ID token or the claim header", i+1)
		}
	}
}

// After a runner swap or hijack recovery the mediator may launch a runner
// whose RUNNER_NAME belongs to another job ID; that must only warn.
func TestEntrypointRunnerNameMismatchOnlyWarns(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	h.env["RUNNER_NAME"] = "cr-job-9999"
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "warning: RUNNER_NAME 'cr-job-9999' does not match cr-job-4242") {
		t.Errorf("no RUNNER_NAME mismatch warning in stderr:\n%s", res.stderr)
	}
}

func TestEntrypointIDTokenTrailingNewline(t *testing.T) {
	for name, suffix := range map[string]string{"LF": "\n", "CRLF": "\r\n", "two LFs": "\n\n"} {
		t.Run(name, func(t *testing.T) {
			var h *harness
			cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
			h = newHarness(t, cs.URL)
			h.meta.setScript(respondIDToken(h.idToken + suffix))
			res := h.run()
			if res.exitCode != 0 {
				t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
			}
			if reqs := cs.recorded(); len(reqs) != 1 || reqs[0].auth != "Bearer "+h.idToken {
				t.Error("claim did not carry the ID token without its trailing line ending")
			}
		})
	}
}

// checkHomeLinks asserts the cache-parity layout: ~/work, ~/.cache, ~/go and
// ~/.npm are symlinks whose literal targets mirror HOME under the scratch dir.
func (h *harness) checkHomeLinks() {
	h.t.Helper()
	for link, target := range map[string]string{
		"work":   h.scratch + "/_work",
		".cache": h.scratch + "/.cache",
		"go":     h.scratch + "/go",
		".npm":   h.scratch + "/.npm",
	} {
		got, err := os.Readlink(filepath.Join(h.home, link))
		if err != nil || got != target {
			h.t.Errorf("~/%s -> %q (err %v), want a symlink to %s", link, got, err, target)
		}
	}
}

func TestEntrypointHomeLinksKeptWhenCorrect(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	for link, target := range map[string]string{"work": "/_work", ".cache": "/.cache", "go": "/go", ".npm": "/.npm"} {
		if err := os.Symlink(h.scratch+target, filepath.Join(h.home, link)); err != nil {
			t.Fatal(err)
		}
	}
	if res := h.run(); res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	h.checkHomeLinks()
}

func TestEntrypointHomeLinkConflicts(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(h *harness)
		wantMsg string
	}{
		{"real ~/.cache dir", func(h *harness) {
			if err := os.Mkdir(filepath.Join(h.home, ".cache"), 0o755); err != nil {
				h.t.Fatal(err)
			}
		}, ".cache exists and is not a symlink"},
		{"real ~/work dir", func(h *harness) {
			if err := os.Mkdir(filepath.Join(h.home, "work"), 0o755); err != nil {
				h.t.Fatal(err)
			}
		}, "work exists and is not a symlink"},
		{"~/go links elsewhere", func(h *harness) {
			if err := os.Symlink(h.scratch+"/gopath", filepath.Join(h.home, "go")); err != nil {
				h.t.Fatal(err)
			}
		}, "go is a symlink to "},
		{"~/.cache links to the old non-mirrored target", func(h *harness) {
			if err := os.Symlink(h.scratch+"/cache", filepath.Join(h.home, ".cache")); err != nil {
				h.t.Fatal(err)
			}
		}, ".cache is a symlink to "},
		{"HOME is not the expected home", func(h *harness) { h.env["CR_EXPECTED_HOME"] = "/home/runner" }, "want /home/runner"},
		{"HOME unset", func(h *harness) { delete(h.env, "HOME") }, "HOME is not set"},
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

// tarCommand runs tar with a minimal environment: an inherited TAR_OPTIONS (for
// example --exclude) would otherwise change what the round-trip tests see.
func tarCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("tar", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	return cmd
}

// TestEntrypointCacheTarRoundTrip reproduces actions/cache's archive scheme
// (entries relative to GITHUB_WORKSPACE, tar -P -C "$GITHUB_WORKSPACE") between
// an ubuntu-latest layout and the layout the entrypoint leaves behind, in both
// directions. GITHUB_WORKSPACE is <home>/work/scion/scion on both.
func TestEntrypointCacheTarRoundTrip(t *testing.T) { cacheTarRoundTrip(t, false) }

// With RUNNER_BUILD_CACHE_IN_RAM=true, /scratch/.cache is a link to a RAM dir; the
// same archives must restore where Go reads, with the ~/.cache bytes in RAM.
func TestEntrypointCacheTarRoundTripRAM(t *testing.T) { cacheTarRoundTrip(t, true) }

func cacheTarRoundTrip(t *testing.T, inRAM bool) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	ramDir := filepath.Join(t.TempDir(), "ram-build-cache")
	cacheRoot := filepath.Join(h.scratch, ".cache") // where ~/.cache bytes physically land
	if inRAM {
		h.env["RUNNER_BUILD_CACHE_IN_RAM"] = "true"
		h.env["CR_RAM_CACHE_DIR"] = ramDir
		cacheRoot = ramDir
	}
	if res := h.run(); res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	crWorkspace := filepath.Join(h.home, "work", "scion", "scion")
	if err := os.MkdirAll(crWorkspace, 0o755); err != nil { // creates it under scratch/_work
		t.Fatal(err)
	}
	entries := []string{"../../../.cache/go-build/ab/obj", "../../../go/pkg/mod/cache/download/m/@v/v1.zip", "../../../.cache/golangci-lint/x"}

	tarRun := func(args ...string) {
		t.Helper()
		out, err := tarCommand(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("tar %v: %v\n%s", args, err, out)
		}
	}
	// writeEntries and readEntry go through filepath.Join, which cleans ".." LEXICALLY:
	// <workspace>/../../../.cache/... becomes <home>/.cache/..., the logical path that Go
	// and golangci-lint use (through the links on the cr-ci layout). Only tar, run with
	// -C <workspace>, resolves ".." physically, exactly as actions/cache does.
	writeEntries := func(workspace, content string) {
		t.Helper()
		for _, e := range entries {
			p := filepath.Join(workspace, e)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	readEntry := func(workspace, entry string) string {
		b, err := os.ReadFile(filepath.Join(workspace, entry))
		if err != nil {
			return ""
		}
		return string(b)
	}

	// ubuntu-latest -> cr-ci: an archive saved on a plain HOME restores where Go reads.
	ubuntuHome := filepath.Join(t.TempDir(), "home", "runner")
	ubuntuWorkspace := filepath.Join(ubuntuHome, "work", "scion", "scion")
	if err := os.MkdirAll(ubuntuWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEntries(ubuntuWorkspace, "from-ubuntu")
	ubuntuTar := filepath.Join(t.TempDir(), "ubuntu.tar")
	tarRun(append([]string{"-cPf", ubuntuTar, "-C", ubuntuWorkspace}, entries...)...)
	tarRun("-xPf", ubuntuTar, "-C", crWorkspace)
	for logical, physical := range map[string]string{
		filepath.Join(h.home, ".cache/go-build/ab/obj"):                filepath.Join(cacheRoot, "go-build/ab/obj"),
		filepath.Join(h.home, "go/pkg/mod/cache/download/m/@v/v1.zip"): filepath.Join(h.scratch, "go/pkg/mod/cache/download/m/@v/v1.zip"),
		filepath.Join(h.home, ".cache/golangci-lint/x"):                filepath.Join(cacheRoot, "golangci-lint/x"),
	} {
		if b, err := os.ReadFile(logical); err != nil || string(b) != "from-ubuntu" {
			t.Errorf("ubuntu archive did not restore to %s (what Go reads): %v", logical, err)
		}
		if b, err := os.ReadFile(physical); err != nil || string(b) != "from-ubuntu" {
			t.Errorf("restored bytes are not physically at %s: %v", physical, err)
		}
	}
	h.checkHomeLinks() // tar must not have replaced a link
	if inRAM {
		if got, err := os.Readlink(filepath.Join(h.scratch, ".cache")); err != nil || got != ramDir {
			t.Errorf("%s/.cache = %q (%v), want a link to %s after the restore", h.scratch, got, err, ramDir)
		}
	}

	// cr-ci -> ubuntu-latest: an archive saved through the links restores under the
	// plain HOME, not under /home/home/... or next to it.
	writeEntries(crWorkspace, "from-cr-ci")
	crTar := filepath.Join(t.TempDir(), "crci.tar")
	tarRun(append([]string{"-cPf", crTar, "-C", crWorkspace}, entries...)...)
	ubuntuHome2 := filepath.Join(t.TempDir(), "home", "runner")
	ubuntuWorkspace2 := filepath.Join(ubuntuHome2, "work", "scion", "scion")
	if err := os.MkdirAll(ubuntuWorkspace2, 0o755); err != nil {
		t.Fatal(err)
	}
	tarRun("-xPf", crTar, "-C", ubuntuWorkspace2)
	for _, e := range entries {
		if got := readEntry(ubuntuWorkspace2, e); got != "from-cr-ci" {
			t.Errorf("cr-ci archive entry %s did not restore under the ubuntu HOME (got %q)", e, got)
		}
	}
	if _, err := os.Stat(filepath.Join(ubuntuHome2, ".cache", "go-build", "ab", "obj")); err != nil {
		t.Errorf("cr-ci archive did not land in ~/.cache/go-build on the ubuntu layout: %v", err)
	}
}

// A whole-directory cache path (actions/cache path: ~/.cache) archives directory
// entries such as ../../../.cache/ itself. Extracting them must not replace the
// ~/.cache link with a real directory: tar resolves ".." physically, so the entry
// lands on /scratch/.cache (a real dir), and the link is never walked.
func TestEntrypointCacheTarWholeDirectory(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	if res := h.run(); res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	crWorkspace := filepath.Join(h.home, "work", "scion", "scion")
	if err := os.MkdirAll(crWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	ubuntuHome := filepath.Join(t.TempDir(), "home", "runner")
	ubuntuWorkspace := filepath.Join(ubuntuHome, "work", "scion", "scion")
	for _, d := range []string{ubuntuWorkspace, filepath.Join(ubuntuHome, ".cache", "go-build", "ab")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ubuntuHome, ".cache", "go-build", "ab", "obj"), []byte("whole-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "whole.tar")
	for _, args := range [][]string{
		{"-cPf", archive, "-C", ubuntuWorkspace, "../../../.cache"},
		{"-xPf", archive, "-C", crWorkspace},
	} {
		if out, err := tarCommand(args...).CombinedOutput(); err != nil {
			t.Fatalf("tar %v: %v\n%s", args, err, out)
		}
	}
	if out, err := tarCommand("-tPf", archive).Output(); err != nil || !strings.Contains(string(out), "../../../.cache/\n") {
		t.Fatalf("archive lacks the ../../../.cache/ directory entry (err %v):\n%s", err, out)
	}
	h.checkHomeLinks() // the directory entry must not have replaced the link
	if b, err := os.ReadFile(filepath.Join(h.home, ".cache", "go-build", "ab", "obj")); err != nil || string(b) != "whole-dir" {
		t.Errorf("whole-directory archive did not restore to ~/.cache/go-build: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(h.scratch, ".cache", "go-build", "ab", "obj")); err != nil {
		t.Errorf("restored bytes are not on the scratch disk: %v", err)
	}
}

// If a HOME link cannot be created, the entrypoint exits 2 before the claim.
func TestEntrypointHomeLinkCreateFails(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusOK))
	h := newHarness(t, cs.URL)
	if os.Geteuid() == 0 {
		h.t.Skip("root ignores directory permissions")
	}
	// HOME read-only: ln -s into it fails, while the scratch dir (a subdirectory with
	// its own permissions) stays writable for the earlier checks.
	if err := os.Chmod(h.home, 0o555); err != nil {
		t.Fatal(err)
	}
	h.t.Cleanup(func() { os.Chmod(h.home, 0o755) })
	res := h.run()
	expectFailure(t, h, res, 2, "cannot create symlink "+h.home+"/work -> "+h.scratch+"/_work")
	if n := len(cs.recorded()); n != 0 {
		t.Errorf("claim requests = %d, want 0 (token must not be spent)", n)
	}
}

// --- Exit code and signals (section 5) ------------------------------------------

func newSuccessHarness(t *testing.T) *harness {
	t.Helper()
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	return h
}

func TestEntrypointPropagatesRunShExitCode(t *testing.T) {
	h := newSuccessHarness(t)
	h.setRunSh(stubRunShWith("exit 134\n"))
	res := h.run()
	if res.exitCode != 134 {
		t.Errorf("exit code = %d, want run.sh's 134; stderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "run.sh exited 134") {
		t.Errorf("stderr does not report run.sh's exit code:\n%s", res.stderr)
	}
}

// run-helper.sh (actions/runner) turns an unknown Runner.Listener exit code into
// "Exiting with unknown error code: N" and exit 0; run.sh then exits 0 too.
func TestEntrypointMapsRunHelperUnknownExitCode(t *testing.T) {
	h := newSuccessHarness(t)
	h.setRunSh(stubRunShWith(`echo "Runner listener exited"
echo "Exiting with unknown error code: 134"
echo "Exiting runner..."
exit 0
`))
	res := h.run()
	if res.exitCode != 134 {
		t.Errorf("exit code = %d, want 134 from run-helper's line; stderr:\n%s", res.exitCode, res.stderr)
	}
	for _, line := range []string{"Exiting with unknown error code: 134", "Exiting runner..."} {
		if !strings.Contains(res.stdout, line+"\n") {
			t.Errorf("runner output line %q was not passed through to stdout:\n%s", line, res.stdout)
		}
	}
	if !strings.Contains(res.stderr, "unknown exit code 134") {
		t.Errorf("stderr does not explain the mapped exit code:\n%s", res.stderr)
	}
}

// Only run-helper.sh's exact line counts: anchored, digits only.
func TestEntrypointIgnoresOtherUnknownCodeLines(t *testing.T) {
	for _, line := range []string{
		"job step said: Exiting with unknown error code: 7",
		"Exiting with unknown error code: 7 (and more)",
		"Exiting with unknown error code: abc",
		"Exiting with unknown error code: 08", // leading zero: would be octal in (( ))
		"Exiting with unknown error code: ",
	} {
		t.Run(line, func(t *testing.T) {
			h := newSuccessHarness(t)
			h.setRunSh(stubRunShWith("printf '%s\\n' '" + line + "'\nexit 0\n"))
			if res := h.run(); res.exitCode != 0 {
				t.Errorf("exit code = %d, want 0: %q must not be taken as run-helper's line; stderr:\n%s", res.exitCode, line, res.stderr)
			}
		})
	}
}

// killEntrypoint ends a started entrypoint on a test's failure path, together with
// the stub run.sh: run.sh runs in its own process group (job control), so killing
// the entrypoint alone could leave it running past the test.
func killEntrypoint(h *harness, cmd *exec.Cmd) {
	if b, err := os.ReadFile(filepath.Join(h.stubOut, "pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL) // run.sh's process group
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	// The entrypoint's own process group (entrypointCmd: Setpgid): the entrypoint, its
	// output watcher and its idle watchdog, which hold the test's stdout/stderr pipes.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// entrypointCmd returns the command that runs entrypoint.sh with the harness env, as
// the leader of its own process group, so killEntrypoint can end it together with the
// background processes it starts (output watcher, idle watchdog). WaitDelay bounds
// Wait if a process that escaped the group still holds the output pipes.
func (h *harness) entrypointCmd() *exec.Cmd {
	cmd := exec.Command("bash", "entrypoint.sh")
	cmd.Env = make([]string, 0, len(h.env))
	for k, v := range h.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 30 * time.Second
	return cmd
}

// signalStub waits until it is signalled, records the signal and exits with a
// signal-specific code (42 for TERM, 43 for INT).
const signalStub = `trap 'echo TERM > "$out/signal"; exit 42' TERM
trap 'echo INT > "$out/signal"; exit 43' INT
touch "$out/ready"
for _ in $(seq 1 300); do sleep 0.1; done
exit 9
`

func TestEntrypointForwardsSignals(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		name string
		code int
	}{{syscall.SIGTERM, "TERM", 42}, {syscall.SIGINT, "INT", 43}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSuccessHarness(t)
			h.setRunSh(stubRunShWith(signalStub))
			cmd := h.entrypointCmd()
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(h.stubOut, "ready")
			deadline := time.Now().Add(20 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					killEntrypoint(h, cmd)
					t.Fatalf("stub run.sh never became ready; stderr:\n%s", stderr.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			err := cmd.Wait()
			code := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got := h.stubFile("signal"); got != tc.name {
				t.Errorf("run.sh received %q, want %s forwarded from the entrypoint", got, tc.name)
			}
			if code != tc.code {
				t.Errorf("exit code = %d, want run.sh's %d; stderr:\n%s", code, tc.code, stderr.String())
			}
			// Normally logged on receipt; in the narrow window before runner_pid is set,
			// logged when the recorded signal is forwarded right after the start.
			if !strings.Contains(stderr.String(), "received SIG"+tc.name+"; forwarding it to run.sh") &&
				!strings.Contains(stderr.String(), "forwarding SIG"+tc.name+", which arrived while run.sh was starting") {
				t.Errorf("stderr does not log the forwarded signal:\n%s", stderr.String())
			}
			for name, secret := range map[string]string{"claim token": h.token, "JIT config": h.jit, "ID token": h.idToken} {
				if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
					t.Errorf("%s leaked into entrypoint output", name)
				}
			}
		})
	}
}

// --- Warm-up mode (deploy-job.sh --warm-up) ---------------------------------------

func warmupHarness(t *testing.T) (*harness, *claimServer) {
	t.Helper()
	cs := newClaimServer(t, respondStatus(http.StatusInternalServerError))
	h := newHarness(t, cs.URL)
	for _, k := range []string{"RUNNER_CLAIM_TOKEN", "MEDIATOR_URL", "GH_JOB_ID", "RUNNER_NAME", "GH_REPOS"} {
		delete(h.env, k)
	}
	h.env["RUNNER_WARMUP"] = "1"
	return h, cs
}

func TestEntrypointWarmUp(t *testing.T) {
	h, cs := warmupHarness(t)
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if h.stubStarted() {
		t.Error("run.sh was started in warm-up mode")
	}
	if n := len(cs.recorded()) + len(h.meta.recorded()); n != 0 {
		t.Errorf("warm-up made %d mediator/metadata requests, want none", n)
	}
	if !strings.Contains(res.stderr, "warm-up OK") {
		t.Errorf("stderr lacks the warm-up result:\n%s", res.stderr)
	}
	h.checkHomeLinks() // the checks still ran
}

func TestEntrypointWarmUpStillChecks(t *testing.T) {
	h, _ := warmupHarness(t)
	if err := os.RemoveAll(h.scratch); err != nil {
		t.Fatal(err)
	}
	expectFailure(t, h, h.run(), 2, "does not exist")

	h2, _ := warmupHarness(t)
	h2.env["RUNNER_WARMUP"] = "yes"
	expectFailure(t, h2, h2.run(), 2, "RUNNER_WARMUP must be 1 or unset")
}

func TestEntrypointBuildCacheInRAMValue(t *testing.T) {
	h := newSuccessHarness(t)
	h.env["RUNNER_BUILD_CACHE_IN_RAM"] = "yes"
	expectFailure(t, h, h.run(), 2, "RUNNER_BUILD_CACHE_IN_RAM must be true or false")
}

func TestEntrypointBuildCacheInRAMLinks(t *testing.T) {
	h := newSuccessHarness(t)
	ramDir := filepath.Join(t.TempDir(), "ram")
	h.env["RUNNER_BUILD_CACHE_IN_RAM"] = "true"
	h.env["CR_RAM_CACHE_DIR"] = ramDir
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d; stderr:\n%s", res.exitCode, res.stderr)
	}
	if got, err := os.Readlink(filepath.Join(h.scratch, ".cache")); err != nil || got != ramDir {
		t.Errorf("%s/.cache = %q (%v), want a link to %s", h.scratch, got, err, ramDir)
	}
	h.checkHomeLinks() // ~/.cache still -> /scratch/.cache (parity)
	for _, d := range []string{"go-build", "golangci-lint"} {
		if fi, err := os.Stat(filepath.Join(ramDir, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s/%s was not created in RAM: %v", ramDir, d, err)
		}
	}
	if !strings.Contains(res.stderr, "build cache in RAM") {
		t.Errorf("stderr does not log RAM mode:\n%s", res.stderr)
	}
}

// A SIGTERM during the claim phase (here: between claim retries) stops the
// entrypoint at the next checkpoint with 128+15, without starting run.sh.
func TestEntrypointSignalDuringClaim(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusServiceUnavailable))
	h := newHarness(t, cs.URL)
	h.env["CR_CLAIM_BACKOFF_SECONDS"] = "2"
	cmd := h.entrypointCmd()
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(cs.recorded()) == 0 {
		if time.Now().After(deadline) {
			killEntrypoint(h, cmd)
			t.Fatalf("no claim attempt; stderr:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code != 143 {
		t.Errorf("exit code = %d, want 143 (128+SIGTERM); stderr:\n%s", code, stderr.String())
	}
	if h.stubStarted() {
		t.Error("run.sh was started after a SIGTERM during the claim")
	}
	if !strings.Contains(stderr.String(), "received SIGTERM before run.sh started") {
		t.Errorf("stderr does not log the early signal:\n%s", stderr.String())
	}
	if n := len(cs.recorded()); n > 2 {
		t.Errorf("%d claim attempts after the signal, want the retries to stop", n)
	}
	// The FIFO directory created before the claim is removed on that exit too.
	if left, _ := filepath.Glob(filepath.Join(h.home, "cr-ci-entrypoint.*")); len(left) != 0 {
		t.Errorf("FIFO directory left behind after the signalled exit: %v", left)
	}
}

// run.sh exiting immediately on the forwarded signal: the entrypoint's wait is
// interrupted by the trap and run.sh may already be gone by the liveness check. The
// exit code must still be run.sh's (42), not the interrupted wait's 128+15. (The race
// is narrow; this guards the code path rather than forcing the interleaving.)
func TestEntrypointSignalFastExitKeepsRunShCode(t *testing.T) {
	for i := 0; i < 3; i++ { // a few rounds: the interleaving varies
		h := newSuccessHarness(t)
		h.setRunSh(stubRunShWith(`sleep 30 &
nap=$!
trap 'kill "$nap" 2>/dev/null; exit 42' TERM
touch "$out/ready"
wait "$nap"
exit 9
`))
		cmd := h.entrypointCmd()
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready := filepath.Join(h.stubOut, "ready")
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				killEntrypoint(h, cmd)
				t.Fatalf("stub never became ready; stderr:\n%s", stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		var exitErr *exec.ExitError
		code := 0
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != 42 {
			t.Fatalf("round %d: exit code = %d, want run.sh's 42; stderr:\n%s", i, code, stderr.String())
		}
	}
}

// The runner-output FIFOs are set up before the claim: a failure there exits 2 with
// the single-use claim token unspent (no claim request at all).
func TestEntrypointFIFOSetupFailsBeforeClaim(t *testing.T) {
	var h *harness
	cs := newClaimServer(t, func(w http.ResponseWriter) { respondJIT(h.jit)(w) })
	h = newHarness(t, cs.URL)
	h.env["CR_FIFO_TMPDIR"] = filepath.Join(h.home, "no-such-dir")
	expectFailure(t, h, h.run(), 2, "cannot create a private temp directory")
	if n := len(cs.recorded()); n != 0 {
		t.Errorf("%d claim requests, want none: the token must stay unspent", n)
	}
}

// A failed claim (exit 3) removes the FIFO directory it created.
func TestEntrypointClaimFailureRemovesFIFODir(t *testing.T) {
	cs := newClaimServer(t, respondStatus(http.StatusGone))
	h := newHarness(t, cs.URL)
	fifoRoot := filepath.Join(h.home, "fifos")
	if err := os.Mkdir(fifoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	h.env["CR_FIFO_TMPDIR"] = fifoRoot
	expectFailure(t, h, h.run(), 3, "410")
	if left, _ := filepath.Glob(filepath.Join(fifoRoot, "cr-ci-entrypoint.*")); len(left) != 0 {
		t.Errorf("FIFO directory left behind after the failed claim: %v", left)
	}
}

// --- Idle timeout (RUNNER_IDLE_TIMEOUT_SECONDS) -------------------------------------

// A runner that has not started a job by its idle deadline is stopped like a cancel
// (SIGTERM to run.sh), and the execution ends 0 with a distinct log line.
func TestEntrypointIdleTimeoutStopsRunner(t *testing.T) {
	h := newSuccessHarness(t)
	// 3 s: the stub's recording prelude runs before its traps are set, and a TERM that
	// came first would end it by default instead of through the trap.
	h.env["RUNNER_IDLE_TIMEOUT_SECONDS"] = "3"
	h.setRunSh(stubRunShWith(signalStub))
	res := h.run()
	if res.exitCode != 0 {
		t.Errorf("exit code = %d, want 0 (an idle stop is not a failure); stderr:\n%s", res.exitCode, res.stderr)
	}
	if got := h.stubFile("signal"); got != "TERM" {
		t.Errorf("run.sh received %q, want TERM from the idle watchdog", got)
	}
	for _, want := range []string{
		"idle timeout: no job started within 3s of the container start; stopping the runner gracefully",
		"idle_timeout: runner stopped after 3s without a job (run.sh exited 42); exiting 0",
	} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not contain %q:\n%s", want, res.stderr)
		}
	}
}

// A job that starts shortly before the deadline (3 s after run.sh starts, the deadline
// 9 s after the entrypoint starts: about 6 s of margin for the claim and a loaded
// machine) is not touched: the runner runs it to
// the end and the entrypoint reports run.sh's own result.
func TestEntrypointIdleTimeoutJobJustBeforeDeadline(t *testing.T) {
	h := newSuccessHarness(t)
	h.env["RUNNER_IDLE_TIMEOUT_SECONDS"] = "9"
	h.setRunSh(stubRunShWith(`trap 'echo TERM > "$out/signal"; exit 42' TERM
trap 'echo INT > "$out/signal"; exit 43' INT
sleep 3
echo "2026-10-22 21:00:00Z: Running job: lint"
for _ in $(seq 1 30); do sleep 0.1; done
echo "2026-10-22 21:00:03Z: Job lint completed with result: Succeeded"
exit 0
`))
	res := h.run()
	if res.exitCode != 0 {
		t.Errorf("exit code = %d, want run.sh's 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.stubOut, "signal")); err == nil {
		t.Errorf("run.sh was signalled (%s), want the started job left alone", h.stubFile("signal"))
	}
	if strings.Contains(res.stderr, "idle") {
		t.Errorf("stderr reports an idle stop for a runner that started a job:\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "run.sh exited 0") {
		t.Errorf("stderr does not report run.sh's exit:\n%s", res.stderr)
	}
}

// A run that ends before the deadline does not wait for it, and leaves no watchdog behind.
func TestEntrypointIdleTimeoutDoesNotDelayExit(t *testing.T) {
	h := newSuccessHarness(t)
	h.env["RUNNER_IDLE_TIMEOUT_SECONDS"] = "60"
	start := time.Now()
	res := h.run()
	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", res.exitCode, res.stderr)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("entrypoint took %s, want it to end with run.sh, not at the idle deadline", d)
	}
}

// The idle stop raced a job start: the watchdog found no job and sent SIGTERM, and the
// job's "Running job:" line arrived anyway (here: printed by the stub on SIGTERM, which
// makes the race deterministic). The cancelled job must not pass for an idle stop.
func TestEntrypointIdleTimeoutRacedJob(t *testing.T) {
	h := newSuccessHarness(t)
	h.env["RUNNER_IDLE_TIMEOUT_SECONDS"] = "3" // room for the stub's prelude before its trap
	h.setRunSh(stubRunShWith(`trap 'echo "2026-10-22 21:00:01Z: Running job: lint"; echo TERM > "$out/signal"; exit 0' TERM
for _ in $(seq 1 300); do sleep 0.1; done
exit 9
`))
	res := h.run()
	if res.exitCode != 4 {
		t.Errorf("exit code = %d, want 4 for an idle stop that raced a job; stderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "ERROR idle_timeout_raced_job:") {
		t.Errorf("stderr does not report the race:\n%s", res.stderr)
	}
	if strings.Contains(res.stderr, "without a job") {
		t.Errorf("stderr reports a plain idle stop for a cancelled job:\n%s", res.stderr)
	}
}
