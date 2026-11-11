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

package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TestKubernetesRuntime_ExecWithStdin_SecretNotInArgv covers:
// the Kubernetes backend's ExecWithStdin must deliver its payload only
// through the post-upgrade exec stream, never folded into the pod exec
// request's "command" query parameter (K8s's equivalent of argv — there is
// no local child process/argv at all, since the exec runs inside the pod via
// the API server's exec subresource).
//
// Driving a real failing exec *stream* through remotecommand.NewSPDYExecutor
// needs a server speaking the Kubernetes exec subprotocol (SPDY upgrade),
// which a fake clientset's reactor framework does not provide (see
// execWithOptionalStdin's own doc comment on why its call site isn't
// separately covered that way). This test takes a narrower, real path
// instead: it points a genuine *rest.Config at an httptest.Server that
// captures the exec request synchronously and then fails the upgrade
// (non-101 response), so NewSPDYExecutor's Stream call errors out quickly
// without needing to implement the SPDY protocol. The captured request is
// exactly what would have been sent to a real API server up to the point of
// upgrade, which is sufficient to prove the secret never appears in it: the
// secret is only ever handed to the stream via the stdin reader, after a
// successful upgrade this test deliberately prevents.
func TestKubernetesRuntime_ExecWithStdin_SecretNotInArgv(t *testing.T) {
	const secret = "S3CR3T-K8S-EXEC-NOT-IN-COMMAND-QUERY"

	var (
		gotRequest  bool
		gotURL      string
		gotBody     []byte
		gotMethod   string
		gotCommands []string
		gotStdin    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest = true
		gotMethod = r.Method
		gotURL = r.URL.String()
		gotCommands = r.URL.Query()["command"]
		gotStdin = r.URL.Query().Get("stdin")
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			gotBody = b
		}
		// Fail the upgrade fast (no 101 Switching Protocols): we only need
		// the pre-upgrade request this handler already captured above.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("fake API server: exec subprotocol not implemented"))
	}))
	defer srv.Close()

	cfg := &rest.Config{Host: srv.URL}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to build clientset against fake server: %v", err)
	}

	rt := &KubernetesRuntime{
		Client: &k8s.Client{Clientset: clientset, Config: cfg},
	}

	// "namespace/pod" form bypasses resolveNamespace's own API lookups,
	// isolating this test to the exec request itself. cmd deliberately never
	// contains the secret — exactly like SendKeys's real tmux invocation —
	// so this test is a regression guard against a future change that folded
	// stdin content into cmd/option.Command instead of the stream.
	cmd := []string{"tmux", "send-keys", "-t", "main", "Enter"}
	_, err = rt.ExecWithStdin(context.Background(), "default/test-pod", cmd, strings.NewReader(secret))
	if err == nil {
		t.Fatal("expected an error: the fake server does not speak the exec subprotocol and never upgrades")
	}

	// Positive control: the fake server must have actually been reached,
	// otherwise this test would pass vacuously (e.g. if NewSPDYExecutor
	// failed before ever dialing out).
	if !gotRequest {
		t.Fatal("expected the fake API server to receive the exec request; it never did, so this test proves nothing")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	// Assert this directly rather than merely inferring "stdin plumbed" from the
	// call succeeding. PodExecOptions.Stdin is
	// encoded as this query parameter (VersionedParams); it must be true
	// since ExecWithStdin passed a non-nil reader.
	if gotStdin != "true" {
		t.Errorf("stdin query parameter = %q, want %q (PodExecOptions.Stdin must be set when a stdin reader is supplied)", gotStdin, "true")
	}

	if strings.Contains(gotURL, secret) {
		t.Errorf("secret leaked into the k8s exec request URL: %q", gotURL)
	}
	for _, c := range gotCommands {
		if strings.Contains(c, secret) {
			t.Errorf("secret leaked into the k8s exec request's command query parameter: %q", c)
		}
	}
	if strings.Contains(string(gotBody), secret) {
		t.Errorf("secret leaked into the k8s exec request body: %q", string(gotBody))
	}
}
