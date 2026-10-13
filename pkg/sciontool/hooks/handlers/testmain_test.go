/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// transportEnvKeys lists the transport credential and platform settings the
// Hub client reads when it builds its HTTP transport. Some of them are also
// in testutil.HubEnvKeys; testMainClearedEnvKeys removes the duplicates.
var transportEnvKeys = []string{
	transportauth.EnvTransportToken,
	transportauth.EnvTransportTokenFile,
	transportauth.EnvTransportAudience,
	transportauth.EnvTransportTokenExpiry,
	transportauth.EnvHubOIDCAudience,
	transportauth.EnvTransportMode,
	transportauth.EnvMetadataMode,
}

// runScopeEnvKeys lists the Hub client inputs, beyond testutil.HubEnvKeys,
// that it sends with every agent-authenticated request.
var runScopeEnvKeys = []string{
	hub.EnvLaunchID,
}

// testMainClearedEnvKeys returns every environment variable TestMain
// clears for the whole test binary, each listed once so that restoring
// them in order cannot undo an earlier restore.
func testMainClearedEnvKeys() []string {
	var keys []string
	keys = append(keys, testutil.HubEnvKeys()...)
	keys = append(keys, agentIdentityEnvKeys...)
	keys = append(keys, transportEnvKeys...)
	keys = append(keys, runScopeEnvKeys...)
	slices.Sort(keys)
	return slices.Compact(keys)
}

// sandboxHomeDir and sandboxTokenHomeDir are the per-binary temp
// directories TestMain points HOME and the Hub client's token home at. They
// are distinct, so a test can tell the token home override apart from the
// client's fallback to HOME.
var (
	sandboxHomeDir      string
	sandboxTokenHomeDir string
)

// stubIsOnGCE replaces transportauth.IsOnGCEFunc for the whole test binary,
// so no test probes the platform for a metadata server.
func stubIsOnGCE() bool { return false }

// savedEnv records whether an environment variable was set, and its value,
// so TestMain can restore it exactly.
type savedEnv struct {
	key   string
	value string
	set   bool
}

func saveAndUnsetEnv(keys []string) []savedEnv {
	saved := make([]savedEnv, 0, len(keys))
	for _, key := range keys {
		value, set := os.LookupEnv(key)
		saved = append(saved, savedEnv{key: key, value: value, set: set})
		_ = os.Unsetenv(key)
	}
	return saved
}

func restoreEnv(saved []savedEnv) {
	for _, s := range saved {
		if s.set {
			_ = os.Setenv(s.key, s.value)
		} else {
			_ = os.Unsetenv(s.key)
		}
	}
}

// TestMain makes this package's tests independent of the machine that runs
// them, for every test in the binary, not only those that call scrubHubEnv:
//
//   - the Hub, agent identity, run id and transport credential
//     environment variables are cleared;
//   - HOME and the Hub client's token home point at two separate temp
//     directories, so the client never reads the host's token file (the
//     token home is otherwise found through a user lookup, not HOME);
//   - platform detection is stubbed to report "not on GCE", so the client
//     never probes for, or uses, a metadata server.
//
// Tests that need a value still set it with t.Setenv. Everything is
// restored after m.Run, before os.Exit.
func TestMain(m *testing.M) {
	saved := saveAndUnsetEnv(append(testMainClearedEnvKeys(), "HOME"))

	sandbox, err := os.MkdirTemp("", "handlers-test-sandbox-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: creating sandbox: %v\n", err)
		os.Exit(1)
	}
	sandboxHomeDir = filepath.Join(sandbox, "home")
	sandboxTokenHomeDir = filepath.Join(sandbox, "token-home")
	for _, dir := range []string{sandboxHomeDir, sandboxTokenHomeDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: creating %s: %v\n", dir, err)
			_ = os.RemoveAll(sandbox)
			os.Exit(1)
		}
	}
	_ = os.Setenv("HOME", sandboxHomeDir)
	restoreTokenHome := hub.SetTokenHome(sandboxTokenHomeDir)
	origIsOnGCE := transportauth.IsOnGCEFunc
	transportauth.IsOnGCEFunc = stubIsOnGCE

	code := m.Run()

	transportauth.IsOnGCEFunc = origIsOnGCE
	restoreTokenHome()
	restoreEnv(saved)
	if err := os.RemoveAll(sandbox); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: removing sandbox: %v\n", err)
	}
	os.Exit(code)
}

