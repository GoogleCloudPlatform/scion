/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
	"gopkg.in/yaml.v3"
)

// writeServicesYAML stages a scion-services.yaml with one service spec in
// agentHome, so RunInit's sidecar-start block finds specs to start —
// without a real sidecar process ever running, since the test also
// overrides runServicesStart.
func writeServicesYAML(t *testing.T, agentHome string) {
	t.Helper()
	dir := filepath.Join(agentHome, ".scion")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	specs := []api.ServiceSpec{{Name: "order-probe", Command: []string{"true"}}}
	data, err := yaml.Marshal(specs)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-services.yaml"), data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestRunInit_ResolveWorkingDir_CalledAfterCloneAndOverridesWorkingDir is
// RunInit's ordering regression test for ResolveWorkingDir: it proves RunInit
// calls InitRunOptions.ResolveWorkingDir only after runGitCloneWorkspace and the
// post-pre-start-hook ownership fixup (the two workspace-preparation steps
// InitRunOptions.ResolveWorkingDir's doc comment says it must follow) have
// run, and before the earliest of the harness-adjacent steps that follow it
// (sidecar services, here — the earliest of the three in RunInit's own
// order). It also proves the resolved value — not the static WorkingDir
// field, which this test also sets, to a directory that must never be used —
// is what reaches the harness the real supervisor starts. It launches a real
// child (`sh -c 'pwd > ...'`) rather than asserting on supervisor.Config
// directly, so it also proves the resolved value actually reaches
// exec.Cmd.Dir end to end, the same way TestHarnessSupervisorConfig proves
// the narrower WorkingDir join.
func TestRunInit_ResolveWorkingDir_CalledAfterCloneAndOverridesWorkingDir(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	writeServicesYAML(t, agentHome)

	var order []string
	withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error {
		order = append(order, "clone")
		return nil
	})
	withRunPostPreStartOwnershipFixup(t, func(uid, gid int, home string, requirePrivilegeDrop bool) {
		order = append(order, "fixup")
	})
	withRunServicesStart(t, func(_ context.Context, _ *services.Manager, _ []api.ServiceSpec, _, _ int, _ string, _ bool) error {
		order = append(order, "sidecars")
		return nil
	})

	resolvedDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	outFile := filepath.Join(t.TempDir(), "pwd.out")

	opts := InitRunOptions{
		DisableTermSignalForwarding: true,
		// Must never be used: ResolveWorkingDir is also set, and its result
		// must supersede this field outright — see WorkingDir's own doc
		// comment for the documented precedence this pins.
		WorkingDir: "/this-directory-must-never-be-used",
		ResolveWorkingDir: func() (string, error) {
			order = append(order, "resolve")
			return resolvedDir, nil
		},
	}

	got := RunInit([]string{"sh", "-c", "pwd >" + outFile}, opts)
	if got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}

	wantOrder := []string{"clone", "fixup", "resolve", "sidecars"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("call order = %v, want %v (ResolveWorkingDir must run after the workspace clone step and the post-pre-start-hook ownership fixup, and before sidecar services start)", order, wantOrder)
	}

	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading harness output: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != resolvedDir {
		t.Errorf("harness ran with cwd %q, want %q (ResolveWorkingDir's result, not the static WorkingDir)", got, resolvedDir)
	}
}

// TestRunInit_ResolveWorkingDirError_ReturnsExitCode18AndNeverStartsHarness
// covers InitRunOptions.ResolveWorkingDir's fail-closed contract: an error
// makes RunInit return exitCodeNoUsableHarnessCwd, report the failure the
// same way every other RunInit failure path does (reportInitFailure), and —
// the property that matters most — never start the harness at all, proven
// by a sentinel file the child would have created never appearing.
func TestRunInit_ResolveWorkingDirError_ReturnsExitCode18AndNeverStartsHarness(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)

	var cloneCalled bool
	withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error {
		cloneCalled = true
		return nil
	})

	sentinel := filepath.Join(t.TempDir(), "harness-started")
	resolverErr := errors.New("no usable harness working directory for the harness child")

	opts := InitRunOptions{
		DisableTermSignalForwarding: true,
		ResolveWorkingDir: func() (string, error) {
			if !cloneCalled {
				t.Error("ResolveWorkingDir was called before the workspace clone step")
			}
			return "", resolverErr
		},
	}

	got := RunInit([]string{"sh", "-c", "touch " + sentinel}, opts)

	if got != exitCodeNoUsableHarnessCwd {
		t.Fatalf("RunInit() = %d, want exitCodeNoUsableHarnessCwd (%d)", got, exitCodeNoUsableHarnessCwd)
	}
	if !cloneCalled {
		t.Error("the workspace clone step never ran")
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Error("the harness process ran despite a ResolveWorkingDir error; it must never start")
	}

	raw, readErr := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if readErr != nil {
		t.Fatalf("expected agent-info.json to be written: %v", readErr)
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
	// Contains, not exact-equals: reportInitFailure's callers wrap the
	// underlying error with a fixed, descriptive prefix (every call site
	// does this except the two with their own "no secrets in this error"
	// justification), so the resolver's own message is a substring of the
	// reported one, not the whole of it.
	if !strings.Contains(info.Detail.Message, resolverErr.Error()) {
		t.Errorf("agent-info.json detail.message = %q, want it to contain %q", info.Detail.Message, resolverErr.Error())
	}
}

