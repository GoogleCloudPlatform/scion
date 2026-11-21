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

//go:build linux

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/rebuildpreflight"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/unitdrift"
	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
)

// newPreflightFixture creates a rebuildFixture whose stub `scion` binary
// executes preflightScript on `server preflight` (and reports --colocated-broker-id
// on `--help` unless probeFail is true).
func newPreflightFixture(t *testing.T, probeFail bool, preflightScript string) *rebuildFixture {
	t.Helper()
	f := &rebuildFixture{
		repo:  t.TempDir(),
		calls: filepath.Join(t.TempDir(), "calls.log"),
	}
	stubs := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("git", fmt.Sprintf(`echo "git $*" >> '%s'
case "$1 $2" in
  "rev-parse HEAD") echo %s ;;
  "rev-parse --abbrev-ref") echo main ;;
  "check-ref-format --branch") echo "$3" ;;
  "version ") echo "git version 2.43.0" ;;
esac
`, f.calls, testCommit))
	write("make", fmt.Sprintf("echo \"make $*\" >> '%s'\n", f.calls))

	helpLine := `echo "Usage: scion server preflight --colocated-broker-id"; exit 0`
	if probeFail {
		helpLine = `echo "Usage: scion server (old)"; exit 0`
	}
	scriptPath := filepath.Join(t.TempDir(), "preflight.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"+preflightScript+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	write("go", fmt.Sprintf(`echo "go $*" >> '%s'
out=""; ld=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -ldflags) ld="$2"; shift 2 ;;
    *) shift ;;
  esac
done
commit=$(printf '%%s\n' "$ld" | sed -n 's/.*pkg\/version\.Commit=\([0-9a-f]*\).*/\1/p')
{
  echo '#!/bin/sh'
  echo 'if [ "$1" = "server" ] && [ "$2" = "preflight" ]; then'
  echo '  if [ "$3" = "--help" ]; then %s; fi'
  echo '  exec "%s" "$@"'
  echo 'fi'
  echo "echo '{\"version\":\"\",\"commit\":\"$commit\"}'"
} > "$out"
chmod 755 "$out"
`, f.calls, helpLine, scriptPath))
	t.Setenv("PATH", stubs+":"+os.Getenv("PATH"))
	return f
}

type recordingOverrideSink struct {
	events []*rebuildpreflight.MaintenanceOverrideEvent
	err    error
}

func (s *recordingOverrideSink) LogMaintenanceOverrideEvent(_ context.Context, ev *rebuildpreflight.MaintenanceOverrideEvent) error {
	s.events = append(s.events, ev)
	return s.err
}

func TestRebuildServer_PreflightBlockingStopsBeforeInstall(t *testing.T) {
	f := newPreflightFixture(t, false, `
echo '{"version":1,"checks":["settings.profiles"],"findings":[{"check":"settings.profiles","code":"settings.profiles.missing","severity":"block","message":"missing dev","data":{"profile":"dev","project":"p1"}}],"blocking":1,"warnings":0}'
exit 3
`)
	inst, root, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	ex := f.executor(inst)

	var log bytes.Buffer
	err := ex.Run(context.Background(), &log, nil)
	if err == nil || !strings.Contains(err.Error(), "1 blocking finding(s)") {
		t.Fatalf("Run = %v, want blocking preflight error", err)
	}
	if *signals != 0 {
		t.Error("S24: restart ran despite blocking preflight")
	}
	if target := readLink(t, root, currentLinkName); target != filepath.Join(releasesDirName, "nightly-20261022") {
		t.Errorf("S24: install activated despite blocking preflight: current -> %q", target)
	}
	res := ex.RunResult()
	if res["commit"] != testCommit || res["branch"] != "main" {
		t.Errorf("result missing commit/branch: %+v", res)
	}
	if res["preflight"] == nil {
		t.Errorf("result missing preflight report: %+v", res)
	}
}

func TestRebuildServer_PreflightNotReadyFailsClosed(t *testing.T) {
	f := newPreflightFixture(t, false, `exit 0`)
	inst, _, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	ex := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:       f.repo,
		Installer:      inst,
		CurrentRelease: "nightly-20261022",
		UnitLocator: func(context.Context, string) (unitdrift.UnitInfo, error) {
			return unitdrift.UnitInfo{}, errors.New("none")
		},
		// ColocatedBrokerID intentionally nil (CN7 / S87): must fail closed as not_ready.
	})
	var log bytes.Buffer
	err := ex.Run(context.Background(), &log, nil)
	if err == nil || !strings.Contains(err.Error(), "not_ready") {
		t.Fatalf("Run = %v, want not_ready", err)
	}
	if *signals != 0 {
		t.Error("restart ran when not_ready")
	}
}

func TestRebuildServer_RelativeConfigPathRefused(t *testing.T) {
	f := newPreflightFixture(t, false, `exit 0`)
	inst, _, _ := symlinkInstallerForRebuild(t, "nightly-20261022")
	ex := f.executor(inst)
	ex.configPath = "relative/server.yaml"
	err := ex.Run(context.Background(), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("Run = %v, want relative ConfigPath refusal", err)
	}
}