// The Hub client's token file resolves under the TestMain token home: not
// under the host user's home, and not under HOME either, so this fails
// without the token home override on any machine.
func TestHermetic_TokenHomeIsSandboxed(t *testing.T) {
	if sandboxHomeDir == "" || sandboxTokenHomeDir == "" {
		t.Fatal("the sandbox directories were not set by TestMain")
	}
	got := hub.TokenFilePath()
	if !strings.HasPrefix(got, sandboxTokenHomeDir+string(filepath.Separator)) {
		t.Errorf("hub.TokenFilePath() = %q, want a path under the sandbox token home %q", got, sandboxTokenHomeDir)
	}
	if strings.HasPrefix(got, sandboxHomeDir+string(filepath.Separator)) {
		t.Errorf("hub.TokenFilePath() = %q is under HOME %q; the token home override is not in effect", got, sandboxHomeDir)
	}
	if home := os.Getenv("HOME"); home != sandboxHomeDir {
		t.Errorf("HOME = %q, want the sandbox home %q", home, sandboxHomeDir)
	}
}

// Platform detection is the TestMain stub, so no test probes for a
// metadata server.
func TestHermetic_PlatformProbeStubbed(t *testing.T) {
	got := reflect.ValueOf(transportauth.IsOnGCEFunc).Pointer()
	want := reflect.ValueOf(stubIsOnGCE).Pointer()
	if got != want {
		t.Fatal("transportauth.IsOnGCEFunc is not the TestMain stub")
	}
	if transportauth.IsOnGCEFunc() {
		t.Error("transportauth.IsOnGCEFunc() = true, want false")
	}
}

// expectedHubClientEnvKeys lists, from the exported constants, every
// environment variable the Hub client reads to find, authenticate to and
// scope requests to a Hub, or to build its transport. It is kept separate
// from the lists TestMain uses, so a key dropped from those lists is
// caught.
var expectedHubClientEnvKeys = []string{
	hub.EnvHubEndpoint,
	hub.EnvHubURL,
	hub.EnvHubToken,
	hub.EnvAgentID,
	hub.EnvAgentMode,
	hub.EnvLaunchID,
	transportauth.EnvTransportToken,
	transportauth.EnvTransportTokenFile,
	transportauth.EnvTransportAudience,
	transportauth.EnvTransportTokenExpiry,
	transportauth.EnvHubOIDCAudience,
	transportauth.EnvTransportMode,
	transportauth.EnvMetadataMode,
}

// Every key TestMain clears, and every Hub client input, is unset when a
// test starts.
func TestHermetic_AmbientEnvCleared(t *testing.T) {
	cleared := testMainClearedEnvKeys()
	for _, key := range expectedHubClientEnvKeys {
		if !slices.Contains(cleared, key) {
			t.Errorf("%s is a Hub client input but TestMain does not clear it", key)
		}
	}
	for _, key := range slices.Concat(cleared, expectedHubClientEnvKeys) {
		if _, set := os.LookupEnv(key); set {
			t.Errorf("%s is set at test start; TestMain must clear it", key)
		}
	}
}

// A handler built only from synthetic test settings sends exactly the
// synthetic agent token, no run id and no transport-auth header.
func TestHermetic_HubRequestUsesOnlySyntheticCredentials(t *testing.T) {
	const syntheticToken = "synthetic-agent-token"

	var mu sync.Mutex
	var headers []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv(hub.EnvHubEndpoint, server.URL)
	t.Setenv(hub.EnvHubToken, syntheticToken)
	t.Setenv(hub.EnvAgentID, "synthetic-agent")

	handler := NewHubHandler()
	if handler == nil {
		t.Fatal("NewHubHandler() = nil, want a handler for the test server")
	}
	if err := handler.Handle(&hooks.Event{Name: hooks.EventPromptSubmit}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(headers) == 0 {
		t.Fatal("the test server received no request")
	}
	transportHeaders := []string{
		transportauth.HeaderAuthorization.HeaderName(),
		transportauth.HeaderProxyAuthorization.HeaderName(),
		transportauth.HeaderServerlessAuthorization.HeaderName(),
	}
	for i, h := range headers {
		if got := h.Get("X-Scion-Agent-Token"); got != syntheticToken {
			t.Errorf("request %d: X-Scion-Agent-Token is not the synthetic token (len %d)", i, len(got))
		}
		if _, ok := h[http.CanonicalHeaderKey(hub.RunIDHeader)]; ok {
			t.Errorf("request %d carries %s", i, hub.RunIDHeader)
		}
		for _, name := range transportHeaders {
			if _, ok := h[http.CanonicalHeaderKey(name)]; ok {
				t.Errorf("request %d carries transport-auth header %s", i, name)
			}
		}
	}
}
