/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

func withRunDropNetAdmin(t *testing.T, f func() error) {
	t.Helper()
	orig := runDropNetAdmin
	runDropNetAdmin = f
	t.Cleanup(func() { runDropNetAdmin = orig })
}

func withRunInitFirewall(t *testing.T, f func() error) {
	t.Helper()
	orig := runInitFirewall
	runInitFirewall = f
	t.Cleanup(func() { runInitFirewall = orig })
}

// withRunMetadataServerStop records each stop and still runs the real Stop,
// so a double stop exercises the real method twice.
func withRunMetadataServerStop(t *testing.T, f func(s *metadata.Server)) {
	t.Helper()
	orig := runMetadataServerStop
	runMetadataServerStop = f
	t.Cleanup(func() { runMetadataServerStop = orig })
}

// withChildStartupWindow sets RunInit's immediate-exit window and records
// which path each RunInit call took (true = immediate exit).
func withChildStartupWindow(t *testing.T, window time.Duration) *[]bool {
	t.Helper()
	origWindow, origObserve := childStartupCheckWindow, observeChildStartupPath
	var paths []bool
	childStartupCheckWindow = window
	observeChildStartupPath = func(immediate bool) { paths = append(paths, immediate) }
	t.Cleanup(func() { childStartupCheckWindow, observeChildStartupPath = origWindow, origObserve })
	return &paths
}

// errDropDetail stands in for the step/errno detail netcap returns; it must
// reach the log only, never the reported failure.
var errDropDetail = errors.New("drop CAP_NET_ADMIN from bounding set: detail-that-must-not-be-reported")

func TestDropNetAdmin(t *testing.T) {
	tests := []struct {
		name     string
		required bool
		dropErr  error
		wantErr  error
	}{
		{"required, dropped", true, nil, nil},
		{"required, drop failed: fatal", true, errDropDetail, errNetAdminDropFailed},
		{"not required, dropped anyway", false, nil, nil},
		{"not required, drop failed: warn only", false, errDropDetail, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called bool
			withRunDropNetAdmin(t, func() error { called = true; return tt.dropErr })
			err := dropNetAdmin(tt.required)
			if !called {
				t.Fatal("drop not attempted")
			}
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("dropNetAdmin(%v) = %v, want %v", tt.required, err, tt.wantErr)
			}
		})
	}
}

func TestSetupInitFirewall(t *testing.T) {
	tests := []struct {
		value         string
		runErr        error
		wantCalled    bool
		wantInstalled bool
		wantErr       bool
	}{
		{value: "", wantCalled: false},
		{value: "0", wantCalled: false},
		{value: "false", wantCalled: false},
		{value: "1", wantCalled: true, wantInstalled: true},
		{value: "true", wantCalled: true, wantInstalled: true},
		{value: "1", runErr: errors.New("exit status 1"), wantCalled: true, wantErr: true},
		{value: "yes please", wantCalled: false, wantErr: true}, // typo must not silently disable
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(initFirewallEnv, tt.value)
			var called bool
			withRunInitFirewall(t, func() error { called = true; return tt.runErr })
			installed, err := setupInitFirewall()
			if called != tt.wantCalled {
				t.Errorf("firewall run = %v, want %v", called, tt.wantCalled)
			}
			if installed != tt.wantInstalled {
				t.Errorf("installed = %v, want %v", installed, tt.wantInstalled)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("setupInitFirewall() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, errInitFirewallFailed) {
				t.Errorf("error = %v, want errInitFirewallFailed", err)
			}
		})
	}
}

// netAdminFixture drives the real RunInit in assign mode with every
// network step recorded in order. A real pre-start hook script records
// whether the drop had already happened when it ran: the drop stub writes
// droppedFile, and the hook copies that observation to hookObservedFile.
// That is the unit-level stand-in for "a pre-start hook process sees no
// CAP_NET_ADMIN"; the container test checks the real bounding set.
type netAdminFixture struct {
	agentHome        string
	order            []string
	stops            int // metadata server stops, counted separately from order
	droppedFile      string
	hookRanFile      string
	hookObservedFile string
	harnessFile      string
}

func newNetAdminFixture(t *testing.T, mode string, installed bool, dropErr error) *netAdminFixture {
	t.Helper()
	f := &netAdminFixture{agentHome: t.TempDir()}
	setupRunInitAsRootlessScion(t, f.agentHome)
	writeServicesYAML(t, f.agentHome)
	t.Setenv("SCION_METADATA_MODE", mode)
	// Start from no firewall and no host networking, whatever the runner's
	// environment holds; tests that need either set it after the fixture.
	t.Setenv(initFirewallEnv, "")
	t.Setenv("SCION_NETWORK_MODE", "")

	scratch := t.TempDir()
	f.droppedFile = filepath.Join(scratch, "dropped")
	f.hookRanFile = filepath.Join(scratch, "hook-ran")
	f.hookObservedFile = filepath.Join(scratch, "hook-saw-drop")
	f.harnessFile = filepath.Join(scratch, "harness-started")

	hooksDir := t.TempDir()
	preStartDir := filepath.Join(hooksDir, "pre-start.d")
	if err := os.MkdirAll(preStartDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch '" + f.hookRanFile + "'\n[ -f '" + f.droppedFile + "' ] && touch '" + f.hookObservedFile + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(preStartDir, "10-observe-drop"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCION_HOOKS_DIR", hooksDir)

	withRunInitFirewall(t, func() error { f.order = append(f.order, "firewall"); return nil })
	withRunVerifyIPv6Blocked(t, func(context.Context) error { return nil })
	withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error {
		f.order = append(f.order, "metadata")
		return nil
	})
	withRunMetadataServerStop(t, func(s *metadata.Server) {
		f.stops++
		s.Stop()
	})
	withRunDropNetAdmin(t, func() error {
		f.order = append(f.order, "drop")
		if dropErr != nil {
			return dropErr
		}
		return os.WriteFile(f.droppedFile, nil, 0o600)
	})
	withRunGitCloneWorkspace(t, func(int, int, string, bool) error {
		f.order = append(f.order, "clone")
		return nil
	})
	withRunServicesStart(t, func(context.Context, *services.Manager, []api.ServiceSpec, int, int, string, bool) error {
		f.order = append(f.order, "sidecars")
		return nil
	})
	withRunFetchSecretOverrides(t, func(*hub.Client, []string) map[string]string { return nil })
	return f
}

func (f *netAdminFixture) run() int {
	return f.runHarness("touch '" + f.harnessFile + "'")
}

func (f *netAdminFixture) runHarness(script string) int {
	return RunInit([]string{"sh", "-c", script}, InitRunOptions{DisableTermSignalForwarding: true})
}

func netAdminPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestRunInit_NetAdminDrop_PrecedesWorkload pins the agreed order: the
// firewall, then the metadata start, then the drop, and only then the
// clone, pre-start hooks, sidecar services and harness. A mutant that
// removes the drop call from RunInit fails here.
// It also covers assign mode with the firewall installed and a successful
// drop: the full order plus the hook-observed-drop check.
func TestRunInit_NetAdminDrop_PrecedesWorkload(t *testing.T) {
	f := newNetAdminFixture(t, "assign", true, nil)
	t.Setenv(initFirewallEnv, "1")

	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	want := []string{"firewall", "metadata", "drop", "clone", "sidecars"}
	if !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
	if !netAdminPathExists(f.hookRanFile) {
		t.Fatal("pre-start hook did not run")
	}
	if !netAdminPathExists(f.hookObservedFile) {
		t.Fatal("pre-start hook ran before the CAP_NET_ADMIN drop")
	}
	if !netAdminPathExists(f.harnessFile) {
		t.Fatal("harness did not run")
	}
}

// TestRunInit_NetAdminDrop_BlockModeAlsoDrops covers the other
// interception mode.
func TestRunInit_NetAdminDrop_BlockModeAlsoDrops(t *testing.T) {
	f := newNetAdminFixture(t, "block", true, nil)
	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	want := []string{"metadata", "drop", "clone", "sidecars"}
	if !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
}

// TestRunInit_NetAdminDropFailure_FailsClosed: when init installed the
// opt-in firewall and the drop fails, nothing workload-controlled may start,
// and the reported failure names only the capability.
func TestRunInit_NetAdminDropFailure_FailsClosed(t *testing.T) {
	f := newNetAdminFixture(t, "assign", true, errDropDetail)
	t.Setenv(initFirewallEnv, "1")

	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"firewall", "metadata", "drop"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v (clone and sidecars must not run)", f.order, want)
	}
	if netAdminPathExists(f.hookRanFile) {
		t.Error("pre-start hook ran after a failed drop")
	}
	if netAdminPathExists(f.harnessFile) {
		t.Error("harness ran after a failed drop")
	}
	if f.stops != 1 {
		t.Errorf("metadata server stops = %d, want 1 (stopped on the fail-closed exit)", f.stops)
	}

	raw, err := os.ReadFile(filepath.Join(f.agentHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("agent-info.json not written: %v", err)
	}
	var info struct {
		Phase  string `json:"phase"`
		Detail struct {
			Message string `json:"message"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal agent-info.json: %v", err)
	}
	if info.Phase != string(state.PhaseError) {
		t.Errorf("phase = %q, want %q", info.Phase, state.PhaseError)
	}
	if info.Detail.Message != errNetAdminDropFailed.Error() {
		t.Errorf("reported message = %q, want exactly %q", info.Detail.Message, errNetAdminDropFailed.Error())
	}
	if strings.Contains(info.Detail.Message, "detail-that-must-not-be-reported") {
		t.Error("reported message carries the drop error detail")
	}
}

// TestRunInit_NetAdminDropFailure_SkippedInterceptionWarnsOnly: the drop
// is still attempted when interception was legitimately skipped, but its
// failure keeps today's start behaviour.
func TestRunInit_NetAdminDropFailure_SkippedInterceptionWarnsOnly(t *testing.T) {
	f := newNetAdminFixture(t, "block", false, errDropDetail)
	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if want := []string{"metadata", "drop", "clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
	if !netAdminPathExists(f.harnessFile) {
		t.Fatal("harness did not run")
	}
}

// TestRunInit_NetAdminDrop_SkippedInterceptionDropSucceeds: interception
// was legitimately skipped and the drop succeeds, so the agent starts
// normally. (This is what a runtime that never granted the capability
// looks like at init level; the no-op itself is netcap's
// TestDropNetAdmin_NoopWhenAbsent.)
func TestRunInit_NetAdminDrop_SkippedInterceptionDropSucceeds(t *testing.T) {
	f := newNetAdminFixture(t, "assign", false, nil)
	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if !netAdminPathExists(f.harnessFile) {
		t.Fatal("harness did not run")
	}
}

// TestRunInit_Passthrough_NoDrop: passthrough runs no metadata server and
// must not touch capabilities.
func TestRunInit_Passthrough_NoDrop(t *testing.T) {
	f := newNetAdminFixture(t, "passthrough", false, nil)
	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if want := []string{"clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v (no metadata start, no drop)", f.order, want)
	}
	if !netAdminPathExists(f.harnessFile) {
		t.Fatal("harness did not run")
	}
}

// TestRunInit_Passthrough_FirewallDrops: passthrough runs no metadata
// server, but an installed firewall is a network control too, so the drop
// still runs before anything workload-controlled. A mutant that gates the
// drop on the metadata server alone fails here.
func TestRunInit_Passthrough_FirewallDrops(t *testing.T) {
	f := newNetAdminFixture(t, "passthrough", false, nil)
	t.Setenv(initFirewallEnv, "1")

	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if want := []string{"firewall", "drop", "clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
	if !netAdminPathExists(f.hookObservedFile) {
		t.Fatal("pre-start hook ran before the CAP_NET_ADMIN drop")
	}
	if !netAdminPathExists(f.harnessFile) {
		t.Fatal("harness did not run")
	}
}

// TestRunInit_Passthrough_FirewallRequiredDropNilResult_Proceeds: RunInit
// treats a nil drop result as success, so a required drop that reports
// nothing to drop must not refuse the start. The real not-held no-op (Held
// is false, so no prctl or capset runs) is pinned by netcap's
// TestDropNetAdmin_NoopWhenAbsent; this test stubs only its result.
func TestRunInit_Passthrough_FirewallRequiredDropNilResult_Proceeds(t *testing.T) {
	f := newNetAdminFixture(t, "passthrough", false, nil)
	t.Setenv(initFirewallEnv, "1")
	withRunDropNetAdmin(t, func() error {
		f.order = append(f.order, "drop")
		return nil // not held: a successful no-op
	})

	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if want := []string{"firewall", "drop", "clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
	if !netAdminPathExists(f.hookRanFile) {
		t.Fatal("pre-start hook did not run after the drop")
	}
	if !netAdminPathExists(f.harnessFile) {
		t.Fatal("harness did not run")
	}
}

// TestRunInit_Passthrough_FirewallDropFailure_FailsClosed: with the
// firewall installed, a failed drop refuses the start before anything
// workload-controlled runs.
func TestRunInit_Passthrough_FirewallDropFailure_FailsClosed(t *testing.T) {
	f := newNetAdminFixture(t, "passthrough", false, errDropDetail)
	t.Setenv(initFirewallEnv, "1")

	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"firewall", "drop"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v (clone and sidecars must not run)", f.order, want)
	}
	if netAdminPathExists(f.hookRanFile) || netAdminPathExists(f.harnessFile) {
		t.Error("workload ran after a failed drop")
	}
	if _, msg := readAgentInfoMessage(t, f.agentHome); msg != errNetAdminDropFailed.Error() {
		t.Errorf("reported message = %q, want exactly %q", msg, errNetAdminDropFailed.Error())
	}
}

// TestRunInit_FirewallSkippedInterception_DropFailure_FailsClosed: in
// assign mode with interception legitimately skipped, a failed drop only
// warns, unless the firewall was installed: then it is fatal.
func TestRunInit_FirewallSkippedInterception_DropFailure_FailsClosed(t *testing.T) {
	f := newNetAdminFixture(t, "assign", false, errDropDetail)
	t.Setenv(initFirewallEnv, "1")

	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"firewall", "metadata", "drop"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v (clone and sidecars must not run)", f.order, want)
	}
	if netAdminPathExists(f.hookRanFile) || netAdminPathExists(f.harnessFile) {
		t.Error("workload ran after a failed drop")
	}
	if _, msg := readAgentInfoMessage(t, f.agentHome); msg != errNetAdminDropFailed.Error() {
		t.Errorf("reported message = %q, want exactly %q", msg, errNetAdminDropFailed.Error())
	}
}

// TestRunInit_FirewallSkippedInterception_BlockMode_DropFailure_FailsClosed:
// the block-mode twin of the assign test above.
func TestRunInit_FirewallSkippedInterception_BlockMode_DropFailure_FailsClosed(t *testing.T) {
	f := newNetAdminFixture(t, "block", false, errDropDetail)
	t.Setenv(initFirewallEnv, "1")

	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"firewall", "metadata", "drop"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v (clone and sidecars must not run)", f.order, want)
	}
	if netAdminPathExists(f.hookRanFile) || netAdminPathExists(f.harnessFile) {
		t.Error("workload ran after a failed drop")
	}
	if _, msg := readAgentInfoMessage(t, f.agentHome); msg != errNetAdminDropFailed.Error() {
		t.Errorf("reported message = %q, want exactly %q", msg, errNetAdminDropFailed.Error())
	}
}

// TestRunInit_FirewallNonFatalMetadataStart_DropFailure_FailsClosed: block
// mode under host networking is the one case where a metadata start failure
// does not stop init (metadata.Config.StartFailureIsFatal). The server is
// still non-nil, so the drop runs, and with the firewall installed it is
// required: a failed drop stops the start.
func TestRunInit_FirewallNonFatalMetadataStart_DropFailure_FailsClosed(t *testing.T) {
	f := newNetAdminFixture(t, "block", false, errDropDetail)
	t.Setenv("SCION_NETWORK_MODE", "host")
	t.Setenv(initFirewallEnv, "1")
	withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error {
		f.order = append(f.order, "metadata")
		return errors.New("listen tcp 127.0.0.1:18380: bind: address already in use")
	})

	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"firewall", "metadata", "drop"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v (clone and sidecars must not run)", f.order, want)
	}
	if netAdminPathExists(f.hookRanFile) || netAdminPathExists(f.harnessFile) {
		t.Error("workload ran after a failed drop")
	}
	if _, msg := readAgentInfoMessage(t, f.agentHome); msg != errNetAdminDropFailed.Error() {
		t.Errorf("reported message = %q, want exactly %q", msg, errNetAdminDropFailed.Error())
	}
}

// TestRunInit_InitFirewallFailure_FailsClosed: a requested firewall that
// fails stops init before the metadata start and before anything
// workload-controlled.
func TestRunInit_InitFirewallFailure_FailsClosed(t *testing.T) {
	f := newNetAdminFixture(t, "assign", true, nil)
	t.Setenv(initFirewallEnv, "1")
	withRunInitFirewall(t, func() error {
		f.order = append(f.order, "firewall")
		return errors.New("exit status 1")
	})
	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"firewall"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
	if netAdminPathExists(f.hookRanFile) || netAdminPathExists(f.harnessFile) {
		t.Error("workload ran after a failed firewall")
	}
}

// The early exits below all happen after the metadata server started (it
// starts before the clone, so the capability drop precedes every
// workload-controlled step). Each must leave nothing running: the deferred
// stop must have run by the time RunInit returns.

func TestRunInit_EarlyExit_CloneFailure_StopsMetadata(t *testing.T) {
	f := newNetAdminFixture(t, "assign", true, nil)
	withRunGitCloneWorkspace(t, func(int, int, string, bool) error {
		f.order = append(f.order, "clone")
		return errors.New("clone failed")
	})
	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"metadata", "drop", "clone"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
	if f.stops == 0 {
		t.Fatal("metadata server not stopped before RunInit returned")
	}
}

func TestRunInit_EarlyExit_RequiredPreStartFailure_StopsMetadata(t *testing.T) {
	f := newNetAdminFixture(t, "assign", true, nil)
	// A staged project pre-start hook makes pre-start failure fatal.
	hook := resolveProjectHookPath(f.agentHome, false)
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"metadata", "drop", "clone"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v (sidecars must not start)", f.order, want)
	}
	if netAdminPathExists(f.harnessFile) {
		t.Error("harness ran after a required pre-start failure")
	}
	if f.stops == 0 {
		t.Fatal("metadata server not stopped before RunInit returned")
	}
}

func TestRunInit_EarlyExit_OverlayError_StopsMetadata(t *testing.T) {
	unsetEnvForTest(t, hooks.HarnessOutputsDirEnv)
	unsetEnvForTest(t, hooks.HarnessSecretsDirEnv)
	f := newNetAdminFixture(t, "assign", true, nil)
	// A required container-script harness whose env overlay is malformed.
	bundleDir := filepath.Join(f.agentHome, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundleDir, "outputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"harness_config":{"provisioner":{"type":"container-script"}},"outputs":{"env":"$HOME/.scion/harness/outputs/env.json"}}`
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "outputs", "env.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"metadata", "drop", "clone"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v (sidecars must not start)", f.order, want)
	}
	if !netAdminPathExists(f.hookRanFile) {
		t.Error("pre-start hook did not run; the overlay error was not the exit reached")
	}
	if f.stops == 0 {
		t.Fatal("metadata server not stopped before RunInit returned")
	}
}

