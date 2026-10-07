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
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

const driftHCBody = "harness: claude\nimage: scion-claude:test\n"

func writeDriftFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// driftEnv returns a server logging to the returned buffer, the global
// .scion dir (where broker-local harness-configs live in the test env) and a
// hub-hydrated copy of "hc" holding config.yaml and provision.py.
func driftEnv(t *testing.T) (*Server, *bytes.Buffer, string, string) {
	t.Helper()
	srv, _, dotScion := dispatchTestEnv(t, false)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))
	hydrated := filepath.Join(t.TempDir(), "cache", "hc")
	writeDriftFile(t, filepath.Join(hydrated, "config.yaml"), driftHCBody)
	writeDriftFile(t, filepath.Join(hydrated, "provision.py"), "print('v2')\n")
	return srv, &logBuf, dotScion, hydrated
}

func writeLocalHC(t *testing.T, dotScion string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(dotScion, "harness-configs", "hc")
	for name, body := range files {
		writeDriftFile(t, filepath.Join(dir, name), body)
	}
	return dir
}

func driftWarned(logBuf *bytes.Buffer) bool {
	out := logBuf.String()
	return strings.Contains(out, "level=WARN") && strings.Contains(out, "Harness-config drift")
}

func TestHarnessConfigDrift_WarnsWhenContentDiffers(t *testing.T) {
	srv, logBuf, dotScion, hydrated := driftEnv(t)
	local := writeLocalHC(t, dotScion, map[string]string{
		"config.yaml":  driftHCBody,
		"provision.py": "print('v1')\n",
	})

	srv.warnHarnessConfigDrift("agent-1", "hc", hydrated, "", "sha256:record")

	if !driftWarned(logBuf) {
		t.Fatalf("expected a drift WARN, got log: %s", logBuf.String())
	}
	out := logBuf.String()
	for _, want := range []string{"hub_hydrated_path=" + hydrated, "on_disk_path=" + local, "harness_config=hc", "agent_id=agent-1",
		"hydrated_content_hash=sha256:", "on_disk_content_hash=sha256:", "dispatch_content_hash=sha256:record"} {
		if !strings.Contains(out, want) {
			t.Errorf("drift WARN missing %q: %s", want, out)
		}
	}
}

func TestHarnessConfigDrift_NoWarnWhenEqual(t *testing.T) {
	srv, logBuf, dotScion, hydrated := driftEnv(t)
	writeLocalHC(t, dotScion, map[string]string{
		"config.yaml":  driftHCBody,
		"provision.py": "print('v2')\n",
	})

	srv.warnHarnessConfigDrift("agent-1", "hc", hydrated, "", "")

	if driftWarned(logBuf) {
		t.Errorf("no drift WARN expected for equal content, got log: %s", logBuf.String())
	}
}

// Line endings are normalized before hashing, as the hub does when it
// stores a harness-config, so a CRLF copy of the same content is not drift.
func TestHarnessConfigDrift_NoWarnForCRLFOnlyDifference(t *testing.T) {
	srv, logBuf, dotScion, hydrated := driftEnv(t)
	writeLocalHC(t, dotScion, map[string]string{
		"config.yaml":  strings.ReplaceAll(driftHCBody, "\n", "\r\n"),
		"provision.py": "print('v2')\r\n",
	})

	srv.warnHarnessConfigDrift("agent-1", "hc", hydrated, "", "")

	if driftWarned(logBuf) {
		t.Errorf("no drift WARN expected for a line-ending-only difference, got log: %s", logBuf.String())
	}
}

// Upgrade backups and atomic-write leftovers are not harness-config content.
func TestHarnessConfigDrift_NoWarnForTransientFilesOnly(t *testing.T) {
	srv, logBuf, dotScion, hydrated := driftEnv(t)
	writeLocalHC(t, dotScion, map[string]string{
		"config.yaml":                      driftHCBody,
		"provision.py":                     "print('v2')\n",
		"config.yaml.bak.20260915T101112Z": "old content\n",
		".provision.py.tmp-12345":          "partial\n",
		// A transient directory is pruned with everything below it.
		"sub.bak.20260915T101112Z/x": "backup dir content\n",
	})

	srv.warnHarnessConfigDrift("agent-1", "hc", hydrated, "", "")

	if driftWarned(logBuf) {
		t.Errorf("no drift WARN expected when only transient files differ, got log: %s", logBuf.String())
	}
}

func TestHarnessConfigDrift_NoWarnWhenNoLocalCopy(t *testing.T) {
	srv, logBuf, _, hydrated := driftEnv(t)

	srv.warnHarnessConfigDrift("agent-1", "hc", hydrated, "", "")

	if driftWarned(logBuf) {
		t.Errorf("no drift WARN expected without an on-disk copy, got log: %s", logBuf.String())
	}
}

// The hash matches the hub's content hash for the same files, so equal
// content compares equal to a hub record's content_hash.
func TestHubCompatibleContentHash_MatchesTransferHash(t *testing.T) {
	dir := t.TempDir()
	writeDriftFile(t, filepath.Join(dir, "config.yaml"), driftHCBody)
	writeDriftFile(t, filepath.Join(dir, "sub", "provision.py"), "print('x')\n")
	files, err := transfer.CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := transfer.ComputeContentHash(files)
	got, err := hubCompatibleContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("hubCompatibleContentHash = %s, want %s", got, want)
	}
}

// buildStartContext runs the drift check for a hub-hydrated harness-config.
func TestBuildStartContext_HarnessConfigDriftWarn(t *testing.T) {
	srv, logBuf, dotScion, hydrated := driftEnv(t)
	writeLocalHC(t, dotScion, map[string]string{"config.yaml": driftHCBody})

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Operation: opCreate,
		AgentID:   "agent-1",
		Name:      "agent-1",
		Config:    &CreateAgentConfig{HarnessConfig: "hc"},
		Prehydrated: prehydratedBundle{
			HarnessConfigDone: true,
			HarnessConfigPath: hydrated,
		},
	})

	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	if !driftWarned(logBuf) {
		t.Errorf("expected a drift WARN from buildStartContext, got log: %s", logBuf.String())
	}
}

// TestSameDir covers warnHarnessConfigDrift's short-circuit when the
// broker-local copy found by name is the hydrated directory itself (the same
// path, or a symlink to it).
func TestSameDir(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !sameDir(dir, dir) || !sameDir(dir, link) {
		t.Error("expected the same directory (directly and via a symlink) to match")
	}
	if sameDir(dir, other) {
		t.Error("expected different directories not to match")
	}
}
