package runtime

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// assertNoMountsUnderHome fails if any agent container volume mount sits at
// or below home. A mount there makes the container runtime create its
// missing parent directories owned by root, which the non-root home sync
// cannot write into.
func assertNoMountsUnderHome(t *testing.T, pod *corev1.Pod, home string) {
	t.Helper()
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.MountPath == home || strings.HasPrefix(vm.MountPath, home+"/") {
			t.Errorf("volume %q is mounted inside the home at %q", vm.Name, vm.MountPath)
		}
	}
}

// assertStagingMount fails unless volume is mounted whole (no subPath),
// read-only, at its directory under the staging root.
func assertStagingMount(t *testing.T, pod *corev1.Pod, volume string) {
	t.Helper()
	want := "/run/scion/" + volume
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == volume && vm.MountPath == want {
			if vm.SubPath != "" {
				t.Errorf("staging mount of %q has subPath %q", volume, vm.SubPath)
			}
			if !vm.ReadOnly {
				t.Errorf("staging mount of %q is not read-only", volume)
			}
			return
		}
	}
	t.Errorf("expected volume %q mounted at %q", volume, want)
}

// assertPlacement fails unless placements copy source to target with 0600.
func assertPlacement(t *testing.T, placements []k8sFilePlacement, target, source string) {
	t.Helper()
	for _, p := range placements {
		if p.Target == target {
			if p.Source != source {
				t.Errorf("placement for %q: source = %q, want %q", target, p.Source, source)
			}
			if p.Mode != 0o600 {
				t.Errorf("placement for %q: mode = %04o, want 0600", target, p.Mode)
			}
			return
		}
	}
	t.Errorf("no placement for target %q in %+v", target, placements)
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root user: root bypasses directory permissions")
	}
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
}

// homeFileConfig is a run config with every kind of file delivered under the
// home, plus one file secret outside it.
func homeFileConfig() RunConfig {
	return RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk", Source: "user"},
			{Name: "SSH_KEY", Type: "file", Target: "~/.ssh/id_rsa", Value: "key", Source: "user"},
			{Name: "GEMINI_OAUTH", Type: "file", Target: "/home/scion/.gemini/oauth_creds.json", Value: "tok", Source: "user"},
			{Name: "TLS_CERT", Type: "file", Target: "/etc/ssl/cert.pem", Value: "cert", Source: "user"},
			{Name: "CONFIG", Type: "variable", Target: "config", Value: "v", Source: "user"},
		},
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{
				{SourcePath: "/host/adc.json", ContainerPath: "~/.config/gcloud/application_default_credentials.json"},
				{SourcePath: "", ContainerPath: "~/.claude/.credentials.json"}, // no source: not delivered
			},
		},
	}
}

func TestBuildPod_HomeFilesStagedNotMounted(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	config := homeFileConfig()

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	assertNoMountsUnderHome(t, pod, "/home/scion")
	assertStagingMount(t, pod, "agent-secrets")
	assertStagingMount(t, pod, "auth-files")

	// A target outside the home keeps its direct subPath mount.
	foundCert := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.MountPath == "/etc/ssl/cert.pem" && vm.Name == "agent-secrets" && vm.SubPath == "TLS_CERT" && vm.ReadOnly {
			foundCert = true
		}
	}
	if !foundCert {
		t.Error("expected a direct read-only subPath mount for /etc/ssl/cert.pem")
	}

	// The agent-secrets volume projects only file keys, never env values.
	for _, v := range pod.Spec.Volumes {
		if v.Name != "agent-secrets" {
			continue
		}
		var keys []string
		for _, it := range v.Secret.Items {
			if it.Key != it.Path {
				t.Errorf("item %q projected at %q, want the key name", it.Key, it.Path)
			}
			keys = append(keys, it.Key)
		}
		want := []string{"SSH_KEY", "GEMINI_OAUTH", "TLS_CERT", "secrets.json"}
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Errorf("agent-secrets items = %v, want %v", keys, want)
		}
	}

	placements := rt.k8sHomeFilePlacements(config)
	if len(placements) != 4 {
		t.Fatalf("got %d placements, want 4: %+v", len(placements), placements)
	}
	assertPlacement(t, placements, "/home/scion/.ssh/id_rsa", "/run/scion/agent-secrets/SSH_KEY")
	assertPlacement(t, placements, "/home/scion/.gemini/oauth_creds.json", "/run/scion/agent-secrets/GEMINI_OAUTH")
	assertPlacement(t, placements, "/home/scion/.scion/secrets.json", "/run/scion/agent-secrets/secrets.json")
	assertPlacement(t, placements, "/home/scion/.config/gcloud/application_default_credentials.json", "/run/scion/auth-files/auth-file-0")

	// Env vars that point at file targets keep their home paths.
	cfg2 := homeFileConfig()
	cfg2.ResolvedSecrets = append(cfg2.ResolvedSecrets, api.ResolvedSecret{
		Name: telemetryGCPCredentialsSecretName, Type: "file", Target: "~/.config/gcloud/telemetry.json", Value: "c", Source: "user",
	})
	pod2, err := rt.buildPod("default", cfg2)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	for _, e := range pod2.Spec.Containers[0].Env {
		if e.Name == telemetryGCPCredentialsEnvVar && e.Value != "/home/scion/.config/gcloud/telemetry.json" {
			t.Errorf("%s = %q, want the home path", telemetryGCPCredentialsEnvVar, e.Value)
		}
	}
	assertPlacement(t, rt.k8sHomeFilePlacements(cfg2), "/home/scion/.config/gcloud/telemetry.json", "/run/scion/agent-secrets/"+telemetryGCPCredentialsSecretName)
}

