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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
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
	repoDir   string
	unit      string
	calls     string
	stubDir   string
	installer maintenance.Installer
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
	writeRebuildStubs(t, f.stubDir, f.calls)
	t.Setenv("PATH", f.stubDir+":"+os.Getenv("PATH"))
	f.installer = rebuildTestInstaller(t, f.repoDir, filepath.Join(f.repoDir, "installed-scion"))
	return f
}

// rebuildStubCommit is the commit the git stub reports for rev-parse HEAD
// and the go stub's build reports for version --format json.
const rebuildStubCommit = "0123456789abcdef0123456789abcdef01234567"

// writeRebuildStubs writes git, make, go and sudo stubs into dir. Each
// records its call in calls and, as its very last action, that it is
// exiting. git answers rev-parse, check-ref-format --branch (echoing a
// valid name, as git does) and version; go writes, at its -o path, a binary
// whose version JSON reports rebuildStubCommit; sudo succeeds (so every
// `sudo -n -l` rule check matches). Only shell builtins and absolute paths
// are used, so a test may leave PATH holding just dir.
func writeRebuildStubs(t *testing.T, dir, calls string) {
	t.Helper()
	stubs := map[string]string{
		"git": fmt.Sprintf(`echo 'git' "$@" >> '%[1]s'
case "$1 $2" in
  "rev-parse HEAD") echo %[2]s ;;
  "rev-parse --abbrev-ref") echo main ;;
  "check-ref-format --branch") echo "$3" ;;
  "version ") echo "git version 2.43.0" ;;
esac
echo 'exited git' "$@" >> '%[1]s'
`, calls, rebuildStubCommit),
		"make": fmt.Sprintf(`echo 'make' "$@" >> '%[1]s'
echo 'exited make' "$@" >> '%[1]s'
`, calls),
		"go": fmt.Sprintf(`echo 'go' "$@" >> '%[1]s'
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; shift; fi
  shift
done
echo '#!/bin/sh' > "$out"
echo "echo '{\"version\":\"\",\"commit\":\"%[2]s\"}'" >> "$out"
/bin/chmod 755 "$out"
echo 'exited go' >> '%[1]s'
`, calls, rebuildStubCommit),
		"sudo": fmt.Sprintf(`echo 'sudo' "$@" >> '%[1]s'
echo 'exited sudo' "$@" >> '%[1]s'
`, calls),
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// rebuildTestInstaller detects the installer the way the hub does at
// startup, for a starter hub: no /opt/scion layout, the repo path set and
// the sudo stub on PATH accepting the rule checks, so sudo-install.
func rebuildTestInstaller(t *testing.T, repo, binaryDest string) maintenance.Installer {
	t.Helper()
	inst := maintenance.Detect(context.Background(), maintenance.DetectOptions{
		InstallRoot: filepath.Join(t.TempDir(), "no-opt-scion"),
		RepoPath:    repo,
		BinaryDest:  binaryDest,
		ServiceName: "scion-hub",
	})
	if inst.Name() != maintenance.StrategySudoInstall {
		t.Fatalf("installer = %s (%s), want sudo-install", inst.Name(), inst.Status().Reason)
	}
	return inst
}

// options returns the fixture's executor options; a nil locate reports the
// fixture's unit file.
func (f *rebuildUnitFixture) options(locate unitdrift.Locator) RebuildServerOptions {
	if locate == nil {
		unit := f.unit
		locate = func(context.Context, string) (unitdrift.UnitInfo, error) {
			return unitdrift.UnitInfo{FragmentPath: unit}, nil
		}
	}
	return RebuildServerOptions{
		RepoPath:     f.repoDir,
		RepoBranch:   "main",
		ServiceName:  "scion-hub",
		Installer:    f.installer,
		UnitLocator:  locate,
		UnitRevision: func(context.Context, string) (string, error) { return "abc1234", nil },
	}
}

func (f *rebuildUnitFixture) executor(locate unitdrift.Locator) *RebuildServerExecutor {
	return NewRebuildServerExecutor(f.options(locate))
}

// restartIssued reports whether the sudo-install restart
// (`sudo -n systemctl restart --no-block scion-hub`, run to completion
// before Run returns) ran and exited.
func (f *rebuildUnitFixture) restartIssued(t *testing.T) bool {
	t.Helper()
	data, _ := os.ReadFile(f.calls)
	return strings.Contains(string(data), "exited sudo -n systemctl restart --no-block scion-hub")
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
	complete := strings.Index(log, "Server rebuild installed; requesting the restart.")
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
	if !f.restartIssued(t) {
		t.Fatal("restart not issued")
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
			o := f.options(locate)
			o.UnitTimeout = 100 * time.Millisecond
			e := NewRebuildServerExecutor(o)
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
	// The sudo stub refuses the restart (as systemctl would on a broken
	// unit); every other sudo command succeeds.
	sudo := fmt.Sprintf("#!/bin/sh\necho 'sudo' \"$@\" >> '%s'\ncase \"$*\" in \"-n systemctl restart\"*) exit 1 ;; esac\n", f.calls)
	if err := os.WriteFile(filepath.Join(f.stubDir, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := f.executor(nil).Run(context.Background(), &buf, nil)
	if err == nil || !strings.Contains(err.Error(), "restarting the hub failed") ||
		!strings.Contains(err.Error(), "cannot be rolled back by the sudo-install installer") {
		t.Fatalf("want a restart failure that keeps the installed build, got %v", err)
	}
	if !strings.Contains(buf.String(), "\n==> Systemd unit check\n"+unitdrift.DriftWarning+"\n") {
		t.Fatalf("drift block missing on the restart-failure path:\n%s", buf.String())
	}
}

// The runner records the run log in the before-restart hook, and nothing
// written after it survives the restart, so the unit report must already
// be in the log the hook sees.
func TestRebuildServerExecutor_UnitReportInLogRecordedBeforeRestart(t *testing.T) {
	f := newRebuildUnitFixture(t, renderedRebuildUnit("ExecStartPre=/bin/true\n"))
	var buf syncBuffer
	var atHook string
	var pending maintenance.PendingRestart
	err := f.executor(nil).RunWithBeforeRestart(context.Background(), &buf, nil,
		func(_ context.Context, p maintenance.PendingRestart) error {
			atHook, pending = buf.String(), p
			if f.restartIssued(t) {
				t.Error("restart ran before the hook")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !f.restartIssued(t) {
		t.Fatal("restart not issued")
	}
	for _, want := range []string{
		"\nServer rebuild installed; requesting the restart.\n",
		"\n==> Systemd unit check\n" + unitdrift.DriftWarning + "\n",
		"  installed: ExecStartPre=/bin/true\n",
	} {
		if !strings.Contains(atHook, want) {
			t.Errorf("log at the hook missing %q:\n%s", want, atHook)
		}
	}
	if pending.Mode != maintenance.UpdateModeGit || pending.Commit != rebuildStubCommit ||
		pending.To != maintenance.GitReleaseName(rebuildStubCommit) || pending.Strategy != maintenance.StrategySudoInstall {
		t.Fatalf("pending restart = %+v", pending)
	}
}
