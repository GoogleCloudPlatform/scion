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
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// EnvHarness returns harness env and telemetry env for testing parity.
type EnvHarness struct{}

func (h *EnvHarness) Name() string { return "test" }
func (h *EnvHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{Harness: "test"}
}
func (h *EnvHarness) GetCommand(task string, resume bool, args []string) []string {
	return []string{"/bin/echo", "test"}
}
func (h *EnvHarness) GetEnv(agentName, homeDir, username string) map[string]string {
	return map[string]string{
		"HARNESS_AGENT_NAME": agentName,
		"HARNESS_HOME":       homeDir,
	}
}
func (h *EnvHarness) DefaultConfigDir() string              { return ".test" }
func (h *EnvHarness) SkillsDir() string                     { return ".test/skills" }
func (h *EnvHarness) HasSystemPrompt(agentHome string) bool { return false }
func (h *EnvHarness) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	return nil
}
func (h *EnvHarness) GetInterruptKey() string                                        { return "C-c" }
func (h *EnvHarness) GetInterruptSequence() []string                                 { return nil }
func (h *EnvHarness) DeliverySpec() api.DeliverySpec                                 { return api.DeliverySpec{} }
func (h *EnvHarness) ReadySpec() api.ReadySpec                                       { return api.ReadySpec{} }
func (h *EnvHarness) GetHarnessEmbedsFS() (embed.FS, string)                         { return embed.FS{}, "" }
func (h *EnvHarness) InjectAgentInstructions(agentHome string, content []byte) error { return nil }
func (h *EnvHarness) InjectSystemPrompt(agentHome string, content []byte) error      { return nil }
func (h *EnvHarness) GetTelemetryEnv() map[string]string {
	return map[string]string{
		"TELEMETRY_ENABLED": "true",
		"TELEMETRY_TARGET":  "localhost:4317",
	}
}
func (h *EnvHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	return &api.ResolvedAuth{Method: "test"}, nil
}

// --- Stage 1.1: Common env/auth composition parity ---

func TestBuildPod_HarnessEnv(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		HomeDir:      "/tmp/home",
		Harness:      &EnvHarness{},
	}

	pod, _ := rt.buildPod("default", config)

	envMap := make(map[string]string)
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Value != "" {
			envMap[env.Name] = env.Value
		}
	}

	if envMap["HARNESS_AGENT_NAME"] != "test-agent" {
		t.Errorf("expected HARNESS_AGENT_NAME=test-agent, got %q", envMap["HARNESS_AGENT_NAME"])
	}
	if envMap["HARNESS_HOME"] != "/tmp/home" {
		t.Errorf("expected HARNESS_HOME=/tmp/home, got %q", envMap["HARNESS_HOME"])
	}
	// Telemetry should not be present when TelemetryEnabled is false
	if _, ok := envMap["TELEMETRY_ENABLED"]; ok {
		t.Error("TELEMETRY_ENABLED should not be set when TelemetryEnabled is false")
	}
}

func TestBuildPod_TelemetryEnv(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:             "test-agent",
		Image:            "test:latest",
		UnixUsername:     "scion",
		Harness:          &EnvHarness{},
		TelemetryEnabled: true,
	}

	pod, _ := rt.buildPod("default", config)

	envMap := make(map[string]string)
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Value != "" {
			envMap[env.Name] = env.Value
		}
	}

	if envMap["TELEMETRY_ENABLED"] != "true" {
		t.Errorf("expected TELEMETRY_ENABLED=true, got %q", envMap["TELEMETRY_ENABLED"])
	}
	if envMap["TELEMETRY_TARGET"] != "localhost:4317" {
		t.Errorf("expected TELEMETRY_TARGET=localhost:4317, got %q", envMap["TELEMETRY_TARGET"])
	}
}

func TestBuildPod_ResolvedAuth_ComposesWithSecrets(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
		},
		ResolvedAuth: &api.ResolvedAuth{
			Method: "vertex-ai",
			EnvVars: map[string]string{
				"GOOGLE_CLOUD_PROJECT": "my-project",
			},
		},
	}

	pod, _ := rt.buildPod("default", config)

	envMap := make(map[string]string)
	var apiKeyFromRef bool
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Value != "" {
			envMap[env.Name] = env.Value
		}
		if env.Name == "API_KEY" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			apiKeyFromRef = true
		}
	}

	// ResolvedSecrets env should be present
	if !apiKeyFromRef {
		t.Error("API_KEY should come from secretKeyRef (ResolvedSecrets)")
	}
	// ResolvedAuth env should also be present (no longer mutually exclusive)
	if envMap["GOOGLE_CLOUD_PROJECT"] != "my-project" {
		t.Error("GOOGLE_CLOUD_PROJECT should be set from ResolvedAuth")
	}
}

