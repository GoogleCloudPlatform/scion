/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func withMetadataInterceptionInstalled(t *testing.T, installed bool) {
	t.Helper()
	orig := metadataInterceptionInstalled
	metadataInterceptionInstalled = func(*metadata.Server) bool { return installed }
	t.Cleanup(func() { metadataInterceptionInstalled = orig })
}

// errDropDetail stands in for the step/errno detail netcap returns; it must
// reach the log only, never the reported failure.
var errDropDetail = errors.New("drop CAP_NET_ADMIN from bounding set: detail-that-must-not-be-reported")

func TestDropNetAdminForMetadata(t *testing.T) {
	tests := []struct {
		name      string
		installed bool
		dropErr   error
		wantErr   error
	}{
		{"installed, dropped", true, nil, nil},
		{"installed, drop failed: fatal", true, errDropDetail, errNetAdminDropFailed},
		{"skipped, dropped anyway", false, nil, nil},
		{"skipped, drop failed: warn only", false, errDropDetail, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called bool
			withRunDropNetAdmin(t, func() error { called = true; return tt.dropErr })
			err := dropNetAdminForMetadata(tt.installed)
			if !called {
				t.Fatal("drop not attempted")
			}
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("dropNetAdminForMetadata(%v) = %v, want %v", tt.installed, err, tt.wantErr)
			}
		})
	}
}

func TestSetupInitFirewall(t *testing.T) {
	tests := []struct {
		value      string
		runErr     error
		wantCalled bool
		wantErr    bool
	}{
		{value: "", wantCalled: false},
		{value: "0", wantCalled: false},
		{value: "false", wantCalled: false},
		{value: "1", wantCalled: true},
		{value: "true", wantCalled: true},
		{value: "1", runErr: errors.New("exit status 1"), wantCalled: true, wantErr: true},
		{value: "yes please", wantCalled: false, wantErr: true}, // typo must not silently disable
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(initFirewallEnv, tt.value)
			var called bool
			withRunInitFirewall(t, func() error { called = true; return tt.runErr })
			err := setupInitFirewall()
			if called != tt.wantCalled {
				t.Errorf("firewall run = %v, want %v", called, tt.wantCalled)
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
	withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error {
		f.order = append(f.order, "metadata")
		return nil
	})
	withMetadataInterceptionInstalled(t, installed)
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

// TestRunInit_NetAdminDropFailure_FailsClosed: interception installed and
// the drop fails, so nothing workload-controlled may start, and the
// reported failure names only the capability.
func TestRunInit_NetAdminDropFailure_FailsClosed(t *testing.T) {
	f := newNetAdminFixture(t, "assign", true, errDropDetail)

	if got := f.run(); got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if want := []string{"metadata", "drop"}; !reflect.DeepEqual(f.order, want) {
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

// The refusal text the Hub shows when the required metadata protection
// cannot be set up. Pinned verbatim: changing it is a reviewed decision.
const goldenMetadataProtectionRefusal = "agent start refused: the configured metadata protection could not be set up in this container. Provide the required host capabilities, or set the metadata mode to passthrough explicitly to opt out."

func TestMetadataProtectionRefusalText_Golden(t *testing.T) {
	if got := errMetadataProtectionUnavailable.Error(); got != goldenMetadataProtectionRefusal {
		t.Fatalf("refusal text = %q, want exactly %q", got, goldenMetadataProtectionRefusal)
	}
}

// preflightChecks are the stubbed results of metadataProtectionPreflight's
// three checks; okPreflight passes all of them.
type preflightChecks struct {
	euid      int
	iptables  error
	usable    bool
	usableErr error
}

var okPreflight = preflightChecks{euid: 0, usable: true}

func withPreflightChecks(t *testing.T, c preflightChecks) {
	t.Helper()
	origEUID, origIPT, origUsable := preflightEUID, preflightResolveIPTables, preflightNetAdminUsable
	preflightEUID = func() int { return c.euid }
	preflightResolveIPTables = func() error { return c.iptables }
	preflightNetAdminUsable = func() (bool, error) { return c.usable, c.usableErr }
	t.Cleanup(func() {
		preflightEUID, preflightResolveIPTables, preflightNetAdminUsable = origEUID, origIPT, origUsable
	})
}

// preflightFailures is one row per condition the preflight refuses.
var preflightFailures = []struct {
	name        string
	checks      preflightChecks
	networkMode string
}{
	{name: "not root", checks: preflightChecks{euid: 1000, usable: true}},
	{name: "host network", checks: okPreflight, networkMode: "host"},
	{name: "iptables not resolvable", checks: preflightChecks{euid: 0, iptables: errors.New("not found in a root-owned directory"), usable: true}},
	{name: "capability missing", checks: preflightChecks{euid: 0, usable: false}},
	{name: "capability check failed", checks: preflightChecks{euid: 0, usableErr: errors.New("read thread status")}},
}

func TestMetadataProtectionPreflight(t *testing.T) {
	required := func(mode, networkMode string) *metadata.Config {
		return &metadata.Config{Mode: mode, NetworkMode: networkMode, InterceptionRequired: true}
	}
	for _, mode := range []string{"block", "assign"} {
		for _, tt := range preflightFailures {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				withPreflightChecks(t, tt.checks)
				if err := metadataProtectionPreflight(required(mode, tt.networkMode)); !errors.Is(err, errMetadataProtectionUnavailable) {
					t.Fatalf("metadataProtectionPreflight() = %v, want errMetadataProtectionUnavailable", err)
				}
			})
		}
		t.Run(mode+"/all checks pass", func(t *testing.T) {
			withPreflightChecks(t, okPreflight)
			if err := metadataProtectionPreflight(required(mode, "")); err != nil {
				t.Fatalf("metadataProtectionPreflight() = %v, want nil", err)
			}
		})
		// Without the runtime's required marker the preflight changes
		// nothing, however the checks would come out.
		t.Run(mode+"/marker off", func(t *testing.T) {
			withPreflightChecks(t, preflightChecks{euid: 1000, iptables: errors.New("missing")})
			if err := metadataProtectionPreflight(&metadata.Config{Mode: mode}); err != nil {
				t.Fatalf("metadataProtectionPreflight() = %v, want nil", err)
			}
		})
	}
	t.Run("no metadata config (passthrough)", func(t *testing.T) {
		withPreflightChecks(t, preflightChecks{euid: 1000})
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
				t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
				t.Setenv(initFirewallEnv, "1")
				if tt.networkMode != "" {
					t.Setenv("SCION_NETWORK_MODE", tt.networkMode)
				}
				withPreflightChecks(t, tt.checks)
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
				if msg != goldenMetadataProtectionRefusal {
					t.Errorf("reported message = %q, want exactly %q", msg, goldenMetadataProtectionRefusal)
				}
			})
		}
	}
}

