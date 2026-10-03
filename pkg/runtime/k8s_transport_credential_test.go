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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testTransportValue    = "transport-value-1"
	testTransportValueNew = "transport-value-2"
	testAgentSecretName   = "scion-agent-cred-agent"
)

// newTransportTestRuntime returns a K8s runtime backed by fake clients that
// understand SecretProviderClass objects (needed for the GKE path).
func newTransportTestRuntime(gke bool) (*KubernetesRuntime, *k8sfake.Clientset) {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClass"},
		&k8sruntime.Unknown{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClassList"},
		&k8sruntime.Unknown{},
	)
	dynClient := fake.NewSimpleDynamicClient(scheme)
	rt := NewKubernetesRuntime(k8s.NewTestClient(dynClient, clientset))
	rt.GKEMode = gke
	return rt, clientset
}

// runUntilPodCreated drives Run until the pod Create call, then cancels the
// context so waitForPodReady returns immediately. It returns the pod spec that
// was submitted to the API server and the per-agent Secret as it existed at
// that moment (nil if none).
func runUntilPodCreated(t *testing.T, rt *KubernetesRuntime, clientset *k8sfake.Clientset, config RunConfig) (*corev1.Pod, *corev1.Secret) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu         sync.Mutex
		createdPod *corev1.Pod
		secretSnap *corev1.Secret
	)
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		mu.Lock()
		createdPod = pod.DeepCopy()
		if s, err := clientset.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), pod.Namespace, "scion-agent-"+config.Name); err == nil {
			secretSnap = s.(*corev1.Secret).DeepCopy()
		}
		mu.Unlock()
		cancel()
		return false, nil, nil
	})

	_, _ = rt.Run(ctx, config)

	mu.Lock()
	defer mu.Unlock()
	if createdPod == nil {
		t.Fatal("Run did not submit a pod")
	}
	return createdPod, secretSnap
}

func transportEnvEntries(pod *corev1.Pod) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == transportauth.EnvTransportToken {
			out = append(out, e)
		}
	}
	return out
}

func assertTransportViaSecretKeyRef(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	entries := transportEnvEntries(pod)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one %s env entry, got %d: %+v", transportauth.EnvTransportToken, len(entries), entries)
	}
	e := entries[0]
	if e.Value != "" {
		t.Errorf("%s must not carry a plain Value, got %q", e.Name, e.Value)
	}
	if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s must use ValueFrom.SecretKeyRef, got %+v", e.Name, e.ValueFrom)
	}
	if got := e.ValueFrom.SecretKeyRef.Name; got != testAgentSecretName {
		t.Errorf("secretKeyRef name = %q, want %q", got, testAgentSecretName)
	}
	if got := e.ValueFrom.SecretKeyRef.Key; got != transportCredentialSecretKey {
		t.Errorf("secretKeyRef key = %q, want %q", got, transportCredentialSecretKey)
	}
	// The value must not appear anywhere else in the container env either.
	for _, ev := range pod.Spec.Containers[0].Env {
		if strings.Contains(ev.Value, testTransportValue) || strings.Contains(ev.Value, testTransportValueNew) {
			t.Errorf("credential value found in plain env entry %s", ev.Name)
		}
	}
}

// baseTransportConfig carries the labels the agent manager sets in
// production (scion.agent=true, scion.name=<name>), so the per-agent Secret
// gets the same labels it does on a real cluster.
func baseTransportConfig(env ...string) RunConfig {
	return RunConfig{
		Name:         "cred-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Env:          env,
		Labels: map[string]string{
			"scion.agent": "true",
			"scion.name":  "cred-agent",
		},
	}
}

