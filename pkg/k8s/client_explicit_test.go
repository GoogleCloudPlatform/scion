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

package k8s

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const explicitTestKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://cluster.example:6443
    certificate-authority: certs/ca.crt
users:
- name: u
  user:
    token: t
contexts:
- name: ctx-a
  context:
    cluster: c
    user: u
- name: ctx-b
  context:
    cluster: c
    user: u
current-context: ctx-a
`

// simulatePod sets the in-cluster environment variables. client-go's
// in-cluster configuration also needs the service-account token file at
// its fixed path, which a test cannot create, so this does NOT make an
// in-cluster fallback possible: the absence of the in-cluster leg is
// proven structurally by TestExplicitFileClientConfig_IsDirectWithNoInClusterLeg.
func simulatePod(t *testing.T) {
	t.Helper()
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
}

func TestNewClientFromKubeconfigFile(t *testing.T) {
	simulatePod(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.kubeconfig")
	if err := os.WriteFile(path, []byte(explicitTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestCA(t, dir)

	c, err := NewClientFromKubeconfigFile(path, "")
	if err != nil {
		t.Fatalf("valid file: %v", err)
	}
	if c.CurrentContext != "ctx-a" {
		t.Errorf("empty context: got %q, want the file's current-context ctx-a", c.CurrentContext)
	}
	if want := filepath.Join(dir, "certs", "ca.crt"); c.Config.CAFile != want {
		t.Errorf("relative CA path: got %q, want %q (resolved against the file's directory)", c.Config.CAFile, want)
	}
	if c.Config.Host != "https://cluster.example:6443" {
		t.Errorf("host = %q", c.Config.Host)
	}

	c, err = NewClientFromKubeconfigFile(path, "ctx-b")
	if err != nil || c.CurrentContext != "ctx-b" {
		t.Fatalf("explicit context: %v %+v", err, c)
	}
	if _, err := NewClientFromKubeconfigFile(path, "no-such-context"); err == nil {
		t.Error("an unknown context must fail")
	}
}

// TestNewClientFromKubeconfigFile_NoFallback: an empty, missing or
// contextless explicit file is an error even where in-cluster credentials
// look available; nothing is loaded from another source.
func TestNewClientFromKubeconfigFile_NoFallback(t *testing.T) {
	simulatePod(t)
	dir := t.TempDir()
	// A valid default source is available too.
	writeTestCA(t, dir)
	defaultPath := filepath.Join(dir, "default.kubeconfig")
	if err := os.WriteFile(defaultPath, []byte(explicitTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", defaultPath)

	empty := filepath.Join(dir, "empty.kubeconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	contextless := filepath.Join(dir, "contextless.kubeconfig")
	noCurrent := explicitTestKubeconfig[:len(explicitTestKubeconfig)-len("current-context: ctx-a\n")]
	if err := os.WriteFile(contextless, []byte(noCurrent), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(dir, "valid.kubeconfig")
	if err := os.WriteFile(valid, []byte(explicitTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ path, context string }{
		"zero-byte file":     {empty, ""},
		"no current-context": {contextless, ""},
		"missing file":       {filepath.Join(dir, "missing.kubeconfig"), ""},
		"unknown context":    {valid, "no-such-context"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := NewClientFromKubeconfigFile(tc.path, tc.context)
			if err == nil {
				t.Fatalf("got a client (context %q, host %q); want an error with no fallback", c.CurrentContext, c.Config.Host)
			}
		})
	}
}

// writeTestCA writes the CA file the test kubeconfig names relatively.
func writeTestCA(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "certs", "ca.crt"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
}

// versionServer is a TLS API server that answers /version only to a
// request carrying "Bearer <token>"; it records the Authorization headers.
func versionServer(t *testing.T, token string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"30","gitVersion":"v1.30.0"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// stubADC replaces the Application Default Credentials token source with a
// usable static token and counts how often it is requested.
func stubADC(t *testing.T, token string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := defaultTokenSource
	t.Cleanup(func() { defaultTokenSource = orig })
	defaultTokenSource = func(context.Context, ...string) (oauth2.TokenSource, error) {
		calls.Add(1)
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}), nil
	}
	return &calls
}

// execKubeconfig writes a kubeconfig for server whose user is the exec
// credential plugin command.
func execKubeconfig(t *testing.T, server, command string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exec.kubeconfig")
	content := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: ` + server + `
    insecure-skip-tls-verify: true
users:
- name: u
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: ` + command + `
      interactiveMode: Never
contexts:
- name: ctx
  context:
    cluster: c
    user: u
current-context: ctx
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestNewClientFromKubeconfigFile_NoADCSubstitution: on GCE, with usable
// Application Default Credentials that the cluster would accept, a failing
// exec plugin of an explicit file makes Verify refuse, and the ADC source is
// never requested.
func TestNewClientFromKubeconfigFile_NoADCSubstitution(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1") // metadata.OnGCE reports true
	srv, seen := versionServer(t, "adc-token")
	adc := stubADC(t, "adc-token")
	c, err := NewClientFromKubeconfigFile(execKubeconfig(t, srv.URL, "/bin/false"), "")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = c.Verify()
	if err == nil {
		t.Fatal("Verify must refuse when the explicit file's credential plugin fails")
	}
	if !strings.Contains(err.Error(), "no other credential source is used") {
		t.Errorf("want the explicit-file refusal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Ensure the credential plugin is installed") {
		t.Errorf("want the operator hint, got: %v", err)
	}
	if n := adc.Load(); n != 0 {
		t.Errorf("the ADC token source was requested %d time(s)", n)
	}
	for _, h := range *seen {
		if h == "Bearer adc-token" {
			t.Error("a request reached the cluster with the ADC token")
		}
	}
}

// TestVerify_DefaultClientExecFailureStillUsesADC: the default/legacy
// client (not an explicit file) keeps its recovery: on GCE a failing exec
// plugin falls back to Application Default Credentials, which the cluster
// accepts.
func TestVerify_DefaultClientExecFailureStillUsesADC(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	t.Setenv("KUBECONFIG", "")
	srv, seen := versionServer(t, "adc-token")
	adc := stubADC(t, "adc-token")
	c, err := NewClientWithContext(execKubeconfig(t, srv.URL, "/bin/false"), "")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := c.Verify(); err != nil {
		t.Fatalf("the default client must recover through ADC: %v", err)
	}
	if n := adc.Load(); n != 1 {
		t.Errorf("ADC token source requested %d time(s), want 1", n)
	}
	found := false
	for _, h := range *seen {
		found = found || h == "Bearer adc-token"
	}
	if !found {
		t.Errorf("the cluster never saw the ADC token: %v", *seen)
	}
}

// TestNewClientFromKubeconfigFile_ExecPluginSucceedsAndRefreshes: the
// explicit file's own exec plugin acquires the credential, and acquires it
// again once it expired; ADC is never requested.
func TestNewClientFromKubeconfigFile_ExecPluginSucceedsAndRefreshes(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	srv, _ := versionServer(t, "plugin-token")
	adc := stubADC(t, "adc-token")
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	script := filepath.Join(dir, "plugin.sh")
	// The credential is already expired when issued, so the next request
	// runs the plugin again (client-go refreshes an expired credential).
	body := "#!/bin/sh\necho run >> '" + counter + "'\n" +
		`echo '{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{"token":"plugin-token","expirationTimestamp":"2000-01-01T00:00:00Z"}}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := NewClientFromKubeconfigFile(execKubeconfig(t, srv.URL, script), "")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := c.Verify(); err != nil {
		t.Fatalf("Verify with the plugin's credential: %v", err)
	}
	if _, err := c.Clientset.Discovery().ServerVersion(); err != nil {
		t.Fatalf("request after the credential expired: %v", err)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if runs := strings.Count(string(data), "run"); runs < 2 {
		t.Errorf("the plugin ran %d time(s), want a refresh (at least 2)", runs)
	}
	if n := adc.Load(); n != 0 {
		t.Errorf("the ADC token source was requested %d time(s)", n)
	}
}

