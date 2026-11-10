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

package runtimebroker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// signedBootstrapDoneRecords returns every JSON log record whose message is
// the signed bootstrap's success line.
func signedBootstrapDoneRecords(logs string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == msgSignedBootstrapDone {
			out = append(out, r)
		}
	}
	return out
}

// signedBootstrapMaxElapsed is a loose upper bound on a test bootstrap's
// logged elapsed time, well under the test timeout: a duration measured
// from the wrong start is far above it.
const signedBootstrapMaxElapsed = time.Minute

// requireOneSignedBootstrapDone asserts that logs hold exactly one success
// line, at Info, for agent name, with the file count, byte total and an
// elapsed time between 0 and signedBootstrapMaxElapsed.
func requireOneSignedBootstrapDone(t *testing.T, logs, name string, files, bytes int) {
	t.Helper()
	recs := signedBootstrapDoneRecords(logs)
	if len(recs) != 1 {
		t.Fatalf("success lines = %d, want exactly 1:\n%s", len(recs), logs)
	}
	r := recs[0]
	if r["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", r["level"])
	}
	if r["name"] != name {
		t.Errorf("name = %v, want %q", r["name"], name)
	}
	if r["files"] != float64(files) {
		t.Errorf("files = %v, want %d", r["files"], files)
	}
	if r["bytes"] != float64(bytes) {
		t.Errorf("bytes = %v, want %d", r["bytes"], bytes)
	}
	elapsed, _ := r["elapsed"].(string)
	d, err := time.ParseDuration(elapsed)
	if err != nil || d < 0 || d >= signedBootstrapMaxElapsed {
		t.Errorf("elapsed = %v (parse err %v), want a duration in [0, %v)", r["elapsed"], err, signedBootstrapMaxElapsed)
	}
}

// requireNoSignedBootstrapDone asserts that logs hold no success line.
func requireNoSignedBootstrapDone(t *testing.T, logs string) {
	t.Helper()
	if n := len(signedBootstrapDoneRecords(logs)); n != 0 {
		t.Fatalf("success lines = %d, want 0:\n%s", n, logs)
	}
}

// bootstrapLogLeaks returns the signed-URL and path material found in text:
// an X-Goog- query parameter, the object store's address, any issued URL,
// any listed object path, a stage name, or any of dirs.
func bootstrapLogLeaks(text string, h *fakeSignedHub, dirs []string) []string {
	found := urlLeaks(text, h.issued)
	needles := append([]string{strings.TrimPrefix(h.srv.URL, "http://"), bootstrapStageInfix}, dirs...)
	for p := range h.objects {
		needles = append(needles, p)
	}
	for _, n := range needles {
		if n != "" && strings.Contains(text, n) {
			found = append(found, n)
		}
	}
	return found
}

// TestSignedBootstrap_LogsOneInfoLine: a successful signed bootstrap of a
// per-agent workspace and of a project workspace, synchronous and async,
// logs exactly one Info line with the agent name, the file count, the byte
// total and the duration.
func TestSignedBootstrap_LogsOneInfoLine(t *testing.T) {
	objects := map[string]string{"README.md": "hello", "src/app.go": "package app"}
	for _, project := range []bool{false, true} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("project=%v/async=%v", project, async), func(t *testing.T) {
				srv, rtb := newAsyncTestServer(t, newAsyncManager())
				srv.config.WorktreeBase = t.TempDir()
				srv.signedDownload.sleep = noSleep
				forbidGCSDownload(t, srv)
				h := newFakeSignedHub(t, objects)
				wireSignedHub(rtb, h)
				rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
					return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
				}
				logs := captureLifecycleJSONLog(srv)
				name := fmt.Sprintf("agent-done-%v-%v", project, async)
				body := signedCreate(name, api.WorkspaceDownloadModeSignedURLs, async)
				if project {
					body["projectSlug"] = "done-" + fmt.Sprint(async)
				}
				w := postCreate(t, srv, body)
				if w.Code >= 300 {
					t.Fatalf("create = %d: %s", w.Code, w.Body.String())
				}
				if !waitUntil(t, 5*time.Second, func() bool { return len(signedBootstrapDoneRecords(logs.String())) > 0 }) {
					t.Fatalf("no success line logged:\n%s", logs.String())
				}
				requireOneSignedBootstrapDone(t, logs.String(), name, 2, len("hello")+len("package app"))
			})
		}
	}
}