func TestRebuildServer_ForcedRunNilCheckpointFailsClosed(t *testing.T) {
	f := newPreflightFixture(t, false, `
echo '{"version":1,"checks":["settings.profiles"],"findings":[{"check":"settings.profiles","code":"settings.profiles.missing","severity":"block","message":"missing dev","data":{"profile":"dev","project":"p1"}}],"blocking":1,"warnings":0}'
exit 3
`)
	inst, _, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	ex := f.executor(inst)
	ex.SetForceGrant(&rebuildpreflight.ForceGrant{
		RerunOf:      "run-failed-1",
		ActorID:      "user-1",
		ActorEmail:   "admin@example.com",
		PinnedBranch: "main",
		PinnedCommit: testCommit,
		FailedFindings: []preflight.Finding{{
			Check:    "settings.profiles",
			Code:     "settings.profiles.missing",
			Severity: preflight.SeverityBlock,
			Message:  "missing dev",
			Data:     map[string]any{"profile": "dev", "project": "p1"},
		}},
	})
	// No checkpoint set (CN-e / S99).
	err := ex.Run(context.Background(), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no checkpoint") {
		t.Fatalf("S99: Run = %v, want nil checkpoint refusal", err)
	}
	if *signals != 0 {
		t.Error("S99: installed/restarted without checkpoint")
	}
}

func TestRebuildServer_ForcedRunCheckpointsEmitsAuditAndInstalls(t *testing.T) {
	secretDSN := "postgres://hub:s3cr3t-exec-pw@db.internal/hub"
	f := newPreflightFixture(t, false, `
echo "debug stderr leaked $SCION_PREFLIGHT_DB" >&2
echo '{"version":1,"checks":["settings.profiles"],"findings":[{"check":"settings.profiles","code":"settings.profiles.missing","severity":"block","message":"missing dev","data":{"profile":"dev","project":"p1"}}],"blocking":1,"warnings":0}'
exit 3
`)
	inst, _, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	sink := &recordingOverrideSink{}
	ex := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:       f.repo,
		Installer:      inst,
		CurrentRelease: "nightly-20261022",
		UnitLocator: func(context.Context, string) (unitdrift.UnitInfo, error) {
			return unitdrift.UnitInfo{}, errors.New("none")
		},
		ColocatedBrokerID: func(context.Context) (string, bool, error) {
			return "broker-1", true, nil
		},
		DatabaseDSN:    secretDSN,
		DatabaseDriver: "postgres",
		Audit:          sink,
		RunID:          "run-forced-2",
		Now:            func() time.Time { return time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC) },
	})
	ex.SetForceGrant(&rebuildpreflight.ForceGrant{
		RerunOf:      "run-failed-1",
		ActorID:      "user-42",
		ActorEmail:   "admin@example.com",
		PinnedBranch: "main",
		PinnedCommit: testCommit,
		FailedFindings: []preflight.Finding{{
			Check:    "settings.profiles",
			Code:     "settings.profiles.missing",
			Severity: preflight.SeverityBlock,
			Message:  "missing dev",
			Data:     map[string]any{"profile": "dev", "project": "p1"},
		}},
	})
	var checkpoints []map[string]any
	ex.SetCheckpoint(func(res map[string]any) error {
		checkpoints = append(checkpoints, res)
		return nil
	})

	var log bytes.Buffer
	if err := ex.Run(context.Background(), &log, nil); err != nil {
		t.Fatalf("forced Run failed: %v\n%s", err, log.String())
	}
	if *signals != 1 {
		t.Errorf("signals = %d, want 1", *signals)
	}
	// S77: stderr DSN must be redacted in run log.
	if strings.Contains(log.String(), "s3cr3t-exec-pw") {
		t.Errorf("S77: raw DSN secret leaked into run log:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "PREFLIGHT OVERRIDDEN by force by admin@example.com (user-42)") {
		t.Errorf("missing PREFLIGHT OVERRIDDEN log line:\n%s", log.String())
	}
	// Checkpoints: Write 1 (before install, overrideApplied: false) and Write 3 (after install, installed: true).
	if len(checkpoints) < 2 {
		t.Fatalf("checkpoints = %d, want >= 2", len(checkpoints))
	}
	if checkpoints[0]["forced"] != true || checkpoints[0]["overrideApplied"] != false {
		t.Errorf("checkpoint[0] = %+v, want forced=true overrideApplied=false", checkpoints[0])
	}
	if checkpoints[1]["installed"] != true || checkpoints[1]["overrideApplied"] != true {
		t.Errorf("checkpoint[1] (write 3) = %+v, want installed=true overrideApplied=true", checkpoints[1])
	}
	// S42: Audit event emitted.
	if len(sink.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(sink.events))
	}
	ev := sink.events[0]
	if ev.RunID != "run-forced-2" || ev.RerunOf != "run-failed-1" || ev.ActorID != "user-42" || ev.Blocking != 1 {
		t.Errorf("audit event = %+v", ev)
	}
}

func TestRebuildServer_ForcedRunQD_ForceNotNeeded(t *testing.T) {
	f := newPreflightFixture(t, false, `
echo '{"version":1,"checks":["settings.profiles"],"findings":[],"blocking":0,"warnings":0}'
exit 0
`)
	inst, _, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	sink := &recordingOverrideSink{}
	ex := f.executor(inst)
	ex.audit = sink
	ex.SetForceGrant(&rebuildpreflight.ForceGrant{
		RerunOf:      "run-failed-1",
		ActorID:      "user-42",
		ActorEmail:   "admin@example.com",
		PinnedBranch: "main",
		PinnedCommit: testCommit,
	})
	var log bytes.Buffer
	if err := ex.Run(context.Background(), &log, nil); err != nil {
		t.Fatalf("Q-D Run failed: %v", err)
	}
	if *signals != 1 {
		t.Errorf("signals = %d, want 1", *signals)
	}
	if len(sink.events) != 0 {
		t.Errorf("Q-D must not emit override audit event, got %d", len(sink.events))
	}
	res := ex.RunResult()
	if res["forced"] != false || res["forceNotNeeded"] != true || res["rerunOf"] != "run-failed-1" {
		t.Errorf("Q-D result = %+v", res)
	}
}