func TestBuildPod_EmptyUnixUsernameKeepsDirectMounts(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	config := homeFileConfig()
	config.UnixUsername = ""

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if p := rt.k8sHomeFilePlacements(config); len(p) != 0 {
		t.Errorf("expected no placements without a per-user home, got %+v", p)
	}
	found := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.MountPath == "/home/.ssh/id_rsa" && vm.SubPath == "SSH_KEY" {
			found = true
		}
	}
	if !found {
		t.Error("expected a direct subPath mount for the SSH key without a per-user home")
	}
}

func TestBuildPod_DuplicateHomeTargetRejected(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	config := homeFileConfig()
	// Same home target as the SSH_KEY secret, written differently.
	config.ResolvedAuth.Files = append(config.ResolvedAuth.Files, api.FileMapping{
		SourcePath: "/host/id_rsa", ContainerPath: "~/.ssh/./id_rsa",
	})
	_, err := rt.buildPod("default", config)
	if err == nil {
		t.Fatal("expected buildPod to reject two files with the same home target")
	}
	if !strings.Contains(err.Error(), "/home/scion/.ssh/id_rsa") {
		t.Errorf("error = %v, want the target named", err)
	}
}

func TestBuildPod_HomeTargetDotDotCleaned(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	config := homeFileConfig()
	config.ResolvedSecrets = append(config.ResolvedSecrets,
		api.ResolvedSecret{Name: "OUTSIDE", Type: "file", Target: "~/../other/x", Value: "o", Source: "user"},
		api.ResolvedSecret{Name: "INSIDE", Type: "file", Target: "~/.ssh/../.ssh/id", Value: "i", Source: "user"},
		// Absolute forms are not cleaned by tilde expansion.
		api.ResolvedSecret{Name: "OUTSIDE_ABS", Type: "file", Target: "/home/scion/../other/y", Value: "o", Source: "user"},
		api.ResolvedSecret{Name: "INSIDE_ABS", Type: "file", Target: "/home/scion/.ssh/../.ssh/id_abs", Value: "i", Source: "user"},
	)
	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	placements := rt.k8sHomeFilePlacements(config)
	assertPlacement(t, placements, "/home/scion/.ssh/id", "/run/scion/agent-secrets/INSIDE")
	assertPlacement(t, placements, "/home/scion/.ssh/id_abs", "/run/scion/agent-secrets/INSIDE_ABS")
	for _, p := range placements {
		if strings.Contains(p.Source, "OUTSIDE") || strings.Contains(p.Target, "other") {
			t.Errorf("target outside the home was placed: %+v", p)
		}
	}
	var outside, outsideAbs, inside bool
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		switch vm.SubPath {
		case "OUTSIDE":
			outside = filepath.Clean(vm.MountPath) == "/home/other/x" && vm.ReadOnly
		case "OUTSIDE_ABS":
			outsideAbs = filepath.Clean(vm.MountPath) == "/home/other/y" && vm.ReadOnly
		case "INSIDE", "INSIDE_ABS":
			inside = true
		}
	}
	if !outside || !outsideAbs {
		t.Error("expected direct read-only mounts resolving to /home/other/x and /home/other/y")
	}
	if inside {
		t.Error("target inside the home after cleaning has a direct mount")
	}
}

