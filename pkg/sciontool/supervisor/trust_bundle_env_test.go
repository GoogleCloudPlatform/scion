/*
Copyright 2026 The Scion Authors.
*/

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSupervisor_HarnessChildInheritsCABundleEnv proves the harness-child
// leg of the egress_trust_bundle env-propagation path: the harness child
// needs NODE_EXTRA_CA_CERTS (and the other CA-bundle vars) to survive the
// supervisor's privilege-dropped exec. Run's env construction for a
// privilege-dropped child (config.Username set, and
// either UID>0 or Rootless — supervisor.go's Run, ~lines 123-136) starts
// from os.Environ() and only ever touches HOME/USER/LOGNAME (setEnvVar);
// everything else, including the CA-bundle vars buildActorTemplate sets on
// the container, passes through untouched. This test exercises that exact
// code path via Rootless (the actual syscall.Credential drop needs real
// root and isn't exercised by any existing unit test either — see
// TestSupervisor_RootlessEnvVars above), which shares the same env-assembly
// lines as the UID>0 branch: nothing downstream of it branches further on
// whether a real credential drop happened.
func TestSupervisor_HarnessChildInheritsCABundleEnv(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "/run/ate/trust-bundle.pem")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/run/ate/trust-bundle.pem")
	t.Setenv("GIT_SSL_CAINFO", "/run/ate/trust-bundle.pem")
	t.Setenv("CURL_CA_BUNDLE", "/run/ate/trust-bundle.pem")
	t.Setenv("SSL_CERT_DIR", "/run/ate")

	outFile := filepath.Join(t.TempDir(), "env.out")

	config := Config{
		GracePeriod: 5 * time.Second,
		Username:    "scion",
		Rootless:    true, // no real privilege drop; same env-assembly path
	}
	sup := New(config)

	script := `printf '%s|%s|%s|%s|%s' "$SSL_CERT_FILE" "$NODE_EXTRA_CA_CERTS" "$GIT_SSL_CAINFO" "$CURL_CA_BUNDLE" "$SSL_CERT_DIR" > ` + outFile
	exitCode, err := sup.Run(context.Background(), []string{"sh", "-c", script})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0", exitCode)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading child output: %v", err)
	}
	want := "/run/ate/trust-bundle.pem|/run/ate/trust-bundle.pem|/run/ate/trust-bundle.pem|/run/ate/trust-bundle.pem|/run/ate"
	if string(got) != want {
		t.Errorf("harness child env = %q, want %q — the supervisor's HOME/USER/LOGNAME rewrite for a privilege-dropped child must not drop unrelated vars", string(got), want)
	}
}