// TestRunInit_MetadataStop_NormalPathStopsTwiceSafely: on a normal run the
// shutdown sequence stops the server and the deferred stop runs again at
// return. Both go through the real Stop.
func TestRunInit_MetadataStop_NormalPathStopsTwiceSafely(t *testing.T) {
	f := newNetAdminFixture(t, "block", true, nil)
	// A 1 ms immediate-exit window and a harness that stays alive for
	// 500 ms make RunInit treat the child as running and take the normal
	// shutdown path; the assertion on the observed path below catches any
	// run that did not.
	paths := withChildStartupWindow(t, time.Millisecond)
	if got := f.runHarness("sleep 0.5; touch '" + f.harnessFile + "'"); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if len(*paths) != 1 || (*paths)[0] {
		t.Fatalf("startup paths = %v, want [false] (normal path)", *paths)
	}
	if f.stops != 2 {
		t.Fatalf("metadata server stops = %d, want 2 (normal shutdown + deferred)", f.stops)
	}
}

// TestRunInit_MetadataStop_ImmediateChildExitStopsViaDefer: a harness that
// exits inside RunInit's immediate-exit window makes RunInit return before
// the normal shutdown sequence. The server must still be stopped,
// exactly once, by the deferred stop.
func TestRunInit_MetadataStop_ImmediateChildExitStopsViaDefer(t *testing.T) {
	f := newNetAdminFixture(t, "block", true, nil)
	// A 10 s window: the touch harness exits well inside it, so RunInit
	// takes the immediate-exit path.
	paths := withChildStartupWindow(t, 10*time.Second)
	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if len(*paths) != 1 || !(*paths)[0] {
		t.Fatalf("startup paths = %v, want [true] (immediate-exit path)", *paths)
	}
	if !netAdminPathExists(f.harnessFile) {
		t.Fatal("harness did not run")
	}
	if f.stops != 1 {
		t.Fatalf("metadata server stops = %d, want 1 (deferred stop only on the immediate-exit path)", f.stops)
	}
}