func TestBuildPod_GKEHomeFileStaged(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	rt.GKEMode = true
	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "GEMINI_OAUTH", Type: "file", Target: "~/.gemini/oauth_creds.json", Value: "tok", Source: "user", Ref: "projects/p/secrets/s"},
		},
	}
	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	assertNoMountsUnderHome(t, pod, "/home/scion")
	assertStagingMount(t, pod, "secrets-store")
	assertPlacement(t, rt.k8sHomeFilePlacements(config), "/home/scion/.gemini/oauth_creds.json", "/run/scion/secrets-store/GEMINI_OAUTH")
}

// TestNonK8sRuntimeFileMountsUnchanged pins the local container runtime
// args for secrets and auth files: they are delivered at their home paths
// (bind mount and staged secret env), not via the Kubernetes staging root.
func TestNonK8sRuntimeFileMountsUnchanged(t *testing.T) {
	config := homeFileConfig()
	config.Harness = harness.New("claude")
	config.BrokerMode = true // no host gcloud config mount: keeps the args hermetic

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	var mounts []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" || args[i] == "--mount" {
			mounts = append(mounts, args[i+1])
		}
	}
	want := []string{"/host/adc.json:/home/scion/.config/gcloud/application_default_credentials.json:ro"}
	if strings.Join(mounts, "\n") != strings.Join(want, "\n") {
		t.Errorf("mount args = %q, want %q", mounts, want)
	}
	if strings.Contains(strings.Join(args, " "), k8sFileStagingRoot) {
		t.Errorf("local runtime args reference the Kubernetes staging root: %q", args)
	}
}

// runPlacement runs the placement script locally with sh, under umask 022
// so the default directory mode is known (0755).
func runPlacement(t *testing.T, home string, placements []k8sFilePlacement) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "umask 022\n"+k8sFilePlacementScript(home, placements))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.Mode()
}

func TestK8sFilePlacementScript(t *testing.T) {
	requireTools(t, "sh", "cp", "mkdir", "chmod", "rm")
	root := t.TempDir()
	staging := filepath.Join(root, "run", "auth-files")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, ".ssh"), 0o751); err != nil {
		t.Fatal(err)
	}
	// Staged files are symlinks into a data dir, as in a Secret volume.
	data := filepath.Join(staging, "..data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"adc": "adc-content", "key": "key-content"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(staging, name)); err != nil {
			t.Fatal(err)
		}
	}
	// An existing regular file at a target is replaced.
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	adcTarget := filepath.Join(home, ".config", "gcloud", "adc.json")
	keyTarget := filepath.Join(home, ".ssh", "id_rsa")
	pubTarget := filepath.Join(home, ".ssh", "declared_mode")
	stderr, err := runPlacement(t, home, []k8sFilePlacement{
		{Source: filepath.Join(staging, "adc"), Target: adcTarget, Mode: 0o600},
		{Source: filepath.Join(staging, "key"), Target: keyTarget, Mode: 0o600},
		{Source: filepath.Join(staging, "key"), Target: pubTarget, Mode: 0o644},
	})
	if err != nil {
		t.Fatalf("placement failed: %v (stderr: %s)", err, stderr)
	}

	for target, want := range map[string]string{adcTarget: "adc-content", keyTarget: "key-content"} {
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read %s: %v", target, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", target, got, want)
		}
		if m := modeOf(t, target); !m.IsRegular() || m.Perm() != 0o600 {
			t.Errorf("%s mode = %v, want regular 0600", target, m)
		}
	}
	// A declared mode other than the default is applied as given.
	if m := modeOf(t, pubTarget).Perm(); m != 0o644 {
		t.Errorf("declared mode not applied: %04o, want 0644", m)
	}
	// The directory holding the file is created 0700; missing ancestors get
	// the default mode; existing directories are left alone.
	if m := modeOf(t, filepath.Dir(adcTarget)).Perm(); m != 0o700 {
		t.Errorf("created credential dir mode = %04o, want 0700", m)
	}
	if m := modeOf(t, filepath.Join(home, ".config")).Perm(); m != 0o755 {
		t.Errorf("created ancestor dir mode = %04o, want the default 0755 under umask 022", m)
	}
	if m := modeOf(t, filepath.Join(home, ".ssh")).Perm(); m != 0o751 {
		t.Errorf("existing dir mode changed to %04o, want 0751", m)
	}
}