// TestExplicitFileClientConfig_IsDirectWithNoInClusterLeg: the explicit
// file's configuration is a direct client configuration, never the deferred
// loader (whose in-cluster leg can substitute in-cluster credentials).
func TestExplicitFileClientConfig_IsDirectWithNoInClusterLeg(t *testing.T) {
	cfg, err := clientcmd.Load([]byte(explicitTestKubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	cc := explicitFileClientConfig(cfg, "")
	if _, ok := cc.(*clientcmd.DirectClientConfig); !ok {
		t.Fatalf("explicit file config is %T, want *clientcmd.DirectClientConfig", cc)
	}
	if _, ok := cc.(*clientcmd.DeferredLoadingClientConfig); ok {
		t.Fatal("explicit file config is the deferred loader")
	}
}

// TestNewClientFromKubeconfigFile_UsesTheDirectConfigAtTheCallSite: the
// explicit-file constructor builds its configuration through the direct
// (no in-cluster leg) constructor, and the configuration it gets is the
// direct kind; a loader built inline at the call site would not use it.
func TestNewClientFromKubeconfigFile_UsesTheDirectConfigAtTheCallSite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.kubeconfig")
	if err := os.WriteFile(path, []byte(explicitTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestCA(t, dir)
	orig := newExplicitClientConfig
	t.Cleanup(func() { newExplicitClientConfig = orig })
	var built []clientcmd.ClientConfig
	newExplicitClientConfig = func(cfg *clientcmdapi.Config, contextName string) clientcmd.ClientConfig {
		cc := orig(cfg, contextName)
		built = append(built, cc)
		return cc
	}
	if _, err := NewClientFromKubeconfigFile(path, "ctx-b"); err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 {
		t.Fatalf("the direct configuration constructor was used %d time(s), want once", len(built))
	}
	if _, ok := built[0].(*clientcmd.DirectClientConfig); !ok {
		t.Fatalf("configuration is %T, want *clientcmd.DirectClientConfig", built[0])
	}
}
