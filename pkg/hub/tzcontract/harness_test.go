// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build tzcontract

package tzcontract

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // "pgx" database/sql driver
	_ "modernc.org/sqlite"             // "sqlite" database/sql driver
)

// ---------------------------------------------------------------------------
// Binary
// ---------------------------------------------------------------------------

var (
	binOnce sync.Once
	binPath string
	binErr  error
	// binDir is the temporary directory the binary was built into, or ""
	// when SCION_TZ_CONTRACT_BIN supplied it. TestMain removes it.
	binDir string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// scionBinary returns the path of the scion binary under test. It uses
// SCION_TZ_CONTRACT_BIN when set; otherwise it builds ./cmd/scion once per
// test process into a temporary directory.
func scionBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		if p := os.Getenv("SCION_TZ_CONTRACT_BIN"); p != "" {
			binPath = p
			return
		}
		root, err := moduleRoot()
		if err != nil {
			binErr = err
			return
		}
		dir, err := os.MkdirTemp("", "scion-tzcontract-bin-")
		if err != nil {
			binErr = err
			return
		}
		binDir = dir
		binPath = filepath.Join(dir, "scion")
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "./cmd/scion")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("go build ./cmd/scion: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatalf("scion binary: %v", binErr)
	}
	return binPath
}

func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not inside a Go module")
	}
	return filepath.Dir(gomod), nil
}

// ---------------------------------------------------------------------------
// Backends
// ---------------------------------------------------------------------------

// backend describes one database the hub under test runs against.
type backend struct {
	name   string // "sqlite" or "postgres"
	dbPath string // sqlite: file path
	pgURL  string // postgres: URL of the per-run database
}

func (b backend) isSQLite() bool { return b.name == "sqlite" }

// newSQLiteBackend returns a backend on a fresh SQLite file.
func newSQLiteBackend(t *testing.T) backend {
	return backend{name: "sqlite", dbPath: filepath.Join(t.TempDir(), "hub.db")}
}

// newPostgresBackend creates a fresh database on the server named by
// SCION_TEST_POSTGRES_URL and drops it at the end of the test.
func newPostgresBackend(t *testing.T, adminURL string) backend {
	t.Helper()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("tzcontract_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse SCION_TEST_POSTGRES_URL: %v", err)
	}
	u.Path = "/" + name
	return backend{name: "postgres", pgURL: u.String()}
}

