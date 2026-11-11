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
	"errors"
	"testing"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
)

// TestSubstrateGetLogs_ReturnsSentinel_NoKubernetesOrAteapiCall pins the
// fail-closed contract for substrate logs: GetLogs returns
// ErrLogsNotSupported and makes no call at all — not to the Kubernetes API,
// not to ateapi (GetActor) — because reading a shared worker pod's logs
// would expose another tenant's actor output alongside the caller's own.
func TestSubstrateGetLogs_ReturnsSentinel_NoKubernetesOrAteapiCall(t *testing.T) {
	rec := &callRecorder{}
	fc := newFakeControlClient(rec)
	k8sClient := k8sfake.NewClientset()

	rt := NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient("http://example.invalid"), k8sClient, config.V1SubstrateConfig{
		SnapshotStorage:   "gs://bucket/prefix/",
		SandboxConfigName: "gvisor-default",
	})

	logs, err := rt.GetLogs(context.Background(), "scion-550e8400-e29/test-agent")

	if !errors.Is(err, ErrLogsNotSupported) {
		t.Fatalf("GetLogs() error = %v, want errors.Is(err, ErrLogsNotSupported)", err)
	}
	if logs != "" {
		t.Errorf("GetLogs() logs = %q, want empty alongside ErrLogsNotSupported", logs)
	}

	if calls := rec.list(); len(calls) != 0 {
		t.Errorf("GetLogs() made ateapi call(s) %v, want zero", calls)
	}
	if actions := k8sClient.Actions(); len(actions) != 0 {
		t.Errorf("GetLogs() made Kubernetes action(s) %v, want zero", actions)
	}
}

// TestSubstrateGetLogs_FixedMessage_NoTenantIdentifiers pins the exact,
// fixed error text: it names no atespace, worker, pod, namespace, actor or
// agent, even though the id passed in encodes an atespace/actor pair — the
// message must not vary with (or leak) its argument.
func TestSubstrateGetLogs_FixedMessage_NoTenantIdentifiers(t *testing.T) {
	rec := &callRecorder{}
	fc := newFakeControlClient(rec)
	rt := NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient("http://example.invalid"), k8sfake.NewClientset(), config.V1SubstrateConfig{
		SnapshotStorage:   "gs://bucket/prefix/",
		SandboxConfigName: "gvisor-default",
	})

	wantMessage := ErrLogsNotSupported.Error()

	_, err := rt.GetLogs(context.Background(), "scion-deadbeef0000/some-other-actor")
	if err == nil || err.Error() != wantMessage {
		t.Fatalf("GetLogs() error = %v, want exactly %q", err, wantMessage)
	}
}