// testAuthToken and testStagedSecretKey are the credential and secret-key
// name TestRunInit_ResolveWorkingDirError_NeverStartsSidecarsMetadataOrSecretFetch
// stages, so its assertion that neither one crosses into the Hub failure
// report checks the exact values the test itself set rather than a
// separately hand-typed literal.
const (
	testAuthToken       = "test-token"
	testStagedSecretKey = "some-key"
)

// TestRunInit_ResolveWorkingDirError_NeverStartsSidecarsMetadataOrSecretFetch
// covers the other half of the fail-closed contract: not just that the
// harness child never starts (the previous test), but that none of the
// three components RunInit can start on its behalf before building the
// supervisor config — sidecar services, the metadata server, and the hub
// secret fetch — start either. All three preconditions are satisfied here
// (a staged scion-services.yaml, SCION_METADATA_MODE, and a configured hub
// client plus SCION_SECRET_KEYS), so if a regression moved ResolveWorkingDir
// to run after any of them, this test would see that one's seam called
// despite the resolver error. The hub client is pointed at a local
// httptest server rather than a closed port, so the test also pins what
// reportInitFailure's best-effort Hub report actually sends on this path:
// exactly one request, to the agent's status endpoint, carrying the error
// phase and the resolver's own message, and neither the requested secret
// key name nor the auth token value.
func TestRunInit_ResolveWorkingDirError_NeverStartsSidecarsMetadataOrSecretFetch(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	writeServicesYAML(t, agentHome)

	var (
		mu          sync.Mutex
		hubRequests []hubStatusRequest
	)
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		hubRequests = append(hubRequests, hubStatusRequest{path: r.URL.Path, body: body})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hubServer.Close)

	t.Setenv("SCION_METADATA_MODE", "block")
	t.Setenv("SCION_HUB_ENDPOINT", hubServer.URL)
	t.Setenv("SCION_AUTH_TOKEN", testAuthToken)
	t.Setenv("SCION_AGENT_ID", "test-agent")
	t.Setenv("SCION_SECRET_KEYS", testStagedSecretKey)

	withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error { return nil })

	var sidecarsRan, metadataRan, secretsRan bool
	withRunServicesStart(t, func(context.Context, *services.Manager, []api.ServiceSpec, int, int, string, bool) error {
		sidecarsRan = true
		return nil
	})
	withRunMetadataServerStart(t, func(context.Context, *metadata.Server) error {
		metadataRan = true
		return nil
	})
	withRunFetchSecretOverrides(t, func(*hub.Client, []string) map[string]string {
		secretsRan = true
		return nil
	})

	resolverErr := errors.New("no usable harness working directory for the harness child")
	opts := InitRunOptions{
		DisableTermSignalForwarding: true,
		ResolveWorkingDir: func() (string, error) {
			return "", resolverErr
		},
	}

	got := RunInit([]string{"sh", "-c", "true"}, opts)
	if got != exitCodeNoUsableHarnessCwd {
		t.Fatalf("RunInit() = %d, want exitCodeNoUsableHarnessCwd (%d)", got, exitCodeNoUsableHarnessCwd)
	}
	if sidecarsRan {
		t.Error("sidecar services started despite a ResolveWorkingDir error; they must never start")
	}
	if metadataRan {
		t.Error("the metadata server started despite a ResolveWorkingDir error; it must never start")
	}
	if secretsRan {
		t.Error("the hub secret fetch ran despite a ResolveWorkingDir error; it must never run")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hubRequests) != 1 {
		t.Fatalf("hub server received %d request(s), want exactly 1 (the failure report)", len(hubRequests))
	}
	req := hubRequests[0]
	if req.path != "/api/v1/agents/test-agent/status" {
		t.Errorf("hub request path = %q, want the agent status endpoint", req.path)
	}
	var reported struct {
		Phase   string `json:"phase"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(req.body, &reported); err != nil {
		t.Fatalf("unmarshal hub request body %q: %v", req.body, err)
	}
	if reported.Phase != string(state.PhaseError) {
		t.Errorf("hub-reported phase = %q, want %q", reported.Phase, state.PhaseError)
	}
	if !strings.Contains(reported.Message, resolverErr.Error()) {
		t.Errorf("hub-reported message = %q, want it to contain %q", reported.Message, resolverErr.Error())
	}
	if strings.Contains(string(req.body), testStagedSecretKey) {
		t.Error("hub request body names the requested secret key; the failure report must not name the requested secret keys")
	}
	if strings.Contains(string(req.body), testAuthToken) {
		t.Error("hub request body contains the auth token value; the failure report must carry no secrets")
	}
}

// hubStatusRequest is one request recorded by a test's httptest.Server
// standing in for the Hub's agent-status endpoint.
type hubStatusRequest struct {
	path string
	body []byte
}

// TestRunInit_NilResolveWorkingDir_UsesStaticWorkingDirUnchanged pins the
// non-substrate path: with ResolveWorkingDir nil (every caller except
// substrate-serve), RunInit must not call it and must hand the static
// WorkingDir to the harness unchanged.
func TestRunInit_NilResolveWorkingDir_UsesStaticWorkingDirUnchanged(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error { return nil })

	staticDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	outFile := filepath.Join(t.TempDir(), "pwd.out")

	got := RunInit([]string{"sh", "-c", "pwd >" + outFile}, InitRunOptions{WorkingDir: staticDir})
	if got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading harness output: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != staticDir {
		t.Errorf("harness ran with cwd %q, want the static WorkingDir %q", got, staticDir)
	}
}