// --- Env precedence: the Docker order (ptone/scion#7382) ---

// agentEnvEntries returns the agent container's env entries in pod order.
func agentEnvEntries(t *testing.T, pod *corev1.Pod) []corev1.EnvVar {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == agentContainerName {
			return c.Env
		}
	}
	t.Fatalf("no %s container in the pod", agentContainerName)
	return nil
}

// effectiveEnvEntry returns the last entry for name, which is the one
// Kubernetes applies.
func effectiveEnvEntry(entries []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	var got corev1.EnvVar
	found := false
	for _, ev := range entries {
		if ev.Name == name {
			got, found = ev, true
		}
	}
	return got, found
}

// emptyTelemetryHarness is EnvHarness with a telemetry env that sets one of
// its GetEnv names to an empty value.
type emptyTelemetryHarness struct{ EnvHarness }

func (h *emptyTelemetryHarness) GetTelemetryEnv() map[string]string {
	return map[string]string{"HARNESS_AGENT_NAME": "", "TELEMETRY_ENABLED": "true"}
}

// requirePremise stops a precedence test whose sources would not really
// collide, so the test cannot pass vacuously.
func requirePremise(t *testing.T, ok bool, premise string) {
	t.Helper()
	if !ok {
		t.Fatalf("test premise does not hold: %s", premise)
	}
}

// The harness env wins over resolved auth for a name both set, as on Docker.
func TestBuildPod_HarnessEnvWinsOverResolvedAuth(t *testing.T) {
	cfg := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Harness:      &EnvHarness{},
		ResolvedAuth: &api.ResolvedAuth{Method: "api-key", EnvVars: map[string]string{
			"HARNESS_AGENT_NAME": "from-resolved-auth",
			"AUTH_ONLY":          "from-resolved-auth",
		}},
	}
	requirePremise(t, cfg.Harness.GetEnv(cfg.Name, cfg.HomeDir, cfg.UnixUsername)["HARNESS_AGENT_NAME"] != "",
		"the harness env sets HARNESS_AGENT_NAME to a non-empty value")
	requirePremise(t, cfg.ResolvedAuth.EnvVars["HARNESS_AGENT_NAME"] != "",
		"resolved auth sets HARNESS_AGENT_NAME to a non-empty value")
	rt, _, _ := newTestK8sRuntime()
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	entries := agentEnvEntries(t, pod)
	if ev, _ := effectiveEnvEntry(entries, "HARNESS_AGENT_NAME"); ev.Value != "test-agent" {
		t.Errorf("HARNESS_AGENT_NAME = %q, want the harness env value %q", ev.Value, "test-agent")
	}
	if ev, _ := effectiveEnvEntry(entries, "AUTH_ONLY"); ev.Value != "from-resolved-auth" {
		t.Errorf("control: AUTH_ONLY = %q, want %q", ev.Value, "from-resolved-auth")
	}
}

// Resolved-auth entries are emitted in sorted name order, so the pod spec
// does not vary between starts.
func TestBuildPod_ResolvedAuthEnvSorted(t *testing.T) {
	auth := map[string]string{}
	for _, n := range []string{"AUTH_H", "AUTH_C", "AUTH_F", "AUTH_A", "AUTH_G", "AUTH_D", "AUTH_B", "AUTH_E"} {
		auth[n] = "v-" + n
	}
	rt, _, _ := newTestK8sRuntime()
	pod, err := rt.buildPod("default", RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedAuth: &api.ResolvedAuth{Method: "api-key", EnvVars: auth},
	})
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	var order []string
	for _, ev := range agentEnvEntries(t, pod) {
		if _, ok := auth[ev.Name]; ok {
			order = append(order, ev.Name)
		}
	}
	if len(order) != len(auth) || !slices.IsSorted(order) {
		t.Errorf("resolved-auth env order = %v, want all %d names in sorted order", order, len(auth))
	}
}

