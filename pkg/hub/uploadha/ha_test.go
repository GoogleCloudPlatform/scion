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

// Package uploadha validates Phase 1 two-process HA resumable workspace uploads
// (ptone/scion#8654 §7 validation check 2, §8 items 5–6).
//
// It builds the real scion binary, starts two separate Hub OS processes against
// the same PostgreSQL database and shared workspace directory on distinct
// loopback ports, and drives the HTTP upload API across both processes —
// including alternating chunks, SIGKILL mid-chunk with lease takeover,
// cross-replica completion and replay, and atomic abort preservation.
//
// Skipped by default in CI unless SCION_TEST_POSTGRES_URL (or
// SCION_TEST_POSTGRES_DSN) is set.
package uploadha

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	defaultUploadSizeMiB = 32
	defaultChunkSizeMiB  = 4
)

var (
	binOnce sync.Once
	binPath string
	binErr  error
	binDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

func scionBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		if p := os.Getenv("SCION_UPLOAD_HA_BIN"); p != "" {
			binPath = p
			return
		}
		root, err := moduleRoot()
		if err != nil {
			binErr = err
			return
		}
		dir, err := os.MkdirTemp("", "scion-uploadha-bin-")
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

func postgresAdminURL() string {
	if u := strings.TrimSpace(os.Getenv("SCION_TEST_POSTGRES_URL")); u != "" {
		return u
	}
	return strings.TrimSpace(os.Getenv("SCION_TEST_POSTGRES_DSN"))
}

func createTempPostgresDB(t *testing.T, adminURL string) (string, *sql.DB) {
	t.Helper()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	dbName := fmt.Sprintf("uploadha_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatalf("create database %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", dbName, err)
		}
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse postgres URL: %v", err)
	}
	u.Path = "/" + dbName
	pgURL := u.String()

	db, err := sql.Open("pgx", pgURL)
	if err != nil {
		t.Fatalf("open temp postgres %s: %v", dbName, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return pgURL, db
}

type hubProc struct {
	t     *testing.T
	name  string
	cmd   *exec.Cmd
	port  int
	base  string
	token string
	logMu sync.Mutex
	logs  bytes.Buffer
	done  chan struct{}
}

func startHub(t *testing.T, name, pgURL, home string) *hubProc {
	t.Helper()
	port := freePort(t)
	args := []string{
		"server", "start", "--foreground",
		"--enable-hub", "--enable-runtime-broker=false",
		"--dev-auth",
		"--host", "127.0.0.1",
		"--web-port", fmt.Sprint(port),
	}
	// Minimal explicit env so container SCION_* variables do not leak in.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TZ=UTC",
		"SCION_SERVER_DATABASE_DRIVER=postgres",
		"SCION_SERVER_DATABASE_URL=" + pgURL,
	}
	h := &hubProc{
		t:    t,
		name: name,
		port: port,
		base: fmt.Sprintf("http://127.0.0.1:%d", port),
		done: make(chan struct{}),
	}
	h.cmd = exec.Command(scionBinary(t), args...)
	h.cmd.Env = env
	h.cmd.Dir = home
	h.cmd.Stdout = h.logWriter()
	h.cmd.Stderr = h.logWriter()
	if err := h.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		_ = h.cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(func() {
		h.stop()
		if t.Failed() {
			t.Logf("%s (pid=%d port=%d) log tail:\n%s", h.name, h.pid(), h.port, h.tailLogs(80))
		}
	})

	deadline := time.Now().Add(90 * time.Second)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		select {
		case <-h.done:
			t.Fatalf("%s exited during startup:\n%s", name, h.tailLogs(60))
		default:
		}
		resp, err := client.Get(h.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not healthy after 90s:\n%s", name, h.tailLogs(60))
		}
		time.Sleep(200 * time.Millisecond)
	}

	tok, err := os.ReadFile(filepath.Join(home, ".scion", "dev-token"))
	if err != nil {
		t.Fatalf("read dev-token for %s: %v", name, err)
	}
	h.token = strings.TrimSpace(string(tok))
	t.Logf("started %s: pid=%d port=%d base=%s", h.name, h.pid(), h.port, h.base)
	return h
}

