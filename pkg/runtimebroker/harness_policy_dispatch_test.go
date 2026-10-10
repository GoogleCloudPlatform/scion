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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// countingHCFetcher is a hub harness-config fetcher stub for
// templatecache.NewResolver. Get reports the current bundle's content hash,
// which is always present in the resolver's cache (so Resolve never
// downloads), and counts calls: one per hydration.
type countingHCFetcher struct {
	calls atomic.Int32
	mu    sync.Mutex
	hash  string
	err   error
}

func (f *countingHCFetcher) Get(_ context.Context, _ string) (string, string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", "", f.err
	}
	return "hc-id", f.hash, nil
}

func (f *countingHCFetcher) RequestDownloadURLs(context.Context, string) (*hubclient.DownloadResponse, error) {
	return nil, errors.New("unexpected download: bundle should be served from cache")
}

func (f *countingHCFetcher) DownloadFile(context.Context, string) ([]byte, error) {
	return nil, errors.New("unexpected download")
}

// hubHCStub is a hub connection whose harness-config record can be changed
// between dispatches with setBundle.
type hubHCStub struct {
	t     *testing.T
	cache *templatecache.Cache
	f     *countingHCFetcher
	n     int
}

// setBundle makes configYAML the hub record's current content and returns
// the local path a hydration of it resolves to.
func (h *hubHCStub) setBundle(configYAML string) string {
	h.t.Helper()
	h.n++
	hash := fmt.Sprintf("sha256:hub-bundle-%d", h.n)
	path, err := h.cache.Put(hash, map[string][]byte{"config.yaml": []byte(configYAML)})
	if err != nil {
		h.t.Fatal(err)
	}
	h.f.mu.Lock()
	h.f.hash = hash
	h.f.mu.Unlock()
	return path
}

// attachHubHCStub registers a hub connection with a stub harness-config
// resolver on srv.
func attachHubHCStub(t *testing.T, srv *Server) *hubHCStub {
	t.Helper()
	cache, err := templatecache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	h := &hubHCStub{t: t, cache: cache, f: &countingHCFetcher{}}
	srv.hubMu.Lock()
	srv.hubConnections["test-hub"] = &HubConnection{
		Name:       "test-hub",
		Hydrator:   templatecache.NewHydrator(cache, nil),
		HCResolver: templatecache.NewResolver(cache, h.f, "harness-config"),
	}
	srv.hubMu.Unlock()
	return h
}

// attachHubHC registers a hub connection whose harness-config resolver
// serves configYAML or, when configYAML is empty, fails with fetchErr. It
// returns the fetcher (for call counts) and the cached path.
func attachHubHC(t *testing.T, srv *Server, configYAML string, fetchErr error) (*countingHCFetcher, string) {
	t.Helper()
	h := attachHubHCStub(t, srv)
	var path string
	if configYAML != "" {
		path = h.setBundle(configYAML)
	}
	h.f.err = fetchErr
	return h.f, path
}