func TestK8sFilePlacementScript_FailureNamesTarget(t *testing.T) {
	requireTools(t, "sh", "cp")
	root := t.TempDir()
	home := filepath.Join(root, "home")
	target := filepath.Join(home, ".gemini", "oauth_creds.json")
	never := filepath.Join(home, "never")
	stderr, err := runPlacement(t, home, []k8sFilePlacement{
		{Source: filepath.Join(root, "missing"), Target: target, Mode: 0o600},
		{Source: filepath.Join(root, "missing"), Target: never, Mode: 0o600},
	})
	if err == nil {
		t.Fatal("expected placement to fail for a missing source")
	}
	if !strings.Contains(stderr, "failed to place file at "+target) {
		t.Errorf("stderr does not name the target: %q", stderr)
	}
	if strings.Contains(stderr, never) {
		t.Errorf("placement continued after the first failure: %q", stderr)
	}
}

func TestK8sFilePlacementScript_QuotesPaths(t *testing.T) {
	requireTools(t, "sh", "cp")
	root := t.TempDir()
	src := filepath.Join(root, "it's src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "dir with 'quote' $(x)", "f")
	if stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}}); err != nil {
		t.Fatalf("placement failed: %v (stderr: %s)", err, stderr)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target not placed: %v", err)
	}
}

