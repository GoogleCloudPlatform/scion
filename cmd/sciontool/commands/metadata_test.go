/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/netcap"
)

// TestRunMetadataStatus_SeamDefaults pins the production defaults of the
// three iptables-section seams, so a change to a default fails here rather
// than passing every stubbed test.
func TestRunMetadataStatus_SeamDefaults(t *testing.T) {
	ptr := func(f any) uintptr { return reflect.ValueOf(f).Pointer() }
	if ptr(metadataStatusGetuid) != ptr(os.Getuid) {
		t.Error("metadataStatusGetuid is not os.Getuid")
	}
	if ptr(metadataStatusNetAdminUsable) != ptr(netcap.Usable) {
		t.Error("metadataStatusNetAdminUsable is not netcap.Usable")
	}
	if ptr(metadataStatusIPTablesCheck) != ptr(checkMetadataIPTablesRedirect) {
		t.Error("metadataStatusIPTablesCheck is not checkMetadataIPTablesRedirect")
	}
}

func TestRunMetadataStatus_NoEnv(t *testing.T) {
	// With no SCION_METADATA_MODE set, runMetadataStatus should return 1
	// (not configured) without panicking. The seams are stubbed so a root
	// runner never reaches the real capability probe or iptables.
	withMetadataStatusSeams(t, 0, false, nil, false)
	t.Setenv("SCION_METADATA_MODE", "")
	t.Setenv("GCE_METADATA_HOST", "")
	t.Setenv("GCE_METADATA_ROOT", "")

	code := runMetadataStatus()
	if code != 1 {
		t.Fatalf("expected exit code 1 when metadata not configured, got %d", code)
	}
}

func TestRunMetadataStatus_ConfiguredButNoServer(t *testing.T) {
	// With SCION_METADATA_MODE set but no server running, should report failures.
	// The seams are stubbed so a root runner never reaches the real
	// capability probe or iptables.
	withMetadataStatusSeams(t, 0, false, nil, false)
	t.Setenv("SCION_METADATA_MODE", "assign")
	t.Setenv("SCION_METADATA_PORT", "19999")
	t.Setenv("GCE_METADATA_HOST", "localhost:19999")
	t.Setenv("GCE_METADATA_ROOT", "localhost:19999")

	code := runMetadataStatus()
	if code != 1 {
		t.Fatalf("expected exit code 1 when server not running, got %d", code)
	}
}

// metadataStatusSeams replaces the iptables-section seams for one test and
// counts the calls the root branch makes.
type metadataStatusSeams struct {
	usableCalls   int
	iptablesCalls int
}

func withMetadataStatusSeams(t *testing.T, uid int, usable bool, usableErr error, iptablesOK bool) *metadataStatusSeams {
	t.Helper()
	s := &metadataStatusSeams{}
	origUID, origUsable, origCheck := metadataStatusGetuid, metadataStatusNetAdminUsable, metadataStatusIPTablesCheck
	t.Cleanup(func() {
		metadataStatusGetuid, metadataStatusNetAdminUsable, metadataStatusIPTablesCheck = origUID, origUsable, origCheck
	})
	metadataStatusGetuid = func() int { return uid }
	metadataStatusNetAdminUsable = func() (bool, error) {
		s.usableCalls++
		return usable, usableErr
	}
	metadataStatusIPTablesCheck = func(port string) {
		s.iptablesCalls++
		if iptablesOK {
			fmt.Println("[ OK ] iptables REDIRECT rule present (stub)")
			return
		}
		fmt.Println("[WARN] iptables REDIRECT rule not found (stub)")
	}
	return s
}

// setNoServerEnv points the status check at a port nothing listens on, so
// the connectivity checks fail (exit 1) and the run still reaches the
// iptables section.
func setNoServerEnv(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	t.Setenv("SCION_METADATA_MODE", "assign")
	t.Setenv("SCION_METADATA_PORT", port)
	t.Setenv("GCE_METADATA_HOST", "localhost:"+port)
	t.Setenv("GCE_METADATA_ROOT", "localhost:"+port)
}

// setHealthyServerEnv starts a stub that answers the health and token
// endpoints with 200, so every non-iptables check passes and the exit code
// shows whether the iptables section changed the result.
func setHealthyServerEnv(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)
	t.Setenv("SCION_METADATA_MODE", "assign")
	t.Setenv("SCION_METADATA_PORT", port)
	t.Setenv("GCE_METADATA_HOST", "localhost:"+port)
	t.Setenv("GCE_METADATA_ROOT", "localhost:"+port)
}

const notVerifiable = "[INFO] iptables rule not verifiable"

// T1: root after the capability drop: a neutral INFO, no iptables call.
func TestRunMetadataStatus_CapabilityDropped_NotVerifiable(t *testing.T) {
	setNoServerEnv(t)
	s := withMetadataStatusSeams(t, 0, false, nil, false)
	var code int
	out := captureStdout(t, func() { code = runMetadataStatus() })
	if !strings.Contains(out, notVerifiable) {
		t.Errorf("output lacks %q:\n%s", notVerifiable, out)
	}
	wantLine := "[INFO] iptables rule not verifiable: " + netcap.CapabilityName +
		" is not usable by this process (init drops it once network setup is done)\n"
	if !strings.Contains(out, wantLine) {
		t.Errorf("output lacks the full not-verifiable line %q:\n%s", wantLine, out)
	}
	if strings.Contains(out, "REDIRECT rule not found") {
		t.Errorf("output reports the rule missing after the capability drop:\n%s", out)
	}
	if s.iptablesCalls != 0 {
		t.Errorf("iptables check called %d times, want 0", s.iptablesCalls)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (connectivity fails; the iptables section must not change it)", code)
	}
}