// openDB opens the backend's database directly, for seeding rows and for
// inspecting stored values. The SQLite connection deliberately sets no
// _timezone option, so text is read and written exactly as stored.
func (b backend) openDB(t *testing.T) *sql.DB {
	t.Helper()
	var db *sql.DB
	var err error
	if b.isSQLite() {
		db, err = sql.Open("sqlite", b.dbPath)
	} else {
		db, err = sql.Open("pgx", b.pgURL)
	}
	if err != nil {
		t.Fatalf("open %s: %v", b.name, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ---------------------------------------------------------------------------
// Hub process
// ---------------------------------------------------------------------------

// hubProc is one running `scion server start --foreground` process.
type hubProc struct {
	t     *testing.T
	cmd   *exec.Cmd
	base  string // http://127.0.0.1:<port>
	token string
	logMu sync.Mutex
	logs  bytes.Buffer
	done  chan struct{}
}

// startHub starts the real binary with TZ=tz in its environment. home is the
// HOME directory, reused across restarts of the same scenario so the dev
// token, hub ID and signing keys persist.
func startHub(t *testing.T, b backend, tz, home string) *hubProc {
	t.Helper()
	port := freePort(t)
	args := []string{
		"server", "start", "--foreground",
		"--enable-hub", "--enable-runtime-broker=false",
		"--dev-auth",
		"--host", "127.0.0.1",
		"--web-port", fmt.Sprint(port),
	}
	// A minimal, explicit environment: no SCION_* or TZ value leaks in from
	// the developer's shell or the agent container.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TZ=" + tz,
	}
	if b.isSQLite() {
		args = append(args, "--db", b.dbPath)
	} else {
		env = append(env,
			"SCION_SERVER_DATABASE_DRIVER=postgres",
			"SCION_SERVER_DATABASE_URL="+b.pgURL,
		)
	}
	h := &hubProc{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{})}
	h.cmd = exec.Command(scionBinary(t), args...)
	h.cmd.Env = env
	h.cmd.Dir = home
	h.cmd.Stdout = h.logWriter()
	h.cmd.Stderr = h.logWriter()
	if err := h.cmd.Start(); err != nil {
		t.Fatalf("start hub: %v", err)
	}
	go func() {
		_ = h.cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(func() {
		h.stop()
		if t.Failed() {
			t.Logf("hub log tail (TZ=%s, %s):\n%s", tz, b.name, h.tailLogs(80))
		}
	})

	deadline := time.Now().Add(90 * time.Second)
	for {
		select {
		case <-h.done:
			t.Fatalf("hub exited during startup (TZ=%s, %s):\n%s", tz, b.name, h.tailLogs(60))
		default:
		}
		resp, err := http.Get(h.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub not healthy after 90s (TZ=%s, %s):\n%s", tz, b.name, h.tailLogs(60))
		}
		time.Sleep(250 * time.Millisecond)
	}
	tok, err := os.ReadFile(filepath.Join(home, ".scion", "dev-token"))
	if err != nil {
		t.Fatalf("read dev token: %v", err)
	}
	h.token = strings.TrimSpace(string(tok))
	return h
}

type lockedWriter struct{ h *hubProc }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.h.logMu.Lock()
	defer w.h.logMu.Unlock()
	return w.h.logs.Write(p)
}

func (h *hubProc) logWriter() io.Writer { return lockedWriter{h} }

func (h *hubProc) logText() string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.logs.String()
}

