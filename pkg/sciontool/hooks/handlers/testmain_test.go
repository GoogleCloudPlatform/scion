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
// Hub client reads when it builds its HTTP transport, beyond the keys
// testutil.HubEnvKeys covers.
var transportEnvKeys = []string{
	transportauth.EnvTransportToken,
	transportauth.EnvTransportTokenFile,
	transportauth.EnvTransportAudience,
	transportauth.EnvTransportTokenExpiry,
	transportauth.EnvHubOIDCAudience,
	transportauth.EnvTransportMode,
	transportauth.EnvMetadataMode,
}

// testMainClearedEnvKeys returns every environment variable TestMain
// clears for the whole test binary, each listed once so that restoring
// them in order cannot undo an earlier restore.
func testMainClearedEnvKeys() []string {
	var keys []string
	keys = append(keys, testutil.HubEnvKeys()...)
	keys = append(keys, agentIdentityEnvKeys...)
	keys = append(keys, transportEnvKeys...)
	slices.Sort(keys)
	return slices.Compact(keys)
}

// sandboxHomeDir is the per-binary temp directory TestMain points HOME and
// the Hub client's token home at.
var sandboxHomeDir string

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
//   - the Hub, agent identity and transport credential environment
//     variables are cleared;
//   - HOME and the Hub client's token home point at a temp directory, so
//     the client never reads the host's token file (the token home is
//     otherwise found through a user lookup, not HOME);
//   - platform detection is stubbed to report "not on GCE", so the client
//     never probes for, or uses, a metadata server.
//
// Tests that need a value still set it with t.Setenv. Everything is
// restored after m.Run, before os.Exit.
func TestMain(m *testing.M) {
	saved := saveAndUnsetEnv(append(testMainClearedEnvKeys(), "HOME"))

	tmpHome, err := os.MkdirTemp("", "handlers-test-home-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: creating sandbox home: %v\n", err)
		os.Exit(1)
	}
	sandboxHomeDir = tmpHome
	_ = os.Setenv("HOME", tmpHome)
	restoreTokenHome := hub.SetTokenHome(tmpHome)
	origIsOnGCE := transportauth.IsOnGCEFunc
	transportauth.IsOnGCEFunc = stubIsOnGCE

	code := m.Run()

	transportauth.IsOnGCEFunc = origIsOnGCE
	restoreTokenHome()
	restoreEnv(saved)
	if err := os.RemoveAll(tmpHome); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: removing sandbox home: %v\n", err)
	}
	os.Exit(code)
}

// The Hub client's token file resolves under the TestMain sandbox, not
// under the host user's home.
func TestHermetic_TokenHomeIsSandboxed(t *testing.T) {
	if sandboxHomeDir == "" {
		t.Fatal("sandboxHomeDir was not set by TestMain")
	}
	got := hub.TokenFilePath()
	if !strings.HasPrefix(got, sandboxHomeDir+string(filepath.Separator)) {
		t.Errorf("hub.TokenFilePath() = %q, want a path under the sandbox home %q", got, sandboxHomeDir)
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

// Every key TestMain clears is unset when a test starts.
func TestHermetic_AmbientEnvCleared(t *testing.T) {
	for _, key := range testMainClearedEnvKeys() {
		if _, set := os.LookupEnv(key); set {
			t.Errorf("%s is set at test start; TestMain must clear it", key)
		}
	}
}

// A handler built only from synthetic test settings sends exactly the
// synthetic agent token and no transport-auth header.
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
		for _, name := range transportHeaders {
			if _, ok := h[http.CanonicalHeaderKey(name)]; ok {
				t.Errorf("request %d carries transport-auth header %s", i, name)
			}
		}
	}
}
