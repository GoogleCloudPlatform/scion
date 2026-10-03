package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// nfsHomeTestConfig is homeFileConfig with the telemetry credential, as an
// NFS-home pod when nfs is true. Nothing outside tests sets
// HomeStorageBackend yet.
func nfsHomeTestConfig(nfs bool) RunConfig {
	cfg := homeFileConfig()
	cfg.ResolvedSecrets = append(cfg.ResolvedSecrets, api.ResolvedSecret{
		Name: telemetryGCPCredentialsSecretName, Type: "file", Target: "~/.config/gcloud/telemetry.json", Value: "c", Source: "user",
	})
	if nfs {
		cfg.HomeStorageBackend = HomeStorageNFS
	}
	return cfg
}

func podEnv(pod *corev1.Pod) map[string]string {
	out := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

func TestBuildPod_NFSHomeVersusPlainPod(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	plainCfg := nfsHomeTestConfig(false)
	plainCfg.Annotations = map[string]string{"scion.home_storage": "nfs", "other": "x"}
	plain, err := rt.buildPod("default", plainCfg)
	if err != nil {
		t.Fatalf("buildPod plain: %v", err)
	}
	nfsCfg := nfsHomeTestConfig(true)
	nfsCfg.Annotations = map[string]string{"other": "x"}
	nfs, err := rt.buildPod("default", nfsCfg)
	if err != nil {
		t.Fatalf("buildPod nfs: %v", err)
	}

	// Annotation: set from the field only; a caller-supplied value is dropped.
	if _, ok := plain.Annotations[k8sHomeStorageAnnotation]; ok {
		t.Errorf("plain pod carries %s", k8sHomeStorageAnnotation)
	}
	if plain.Annotations["other"] != "x" || nfs.Annotations["other"] != "x" {
		t.Error("other annotations not kept")
	}
	if got := nfs.Annotations[k8sHomeStorageAnnotation]; got != "nfs" {
		t.Errorf("nfs pod %s = %q, want nfs", k8sHomeStorageAnnotation, got)
	}
	if plainCfg.Annotations[k8sHomeStorageAnnotation] != "nfs" || len(nfsCfg.Annotations) != 1 {
		t.Error("buildPod modified the caller's annotation map")
	}

	// Mounts: no mount inside the home in either pod; the memory dir only on
	// the NFS-home pod.
	assertNoMountsUnderHome(t, plain, "/home/scion")
	assertNoMountsUnderHome(t, nfs, "/home/scion")
	for _, pod := range []*corev1.Pod{plain, nfs} {
		assertStagingMount(t, pod, "agent-secrets")
		assertStagingMount(t, pod, "auth-files")
	}
	hasMem := func(pod *corev1.Pod) bool {
		vol, mount := false, false
		for _, v := range pod.Spec.Volumes {
			if v.Name == "scion-mem" && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
				vol = true
			}
		}
		for _, vm := range pod.Spec.Containers[0].VolumeMounts {
			if vm.Name == "scion-mem" && vm.MountPath == "/run/scion/mem" && !vm.ReadOnly && vm.SubPath == "" {
				mount = true
			}
		}
		return vol && mount
	}
	if hasMem(plain) {
		t.Error("plain pod has the memory dir")
	}
	if !hasMem(nfs) {
		t.Error("nfs pod lacks the memory-backed scion-mem volume at /run/scion/mem")
	}

	// Env values.
	pe, ne := podEnv(plain), podEnv(nfs)
	for _, name := range []string{"SCION_SECRETS_FILE", "SCION_HOME_LINKS", "SCION_HARNESS_OUTPUTS_DIR", "SCION_HARNESS_SECRETS_DIR"} {
		if _, ok := pe[name]; ok {
			t.Errorf("plain pod sets %s", name)
		}
	}
	if got := pe[telemetryGCPCredentialsEnvVar]; got != "/home/scion/.config/gcloud/telemetry.json" {
		t.Errorf("plain %s = %q, want the home path", telemetryGCPCredentialsEnvVar, got)
	}
	want := map[string]string{
		"SCION_SECRETS_FILE":          "/run/scion/agent-secrets/secrets.json",
		telemetryGCPCredentialsEnvVar: "/run/scion/agent-secrets/" + telemetryGCPCredentialsSecretName,
		"SCION_HARNESS_OUTPUTS_DIR":   "/run/scion/mem/outputs",
		"SCION_HARNESS_SECRETS_DIR":   "/run/scion/mem/harness-secrets",
		"HOME":                        "/home/scion",
	}
	for k, v := range want {
		if ne[k] != v {
			t.Errorf("nfs %s = %q, want %q", k, ne[k], v)
		}
	}
	var links []k8sHomeLink
	if err := json.Unmarshal([]byte(ne["SCION_HOME_LINKS"]), &links); err != nil {
		t.Fatalf("SCION_HOME_LINKS is not JSON: %v", err)
	}
	if !reflect.DeepEqual(links, rt.k8sHomeLinks(nfsCfg)) {
		t.Errorf("SCION_HOME_LINKS = %+v, want %+v", links, rt.k8sHomeLinks(nfsCfg))
	}
	for _, s := range nfsCfg.ResolvedSecrets {
		if strings.Contains(ne["SCION_HOME_LINKS"], s.Value) && len(s.Value) > 1 {
			t.Errorf("SCION_HOME_LINKS contains the value of %s", s.Name)
		}
	}
}