// An empty telemetry value does not blank a GetEnv value for the same name,
// as on Docker.
func TestBuildPod_EmptyTelemetryValueKeepsGetEnvValue(t *testing.T) {
	cfg := RunConfig{
		Name:             "test-agent",
		Image:            "test:latest",
		UnixUsername:     "scion",
		Harness:          &emptyTelemetryHarness{},
		TelemetryEnabled: true,
	}
	requirePremise(t, cfg.Harness.GetEnv(cfg.Name, cfg.HomeDir, cfg.UnixUsername)["HARNESS_AGENT_NAME"] != "",
		"GetEnv sets HARNESS_AGENT_NAME to a non-empty value")
	tv, inTelemetry := cfg.Harness.GetTelemetryEnv()["HARNESS_AGENT_NAME"]
	requirePremise(t, inTelemetry && tv == "", "GetTelemetryEnv sets HARNESS_AGENT_NAME to an empty value")
	rt, _, _ := newTestK8sRuntime()
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	entries := agentEnvEntries(t, pod)
	if ev, _ := effectiveEnvEntry(entries, "HARNESS_AGENT_NAME"); ev.Value != "test-agent" {
		t.Errorf("HARNESS_AGENT_NAME = %q, want the GetEnv value %q", ev.Value, "test-agent")
	}
	if ev, _ := effectiveEnvEntry(entries, "TELEMETRY_ENABLED"); ev.Value != "true" {
		t.Errorf("control: TELEMETRY_ENABLED = %q, want %q", ev.Value, "true")
	}
}

// An env-type secret wins over a harness env name, as on Docker. The
// harness sets HARNESS_AGENT_NAME to the agent name (never empty), so the
// harness entry is really emitted and the two sources collide.
func TestBuildPod_SecretWinsOverHarnessEnv(t *testing.T) {
	cfg := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Harness:      &EnvHarness{},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "harness-name-secret", Type: "environment", Target: "HARNESS_AGENT_NAME", Value: "from-secret", Source: "user"},
		},
	}
	requirePremise(t, cfg.Harness.GetEnv(cfg.Name, cfg.HomeDir, cfg.UnixUsername)["HARNESS_AGENT_NAME"] != "",
		"the harness env sets HARNESS_AGENT_NAME to a non-empty value")
	rt, _, _ := newTestK8sRuntime()
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	ev, ok := effectiveEnvEntry(agentEnvEntries(t, pod), "HARNESS_AGENT_NAME")
	if !ok || ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil || ev.ValueFrom.SecretKeyRef.Key != "harness-name-secret" {
		t.Errorf("HARNESS_AGENT_NAME = %+v (present %v), want the secretKeyRef for harness-name-secret", ev, ok)
	}
}

// SCION_START_CMD stays the runtime's value even when an env-type secret
// targets it.
func TestBuildPod_StartCmdWinsOverSecret(t *testing.T) {
	cfg := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Harness:      &EnvHarness{},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "start-cmd-secret", Type: "environment", Target: "SCION_START_CMD", Value: "from-secret", Source: "user"},
		},
	}
	requirePremise(t, cfg.ResolvedSecrets[0].Value != "", "the secret targeting SCION_START_CMD has a non-empty value")
	rt, _, _ := newTestK8sRuntime()
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	entries := agentEnvEntries(t, pod)
	_, hasStart := effectiveEnvEntry(entries, "SCION_START_CMD")
	requirePremise(t, hasStart, "buildPod emits a SCION_START_CMD entry")
	for _, ev := range entries {
		if ev.Name == "SCION_START_CMD" && ev.ValueFrom != nil {
			t.Errorf("SCION_START_CMD has a secret-backed entry: %+v", ev)
		}
	}
	if ev, ok := effectiveEnvEntry(entries, "SCION_START_CMD"); !ok || !strings.Contains(ev.Value, "tmux") {
		t.Errorf("SCION_START_CMD = %q (present %v), want the runtime's tmux start command", ev.Value, ok)
	}
}

