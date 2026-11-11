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

package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// observeReExec replaces the reExecWithCleanEnv seam with an observer that
// counts calls and returns nil without exec'ing (so the test binary's own
// process image is never replaced), restoring the original on cleanup. It
// returns the call counter.
func observeReExec(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := reExecWithCleanEnv
	reExecWithCleanEnv = func() error {
		calls.Add(1)
		return nil
	}
	t.Cleanup(func() { reExecWithCleanEnv = orig })
	return &calls
}

// stubRunInitSideEffects makes a real RunInit call hermetic: a non-root
// setupHostUser result (so RequirePrivilegeDrop: true can pass the
// fail-closed check), and no real clone, metadata server, secret fetch,
// ownership fixup or sidecar services.
func stubRunInitSideEffects(t *testing.T) {
	t.Helper()
	origSetupHostUser := runSetupHostUser
	runSetupHostUser = func(bool) (int, int, bool) { return os.Getuid(), os.Getgid(), false }
	t.Cleanup(func() { runSetupHostUser = origSetupHostUser })

	withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error { return nil })
	withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error { return nil })
	withRunFetchSecretOverrides(t, func(*hub.Client, []string) map[string]string { return nil })
	withRunPostPreStartOwnershipFixup(t, func(uid, gid int, home string, rpd bool) {})
	withRunServicesStart(t, func(context.Context, *services.Manager, []api.ServiceSpec, int, int, string, bool) error { return nil })
}

// setTransportTokenEnv puts a bootstrap transport credential in the process
// environment (restored on cleanup, along with the file/expiry vars
// stageTransportToken rewrites) and redirects the transport token file into
// a temp token home. It returns the token and the expected file path.
func setTransportTokenEnv(t *testing.T) (token, wantPath string) {
	t.Helper()
	tokenHome := t.TempDir()
	t.Cleanup(hub.SetTokenHome(tokenHome))
	token = makeDoctorTestJWT(time.Now().Add(time.Hour))
	t.Setenv(transportauth.EnvTransportToken, token)
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	_ = os.Unsetenv(transportauth.EnvTransportTokenFile)
	t.Setenv(transportauth.EnvTransportTokenExpiry, "")
	return token, filepath.Join(tokenHome, ".scion", transportauth.TransportTokenFileName)
}

// assertTransportTokenStaged checks the file staging stageTransportToken
// must still perform whether or not the re-exec runs.
func assertTransportTokenStaged(t *testing.T, token, wantPath string) {
	t.Helper()
	if v, ok := os.LookupEnv(transportauth.EnvTransportToken); ok {
		t.Errorf("%s = %q after RunInit, want it cleared", transportauth.EnvTransportToken, v)
	}
	if got := os.Getenv(transportauth.EnvTransportTokenFile); got != wantPath {
		t.Errorf("%s = %q, want %q", transportauth.EnvTransportTokenFile, got, wantPath)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("transport token file not staged: %v", err)
	}
	if string(data) != token {
		t.Error("transport token file does not hold the bootstrap value")
	}
}

// TestRunInit_DisableReExec_SkipsReExecButStagesTransportToken drives the
// real RunInit with DisableReExec: true and SCION_TRANSPORT_TOKEN set: the
// re-exec seam must not be called, while the transport credential is still
// moved from the environment into its file.
func TestRunInit_DisableReExec_SkipsReExecButStagesTransportToken(t *testing.T) {
	setupRunInitAsRootlessScion(t, t.TempDir())
	stubRunInitSideEffects(t)
	token, wantPath := setTransportTokenEnv(t)
	calls := observeReExec(t)

	_ = RunInit([]string{"sh", "-c", "true"}, InitRunOptions{
		DisableTermSignalForwarding: true,
		RequirePrivilegeDrop:        true,
		DisableReExec:               true,
	})

	if n := calls.Load(); n != 0 {
		t.Errorf("reExecWithCleanEnv called %d time(s) with DisableReExec: true, want 0", n)
	}
	assertTransportTokenStaged(t, token, wantPath)
}

// TestRunInit_ZeroValue_ReExecsAfterClearingTransportToken is the positive
// control for the test above: with the zero-value InitRunOptions
// (DisableReExec: false, as `sciontool init` uses) and a staged transport
// token, RunInit must reach the re-exec seam.
func TestRunInit_ZeroValue_ReExecsAfterClearingTransportToken(t *testing.T) {
	setupRunInitAsRootlessScion(t, t.TempDir())
	stubRunInitSideEffects(t)
	token, wantPath := setTransportTokenEnv(t)
	calls := observeReExec(t)

	_ = RunInit([]string{"sh", "-c", "true"}, InitRunOptions{DisableTermSignalForwarding: true})

	if n := calls.Load(); n != 1 {
		t.Errorf("reExecWithCleanEnv called %d time(s) with DisableReExec: false and a cleared transport token, want 1", n)
	}
	assertTransportTokenStaged(t, token, wantPath)
}

