package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// namespaceEnvKeys are the environment variables defaultKubernetesNamespace
// consults, in precedence order.
var namespaceEnvKeys = []string{"SCION_K8S_NAMESPACE", "POD_NAMESPACE"}

// TestMain pins every namespace source defaultKubernetesNamespace reads to
// hermetic values before any test runs, so the package's tests resolve the
// "default" namespace no matter where `go test` runs. Without this, running
// inside a Kubernetes pod (where kubelet mounts the service-account namespace
// file and POD_NAMESPACE is often set) changes the namespace every
// NewKubernetesRuntime-based test sees.
//
// Tests that exercise the resolution chain itself override these with
// t.Setenv and setServiceAccountNamespacePathForTest.
func TestMain(m *testing.M) {
	os.Exit(runHermetic(m))
}

func runHermetic(m *testing.M) int {
	for _, key := range namespaceEnvKeys {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: unset %s: %v\n", key, err)
			return 1
		}
	}

	dir, err := os.MkdirTemp("", "scion-runtime-ns-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: create temp dir: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// A path that does not exist: the file step of the chain is skipped and
	// resolution falls through to "default".
	serviceAccountNamespacePath = filepath.Join(dir, "namespace-absent")

	return m.Run()
}

// setServiceAccountNamespacePathForTest points the in-cluster namespace file
// source at path for the duration of t.
func setServiceAccountNamespacePathForTest(t *testing.T, path string) {
	t.Helper()
	prev := serviceAccountNamespacePath
	serviceAccountNamespacePath = path
	t.Cleanup(func() { serviceAccountNamespacePath = prev })
}

// TestHermeticNamespaceDefault guards TestMain: with no per-test override,
// both the resolver and a freshly built runtime must see "default".
func TestHermeticNamespaceDefault(t *testing.T) {
	if got := defaultKubernetesNamespace(); got != "default" {
		t.Fatalf("defaultKubernetesNamespace() = %q, want %q (TestMain did not pin namespace sources)", got, "default")
	}
	if got := NewKubernetesRuntime(nil).DefaultNamespace; got != "default" {
		t.Fatalf("NewKubernetesRuntime(nil).DefaultNamespace = %q, want %q", got, "default")
	}
}

// TestDefaultKubernetesNamespace_Precedence asserts the resolution order with
// a fake in-cluster namespace file present: SCION_K8S_NAMESPACE, then
// POD_NAMESPACE, then the namespace file, then "default". Blank or
// whitespace-only values are treated as unset.
func TestDefaultKubernetesNamespace_Precedence(t *testing.T) {
	writeNSFile := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "namespace")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write fake namespace file: %v", err)
		}
		return p
	}

	tests := []struct {
		name     string
		scionNS  string
		podNS    string
		fileBody *string // nil: file absent
		want     string
	}{
		{name: "all sources set: SCION_K8S_NAMESPACE wins", scionNS: "from-scion", podNS: "from-pod", fileBody: strPtr("from-file\n"), want: "from-scion"},
		{name: "POD_NAMESPACE beats namespace file", podNS: "from-pod", fileBody: strPtr("from-file\n"), want: "from-pod"},
		{name: "namespace file used when env unset", fileBody: strPtr("from-file\n"), want: "from-file"},
		{name: "whitespace env falls through to file", scionNS: "  ", podNS: "\t", fileBody: strPtr("  from-file  \n"), want: "from-file"},
		{name: "blank namespace file falls through to default", fileBody: strPtr(" \n"), want: "default"},
		{name: "no sources: default", want: "default"},
		{name: "POD_NAMESPACE used when file absent", podNS: "from-pod", want: "from-pod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SCION_K8S_NAMESPACE", tt.scionNS)
			t.Setenv("POD_NAMESPACE", tt.podNS)
			if tt.fileBody != nil {
				setServiceAccountNamespacePathForTest(t, writeNSFile(t, *tt.fileBody))
			} else {
				setServiceAccountNamespacePathForTest(t, filepath.Join(t.TempDir(), "absent"))
			}

			if got := defaultKubernetesNamespace(); got != tt.want {
				t.Errorf("defaultKubernetesNamespace() = %q, want %q", got, tt.want)
			}
			if got := DefaultKubernetesNamespace(); got != tt.want {
				t.Errorf("DefaultKubernetesNamespace() = %q, want %q", got, tt.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