func TestBuildPod_NFSHomeWithoutSecretsJSON(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := RunConfig{Name: "a", Image: "i", UnixUsername: "scion", HomeStorageBackend: HomeStorageNFS}
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	env := podEnv(pod)
	if _, ok := env["SCION_SECRETS_FILE"]; ok {
		t.Error("SCION_SECRETS_FILE set without a secrets.json")
	}
	if env["SCION_HOME_LINKS"] != "[]" {
		t.Errorf("SCION_HOME_LINKS = %q, want []", env["SCION_HOME_LINKS"])
	}
}

func TestBuildPod_NFSHomeRejectsMountsUnderHome(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := RunConfig{
		Name: "a", Image: "i", UnixUsername: "scion",
		Volumes: []api.VolumeMount{
			{Type: "gcs", Bucket: "b1", Target: "/data"},
			{Type: "gcs", Bucket: "b2", Target: "/home/scion/cache"},
		},
	}
	if _, err := rt.buildPod("default", cfg); err != nil {
		t.Fatalf("plain pod with a volume inside the home: %v", err)
	}
	cfg.HomeStorageBackend = HomeStorageNFS
	_, err := rt.buildPod("default", cfg)
	if err == nil {
		t.Fatal("expected a volume inside the home to be rejected")
	}
	if !strings.Contains(err.Error(), `"gcs-vol-1"`) || !strings.Contains(err.Error(), "/home/scion/cache") {
		t.Errorf("error = %v, want the volume and path named", err)
	}

	cfg.Volumes = cfg.Volumes[:1]
	if _, err := rt.buildPod("default", cfg); err != nil {
		t.Errorf("volume outside the home rejected: %v", err)
	}
}

func TestBuildPod_HomeStorageBackendValues(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	for _, tc := range []struct {
		backend, user, wantErr string
	}{
		{"", "scion", ""},
		{"local", "scion", ""},
		{"nfs", "scion", ""},
		{"nfs", "", "requires a container user name"},
		{"gcs", "scion", `unsupported home storage backend "gcs"`},
	} {
		_, err := rt.buildPod("default", RunConfig{Name: "a", Image: "i", UnixUsername: tc.user, HomeStorageBackend: tc.backend})
		if tc.wantErr == "" && err != nil {
			t.Errorf("%q/%q: %v", tc.backend, tc.user, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%q/%q: error = %v, want %q", tc.backend, tc.user, err, tc.wantErr)
		}
	}
	// "local" builds the same pod as unset.
	a, _ := rt.buildPod("default", nfsHomeTestConfig(false))
	cfg := nfsHomeTestConfig(false)
	cfg.HomeStorageBackend = "local"
	b, _ := rt.buildPod("default", cfg)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Error(`home storage "local" changes the pod spec`)
	}
}