// placementFixture creates a home and one staged source file.
func placementFixture(t *testing.T) (root, home, src string) {
	t.Helper()
	root = t.TempDir()
	home = filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	src = filepath.Join(root, "staged")
	if err := os.WriteFile(src, []byte("staged-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, home, src
}

func TestK8sFilePlacementScript_RefusesSymlinks(t *testing.T) {
	requireTools(t, "sh", "cp", "mkdir", "chmod", "rm")

	t.Run("symlinked_parent_outside_home", func(t *testing.T) {
		root, home, src := placementFixture(t)
		elsewhere := filepath.Join(root, "elsewhere")
		if err := os.MkdirAll(elsewhere, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(home, ".gemini")); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, ".gemini", "oauth_creds.json")
		stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}})
		if err == nil {
			t.Fatal("expected placement through a symlinked directory to fail")
		}
		if !strings.Contains(stderr, "failed to place file at "+target) ||
			!strings.Contains(stderr, filepath.Join(home, ".gemini")+" is a symbolic link") {
			t.Errorf("stderr = %q, want the target and the symlinked directory named", stderr)
		}
		if _, err := os.Lstat(filepath.Join(elsewhere, "oauth_creds.json")); err == nil {
			t.Error("file was written through the symlinked directory")
		}
	})

	t.Run("symlinked_intermediate_inside_home", func(t *testing.T) {
		_, home, src := placementFixture(t)
		real := filepath.Join(home, "real")
		if err := os.MkdirAll(filepath.Join(real, "gcloud"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(home, ".config")); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, ".config", "gcloud", "adc.json")
		stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}})
		if err == nil {
			t.Fatal("expected placement through a symlinked ancestor to fail")
		}
		if !strings.Contains(stderr, filepath.Join(home, ".config")+" is a symbolic link") {
			t.Errorf("stderr = %q, want the symlinked ancestor named", stderr)
		}
		if _, err := os.Lstat(filepath.Join(real, "gcloud", "adc.json")); err == nil {
			t.Error("file was written through the symlinked ancestor")
		}
	})

	t.Run("symlinked_target", func(t *testing.T) {
		root, home, src := placementFixture(t)
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		decoy := filepath.Join(root, "decoy")
		if err := os.WriteFile(decoy, []byte("decoy"), 0o644); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, ".ssh", "id_rsa")
		if err := os.Symlink(decoy, target); err != nil {
			t.Fatal(err)
		}
		stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}})
		if err == nil {
			t.Fatal("expected placement onto a symlinked target to fail")
		}
		if !strings.Contains(stderr, "failed to place file at "+target) || !strings.Contains(stderr, "symbolic link") {
			t.Errorf("stderr = %q, want the target named as a symlink", stderr)
		}
		if got, _ := os.ReadFile(decoy); string(got) != "decoy" {
			t.Errorf("symlink target was written through: %q", got)
		}
		if m := modeOf(t, target); m&os.ModeSymlink == 0 {
			t.Errorf("symlink at target was replaced: mode %v", m)
		}
	})

	t.Run("symlinked_second_level_dir", func(t *testing.T) {
		root, home, src := placementFixture(t)
		elsewhere := filepath.Join(root, "elsewhere")
		if err := os.MkdirAll(elsewhere, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(home, ".config"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(home, ".config", "gcloud")
		if err := os.Symlink(elsewhere, link); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(link, "adc.json")
		stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}})
		if err == nil {
			t.Fatal("expected placement through a symlinked second-level directory to fail")
		}
		if !strings.Contains(stderr, "failed to place file at "+target) ||
			!strings.Contains(stderr, link+" is a symbolic link") {
			t.Errorf("stderr = %q, want the target and %s named as a symbolic link", stderr, link)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Errorf("files written outside the home: %v", entries)
		}
	})

	t.Run("symlinked_middle_of_three_levels", func(t *testing.T) {
		root, home, src := placementFixture(t)
		elsewhere := filepath.Join(root, "elsewhere")
		if err := os.MkdirAll(filepath.Join(elsewhere, "c"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(home, "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(home, "a", "b")
		if err := os.Symlink(elsewhere, link); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(link, "c", "f")
		stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}})
		if err == nil {
			t.Fatal("expected placement through a symlinked middle directory to fail")
		}
		if !strings.Contains(stderr, "failed to place file at "+target) ||
			!strings.Contains(stderr, link+" is a symbolic link") {
			t.Errorf("stderr = %q, want the target and %s named as a symbolic link", stderr, link)
		}
		if entries, _ := os.ReadDir(filepath.Join(elsewhere, "c")); len(entries) != 0 {
			t.Errorf("files written outside the home: %v", entries)
		}
	})

	t.Run("directory_at_target", func(t *testing.T) {
		_, home, src := placementFixture(t)
		target := filepath.Join(home, ".ssh", "id_rsa")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}})
		if err == nil {
			t.Fatal("expected placement onto a directory to fail")
		}
		if !strings.Contains(stderr, "failed to place file at "+target+": target exists and is not a regular file") {
			t.Errorf("stderr = %q, want the not-a-regular-file message", stderr)
		}
	})

	t.Run("non_directory_parent", func(t *testing.T) {
		_, home, src := placementFixture(t)
		if err := os.WriteFile(filepath.Join(home, ".gemini"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, ".gemini", "oauth_creds.json")
		stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}})
		if err == nil {
			t.Fatal("expected placement under a regular file to fail")
		}
		if !strings.Contains(stderr, filepath.Join(home, ".gemini")+" is not a directory") {
			t.Errorf("stderr = %q, want the non-directory named", stderr)
		}
	})
}

func TestK8sFilePlacementScript_UnwritableDirNamed(t *testing.T) {
	skipIfRoot(t)
	requireTools(t, "sh", "cp", "mkdir", "chmod", "rm")
	_, home, src := placementFixture(t)
	dir := filepath.Join(home, ".gemini")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	target := filepath.Join(dir, "oauth_creds.json")
	stderr, err := runPlacement(t, home, []k8sFilePlacement{{Source: src, Target: target, Mode: 0o600}})
	if err == nil {
		t.Fatal("expected placement into a non-writable directory to fail")
	}
	if !strings.Contains(stderr, "failed to place file at "+target) ||
		!strings.Contains(stderr, "directory "+dir+" is not writable") {
		t.Errorf("stderr = %q, want the target and the non-writable directory named", stderr)
	}
	if m := modeOf(t, dir).Perm(); m != 0o500 {
		t.Errorf("existing directory mode changed to %04o", m)
	}
}

// buildHomeArchive archives src with the same tar arguments syncToPod uses.
func buildHomeArchive(t *testing.T, src string) []byte {
	t.Helper()
	cmd := exec.Command("tar", syncArchiveCreateArgs(src)...)
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("tar create: %v", err)
	}
	return out
}