// The refusal texts the Hub shows when the required metadata protection
// cannot be set up. Pinned verbatim: changing them is a reviewed decision.
const (
	goldenMetadataProtectionRefusal         = "agent start refused: the configured metadata protection could not be set up in this container (metadata_protection_unavailable). Verify the broker and agent image versions match (use a broker build that supports runtime-managed interception) and host capabilities are available, or set the metadata mode to passthrough explicitly to opt out."
	goldenMetadataInterceptionMarkerMissing = "agent start refused: metadata interception version skew (metadata_interception_marker_missing): the broker and agent image disagree on who installs metadata interception. Upgrade the broker to a build that supports runtime-managed interception or use a matching agent image, or set the metadata mode to passthrough explicitly to opt out."
)

func TestMetadataProtectionRefusalText_Golden(t *testing.T) {
	if got := errMetadataProtectionUnavailable.Error(); got != goldenMetadataProtectionRefusal {
		t.Fatalf("refusal text = %q, want exactly %q", got, goldenMetadataProtectionRefusal)
	}
	if got := errMetadataInterceptionMarkerMissing.Error(); got != goldenMetadataInterceptionMarkerMissing {
		t.Fatalf("skew refusal text = %q, want exactly %q", got, goldenMetadataInterceptionMarkerMissing)
	}
}

func withRunWaitForRedirect(t *testing.T, f func(context.Context, string, string) error) {
	t.Helper()
	orig := runWaitForRedirect
	runWaitForRedirect = f
	t.Cleanup(func() { runWaitForRedirect = orig })
}

func withRunVerifyIPv6Blocked(t *testing.T, f func(context.Context) error) {
	t.Helper()
	orig := runVerifyIPv6Blocked
	runVerifyIPv6Blocked = f
	t.Cleanup(func() { runVerifyIPv6Blocked = orig })
}

func withPreflightEUID(t *testing.T, euid int) {
	t.Helper()
	orig := preflightEUID
	preflightEUID = func() int { return euid }
	t.Cleanup(func() { preflightEUID = orig })
}

func withPreflightNetAdminUsable(t *testing.T, usable bool, err error) {
	t.Helper()
	orig := preflightNetAdminUsable
	preflightNetAdminUsable = func() (bool, error) { return usable, err }
	t.Cleanup(func() { preflightNetAdminUsable = orig })
}

func withRunInstallInterceptionRules(t *testing.T, f func(string, int) error) {
	t.Helper()
	orig := runInstallInterceptionRules
	runInstallInterceptionRules = f
	t.Cleanup(func() { runInstallInterceptionRules = orig })
}

// preflightFailures is one row per condition the preflight refuses.
var preflightFailures = []struct {
	name        string
	source      string
	networkMode string
	euid        int
	usable      bool
	usableErr   error
	wantErr     error
}{
	{name: "missing marker non-root", source: "", euid: 1000, usable: true, wantErr: errMetadataInterceptionMarkerMissing},
	{name: "missing marker without CAP_NET_ADMIN", source: "", euid: 0, usable: false, wantErr: errMetadataInterceptionMarkerMissing},
	{name: "missing marker capability check error", source: "", euid: 0, usableErr: errors.New("proc unreadable"), wantErr: errMetadataInterceptionMarkerMissing},
	{name: "unknown source marker", source: "legacy", euid: 0, usable: true, wantErr: errMetadataInterceptionMarkerMissing},
	{name: "host network", source: metadata.InterceptionSourceRuntime, networkMode: "host", euid: 0, usable: true, wantErr: errMetadataProtectionUnavailable},
	{name: "host network legacy", source: "", networkMode: "host", euid: 0, usable: true, wantErr: errMetadataProtectionUnavailable},
}

func TestMetadataProtectionPreflight(t *testing.T) {
	required := func(mode, source, networkMode string) *metadata.Config {
		return &metadata.Config{
			Mode:                 mode,
			NetworkMode:          networkMode,
			InterceptionRequired: true,
			InterceptionSource:   source,
		}
	}
	for _, mode := range []string{"block", "assign"} {
		for _, tt := range preflightFailures {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				withPreflightEUID(t, tt.euid)
				withPreflightNetAdminUsable(t, tt.usable, tt.usableErr)
				if err := metadataProtectionPreflight(required(mode, tt.source, tt.networkMode)); !errors.Is(err, tt.wantErr) {
					t.Fatalf("metadataProtectionPreflight() = %v, want %v", err, tt.wantErr)
				}
			})
		}
		t.Run(mode+"/runtime source non-host passes", func(t *testing.T) {
			withPreflightEUID(t, 1000)
			withPreflightNetAdminUsable(t, false, nil)
			if err := metadataProtectionPreflight(required(mode, metadata.InterceptionSourceRuntime, "")); err != nil {
				t.Fatalf("metadataProtectionPreflight() = %v, want nil", err)
			}
		})
		t.Run(mode+"/legacy fallback passes when root with usable NET_ADMIN", func(t *testing.T) {
			withPreflightEUID(t, 0)
			withPreflightNetAdminUsable(t, true, nil)
			if err := metadataProtectionPreflight(required(mode, "", "")); err != nil {
				t.Fatalf("metadataProtectionPreflight() = %v, want nil", err)
			}
		})
		// Without the runtime's required marker the preflight changes nothing.
		t.Run(mode+"/marker off", func(t *testing.T) {
			if err := metadataProtectionPreflight(&metadata.Config{Mode: mode, NetworkMode: "host"}); err != nil {
				t.Fatalf("metadataProtectionPreflight() = %v, want nil", err)
			}
		})
	}
	t.Run("no metadata config (passthrough)", func(t *testing.T) {
		if err := metadataProtectionPreflight(nil); err != nil {
			t.Fatalf("metadataProtectionPreflight(nil) = %v, want nil", err)
		}
	})
}

