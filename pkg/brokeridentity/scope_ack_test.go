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

package brokeridentity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestVerifyScope_DockerDaemonChangeRefused(t *testing.T) {
	err := VerifyScope(dockerScope("D1", "unix:///var/run/docker.sock"), dockerScope("D2", "unix:///var/run/docker.sock"))
	if !errors.Is(err, ErrExecutionScopeChanged) {
		t.Fatalf("want ErrExecutionScopeChanged, got %v", err)
	}
}

func TestVerifyScope_DockerEndpointChangeSameDaemonAccepted(t *testing.T) {
	if err := VerifyScope(dockerScope("D1", "unix:///var/run/docker.sock"), dockerScope("D1", "tcp://10.0.0.5:2375")); err != nil {
		t.Fatalf("endpoint change with the same daemon must be accepted: %v", err)
	}
}

func TestVerifyScope_DockerEmptyDaemonIDIsError(t *testing.T) {
	err := VerifyScope(dockerScope("D1", "unix:///var/run/docker.sock"), dockerScope("", "unix:///var/run/docker.sock"))
	if !errors.Is(err, ErrExecutionScopeUnidentified) {
		t.Fatalf("want ErrExecutionScopeUnidentified, got %v", err)
	}
}

func TestVerifyScope_ExistingIdentityChecksObservedScope(t *testing.T) {
	dir := instanceDir(t)
	if _, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D2", ""), nil)
	if !errors.Is(err, ErrExecutionScopeChanged) {
		t.Fatalf("restart against another daemon must be refused, got %v", err)
	}
	// The recorded identity is unchanged.
	data, _ := os.ReadFile(filepath.Join(dir, IdentityFileName))
	id, err := readIdentity(filepath.Join(dir, IdentityFileName))
	if err != nil || id.ExecutionScope.Docker.DaemonID != "D1" {
		t.Fatalf("identity must not be retargeted: %s (%v)", data, err)
	}
}

func k8sScope(cluster, ns, server string) ExecutionScope {
	return ExecutionScope{Type: TargetTypeKubernetes, Kubernetes: &KubernetesScope{ClusterUID: cluster, Namespace: ns, APIServer: server}}
}

func TestVerifyScope_KubernetesIgnoresContextAliasAndCredentials(t *testing.T) {
	// Context aliases and credentials are not part of the record at all;
	// an API server URL change with the same cluster and namespace is the
	// same target.
	if err := VerifyScope(k8sScope("uid-1", "agents", NormalizeKubernetesAPIServer("https://A.example:443/")),
		k8sScope("uid-1", "agents", NormalizeKubernetesAPIServer("https://b.example"))); err != nil {
		t.Fatalf("same cluster and namespace must be accepted: %v", err)
	}
}

func TestVerifyScope_KubernetesClusterOrNamespaceChangeRefused(t *testing.T) {
	if err := VerifyScope(k8sScope("uid-1", "agents", ""), k8sScope("uid-2", "agents", "")); !errors.Is(err, ErrExecutionScopeChanged) {
		t.Fatalf("cluster change must be refused, got %v", err)
	}
	if err := VerifyScope(k8sScope("uid-1", "agents", ""), k8sScope("uid-1", "other", "")); !errors.Is(err, ErrExecutionScopeChanged) {
		t.Fatalf("namespace change must be refused, got %v", err)
	}
}

func TestNormalizeDockerEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"unix:///var/run//docker.sock": "unix:///var/run/docker.sock",
		"TCP://Host.Example":           "tcp://host.example:2375",
		"tcp://h:1234":                 "tcp://h:1234",
		"":                             "",
	} {
		if got := NormalizeDockerEndpoint(in); got != want {
			t.Errorf("NormalizeDockerEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func ackIdentity() *Identity {
	return &Identity{
		SchemaVersion:   SchemaVersion,
		InstanceKey:     "local-docker",
		RuntimeBrokerID: "b-1",
		RuntimeTarget:   api.RuntimeTargetDescriptor{ID: "t-1", Type: TargetTypeDocker},
		ExecutionScope:  dockerScope("D1", ""),
	}
}

func ackCode(t *testing.T, err error) (string, string) {
	t.Helper()
	var ae *AckError
	if !errors.As(err, &ae) {
		t.Fatalf("want *AckError, got %v", err)
	}
	return ae.Code, ae.Phase
}

func TestCheckActivationAck_MissingRegistrationAck(t *testing.T) {
	code, phase := ackCode(t, CheckActivationAck(ackIdentity(), PhaseRegister, "b-1", nil))
	if code != api.ErrCodeRuntimeTargetAckMissing || phase != PhaseRegister {
		t.Fatalf("got %s/%s", code, phase)
	}
}

func TestCheckActivationAck_MismatchedRegistrationAck(t *testing.T) {
	// Name-adoption fixture: another row's ID and no runtimeTarget.
	code, _ := ackCode(t, CheckActivationAck(ackIdentity(), PhaseRegister, "legacy-row", nil))
	if code != api.ErrCodeRuntimeTargetBindingConflict {
		t.Fatalf("name adoption: got %s", code)
	}
	code, _ = ackCode(t, CheckActivationAck(ackIdentity(), PhaseRegister, "b-1", &api.RuntimeTargetDescriptor{ID: "t-2", Type: TargetTypeDocker}))
	if code != api.ErrCodeRuntimeTargetBindingConflict {
		t.Fatalf("target mismatch: got %s", code)
	}
}

func TestCheckActivationAck_MissingJoinAck(t *testing.T) {
	code, phase := ackCode(t, CheckActivationAck(ackIdentity(), PhaseJoin, "b-1", nil))
	if code != api.ErrCodeRuntimeTargetAckMissing || phase != PhaseJoin {
		t.Fatalf("got %s/%s", code, phase)
	}
}

func TestCheckActivationAck_MismatchedJoinAck(t *testing.T) {
	code, _ := ackCode(t, CheckActivationAck(ackIdentity(), PhaseJoin, "b-1", &api.RuntimeTargetDescriptor{ID: "t-1", Type: TargetTypeKubernetes}))
	if code != api.ErrCodeRuntimeTargetBindingConflict {
		t.Fatalf("type mismatch: got %s", code)
	}
	code, _ = ackCode(t, CheckActivationAck(ackIdentity(), PhaseJoin, "other", &api.RuntimeTargetDescriptor{ID: "t-1", Type: TargetTypeDocker}))
	if code != api.ErrCodeRuntimeTargetBindingConflict {
		t.Fatalf("ID mismatch: got %s", code)
	}
}

func TestAckError_Fields(t *testing.T) {
	got := &api.RuntimeTargetDescriptor{ID: "t-9", Type: TargetTypeDocker}
	var ae *AckError
	if !errors.As(CheckActivationAck(ackIdentity(), PhaseActivate, "b-1", got), &ae) {
		t.Fatal("want *AckError")
	}
	if ae.Code != api.ErrCodeRuntimeTargetBindingConflict || ae.Phase != PhaseActivate || ae.RuntimeBrokerID != "b-1" {
		t.Fatalf("fields: %+v", ae)
	}
	if ae.Expected.RuntimeBrokerID != "b-1" || ae.Expected.RuntimeTarget == nil || ae.Expected.RuntimeTarget.ID != "t-1" {
		t.Fatalf("expected binding: %+v", ae.Expected)
	}
	if ae.Got.RuntimeBrokerID != "b-1" || ae.Got.RuntimeTarget != got {
		t.Fatalf("got binding: %+v", ae.Got)
	}
}

func TestCheckActivationAck_DistinctCodes(t *testing.T) {
	if api.ErrCodeRuntimeTargetAckMissing == api.ErrCodeRuntimeTargetBindingConflict {
		t.Fatal("ack codes must be distinct")
	}
	if api.ErrCodeRuntimeTargetAckMissing != "runtime_target_ack_missing" ||
		api.ErrCodeRuntimeTargetBindingConflict != "runtime_target_binding_conflict" {
		t.Fatal("frozen ack code values changed")
	}
}

func TestCheckActivationAck_EvaluationOrder(t *testing.T) {
	// (1) ID first, even with no target.
	code, _ := ackCode(t, CheckActivationAck(ackIdentity(), PhaseRegister, "x", nil))
	if code != api.ErrCodeRuntimeTargetBindingConflict {
		t.Fatalf("ID must be checked first: got %s", code)
	}
	// (2) then the missing target.
	code, _ = ackCode(t, CheckActivationAck(ackIdentity(), PhaseRegister, "b-1", nil))
	if code != api.ErrCodeRuntimeTargetAckMissing {
		t.Fatalf("missing target second: got %s", code)
	}
	// (3) then the target mismatch.
	code, _ = ackCode(t, CheckActivationAck(ackIdentity(), PhaseRegister, "b-1", &api.RuntimeTargetDescriptor{ID: "t-9", Type: TargetTypeDocker}))
	if code != api.ErrCodeRuntimeTargetBindingConflict {
		t.Fatalf("target mismatch third: got %s", code)
	}
	// Bound result.
	if err := CheckActivationAck(ackIdentity(), PhaseRegister, "b-1", &api.RuntimeTargetDescriptor{ID: "t-1", Type: TargetTypeDocker, DisplayName: "any"}); err != nil {
		t.Fatalf("bound result must pass (display name is not identity): %v", err)
	}
}

func TestCheckActivationAck_AllPhases(t *testing.T) {
	for _, phase := range []string{PhaseRegister, PhaseJoin, PhaseEmbedded, PhaseActivate} {
		code, got := ackCode(t, CheckActivationAck(ackIdentity(), phase, "b-1", nil))
		if code != api.ErrCodeRuntimeTargetAckMissing || got != phase {
			t.Fatalf("phase %s: got %s/%s", phase, code, got)
		}
		code, _ = ackCode(t, CheckActivationAck(ackIdentity(), phase, "other", nil))
		if code != api.ErrCodeRuntimeTargetBindingConflict {
			t.Fatalf("phase %s: got %s", phase, code)
		}
		code, _ = ackCode(t, CheckActivationAck(ackIdentity(), phase, "b-1", &api.RuntimeTargetDescriptor{ID: "t-9", Type: TargetTypeDocker}))
		if code != api.ErrCodeRuntimeTargetBindingConflict {
			t.Fatalf("phase %s target mismatch: got %s", phase, code)
		}
		if err := CheckActivationAck(ackIdentity(), phase, "b-1", &api.RuntimeTargetDescriptor{ID: "t-1", Type: TargetTypeDocker}); err != nil {
			t.Fatalf("phase %s bound: %v", phase, err)
		}
	}
}

// TestCheckActivationAck_MixedReplicaSequence: registration acknowledged by a
// capable replica, then a join served by an unaware replica.
func TestCheckActivationAck_MixedReplicaSequence(t *testing.T) {
	id := ackIdentity()
	if err := CheckActivationAck(id, PhaseRegister, "b-1", &api.RuntimeTargetDescriptor{ID: "t-1", Type: TargetTypeDocker}); err != nil {
		t.Fatalf("register: %v", err)
	}
	code, phase := ackCode(t, CheckActivationAck(id, PhaseJoin, "b-1", nil))
	if code != api.ErrCodeRuntimeTargetAckMissing || phase != PhaseJoin {
		t.Fatalf("join from unaware replica: got %s/%s", code, phase)
	}
}

func TestAckError_MessagesNameTheBroker(t *testing.T) {
	err := CheckActivationAck(ackIdentity(), PhaseJoin, "b-1", nil)
	if msg := err.Error(); !contains(msg, "b-1") || !contains(msg, "runtime_target_ack_missing") {
		t.Fatalf("message: %s", msg)
	}
	err = CheckActivationAck(ackIdentity(), PhaseRegister, "legacy-row", nil)
	if msg := err.Error(); !contains(msg, "legacy-row") || !contains(msg, "runtime_target_binding_conflict") {
		t.Fatalf("message: %s", msg)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