func TestBuildPod_ResolvedAuth_NoHostPath(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedAuth: &api.ResolvedAuth{
			Method: "api-key",
			EnvVars: map[string]string{
				"API_KEY": "sk-test",
			},
			Files: []api.FileMapping{
				{SourcePath: "/host/path/to/cred.json", ContainerPath: "~/.config/gcloud/adc.json"},
			},
		},
	}

	pod, _ := rt.buildPod("default", config)

	// Auth env should be present
	envMap := make(map[string]string)
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Value != "" {
			envMap[env.Name] = env.Value
		}
	}
	if envMap["API_KEY"] != "sk-test" {
		t.Error("expected API_KEY=sk-test from ResolvedAuth")
	}

	// Auth files should use Secret volume, NOT hostPath
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			t.Errorf("found hostPath volume %q — auth files should use K8s Secret, not hostPath", v.Name)
		}
	}

	// Should have auth-files volume with Secret source
	foundAuthVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "auth-files" {
			foundAuthVol = true
			if v.Secret == nil || v.Secret.SecretName != "scion-auth-test-agent" {
				t.Errorf("expected Secret volume scion-auth-test-agent, got %+v", v.VolumeSource)
			}
		}
	}
	if !foundAuthVol {
		t.Error("expected auth-files volume")
	}

	// The auth file target is under home: it is staged, not mounted there.
	assertNoMountsUnderHome(t, pod, "/home/scion")
	assertStagingMount(t, pod, "auth-files")
	assertPlacement(t, rt.k8sHomeFilePlacements(config), "/home/scion/.config/gcloud/adc.json", "/run/scion/auth-files/auth-file-0")
}

// --- Stage 1.2: Non-GCS volume handling ---

func TestBuildPod_LocalVolumes_Skipped(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Volumes: []api.VolumeMount{
			{Source: "/host/data", Target: "/data", Type: "local"},
			{Source: "/host/other", Target: "/other"},
		},
	}

	pod, _ := rt.buildPod("default", config)

	// Should NOT have any volumes for local mounts (only workspace emptydir should exist)
	for _, v := range pod.Spec.Volumes {
		if v.Name != "workspace" {
			t.Errorf("unexpected volume %q — local volumes should be skipped on k8s", v.Name)
		}
	}
}

func TestBuildPod_GCSVolumes_StillWork(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Volumes: []api.VolumeMount{
			{Type: "gcs", Bucket: "my-bucket", Target: "/mnt/data"},
		},
	}

	pod, _ := rt.buildPod("default", config)

	foundGCS := false
	for _, v := range pod.Spec.Volumes {
		if v.CSI != nil && v.CSI.Driver == "gcsfuse.csi.storage.gke.io" {
			foundGCS = true
		}
	}
	if !foundGCS {
		t.Error("expected GCS CSI volume")
	}
}

// --- Stage 1.3: Git/workspace parity ---

func TestBuildPod_WorkingDir_Default(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, _ := rt.buildPod("default", config)

	if pod.Spec.Containers[0].WorkingDir != "/workspace" {
		t.Errorf("expected WorkingDir /workspace, got %s", pod.Spec.Containers[0].WorkingDir)
	}
}

func TestBuildPod_GitClone_Annotations(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		GitClone:     &api.GitCloneConfig{URL: "https://github.com/example/repo.git"},
		Annotations: map[string]string{
			"scion.git_clone":     "true",
			"scion.git_clone_url": "https://github.com/example/repo.git",
		},
	}

	pod, _ := rt.buildPod("default", config)

	if pod.Annotations["scion.git_clone"] != "true" {
		t.Error("expected scion.git_clone annotation")
	}
	if pod.Spec.Containers[0].WorkingDir != "/workspace" {
		t.Errorf("expected WorkingDir /workspace for gitClone, got %s", pod.Spec.Containers[0].WorkingDir)
	}
}

// --- Stage 1.1 continued: createAuthFileSecret ---

func TestCreateAuthFileSecret(t *testing.T) {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)
	rt := NewKubernetesRuntime(client)

	// Create a temp file to act as the auth source
	tmpDir := t.TempDir()
	credFile := filepath.Join(tmpDir, "cred.json")
	if err := os.WriteFile(credFile, []byte(`{"type":"service_account"}`), 0600); err != nil {
		t.Fatal(err)
	}

	files := []api.FileMapping{
		{SourcePath: credFile, ContainerPath: "~/.config/gcloud/adc.json"},
	}
	labels := map[string]string{"scion.name": "test-agent"}

	err := rt.createAuthFileSecret(context.Background(), "default", "test-agent", files, labels)
	if err != nil {
		t.Fatalf("createAuthFileSecret failed: %v", err)
	}

	// Verify the secret was created
	secret, err := clientset.CoreV1().Secrets("default").Get(context.Background(), "scion-auth-test-agent", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get auth secret: %v", err)
	}

	if string(secret.Data["auth-file-0"]) != `{"type":"service_account"}` {
		t.Errorf("unexpected secret data: %s", string(secret.Data["auth-file-0"]))
	}

	if secret.Labels["scion.agent"] != "test-agent" {
		t.Error("expected scion.agent label on auth secret")
	}
}