// TestRunInit_MetadataProtectionPreflight_PassesWhenUsable: with the
// marker set and every check passing, the start goes on as before.
func TestRunInit_MetadataProtectionPreflight_PassesWhenUsable(t *testing.T) {
	for _, mode := range []string{"block", "assign"} {
		t.Run(mode, func(t *testing.T) {
			f := newNetAdminFixture(t, mode, true, nil)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			withPreflightChecks(t, okPreflight)
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

// TestRunInit_MetadataProtectionPreflight_MarkerOffUnchanged: without the
// marker (Kubernetes, rootless Podman, the Apple runtime, host networking)
// the start is unchanged even though every check would fail. A mutant
// that ignores the marker fails here.
func TestRunInit_MetadataProtectionPreflight_MarkerOffUnchanged(t *testing.T) {
	for _, mode := range []string{"block", "assign"} {
		t.Run(mode, func(t *testing.T) {
			f := newNetAdminFixture(t, mode, false, nil)
			withPreflightChecks(t, preflightChecks{euid: 1000, iptables: errors.New("missing")})
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
// passthrough runs no metadata server, so neither the marker nor failing
// checks change anything.
func TestRunInit_MetadataProtectionPreflight_PassthroughUnchanged(t *testing.T) {
	f := newNetAdminFixture(t, "passthrough", false, nil)
	t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
	withPreflightChecks(t, preflightChecks{euid: 1000, iptables: errors.New("missing")})
	if got := f.run(); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if want := []string{"clone", "sidecars"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("order = %v, want %v", f.order, want)
	}
}

// TestRunInit_MetadataStartRefusal_AfterStaging_CleansUp: the preflight
// passes but the real install fails (here, a stubbed start returning the
// interception error). That late refusal comes after staging, so it
// removes the transport token file and its pointer and, through the
// deferred finishStagedHubToken, the hub credential this init staged.
// File secrets written to their payload targets and the harness secrets
// in the in-memory dir stay, as on every other init failure after staging:
// init no longer has their names after the re-exec, and on Kubernetes the
// runtime fills that dir itself. A mutant that drops the cleanup fails
// here.
func TestRunInit_MetadataStartRefusal_AfterStaging_CleansUp(t *testing.T) {
	for _, mode := range []string{"block", "assign"} {
		t.Run(mode, func(t *testing.T) {
			f := newNetAdminFixture(t, mode, true, nil)
			observeReExec(t)
			t.Setenv(metadata.InterceptionEnvVar, metadata.InterceptionRequired)
			withPreflightChecks(t, okPreflight)
			creds := newCredentialFixture(t, f.agentHome)
			var transportAtStart, hubTokenAtStart, harnessSecretAtStart bool
			withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error {
				f.order = append(f.order, "metadata")
				transportAtStart = netAdminPathExists(creds.transportPath)
				hubTokenAtStart = netAdminPathExists(creds.hubTokenPath)
				harnessSecretAtStart = netAdminPathExists(creds.harnessSecretPath)
				return fmt.Errorf("%w: redirect: exit status 4", metadata.ErrInterceptionUnavailable)
			})

			if got := f.run(); got != 1 {
				t.Fatalf("RunInit() = %d, want 1", got)
			}
			if want := []string{"metadata"}; !reflect.DeepEqual(f.order, want) {
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
			if phase, _ := readAgentInfoMessage(t, f.agentHome); phase != string(state.PhaseError) {
				t.Errorf("phase = %q, want %q", phase, state.PhaseError)
			}
		})
	}
}
