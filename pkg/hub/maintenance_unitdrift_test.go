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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/unitdrift"
)

const rebuildUnitTemplate = `[Unit]
Description=Scion Hub API Server

[Service]
WorkingDirectory=@REPO_DIR@
ExecStart=@SCION_BIN@ --global server start --enable-hub
Restart=always
`

// rebuildUnitFixture is a fake checkout holding the unit template, an
// installed unit file and stub git/make/go/sudo commands that record their
// calls.
type rebuildUnitFixture struct {
	repoDir string
	unit    string
	calls   string
	stubDir string
}

func newRebuildUnitFixture(t *testing.T, installed string) *rebuildUnitFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("rebuild-server only runs on linux")
	}
	f := &rebuildUnitFixture{
		repoDir: t.TempDir(),
		unit:    filepath.Join(t.TempDir(), "scion-hub.service"),
		calls:   filepath.Join(t.TempDir(), "commands.log"),
		stubDir: t.TempDir(),
	}
	tmpl := filepath.Join(f.repoDir, unitdrift.TemplatePath)
	if err := os.MkdirAll(filepath.Dir(tmpl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpl, []byte(rebuildUnitTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.unit, []byte(installed), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"git", "make", "go", "sudo"} {
		script := fmt.Sprintf("#!/bin/sh\necho '%s' \"$@\" >> '%s'\n", cmd, f.calls)
		if err := os.WriteFile(filepath.Join(f.stubDir, cmd), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", f.stubDir+":"+os.Getenv("PATH"))
	return f
}

func (f *rebuildUnitFixture) executor(locate unitdrift.Locator) *RebuildServerExecutor {
	e := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:    f.repoDir,
		RepoBranch:  "main",
		BinaryDest:  filepath.Join(f.repoDir, "installed-scion"),
		ServiceName: "scion-hub",
	})
	if locate == nil {
		unit := f.unit
		locate = func(context.Context, string) (unitdrift.UnitInfo, error) {
			return unitdrift.UnitInfo{FragmentPath: unit}, nil
		}
	}
	e.unitLocator = locate
	e.unitRevision = func(context.Context, string) (string, error) { return "abc1234", nil }
	return e
}

// restartIssued waits briefly for the fire-and-forget restart stub to log.
func (f *rebuildUnitFixture) restartIssued(t *testing.T) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(f.calls)
		if strings.Contains(string(data), "sudo systemctl restart scion-hub") {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func renderedRebuildUnit(extra string) string {
	s := strings.ReplaceAll(rebuildUnitTemplate, "@REPO_DIR@", "/home/scion/scion")
	s = strings.ReplaceAll(s, "@SCION_BIN@", "/usr/local/bin/scion")
	return strings.Replace(s, "Restart=always", extra+"Restart=always", 1)
}

func TestRebuildServerExecutor_UnitDriftReportedAtLogTail(t *testing.T) {
	f := newRebuildUnitFixture(t, renderedRebuildUnit("ExecStartPre=/bin/true\n"))
	var buf bytes.Buffer
	if err := f.executor(nil).Run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("drift must not fail the run: %v", err)
	}
	if !f.restartIssued(t) {
		t.Fatal("restart not issued")
	}
	log := buf.String()
	complete := strings.Index(log, "Server rebuild complete, restart initiated.")
	block := strings.Index(log, unitdrift.DriftWarning)
	if complete < 0 || block < 0 || block < complete {
		t.Fatalf("drift block must follow the completion line:\n%s", log)
	}
	wantTail := "\n==> Systemd unit check\n" + unitdrift.DriftWarning + "\n"
	if !strings.Contains(log, wantTail) {
		t.Fatalf("missing drift block header:\n%s", log)
	}
	for _, want := range []string{
		"commit abc1234 (branch main)",
		"not on the hub VM",
		"--no-tls",
		"  installed: ExecStartPre=/bin/true\n",
	} {
		if !strings.Contains(log[block:], want) {
			t.Errorf("drift block missing %q:\n%s", want, log[block:])
		}
	}
	if !strings.HasSuffix(log, "  installed: ExecStartPre=/bin/true\n") {
		t.Errorf("drift block must be the end of the log:\n%s", log)
	}
	if !strings.Contains(log, "==> Checking systemd unit against template (result at end of log)") {
		t.Errorf("missing step line:\n%s", log)
	}
}

func TestRebuildServerExecutor_UnitMatch(t *testing.T) {
	f := newRebuildUnitFixture(t, renderedRebuildUnit(""))
	var buf bytes.Buffer
	if err := f.executor(nil).Run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	log := buf.String()
	if !strings.HasSuffix(log, "==> Systemd unit check\nSystemd unit matches template.\n") {
		t.Fatalf("log should end with the match line:\n%s", log)
	}
	if strings.Contains(log, "WARNING") {
		t.Fatalf("unexpected warning:\n%s", log)
	}
}

func TestRebuildServerExecutor_UnitCheckFailuresStillRestart(t *testing.T) {
	cases := map[string]unitdrift.Locator{
		"locator error": func(context.Context, string) (unitdrift.UnitInfo, error) {
			return unitdrift.UnitInfo{}, fmt.Errorf("no systemd")
		},
		"locator panics": func(context.Context, string) (unitdrift.UnitInfo, error) {
			panic("boom")
		},
		"locator blocks": func(ctx context.Context, _ string) (unitdrift.UnitInfo, error) {
			<-ctx.Done()
			return unitdrift.UnitInfo{}, ctx.Err()
		},
	}
	for name, locate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRebuildUnitFixture(t, renderedRebuildUnit("ExecStartPre=/bin/true\n"))
			e := f.executor(locate)
			e.unitTimeout = 100 * time.Millisecond
			var buf bytes.Buffer
			if err := e.Run(context.Background(), &buf, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !f.restartIssued(t) {
				t.Fatal("restart not issued")
			}
			log := buf.String()
			if !strings.Contains(log, "==> Systemd unit check\nSystemd unit check skipped: ") || strings.Contains(log, "WARNING") {
				t.Fatalf("want a skipped report:\n%s", log)
			}
		})
	}
}

func TestRebuildServerExecutor_UnitReportWrittenWhenRestartFails(t *testing.T) {
	f := newRebuildUnitFixture(t, renderedRebuildUnit("ExecStartPre=/bin/true\n"))
	// The sudo stub removes itself after the install step, and PATH holds
	// only the stubs, so starting the restart fails.
	sudo := fmt.Sprintf("#!/bin/sh\necho 'sudo' \"$@\" >> '%s'\n/bin/rm -f \"$0\"\n", f.calls)
	if err := os.WriteFile(filepath.Join(f.stubDir, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.stubDir)
	var buf bytes.Buffer
	err := f.executor(nil).Run(context.Background(), &buf, nil)
	if err == nil || !strings.Contains(err.Error(), "restarting service failed") {
		t.Fatalf("want restart failure, got %v", err)
	}
	if !strings.Contains(buf.String(), "\n==> Systemd unit check\n"+unitdrift.DriftWarning+"\n") {
		t.Fatalf("drift block missing on the restart-failure path:\n%s", buf.String())
	}
}