// No other secrets: the Secret is still created and holds only the credential.
func TestRun_TransportCredential_NoOtherSecrets(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	config := baseTransportConfig(
		"SCION_HUB_ENDPOINT=https://hub.example",
		transportauth.EnvTransportToken+"="+testTransportValue,
		transportauth.EnvTransportTokenExpiry+"=2026-10-01T13:00:00Z",
	)

	pod, secret := runUntilPodCreated(t, rt, clientset, config)

	assertTransportViaSecretKeyRef(t, pod)
	if secret == nil {
		t.Fatal("expected per-agent Secret to exist when the pod is created")
	}
	if got := string(secret.Data[transportCredentialSecretKey]); got != testTransportValue {
		t.Errorf("Secret[%s] = %q, want %q", transportCredentialSecretKey, got, testTransportValue)
	}
	if len(secret.Data) != 1 {
		t.Errorf("expected Secret to hold only the credential, got keys %v", reflect.ValueOf(secret.Data).MapKeys())
	}

	// Other env entries stay plain, including the expiry.
	plain := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.ValueFrom == nil {
			plain[e.Name] = e.Value
		}
	}
	if plain["SCION_HUB_ENDPOINT"] != "https://hub.example" {
		t.Errorf("SCION_HUB_ENDPOINT should remain plain, got %q", plain["SCION_HUB_ENDPOINT"])
	}
	if plain[transportauth.EnvTransportTokenExpiry] != "2026-10-01T13:00:00Z" {
		t.Errorf("%s should remain plain", transportauth.EnvTransportTokenExpiry)
	}
}

// Alongside existing secrets: both are in the same Secret and both are refs.
func TestRun_TransportCredential_WithOtherSecrets(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	config := baseTransportConfig(transportauth.EnvTransportToken + "=" + testTransportValue)
	config.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
	}

	pod, secret := runUntilPodCreated(t, rt, clientset, config)

	assertTransportViaSecretKeyRef(t, pod)
	if secret == nil {
		t.Fatal("expected per-agent Secret")
	}
	if string(secret.Data["API_KEY"]) != "sk-123" {
		t.Errorf("user secret missing from Secret")
	}
	if string(secret.Data[transportCredentialSecretKey]) != testTransportValue {
		t.Errorf("credential missing from Secret")
	}
}

// Restart/resume both re-enter Run with a freshly minted value; the Secret
// must carry the new value and the pod must still reference it. The first
// run's Secret does not carry over: Run removes it, both when that start is
// cancelled and in the pre-clean of the second run.
func TestRun_TransportCredential_SecondRunReplacesValue(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)

	_, first := runUntilPodCreated(t, rt, clientset, baseTransportConfig(transportauth.EnvTransportToken+"="+testTransportValue))
	if first == nil || string(first.Data[transportCredentialSecretKey]) != testTransportValue {
		t.Fatalf("first run: Secret does not hold the first value: %+v", first)
	}

	resume := baseTransportConfig(transportauth.EnvTransportToken + "=" + testTransportValueNew)
	resume.Resume = true
	pod, second := runUntilPodCreated(t, rt, clientset, resume)

	assertTransportViaSecretKeyRef(t, pod)
	if second == nil {
		t.Fatal("second run: expected per-agent Secret")
	}
	if got := string(second.Data[transportCredentialSecretKey]); got != testTransportValueNew {
		t.Errorf("second run: Secret[%s] = %q, want %q", transportCredentialSecretKey, got, testTransportValueNew)
	}
}