// extractHomeArchive extracts archive into dest with the same command
// syncToPod runs in the pod, as the current (non-root) user.
func extractHomeArchive(t *testing.T, archive []byte, dest string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", syncArchiveExtractCommand(dest))
	cmd.Stdin = bytes.NewReader(archive)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

// writeTemplateHome creates a source home like the default template's,
// including directories without owner write.
func writeTemplateHome(t *testing.T, src string) {
	t.Helper()
	files := map[string]string{
		".gemini/.geminiignore":        "x",
		".ssh/known_hosts":             "x",
		".config/gcloud/configuration": "x",
		".scion/agent.json":            "{}",
		".claude/debug/log":            "x",
		".claude/ro/file":              "x",
		".zshrc":                       "x",
	}
	for rel, content := range files {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(src, ".claude", "debug"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, ".claude", "ro"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(src, ".claude", "debug"), 0o755)
		_ = os.Chmod(filepath.Join(src, ".claude", "ro"), 0o755)
	})
}

// TestHomeSyncArchive_ReadOnlySourceDirs checks that a source home with
// 0555 and 0500 directories extracts fully as a non-root user with the
// sync's tar flags.
func TestHomeSyncArchive_ReadOnlySourceDirs(t *testing.T) {
	skipIfRoot(t)
	requireTools(t, "tar", "sh")
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dest := filepath.Join(root, "dest")
	writeTemplateHome(t, src)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(dest, ".claude", "debug"), 0o755)
		_ = os.Chmod(filepath.Join(dest, ".claude", "ro"), 0o755)
	})

	if stderr, err := extractHomeArchive(t, buildHomeArchive(t, src), dest); err != nil {
		t.Fatalf("extract failed: %v (stderr: %s)", err, stderr)
	}
	for _, rel := range []string{".claude/debug/log", ".claude/ro/file", ".gemini/.geminiignore"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing %s after extract: %v", rel, err)
		}
	}
}

// TestHomeSync_PodMountsLeaveHomeWritable simulates the agent container:
// for every volume mount the pod spec places inside the home, the parent
// directories the container runtime would create are made, owned by
// someone else (simulated as not writable by the current user). The home
// archive is then extracted as the current non-root user with the sync's
// tar command. With file mounts inside the home this fails with
// "Cannot open: Permission denied"; with the files staged outside the home
// it succeeds.
func TestHomeSync_PodMountsLeaveHomeWritable(t *testing.T) {
	skipIfRoot(t)
	requireTools(t, "tar", "sh")
	rt, _, _ := newTestK8sRuntime()
	config := homeFileConfig()
	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	root := t.TempDir()
	src := filepath.Join(root, "src")
	writeTemplateHome(t, src)
	destHome := filepath.Join(root, "home", "scion")
	if err := os.MkdirAll(destHome, 0o700); err != nil {
		t.Fatal(err)
	}

	var locked []string
	t.Cleanup(func() {
		for _, d := range locked {
			_ = os.Chmod(d, 0o755)
		}
		_ = os.Chmod(filepath.Join(destHome, ".claude", "debug"), 0o755)
		_ = os.Chmod(filepath.Join(destHome, ".claude", "ro"), 0o755)
	})
	const podHome = "/home/scion"
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if !strings.HasPrefix(vm.MountPath, podHome+"/") {
			continue
		}
		rel := strings.TrimPrefix(filepath.Dir(vm.MountPath), podHome)
		dir := destHome
		for _, part := range strings.Split(strings.Trim(rel, "/"), "/") {
			if part == "" {
				continue
			}
			dir = filepath.Join(dir, part)
			if _, err := os.Stat(dir); err == nil {
				continue
			}
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			locked = append(locked, dir)
		}
	}
	// Lock the runtime-created directories only once all exist.
	for _, d := range locked {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}

	if stderr, err := extractHomeArchive(t, buildHomeArchive(t, src), destHome); err != nil {
		t.Fatalf("home sync extract failed with pod mounts %v: %v (stderr: %s)", locked, err, stderr)
	}
}

