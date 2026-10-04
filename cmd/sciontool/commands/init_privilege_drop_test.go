/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"gopkg.in/yaml.v3"
)

// TestParseCapBit covers parseCapBit generically (the function hasCapBit,
// substrate_privilege_drop.go, uses to check any capability in
// substratecaps.Required, not just
// SETUID) — bit 6 (CAP_SETGID), bit 1 (CAP_DAC_OVERRIDE) and bit 0
// (CAP_CHOWN) alongside a few edge cases, complementing TestParseCapSetUID's
// bit-7-specific coverage.
func TestParseCapBit(t *testing.T) {
	tests := []struct {
		name  string
		input string
		bit   uint
		want  bool
	}{
		{
			name:  "bit 6 (SETGID) present among limited caps",
			input: "Name:\tinit\nCapEff:\t00000000000000ff\n",
			bit:   6,
			want:  true,
		},
		{
			name:  "bit 6 (SETGID) absent (only SETUID bit set)",
			input: "Name:\tinit\nCapEff:\t0000000000000080\n",
			bit:   6,
			want:  false,
		},
		{
			name:  "only bit 6 set",
			input: "CapEff:\t0000000000000040\n",
			bit:   6,
			want:  true,
		},
		{
			name:  "bit 0 (CHOWN) set",
			input: "CapEff:\t0000000000000001\n",
			bit:   0,
			want:  true,
		},
		{
			name:  "bit 1 (DAC_OVERRIDE) absent (SETUID/SETGID/CHOWN set, DAC_OVERRIDE not)",
			input: "CapEff:\t00000000000000c1\n",
			bit:   1,
			want:  false,
		},
		{
			name:  "bit 1 (DAC_OVERRIDE) present",
			input: "CapEff:\t0000000000000002\n",
			bit:   1,
			want:  true,
		},
		{
			name:  "no capabilities",
			input: "CapEff:\t0000000000000000\n",
			bit:   6,
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCapBit(tc.input, tc.bit); got != tc.want {
				t.Errorf("parseCapBit(%q, %d) = %v, want %v", tc.input, tc.bit, got, tc.want)
			}
		})
	}
}

// TestDefaultScionUserLookup_RefusesUnderTest and
// TestDefaultLookupUserByID_RefusesUnderTest prove the second, independent
// defense in defaultScionUserLookup/defaultLookupUserByID's own doc
// comment: called directly (as if TestMain's own override of the
// scionUserLookup/lookupUserByID vars had been accidentally removed —
// exactly the incident these exist to catch a second time), neither may
// ever resolve a real account while running under `go test`.
func TestDefaultScionUserLookup_RefusesUnderTest(t *testing.T) {
	if _, err := defaultScionUserLookup("scion"); !errors.Is(err, errRealUserLookupDisabledUnderTest) {
		t.Errorf("defaultScionUserLookup(%q) error = %v, want errRealUserLookupDisabledUnderTest", "scion", err)
	}
}

func TestDefaultLookupUserByID_RefusesUnderTest(t *testing.T) {
	if _, err := defaultLookupUserByID("0"); !errors.Is(err, errRealUserLookupDisabledUnderTest) {
		t.Errorf("defaultLookupUserByID(%q) error = %v, want errRealUserLookupDisabledUnderTest", "0", err)
	}
}

// -----------------------------------------------------------------------
// substrate-serve's wiring, exercised directly so a test can catch a
// regression in it.
// -----------------------------------------------------------------------