// GKE CSI path: credential goes to the K8s Secret (not the SecretProviderClass)
// and the pod references it via secretKeyRef.
func TestRun_TransportCredential_GKEPath(t *testing.T) {
	rt, clientset := newTransportTestRuntime(true)
	config := baseTransportConfig(transportauth.EnvTransportToken + "=" + testTransportValue)
	config.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user", Ref: "projects/p/secrets/api-key"},
		{Name: "TLS_CERT", Type: "file", Target: "/etc/ssl/cert.pem", Value: "cert-data", Source: "user", Ref: "projects/p/secrets/tls-cert"},
	}

	// Read the SecretProviderClass as it exists when the pod is created:
	// runUntilPodCreated cancels the start at that point, and Run then
	// removes the objects of that start, the SecretProviderClass included.
	var (
		spcMu   sync.Mutex
		spc     *unstructured.Unstructured
		spcErr  error
		spcRead bool
	)
	clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		got, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(context.Background(), testAgentSecretName, metav1.GetOptions{})
		spcMu.Lock()
		spc, spcErr, spcRead = got, err, true
		spcMu.Unlock()
		return false, nil, nil
	})

	pod, secret := runUntilPodCreated(t, rt, clientset, config)

	assertTransportViaSecretKeyRef(t, pod)
	hasCSI := false
	for _, v := range pod.Spec.Volumes {
		if v.CSI != nil {
			hasCSI = true
		}
	}
	if !hasCSI {
		t.Fatal("expected the GKE CSI path to be taken")
	}
	if secret == nil || string(secret.Data[transportCredentialSecretKey]) != testTransportValue {
		t.Fatalf("expected credential in the K8s Secret on the GKE path, got %+v", secret)
	}
	if _, ok := secret.Data["TLS_CERT"]; ok {
		t.Error("file-type secret must not be copied into the K8s Secret on the GKE path")
	}

	spcMu.Lock()
	defer spcMu.Unlock()
	if !spcRead {
		t.Fatal("SecretProviderClass was not read at pod creation")
	}
	if spcErr != nil {
		t.Fatalf("expected SecretProviderClass: %v", spcErr)
	}
	params, _, _ := unstructured.NestedString(spc.Object, "spec", "parameters", "secrets")
	if strings.Contains(params, transportCredentialSecretKey) || strings.Contains(params, testTransportValue) {
		t.Errorf("credential must not be referenced by the SecretProviderClass: %s", params)
	}
}

// GKE mode with no Ref secrets: the credential alone must not switch on CSI.
func TestRun_TransportCredential_GKEModeNoRefs(t *testing.T) {
	rt, clientset := newTransportTestRuntime(true)
	pod, secret := runUntilPodCreated(t, rt, clientset, baseTransportConfig(transportauth.EnvTransportToken+"="+testTransportValue))

	assertTransportViaSecretKeyRef(t, pod)
	for _, v := range pod.Spec.Volumes {
		if v.CSI != nil {
			t.Error("credential alone must not enable the CSI volume")
		}
	}
	if secret == nil || string(secret.Data[transportCredentialSecretKey]) != testTransportValue {
		t.Fatalf("expected credential in the K8s Secret, got %+v", secret)
	}
}

// Absent credential: no Secret is created and the pod is unchanged.
func TestRun_TransportCredential_Absent(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	config := baseTransportConfig("SCION_HUB_ENDPOINT=https://hub.example")

	pod, secret := runUntilPodCreated(t, rt, clientset, config)

	if secret != nil {
		t.Errorf("no Secret expected when the credential is absent, got %+v", secret.Data)
	}
	if n := len(transportEnvEntries(pod)); n != 0 {
		t.Errorf("expected no %s env entry, got %d", transportauth.EnvTransportToken, n)
	}

	want, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want.Spec.Containers[0].Env, pod.Spec.Containers[0].Env) {
		t.Errorf("pod env changed when the credential is absent:\n got: %+v\nwant: %+v", pod.Spec.Containers[0].Env, want.Spec.Containers[0].Env)
	}
}

func TestDivertTransportCredential_Absent(t *testing.T) {
	env := []string{"A=1", transportauth.EnvTransportToken + "="}
	secrets := []api.ResolvedSecret{{Name: "X", Type: "environment", Target: transportauth.EnvTransportToken, Value: "user"}}

	gotEnv, gotSecrets := divertTransportCredential(env, secrets)

	if !reflect.DeepEqual(gotEnv, env) || !reflect.DeepEqual(gotSecrets, secrets) {
		t.Errorf("inputs must be returned unchanged when no credential value is present: env=%v secrets=%v", gotEnv, gotSecrets)
	}
}