// TestSignedBootstrap_LogCarriesNoURLOrPath plants V4-shaped signed URLs, a
// query string and distinctive object paths in the fake Hub's pages and
// asserts that none of them, nor the object store's address, a stage name or
// the workspace directory, appears anywhere in the broker log, for a
// successful bootstrap and a failed one. The detector is checked on planted
// values first.
func TestSignedBootstrap_LogCarriesNoURLOrPath(t *testing.T) {
	objects := map[string]string{"planted-dir/planted-file.txt": "content", "top-planted.md": "x"}

	t.Run("detector control", func(t *testing.T) {
		h := newFakeSignedHub(t, objects)
		if _, err := h.fetch(t.Context(), CreateAgentRequest{}, ""); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		for name, planted := range map[string]string{
			"issued URL":   h.issued[0],
			"query only":   "q=" + h.issued[0][strings.Index(h.issued[0], "?")+1:],
			"object path":  "planted-dir/planted-file.txt",
			"store host":   strings.TrimPrefix(h.srv.URL, "http://"),
			"workspace":    filepath.Join(dir, "workspace"),
			"stage suffix": ".workspace" + bootstrapStageInfix + "0123",
		} {
			if len(bootstrapLogLeaks(`{"msg":"x","v":"`+planted+`"}`, h, []string{dir})) == 0 {
				t.Errorf("%s: planted %q not detected", name, planted)
			}
		}
		if leaks := bootstrapLogLeaks(`{"msg":"`+msgSignedBootstrapDone+`","files":2,"bytes":8,"elapsed":"1ms"}`, h, []string{dir}); len(leaks) != 0 {
			t.Errorf("clean line detected: %v", leaks)
		}
	})
	if t.Failed() {
		t.FailNow()
	}

	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			srv, rtb := newAsyncTestServer(t, newAsyncManager())
			srv.config.WorktreeBase = t.TempDir()
			srv.signedDownload.sleep = noSleep
			forbidGCSDownload(t, srv)
			h := newFakeSignedHub(t, objects)
			if fail {
				// A 404 fails closed: the listing is stale. The error body
				// quotes the URL.
				h.objectStatus = func(p string, _ int) int {
					if p == "top-planted.md" {
						return http.StatusNotFound
					}
					return 0
				}
			}
			wireSignedHub(rtb, h)
			logs := captureLifecycleJSONLog(srv)
			name := fmt.Sprintf("agent-noleak-%v", fail)
			w := postCreate(t, srv, signedCreate(name, api.WorkspaceDownloadModeSignedURLs, false))
			if fail != (w.Code >= 300) {
				t.Fatalf("create = %d (want failure %v): %s", w.Code, fail, w.Body.String())
			}
			if len(h.issued) == 0 {
				t.Fatal("the fake Hub must issue signed URLs")
			}
			text := logs.String()
			if fail {
				requireNoSignedBootstrapDone(t, text)
				if findLogRecord(text, "GCS workspace bootstrap failed", nil) == nil {
					t.Fatalf("the failure must still be logged:\n%s", text)
				}
			} else {
				requireOneSignedBootstrapDone(t, text, name, 2, len("content")+len("x"))
			}
			if leaks := bootstrapLogLeaks(text, h, []string{srv.config.WorktreeBase}); len(leaks) != 0 {
				t.Errorf("broker log leaks %v:\n%s", leaks, text)
			}
		})
	}
}

// TestSignedBootstrap_NoSuccessLineWithoutCompletion: a signed bootstrap
// that is skipped, or that fails after its download, logs no success line.
func TestSignedBootstrap_NoSuccessLineWithoutCompletion(t *testing.T) {
	t.Run("case A skip", func(t *testing.T) {
		f := newCaseAFixture(t, map[string]string{"a.txt": "A"})
		f.writeMarker(t, "m")
		if err := os.WriteFile(filepath.Join(f.w, "theirs.txt"), []byte("t"), 0o644); err != nil {
			t.Fatal(err)
		}
		logs := captureLifecycleJSONLog(f.srv)
		if skipped, err := f.run(); err != nil || !skipped {
			t.Fatalf("run = (skipped %v, err %v), want a skip", skipped, err)
		}
		requireNoSignedBootstrapDone(t, logs.String())
	})
	t.Run("case A failure after the final rename", func(t *testing.T) {
		f := newCaseAFixture(t, map[string]string{"a.txt": "A"})
		f.writeMarker(t, "m")
		logs := captureLifecycleJSONLog(f.srv)
		crashAt(t, "after_rename")
		if _, err := f.run(); !errors.Is(err, errSignedBootstrapCrash) {
			t.Fatalf("err = %v, want the injected failure", err)
		}
		if f.h.pageCalls == 0 {
			t.Fatal("the download must have run")
		}
		requireNoSignedBootstrapDone(t, logs.String())
	})
	t.Run("case C failure while applying", func(t *testing.T) {
		srv, rtb := newAsyncTestServer(t, newAsyncManager())
		srv.config.WorktreeBase = t.TempDir()
		srv.signedDownload.sleep = noSleep
		forbidGCSDownload(t, srv)
		h := newFakeSignedHub(t, map[string]string{"a.txt": "A"})
		wireSignedHub(rtb, h)
		logs := captureLifecycleJSONLog(srv)
		crashAt(t, "apply_file")
		w := postCreate(t, srv, signedCreate("agent-apply-fails", api.WorkspaceDownloadModeSignedURLs, false))
		if w.Code < 300 {
			t.Fatalf("create = %d, want a failure: %s", w.Code, w.Body.String())
		}
		if h.pageCalls == 0 {
			t.Fatal("the download must have run")
		}
		requireNoSignedBootstrapDone(t, logs.String())
	})
}