// doSubstrateServeJSON drives an HTTP request through a *substrate.Server's
// Handler() the same way pkg/sciontool/substrate's own tests do, without
// this package importing that type by name (Go infers it from
// newSubstrateServeServer's return value).
func doSubstrateServeJSON(t *testing.T, srv interface{ Handler() http.Handler }, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestSubstrateServeInitOptions_RequiresPrivilegeDrop asserts that
// substrate-serve's InitRunner passes RequirePrivilegeDrop: true to RunInit:
// substrate must never start the harness as root. A regression that flips
// this function's literal to RequirePrivilegeDrop: false fails this test.
func TestSubstrateServeInitOptions_RequiresPrivilegeDrop(t *testing.T) {
	withScionUserLookup(t, func(username string) (*user.User, error) {
		return &user.User{Username: username, Uid: strconv.Itoa(os.Getuid()), Gid: strconv.Itoa(os.Getgid()), HomeDir: t.TempDir()}, nil
	})

	opts := substrateServeInitOptions(true)
	if !opts.RequirePrivilegeDrop {
		t.Error("substrateServeInitOptions(...).RequirePrivilegeDrop = false, want true — substrate must never start the harness as root")
	}
	if opts.DisableTermSignalForwarding {
		t.Error("substrateServeInitOptions(true).DisableTermSignalForwarding = true, want false (passthrough)")
	}
	if opts.ResolveWorkingDir == nil {
		t.Error("substrateServeInitOptions(true).ResolveWorkingDir = nil, want a resolver — RunInit calls it after preparing the workspace")
	}
	if opts.WorkingDir != "" {
		t.Errorf("substrateServeInitOptions(true).WorkingDir = %q, want \"\" — resolution is deferred to ResolveWorkingDir", opts.WorkingDir)
	}
	if !opts.DisablePortForwarding {
		t.Error("substrateServeInitOptions(...).DisablePortForwarding = false, want true — Substrate's egress cannot reach the hub port-forward tunnel")
	}
	if !opts.DisableReExec {
		t.Error("substrateServeInitOptions(...).DisableReExec = false, want true — a re-exec would replace substrate-serve's bootstrapped PID 1")
	}
	opts2 := substrateServeInitOptions(false)
	if !opts2.DisableTermSignalForwarding {
		t.Error("substrateServeInitOptions(false).DisableTermSignalForwarding = false, want true (passthrough)")
	}
}

// TestNewSubstrateServeServer_PrivilegeDropPreconditionRejectsBootstrap
// drives the exact server construction runSubstrateServe uses (real
// PrivilegeDropChecker; a stubbed init runner — see newSubstrateServeServer's
// doc comment for why it's never the real RunInit in a test) and proves the
// wiring itself: with SCION_HOST_UID/GID absent from the process
// environment, the deterministic branch of checkPrivilegeDropFeasible, the
// bootstrap must be rejected without ever invoking the init runner. If
// WithPrivilegeDropChecker were ever dropped from newSubstrateServeServer,
// this test would instead see 200 and its own assertions would fail —
// cleanly, as a normal test failure, not by driving a real RunInit.
func TestNewSubstrateServeServer_PrivilegeDropPreconditionRejectsBootstrap(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_HOST_GID", "")
	scrubHubEnv(t)

	var initCalled bool
	stubRunInit := func(argv []string, opts InitRunOptions) int {
		initCalled = true
		return 0
	}

	srv := newSubstrateServeServer(stubRunInit)
	rec := doSubstrateServeJSON(t, srv, "POST", "/scion/v1/bootstrap", "any-token", map[string]any{
		"env":           map[string]string{},
		"files":         []any{},
		"start_cmd":     "true",
		"control_token": "tok",
	})

	if rec.Code == 200 || rec.Code < 400 {
		t.Errorf("status = %d, want a non-2xx rejection (SCION_HOST_UID/GID are unset)", rec.Code)
	}
	// Give any wrongly-started goroutine a moment to flip the flag before
	// asserting it never did (the real init runner call happens
	// asynchronously — see handleBootstrap).
	time.Sleep(20 * time.Millisecond)
	if initCalled {
		t.Error("the init runner was invoked despite the privilege-drop precondition failing; the harness must never start")
	}
}

// -----------------------------------------------------------------------
// RunInit's defence-in-depth failure reporting (real integration —
// drives the actual RunInit, not a stub).
// -----------------------------------------------------------------------

// TestReportInitFailure_WritesPhaseErrorAndMessage proves the shared
// reporting helper every RunInit failure path (including the privilege-drop
// defence-in-depth check) calls: it must report PhaseError to local
// agent-info state (the same way the git-clone failure path does),
// not just log it. Driven directly against a temp directory rather than
// through RunInit's real setupHostUser/resolveAgentHome, which resolve
// against whatever "scion" user (if any) actually exists on the machine
// running the test.
func TestReportInitFailure_WritesPhaseErrorAndMessage(t *testing.T) {
	scrubHubEnv(t)
	tmpHome := t.TempDir()

	reportInitFailure(tmpHome, errPrivilegeDropRequired)

	raw, err := os.ReadFile(filepath.Join(tmpHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("expected agent-info.json to be written: %v", err)
	}
	var info struct {
		Phase  string `json:"phase"`
		Detail struct {
			Message string `json:"message"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal agent-info.json %q: %v", raw, err)
	}
	if info.Phase != string(state.PhaseError) {
		t.Errorf("agent-info.json phase = %q, want %q", info.Phase, state.PhaseError)
	}
	if info.Detail.Message != errPrivilegeDropRequired.Error() {
		t.Errorf("agent-info.json detail.message = %q, want %q", info.Detail.Message, errPrivilegeDropRequired.Error())
	}
}

// TestRunInit_PrivilegeDropFailure_ReturnsSentinel drives the real RunInit
// with RequirePrivilegeDrop: true and SCION_HOST_UID/GID unset — the one
// combination that fails closed regardless of whether this test happens to
// run as root, with real capabilities, or as a machine account literally
// named "scion" (setupHostUser's rootless-shortcut branch), since none of
// those branches ever produce a non-zero targetUID without
// SCION_HOST_UID/GID set. It asserts only the sentinel return code, which
// is independent of where resolveAgentHome happens to land on this
// particular machine (see TestReportInitFailure_* for that part).
func TestRunInit_PrivilegeDropFailure_ReturnsSentinel(t *testing.T) {
	scrubHubEnv(t)
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_HOST_GID", "")
	t.Setenv("HOME", t.TempDir())

	got := RunInit([]string{"true"}, InitRunOptions{DisableTermSignalForwarding: true, RequirePrivilegeDrop: true})
	if got != exitCodePrivilegeDropRequired {
		t.Fatalf("RunInit() = %d, want exitCodePrivilegeDropRequired (%d)", got, exitCodePrivilegeDropRequired)
	}
}

// TestRunInit_CallsStartReaperSeamExactlyOnce pins RunInit to calling the
// startReaper package var — not procreap.StartReaper directly — so TestMain's
// own startReaper = func() {} stub actually reaches the call RunInit makes.
// Without this seam, every RunInit call in this test binary would install a
// real, process-wide SIGCHLD handler that reaps any zombie child no
// in-flight exec.Cmd has claimed — including one a *different* test's own
// exec.Cmd.Wait is still waiting on, which races it and fails with ECHILD.
// Reusing TestRunInit_PrivilegeDropFailure_ReturnsSentinel's own
// RequirePrivilegeDrop: true setup drives the shortest real path to that
// call: startReaper runs as RunInit's very first statement, before the
// privilege-drop gate itself returns the sentinel exit code.
func TestRunInit_CallsStartReaperSeamExactlyOnce(t *testing.T) {
	scrubHubEnv(t)
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_HOST_GID", "")
	t.Setenv("HOME", t.TempDir())

	var calls int
	withStartReaper(t, func() { calls++ })

	got := RunInit([]string{"true"}, InitRunOptions{DisableTermSignalForwarding: true, RequirePrivilegeDrop: true})
	if got != exitCodePrivilegeDropRequired {
		t.Fatalf("RunInit() = %d, want exitCodePrivilegeDropRequired (%d)", got, exitCodePrivilegeDropRequired)
	}
	if calls != 1 {
		t.Errorf("startReaper seam called %d time(s), want exactly 1", calls)
	}
}

// TestRunInit_StagedSecretsDecodeFailure_ReportsInitFailure drives the real
// RunInit through a *different* pre-launch failure path than the
// privilege-drop gate, to prove reportInitFailure was actually wired at
// this call site too, not just the privilege-drop one — every RunInit
// failure path that returns before the harness launches must report, not
// just the one this package's tests happen to exercise most. SCION_STAGED_SECRETS
// holding undecodable data makes stagedsecrets.Decode fail before anything
// else in RunInit runs.
func TestRunInit_StagedSecretsDecodeFailure_ReportsInitFailure(t *testing.T) {
	scrubHubEnv(t)
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_HOST_GID", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(stagedsecrets.EnvVar, "not valid base64 or json!!!")

	got := RunInit([]string{"true"}, InitRunOptions{DisableTermSignalForwarding: true})
	if got == 0 {
		t.Fatal("RunInit() = 0, want non-zero for an undecodable SCION_STAGED_SECRETS payload")
	}

	raw, err := os.ReadFile(filepath.Join(tmpHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("expected agent-info.json to be written: %v", err)
	}
	var info struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal agent-info.json %q: %v", raw, err)
	}
	if info.Phase != string(state.PhaseError) {
		t.Errorf("agent-info.json phase = %q, want %q", info.Phase, state.PhaseError)
	}
}

// TestRunInit_StagedSecretsWriteFailure_HardErrorsAndNeverRunsChild pins
// init.go's stagedsecrets.Write failure branch as a hard error (return 1),
// never a swallow-and-continue. A planted symlink at
// <agentHome>/.scion/secrets.json (e.g. left over from a persisted home,
// simulating a restart) makes stagedsecrets.Write refuse — this proves
// RunInit surfaces that refusal as a non-zero exit, reports the error phase,
// leaves the symlink's target completely untouched, and never reaches the
// point where it would exec the child command at all (the child argv here,
// "definitely-not-a-real-binary-xyz", would itself fail loudly if exec were
// ever attempted, but RunInit must never get that far in the first place).
func TestRunInit_StagedSecretsWriteFailure_HardErrorsAndNeverRunsChild(t *testing.T) {
	scrubHubEnv(t)
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_HOST_GID", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(tmpHome, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(scionDir, "secrets.json")); err != nil {
		t.Fatal(err)
	}

	staged := stagedsecrets.Staged{VariableSecrets: map[string]string{"token": "abc123"}}
	data, err := json.Marshal(staged)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(stagedsecrets.EnvVar, base64.StdEncoding.EncodeToString(data))

	got := RunInit([]string{"definitely-not-a-real-binary-xyz"}, InitRunOptions{DisableTermSignalForwarding: true})
	if got == 0 {
		t.Fatal("RunInit() = 0, want non-zero when stagedsecrets.Write refuses a symlinked secrets.json")
	}

	link, err := os.Readlink(filepath.Join(scionDir, "secrets.json"))
	if err != nil {
		t.Fatalf("secrets.json is no longer a symlink after the refused write: %v", err)
	}
	if link != victim {
		t.Errorf("secrets.json symlink target = %q, want %q (unchanged)", link, victim)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(content) != "untouched" {
		t.Errorf("victim content = %q, want %q (unchanged — the refused write must never reach it)", content, "untouched")
	}

	raw, err := os.ReadFile(filepath.Join(tmpHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("expected agent-info.json to be written: %v", err)
	}
	var info struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal agent-info.json %q: %v", raw, err)
	}
	if info.Phase != string(state.PhaseError) {
		t.Errorf("agent-info.json phase = %q, want %q", info.Phase, state.PhaseError)
	}
}

// -----------------------------------------------------------------------
// adjustScionUser's requirePrivilegeDrop-gated fail-closed checks.
// -----------------------------------------------------------------------

// withScionUserLookup temporarily overrides the scionUserLookup package var.
func withScionUserLookup(t *testing.T, f func(string) (*user.User, error)) {
	t.Helper()
	orig := scionUserLookup
	scionUserLookup = f
	t.Cleanup(func() { scionUserLookup = orig })
}

// withRunDirectSetUID temporarily overrides the runDirectSetUID package var.
func withRunDirectSetUID(t *testing.T, f func(string, string, string, bool) error) {
	t.Helper()
	orig := runDirectSetUID
	runDirectSetUID = f
	t.Cleanup(func() { runDirectSetUID = orig })
}

func TestAdjustScionUser_AlreadyCorrect_ShortCircuitsRegardlessOfFlag(t *testing.T) {
	withScionUserLookup(t, func(string) (*user.User, error) {
		return &user.User{Uid: "1000", Gid: "1000"}, nil
	})
	var directCalled bool
	withRunDirectSetUID(t, func(string, string, string, bool) error {
		directCalled = true
		return nil
	})

	for _, requirePrivilegeDrop := range []bool{true, false} {
		uid, gid, rootless := adjustScionUser(1000, 1000, "1000", "1000", requirePrivilegeDrop)
		if uid != 1000 || gid != 1000 || rootless != false {
			t.Errorf("requirePrivilegeDrop=%v: adjustScionUser() = (%d,%d,%v), want (1000,1000,false)", requirePrivilegeDrop, uid, gid, rootless)
		}
	}
	if directCalled {
		t.Error("directSetUID was called even though the scion user already had the correct UID/GID")
	}
}

// TestAdjustScionUser_ScionUserNotFound covers the first fail-closed case: under
// RequirePrivilegeDrop, a missing scion user must fail closed; every other
// runtime (RequirePrivilegeDrop: false) must see the same (uid, gid, false)
// result, since those runtimes depend on that lenient fallback.
func TestAdjustScionUser_ScionUserNotFound(t *testing.T) {
	withScionUserLookup(t, func(string) (*user.User, error) {
		return nil, errors.New("user: unknown user scion")
	})
	// Force the direct-edit branch (skip a real usermod/groupmod exec call).
	t.Setenv("SCION_ALT_USERMOD", "1")

	t.Run("RequirePrivilegeDrop=true fails closed", func(t *testing.T) {
		var calls int
		withRunDirectSetUID(t, func(string, string, string, bool) error {
			calls++
			return nil
		})
		uid, gid, rootless := adjustScionUser(1000, 1000, "1000", "1000", true)
		if uid != 0 || gid != 0 || rootless != false {
			t.Errorf("adjustScionUser(..., true) = (%d,%d,%v), want (0,0,false)", uid, gid, rootless)
		}
		// runDirectSetUID must never be called once the early return fires:
		// this pins "never even try the rewrite," not just "the return
		// value happens to come out right."
		if calls != 0 {
			t.Errorf("runDirectSetUID was called %d time(s) after a failed scion user lookup with requirePrivilegeDrop=true, want 0 — the early return must skip the rewrite attempt entirely, not just fail closed on its result", calls)
		}
	})

	t.Run("RequirePrivilegeDrop=false is unchanged", func(t *testing.T) {
		withRunDirectSetUID(t, func(string, string, string, bool) error { return nil })
		uid, gid, rootless := adjustScionUser(1000, 1000, "1000", "1000", false)
		if uid != 1000 || gid != 1000 || rootless != false {
			t.Errorf("adjustScionUser(..., false) = (%d,%d,%v), want (1000,1000,false) — unenforced callers must stay byte-identical", uid, gid, rootless)
		}
	})
}

// TestAdjustScionUser_DirectSetUIDRewroteNothing covers the second
// fail-closed case.
func TestAdjustScionUser_DirectSetUIDRewroteNothing(t *testing.T) {
	// An existing (but mismatched) scion user, so adjustScionUser proceeds
	// past the "already correct" shortcut into the rewrite attempt.
	withScionUserLookup(t, func(string) (*user.User, error) {
		return &user.User{Uid: "2000", Gid: "2000"}, nil
	})
	withRunDirectSetUID(t, func(string, string, string, bool) error {
		return wrapPasswdEntryNotRewritten(errPasswdEntryNotRewritten)
	})
	t.Setenv("SCION_ALT_USERMOD", "1")

	t.Run("RequirePrivilegeDrop=true fails closed", func(t *testing.T) {
		uid, gid, rootless := adjustScionUser(1000, 1000, "1000", "1000", true)
		if uid != 0 || gid != 0 || rootless != false {
			t.Errorf("adjustScionUser(..., true) = (%d,%d,%v), want (0,0,false)", uid, gid, rootless)
		}
	})

	t.Run("RequirePrivilegeDrop=false is unchanged", func(t *testing.T) {
		uid, gid, rootless := adjustScionUser(1000, 1000, "1000", "1000", false)
		if uid != 1000 || gid != 1000 || rootless != false {
			t.Errorf("adjustScionUser(..., false) = (%d,%d,%v), want (1000,1000,false) — unenforced callers must stay byte-identical", uid, gid, rootless)
		}
	})
}

// TestAdjustScionUser_DirectSetUIDOtherError_AlwaysFailsClosed proves the
// requirePrivilegeDrop gate only widens what fails closed; it does not
// narrow the pre-existing behaviour where ANY real directSetUID error
// (e.g. the sed command itself failing) already failed closed unconditionally
// for every runtime.
func TestAdjustScionUser_DirectSetUIDOtherError_AlwaysFailsClosed(t *testing.T) {
	withScionUserLookup(t, func(string) (*user.User, error) {
		return &user.User{Uid: "2000", Gid: "2000"}, nil
	})
	withRunDirectSetUID(t, func(string, string, string, bool) error {
		return errors.New("sed /etc/group: exit status 1 (output: sed: -e expression #1, char 1: unknown option to `s')")
	})
	t.Setenv("SCION_ALT_USERMOD", "1")

	for _, requirePrivilegeDrop := range []bool{true, false} {
		uid, gid, rootless := adjustScionUser(1000, 1000, "1000", "1000", requirePrivilegeDrop)
		if uid != 0 || gid != 0 || rootless != false {
			t.Errorf("requirePrivilegeDrop=%v: adjustScionUser() = (%d,%d,%v), want (0,0,false) (a real command failure, pre-existing behaviour)", requirePrivilegeDrop, uid, gid, rootless)
		}
	}
}

// TestAdjustScionUser_PostAdjustVerifyMismatch covers the third fail-closed case.
func TestAdjustScionUser_PostAdjustVerifyMismatch(t *testing.T) {
	var calls int
	withScionUserLookup(t, func(string) (*user.User, error) {
		calls++
		if calls == 1 {
			// Pre-adjustment: mismatched, so we proceed to rewrite.
			return &user.User{Uid: "2000", Gid: "2000"}, nil
		}
		// Post-adjustment verify: still shows the OLD uid/gid — the sed/
		// usermod step silently did not take effect.
		return &user.User{Uid: "2000", Gid: "2000"}, nil
	})
	withRunDirectSetUID(t, func(string, string, string, bool) error { return nil })
	t.Setenv("SCION_ALT_USERMOD", "1")

	t.Run("RequirePrivilegeDrop=true fails closed", func(t *testing.T) {
		calls = 0
		uid, gid, rootless := adjustScionUser(1000, 1000, "1000", "1000", true)
		if uid != 0 || gid != 0 || rootless != false {
			t.Errorf("adjustScionUser(..., true) = (%d,%d,%v), want (0,0,false)", uid, gid, rootless)
		}
	})

	t.Run("RequirePrivilegeDrop=false is unchanged", func(t *testing.T) {
		calls = 0
		uid, gid, rootless := adjustScionUser(1000, 1000, "1000", "1000", false)
		if uid != 1000 || gid != 1000 || rootless != false {
			t.Errorf("adjustScionUser(..., false) = (%d,%d,%v), want (1000,1000,false) — unenforced callers must stay byte-identical", uid, gid, rootless)
		}
	})
}

// -----------------------------------------------------------------------
// directSetUID's "no entry to rewrite" detection.
// -----------------------------------------------------------------------

// TestDirectSetUIDAt_NoEntryToRewrite_ReturnsError proves directSetUIDAt,
// run against a passwd file with no entry for username, still chowns the
// home directory — the sed simply matches nothing and exits 0 — while the
// pre-check only changes what directSetUIDAt *reports*, not what it *does*:
// home gets chowned either way, and only an enforced requirePrivilegeDrop=true
// caller treats the report as fatal (adjustScionUser, tested separately).
func TestDirectSetUIDAt_NoEntryToRewrite_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	groupPath := filepath.Join(dir, "group")
	passwdPath := filepath.Join(dir, "passwd")
	if err := os.WriteFile(groupPath, []byte("other:x:2000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwdPath, []byte("other:x:2000:2000:Other User:/home/other:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chowns := recordDirectSetUIDAtChowns(t)

	// newUID/newGID are the test process's own uid/gid, not "1000": os.Chown
	// to a *different* uid/gid requires CAP_CHOWN, which this test process
	// doesn't have, but a self-chown still exercises the call this test is
	// checking for.
	self := strconv.Itoa(os.Getuid())
	selfGID := strconv.Itoa(os.Getgid())
	err := directSetUIDAt("scion", self, selfGID, groupPath, passwdPath, homeDir, false)
	if !errors.Is(err, errPasswdEntryNotRewritten) {
		t.Fatalf("directSetUIDAt() = %v, want an error wrapping errPasswdEntryNotRewritten", err)
	}

	// Confirm nothing was rewritten in the passwd/group files as a side
	// effect of detecting this (the sed's substitute matched nothing, as
	// intended).
	groupContent, _ := os.ReadFile(groupPath)
	if !strings.Contains(string(groupContent), "other:x:2000:") {
		t.Errorf("group file was modified despite no matching entry: %q", groupContent)
	}

	// But the home chown must still have happened — see the doc comment
	// above.
	assertHomeChowned(t, *chowns)
}

// TestDirectSetUIDAt_PasswdEntryDisabledAccount_HomeStillChownedButReportsError
// covers a "scion:*:" entry: it exists, but is disabled (no leading "x" —
// see passwdEntryExists's own doc comment on why it anchors on
// "username:x:" and not just "username:"), so it still doesn't match the
// sed pattern either (which also anchors on ":x:"). This is the case
// passwdEntryExists's tightened prefix check made newly reachable, and
// pins that check against loosening back to a bare "username:" prefix.
// Home must still be chowned, exactly like the missing-entry case above.
func TestDirectSetUIDAt_PasswdEntryDisabledAccount_HomeStillChownedButReportsError(t *testing.T) {
	dir := t.TempDir()
	groupPath := filepath.Join(dir, "group")
	passwdPath := filepath.Join(dir, "passwd")
	if err := os.WriteFile(groupPath, []byte("scion:x:2000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwdPath, []byte("scion:*:2000:2000:Scion (disabled):/home/scion:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chowns := recordDirectSetUIDAtChowns(t)

	self := strconv.Itoa(os.Getuid())
	selfGID := strconv.Itoa(os.Getgid())
	err := directSetUIDAt("scion", self, selfGID, groupPath, passwdPath, homeDir, false)
	if !errors.Is(err, errPasswdEntryNotRewritten) {
		t.Fatalf("directSetUIDAt() = %v, want an error wrapping errPasswdEntryNotRewritten (a \"scion:*:\" line is not a rewritable entry)", err)
	}

	passwdContent, _ := os.ReadFile(passwdPath)
	if !strings.Contains(string(passwdContent), "scion:*:2000:2000:") {
		t.Errorf("passwd file was modified despite no \":x:\" entry to match: %q", passwdContent)
	}

	assertHomeChowned(t, *chowns)
}

// directSetUIDAtChownCall records one directSetUIDAtChownAt invocation: name
// is the directory entry it chowned (name "." means the home directory fd
// itself — see directSetUIDAtChownAt's own doc comment).
type directSetUIDAtChownCall struct {
	name  string
	flags int
}

// recordDirectSetUIDAtChowns overrides directSetUIDAtChownAt for the rest of
// the test and returns the calls it's made with, in order — the portable,
// deterministic replacement for a filesystem-ctime-based chown detector
// (see directSetUIDAtChownAt's own doc comment for why ctime doesn't work
// here). The recorded fake still calls through to the real unix.Fchownat, so
// these tests keep exercising the real syscall, not just the recording.
func recordDirectSetUIDAtChowns(t *testing.T) *[]directSetUIDAtChownCall {
	t.Helper()
	orig := directSetUIDAtChownAt
	var calls []directSetUIDAtChownCall
	directSetUIDAtChownAt = func(dirFd int, name string, uid, gid, flags int) error {
		calls = append(calls, directSetUIDAtChownCall{name: name, flags: flags})
		return orig(dirFd, name, uid, gid, flags)
	}
	t.Cleanup(func() { directSetUIDAtChownAt = orig })
	return &calls
}

// assertHomeChowned fails the test unless calls (as recorded by
// recordDirectSetUIDAtChowns) includes the home directory fd's own chown
// (name == ".").
func assertHomeChowned(t *testing.T, calls []directSetUIDAtChownCall) {
	t.Helper()
	for _, c := range calls {
		if c.name == "." {
			return
		}
	}
	t.Errorf("directSetUIDAtChownAt was never called for the home directory fd itself — the home chown must run even when there's no passwd entry to rewrite (calls: %v)", calls)
}

// recordHomeEntryChowns overrides homeEntryChownTestHook for the rest of the
// test and returns the names of every $HOME entry directSetUIDAt actually
// attempted to chown, in order — unlike recordDirectSetUIDAtChowns, this
// identifies entries by name on every platform, including GOOS=linux where
// the real per-entry chown call itself carries an empty name (see
// homeEntryChownTestHook's own doc comment).
func recordHomeEntryChowns(t *testing.T) *[]string {
	t.Helper()
	orig := homeEntryChownTestHook
	var names []string
	homeEntryChownTestHook = func(name string) {
		names = append(names, name)
	}
	t.Cleanup(func() { homeEntryChownTestHook = orig })
	return &names
}

// sawName reports whether names contains want.
func sawName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestDirectSetUIDAt_SymlinkedHomeDir_EnforcedRefusesUnenforcedSkips proves
// that when homeDir itself is a symlink (planted ahead of a
// restart on a persisted home), directSetUIDAt never follows it — under
// requirePrivilegeDrop it refuses outright before touching anything, and
// otherwise it logs and simply skips the chown pass, in both cases leaving
// the symlink's target completely untouched.
func TestDirectSetUIDAt_SymlinkedHomeDir_EnforcedRefusesUnenforcedSkips(t *testing.T) {
	for _, requirePrivilegeDrop := range []bool{true, false} {
		t.Run(fmt.Sprintf("requirePrivilegeDrop=%v", requirePrivilegeDrop), func(t *testing.T) {
			dir := t.TempDir()
			groupPath := filepath.Join(dir, "group")
			passwdPath := filepath.Join(dir, "passwd")
			if err := os.WriteFile(groupPath, []byte("scion:x:2000:\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(passwdPath, []byte("scion:x:2000:2000:Scion:/home/scion:/bin/sh\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			victim := t.TempDir()
			if err := os.Chmod(victim, 0o750); err != nil {
				t.Fatal(err)
			}
			wantInfo, err := os.Lstat(victim)
			if err != nil {
				t.Fatal(err)
			}
			homeDir := filepath.Join(dir, "home")
			if err := os.Symlink(victim, homeDir); err != nil {
				t.Fatal(err)
			}

			err = directSetUIDAt("scion", "1000", "1000", groupPath, passwdPath, homeDir, requirePrivilegeDrop)
			if requirePrivilegeDrop && err == nil {
				t.Fatal("directSetUIDAt() = nil error, want a refusal for a symlinked home directory under requirePrivilegeDrop")
			}
			if !requirePrivilegeDrop && err != nil {
				t.Errorf("directSetUIDAt() = %v, want nil (unenforced mode logs and skips the chown pass, but the passwd/group rewrite still succeeds)", err)
			}

			gotInfo, err := os.Lstat(victim)
			if err != nil {
				t.Fatal(err)
			}
			if gotInfo.Mode() != wantInfo.Mode() {
				t.Errorf("victim mode changed: got %v, want %v (home symlink must never be followed)", gotInfo.Mode(), wantInfo.Mode())
			}
			link, err := os.Readlink(homeDir)
			if err != nil {
				t.Fatalf("homeDir is no longer a symlink: %v", err)
			}
			if link != victim {
				t.Errorf("homeDir symlink target = %q, want %q (unchanged)", link, victim)
			}
		})
	}
}

// TestDirectSetUIDAt_SymlinkedHomeEntry_ChownsLinkNotTarget proves that a
// symlinked ENTRY inside homeDir (homeDir itself a real
// directory) is chowned via AT_SYMLINK_NOFOLLOW — the link itself, never
// whatever it points at — while a normal, non-symlink entry is still
// chowned as usual.
func TestDirectSetUIDAt_SymlinkedHomeEntry_ChownsLinkNotTarget(t *testing.T) {
	dir := t.TempDir()
	groupPath := filepath.Join(dir, "group")
	passwdPath := filepath.Join(dir, "passwd")
	if err := os.WriteFile(groupPath, []byte("scion:x:2000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwdPath, []byte("scion:x:2000:2000:Scion:/home/scion:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(homeDir, "x")); err != nil {
		t.Fatal(err)
	}
	// A normal, non-symlink entry alongside the planted symlink, so the
	// fix's "normal entries are still chowned" half is pinned too.
	normalEntry := filepath.Join(homeDir, "normal")
	if err := os.WriteFile(normalEntry, []byte("skel"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := recordHomeEntryChowns(t)

	self := strconv.Itoa(os.Getuid())
	selfGID := strconv.Itoa(os.Getgid())
	if err := directSetUIDAt("scion", self, selfGID, groupPath, passwdPath, homeDir, true); err != nil {
		t.Fatalf("directSetUIDAt() = %v, want nil", err)
	}

	gotInfo, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode() != wantInfo.Mode() {
		t.Errorf("victim mode changed: got %v, want %v (symlink target must never be chowned)", gotInfo.Mode(), wantInfo.Mode())
	}

	if !sawName(*names, "x") {
		t.Error("the symlinked entry \"x\" was never a chown target")
	}
	if !sawName(*names, "normal") {
		t.Error("the normal entry \"normal\" was never a chown target")
	}
}

// TestDirectSetUIDAt_EnforcedChownsPreexistingSubdirectory asserts that
// under enforcement, directSetUIDAt still chowns a pre-existing $HOME
// subdirectory. The hardlink-regular-file skip below is gated on
// unix.S_IFREG precisely because a directory legitimately has Nlink >= 2
// (from its own "." entry and every subdirectory's ".."); checking Nlink
// alone, without the regular-file type guard, would misclassify every such
// subdirectory as "hardlinked" and leave it un-chowned.
func TestDirectSetUIDAt_EnforcedChownsPreexistingSubdirectory(t *testing.T) {
	dir := t.TempDir()
	groupPath := filepath.Join(dir, "group")
	passwdPath := filepath.Join(dir, "passwd")
	if err := os.WriteFile(groupPath, []byte("scion:x:2000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwdPath, []byte("scion:x:2000:2000:Scion:/home/scion:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(homeDir, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(subdir)
	if err != nil {
		t.Fatal(err)
	}
	if nlink := st.Sys().(*syscall.Stat_t).Nlink; nlink < 2 {
		t.Fatalf("subdir link count = %d, want >= 2 (an empty directory's own \".\" entry) for this test to exercise the type guard", nlink)
	}
	names := recordHomeEntryChowns(t)

	self := strconv.Itoa(os.Getuid())
	selfGID := strconv.Itoa(os.Getgid())
	if err := directSetUIDAt("scion", self, selfGID, groupPath, passwdPath, homeDir, true); err != nil {
		t.Fatalf("directSetUIDAt() = %v, want nil", err)
	}

	if !sawName(*names, "subdir") {
		t.Error("the pre-existing subdirectory \"subdir\" was never a chown target; want it chowned despite its own Nlink >= 2")
	}
}

// TestDirectSetUIDAt_EnforcedSkipsHardlinkedEntry asserts that under
// enforcement, directSetUIDAt skips chowning a hardlinked regular $HOME
// entry: AT_SYMLINK_NOFOLLOW on the chown itself defeats a symlinked entry,
// but a HARDLINKED REGULAR FILE has no symlink for that flag to stop at —
// it names the very same inode as an unrelated (possibly root-owned) file
// the workload does not own at all, planted by hard-linking into $HOME
// (which only needs write access to the directory, not ownership of the
// target). A planted hardlink entry must never reach the chown call at
// all, while a normal entry is still chowned as usual, and the victim's
// own ownership and mode never change.
func TestDirectSetUIDAt_EnforcedSkipsHardlinkedEntry(t *testing.T) {
	dir := t.TempDir()
	groupPath := filepath.Join(dir, "group")
	passwdPath := filepath.Join(dir, "passwd")
	if err := os.WriteFile(groupPath, []byte("scion:x:2000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwdPath, []byte("scion:x:2000:2000:Scion:/home/scion:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, filepath.Join(homeDir, "hardlinked")); err != nil {
		t.Fatal(err)
	}
	normalEntry := filepath.Join(homeDir, "normal")
	if err := os.WriteFile(normalEntry, []byte("skel"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := recordHomeEntryChowns(t)

	self := strconv.Itoa(os.Getuid())
	selfGID := strconv.Itoa(os.Getgid())
	if err := directSetUIDAt("scion", self, selfGID, groupPath, passwdPath, homeDir, true); err != nil {
		t.Fatalf("directSetUIDAt() = %v, want nil", err)
	}

	if sawName(*names, "hardlinked") {
		t.Error("the hardlinked entry was a chown target; want it skipped under enforcement")
	}
	if !sawName(*names, "normal") {
		t.Error("the normal entry was never a chown target")
	}

	gotInfo, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode() != wantInfo.Mode() {
		t.Errorf("victim mode changed: got %v, want %v", gotInfo.Mode(), wantInfo.Mode())
	}
	if gotInfo.Sys().(*syscall.Stat_t).Uid != wantInfo.Sys().(*syscall.Stat_t).Uid {
		t.Error("victim owner changed")
	}
}

func TestDirectSetUIDAt_RewritesExistingEntry(t *testing.T) {
	dir := t.TempDir()
	groupPath := filepath.Join(dir, "group")
	passwdPath := filepath.Join(dir, "passwd")
	if err := os.WriteFile(groupPath, []byte("scion:x:2000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwdPath, []byte("scion:x:2000:2000:Scion:/home/scion:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := directSetUIDAt("scion", "1000", "1000", groupPath, passwdPath, homeDir, false); err != nil {
		t.Fatalf("directSetUIDAt() = %v, want nil", err)
	}

	groupContent, err := os.ReadFile(groupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(groupContent), "scion:x:1000:") {
		t.Errorf("group file = %q, want it rewritten to GID 1000", groupContent)
	}
	passwdContent, err := os.ReadFile(passwdPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(passwdContent), "scion:x:1000:1000:") {
		t.Errorf("passwd file = %q, want it rewritten to UID/GID 1000", passwdContent)
	}
}

// TestDirectSetUIDAt_PasswdEntryNoMatchingGroupLine_GroupIsBestEffort proves
// a passwd entry that exists but whose primary group isn't literally named
// "scion" (e.g. `useradd -g users scion`) is a legitimate, real-world case
// — the group sed matching nothing must not fail the whole operation, and
// the passwd rewrite (the one that actually matters) must still succeed:
// only a real command failure or a missing *passwd* entry is an error.
func TestDirectSetUIDAt_PasswdEntryNoMatchingGroupLine_GroupIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	groupPath := filepath.Join(dir, "group")
	passwdPath := filepath.Join(dir, "passwd")
	// No "scion:" line in the group file at all — a user whose primary
	// group has a different name.
	if err := os.WriteFile(groupPath, []byte("users:x:100:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwdPath, []byte("scion:x:2000:100:Scion:/home/scion:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := directSetUIDAt("scion", "1000", "1000", groupPath, passwdPath, homeDir, false); err != nil {
		t.Fatalf("directSetUIDAt() = %v, want nil — a group sed matching nothing must not be an error", err)
	}

	groupContent, err := os.ReadFile(groupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(groupContent), "users:x:100:") {
		t.Errorf("group file = %q, want unchanged (no scion group entry to rewrite)", groupContent)
	}

	passwdContent, err := os.ReadFile(passwdPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(passwdContent), "scion:x:1000:1000:") {
		t.Errorf("passwd file = %q, want it rewritten to UID/GID 1000 despite the group not matching", passwdContent)
	}
}

// wrapPasswdEntryNotRewritten wraps err the way a real call site would
// (directSetUIDAt wraps errPasswdEntryNotRewritten with the path via
// fmt.Errorf("%w", ...)), so errors.Is still matches after runDirectSetUID
// is faked.
func wrapPasswdEntryNotRewritten(err error) error {
	return fmt.Errorf("/etc/group: %w", err)
}

// TestRunInit_Enforced_RootGIDFromSetupHostUser_FailsClosed pins RunInit's
// only production call to requirePrivilegeDropOrFail (in RunInit itself): it
// must forward setupHostUser's real targetUID and targetGID, unchanged, not
// a swapped argument or a hardcoded value. Each pair below fails the gate's
// UID>0 && GID>0 predicate for a different reason (a root GID, or a negative
// UID/GID), so a call site that substitutes the wrong value, or a predicate
// that only checks for a zero UID/GID instead of a non-positive one, lets at
// least one of them through.
func TestRunInit_Enforced_RootGIDFromSetupHostUser_FailsClosed(t *testing.T) {
	for _, pair := range [][2]int{{1000, 0}, {1000, -1}, {-1, 1000}} {
		scrubHubEnv(t)
		t.Setenv("HOME", t.TempDir())
		orig := runSetupHostUser
		uid, gid := pair[0], pair[1]
		runSetupHostUser = func(bool) (int, int, bool) { return uid, gid, false }
		t.Cleanup(func() { runSetupHostUser = orig })
		got := RunInit([]string{"true"}, InitRunOptions{DisableTermSignalForwarding: true, RequirePrivilegeDrop: true})
		if got != exitCodePrivilegeDropRequired {
			t.Errorf("setupHostUser=(%d,%d): RunInit() = %d, want exitCodePrivilegeDropRequired (%d)", uid, gid, got, exitCodePrivilegeDropRequired)
		}
	}
}

// TestRunInit_Enforced_RootlessFromSetupHostUser_FailsClosedBeforeServices
// pins that RunInit's enforced gate does not exempt setupHostUser's rootless
// result: setupHostUser returns (0, 0, true) both for a root process
// without CAP_SETUID and for an unmapped target UID, and in both cases PID 1
// is still real root. The gate must refuse (exit
// exitCodePrivilegeDropRequired) before git clone or services start ever
// run: those two downstream steps receive the same targetUID/targetGID the
// gate itself saw, and the supervisor's own independent refusal one layer
// down only protects the harness child, not git clone or the sidecar
// services started before it.
func TestRunInit_Enforced_RootlessFromSetupHostUser_FailsClosedBeforeServices(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	writeServicesYAMLWithInvalidEntry(t, agentHome)

	orig := runSetupHostUser
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	t.Cleanup(func() { runSetupHostUser = orig })

	cloneReached, servicesReached := false, false
	withRunGitCloneWorkspace(t, func(int, int, string, bool) error { cloneReached = true; return nil })
	withRunServicesStart(t, func(_ context.Context, _ *services.Manager, _ []api.ServiceSpec, uid, gid int, _ string, _ bool) error {
		servicesReached = true
		t.Logf("services started with uid=%d gid=%d in enforced mode", uid, gid)
		return nil
	})

	got := RunInit([]string{"true"}, InitRunOptions{DisableTermSignalForwarding: true, RequirePrivilegeDrop: true})
	if got != exitCodePrivilegeDropRequired {
		t.Errorf("setupHostUser=(0,0,rootless=true): RunInit() = %d, want exitCodePrivilegeDropRequired (%d)", got, exitCodePrivilegeDropRequired)
	}
	if cloneReached || servicesReached {
		t.Errorf("enforced + rootless root: gitClone reached=%v, servicesStart reached=%v; want neither (they would run as uid 0)", cloneReached, servicesReached)
	}
}

// TestSeamDefaultsAreRealFunctions pins that every test-only seam RunInit's
// setup path exposes (see runAdjustScionUser, runChownTreeRootOwned,
// setupHostUserHasCapSetUID, setupHostUserIsUIDMapped, setupHostUserGetuid,
// postPreStartGeteuid, runSetupHostUser, and runDirectSetUID's doc comments)
// still defaults to the real function it wraps. A default that silently
// became a stub would skip the checks or side effects those functions
// perform in production, while every test — which only ever reassigns the
// var for the duration of its own run, never inspects its starting value —
// would keep passing.
func TestSeamDefaultsAreRealFunctions(t *testing.T) {
	ptr := func(f any) uintptr { return reflect.ValueOf(f).Pointer() }
	for name, pair := range map[string][2]any{
		"runAdjustScionUser":        {runAdjustScionUser, adjustScionUser},
		"runChownTreeRootOwned":     {runChownTreeRootOwned, chownTreeRootOwned},
		"setupHostUserHasCapSetUID": {setupHostUserHasCapSetUID, hasCapSetUID},
		"setupHostUserIsUIDMapped":  {setupHostUserIsUIDMapped, isUIDMapped},
		"setupHostUserGetuid":       {setupHostUserGetuid, os.Getuid},
		"postPreStartGeteuid":       {postPreStartGeteuid, os.Geteuid},
		"runSetupHostUser":          {runSetupHostUser, setupHostUser},
		"runDirectSetUID":           {runDirectSetUID, directSetUID},
	} {
		if ptr(pair[0]) != ptr(pair[1]) {
			t.Errorf("%s default is not the real function", name)
		}
	}
}

// -----------------------------------------------------------------------
// RunInit integration test seams: runGitCloneWorkspace,
// runPostPreStartOwnershipFixup, runServicesStart, runMetadataServerStart
// and runFetchSecretOverrides substituted (see each var's own doc comment
// for why the seam exists instead of a real clone, chown, sidecar process,
// listener socket or hub request) so a test can drive the real RunInit and
// directly observe each step, and the RequirePrivilegeDrop value each one
// receives, without any of the real side effects.
// -----------------------------------------------------------------------

// withRunGitCloneWorkspace temporarily overrides the runGitCloneWorkspace
// package var.
func withRunGitCloneWorkspace(t *testing.T, f func(uid, gid int, agentHome string, requirePrivilegeDrop bool) error) {
	t.Helper()
	orig := runGitCloneWorkspace
	runGitCloneWorkspace = f
	t.Cleanup(func() { runGitCloneWorkspace = orig })
}

// withRunPostPreStartOwnershipFixup temporarily overrides the
// runPostPreStartOwnershipFixup package var.
func withRunPostPreStartOwnershipFixup(t *testing.T, f func(uid, gid int, agentHome string, requirePrivilegeDrop bool)) {
	t.Helper()
	orig := runPostPreStartOwnershipFixup
	runPostPreStartOwnershipFixup = f
	t.Cleanup(func() { runPostPreStartOwnershipFixup = orig })
}

// withRunServicesStart temporarily overrides the runServicesStart package
// var.
func withRunServicesStart(t *testing.T, f func(ctx context.Context, m *services.Manager, specs []api.ServiceSpec, uid, gid int, username string, requirePrivilegeDrop bool) error) {
	t.Helper()
	orig := runServicesStart
	runServicesStart = f
	t.Cleanup(func() { runServicesStart = orig })
}

// withRunMetadataServerStart temporarily overrides the
// runMetadataServerStart package var.
func withRunMetadataServerStart(t *testing.T, f func(ctx context.Context, s *metadata.Server) error) {
	t.Helper()
	orig := runMetadataServerStart
	runMetadataServerStart = f
	t.Cleanup(func() { runMetadataServerStart = orig })
}

// withRunFetchSecretOverrides temporarily overrides the
// runFetchSecretOverrides package var.
func withRunFetchSecretOverrides(t *testing.T, f func(client *hub.Client, keys []string) map[string]string) {
	t.Helper()
	orig := runFetchSecretOverrides
	runFetchSecretOverrides = f
	t.Cleanup(func() { runFetchSecretOverrides = orig })
}

// withStartReaper temporarily overrides the startReaper package var.
func withStartReaper(t *testing.T, f func()) {
	t.Helper()
	orig := startReaper
	startReaper = f
	t.Cleanup(func() { startReaper = orig })
}

// writeServicesYAMLWithInvalidEntry is writeServicesYAML plus one entry
// whose Name is invalid (services.ValidateServiceName rejects any embedded
// path separator) — for the one test that needs to prove the parse-time
// gate actually drops it before anything downstream ever sees it.
func writeServicesYAMLWithInvalidEntry(t *testing.T, agentHome string) {
	t.Helper()
	dir := filepath.Join(agentHome, ".scion")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	specs := []api.ServiceSpec{
		{Name: "order-probe", Command: []string{"true"}},
		{Name: "../escape", Command: []string{"true"}},
	}
	data, err := yaml.Marshal(specs)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-services.yaml"), data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// scionMetadataAndSecretEnvVars lists every SCION_* environment variable
// RunInit reads (directly, or through metadata.ConfigFromEnv) to decide
// whether to start the metadata server, stage secrets, or fetch secrets from
// the Hub. setupRunInitAsRootlessScion unsets each of these outright.
var scionMetadataAndSecretEnvVars = []string{
	"SCION_METADATA_MODE",
	"SCION_METADATA_PORT",
	"SCION_METADATA_BIND_ADDRESS",
	"SCION_METADATA_SA_EMAIL",
	"SCION_METADATA_PROJECT_ID",
	"SCION_NETWORK_MODE",
	"SCION_SECRET_KEYS",
	// SCION_STAGED_SECRETS carries a base64-encoded secrets blob RunInit
	// decodes and writes to agentHome before re-execing itself; it isn't a
	// Hub fetch, but it is a secret-handling input RunInit reads on this
	// path.
	"SCION_STAGED_SECRETS",
}

// setupRunInitAsRootlessScion configures the environment a single RunInit
// call needs to reach past setupHostUser as the same "rootless,
// already the scion user" case an enforced caller runs under in production,
// without requiring the test process to actually be root: scionUserLookup is
// faked to report the test process's own real uid/gid (so setupHostUser's
// "already running as scion user" shortcut applies) with agentHome as its
// home directory, and every env var RunInit reads while preparing the
// workspace is set to a harmless no-op value.
func setupRunInitAsRootlessScion(t *testing.T, agentHome string) {
	t.Helper()
	scrubHubEnv(t)
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_HOST_GID", "")
	t.Setenv("SCION_GIT_CLONE_URL", "")
	// Every var in scionMetadataAndSecretEnvVars is unset outright, not just
	// set to "": os.LookupEnv (metadata.ConfigFromEnv's own check for
	// SCION_METADATA_MODE) treats "present but empty" as a real, if
	// malformed, value — for SCION_METADATA_MODE that still starts a
	// metadata server in the default "block" mode. t.Setenv runs first, so
	// whatever ambient value the container this test binary happens to run
	// in has set (this project's own agent containers set
	// SCION_METADATA_MODE) is restored at cleanup; os.Unsetenv then makes
	// the variable genuinely absent for the test itself. A case that needs
	// either path enabled sets the relevant var itself, after this call.
	for _, k := range scionMetadataAndSecretEnvVars {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	// The telemetry pipeline defaults to enabled and binds a local OTLP
	// receiver on fixed loopback ports (4317/4318). Every RunInit call in
	// this file drives the real telemetry.New(), so leaving it enabled
	// would make these tests bind those ports for real and collide with
	// anything else already listening on them.
	t.Setenv("SCION_TELEMETRY_ENABLED", "false")
	t.Setenv("HOME", agentHome)
	withScionUserLookup(t, func(string) (*user.User, error) {
		return &user.User{Uid: strconv.Itoa(os.Getuid()), Gid: strconv.Itoa(os.Getgid()), HomeDir: agentHome}, nil
	})
}

// setupRunInitAsRootlessScionEnvVarsUnderTest is
// TestSetupRunInitAsRootlessScion_DisablesMetadataServerAndTelemetry's own
// copy of the environment variables setupRunInitAsRootlessScion must unset,
// written out independently of scionMetadataAndSecretEnvVars (rather than
// looping that var itself for both the ambient setenv below and the
// after-the-fact assertion). If an entry were ever dropped from
// scionMetadataAndSecretEnvVars, this test must still set it to an ambient
// value and still assert it is gone, or the two lists drifting apart in
// lockstep would silently stop testing anything.
var setupRunInitAsRootlessScionEnvVarsUnderTest = []string{
	"SCION_METADATA_MODE",
	"SCION_METADATA_PORT",
	"SCION_METADATA_BIND_ADDRESS",
	"SCION_METADATA_SA_EMAIL",
	"SCION_METADATA_PROJECT_ID",
	"SCION_NETWORK_MODE",
	"SCION_SECRET_KEYS",
	"SCION_STAGED_SECRETS",
}

// TestSetupRunInitAsRootlessScion_DisablesMetadataServerAndTelemetry pins
// the hermeticity setupRunInitAsRootlessScion promises every RunInit test in
// this file, independent of the environment the test binary runs in: with
// an agent container's own metadata, secret and telemetry settings present,
// the helper leaves RunInit nothing that would start the metadata server,
// stage or fetch secrets, or start the telemetry pipeline (both server
// paths bind fixed loopback ports).
func TestSetupRunInitAsRootlessScion_DisablesMetadataServerAndTelemetry(t *testing.T) {
	for _, k := range setupRunInitAsRootlessScionEnvVarsUnderTest {
		t.Setenv(k, "ambient")
	}
	t.Setenv("SCION_METADATA_MODE", "assign")
	t.Setenv("SCION_TELEMETRY_ENABLED", "true")

	setupRunInitAsRootlessScion(t, t.TempDir())

	for _, k := range setupRunInitAsRootlessScionEnvVarsUnderTest {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s = %q after setupRunInitAsRootlessScion, want it absent", k, v)
		}
	}
	if cfg := metadata.ConfigFromEnv(); cfg != nil {
		t.Errorf("metadata.ConfigFromEnv() = %+v after setupRunInitAsRootlessScion, want nil (RunInit would start a metadata server)", cfg)
	}
	if telemetry.LoadConfig().Enabled {
		t.Error("telemetry.LoadConfig().Enabled = true after setupRunInitAsRootlessScion, want false (RunInit would bind the OTLP receiver ports)")
	}
}

// TestRunInit_ThreadsRequirePrivilegeDropToEveryGatedCallSite is the core
// regression test: it drives RunInit end to end (with every real
// downstream step stubbed via its own package-var seam) and asserts that
// each of the RequirePrivilegeDrop-gated call sites receives exactly the
// value InitRunOptions.RequirePrivilegeDrop was set to — for both true and
// false. runSetupHostUser is stubbed to return a non-zero uid/gid so a
// non-root test process can get past requirePrivilegeDropOrFail's
// fail-closed check with RequirePrivilegeDrop: true and reach every
// downstream site at all (see its own doc comment).
func TestRunInit_ThreadsRequirePrivilegeDropToEveryGatedCallSite(t *testing.T) {
	for _, requirePrivilegeDrop := range []bool{true, false} {
		t.Run(fmt.Sprintf("RequirePrivilegeDrop=%v", requirePrivilegeDrop), func(t *testing.T) {
			agentHome := t.TempDir()
			setupRunInitAsRootlessScion(t, agentHome)
			// One valid entry plus one invalid entry: proves the parse-time
			// gate (validateServiceSpecs, called right after
			// yaml.Unmarshal, before runServicesStart is ever reached) is
			// what actually filters the invalid entry out — not just
			// Manager.Start's own belt-and-suspenders re-check, which the
			// runServicesStart seam below bypasses entirely.
			writeServicesYAMLWithInvalidEntry(t, agentHome)
			t.Setenv("SCION_METADATA_MODE", "block")

			var gotSetupHostUser *bool
			origSetupHostUser := runSetupHostUser
			runSetupHostUser = func(rpd bool) (int, int, bool) {
				gotSetupHostUser = &rpd
				return os.Getuid(), os.Getgid(), false
			}
			t.Cleanup(func() { runSetupHostUser = origSetupHostUser })

			withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error { return nil })
			withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error { return nil })
			withRunFetchSecretOverrides(t, func(*hub.Client, []string) map[string]string { return nil })

			var gotFixup, gotServices, gotDebug, gotGcloud, gotServicesYAML *bool

			withRunPostPreStartOwnershipFixup(t, func(uid, gid int, home string, rpd bool) {
				gotFixup = &rpd
			})
			var gotSpecs []api.ServiceSpec
			withRunServicesStart(t, func(_ context.Context, _ *services.Manager, specs []api.ServiceSpec, _, _ int, _ string, rpd bool) error {
				gotServices = &rpd
				gotSpecs = specs
				return nil
			})

			origDebug := runBlockClaudeDebugSymlink
			runBlockClaudeDebugSymlink = func(debugDir string, rpd bool) { gotDebug = &rpd }
			t.Cleanup(func() { runBlockClaudeDebugSymlink = origDebug })

			origGcloud := runCleanGcloudConfigForMetadata
			runCleanGcloudConfigForMetadata = func(dir string, rpd bool) { gotGcloud = &rpd }
			t.Cleanup(func() { runCleanGcloudConfigForMetadata = origGcloud })

			origReadYAML := runReadServicesYAML
			runReadServicesYAML = func(path string, rpd bool) ([]byte, error) {
				gotServicesYAML = &rpd
				return origReadYAML(path, rpd)
			}
			t.Cleanup(func() { runReadServicesYAML = origReadYAML })

			opts := InitRunOptions{
				DisableTermSignalForwarding: true,
				RequirePrivilegeDrop:        requirePrivilegeDrop,
			}
			// "claude" as the child's own argv[0] makes isClaude true so the
			// debug-symlink seam actually fires; the exit code is otherwise
			// irrelevant — every seam this test cares about runs well
			// before the real child would ever be exec'd.
			_ = RunInit([]string{"claude", "sh", "-c", "true"}, opts)

			for name, got := range map[string]*bool{
				"runSetupHostUser":                gotSetupHostUser,
				"runPostPreStartOwnershipFixup":   gotFixup,
				"runServicesStart":                gotServices,
				"runBlockClaudeDebugSymlink":      gotDebug,
				"runCleanGcloudConfigForMetadata": gotGcloud,
				"runReadServicesYAML":             gotServicesYAML,
			} {
				if got == nil {
					t.Errorf("%s: seam was never called", name)
					continue
				}
				if *got != requirePrivilegeDrop {
					t.Errorf("%s: requirePrivilegeDrop = %v, want %v", name, *got, requirePrivilegeDrop)
				}
			}

			// The parse-time gate (validateServiceSpecs) must be what
			// filtered the invalid entry: runServicesStart is a seam here,
			// so Manager.Start's own belt-and-suspenders re-check never
			// runs at all in this test — if the specs the seam received
			// still contained the invalid entry, only the parse gate could
			// be the thing missing.
			var gotNames []string
			for _, s := range gotSpecs {
				gotNames = append(gotNames, s.Name)
			}
			if len(gotNames) != 1 || gotNames[0] != "order-probe" {
				t.Errorf("specs reaching runServicesStart = %v, want [order-probe] — the parse-time gate should have dropped the invalid entry before this seam ever saw it", gotNames)
			}
		})
	}
}

// TestRunInit_PinsLifecycleManagerAndTokenOwnerCheckCallSites pins RunInit's
// own call site onto runNewLifecycleManager and runEnforceTokenFileOwnerChecks:
// the former receives agentHome, the exact targetUID/targetGID
// runSetupHostUser returned, and opts.RequirePrivilegeDrop; the latter
// receives runNewLifecycleManager's own second return value, not a value
// re-derived directly from opts.RequirePrivilegeDrop. The stub returns a
// fixed enforceTokenOwnerChecks value that DIFFERS from
// RequirePrivilegeDrop in the false case specifically to prove the second
// claim — if the call site read opts.RequirePrivilegeDrop again instead of
// the stub's return value, this test's false case would see false where it
// expects true.
func TestRunInit_PinsLifecycleManagerAndTokenOwnerCheckCallSites(t *testing.T) {
	for _, requirePrivilegeDrop := range []bool{true, false} {
		t.Run(fmt.Sprintf("RequirePrivilegeDrop=%v", requirePrivilegeDrop), func(t *testing.T) {
			agentHome := t.TempDir()
			setupRunInitAsRootlessScion(t, agentHome)

			// The real process uid/gid, not an arbitrary pair: supervisor.Run's
			// own home-directory chown targets a fixed "/home/scion" path
			// unrelated to agentHome, and only succeeds (as a self-chown,
			// without CAP_CHOWN) when the target uid already owns the path —
			// an arbitrary uid would fail loudly there for reasons unrelated
			// to what this test checks.
			stubUID, stubGID := os.Getuid(), os.Getgid()
			origSetupHostUser := runSetupHostUser
			runSetupHostUser = func(rpd bool) (int, int, bool) {
				return stubUID, stubGID, false
			}
			t.Cleanup(func() { runSetupHostUser = origSetupHostUser })

			withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error { return nil })
			withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error { return nil })
			withRunFetchSecretOverrides(t, func(*hub.Client, []string) map[string]string { return nil })
			withRunPostPreStartOwnershipFixup(t, func(uid, gid int, home string, rpd bool) {})
			withRunServicesStart(t, func(context.Context, *services.Manager, []api.ServiceSpec, int, int, string, bool) error { return nil })

			type lifecycleArgs struct {
				agentHome            string
				targetUID, targetGID int
				requirePrivilegeDrop bool
			}
			var gotLifecycleArgs *lifecycleArgs
			// Deliberately the OPPOSITE of requirePrivilegeDrop: proves the
			// EnforceTokenFileOwnerChecks call site forwards THIS value, not
			// a fresh read of opts.RequirePrivilegeDrop.
			stubEnforceTokenOwnerChecks := !requirePrivilegeDrop
			origNewLifecycleManager := runNewLifecycleManager
			runNewLifecycleManager = func(agentHome string, targetUID, targetGID int, requirePrivilegeDrop bool) (*hooks.LifecycleManager, bool) {
				gotLifecycleArgs = &lifecycleArgs{agentHome, targetUID, targetGID, requirePrivilegeDrop}
				return hooks.NewLifecycleManager(), stubEnforceTokenOwnerChecks
			}
			t.Cleanup(func() { runNewLifecycleManager = origNewLifecycleManager })

			var gotEnforceTokenOwnerChecks *bool
			origEnforceTokenFileOwnerChecks := runEnforceTokenFileOwnerChecks
			runEnforceTokenFileOwnerChecks = func(enabled bool) { gotEnforceTokenOwnerChecks = &enabled }
			t.Cleanup(func() { runEnforceTokenFileOwnerChecks = origEnforceTokenFileOwnerChecks })

			opts := InitRunOptions{
				DisableTermSignalForwarding: true,
				RequirePrivilegeDrop:        requirePrivilegeDrop,
			}
			_ = RunInit([]string{"sh", "-c", "true"}, opts)

			if gotLifecycleArgs == nil {
				t.Fatal("runNewLifecycleManager was never called")
			}
			if gotLifecycleArgs.agentHome != agentHome {
				t.Errorf("agentHome = %q, want %q", gotLifecycleArgs.agentHome, agentHome)
			}
			if gotLifecycleArgs.targetUID != stubUID || gotLifecycleArgs.targetGID != stubGID {
				t.Errorf("targetUID/GID = %d/%d, want %d/%d (runSetupHostUser's own return)", gotLifecycleArgs.targetUID, gotLifecycleArgs.targetGID, stubUID, stubGID)
			}
			if gotLifecycleArgs.requirePrivilegeDrop != requirePrivilegeDrop {
				t.Errorf("requirePrivilegeDrop = %v, want %v", gotLifecycleArgs.requirePrivilegeDrop, requirePrivilegeDrop)
			}

			if gotEnforceTokenOwnerChecks == nil {
				t.Fatal("runEnforceTokenFileOwnerChecks was never called")
			}
			if *gotEnforceTokenOwnerChecks != stubEnforceTokenOwnerChecks {
				t.Errorf("EnforceTokenFileOwnerChecks(%v), want %v (runNewLifecycleManager's own second return value)", *gotEnforceTokenOwnerChecks, stubEnforceTokenOwnerChecks)
			}
		})
	}
}