// dispatchStampedAgent posts a provision-only create stamped with a hub
// harness-config ID and hash, as the hub does.
func dispatchStampedAgent(t *testing.T, srv *Server, harnessConfig string) (int, string) {
	t.Helper()
	body := `{
		"name": "gate-agent",
		"id": "agent-uuid-gate",
		"slug": "gate-agent",
		"requestId": "req-gate-1",
		"provisionOnly": true,
		"config": {"harnessConfig": "` + harnessConfig + `", "harnessConfigId": "hc-id", "harnessConfigHash": "sha256:dispatch"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// (a) allow=false, stamped dispatch, hub bundle is scripted, nothing on disk.
func TestCreateAgentGate_RefusesHydratedScriptedBundle(t *testing.T) {
	srv, mgr, _ := dispatchTestEnv(t, false)
	attachHubHC(t, srv, scriptedHarnessYAML, nil)

	code, body := dispatchStampedAgent(t, srv, "hub-only")
	if code != http.StatusForbidden || !strings.Contains(body, ErrCodeForbidden) {
		t.Fatalf("expected 403 %s for a hub-hydrated scripted bundle, got %d: %s", ErrCodeForbidden, code, body)
	}
	if mgr.provisionCalled {
		t.Error("Provision must not run when the gate refuses")
	}
}

// (b) Same, but a declarative broker-local copy of the same name exists: the
// gate evaluates the hydrated hub bundle (ptone/scion#621).
func TestCreateAgentGate_EvaluatesHydratedBundleOverLocalCopy(t *testing.T) {
	srv, mgr, dotScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, dotScion, "shadowed", "harness: claude\nimage: scion-claude:test\n")
	attachHubHC(t, srv, scriptedHarnessYAML, nil)

	code, body := dispatchStampedAgent(t, srv, "shadowed")
	if code != http.StatusForbidden {
		t.Fatalf("gate did not evaluate the hydrated scripted bundle: got %d: %s", code, body)
	}
	if mgr.provisionCalled {
		t.Error("Provision must not run when the gate refuses")
	}
}

// (c) allow=false and hydration fails: the gate answers exactly as launch
// does for the same failure (status and body), and the dispatch attempt is
// recorded as failed with the hydration message.
func TestCreateAgentGate_HydrationFailureMapsLikeLaunch(t *testing.T) {
	// Reference: allow=true skips the gate, so launch (buildStartContext)
	// hits the failure.
	refSrv, _, _ := dispatchTestEnv(t, true)
	attachHubHC(t, refSrv, "", errors.New("record vanished"))
	wantCode, wantBody := dispatchStampedAgent(t, refSrv, "hub-only")
	if wantCode != http.StatusInternalServerError {
		t.Fatalf("reference launch failure: expected 500, got %d: %s", wantCode, wantBody)
	}

	srv, mgr, _ := dispatchTestEnv(t, false)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))
	f, _ := attachHubHC(t, srv, "", errors.New("record vanished"))
	code, body := dispatchStampedAgent(t, srv, "hub-only")
	// The refusal comes from the gate, ahead of buildStartContext: the
	// dispatch never reaches the end of pre-flight.
	if strings.Contains(logBuf.String(), "Agent dispatch: pre-flight complete") {
		t.Error("hydration failure was reported after pre-flight completed, not by the gate")
	}
	if code != wantCode || body != wantBody {
		t.Fatalf("gate hydration failure mapped differently from launch:\n gate:   %d %s\n launch: %d %s", code, body, wantCode, wantBody)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("expected exactly one hydration attempt (gate only), got %d", got)
	}
	if mgr.provisionCalled {
		t.Error("Provision must not run after a gate hydration failure")
	}
	srv.dispatchAttemptsMu.Lock()
	defer srv.dispatchAttemptsMu.Unlock()
	var failed bool
	for _, a := range srv.dispatchAttempts {
		if a.Status == dispatchAttemptFailed && a.HTTPStatus == http.StatusInternalServerError &&
			strings.Contains(a.Error, "Failed to hydrate harness-config") {
			failed = true
		}
	}
	if !failed {
		t.Error("dispatch attempt was not marked failed with the hydration message")
	}
}

// Launch provisions exactly the bundle the gate evaluated: one hydration per
// create, and Provision receives the gate's path.
func TestCreateAgentGate_LaunchReusesGateHydratedBundle(t *testing.T) {
	srv, mgr, _ := dispatchTestEnv(t, false)
	f, cachedPath := attachHubHC(t, srv, "harness: claude\nimage: scion-claude:test\n", nil)

	code, body := dispatchStampedAgent(t, srv, "hub-only")
	if code != http.StatusCreated {
		t.Fatalf("declarative hub bundle should be accepted, got %d: %s", code, body)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("expected one hydration shared by gate and launch, got %d", got)
	}
	if mgr.lastOpts.HarnessConfigPath != cachedPath {
		t.Errorf("launch HarnessConfigPath = %q, want the gate-hydrated %q", mgr.lastOpts.HarnessConfigPath, cachedPath)
	}
	// The prehydrated path carries the dispatch's harness-config ID with the
	// path (currentHarnessConfigIdentity needs it for a hub-hydrated config).
	if mgr.lastOpts.HarnessConfigID != "hc-id" {
		t.Errorf("launch HarnessConfigID = %q, want hc-id", mgr.lastOpts.HarnessConfigID)
	}
}

// (d) allow=true: the gate does not hydrate; only launch does (one call).
func TestCreateAgentGate_AllowTrueDoesNotHydrateEarly(t *testing.T) {
	srv, mgr, _ := dispatchTestEnv(t, true)
	f, cachedPath := attachHubHC(t, srv, scriptedHarnessYAML, nil)

	code, body := dispatchStampedAgent(t, srv, "hub-only")
	if code != http.StatusCreated {
		t.Fatalf("allow=true should admit the scripted bundle, got %d: %s", code, body)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("expected only launch's hydration with allow=true, got %d", got)
	}
	if mgr.lastOpts.HarnessConfigPath != cachedPath {
		t.Errorf("launch HarnessConfigPath = %q, want %q", mgr.lastOpts.HarnessConfigPath, cachedPath)
	}
	if mgr.lastOpts.HarnessConfigID != "hc-id" {
		t.Errorf("launch HarnessConfigID = %q, want hc-id", mgr.lastOpts.HarnessConfigID)
	}
}

// The gate fails closed when the hydrated copy cannot be loaded.
func TestLookupHarnessConfigForPolicy_UnloadableHydratedFailsClosed(t *testing.T) {
	srv, _, dotScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, dotScion, "hc", "harness: claude\nimage: scion-claude:test\n")

	req := CreateAgentRequest{Config: &CreateAgentConfig{HarnessConfig: "hc"}}
	_, _, _, ok, err := srv.lookupHarnessConfigDirForPolicy(req, "", filepath.Join(t.TempDir(), "missing"))
	if err == nil || ok {
		t.Fatalf("expected an error and no fallback to the broker-local copy, got ok=%v err=%v", ok, err)
	}
}

// TestEvaluateHarnessConfigPolicy_IgnoresOverrideMutableFields guards the
// contract documented on evaluateHarnessConfigPolicy: the gate sees the entry
// before the profile harness_overrides merge, so its decision must not depend
// on any field an override can change. The field list is derived from
// config.V1HarnessOverride (plus profile volumes, which merge into Volumes),
// so a new override field is covered automatically. If this fails, evaluate
// that field after applying the override merge, then update this test.
func TestEvaluateHarnessConfigPolicy_IgnoresOverrideMutableFields(t *testing.T) {
	entryType := reflect.TypeOf(config.HarnessConfigEntry{})
	overrideType := reflect.TypeOf(config.V1HarnessOverride{})
	// Override fields that are not merged into HarnessConfigEntry at all,
	// so the gate's entry cannot carry them. Any other unmatched field
	// fails the test so it is classified deliberately.
	notOnEntry := map[string]string{
		"Resources": "applied to the agent's resource spec, not merged into HarnessConfigEntry",
	}
	var fields []string
	for i := 0; i < overrideType.NumField(); i++ {
		name := overrideType.Field(i).Name
		if _, ok := entryType.FieldByName(name); !ok {
			if _, known := notOnEntry[name]; known {
				continue
			}
			t.Fatalf("override field %s has no HarnessConfigEntry counterpart; classify it in this guard", name)
		}
		fields = append(fields, name)
	}
	if len(fields) == 0 {
		t.Fatal("no override-mutable fields found; guard is vacuous")
	}

	for _, allow := range []bool{true, false} {
		srv := &Server{config: ServerConfig{AllowContainerScriptHarnesses: allow}}
		for _, prov := range []*config.HarnessProvisionerConfig{nil, {Type: "container-script"}} {
			base := config.HarnessConfigEntry{Provisioner: prov}
			want := srv.evaluateHarnessConfigPolicy("hc", base)
			for _, name := range fields {
				mutated := base
				fv := reflect.ValueOf(&mutated).Elem().FieldByName(name)
				setNonZero(t, fv, name)
				if got := srv.evaluateHarnessConfigPolicy("hc", mutated); got != want {
					t.Errorf("allow=%v provisioner=%v: decision changed when override-mutable field %s was set (%+v vs %+v); evaluate it after the harness_overrides merge", allow, prov != nil, name, got, want)
				}
			}
		}
	}
}

func setNonZero(t *testing.T, v reflect.Value, name string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("override-" + name)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem())
		v.Set(m)
	case reflect.Slice:
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), reflect.New(v.Type().Elem()).Elem()))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	default:
		t.Fatalf("setNonZero: unsupported kind %s for %s; extend this guard", v.Kind(), name)
	}
}

const declarativeHarnessYAML = "harness: claude\nimage: scion-claude:test\n"

// postAgentAction posts a start or restart for agent id with body.
func postAgentAction(t *testing.T, srv *Server, id, action, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+id+"/"+action, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

const stampedStartBody = `{"harnessConfig": "hub-only", "harnessConfigId": "hc-id", "harnessConfigHash": "sha256:dispatch"}`

// assertPolicyRefusal checks the refusal create, start and restart share:
// 403, ErrCodeForbidden, naming the harness-config and the broker setting.
func assertPolicyRefusal(t *testing.T, code int, body, hcName string) {
	t.Helper()
	if code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", code, body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not an ErrorResponse envelope: %v: %s", err, body)
	}
	if resp.Error.Code != ErrCodeForbidden {
		t.Errorf("error code = %q, want %q", resp.Error.Code, ErrCodeForbidden)
	}
	if !strings.Contains(resp.Error.Message, "allow_container_script_harnesses=true") || !strings.Contains(resp.Error.Message, hcName) {
		t.Errorf("refusal message should name %q and the broker setting, got %q", hcName, resp.Error.Message)
	}
}

func TestStartAgentGate_HydratedConfig(t *testing.T) {
	t.Run("declarative passes with allow=false", func(t *testing.T) {
		srv, mgr, _ := dispatchTestEnv(t, false)
		h := attachHubHCStub(t, srv)
		path := h.setBundle(declarativeHarnessYAML)
		code, body := postAgentAction(t, srv, "gate-agent", "start", stampedStartBody)
		if code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d: %s", code, body)
		}
		if got := mgr.LastStartOpts().HarnessConfigPath; got != path {
			t.Errorf("start HarnessConfigPath = %q, want %q", got, path)
		}
		if got := mgr.LastStartOpts().HarnessConfigID; got != "hc-id" {
			t.Errorf("start HarnessConfigID = %q, want hc-id", got)
		}
		if got := h.f.calls.Load(); got != 1 {
			t.Errorf("expected one hydration per start, got %d", got)
		}
	})
	t.Run("container-script refused with allow=false", func(t *testing.T) {
		srv, mgr, _ := dispatchTestEnv(t, false)
		attachHubHCStub(t, srv).setBundle(scriptedHarnessYAML)
		code, body := postAgentAction(t, srv, "gate-agent", "start", stampedStartBody)
		assertPolicyRefusal(t, code, body, "hub-only")
		if mgr.StartCalls() != 0 {
			t.Error("Start must not run when the gate refuses")
		}
	})
	t.Run("container-script passes with allow=true", func(t *testing.T) {
		srv, mgr, _ := dispatchTestEnv(t, true)
		attachHubHCStub(t, srv).setBundle(scriptedHarnessYAML)
		code, body := postAgentAction(t, srv, "gate-agent", "start", stampedStartBody)
		if code != http.StatusAccepted || mgr.StartCalls() != 1 {
			t.Fatalf("expected 202 and one Start, got %d (starts=%d): %s", code, mgr.StartCalls(), body)
		}
	})
}

// The start gate evaluates the hub record's current content as hydrated for
// this start, which is what launch provisions.
func TestStartAgentGate_EvaluatesCurrentHydratedConfig(t *testing.T) {
	srv, mgr, _ := dispatchTestEnv(t, false)
	h := attachHubHCStub(t, srv)
	h.setBundle(declarativeHarnessYAML)
	if code, body := dispatchStampedAgent(t, srv, "hub-only"); code != http.StatusCreated {
		t.Fatalf("create with a declarative bundle: expected 201, got %d: %s", code, body)
	}

	h.setBundle(scriptedHarnessYAML)
	code, body := postAgentAction(t, srv, "gate-agent", "start", stampedStartBody)
	assertPolicyRefusal(t, code, body, "hub-only")
	if mgr.StartCalls() != 0 {
		t.Error("Start must not run when the gate refuses")
	}
}

// restartGateEnv prepares an existing agent "rs-agent" whose container lists
// projectScion as its project, with saved harness-config "rhc" on disk.
// Restart carries no harness-config reference, so launch resolves the saved
// name from the broker's disk; that is what the gate evaluates.
func restartGateEnv(t *testing.T, allow bool, hcYAML string) (*Server, *provisionCapturingManager, string) {
	t.Helper()
	srv, mgr, _ := dispatchTestEnv(t, allow)
	projectScion := filepath.Join(t.TempDir(), ".scion")
	writeHarnessConfig(t, projectScion, "rhc", hcYAML)
	home := config.GetAgentHomePath(projectScion, "rs-agent")
	writeHarnessConfigDirAt(t, home, "harness: claude\n") // creates home; config.yaml unused
	info, _ := json.Marshal(api.AgentInfo{Name: "rs-agent", HarnessConfig: "rhc"})
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), info, 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.agents = []api.AgentInfo{{Name: "rs-agent", ContainerID: "c-rs", ProjectPath: projectScion, Phase: "stopped"}}
	return srv, mgr, projectScion
}

func TestRestartAgentGate_SavedConfig(t *testing.T) {
	t.Run("declarative passes with allow=false", func(t *testing.T) {
		srv, mgr, _ := restartGateEnv(t, false, declarativeHarnessYAML)
		code, body := postAgentAction(t, srv, "rs-agent", "restart", `{}`)
		if code != http.StatusAccepted || mgr.StartCalls() != 1 {
			t.Fatalf("expected 202 and one Start, got %d (starts=%d): %s", code, mgr.StartCalls(), body)
		}
	})
	t.Run("container-script refused with allow=false", func(t *testing.T) {
		srv, mgr, _ := restartGateEnv(t, false, scriptedHarnessYAML)
		code, body := postAgentAction(t, srv, "rs-agent", "restart", `{}`)
		assertPolicyRefusal(t, code, body, "rhc")
		if mgr.StopCalls() != 0 || mgr.StartCalls() != 0 {
			t.Errorf("restart refusal must precede stop and start (stops=%d starts=%d)", mgr.StopCalls(), mgr.StartCalls())
		}
	})
	t.Run("container-script passes with allow=true", func(t *testing.T) {
		srv, mgr, _ := restartGateEnv(t, true, scriptedHarnessYAML)
		code, body := postAgentAction(t, srv, "rs-agent", "restart", `{}`)
		if code != http.StatusAccepted || mgr.StartCalls() != 1 {
			t.Fatalf("expected 202 and one Start, got %d (starts=%d): %s", code, mgr.StartCalls(), body)
		}
	})
}

// The restart gate evaluates the harness-config as it resolves at restart
// time, which is what launch provisions.
func TestRestartAgentGate_EvaluatesCurrentConfig(t *testing.T) {
	srv, mgr, projectScion := restartGateEnv(t, false, declarativeHarnessYAML)
	if code, body := postAgentAction(t, srv, "rs-agent", "restart", `{}`); code != http.StatusAccepted {
		t.Fatalf("first restart: expected 202, got %d: %s", code, body)
	}
	// The mock's Start appends a second entry for the agent; keep the one
	// container a real runtime would list after the restart.
	mgr.mu.Lock()
	mgr.agents = mgr.agents[:1]
	mgr.mu.Unlock()
	writeHarnessConfig(t, projectScion, "rhc", scriptedHarnessYAML)
	stopsBefore, startsBefore := mgr.StopCalls(), mgr.StartCalls()
	code, body := postAgentAction(t, srv, "rs-agent", "restart", `{}`)
	assertPolicyRefusal(t, code, body, "rhc")
	if mgr.StopCalls() != stopsBefore || mgr.StartCalls() != startsBefore {
		t.Error("restart refusal must precede stop and start")
	}
}

// postUnstampedCreate posts a provision-only create naming harness-config
// hcName with the given project path ("" for none).
func postUnstampedCreate(t *testing.T, srv *Server, projectPath, hcName string) (int, string) {
	t.Helper()
	body := `{"name": "loc-agent", "id": "agent-uuid-loc", "slug": "loc-agent", "provisionOnly": true,
		"projectPath": "` + projectPath + `", "config": {"harnessConfig": "` + hcName + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// Harness-config resolution uses a single resolved project dir
// (config.GetResolvedProjectDir) for provisioning, launch and the policy
// gate. Each case puts a container-script harness-config in the location
// that dir resolves to and a declarative copy of the same name in the global
// directory; with allow=false the unstamped create is refused.
func TestCreateAgentGate_EvaluatesResolvedProjectDir(t *testing.T) {
	t.Run("project root resolves to <root>/.scion", func(t *testing.T) {
		srv, mgr, globalScion := dispatchTestEnv(t, false)
		writeHarnessConfig(t, globalScion, "loc-hc", declarativeHarnessYAML)
		root := t.TempDir()
		writeHarnessConfig(t, filepath.Join(root, ".scion"), "loc-hc", scriptedHarnessYAML)

		code, body := postUnstampedCreate(t, srv, root, "loc-hc")
		assertPolicyRefusal(t, code, body, "loc-hc")
		if mgr.provisionCalled {
			t.Error("Provision must not run when the gate refuses")
		}
	})

	t.Run("no project path walks up to the enclosing project", func(t *testing.T) {
		srv, mgr, globalScion := dispatchTestEnv(t, false)
		writeHarnessConfig(t, globalScion, "loc-hc", declarativeHarnessYAML)
		root := t.TempDir()
		writeHarnessConfig(t, filepath.Join(root, ".scion"), "loc-hc", scriptedHarnessYAML)
		below := filepath.Join(root, "src", "pkg")
		if err := os.MkdirAll(below, 0o755); err != nil {
			t.Fatal(err)
		}
		// dispatchTestEnv restores the original working directory.
		if err := os.Chdir(below); err != nil {
			t.Fatal(err)
		}

		code, body := postUnstampedCreate(t, srv, "", "loc-hc")
		assertPolicyRefusal(t, code, body, "loc-hc")
		if mgr.provisionCalled {
			t.Error("Provision must not run when the gate refuses")
		}
	})

	t.Run("split storage resolves to the external config dir", func(t *testing.T) {
		srv, mgr, globalScion := dispatchTestEnv(t, false)
		writeHarnessConfig(t, globalScion, "loc-hc", declarativeHarnessYAML)
		root := t.TempDir()
		marker := &config.ProjectMarker{ProjectID: "0f8e2c1a-split-test", ProjectName: "split-proj", ProjectSlug: "split-proj"}
		if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), marker); err != nil {
			t.Fatal(err)
		}
		external, err := marker.ExternalProjectPath()
		if err != nil {
			t.Fatal(err)
		}
		if resolved, err := config.GetResolvedProjectDir(root); err != nil || resolved != external {
			t.Fatalf("fixture: root should resolve to the external dir %q, got %q (%v)", external, resolved, err)
		}
		writeHarnessConfig(t, external, "loc-hc", scriptedHarnessYAML)

		code, body := postUnstampedCreate(t, srv, root, "loc-hc")
		assertPolicyRefusal(t, code, body, "loc-hc")
		if mgr.provisionCalled {
			t.Error("Provision must not run when the gate refuses")
		}
	})

	t.Run("declarative in the resolved dir passes", func(t *testing.T) {
		srv, mgr, globalScion := dispatchTestEnv(t, false)
		writeHarnessConfig(t, globalScion, "loc-hc", scriptedHarnessYAML) // outranked by the project copy
		root := t.TempDir()
		writeHarnessConfig(t, filepath.Join(root, ".scion"), "loc-hc", declarativeHarnessYAML)

		code, body := postUnstampedCreate(t, srv, root, "loc-hc")
		if code != http.StatusCreated || !mgr.provisionCalled {
			t.Fatalf("expected 201 for a declarative entry in the resolved project dir, got %d: %s", code, body)
		}
	})
}

// Start uses the same resolved project dir.
func TestStartAgentGate_EvaluatesResolvedProjectDir(t *testing.T) {
	srv, mgr, globalScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, globalScion, "loc-hc", declarativeHarnessYAML)
	root := t.TempDir()
	writeHarnessConfig(t, filepath.Join(root, ".scion"), "loc-hc", scriptedHarnessYAML)
	code, body := postAgentAction(t, srv, "loc-agent", "start", `{"projectPath": "`+root+`", "harnessConfig": "loc-hc"}`)
	assertPolicyRefusal(t, code, body, "loc-hc")
	if mgr.StartCalls() != 0 {
		t.Error("Start must not run when the gate refuses")
	}
}