// TestRun_PlacesHomeFilesAfterSyncBeforeStartupGate checks that Run runs the
// home sync, then copies the staged files into the home, then signals the
// startup gate; that a placement failure stops the start with an error naming
// the target and without signalling the gate; and that file and secret
// contents never appear in logs, exec commands or errors.
func TestRun_PlacesHomeFilesAfterSyncBeforeStartupGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failPlace bool
	}{{name: "success"}, {name: "placement_fails", failPlace: true}} {
		t.Run(tc.name, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			rt.execProbe = func(context.Context, string, string) error { return nil }
			var mu sync.Mutex
			var cmds []string
			rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
				joined := strings.Join(cmd, " ")
				mu.Lock()
				cmds = append(cmds, joined)
				mu.Unlock()
				if tc.failPlace && strings.Contains(joined, "place ") {
					return "", fmt.Errorf("exec failed: exit 1 (stderr: failed to place file at /home/scion/.ssh/id_rsa)")
				}
				return "", nil
			}
			rt.homeSync = func(_ context.Context, _, _, src, dst string, _ []string) error {
				mu.Lock()
				cmds = append(cmds, "home-sync "+src+" "+dst)
				mu.Unlock()
				return nil
			}

			logs := &lockedBuffer{}
			prevLog := runtimeLog
			runtimeLog = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			t.Cleanup(func() { runtimeLog = prevLog })

			const adcContent = "adc-file-content-5d1e"
			adc := filepath.Join(t.TempDir(), "adc.json")
			if err := os.WriteFile(adc, []byte(adcContent), 0o600); err != nil {
				t.Fatal(err)
			}
			config := homeFileConfig()
			secretValues := []string{adcContent}
			for i := range config.ResolvedSecrets {
				v := "secret-value-" + config.ResolvedSecrets[i].Name + "-9c4b"
				config.ResolvedSecrets[i].Value = v
				secretValues = append(secretValues, v)
			}
			config.Name = "place-agent"
			config.Harness = &MockHarness{}
			config.ResolvedAuth.Files[0].SourcePath = adc
			config.HomeDir = t.TempDir()

			errChan := make(chan error, 1)
			go func() {
				_, err := rt.Run(context.Background(), config)
				errChan <- err
			}()

			var pod *corev1.Pod
			for i := 0; i < 50; i++ {
				p, err := clientset.CoreV1().Pods("default").Get(context.Background(), "place-agent", metav1.GetOptions{})
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

			var runErr error
			select {
			case runErr = <-errChan:
			case <-time.After(15 * time.Second):
				t.Fatal("Run did not return")
			}

			mu.Lock()
			defer mu.Unlock()
			// File and secret contents never reach logs, exec commands or errors.
			for _, v := range secretValues {
				if strings.Contains(logs.String(), v) {
					t.Errorf("logs contain the content %q", v)
				}
				if strings.Contains(strings.Join(cmds, "\n"), v) {
					t.Errorf("exec commands contain the content %q", v)
				}
				if runErr != nil && strings.Contains(runErr.Error(), v) {
					t.Errorf("Run error contains the content %q", v)
				}
			}
			if !strings.Contains(logs.String(), "Placing files in agent home") {
				t.Errorf("placement log not captured: %s", logs.String())
			}
			syncIdx, placeIdx, gateIdx := -1, -1, -1
			for i, c := range cmds {
				if strings.HasPrefix(c, "home-sync ") && syncIdx < 0 {
					syncIdx = i
				}
				if strings.Contains(c, "place ") && placeIdx < 0 {
					placeIdx = i
				}
				if strings.Contains(c, ".scion-home-ready") {
					gateIdx = i
				}
			}
			if placeIdx < 0 {
				t.Fatalf("no placement exec issued; execs: %q", cmds)
			}
			if syncIdx < 0 {
				t.Fatalf("home sync did not run; execs: %q", cmds)
			}
			if syncIdx > placeIdx {
				t.Errorf("placement (step %d) ran before the home sync (step %d): %q", placeIdx, syncIdx, cmds)
			}
			if !strings.Contains(cmds[placeIdx], "/home/scion/.gemini/oauth_creds.json") {
				t.Errorf("placement exec does not cover the .gemini target: %q", cmds[placeIdx])
			}
			if tc.failPlace {
				if runErr == nil || !strings.Contains(runErr.Error(), "/home/scion/.ssh/id_rsa") {
					t.Errorf("Run error = %v, want a start error naming the target", runErr)
				}
				if gateIdx >= 0 {
					t.Errorf("startup gate signalled after a placement failure: %q", cmds)
				}
				return
			}
			if runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}
			if gateIdx < placeIdx {
				t.Errorf("startup gate (exec %d) signalled before placement (exec %d): %q", gateIdx, placeIdx, cmds)
			}
		})
	}
}

// lockedBuffer is a bytes.Buffer safe for concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