func (h *hubProc) pid() int {
	if h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
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

func (h *hubProc) killSIGKILL() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGKILL)
	<-h.done
}

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
	case <-time.After(15 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.done
	}
}

func (h *hubProc) procRSSSummary() string {
	pid := h.pid()
	if pid <= 0 {
		return "n/a"
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "n/a"
	}
	var rss, hwm string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			rss = strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:"))
		} else if strings.HasPrefix(line, "VmHWM:") {
			hwm = strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	return fmt.Sprintf("VmRSS=%s VmHWM=%s", rss, hwm)
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

var apiClient = &http.Client{Timeout: 2 * time.Minute}

func (h *hubProc) doJSON(method, reqPath string, body any) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal %s %s: %v", method, reqPath, err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, h.base+reqPath, rdr)
	if err != nil {
		h.t.Fatalf("new request %s %s: %v", method, reqPath, err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s (%s): %v", method, reqPath, h.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (h *hubProc) mustJSON(method, reqPath string, body any, want ...int) map[string]any {
	h.t.Helper()
	status, out := h.doJSON(method, reqPath, body)
	matched := false
	for _, w := range want {
		if status == w {
			matched = true
			break
		}
	}
	if !matched {
		h.t.Fatalf("%s %s (%s): status %d, want %v: %s", method, reqPath, h.name, status, want, string(out))
	}
	m := map[string]any{}
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &m); err != nil {
			h.t.Fatalf("%s %s (%s): decode JSON: %v: %s", method, reqPath, h.name, err, string(out))
		}
	}
	return m
}

func (h *hubProc) putChunk(reqPath string, body io.Reader, contentLength int64) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPut, h.base+reqPath, body)
	if err != nil {
		return 0, nil, err
	}
	req.ContentLength = contentLength
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := apiClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

type apiErrorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeAPIError(t *testing.T, body []byte) apiErrorEnvelope {
	t.Helper()
	var env apiErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, string(body))
	}
	return env
}

// deterministicChunk returns a deterministic pseudo-random slice for byte range
// [offset, offset+n) using an 8-byte counter mixed with splitmix64 so we never
// need to hold a 1 GiB payload in RAM while still guaranteeing every chunk is
// unique and verifiable.
func deterministicChunk(offset, n int64) []byte {
	buf := make([]byte, n)
	var pos int64
	for pos < n {
		idx := uint64((offset + pos) / 8)
		z := idx + 0x9e3779b97f4a7c15
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z = z ^ (z >> 31)
		var word [8]byte
		binary.LittleEndian.PutUint64(word[:], z)
		startInWord := (offset + pos) % 8
		take := int64(8) - startInWord
		if pos+take > n {
			take = n - pos
		}
		copy(buf[pos:pos+take], word[startInWord:startInWord+take])
		pos += take
	}
	return buf
}

func sha256File(t *testing.T, path string) (string, int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n
}

func findPartialFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), ".scion-upload-") && strings.HasSuffix(d.Name(), ".partial") {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

// stalledChunkReader yields head bytes immediately, signals headSent, and then
// blocks until release is closed (simulating a slow/stalled upload body so the
// test can SIGKILL the serving hub deterministically mid-chunk).
type stalledChunkReader struct {
	head     []byte
	sent     bool
	headSent chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (r *stalledChunkReader) Read(p []byte) (int, error) {
	if !r.sent {
		n := copy(p, r.head)
		r.head = r.head[n:]
		if len(r.head) == 0 {
			r.sent = true
			r.once.Do(func() { close(r.headSent) })
		}
		return n, nil
	}
	<-r.release
	return 0, errors.New("stalled reader released")
}

func parseEnvInt64(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func TestWorkspaceUploads_TwoProcessHA(t *testing.T) {
	adminURL := postgresAdminURL()
	if adminURL == "" {
		t.Skip("set SCION_TEST_POSTGRES_URL (or SCION_TEST_POSTGRES_DSN) to run two-process HA upload validation")
	}

	totalBytes := parseEnvInt64("SCION_UPLOAD_HA_SIZE_MIB", defaultUploadSizeMiB) << 20
	chunkBytes := parseEnvInt64("SCION_UPLOAD_HA_CHUNK_MIB", defaultChunkSizeMiB) << 20
	if chunkBytes > 16<<20 {
		chunkBytes = 16 << 20
	}

	pgURL, db := createTempPostgresDB(t, adminURL)
	sharedHome := t.TempDir()

	// Start Hub A first so ~/.scion (dev-token, settings) is initialized once
	// cleanly, then start Hub B sharing the same HOME (workspace root) and
	// PostgreSQL database on a different loopback port.
	hubA := startHub(t, "hub-A", pgURL, sharedHome)
	hubB := startHub(t, "hub-B", pgURL, sharedHome)

	// Enable hub.large_uploads via Hub A's Admin API and wait for Hub B to
	// observe the experiment override via shared DB operational settings.
	expState := hubA.mustJSON(http.MethodGet, "/api/v1/admin/experiments", nil, http.StatusOK)
	hubA.mustJSON(http.MethodPut, "/api/v1/admin/experiments", map[string]any{
		"expected_revision": expState["revision"],
		"overrides":         map[string]any{"hub.large_uploads": true},
	}, http.StatusOK)

	// When testing above the 50 MiB default (or when explicitly requested via
	// SCION_UPLOAD_HA_MAX_FILE_BYTES), configure server.hub.uploads.max_workspace_file_size
	// through Hub A's Admin API and confirm Hub B observes the new limit live.
	maxFileBytes := parseEnvInt64("SCION_UPLOAD_HA_MAX_FILE_BYTES", 0)
	if maxFileBytes == 0 && totalBytes > 50<<20 {
		maxFileBytes = totalBytes
	}
	if maxFileBytes > 0 {
		putPayload := map[string]any{
			"server": map[string]any{
				"hub": map[string]any{
					"uploads": map[string]any{
						"max_workspace_file_size": maxFileBytes,
					},
				},
			},
		}
		putReqBytes, _ := json.Marshal(putPayload)
		putStatus, putRespBytes := hubA.doJSON(http.MethodPut, "/api/v1/admin/server-config", putPayload)
		if putStatus != http.StatusOK {
			t.Fatalf("PUT /api/v1/admin/server-config on hub-A: status %d, body: %s", putStatus, string(putRespBytes))
		}
		t.Logf("admin PUT /api/v1/admin/server-config on hub-A: req=%s status=%d resp=%s",
			string(putReqBytes), putStatus, string(bytes.TrimSpace(putRespBytes)))

		// Confirm Hub B observes the updated max_workspace_file_size live via
		// GET /api/v1/settings/public (propagated via Postgres LISTEN/NOTIFY).
		limitPropStart := time.Now()
		limitDeadline := limitPropStart.Add(30 * time.Second)
		for {
			pubStatus, pubBody := hubB.doJSON(http.MethodGet, "/api/v1/settings/public", nil)
			if pubStatus == http.StatusOK {
				var pub struct {
					Uploads struct {
						MaxWorkspaceFileSize int64 `json:"maxWorkspaceFileSize"`
					} `json:"uploads"`
				}
				if err := json.Unmarshal(pubBody, &pub); err == nil && pub.Uploads.MaxWorkspaceFileSize == maxFileBytes {
					t.Logf("hub-B observed uploads.maxWorkspaceFileSize=%d live in %s (GET /api/v1/settings/public)",
						pub.Uploads.MaxWorkspaceFileSize, time.Since(limitPropStart).Round(time.Millisecond))
					break
				}
			}
			if time.Now().After(limitDeadline) {
				t.Fatalf("hub-B did not observe uploads.maxWorkspaceFileSize=%d within 30s (last status=%d body=%s)",
					maxFileBytes, pubStatus, string(pubBody))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Create a hub-managed project through Hub A.
	projResp := hubA.mustJSON(http.MethodPost, "/api/v1/projects", map[string]any{
		"name": "Upload HA Gate",
		"slug": "upload-ha-gate",
	}, http.StatusOK, http.StatusCreated)
	projectID, _ := projResp["id"].(string)
	if projectID == "" {
		t.Fatalf("project creation returned empty id: %v", projResp)
	}
	workspaceDir := filepath.Join(sharedHome, ".scion", "projects", "upload-ha-gate")
	t.Logf("project=%s workspace=%s totalBytes=%d (%d MiB) chunkBytes=%d (%d MiB)",
		projectID, workspaceDir, totalBytes, totalBytes>>20, chunkBytes, chunkBytes>>20)

	// Wait until Hub B observes hub.large_uploads=true (operational settings
	// propagation across replicas).
	propDeadline := time.Now().Add(30 * time.Second)
	for {
		status, body := hubB.doJSON(http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/uploads/nonexistent-probe", projectID), nil)
		if status == http.StatusNotFound {
			env := decodeAPIError(t, body)
			if env.Error.Code != "experiment_disabled" {
				break
			}
		}
		if time.Now().After(propDeadline) {
			t.Fatalf("hub-B did not observe hub.large_uploads=true within 30s (last status=%d body=%s)", status, string(body))
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("hub-B observed hub.large_uploads=true")
	t.Logf("baseline RSS: hub-A (%s), hub-B (%s)", hubA.procRSSSummary(), hubB.procRSSSummary())

	uploadsBase := fmt.Sprintf("/api/v1/projects/%s/workspace/uploads", projectID)

	// -----------------------------------------------------------------------
	// Scenario 1 (§7 check 2, §8 item 5):
	// Alternating chunks A/B, SIGKILL Hub A mid-chunk so its lease must be
	// taken over by Hub B, restart Hub A, finish alternating chunks, complete
	// on the other hub than the one that served the last chunk, and replay
	// complete on the opposite hub.
	// -----------------------------------------------------------------------
	startTime := time.Now()
	destRelPath := "ha/model.bin"
	started := hubA.mustJSON(http.MethodPost, uploadsBase, map[string]any{
		"name":      "model.bin",
		"path":      destRelPath,
		"size":      totalBytes,
		"mediaType": "application/octet-stream",
		"overwrite": true,
	}, http.StatusCreated)
	sid, _ := started["id"].(string)
	if !strings.HasPrefix(sid, "upl_") {
		t.Fatalf("unexpected session id: %v", started)
	}
	serverChunkBytes := int64(started["chunkBytes"].(float64))
	if chunkBytes > serverChunkBytes {
		chunkBytes = serverChunkBytes
	}
	t.Logf("started session=%s on hub-A (serverChunkBytes=%d, using=%d)", sid, serverChunkBytes, chunkBytes)

	srcHasher := sha256.New()
	var offset int64
	chunkIdx := 0
	didMidChunkKill := false
	var lastChunkHub *hubProc

	for offset < totalBytes {
		n := chunkBytes
		if totalBytes-offset < n {
			n = totalBytes - offset
		}
		chunkData := deterministicChunk(offset, n)
		_, _ = srcHasher.Write(chunkData)

		// On chunk index 2 (after chunk 0 on Hub A and chunk 1 on Hub B),
		// start sending a stalled body to Hub A, wait until Hub A holds the
		// Postgres lease and has written partial bytes to disk, then SIGKILL
		// Hub A mid-chunk.
		if chunkIdx == 2 && !didMidChunkKill {
			didMidChunkKill = true
			headLen := int64(128 * 1024)
			if headLen > n/2 {
				headLen = n / 2
			}
			stalled := &stalledChunkReader{
				head:     append([]byte(nil), chunkData[:headLen]...),
				headSent: make(chan struct{}),
				release:  make(chan struct{}),
			}
			putDone := make(chan error, 1)
			go func() {
				_, _, err := hubA.putChunk(fmt.Sprintf("%s/%s?offset=%d", uploadsBase, sid, offset), stalled, n)
				putDone <- err
			}()

			<-stalled.headSent

			// Wait until Hub A has claimed the lease in Postgres and written
			// uncommitted bytes past offset into the partial file.
			partialPath := filepath.Join(workspaceDir, "ha", ".scion-upload-"+sid+".partial")
			waitDeadline := time.Now().Add(15 * time.Second)
			for {
				var leaseToken sql.NullString
				if err := db.QueryRow(`SELECT lease_token FROM upload_sessions WHERE id = $1`, sid).Scan(&leaseToken); err != nil {
					t.Fatalf("query lease_token: %v", err)
				}
				fi, statErr := os.Stat(partialPath)
				if leaseToken.Valid && leaseToken.String != "" && statErr == nil && fi.Size() > offset {
					t.Logf("confirmed hub-A mid-chunk: lease_token=%s partialSize=%d (> offset=%d)", leaseToken.String, fi.Size(), offset)
					break
				}
				if time.Now().After(waitDeadline) {
					t.Fatalf("timed out waiting for hub-A to hold lease and write partial bytes (partialPath=%s)", partialPath)
				}
				time.Sleep(50 * time.Millisecond)
			}

			// SIGKILL Hub A while it holds the lease mid-chunk.
			killAt := time.Now().UTC()
			killedPID := hubA.pid()
			hubA.killSIGKILL()
			close(stalled.release)
			putErr := <-putDone
			t.Logf("SIGKILL hub-A (pid=%d) at %s; in-flight PUT returned err=%v", killedPID, killAt.Format(time.RFC3339Nano), putErr)

			// Immediately verify Hub B refuses with 409 upload_busy while the
			// dead Hub A's lease is still unexpired.
			busyStatus, busyBody, err := hubB.putChunk(fmt.Sprintf("%s/%s?offset=%d", uploadsBase, sid, offset), bytes.NewReader(chunkData), n)
			if err != nil {
				t.Fatalf("hub-B busy check failed: %v", err)
			}
			if busyStatus != http.StatusConflict {
				t.Fatalf("expected 409 upload_busy from hub-B while dead hub-A lease active, got %d: %s", busyStatus, string(busyBody))
			}
			busyEnv := decodeAPIError(t, busyBody)
			if busyEnv.Error.Code != "upload_busy" {
				t.Fatalf("expected error.code=upload_busy, got %q (body=%s)", busyEnv.Error.Code, string(busyBody))
			}
			t.Logf("hub-B correctly returned 409 upload_busy while hub-A lease active")

			// Verify GET status on Hub B shows received is still at offset.
			st := hubB.mustJSON(http.MethodGet, fmt.Sprintf("%s/%s", uploadsBase, sid), nil, http.StatusOK)
			if gotRec := int64(st["received"].(float64)); gotRec != offset {
				t.Fatalf("expected received=%d after mid-chunk kill, got %d", offset, gotRec)
			}

			// Simulate lease TTL expiry (protocol uploadLeaseTTL is 5m30s) by
			// backdating lease_until in Postgres so Hub B can take over the
			// lease without waiting 5.5 minutes.
			if _, err := db.Exec(`UPDATE upload_sessions SET lease_until = NOW() - INTERVAL '5 seconds' WHERE id = $1`, sid); err != nil {
				t.Fatalf("expire lease_until: %v", err)
			}

			// Hub B takes over the expired lease, truncates uncommitted bytes
			// left by killed Hub A, and commits the full chunk.
			status, body, err := hubB.putChunk(fmt.Sprintf("%s/%s?offset=%d", uploadsBase, sid, offset), bytes.NewReader(chunkData), n)
			if err != nil {
				t.Fatalf("hub-B takeover PUT failed: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("hub-B takeover PUT status=%d body=%s", status, string(body))
			}
			var takeovers int
			if err := db.QueryRow(`SELECT takeovers FROM upload_sessions WHERE id = $1`, sid).Scan(&takeovers); err != nil {
				t.Fatalf("query takeovers: %v", err)
			}
			if takeovers < 1 {
				t.Fatalf("expected takeovers >= 1 after lease takeover, got %d", takeovers)
			}
			t.Logf("hub-B took over lease and committed chunk %d (offset=%d..%d, takeovers=%d)", chunkIdx, offset, offset+n, takeovers)

			// Restart Hub A as a fresh OS process so subsequent chunks and
			// complete/replay continue across two live replicas.
			hubA = startHub(t, "hub-A-restarted", pgURL, sharedHome)
			lastChunkHub = hubB
			offset += n
			chunkIdx++
			continue
		}

		target := hubA
		if chunkIdx%2 == 1 {
			target = hubB
		}
		status, body, err := target.putChunk(fmt.Sprintf("%s/%s?offset=%d", uploadsBase, sid, offset), bytes.NewReader(chunkData), n)
		if err != nil {
			t.Fatalf("chunk %d on %s failed: %v", chunkIdx, target.name, err)
		}
		if status != http.StatusOK {
			t.Fatalf("chunk %d on %s status=%d body=%s", chunkIdx, target.name, status, string(body))
		}
		lastChunkHub = target
		offset += n
		chunkIdx++
	}

	// Call complete on the OTHER hub than the one that served the last chunk.
	completeHub := hubA
	replayHub := hubB
	if lastChunkHub == hubA {
		completeHub = hubB
		replayHub = hubA
	}
	t.Logf("last chunk served by %s; calling complete on %s and replay on %s", lastChunkHub.name, completeHub.name, replayHub.name)

	compStatus, compBody := completeHub.doJSON(http.MethodPost, fmt.Sprintf("%s/%s/complete", uploadsBase, sid), nil)
	if compStatus != http.StatusOK {
		t.Fatalf("complete on %s failed: status=%d body=%s", completeHub.name, compStatus, string(compBody))
	}

	// Replay complete on the opposite hub and assert semantically identical
	// JSON response (§8 item 5). Postgres jsonb canonicalizes key order and
	// whitespace on read-back, so compare the decoded JSON objects.
	replayStatus, replayBody := replayHub.doJSON(http.MethodPost, fmt.Sprintf("%s/%s/complete", uploadsBase, sid), nil)
	if replayStatus != http.StatusOK {
		t.Fatalf("complete replay on %s failed: status=%d body=%s", replayHub.name, replayStatus, string(replayBody))
	}
	var firstObj, replayObj map[string]any
	if err := json.Unmarshal(compBody, &firstObj); err != nil {
		t.Fatalf("unmarshal first complete response: %v (body=%s)", err, string(compBody))
	}
	if err := json.Unmarshal(replayBody, &replayObj); err != nil {
		t.Fatalf("unmarshal replay complete response: %v (body=%s)", err, string(replayBody))
	}
	firstCanon, _ := json.Marshal(firstObj)
	replayCanon, _ := json.Marshal(replayObj)
	if !bytes.Equal(firstCanon, replayCanon) {
		t.Fatalf("complete replay mismatch:\n  first  (%s): %s\n  replay (%s): %s",
			completeHub.name, string(compBody), replayHub.name, string(replayBody))
	}
	t.Logf("complete response matched on replay: %s", string(firstCanon))

	elapsed := time.Since(startTime)
	wantHash := hex.EncodeToString(srcHasher.Sum(nil))
	destAbsPath := filepath.Join(workspaceDir, filepath.FromSlash(destRelPath))
	gotHash, gotSize := sha256File(t, destAbsPath)
	if gotSize != totalBytes {
		t.Fatalf("stored file size=%d, want %d", gotSize, totalBytes)
	}
	if gotHash != wantHash {
		t.Fatalf("stored file SHA-256=%s, want %s", gotHash, wantHash)
	}
	if partials := findPartialFiles(t, workspaceDir); len(partials) > 0 {
		t.Fatalf("unexpected partial files remaining after complete: %v", partials)
	}
	t.Logf("PASS Scenario 1: size=%d (%d MiB) chunks=%d sha256=%s duration=%s",
		gotSize, gotSize>>20, chunkIdx, gotHash, elapsed.Round(time.Millisecond))
	t.Logf("post-upload RSS: %s (%s), %s (%s)", hubA.name, hubA.procRSSSummary(), hubB.name, hubB.procRSSSummary())

	// -----------------------------------------------------------------------
	// Scenario 2 (§8 item 6):
	// Existing target file stays byte-identical when an overwrite upload is
	// aborted mid-flight on the other hub, and no partial file remains.
	// -----------------------------------------------------------------------
	beforeHash, beforeSize := sha256File(t, destAbsPath)
	abortStart := hubA.mustJSON(http.MethodPost, uploadsBase, map[string]any{
		"name":      "model.bin",
		"path":      destRelPath,
		"size":      2 * (1 << 20),
		"mediaType": "application/octet-stream",
		"overwrite": true,
	}, http.StatusCreated)
	abortSID, _ := abortStart["id"].(string)

	// Upload one 1 MiB chunk on Hub B so the partial file is created next to dest.
	abortChunk := bytes.Repeat([]byte("A"), 1<<20)
	abStatus, abBody, err := hubB.putChunk(fmt.Sprintf("%s/%s?offset=0", uploadsBase, abortSID), bytes.NewReader(abortChunk), int64(len(abortChunk)))
	if err != nil || abStatus != http.StatusOK {
		t.Fatalf("abort scenario chunk on hub-B failed: status=%d err=%v body=%s", abStatus, err, string(abBody))
	}

	// Verify target file is untouched while the overwrite upload is in flight.
	midHash, midSize := sha256File(t, destAbsPath)
	if midHash != beforeHash || midSize != beforeSize {
		t.Fatalf("target file modified while overwrite upload in flight: got (%d, %s), want (%d, %s)", midSize, midHash, beforeSize, beforeHash)
	}

	// Abort from Hub A -> 204 No Content.
	delStatus, delBody := hubA.doJSON(http.MethodDelete, fmt.Sprintf("%s/%s", uploadsBase, abortSID), nil)
	if delStatus != http.StatusNoContent {
		t.Fatalf("abort DELETE on %s failed: status=%d body=%s", hubA.name, delStatus, string(delBody))
	}

	afterHash, afterSize := sha256File(t, destAbsPath)
	if afterHash != beforeHash || afterSize != beforeSize {
		t.Fatalf("target file changed after abort: got (%d, %s), want (%d, %s)", afterSize, afterHash, beforeSize, beforeHash)
	}
	if partials := findPartialFiles(t, workspaceDir); len(partials) > 0 {
		t.Fatalf("unexpected partial files remaining after abort: %v", partials)
	}
	t.Logf("PASS Scenario 2: existing target %s remained byte-identical (size=%d sha256=%s) after abort of %s; no partials remain",
		destRelPath, afterSize, afterHash, abortSID)

	logInterestingLines(t, hubB)
	logInterestingLines(t, hubA)
}

func logInterestingLines(t *testing.T, h *hubProc) {
	t.Helper()
	var kept []string
	for _, line := range strings.Split(h.logText(), "\n") {
		if strings.Contains(line, "Scion server ready") ||
			strings.Contains(line, "Using Postgres LISTEN/NOTIFY") ||
			strings.Contains(line, "experiment override set") ||
			strings.Contains(line, "Received settings update event") ||
			strings.Contains(line, "upload") {
			kept = append(kept, line)
		}
	}
	if len(kept) > 12 {
		kept = kept[:12]
	}
	if len(kept) > 0 {
		t.Logf("server log excerpt (%s, pid=%d):\n%s", h.name, h.pid(), strings.Join(kept, "\n"))
	}
}