// TestK8sHomeLinks_MatchCopyPlacements checks that link mode and copy mode
// cover the same targets from the same staged sources.
func TestK8sHomeLinks_MatchCopyPlacements(t *testing.T) {
	for _, tc := range podGoldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			rt.GKEMode = tc.gke
			cfg := tc.cfg
			placements := rt.k8sHomeFilePlacements(cfg)
			links := rt.k8sHomeLinks(cfg)
			if len(links) != len(placements) {
				t.Fatalf("%d links, %d placements", len(links), len(placements))
			}
			for i, p := range placements {
				l := links[i]
				if filepath.IsAbs(l.Target) || filepath.Join("/home/scion", l.Target) != p.Target {
					t.Errorf("link target %q does not match placement %q", l.Target, p.Target)
				}
				if l.Source != p.Source {
					t.Errorf("link source %q, placement source %q", l.Source, p.Source)
				}
				if l.Mode != "0600" {
					t.Errorf("link mode %q, want 0600", l.Mode)
				}
			}
			cfg.HomeStorageBackend = HomeStorageNFS
			targets := rt.k8sHomeLinkTargets(cfg)
			if len(targets) != len(links) {
				t.Errorf("sync excludes %v do not match the links", targets)
			}
		})
	}
	// Plain pods exclude nothing from the sync.
	rt, _, _ := newTestK8sRuntime()
	if got := rt.k8sHomeLinkTargets(nfsHomeTestConfig(false)); got != nil {
		t.Errorf("plain pod sync excludes = %v, want none", got)
	}
}