// TestSubstrateServeBootstrap_TransportTokenDoesNotReExecPID1 drives
// substrate-serve's real bootstrap path — newSubstrateServeServer, so the
// real substrateServeInitRunner and substrateServeInitOptions wiring — into
// the real RunInit with SCION_TRANSPORT_TOKEN in the request env. A re-exec
// here would replace substrate-serve's bootstrapped PID 1 with a fresh,
// unbootstrapped process, so the seam must not be called, the server must
// report a running state, and /exec must still accept the control token.
func TestSubstrateServeBootstrap_TransportTokenDoesNotReExecPID1(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	stubRunInitSideEffects(t)
	token, wantPath := setTransportTokenEnv(t)
	calls := observeReExec(t)

	origDeps := defaultPrivilegeDropPreconditionDeps
	t.Cleanup(func() { defaultPrivilegeDropPreconditionDeps = origDeps })
	defaultPrivilegeDropPreconditionDeps = fakePrivilegeDropDeps(t)
	withScionUserLookup(t, func(string) (*user.User, error) {
		return &user.User{Uid: strconv.Itoa(os.Getuid()), Gid: strconv.Itoa(os.Getgid()), HomeDir: agentHome}, nil
	})
	origFixup := bootstrapRootfsFixup
	t.Cleanup(func() { bootstrapRootfsFixup = origFixup })
	bootstrapRootfsFixup = func(string) {}

	// handleBootstrap os.Setenv's every req.Env key; t.Setenv first so
	// cleanup restores them.
	workspace := t.TempDir()
	t.Setenv("SCION_WORKSPACE_PATH", "")

	exits := make(chan int, 1)
	runInit := func(argv []string, opts InitRunOptions) int {
		code := RunInit(argv, opts)
		exits <- code
		return code
	}

	const controlToken = "control-tok"
	srv := newSubstrateServeServer(runInit)
	rec := doSubstrateServeJSON(t, srv, "POST", "/scion/v1/bootstrap", "any-token", map[string]any{
		"env": map[string]string{
			transportauth.EnvTransportToken: token,
			"SCION_WORKSPACE_PATH":          workspace,
		},
		"files":         []any{},
		"start_cmd":     "true",
		"control_token": controlToken,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// RunInit's own exit code is not asserted: on a non-root test runner the
	// supervisor's credential drop (setgroups) fails with EPERM, so the
	// harness child itself cannot start here. Everything this test checks
	// happens before that point or does not depend on it.
	select {
	case <-exits:
	case <-time.After(30 * time.Second):
		t.Fatal("in-process RunInit did not return within 30s")
	}

	if n := calls.Load(); n != 0 {
		t.Errorf("reExecWithCleanEnv called %d time(s) on the substrate-serve bootstrap path, want 0 — it would replace the bootstrapped PID 1", n)
	}
	assertTransportTokenStaged(t, token, wantPath)

	health := doSubstrateServeJSON(t, srv, "GET", "/scion/v1/healthz", "", nil)
	var hr struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(health.Body.Bytes(), &hr); err != nil {
		t.Fatalf("decode healthz: %v (body=%s)", err, health.Body.String())
	}
	// Bootstrapped: "running", or "init-failed" when the harness child could
	// not start (see above) — never back to "awaiting-bootstrap".
	if hr.State != "running" && hr.State != "init-failed" {
		t.Errorf("healthz state = %q, want a bootstrapped state (running or init-failed)", hr.State)
	}

	// An empty argv is rejected with 400 only after the control-token
	// check passes; a wrong token is 401.
	if rec := doSubstrateServeJSON(t, srv, "POST", "/scion/v1/exec", controlToken, map[string]any{"argv": []string{}}); rec.Code != http.StatusBadRequest {
		t.Errorf("/exec with the control token: status = %d, want 400 (authenticated, empty argv); body=%s", rec.Code, rec.Body.String())
	}
	if rec := doSubstrateServeJSON(t, srv, "POST", "/scion/v1/exec", "wrong-token", map[string]any{"argv": []string{}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("/exec with a wrong token: status = %d, want 401", rec.Code)
	}
}
