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

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// uidPreconditionFakeRuntime implements both runtime.Runtime (embedded, so
// only the methods this test needs are overridden) and UIDPreconditionDeleter,
// recording every call.
type uidPreconditionFakeRuntime struct {
	runtime.Runtime
	deleted      []ResourceHandle
	deleteErr    error
	plainDeletes []string
}

func (f *uidPreconditionFakeRuntime) DeleteResource(ctx context.Context, handle ResourceHandle) error {
	f.deleted = append(f.deleted, handle)
	return f.deleteErr
}

func (f *uidPreconditionFakeRuntime) Delete(ctx context.Context, ref runtime.RunRef) error {
	f.plainDeletes = append(f.plainDeletes, ref.ID)
	return nil
}

func TestCleanupLaunch_PrefersUIDPrecondition(t *testing.T) {
	rt := &uidPreconditionFakeRuntime{}
	mgr := &AgentManager{Runtime: rt}

	handles := []ResourceHandle{
		{Kind: "secret", Namespace: "ns", Name: "scion-agent-foo", UID: "uid-1"},
		{Kind: "pod", Namespace: "ns", Name: "foo", UID: "uid-2"},
	}
	if err := mgr.CleanupLaunch(context.Background(), handles); err != nil {
		t.Fatalf("CleanupLaunch: %v", err)
	}
	if len(rt.deleted) != 2 {
		t.Fatalf("expected 2 UID-precondition deletes, got %d", len(rt.deleted))
	}
	if len(rt.plainDeletes) != 0 {
		t.Fatalf("expected no plain deletes when UIDPreconditionDeleter is available, got %v", rt.plainDeletes)
	}
}

// plainFakeRuntime implements only runtime.Runtime, not UIDPreconditionDeleter.
type plainFakeRuntime struct {
	runtime.Runtime
	deletes []string
}

func (f *plainFakeRuntime) Name() string { return "plain" }

func (f *plainFakeRuntime) Delete(ctx context.Context, ref runtime.RunRef) error {
	f.deletes = append(f.deletes, ref.ID)
	return nil
}

// TestCleanupLaunch_SkipsHandleWithoutUIDPrecondition covers design §3.8.4:
// a runtime with no UID-precondition delete must not fall back to an
// unconditional Delete(ctx, h.Name) -- that is exactly what the precondition
// exists to prevent -- so the handle is skipped and reported as an error.
func TestCleanupLaunch_SkipsHandleWithoutUIDPrecondition(t *testing.T) {
	rt := &plainFakeRuntime{}
	mgr := &AgentManager{Runtime: rt}

	handles := []ResourceHandle{{Kind: "container", Name: "container-1"}}
	err := mgr.CleanupLaunch(context.Background(), handles)
	if err == nil {
		t.Fatal("expected an error for a handle the runtime cannot UID-precondition delete")
	}
	if len(rt.deletes) != 0 {
		t.Fatalf("expected no unconditional delete-by-name, got %v", rt.deletes)
	}
}

func TestCleanupLaunch_JoinsErrorsButAttemptsEveryHandle(t *testing.T) {
	rt := &uidPreconditionFakeRuntime{deleteErr: errors.New("boom")}
	mgr := &AgentManager{Runtime: rt}

	handles := []ResourceHandle{
		{Kind: "secret", Name: "a"},
		{Kind: "pod", Name: "b"},
	}
	err := mgr.CleanupLaunch(context.Background(), handles)
	if err == nil {
		t.Fatal("expected a joined error")
	}
	if len(rt.deleted) != 2 {
		t.Fatalf("expected both handles attempted despite the first failing, got %d", len(rt.deleted))
	}
}

func TestCleanupLaunch_EmptyHandlesIsNoOp(t *testing.T) {
	rt := &plainFakeRuntime{}
	mgr := &AgentManager{Runtime: rt}
	if err := mgr.CleanupLaunch(context.Background(), nil); err != nil {
		t.Fatalf("CleanupLaunch(nil): %v", err)
	}
	if len(rt.deletes) != 0 {
		t.Fatalf("expected no deletes, got %v", rt.deletes)
	}
}
