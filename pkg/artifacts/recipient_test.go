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

package artifacts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recipientHost is fakeHost plus RecipientAuthority, answered from the same
// tables for a named principal rather than for the caller of ctx.
type recipientHost struct {
	*fakeHost
	// err, when set, makes every RecipientAuthority call fail.
	err error
	// asked records "permits|authorize principalRef scope" per call.
	asked []string
}

func (h *recipientHost) RecipientPermits(_ context.Context, kind, ref, scope, perm string) (bool, error) {
	h.asked = append(h.asked, "permits "+PrincipalRef(kind, ref)+" "+scope)
	if h.err != nil {
		return false, h.err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return scope != "" && !h.denied[PrincipalRef(kind, ref)+" "+scope+" "+perm], nil
}

func (h *recipientHost) RecipientAuthorize(_ context.Context, kind, ref, scope, perm string) (bool, error) {
	h.asked = append(h.asked, "authorize "+PrincipalRef(kind, ref)+" "+scope)
	if h.err != nil {
		return false, h.err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.perms[PrincipalRef(kind, ref)][scope][perm], nil
}

// newRecipientFixture is newFixture with a host that implements
// RecipientAuthority.
func newRecipientFixture(t *testing.T) (*fixture, *recipientHost) {
	t.Helper()
	f := newFixture(t, false)
	h := &recipientHost{fakeHost: f.host}
	svc := NewService(h)
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	f.svc = svc
	return f, h
}

func (f *fixture) grantScope(id, scope string) {
	f.t.Helper()
	if _, err := f.db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES (?, ?, 'scope', ?, 'read', ?)`, "gs-"+id+"-"+scope, id, scope,
		time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) mayNotRead(refs []MessageRef, p principal) int {
	f.t.Helper()
	n, err := f.svc.RecipientMayNotRead(context.Background(), refs, p.kind, p.ref)
	if err != nil {
		f.t.Fatalf("RecipientMayNotRead(%s): %v", p.ref, err)
	}
	return n
}

// TestRecipientMayNotReadRules: each of canReadWith's rules, applied to a
// named recipient instead of the caller.
func TestRecipientMayNotReadRules(t *testing.T) {
	f, h := newRecipientFixture(t)
	id := f.publish(agentA, "design.md", []byte("# v1"), "title=Design").Artifact.ID
	refs := []MessageRef{{ArtifactID: id}}

	// Home-scope policy: a project-1 agent can read; a project-2 agent
	// and an unrelated user cannot.
	if n := f.mayNotRead(refs, agentB); n != 0 {
		t.Errorf("home-project agent: %d, want 0", n)
	}
	if n := f.mayNotRead(refs, agentX); n != 1 {
		t.Errorf("other-project agent: %d, want 1", n)
	}
	if n := f.mayNotRead(refs, outside); n != 1 {
		t.Errorf("unrelated user: %d, want 1", n)
	}

	// Owner, even without any policy.
	h.perms[PrincipalRef(agentA.kind, agentA.ref)] = nil
	if n := f.mayNotRead(refs, agentA); n != 0 {
		t.Errorf("owner: %d, want 0", n)
	}

	// The recipient's credential (its next token's scopes) refuses reads:
	// warned even for the owner and a home-project reader.
	f.host.deny(agentB, "project-1", PermissionRead)
	f.host.deny(agentA, "project-1", PermissionRead)
	if n := f.mayNotRead(refs, agentB); n != 1 {
		t.Errorf("home-project agent whose credential refuses reads: %d, want 1", n)
	}
	if n := f.mayNotRead(refs, agentA); n != 1 {
		t.Errorf("owner whose credential refuses reads: %d, want 1", n)
	}

	// A principal grant.
	f.grantPrincipal(id, outside)
	if n := f.mayNotRead(refs, outside); n != 0 {
		t.Errorf("principal grantee: %d, want 0", n)
	}

	// A scope grant counts only for a scope the recipient is authorized in.
	if n := f.mayNotRead(refs, agentX); n != 1 {
		t.Fatalf("other-project agent before scope grant: %d, want 1", n)
	}
	f.grantScope(id, "project-2")
	if n := f.mayNotRead(refs, agentX); n != 0 {
		t.Errorf("agent in a granted scope: %d, want 0", n)
	}
}

// TestRecipientMayNotReadExpiry: an expired grant does not count, and an
// expired artifact is unreadable to everyone, its owner included.
func TestRecipientMayNotReadExpiry(t *testing.T) {
	f, _ := newRecipientFixture(t)
	id := f.publish(agentA, "design.md", []byte("# v1"), "").Artifact.ID
	refs := []MessageRef{{ArtifactID: id}}
	past := time.Now().Add(-time.Hour).UTC().Format(sqliteTimeLayout)

	f.grantPrincipal(id, outside)
	if _, err := f.db.Exec(`UPDATE artifact_grant SET expires_at = ? WHERE artifact_id = ?`, past, id); err != nil {
		t.Fatal(err)
	}
	if n := f.mayNotRead(refs, outside); n != 1 {
		t.Errorf("expired grant: %d, want 1", n)
	}
	if _, err := f.db.Exec(`UPDATE artifact SET expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatal(err)
	}
	for _, p := range []principal{agentA, agentB} {
		if n := f.mayNotRead(refs, p); n != 1 {
			t.Errorf("expired artifact, %s: %d, want 1", p.ref, n)
		}
	}
}

// TestRecipientMayNotReadUnfinalized: before its first version is
// finalized an artifact is its owner's alone.
func TestRecipientMayNotReadUnfinalized(t *testing.T) {
	f, _ := newRecipientFixture(t)
	id := f.publish(agentA, "design.md", []byte("# v1"), "").Artifact.ID
	if _, err := f.db.Exec(`UPDATE artifact SET current_seq = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	refs := []MessageRef{{ArtifactID: id}}
	if n := f.mayNotRead(refs, agentB); n != 1 {
		t.Errorf("home-project agent, unfinalized: %d, want 1", n)
	}
	if n := f.mayNotRead(refs, agentA); n != 0 {
		t.Errorf("owner, unfinalized: %d, want 0", n)
	}
}

// TestRecipientMayNotReadCounts: the count is per reference, each artifact
// is checked once, and a reference to a missing or malformed id is never
// counted (the warning is about access, and such a reference was never
// admitted).
func TestRecipientMayNotReadCounts(t *testing.T) {
	f, h := newRecipientFixture(t)
	a := f.publish(agentA, "a.md", []byte("# a"), "").Artifact.ID
	b := f.publish(agentX, "b.md", []byte("# b"), "").Artifact.ID
	refs := []MessageRef{
		{ArtifactID: a},
		{ArtifactID: b},
		{ArtifactID: b, Seq: 1},
		{ArtifactID: uuid.NewString()},
		{ArtifactID: "not-canonical"},
	}
	h.asked = nil
	if n := f.mayNotRead(refs, agentB); n != 2 {
		t.Errorf("count = %d, want 2 (both references to b)", n)
	}
	permits := 0
	for _, q := range h.asked {
		if len(q) > 8 && q[:8] == "permits " {
			permits++
		}
	}
	if permits != 2 {
		t.Errorf("credential asked %d times, want once per artifact (2): %v", permits, h.asked)
	}
}

// TestRecipientMayNotReadSilentCases: no warning (0) without the host
// extension, without a store, or without a recipient; a lookup failure
// returns the error and 0, never a count.
func TestRecipientMayNotReadSilentCases(t *testing.T) {
	f, h := newRecipientFixture(t)
	id := f.publish(agentA, "design.md", []byte("# v1"), "").Artifact.ID
	refs := []MessageRef{{ArtifactID: id}}
	ctx := context.Background()

	plain := NewService(f.host) // fakeHost alone: no RecipientAuthority
	plain.SetStore(f.store)
	if n, err := plain.RecipientMayNotRead(ctx, refs, outside.kind, outside.ref); n != 0 || err != nil {
		t.Errorf("host without the extension: %d, %v; want 0, nil", n, err)
	}
	if n, err := NewService(h).RecipientMayNotRead(ctx, refs, outside.kind, outside.ref); n != 0 || err != nil {
		t.Errorf("no store: %d, %v; want 0, nil", n, err)
	}
	if n, err := f.svc.RecipientMayNotRead(ctx, refs, "", ""); n != 0 || err != nil {
		t.Errorf("no recipient: %d, %v; want 0, nil", n, err)
	}

	h.err = errors.New("authz unavailable")
	if n, err := f.svc.RecipientMayNotRead(ctx, refs, outside.kind, outside.ref); n != 0 || err == nil {
		t.Errorf("authority failure: %d, %v; want 0 and the error", n, err)
	}
	h.err = nil

	f.svc.SetStore(failGrantsStore{f.store})
	if n, err := f.svc.RecipientMayNotRead(ctx, refs, outside.kind, outside.ref); n != 0 || err == nil {
		t.Errorf("grant read failure: %d, %v; want 0 and the error", n, err)
	}
	f.svc.SetStore(failGetArtifactStore{f.store})
	if n, err := f.svc.RecipientMayNotRead(ctx, refs, outside.kind, outside.ref); n != 0 || err == nil {
		t.Errorf("artifact read failure: %d, %v; want 0 and the error", n, err)
	}
}

// TestRecipientMayNotReadNeverAsksForTheCaller: the check answers only
// from RecipientAuthority for the named recipient. The caller in ctx (here
// the owner, who could read everything) never changes the answer.
func TestRecipientMayNotReadNeverAsksForTheCaller(t *testing.T) {
	f, _ := newRecipientFixture(t)
	id := f.publish(agentA, "design.md", []byte("# v1"), "").Artifact.ID
	ctx := context.WithValue(context.Background(), principalKey{}, agentA)
	n, err := f.svc.RecipientMayNotRead(ctx, []MessageRef{{ArtifactID: id}}, outside.kind, outside.ref)
	if err != nil || n != 1 {
		t.Errorf("with the owner as caller: %d, %v; want 1, nil", n, err)
	}
}