// Conflict rule: the hub value wins; conflicting resolved secrets are dropped.
func TestDivertTransportCredential_ConflictsDropped(t *testing.T) {
	env := []string{"A=1", transportauth.EnvTransportToken + "=" + testTransportValue, "B=2"}
	secrets := []api.ResolvedSecret{
		{Name: "KEEP", Type: "environment", Target: "KEEP", Value: "k", Source: "user"},
		{Name: "USER_TRANSPORT", Type: "environment", Target: transportauth.EnvTransportToken, Value: "user-value", Source: "user"},
		{Name: transportCredentialSecretKey, Type: "file", Target: "/tmp/x", Value: "f", Source: "project"},
		{Name: "FILE_SAME_TARGET_NAME", Type: "file", Target: transportauth.EnvTransportToken, Value: "f2", Source: "project"},
	}
	envCopy := append([]string(nil), env...)
	secretsCopy := append([]api.ResolvedSecret(nil), secrets...)

	gotEnv, gotSecrets := divertTransportCredential(env, secrets)

	if !reflect.DeepEqual(gotEnv, []string{"A=1", "B=2"}) {
		t.Errorf("env = %v, want credential removed", gotEnv)
	}
	var names []string
	for _, s := range gotSecrets {
		names = append(names, s.Name)
	}
	wantNames := []string{"KEEP", "FILE_SAME_TARGET_NAME", transportCredentialSecretKey}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("secret names = %v, want %v", names, wantNames)
	}
	last := gotSecrets[len(gotSecrets)-1]
	if last.Type != "environment" || last.Target != transportauth.EnvTransportToken || last.Value != testTransportValue {
		t.Errorf("unexpected credential entry: %+v", last)
	}
	if !reflect.DeepEqual(env, envCopy) || !reflect.DeepEqual(secrets, secretsCopy) {
		t.Error("caller slices were modified")
	}
}

func TestDivertTransportCredential_LastNonEmptyWins(t *testing.T) {
	env := []string{
		transportauth.EnvTransportToken + "=" + testTransportValue,
		transportauth.EnvTransportToken + "=" + testTransportValueNew,
		transportauth.EnvTransportToken + "=",
	}
	gotEnv, gotSecrets := divertTransportCredential(env, nil)
	if len(gotEnv) != 0 {
		t.Errorf("all credential entries must leave env, got %v", gotEnv)
	}
	if len(gotSecrets) != 1 || gotSecrets[0].Value != testTransportValueNew {
		t.Errorf("expected single entry with the last non-empty value, got %+v", gotSecrets)
	}
}

// A conflicting user secret that targets the same variable must not produce a
// second env entry in the pod spec.
func TestRun_TransportCredential_ConflictSingleEnvEntry(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	config := baseTransportConfig(transportauth.EnvTransportToken + "=" + testTransportValue)
	config.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "USER_TRANSPORT", Type: "environment", Target: transportauth.EnvTransportToken, Value: "user-value", Source: "user"},
	}

	pod, secret := runUntilPodCreated(t, rt, clientset, config)

	assertTransportViaSecretKeyRef(t, pod)
	if secret == nil {
		t.Fatal("expected per-agent Secret")
	}
	if _, ok := secret.Data["USER_TRANSPORT"]; ok {
		t.Error("conflicting user secret must be dropped from the Secret")
	}
	if string(secret.Data[transportCredentialSecretKey]) != testTransportValue {
		t.Error("hub-provided value must win")
	}
}

// Docker/Podman: the shared run-args builder is unchanged and still passes
// the variable through as -e KEY=VALUE.
func TestBuildCommonRunArgs_TransportCredentialUnchanged(t *testing.T) {
	cfg := RunConfig{
		Name:         "cred-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Harness:      &harness.Generic{},
		Env:          []string{transportauth.EnvTransportToken + "=" + testTransportValue},
	}
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := transportauth.EnvTransportToken + "=" + testTransportValue
	found := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" && args[i+1] == want {
			found = true
		}
	}
	if !found {
		t.Errorf("expected -e %s in docker args, got %v", want, args)
	}
}