// stagedPayload is a SCION_STAGED_SECRETS value with one file secret at
// target, one harness secret named harnessSecretName and a hub credential.
func stagedPayload(t *testing.T, target, hubToken string) string {
	t.Helper()
	data, err := json.Marshal(stagedsecrets.Staged{
		FileSecrets: []stagedsecrets.FileSecret{{
			Name:   "example",
			Target: target,
			Value:  base64.StdEncoding.EncodeToString([]byte("example-file-content")),
		}},
		HubToken: hubToken,
		HarnessSecrets: []stagedsecrets.HarnessSecret{{
			Name:  harnessSecretName,
			Value: base64.StdEncoding.EncodeToString([]byte("example-harness-secret")),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(data)
}

const harnessSecretName = "netadmin-example-harness-secret"

// credentialFixture puts a transport credential and a staged payload (a
// file secret, a harness secret and a hub credential) in the environment,
// with the token home and the in-memory harness root in temp dirs, and
// returns the paths staging would write.
type credentialFixture struct {
	transportPath, hubTokenPath, fileSecretPath, harnessSecretPath string
}

func newCredentialFixture(t *testing.T, agentHome string) credentialFixture {
	t.Helper()
	_, transportPath := setTransportTokenEnv(t)
	t.Setenv(envHubTokenStaged, "")
	_ = os.Unsetenv(envHubTokenStaged)
	memRoot := t.TempDir()
	origRoot := harnessSecretsRoot
	harnessSecretsRoot = memRoot
	t.Cleanup(func() { harnessSecretsRoot = origRoot })
	harnessSecretsDir := filepath.Join(memRoot, "harness-secrets")
	t.Setenv(hooks.HarnessSecretsDirEnv, harnessSecretsDir)
	c := credentialFixture{
		transportPath:     transportPath,
		hubTokenPath:      filepath.Join(filepath.Dir(transportPath), hub.TokenFile),
		fileSecretPath:    filepath.Join(agentHome, ".config", "netadmin-example", "secret.txt"),
		harnessSecretPath: filepath.Join(harnessSecretsDir, harnessSecretName),
	}
	t.Setenv(stagedsecrets.EnvVar, stagedPayload(t, c.fileSecretPath, makeDoctorTestJWT(time.Now().Add(time.Hour))))
	return c
}

func readAgentInfoMessage(t *testing.T, agentHome string) (phase, message string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("agent-info.json not written: %v", err)
	}
	var info struct {
		Phase  string `json:"phase"`
		Detail struct {
			Message string `json:"message"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal agent-info.json: %v", err)
	}
	return info.Phase, info.Detail.Message
}

// TestRunInit_MetadataProtectionPreflight_RefusesBeforeCredentials: in
// block and assign mode with the runtime's required marker, each condition
// the preflight checks refuses the start with the fixed text, before the
// transport credential, the staged file and harness secrets or the staged
// hub credential are written, and before the firewall, the metadata start, the drop or
// anything workload-controlled runs. A mutant that removes the preflight
// call, or makes it warn only, fails here.
func TestRunInit_MetadataProtectionPreflight_RefusesBeforeCredentials(t *testing.T) {
	for _, mode := range []string{"block", "assign"} {
		for _, tt := range preflightFailures {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				f := newNetAdminFixture(t, mode, true, nil)
				reExecs := observeReExec(t)
				withPreflightEUID(t, tt.euid)
				withPreflightNetAdminUsable(t, tt.usable, tt.usableErr)
				t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
				t.Setenv(metadata.InterceptionSourceEnvVar, tt.source)
				t.Setenv(initFirewallEnv, "1")
				if tt.networkMode != "" {
					t.Setenv("SCION_NETWORK_MODE", tt.networkMode)
				}
				creds := newCredentialFixture(t, f.agentHome)

				if got := f.run(); got != 1 {
					t.Fatalf("RunInit() = %d, want 1", got)
				}
				if len(f.order) != 0 {
					t.Fatalf("order = %v, want nothing (no firewall, metadata start, drop, clone or sidecars)", f.order)
				}
				if n := reExecs.Load(); n != 0 {
					t.Errorf("re-exec ran %d time(s); staging was reached", n)
				}
				for what, path := range map[string]string{
					"transport token file":   creds.transportPath,
					"staged hub credential":  creds.hubTokenPath,
					"staged file secret":     creds.fileSecretPath,
					"staged harness secret":  creds.harnessSecretPath,
					"pre-start hook marker":  f.hookRanFile,
					"harness started marker": f.harnessFile,
				} {
					if netAdminPathExists(path) {
						t.Errorf("%s exists after the refusal", what)
					}
				}
				phase, msg := readAgentInfoMessage(t, f.agentHome)
				if phase != string(state.PhaseError) {
					t.Errorf("phase = %q, want %q", phase, state.PhaseError)
				}
				if msg != tt.wantErr.Error() {
					t.Errorf("reported message = %q, want exactly %q", msg, tt.wantErr.Error())
				}
			})
		}
	}
}

// TestRunInit_MetadataProtectionPreflight_PassesWhenUsable: with the
// required and runtime-source markers set, init skips the in-container
// firewall, awaits the redirect probe after starting the metadata server,
// and only then runs clone/hooks/sidecars/harness.
func TestRunInit_MetadataProtectionPreflight_PassesWhenUsable(t *testing.T) {
	for _, mode := range []string{"block", "assign"} {
		t.Run(mode, func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, nil)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			t.Setenv(metadata.InterceptionSourceEnvVar, metadata.InterceptionSourceRuntime)
			t.Setenv(initFirewallEnv, "1") // handled by sidecar, must not run in init
			withRunWaitForRedirect(t, func(_ context.Context, probePath, probeToken string) error {
				if probePath == "" {
					t.Error("waitForRedirect called with empty probePath")
				}
				if probeToken == "" {
					t.Error("waitForRedirect called with empty probeToken")
				}
				f.order = append(f.order, "probe")
				return nil
			})
			withRunVerifyIPv6Blocked(t, func(context.Context) error {
				f.order = append(f.order, "ipv6-probe")
				return nil
			})
			if got := f.run(); got != 0 {
				t.Fatalf("RunInit() = %d, want 0", got)
			}
			if want := []string{"metadata", "probe", "ipv6-probe", "drop", "clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
				t.Fatalf("order = %v, want %v", f.order, want)
			}
			if !netAdminPathExists(f.harnessFile) {
				t.Fatal("harness did not run")
			}
		})
	}
}

// TestRunInit_MetadataProtectionPreflight_MarkerOffUnchanged: without the
// marker (Kubernetes, rootless Podman, the Apple runtime, host networking)
// the start is unchanged.
func TestRunInit_MetadataProtectionPreflight_MarkerOffUnchanged(t *testing.T) {
	for _, mode := range []string{"block", "assign"} {
		t.Run(mode, func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, nil)
			if got := f.run(); got != 0 {
				t.Fatalf("RunInit() = %d, want 0", got)
			}
			if want := []string{"metadata", "drop", "clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
				t.Fatalf("order = %v, want %v", f.order, want)
			}
			if !netAdminPathExists(f.harnessFile) {
				t.Fatal("harness did not run")
			}
		})
	}
}

// TestRunInit_MetadataProtectionPreflight_PassthroughUnchanged:
// passthrough runs no metadata server, so the marker changes nothing.
func TestRunInit_MetadataProtectionPreflight_PassthroughUnchanged(t *testing.T) {
	f := newNetAdminFixture(t, "passthrough", false, nil)
	t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if want := []string{"clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
}

// TestRunInit_MetadataStartRefusal_AfterStaging_CleansUp: the preflight
// passes but the redirect probe times out/fails. That late refusal comes
// after staging, so it removes the transport token file and its pointer
// and, through the deferred finishStagedHubToken, the hub credential this
// init staged, stops the metadata server, and never starts the harness.
func TestRunInit_MetadataStartRefusal_AfterStaging_CleansUp(t *testing.T) {
	for _, mode := range []string{"block", "assign"} {
		t.Run(mode, func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, nil)
			observeReExec(t)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			t.Setenv(metadata.InterceptionSourceEnvVar, metadata.InterceptionSourceRuntime)
			creds := newCredentialFixture(t, f.agentHome)
			var transportAtStart, hubTokenAtStart, harnessSecretAtStart bool
			withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error {
				f.order = append(f.order, "metadata")
				transportAtStart = netAdminPathExists(creds.transportPath)
				hubTokenAtStart = netAdminPathExists(creds.hubTokenPath)
				harnessSecretAtStart = netAdminPathExists(creds.harnessSecretPath)
				return nil
			})
			withRunWaitForRedirect(t, func(context.Context, string, string) error {
				f.order = append(f.order, "probe")
				return errMetadataProtectionUnavailable
			})

			if got := f.run(); got != 1 {
				t.Fatalf("RunInit() = %d, want 1", got)
			}
			if want := []string{"metadata", "probe"}; !reflect.DeepEqual(f.order, want) {
				t.Fatalf("order = %v, want %v (no drop, clone or sidecars)", f.order, want)
			}
			// Controls: staging did run before the start.
			if !transportAtStart || !hubTokenAtStart || !harnessSecretAtStart {
				t.Fatalf("staged before the start: transport %v, hub credential %v, harness secret %v; want all",
					transportAtStart, hubTokenAtStart, harnessSecretAtStart)
			}
			if netAdminPathExists(creds.transportPath) {
				t.Error("transport token file still present after the refusal")
			}
			if v := os.Getenv(transportauth.EnvTransportTokenFile); v != "" {
				t.Errorf("%s = %q after the refusal, want it unset", transportauth.EnvTransportTokenFile, v)
			}
			if netAdminPathExists(creds.hubTokenPath) {
				t.Error("staged hub credential still present after the refusal")
			}
			// Documented residue: file-secret targets and the harness
			// secrets are left in place.
			if !netAdminPathExists(creds.fileSecretPath) {
				t.Error("staged file secret is gone; the documented behaviour is that it stays")
			}
			if !netAdminPathExists(creds.harnessSecretPath) {
				t.Error("staged harness secret is gone; the documented behaviour is that it stays")
			}
			if netAdminPathExists(f.harnessFile) {
				t.Error("harness ran after the refusal")
			}
			if f.stops != 1 {
				t.Errorf("metadata server stops = %d, want 1", f.stops)
			}
			phase, msg := readAgentInfoMessage(t, f.agentHome)
			if phase != string(state.PhaseError) {
				t.Errorf("phase = %q, want %q", phase, state.PhaseError)
			}
			if msg != goldenMetadataProtectionRefusal {
				t.Errorf("reported message = %q, want %q", msg, goldenMetadataProtectionRefusal)
			}
		})
	}
}

func TestWaitForRedirect(t *testing.T) {
	origBase, origAttempt, origDeadline, origInterval := redirectProbeBaseURL, redirectProbeAttemptTimeout, redirectProbeDeadline, redirectProbeInterval
	t.Cleanup(func() {
		redirectProbeBaseURL, redirectProbeAttemptTimeout, redirectProbeDeadline, redirectProbeInterval = origBase, origAttempt, origDeadline, origInterval
	})
	redirectProbeAttemptTimeout = time.Second
	redirectProbeInterval = 10 * time.Millisecond

	const wantPath = "/probe-token-path"
	const wantToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("retries until 200 with matching token", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != wantPath {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if attempts.Add(1) < 3 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, wantToken)
		}))
		t.Cleanup(srv.Close)
		redirectProbeBaseURL = srv.URL
		redirectProbeDeadline = 5 * time.Second

		if err := waitForRedirect(context.Background(), wantPath, wantToken); err != nil {
			t.Fatalf("waitForRedirect() = %v, want nil", err)
		}
		if got := attempts.Load(); got != 3 {
			t.Errorf("attempts = %d, want 3", got)
		}
	})

	t.Run("rejects bare 204", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(srv.Close)
		redirectProbeBaseURL = srv.URL
		redirectProbeDeadline = 60 * time.Millisecond

		err := waitForRedirect(context.Background(), wantPath, wantToken)
		if !errors.Is(err, errMetadataProtectionUnavailable) {
			t.Fatalf("waitForRedirect() = %v, want errMetadataProtectionUnavailable", err)
		}
	})

	t.Run("rejects wrong token body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "wrong-token")
		}))
		t.Cleanup(srv.Close)
		redirectProbeBaseURL = srv.URL
		redirectProbeDeadline = 60 * time.Millisecond

		err := waitForRedirect(context.Background(), wantPath, wantToken)
		if !errors.Is(err, errMetadataProtectionUnavailable) {
			t.Fatalf("waitForRedirect() = %v, want errMetadataProtectionUnavailable", err)
		}
	})

	t.Run("rejects oversized body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, wantToken+strings.Repeat("x", maxProbeBodyBytes))
		}))
		t.Cleanup(srv.Close)
		redirectProbeBaseURL = srv.URL
		redirectProbeDeadline = 60 * time.Millisecond

		err := waitForRedirect(context.Background(), wantPath, wantToken)
		if !errors.Is(err, errMetadataProtectionUnavailable) {
			t.Fatalf("waitForRedirect() = %v, want errMetadataProtectionUnavailable", err)
		}
	})

	t.Run("rejects empty path or token immediately", func(t *testing.T) {
		if err := waitForRedirect(context.Background(), "", wantToken); !errors.Is(err, errMetadataProtectionUnavailable) {
			t.Fatalf("empty path: got %v, want errMetadataProtectionUnavailable", err)
		}
		if err := waitForRedirect(context.Background(), wantPath, ""); !errors.Is(err, errMetadataProtectionUnavailable) {
			t.Fatalf("empty token: got %v, want errMetadataProtectionUnavailable", err)
		}
	})

	t.Run("times out when server never returns valid token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		redirectProbeBaseURL = srv.URL
		redirectProbeDeadline = 60 * time.Millisecond

		err := waitForRedirect(context.Background(), wantPath, wantToken)
		if !errors.Is(err, errMetadataProtectionUnavailable) {
			t.Fatalf("waitForRedirect() = %v, want errMetadataProtectionUnavailable", err)
		}
	})
}

func TestRedirectProbeDeadline_ExceedsNetnsSetupTimeout(t *testing.T) {
	// pkg/runtime/common.go defines netnsSetupTimeout = 3 * time.Minute.
	const brokerNetnsSetupTimeout = 3 * time.Minute
	if redirectProbeDeadline <= brokerNetnsSetupTimeout {
		t.Fatalf("redirectProbeDeadline (%s) must exceed broker netnsSetupTimeout (%s)", redirectProbeDeadline, brokerNetnsSetupTimeout)
	}
	if legacyRedirectProbeDeadline <= 0 || legacyRedirectProbeDeadline >= redirectProbeDeadline {
		t.Fatalf("legacyRedirectProbeDeadline (%s) must be positive and shorter than redirectProbeDeadline (%s)", legacyRedirectProbeDeadline, redirectProbeDeadline)
	}
}

func TestVerifyIPv6MetadataBlocked(t *testing.T) {
	origAddrs, origTimeout, origDial := ipv6ProbeAddrs, ipv6ProbeTimeout, ipv6ProbeDial
	t.Cleanup(func() {
		ipv6ProbeAddrs, ipv6ProbeTimeout, ipv6ProbeDial = origAddrs, origTimeout, origDial
	})
	ipv6ProbeTimeout = 2 * time.Second

	t.Run("attempts dials and passes when IPv6 is unavailable (ENETUNREACH/EAFNOSUPPORT)", func(t *testing.T) {
		var mu sync.Mutex
		dialed := make(map[string]bool)
		ipv6ProbeDial = func(_ context.Context, _ string, addr string) (net.Conn, error) {
			mu.Lock()
			dialed[addr] = true
			mu.Unlock()
			if strings.Contains(addr, "fd20:ce::254") {
				return nil, syscall.EAFNOSUPPORT
			}
			return nil, syscall.ENETUNREACH
		}
		if err := verifyIPv6MetadataBlocked(context.Background()); err != nil {
			t.Fatalf("verifyIPv6MetadataBlocked() = %v, want nil", err)
		}
		for _, want := range []string{"[fd20:ce::254]:80", "[fd00:ec2::254]:80"} {
			if !dialed[want] {
				t.Errorf("expected dial attempt to %s even without IPv6, dialed=%v", want, dialed)
			}
		}
	})

	t.Run("passes when endpoints return refused, reset, or timeout", func(t *testing.T) {
		for _, dialErr := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, context.DeadlineExceeded} {
			var mu sync.Mutex
			dialed := make(map[string]bool)
			ipv6ProbeDial = func(_ context.Context, _ string, addr string) (net.Conn, error) {
				mu.Lock()
				dialed[addr] = true
				mu.Unlock()
				return nil, dialErr
			}
			if err := verifyIPv6MetadataBlocked(context.Background()); err != nil {
				t.Fatalf("verifyIPv6MetadataBlocked() with %v = %v, want nil", dialErr, err)
			}
			for _, want := range []string{"[fd20:ce::254]:80", "[fd00:ec2::254]:80"} {
				if !dialed[want] {
					t.Errorf("expected dial attempt to %s, dialed=%v", want, dialed)
				}
			}
		}
	})

	t.Run("fails closed when an endpoint accepts a TCP connection regardless of NetnsHasIPv6", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, acceptErr := ln.Accept()
				if acceptErr != nil {
					return
				}
				_ = c.Close()
			}
		}()

		ipv6ProbeAddrs = []string{ln.Addr().String(), "127.0.0.1:1"}
		ipv6ProbeDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == "127.0.0.1:1" {
				return nil, syscall.ECONNREFUSED
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		// Even when NetnsHasIPv6() is false (as in IPv4-only containers),
		// a reachable endpoint must still fail closed.
		err = verifyIPv6MetadataBlocked(context.Background())
		if !errors.Is(err, errMetadataProtectionUnavailable) {
			t.Fatalf("verifyIPv6MetadataBlocked() = %v, want errMetadataProtectionUnavailable (NetnsHasIPv6=%v)", err, metadata.NetnsHasIPv6())
		}
	})
}

// TestRunInit_MetadataProtectionLegacyFallback verifies the old-broker +
// new-image compatibility path: when SCION_METADATA_INTERCEPTION=required and
// SCION_METADATA_INTERCEPTION_SOURCE is absent, init runs the optional
// firewall, starts the metadata server, installs iptables rules in-container,
// verifies the redirect via probe, and fatally drops CAP_NET_ADMIN before
// clone, hooks, sidecars, or harness.
func TestRunInit_MetadataProtectionLegacyFallback(t *testing.T) {
	for _, mode := range []string{"block", "assign"} {
		t.Run(mode+"/succeeds and drops NET_ADMIN before untrusted code", func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, nil)
			withPreflightEUID(t, 0)
			withPreflightNetAdminUsable(t, true, nil)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			t.Setenv(metadata.InterceptionSourceEnvVar, "")
			t.Setenv(initFirewallEnv, "1")
			var gotMode string
			var gotPort int
			withRunInstallInterceptionRules(t, func(m string, p int) error {
				gotMode, gotPort = m, p
				f.order = append(f.order, "install-rules")
				return nil
			})
			withRunWaitForRedirect(t, func(ctx context.Context, probePath, probeToken string) error {
				if probePath == "" || probeToken == "" {
					t.Errorf("waitForRedirect called with empty path/token: path=%q token=%q", probePath, probeToken)
				}
				if dl, ok := ctx.Deadline(); !ok {
					t.Error("legacy waitForRedirect context has no deadline")
				} else if remaining := time.Until(dl); remaining <= 0 || remaining > legacyRedirectProbeDeadline+time.Second {
					t.Errorf("legacy waitForRedirect deadline remaining = %s, want within %s", remaining, legacyRedirectProbeDeadline)
				}
				f.order = append(f.order, "probe")
				return nil
			})
			withRunVerifyIPv6Blocked(t, func(context.Context) error {
				f.order = append(f.order, "ipv6-probe")
				return nil
			})
			if got := f.run(); got != 0 {
				t.Fatalf("RunInit() = %d, want 0", got)
			}
			if want := []string{"firewall", "metadata", "install-rules", "probe", "ipv6-probe", "drop", "clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
				t.Fatalf("order = %v, want %v", f.order, want)
			}
			if gotMode != mode || gotPort != 18380 {
				t.Errorf("installRules(%q, %d), want (%q, 18380)", gotMode, gotPort, mode)
			}
			if !netAdminPathExists(f.hookObservedFile) {
				t.Error("pre-start hook ran before CAP_NET_ADMIN was dropped")
			}
			if !netAdminPathExists(f.harnessFile) {
				t.Fatal("harness did not run")
			}
		})

		t.Run(mode+"/rule install failure fails closed and cleans up tokens", func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, nil)
			observeReExec(t)
			withPreflightEUID(t, 0)
			withPreflightNetAdminUsable(t, true, nil)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			t.Setenv(metadata.InterceptionSourceEnvVar, "")
			creds := newCredentialFixture(t, f.agentHome)
			withRunInstallInterceptionRules(t, func(string, int) error {
				f.order = append(f.order, "install-rules")
				return metadata.ErrInterceptionUnavailable
			})
			if got := f.run(); got != 1 {
				t.Fatalf("RunInit() = %d, want 1", got)
			}
			if want := []string{"metadata", "install-rules"}; !reflect.DeepEqual(f.order, want) {
				t.Fatalf("order = %v, want %v", f.order, want)
			}
			if netAdminPathExists(creds.transportPath) {
				t.Error("transport token file still present after legacy install refusal")
			}
			if netAdminPathExists(creds.hubTokenPath) {
				t.Error("staged hub credential still present after legacy install refusal")
			}
			if netAdminPathExists(f.harnessFile) {
				t.Error("harness ran after legacy install failure")
			}
			phase, msg := readAgentInfoMessage(t, f.agentHome)
			if phase != string(state.PhaseError) || msg != goldenMetadataProtectionRefusal {
				t.Errorf("agent-info = (%q, %q), want (%q, %q)", phase, msg, state.PhaseError, goldenMetadataProtectionRefusal)
			}
		})

		t.Run(mode+"/probe failure fails closed and cleans up tokens", func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, nil)
			observeReExec(t)
			withPreflightEUID(t, 0)
			withPreflightNetAdminUsable(t, true, nil)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			t.Setenv(metadata.InterceptionSourceEnvVar, "")
			creds := newCredentialFixture(t, f.agentHome)
			withRunInstallInterceptionRules(t, func(string, int) error {
				f.order = append(f.order, "install-rules")
				return nil
			})
			withRunWaitForRedirect(t, func(context.Context, string, string) error {
				f.order = append(f.order, "probe")
				return errMetadataProtectionUnavailable
			})
			if got := f.run(); got != 1 {
				t.Fatalf("RunInit() = %d, want 1", got)
			}
			if want := []string{"metadata", "install-rules", "probe"}; !reflect.DeepEqual(f.order, want) {
				t.Fatalf("order = %v, want %v", f.order, want)
			}
			if netAdminPathExists(creds.transportPath) {
				t.Error("transport token file still present after legacy probe refusal")
			}
			if netAdminPathExists(creds.hubTokenPath) {
				t.Error("staged hub credential still present after legacy probe refusal")
			}
			if netAdminPathExists(f.harnessFile) {
				t.Error("harness ran after legacy probe failure")
			}
			phase, msg := readAgentInfoMessage(t, f.agentHome)
			if phase != string(state.PhaseError) || msg != goldenMetadataProtectionRefusal {
				t.Errorf("agent-info = (%q, %q), want (%q, %q)", phase, msg, state.PhaseError, goldenMetadataProtectionRefusal)
			}
		})

		t.Run(mode+"/CAP_NET_ADMIN drop failure is fatal even without firewall", func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, errDropDetail)
			observeReExec(t)
			withPreflightEUID(t, 0)
			withPreflightNetAdminUsable(t, true, nil)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			t.Setenv(metadata.InterceptionSourceEnvVar, "")
			creds := newCredentialFixture(t, f.agentHome)
			withRunInstallInterceptionRules(t, func(string, int) error {
				f.order = append(f.order, "install-rules")
				return nil
			})
			withRunWaitForRedirect(t, func(context.Context, string, string) error {
				f.order = append(f.order, "probe")
				return nil
			})
			if got := f.run(); got != 1 {
				t.Fatalf("RunInit() = %d, want 1", got)
			}
			if want := []string{"metadata", "install-rules", "probe", "drop"}; !reflect.DeepEqual(f.order, want) {
				t.Fatalf("order = %v, want %v", f.order, want)
			}
			if netAdminPathExists(creds.transportPath) {
				t.Error("transport token file still present after fatal drop failure")
			}
			if netAdminPathExists(creds.hubTokenPath) {
				t.Error("staged hub credential still present after fatal drop failure")
			}
			if netAdminPathExists(f.harnessFile) {
				t.Error("harness ran after fatal drop failure")
			}
			phase, msg := readAgentInfoMessage(t, f.agentHome)
			if phase != string(state.PhaseError) || msg != errNetAdminDropFailed.Error() {
				t.Errorf("agent-info = (%q, %q), want (%q, %q)", phase, msg, state.PhaseError, errNetAdminDropFailed.Error())
			}
		})

		t.Run(mode+"/firewall failure cleans up staged tokens and refuses harness", func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, nil)
			observeReExec(t)
			withPreflightEUID(t, 0)
			withPreflightNetAdminUsable(t, true, nil)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			t.Setenv(metadata.InterceptionSourceEnvVar, "")
			t.Setenv(initFirewallEnv, "1")
			creds := newCredentialFixture(t, f.agentHome)
			withRunInitFirewall(t, func() error {
				f.order = append(f.order, "firewall")
				return errors.New("iptables-restore failed")
			})
			if got := f.run(); got != 1 {
				t.Fatalf("RunInit() = %d, want 1", got)
			}
			if want := []string{"firewall"}; !reflect.DeepEqual(f.order, want) {
				t.Fatalf("order = %v, want %v", f.order, want)
			}
			if netAdminPathExists(creds.transportPath) {
				t.Error("transport token file still present after firewall failure")
			}
			if netAdminPathExists(creds.hubTokenPath) {
				t.Error("staged hub credential still present after firewall failure")
			}
			if netAdminPathExists(f.harnessFile) {
				t.Error("harness ran after firewall failure")
			}
			phase, msg := readAgentInfoMessage(t, f.agentHome)
			if phase != string(state.PhaseError) || msg != errInitFirewallFailed.Error() {
				t.Errorf("agent-info = (%q, %q), want (%q, %q)", phase, msg, state.PhaseError, errInitFirewallFailed.Error())
			}
		})
	}
}

func TestRunInit_IPv6NegativeProbeFailure_CleansUp(t *testing.T) {
	paths := []struct {
		name   string
		source string
		want   []string
	}{
		{name: "runtime", source: metadata.InterceptionSourceRuntime, want: []string{"metadata", "probe", "ipv6-probe"}},
		{name: "legacy", source: "", want: []string{"metadata", "install-rules", "probe", "ipv6-probe"}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			f := newNetAdminFixture(t, "block", false, nil)
			observeReExec(t)
			withPreflightEUID(t, 0)
			withPreflightNetAdminUsable(t, true, nil)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			t.Setenv(metadata.InterceptionSourceEnvVar, p.source)
			creds := newCredentialFixture(t, f.agentHome)
			withRunInstallInterceptionRules(t, func(string, int) error {
				f.order = append(f.order, "install-rules")
				return nil
			})
			withRunWaitForRedirect(t, func(context.Context, string, string) error {
				f.order = append(f.order, "probe")
				return nil
			})
			withRunVerifyIPv6Blocked(t, func(context.Context) error {
				f.order = append(f.order, "ipv6-probe")
				return errMetadataProtectionUnavailable
			})

			if got := f.run(); got != 1 {
				t.Fatalf("RunInit() = %d, want 1", got)
			}
			if !reflect.DeepEqual(f.order, p.want) {
				t.Fatalf("order = %v, want %v", f.order, p.want)
			}
			if netAdminPathExists(creds.transportPath) {
				t.Error("transport token file still present after IPv6 probe refusal")
			}
			if netAdminPathExists(creds.hubTokenPath) {
				t.Error("staged hub credential still present after IPv6 probe refusal")
			}
			if netAdminPathExists(f.harnessFile) {
				t.Error("harness ran after IPv6 probe failure")
			}
			phase, msg := readAgentInfoMessage(t, f.agentHome)
			if phase != string(state.PhaseError) || msg != goldenMetadataProtectionRefusal {
				t.Errorf("agent-info = (%q, %q), want (%q, %q)", phase, msg, state.PhaseError, goldenMetadataProtectionRefusal)
			}
		})
	}
}