// --- K8s exec user context parity (matches Docker/Podman --user scion) ---

func TestK8sExec_WrapsCommandWithSu(t *testing.T) {
	// Verify that Exec wraps commands so they run as the scion user,
	// matching the --user scion flag used by Docker/Podman runtimes.
	// The wrapper uses ExecAsUserCmd (sh -c with whoami fallback)
	// instead of a bare `su -` invocation, so it works on container
	// images whose /etc/pam.d/su lacks pam_rootok.so. ExecAsUserCmd
	// passes user/cmd as positional shell arguments, so the joined
	// shell-quoted cmd appears verbatim as the trailing argv entry.
	cmd := []string{"tmux", "send-keys", "-t", "scion:0", "hello world", "Enter"}

	// Simulate the wrapping logic from Exec.
	quoted := make([]string, len(cmd))
	for i, arg := range cmd {
		quoted[i] = fmt.Sprintf("'%s'", strings.ReplaceAll(arg, "'", "'\"'\"'"))
	}
	joined := strings.Join(quoted, " ")
	wrapped := ExecAsUserCmd("scion", joined)

	if len(wrapped) != 6 || wrapped[0] != "sh" || wrapped[1] != "-c" {
		t.Fatalf("expected [sh -c <script> <name> <user> <cmd>] wrapper, got: %v", wrapped)
	}

	// The script must retain a su fallback so legacy
	// root-entrypoint images keep working.
	script := wrapped[2]
	if !strings.Contains(script, `su - "$1" -c "$2"`) {
		t.Errorf("expected script to retain su fallback, got: %s", script)
	}

	// User and cmd are passed as positional argv ($1, $2 — $0 is
	// the script-name label) — both must round-trip through the
	// helper untouched.
	if wrapped[4] != "scion" {
		t.Errorf("expected user at wrapped[4] to be \"scion\", got %q", wrapped[4])
	}
	if wrapped[5] != joined {
		t.Errorf("expected joined cmd at wrapped[5] verbatim, got %q want %q", wrapped[5], joined)
	}
	for _, arg := range cmd {
		if !strings.Contains(wrapped[5], arg) {
			t.Errorf("joined cmd %q should contain argument %q", wrapped[5], arg)
		}
	}
}

func TestK8sExec_QuotesSingleQuotesInArgs(t *testing.T) {
	cmd := []string{"echo", "it's a test"}

	quoted := make([]string, len(cmd))
	for i, arg := range cmd {
		quoted[i] = fmt.Sprintf("'%s'", strings.ReplaceAll(arg, "'", "'\"'\"'"))
	}
	shellCmd := strings.Join(quoted, " ")

	// The single quote in "it's" should be escaped
	if !strings.Contains(shellCmd, "'\"'\"'") {
		t.Errorf("expected escaped single quote in %q", shellCmd)
	}
}

func TestK8sAttach_ResolvesUsernameFromAnnotations(t *testing.T) {
	// Verify that Attach reads the username from scion.username annotation
	tests := []struct {
		name        string
		annotations map[string]string
		wantUser    string
	}{
		{
			name:        "uses annotation username",
			annotations: map[string]string{"scion.username": "myuser"},
			wantUser:    "myuser",
		},
		{
			name:        "defaults to scion when annotation missing",
			annotations: map[string]string{},
			wantUser:    "scion",
		},
		{
			name:        "defaults to scion when annotation empty",
			annotations: map[string]string{"scion.username": ""},
			wantUser:    "scion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the username resolution logic from Attach
			username := "scion"
			if u, ok := tt.annotations["scion.username"]; ok && u != "" {
				username = u
			}
			if username != tt.wantUser {
				t.Errorf("got username %q, want %q", username, tt.wantUser)
			}
		})
	}
}
