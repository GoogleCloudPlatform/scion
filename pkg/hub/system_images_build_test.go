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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
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
	mu         sync.Mutex
	lines      []string
	doneImages []string
}

func (r *imageBuildEventRecorder) PublishRaw(_ string, data interface{}) {
	switch ev := data.(type) {
	case imageBuildLogEvent:
		r.mu.Lock()
		r.lines = append(r.lines, ev.Line)
		r.mu.Unlock()
	case map[string]string:
		if ev["status"] == "done" {
			r.mu.Lock()
			r.doneImages = append(r.doneImages, ev["image"])
			r.mu.Unlock()
		}
	}
}

func (r *imageBuildEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func (r *imageBuildEventRecorder) snapshotDoneImages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.doneImages...)
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

func TestHandleSystemImagesBuild_BundledHarnessAllowlist(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "probe.sh")
	writeExecutable(t, probe, "#!/bin/sh\nexit 0\n")
	if err := exec.Command(probe).Run(); err != nil {
		t.Skipf("cannot execute scripts in temp dir %s (noexec?): %v", root, err)
	}
	writeExecutable(t, filepath.Join(root, "image-build", "scripts", "build-images.sh"), "#!/bin/sh\necho stub-build\n")
	t.Setenv("SCION_ROOT", root)
	restore := config.OverrideRuntimeDetection(
		func(string) (string, error) { return "/usr/bin/podman", nil },
		func(string, []string) error { return nil },
	)
	t.Cleanup(restore)
	builder := NewAuthenticatedUser("u-build", "builder@example.com", "Builder", "admin", "cli")

	// Derive the expected buildable harness set and configured images from the
	// embedded harnesses FS, matching TestHarnessImageMap_MatchesBundledHarnesses.
	hfs := harness.HarnessesFS()
	entries, err := fs.ReadDir(hfs, ".")
	if err != nil {
		t.Fatalf("read embedded harnesses FS: %v", err)
	}
	var wantNames []string
	var wantImages []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, err := fs.Stat(hfs, name+"/Dockerfile"); err != nil {
			continue
		}
		data, err := fs.ReadFile(hfs, name+"/config.yaml")
		if err != nil {
			t.Fatalf("read %s/config.yaml: %v", name, err)
		}
		cfg, err := config.ParseHarnessConfigYAML(data)
		if err != nil {
			t.Fatalf("parse %s/config.yaml: %v", name, err)
		}
		if cfg.Image == "" {
			t.Fatalf("%s/config.yaml has empty image", name)
		}
		wantNames = append(wantNames, name)
	}
	if len(wantNames) == 0 {
		t.Fatal("found no harnesses/<name>/Dockerfile in the embedded FS")
	}
	sort.Strings(wantNames)
	hasGeminiCLI := false
	for _, name := range wantNames {
		if name == "gemini-cli" {
			hasGeminiCLI = true
		}
		data, _ := fs.ReadFile(hfs, name+"/config.yaml")
		cfg, _ := config.ParseHarnessConfigYAML(data)
		wantImages = append(wantImages, strings.TrimSpace(cfg.Image))
	}
	if !hasGeminiCLI {
		t.Fatalf("expected gemini-cli in bundled buildable harnesses %v", wantNames)
	}

	t.Run("rejects retired gemini and unknown harnesses", func(t *testing.T) {
		for _, rejected := range []string{"gemini", "gen", "unknown-harness"} {
			srv := &Server{ctx: context.Background(), events: noopEventPublisher{}}
			body, err := json.Marshal(imageBuildRequest{Harnesses: []string{rejected}})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/system/images/build", strings.NewReader(string(body)))
			req.RemoteAddr = "127.0.0.1:12345"
			req = req.WithContext(contextWithIdentity(req.Context(), builder))
			w := httptest.NewRecorder()

			srv.handleSystemImagesBuild(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("harness %q: status = %d, want %d (body: %s)", rejected, w.Code, http.StatusBadRequest, w.Body.String())
			}
			var errResp struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("harness %q: decode error body %q: %v", rejected, w.Body.String(), err)
			}
			if wantMsg := fmt.Sprintf("unknown harness %q", rejected); errResp.Error.Message != wantMsg {
				t.Errorf("harness %q: message = %q, want %q", rejected, errResp.Error.Message, wantMsg)
			}
			if srv.imageBuildActive.Load() {
				t.Errorf("harness %q: rejected build must not leave imageBuildActive set", rejected)
			}
		}
	})

	t.Run("accepts all bundled buildable harnesses and emits configured image names", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		rec := &imageBuildEventRecorder{}
		srv := &Server{ctx: ctx, events: rec}

		body, err := json.Marshal(imageBuildRequest{Harnesses: wantNames})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/system/images/build", strings.NewReader(string(body)))
		req.RemoteAddr = "127.0.0.1:12345"
		req = req.WithContext(contextWithIdentity(req.Context(), builder))
		w := httptest.NewRecorder()

		srv.handleSystemImagesBuild(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusOK, w.Body.String())
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) && srv.imageBuildActive.Load() {
			time.Sleep(20 * time.Millisecond)
		}
		if srv.imageBuildActive.Load() {
			t.Fatal("build did not finish: imageBuildActive still set")
		}

		gotImages := rec.snapshotDoneImages()
		if len(gotImages) != len(wantImages) {
			t.Fatalf("done images = %v, want %v", gotImages, wantImages)
		}
		for i := range wantImages {
			if gotImages[i] != wantImages[i] {
				t.Errorf("done image[%d] = %q, want %q", i, gotImages[i], wantImages[i])
			}
		}
	})

	t.Run("emits configured image from config.yaml rather than synthesized name", func(t *testing.T) {
		synthFS := fstest.MapFS{
			"gemini-cli/Dockerfile":  {Data: []byte("FROM scion-base:latest\n")},
			"gemini-cli/config.yaml": {Data: []byte("harness: gemini-cli\nimage: custom.registry.example/gemini-agent:v9\n")},
			"no-image/Dockerfile":    {Data: []byte("FROM scion-base:latest\n")},
			"no-image/config.yaml":   {Data: []byte("harness: no-image\n")},
			"no-docker/config.yaml":  {Data: []byte("harness: no-docker\nimage: scion-no-docker:latest\n")},
			"gen/main.go":            {Data: []byte("package main\n")},
		}
		images := bundledHarnessBuildImagesFromFS(synthFS)
		if len(images) != 1 || images["gemini-cli"] != "custom.registry.example/gemini-agent:v9" {
			t.Fatalf("bundledHarnessBuildImagesFromFS = %v, want only gemini-cli -> custom.registry.example/gemini-agent:v9", images)
		}

		rec := &imageBuildEventRecorder{}
		srv := &Server{ctx: context.Background(), events: rec}
		srv.publishImageBuildCompletion(
			imageBuildSubject("u-build", "job-custom"),
			[]string{"gemini-cli"},
			images,
		)

		gotImages := rec.snapshotDoneImages()
		if len(gotImages) != 1 || gotImages[0] != "custom.registry.example/gemini-agent:v9" {
			t.Fatalf("done images = %v, want [custom.registry.example/gemini-agent:v9]", gotImages)
		}
	})
}