func runLinkVerify(t *testing.T, home string, links []k8sHomeLink, skipped map[string]bool) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", k8sHomeLinkVerifyScript(home, links, skipped))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestK8sHomeLinkVerifyScript(t *testing.T) {
	requireTools(t, "sh", "readlink")
	setup := func(t *testing.T) (home string, links []k8sHomeLink) {
		root := t.TempDir()
		home = filepath.Join(root, "home")
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(root, "run", "agent-secrets", "SSH_KEY")
		return home, []k8sHomeLink{{Target: ".ssh/id_rsa", Source: src, Mode: "0600"}}
	}

	t.Run("expected_link", func(t *testing.T) {
		home, links := setup(t)
		if err := os.Symlink(links[0].Source, filepath.Join(home, ".ssh/id_rsa")); err != nil {
			t.Fatal(err)
		}
		if out, err := runLinkVerify(t, home, links, nil); err != nil {
			t.Errorf("expected link refused: %v %s", err, out)
		}
	})

	t.Run("recorded_skip", func(t *testing.T) {
		home, links := setup(t)
		target := filepath.Join(home, ".ssh/id_rsa")
		if err := os.WriteFile(target, []byte("user"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := runLinkVerify(t, home, links, map[string]bool{".ssh/id_rsa": true}); err != nil {
			t.Errorf("recorded skip refused: %v %s", err, out)
		}
		if b, _ := os.ReadFile(target); string(b) != "user" {
			t.Error("verification changed the user file")
		}
	})

	fails := []struct {
		name    string
		prep    func(t *testing.T, target, src string)
		skipped bool
		reason  string
	}{
		{"regular_file", func(t *testing.T, target, _ string) {
			if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false, "expected a symbolic link to the staged file"},
		{"wrong_link", func(t *testing.T, target, _ string) {
			if err := os.Symlink("/elsewhere", target); err != nil {
				t.Fatal(err)
			}
		}, false, "symbolic link does not point to the staged file"},
		{"missing", func(*testing.T, string, string) {}, false, "missing"},
		{"skip_recorded_but_directory", func(t *testing.T, target, _ string) {
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
		}, true, "expected a symbolic link to the staged file"},
	}
	for _, tc := range fails {
		t.Run(tc.name, func(t *testing.T) {
			home, links := setup(t)
			target := filepath.Join(home, ".ssh/id_rsa")
			tc.prep(t, target, links[0].Source)
			out, err := runLinkVerify(t, home, links, map[string]bool{".ssh/id_rsa": tc.skipped})
			if err == nil {
				t.Fatal("expected verification to fail")
			}
			if !strings.Contains(out, "home file check failed at "+target+": "+tc.reason) {
				t.Errorf("output = %q, want the target and %q", out, tc.reason)
			}
		})
	}
}

func TestParseK8sHomeLinksResult(t *testing.T) {
	got, err := parseK8sHomeLinksResult("")
	if err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
	got, err = parseK8sHomeLinksResult(`{"links":[{"target":".ssh/id_rsa","state":"linked"},{"target":".gemini/oauth_creds.json","state":"skipped"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]bool{".gemini/oauth_creds.json": true}) {
		t.Errorf("skipped = %v", got)
	}
	if _, err := parseK8sHomeLinksResult("{"); err == nil || !strings.Contains(err.Error(), k8sHomeLinksResultFile) {
		t.Errorf("invalid result: err = %v", err)
	}
}

// TestPlaceK8sHomeFiles_LinkModeVerifiesOnly checks that link mode reads the
// result file, runs only the verification (never the copy script) and
// passes the recorded skips to it.
func TestPlaceK8sHomeFiles_LinkModeVerifiesOnly(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	var cmds []string
	rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
		joined := strings.Join(cmd, " ")
		cmds = append(cmds, joined)
		if strings.Contains(joined, "home-links-result.json") {
			return `{"links":[{"target":".gemini/oauth_creds.json","state":"skipped"}]}`, nil
		}
		return "", nil
	}
	cfg := nfsHomeTestConfig(true)
	if err := rt.placeK8sHomeFiles(context.Background(), "default", "p", cfg, k8sHomeFilesLink); err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 {
		t.Fatalf("execs = %q, want the result read and the check", cmds)
	}
	if strings.Contains(cmds[1], "place ") || strings.Contains(cmds[1], "cp ") {
		t.Errorf("link mode ran a copy: %q", cmds[1])
	}
	if !strings.Contains(cmds[1], "check '/home/scion/.gemini/oauth_creds.json' '/run/scion/agent-secrets/GEMINI_OAUTH' 1") {
		t.Errorf("recorded skip not passed: %q", cmds[1])
	}
	if !strings.Contains(cmds[1], "check '/home/scion/.ssh/id_rsa' '/run/scion/agent-secrets/SSH_KEY' 0") {
		t.Errorf("link check missing: %q", cmds[1])
	}

	// Copy mode is unchanged.
	cmds = nil
	if err := rt.placeK8sHomeFiles(context.Background(), "default", "p", nfsHomeTestConfig(false), k8sHomeFilesCopy); err != nil {
		t.Fatal(err)
	}
	want := "sh -c " + k8sFilePlacementScript("/home/scion", rt.k8sHomeFilePlacements(nfsHomeTestConfig(false)))
	if len(cmds) != 1 || cmds[0] != want {
		t.Errorf("copy mode execs = %q", cmds)
	}
}

func TestSyncArchiveCreateArgs_ExcludesLinkTargets(t *testing.T) {
	if got := syncArchiveCreateArgs("/src"); !reflect.DeepEqual(got, []string{"-cz", "-C", "/src", "."}) {
		t.Errorf("args without excludes = %q", got)
	}
	requireTools(t, "tar", "sh")
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dest := filepath.Join(root, "dest")
	files := []string{
		".ssh/id_rsa", ".ssh/id_rsa.pub", ".scion/secrets.json", ".scion/agent.json",
		"cfg/a[1]*.json", "cfg/a1x.json", "nested/.ssh/id_rsa",
	}
	for _, rel := range files {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	excludes := []string{".ssh/id_rsa", ".scion/secrets.json", "cfg/a[1]*.json"}
	cmd := exec.Command("tar", syncArchiveCreateArgs(src, excludes...)...)
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	archive, err := cmd.Output()
	if err != nil {
		t.Fatalf("tar create: %v", err)
	}
	if stderr, err := extractHomeArchive(t, archive, dest); err != nil {
		t.Fatalf("extract: %v %s", err, stderr)
	}
	for _, rel := range files {
		_, err := os.Lstat(filepath.Join(dest, rel))
		excluded := false
		for _, e := range excludes {
			excluded = excluded || e == rel
		}
		if excluded && err == nil {
			t.Errorf("%s was synced, want it excluded", rel)
		}
		if !excluded && err != nil {
			t.Errorf("%s missing after sync: %v", rel, err)
		}
	}
}

// startFakeK8sPod runs rt.Run with config, marks the created pod ready and
// returns Run's error.
func startFakeK8sPod(t *testing.T, rt *KubernetesRuntime, clientset *fake.Clientset, config RunConfig) error {
	t.Helper()
	errChan := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), config)
		errChan <- err
	}()
	var pod *corev1.Pod
	for i := 0; i < 50; i++ {
		p, err := clientset.CoreV1().Pods("default").Get(context.Background(), config.Name, metav1.GetOptions{})
		if err == nil {
			pod = p
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pod == nil {
		t.Fatal("pod was not created")
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: agentContainerName, Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if _, err := clientset.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errChan:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
	return nil
}

// TestRun_NFSHomeExcludesLinksAndVerifies checks the start of an NFS-home
// pod: the home sync excludes the link targets, link mode verifies instead
// of copying, and a failed verification stops the start before the startup
// gate, naming the target. A plain pod syncs without excludes.
func TestRun_NFSHomeExcludesLinksAndVerifies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nfs        bool
		failVerify bool
	}{{"plain", false, false}, {"nfs", true, false}, {"nfs_verify_fails", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			rt.execProbe = func(context.Context, string, string) error { return nil }
			var mu sync.Mutex
			var cmds []string
			var excludes []string
			rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
				joined := strings.Join(cmd, " ")
				mu.Lock()
				cmds = append(cmds, joined)
				mu.Unlock()
				if tc.failVerify && strings.Contains(joined, "check ") {
					return "", fmt.Errorf("exec failed: exit 1 (stderr: home file check failed at /home/scion/.ssh/id_rsa: missing)")
				}
				return "", nil
			}
			rt.homeSync = func(_ context.Context, _, _, _, _ string, ex []string) error {
				mu.Lock()
				excludes = ex
				cmds = append(cmds, "home-sync")
				mu.Unlock()
				return nil
			}
			adc := filepath.Join(t.TempDir(), "adc.json")
			if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			config := nfsHomeTestConfig(tc.nfs)
			config.Name = "nfs-agent"
			config.Harness = &MockHarness{}
			config.ResolvedAuth.Files[0].SourcePath = adc
			config.HomeDir = t.TempDir()

			runErr := startFakeK8sPod(t, rt, clientset, config)

			mu.Lock()
			defer mu.Unlock()
			joined := strings.Join(cmds, "\n")
			gate := strings.Contains(joined, ".scion-home-ready")
			if !tc.nfs {
				if runErr != nil {
					t.Fatalf("Run: %v", runErr)
				}
				if len(excludes) != 0 {
					t.Errorf("plain pod sync excludes = %v", excludes)
				}
				if !strings.Contains(joined, "place ") {
					t.Error("plain pod did not copy the home files")
				}
				return
			}
			wantEx := []string{".ssh/id_rsa", ".gemini/oauth_creds.json", ".config/gcloud/telemetry.json", ".scion/secrets.json", ".config/gcloud/application_default_credentials.json"}
			if !sameStrings(excludes, wantEx) {
				t.Errorf("sync excludes = %v, want %v", excludes, wantEx)
			}
			if strings.Contains(joined, "place ") {
				t.Error("NFS-home pod ran the copy script")
			}
			if !strings.Contains(joined, "check ") {
				t.Errorf("no link verification; execs: %q", cmds)
			}
			if tc.failVerify {
				if runErr == nil || !strings.Contains(runErr.Error(), "/home/scion/.ssh/id_rsa") {
					t.Errorf("Run error = %v, want the target named", runErr)
				}
				if gate {
					t.Error("startup gate signalled after a failed verification")
				}
				return
			}
			if runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}
			if !gate {
				t.Error("startup gate not signalled")
			}
		})
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
