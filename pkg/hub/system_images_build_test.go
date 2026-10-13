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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func collectBuildLog(t *testing.T, r io.Reader) ([]string, error) {
	t.Helper()
	var lines []string
	err := streamBuildLog(r, func(line string) { lines = append(lines, line) })
	return lines, err
}

// truncatedBuildLogLine is the line streamBuildLog emits for an over-long
// line made of the byte c repeated.
func truncatedBuildLogLine(c string) string {
	return strings.Repeat(c, imageBuildLogMaxLine) + fmt.Sprintf(imageBuildLogTruncatedFmt, imageBuildLogMaxLine)
}

func TestStreamBuildLog_LongLines(t *testing.T) {
	long := strings.Repeat("a", 100_000)
	tooLong := strings.Repeat("b", imageBuildLogMaxLine+5000)
	input := "first\r\n" + long + "\n\n" + tooLong + "\nlast-no-newline"

	lines, err := collectBuildLog(t, strings.NewReader(input))
	if err != nil {
		t.Fatalf("streamBuildLog error: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5", len(lines))
	}
	if lines[0] != "first" {
		t.Errorf("line 0 = %q, want %q", lines[0], "first")
	}
	if lines[1] != long {
		t.Errorf("line 1 has length %d, want the full %d-byte line", len(lines[1]), len(long))
	}
	if lines[2] != "" {
		t.Errorf("line 2 = %q, want empty line", lines[2])
	}
	if want := truncatedBuildLogLine("b"); lines[3] != want {
		t.Errorf("line 3 has length %d, want %d (cut at %d bytes plus marker)", len(lines[3]), len(want), imageBuildLogMaxLine)
	}
	if lines[4] != "last-no-newline" {
		t.Errorf("line 4 = %q, want %q", lines[4], "last-no-newline")
	}
}

// failingReader returns its data, then err on every later read.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestStreamBuildLog_ReadErrorReturnsPendingLineAndError(t *testing.T) {
	readErr := errors.New("boom")
	lines, err := collectBuildLog(t, &failingReader{data: []byte("one\npartial"), err: readErr})
	if !errors.Is(err, readErr) {
		t.Fatalf("err = %v, want %v", err, readErr)
	}
	if len(lines) != 2 || lines[0] != "one" || lines[1] != "partial" {
		t.Fatalf("lines = %q, want [one partial]", lines)
	}
}

// A final line with no newline that exactly fills the read buffer comes back
// from ReadLine with isPrefix set, followed by EOF or an error with no data.
// It must still be emitted.
func TestStreamBuildLog_UnterminatedLineOnBufferBoundary(t *testing.T) {
	last := strings.Repeat("c", imageBuildLogReadBuf)

	lines, err := collectBuildLog(t, strings.NewReader("first\n"+last))
	if err != nil {
		t.Fatalf("streamBuildLog error at EOF: %v", err)
	}
	if len(lines) != 2 || lines[0] != "first" || lines[1] != last {
		t.Fatalf("at EOF: got %d lines, want [first, %d x c]", len(lines), len(last))
	}

	readErr := errors.New("boom")
	lines, err = collectBuildLog(t, &failingReader{data: []byte("first\n" + last), err: readErr})
	if !errors.Is(err, readErr) {
		t.Fatalf("err = %v, want %v", err, readErr)
	}
	if len(lines) != 2 || lines[0] != "first" || lines[1] != last {
		t.Fatalf("on read error: got %d lines, want [first, %d x c]", len(lines), len(last))
	}
}

type imageBuildEventRecorder struct {
	noopEventPublisher
	mu    sync.Mutex
	lines []string
}

func (r *imageBuildEventRecorder) PublishRaw(_ string, data interface{}) {
	if ev, ok := data.(imageBuildLogEvent); ok {
		r.mu.Lock()
		r.lines = append(r.lines, ev.Line)
		r.mu.Unlock()
	}
}

func (r *imageBuildEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestHandleSystemImagesBuild_LongLogLinesDoNotHang runs a build script that
// prints a line over the default 64 KiB scanner limit and one over
// imageBuildLogMaxLine. The build must finish, clear imageBuildActive, and a
// second build must then be accepted.
func TestHandleSystemImagesBuild_LongLogLinesDoNotHang(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "probe.sh")
	writeExecutable(t, probe, "#!/bin/sh\nexit 0\n")
	if err := exec.Command(probe).Run(); err != nil {
		t.Skipf("cannot execute scripts in temp dir %s (noexec?): %v", root, err)
	}
	writeExecutable(t, filepath.Join(root, "image-build", "scripts", "build-images.sh"), `#!/bin/sh
echo before-long-lines
head -c 100000 /dev/zero | tr '\0' a
echo
head -c 2097152 /dev/zero | tr '\0' b
echo
echo after-long-lines
`)
	t.Setenv("SCION_ROOT", root)
	restore := config.OverrideRuntimeDetection(
		func(string) (string, error) { return "/usr/bin/podman", nil },
		func(string, []string) error { return nil },
	)
	t.Cleanup(restore)
	builder := NewAuthenticatedUser("u-build", "builder@example.com", "Builder", "admin", "cli")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := &imageBuildEventRecorder{}
	srv := &Server{ctx: ctx, events: rec}

	startBuild := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/system/images/build", strings.NewReader(`{"harnesses":["claude"]}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req = req.WithContext(contextWithIdentity(req.Context(), builder))
		w := httptest.NewRecorder()
		srv.handleSystemImagesBuild(w, req)
		return w.Code
	}
	waitIdle := func() bool {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if !srv.imageBuildActive.Load() {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}

	if code := startBuild(); code != http.StatusOK {
		t.Fatalf("first build: status %d, want %d", code, http.StatusOK)
	}
	if !waitIdle() {
		t.Fatalf("first build did not finish: imageBuildActive still set; log so far has %d lines", len(rec.snapshot()))
	}

	lines := rec.snapshot()
	var sawLong, sawTruncated, sawAfter, sawComplete bool
	for _, l := range lines {
		switch {
		case l == strings.Repeat("a", 100000):
			sawLong = true
		case l == truncatedBuildLogLine("b"):
			sawTruncated = true
		case l == "after-long-lines":
			sawAfter = true
		case l == "build complete":
			sawComplete = true
		}
	}
	if !sawLong || !sawTruncated || !sawAfter || !sawComplete {
		t.Fatalf("build log missing lines: long=%v truncated=%v after=%v complete=%v (%d lines)",
			sawLong, sawTruncated, sawAfter, sawComplete, len(lines))
	}

	if code := startBuild(); code != http.StatusOK {
		t.Fatalf("second build: status %d, want %d", code, http.StatusOK)
	}
	if !waitIdle() {
		t.Fatalf("second build did not finish: imageBuildActive still set")
	}
}
