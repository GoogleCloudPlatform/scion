package runtime

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

var updatePodGolden = flag.Bool("update-pod-golden", false, "rewrite testdata/k8s_pod_golden from the current buildPod output")

// podGoldenCases are representative run configs whose pod specs are pinned
// byte for byte. They cover every file-carrying volume, the telemetry
// credential env var, the Secrets Store CSI path and user volumes inside and
// outside the home.
func podGoldenCases() []struct {
	name string
	gke  bool
	cfg  RunConfig
} {
	files := homeFileConfig()
	files.ResolvedSecrets = append(files.ResolvedSecrets, api.ResolvedSecret{
		Name: telemetryGCPCredentialsSecretName, Type: "file", Target: "~/.config/gcloud/telemetry.json", Value: "c", Source: "user",
	})

	gke := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "GEMINI_OAUTH", Type: "file", Target: "~/.gemini/oauth_creds.json", Value: "tok", Source: "user", Ref: "projects/p/secrets/s"},
			{Name: "TLS_CERT", Type: "file", Target: "/etc/ssl/cert.pem", Value: "cert", Source: "user", Ref: "projects/p/secrets/c"},
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk", Source: "user", Ref: "projects/p/secrets/k"},
		},
	}

	volumes := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Volumes: []api.VolumeMount{
			{Type: "gcs", Bucket: "b1", Target: "/data"},
			{Type: "gcs", Bucket: "b2", Target: "/home/scion/cache", ReadOnly: true},
		},
	}

	minimal := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	return []struct {
		name string
		gke  bool
		cfg  RunConfig
	}{
		{"home-files", false, files},
		{"gke-csi", true, gke},
		{"volumes", false, volumes},
		{"minimal", false, minimal},
	}
}

// TestBuildPod_PlainPodGolden pins the pod spec for pods without an NFS home:
// it must stay byte-identical to the captured testdata.
func TestBuildPod_PlainPodGolden(t *testing.T) {
	for _, tc := range podGoldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			rt.GKEMode = tc.gke
			pod, err := rt.buildPod("default", tc.cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			got, err := json.MarshalIndent(pod, "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got = append(got, '\n')
			file := filepath.Join("testdata", "k8s_pod_golden", tc.name+".json")
			if *updatePodGolden {
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("pod spec for %s differs from %s:\n%s", tc.name, file, got)
			}
		})
	}
}
