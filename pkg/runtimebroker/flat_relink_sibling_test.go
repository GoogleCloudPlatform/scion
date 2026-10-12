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

package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// relinkSiblings sets up the re-link fixture's files and two flat instances
// of one host sharing a workspace lock service. Instance B holds a live
// ownership record (and a running object) in projectID; instance A lists
// only its own objects, so it cannot see B's agent through its manager.
func relinkSiblings(t *testing.T, projectID string) (*relinkFixture, *partitionInstance) {
	t.Helper()
	f := newRelinkFixture(t) // sets HOME; the instances below use the same global directory
	d := &sharedDaemon{}
	locks := NewWorkspaceLocks()
	a := newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks)
	b := newPartitionInstanceWithLocks(t, d, "docker-b", t.TempDir(), locks)
	b.own(t, d, projectID, "agent-b", "worker-b", "cid-b")
	return f, a
}

// TestFlatRelink_SiblingInstanceAgentOfOldIDRefuses: on a host with several
// flat instances, a re-link on instance A refuses to move the earlier
// project config directory while instance B has a live agent of the earlier
// project ID: 409, both directories and the marker unchanged.
func TestFlatRelink_SiblingInstanceAgentOfOldIDRefuses(t *testing.T) {
	f, a := relinkSiblings(t, relinkOldID)

	err := a.srv.relinkSharedProjectIdentity(context.Background(), f.root, "", relinkNewID, relinkAgent)
	var relinkErr *projectRelinkError
	if !errors.As(err, &relinkErr) || !strings.Contains(err.Error(), "running") {
		t.Fatalf("expected an in-use projectRelinkError, got %v", err)
	}
	if got := projectRelinkStartContextError(err).Status; got != http.StatusConflict {
		t.Fatalf("status = %d, want %d", got, http.StatusConflict)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker rewritten to %q", got)
	}
}

// TestFlatRelink_SiblingInstanceAgentOfOtherProjectDoesNotBlock: a sibling
// instance's live agent in another project does not block the re-link.
func TestFlatRelink_SiblingInstanceAgentOfOtherProjectDoesNotBlock(t *testing.T) {
	f, a := relinkSiblings(t, relinkOtherID)

	if err := a.srv.relinkSharedProjectIdentity(context.Background(), f.root, "", relinkNewID, relinkAgent); err != nil {
		t.Fatalf("relink: %v", err)
	}
	assertState(t, f.newRoot)
	assertAbsent(t, f.oldRoot)
	if got := f.markerID(t); got != relinkNewID {
		t.Fatalf("marker = %q, want %q", got, relinkNewID)
	}
}

// TestFlatOtherProjectAgentsInUse_CountsSiblingInstances: for a flat
// instance, a project in which another instance of the host has live agents
// is in use (the hub identity record and the re-link both ask this); a
// project no instance uses is not.
func TestFlatOtherProjectAgentsInUse_CountsSiblingInstances(t *testing.T) {
	_, a := relinkSiblings(t, relinkOldID)
	ctx := context.Background()

	if busy, err := a.srv.otherProjectAgentsInUse(ctx, relinkOldID, ""); err != nil || !busy {
		t.Fatalf("project with a sibling's live agent: busy=%v err=%v, want busy", busy, err)
	}
	if busy, err := a.srv.otherProjectAgentsInUse(ctx, relinkOtherID, ""); err != nil || busy {
		t.Fatalf("unused project: busy=%v err=%v, want not busy", busy, err)
	}
}

// TestFlatRelink_UnreadableSiblingRefuses: another instance of the host
// whose agents cannot be read counts as using the project (fail closed), so
// the re-link refuses (409) and leaves both directories unchanged.
func TestFlatRelink_UnreadableSiblingRefuses(t *testing.T) {
	f := newRelinkFixture(t)
	d := &sharedDaemon{}
	locks := NewWorkspaceLocks()
	a := newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks)
	locks.registerWorkspaceUser(&Server{}, &workspaceUser{instance: "unreadable-sibling",
		projectInUse: func(string) (bool, error) { return false, errors.New("ownership records cannot be read") }})

	if busy, err := a.srv.otherProjectAgentsInUse(context.Background(), relinkOldID, ""); err != nil || !busy {
		t.Fatalf("unreadable sibling: busy=%v err=%v, want busy", busy, err)
	}
	err := a.srv.relinkSharedProjectIdentity(context.Background(), f.root, "", relinkNewID, relinkAgent)
	var relinkErr *projectRelinkError
	if !errors.As(err, &relinkErr) {
		t.Fatalf("expected a projectRelinkError, got %v", err)
	}
	if got := projectRelinkStartContextError(err).Status; got != http.StatusConflict {
		t.Fatalf("status = %d, want %d", got, http.StatusConflict)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker rewritten to %q", got)
	}
}