// T1 with a healthy server: the not-verifiable path must leave a passing
// run at exit 0. This is the variant that catches a mutant marking it failed.
func TestRunMetadataStatus_CapabilityDropped_HealthyServerStillPasses(t *testing.T) {
	setHealthyServerEnv(t)
	s := withMetadataStatusSeams(t, 0, false, nil, false)
	var code int
	out := captureStdout(t, func() { code = runMetadataStatus() })
	if !strings.Contains(out, notVerifiable) {
		t.Errorf("output lacks %q:\n%s", notVerifiable, out)
	}
	if s.iptablesCalls != 0 {
		t.Errorf("iptables check called %d times, want 0", s.iptablesCalls)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a not-verifiable rule is not a failure)", code)
	}
}

// T2: root with the capability, rule missing: today's WARN.
func TestRunMetadataStatus_CapabilityUsable_RuleMissingWarns(t *testing.T) {
	setNoServerEnv(t)
	s := withMetadataStatusSeams(t, 0, true, nil, false)
	var code int
	out := captureStdout(t, func() { code = runMetadataStatus() })
	if !strings.Contains(out, "REDIRECT rule not found") {
		t.Errorf("output lacks the rule-missing WARN:\n%s", out)
	}
	if s.iptablesCalls != 1 {
		t.Errorf("iptables check called %d times, want 1", s.iptablesCalls)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// T3: root with the capability, rule present.
func TestRunMetadataStatus_CapabilityUsable_RulePresent(t *testing.T) {
	setNoServerEnv(t)
	s := withMetadataStatusSeams(t, 0, true, nil, true)
	var code int
	out := captureStdout(t, func() { code = runMetadataStatus() })
	if s.iptablesCalls != 1 {
		t.Errorf("iptables check called %d times, want 1", s.iptablesCalls)
	}
	if strings.Contains(out, notVerifiable) {
		t.Errorf("output says not verifiable although the capability is usable:\n%s", out)
	}
	if !strings.Contains(out, "REDIRECT rule present") {
		t.Errorf("output lacks the rule-present line:\n%s", out)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// T4: the capability check itself fails: WARN and skip.
func TestRunMetadataStatus_CapabilityCheckError_WarnsAndSkips(t *testing.T) {
	setNoServerEnv(t)
	s := withMetadataStatusSeams(t, 0, false, errors.New("read thread status"), false)
	var code int
	out := captureStdout(t, func() { code = runMetadataStatus() })
	if !strings.Contains(out, "[WARN] skipping the iptables check: read thread status") {
		t.Errorf("output lacks the capability-check WARN:\n%s", out)
	}
	if s.iptablesCalls != 0 {
		t.Errorf("iptables check called %d times, want 0", s.iptablesCalls)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// T4 with a healthy server: a failed capability check must leave a passing
// run at exit 0.
func TestRunMetadataStatus_CapabilityCheckError_HealthyServerStillPasses(t *testing.T) {
	setHealthyServerEnv(t)
	s := withMetadataStatusSeams(t, 0, false, errors.New("x"), false)
	var code int
	out := captureStdout(t, func() { code = runMetadataStatus() })
	if !strings.Contains(out, "[WARN] skipping the iptables check: x") {
		t.Errorf("output lacks the capability-check WARN:\n%s", out)
	}
	if s.iptablesCalls != 0 {
		t.Errorf("iptables check called %d times, want 0", s.iptablesCalls)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a failed capability check is not a failure)", code)
	}
}

// T2 with a healthy server: a missing rule is a WARN and must leave a passing
// run at exit 0.
func TestRunMetadataStatus_CapabilityUsable_RuleMissing_HealthyServerStillPasses(t *testing.T) {
	setHealthyServerEnv(t)
	s := withMetadataStatusSeams(t, 0, true, nil, false)
	var code int
	out := captureStdout(t, func() { code = runMetadataStatus() })
	if !strings.Contains(out, "REDIRECT rule not found") {
		t.Errorf("output lacks the rule-missing WARN:\n%s", out)
	}
	if s.iptablesCalls != 1 {
		t.Errorf("iptables check called %d times, want 1", s.iptablesCalls)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a missing rule is a warning, not a failure)", code)
	}
}

// T5: non-root: unchanged, and neither seam is consulted.
func TestRunMetadataStatus_NonRoot_SkipsBeforeCapabilityCheck(t *testing.T) {
	setNoServerEnv(t)
	s := withMetadataStatusSeams(t, 1000, true, nil, true)
	var code int
	out := captureStdout(t, func() { code = runMetadataStatus() })
	if !strings.Contains(out, "Not running as root") {
		t.Errorf("output lacks the non-root INFO:\n%s", out)
	}
	if s.usableCalls != 0 || s.iptablesCalls != 0 {
		t.Errorf("usable calls %d, iptables calls %d; want 0 and 0", s.usableCalls, s.iptablesCalls)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