// enforceHarnessConfigPolicy's fail-closed mapping for an unloadable
// hydrated copy: 500 runtime_error with no broker path in the response
// when the policy can refuse; OK when it cannot.
func TestEnforceHarnessConfigPolicy_UnloadableHydratedCopy(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%v", allow), func(t *testing.T) {
			srv, _, _ := dispatchTestEnv(t, allow)
			missing := filepath.Join(t.TempDir(), "missing-hc")
			d := srv.enforceHarnessConfigPolicy(harnessPolicyInput{
				Req:            CreateAgentRequest{Config: &CreateAgentConfig{HarnessConfig: "hc"}},
				HydratedHCPath: missing,
			})
			if allow {
				if !d.OK {
					t.Fatalf("allow=true must pass, got %+v", d)
				}
				return
			}
			if d.OK || d.HTTPStatus != http.StatusInternalServerError || d.Code != ErrCodeRuntimeError {
				t.Fatalf("expected 500 %s refusal, got %+v", ErrCodeRuntimeError, d)
			}
			if strings.Contains(d.Message, missing) || strings.Contains(d.Message, "/") {
				t.Errorf("response message must not carry a broker path: %q", d.Message)
			}
			if !strings.Contains(d.detail(), missing) {
				t.Errorf("detail should carry the path for logs, got %q", d.detail())
			}
		})
	}
}