func (h *hubProc) tailLogs(n int) string {
	lines := strings.Split(h.logText(), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// stop sends SIGTERM and waits for a clean exit (SIGKILL after 30s). It is
// idempotent.
func (h *hubProc) stop() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	select {
	case <-h.done:
		return
	default:
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.done:
	case <-time.After(30 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.done
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

var httpClient = &http.Client{Timeout: 60 * time.Second}

// do sends a request with the dev token and returns the status and body.
func (h *hubProc) do(method, path string, body any) (int, []byte) {
	h.t.Helper()
	return h.doWithTimeout(method, path, body, 0)
}

// doWithTimeout is do with a per-request timeout. A timeout is not a test
// failure: it returns status 0, so callers can fire a request whose side
// effects happen before a slow tail (e.g. a broker dispatch retry).
func (h *hubProc) doWithTimeout(method, path string, body any, timeout time.Duration) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		rdr = bytes.NewReader(buf)
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rdr)
	if err != nil {
		h.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if timeout > 0 && ctx.Err() != nil {
			return 0, nil
		}
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// mustJSON sends a request, requires one of the wanted status codes, and
// decodes the body into a generic map.
func (h *hubProc) mustJSON(method, path string, body any, want ...int) map[string]any {
	h.t.Helper()
	status, out := h.do(method, path, body)
	ok := false
	for _, w := range want {
		if status == w {
			ok = true
		}
	}
	if !ok {
		h.t.Fatalf("%s %s: status %d, want %v: %s", method, path, status, want, truncate(out, 800))
	}
	m := map[string]any{}
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &m); err != nil {
			h.t.Fatalf("%s %s: decode: %v: %s", method, path, err, truncate(out, 800))
		}
	}
	return m
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

// sseEvent is one decoded "update" event: {"subject": ..., "data": {...}}.
type sseEvent struct {
	Subject string         `json:"subject"`
	Data    map[string]any `json:"data"`
}

// sseStream subscribes to the given subjects on /events.
type sseStream struct {
	events chan sseEvent
	cancel context.CancelFunc
}

func (h *hubProc) subscribe(subjects ...string) *sseStream {
	h.t.Helper()
	q := url.Values{}
	for _, s := range subjects {
		q.Add("sub", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/events?"+q.Encode(), nil)
	if err != nil {
		cancel()
		h.t.Fatalf("sse request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		h.t.Fatalf("sse connect: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		h.t.Fatalf("sse connect: status %d: %s", resp.StatusCode, truncate(b, 400))
	}
	s := &sseStream{events: make(chan sseEvent, 256), cancel: cancel}
	go func() {
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
		var data strings.Builder
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			case line == "":
				if data.Len() > 0 {
					var ev sseEvent
					if json.Unmarshal([]byte(data.String()), &ev) == nil && ev.Subject != "" {
						select {
						case s.events <- ev:
						default:
						}
					}
					data.Reset()
				}
			}
		}
	}()
	h.t.Cleanup(cancel)
	// Give the server time to register the subscription before the caller
	// triggers the event it waits for. With Postgres, a project subject needs
	// a new LISTEN channel, which the listener picks up on a one-second poll.
	time.Sleep(2500 * time.Millisecond)
	return s
}

// wait returns the first event whose subject has the given suffix and whose
// data satisfies match.
func (s *sseStream) wait(t *testing.T, subjectSuffix string, match func(map[string]any) bool) sseEvent {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev := <-s.events:
			if strings.HasSuffix(ev.Subject, subjectSuffix) && match(ev.Data) {
				return ev
			}
		case <-timeout:
			t.Fatalf("no SSE event with subject suffix %q within 20s", subjectSuffix)
		}
	}
}

// ---------------------------------------------------------------------------
// Timestamp assertions
// ---------------------------------------------------------------------------

// slack absorbs storage rounding (Postgres keeps microseconds and rounds).
const slack = time.Millisecond

// window brackets a server-side write: every server-generated timestamp for
// that write must fall inside [lo, hi].
type window struct{ lo, hi time.Time }

func openWindow() window { return window{lo: time.Now()} }

func (w *window) close() { w.hi = time.Now() }

// wireTime asserts that v is a non-zero RFC 3339 string in UTC with a "Z"
// suffix and returns the parsed instant.
func wireTime(t *testing.T, label string, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s: want an RFC 3339 string, got %T (%v)", label, v, v)
	}
	if !strings.HasSuffix(s, "Z") {
		t.Errorf("%s = %q: want a UTC value with a Z suffix", label, s)
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%s = %q: not RFC 3339: %v", label, s, err)
	}
	if ts.IsZero() || ts.Year() < 2000 {
		t.Errorf("%s = %q: zero or implausible time", label, s)
	}
	return ts
}

// wireTimeIn asserts wireTime and that the instant lies inside w.
func wireTimeIn(t *testing.T, label string, v any, w window) time.Time {
	t.Helper()
	ts := wireTime(t, label, v)
	if ts.Before(w.lo.Add(-slack)) || ts.After(w.hi.Add(slack)) {
		t.Errorf("%s = %s: outside the write window [%s, %s]", label, ts.Format(time.RFC3339Nano),
			w.lo.UTC().Format(time.RFC3339Nano), w.hi.UTC().Format(time.RFC3339Nano))
	}
	return ts
}

// storePrecision is the finest resolution the contract compares at. Postgres
// rounds to microseconds, and some responses echo the in-memory value that was
// written rather than the stored one. A zone error is minutes or hours.
const storePrecision = time.Microsecond

// wireTimeEq asserts wireTime and instant equality with want, at
// storePrecision.
func wireTimeEq(t *testing.T, label string, v any, want time.Time) {
	t.Helper()
	ts := wireTime(t, label, v)
	d := ts.Sub(want)
	if d < 0 {
		d = -d
	}
	if d >= storePrecision {
		t.Errorf("%s = %s: want the instant %s", label, ts.Format(time.RFC3339Nano), want.UTC().Format(time.RFC3339Nano))
	}
}

// ---------------------------------------------------------------------------
// Generic JSON access
// ---------------------------------------------------------------------------

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func obj(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

func list(m map[string]any, key string) []map[string]any {
	raw, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if o, ok := r.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func findBy(items []map[string]any, key, value string) map[string]any {
	for _, it := range items {
		if str(it, key) == value {
			return it
		}
	}
	return nil
}
