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

// fakeRecipientHost is fakeHost plus RecipientAuthority, answered from the
// same tables for a named principal rather than for the caller of ctx.
type fakeRecipientHost struct {
	*fakeHost
	// err, when set, makes every RecipientSubject question fail.
	err error
	// resolveErr, when set, makes ResolveRecipient fail.
	resolveErr error
	// unknown lists principal refs ResolveRecipient does not know.
	unknown map[string]bool
	// asked records "resolve|permits|authorize principalRef [scope]" per call.
	asked []string
}

type fakeRecipientSubject struct {
	h         *fakeRecipientHost
	kind, ref string
}

func (h *fakeRecipientHost) ResolveRecipient(_ context.Context, kind, ref string) (RecipientSubject, error) {
	h.asked = append(h.asked, "resolve "+PrincipalRef(kind, ref))
	if h.resolveErr != nil {
		return nil, h.resolveErr
	}
	if h.unknown[ref] {
		return nil, nil
	}
	return &fakeRecipientSubject{h: h, kind: kind, ref: ref}, nil
}

func (s *fakeRecipientSubject) Permits(_ context.Context, scope, perm string) (bool, error) {
	h := s.h
	h.asked = append(h.asked, "permits "+PrincipalRef(s.kind, s.ref)+" "+scope)
	if h.err != nil {
		return false, h.err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return scope != "" && !h.denied[PrincipalRef(s.kind, s.ref)+" "+scope+" "+perm], nil
}

func (s *fakeRecipientSubject) Authorize(_ context.Context, scope, perm string) (bool, error) {
	h := s.h
	h.asked = append(h.asked, "authorize "+PrincipalRef(s.kind, s.ref)+" "+scope)
	if h.err != nil {
		return false, h.err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.perms[PrincipalRef(s.kind, s.ref)][scope][perm], nil
}

// newRecipientFixture is newFixture with a host that implements
// RecipientAuthority.
func newRecipientFixture(t *testing.T) (*fixture, *fakeRecipientHost) {
	t.Helper()
	f := newFixture(t, false)
	h := &fakeRecipientHost{fakeHost: f.host, unknown: map[string]bool{}}
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
	// The recipient is resolved once per call, and each artifact is decided
	// once: canReadWith asks the credential of the home scope twice per
	// artifact (step 1, and again inside Authorize), never more.
	count := map[string]int{}
	for _, q := range h.asked {
		count[q]++
	}
	if count["resolve "+PrincipalRef(agentB.kind, agentB.ref)] != 1 {
		t.Errorf("recipient resolved %d times, want 1: %v", count["resolve "+PrincipalRef(agentB.kind, agentB.ref)], h.asked)
	}
	for _, scope := range []string{"project-1", "project-2"} {
		if n := count["permits "+PrincipalRef(agentB.kind, agentB.ref)+" "+scope]; n != 2 {
			t.Errorf("credential in %s asked %d times, want 2 (each artifact decided once): %v", scope, n, h.asked)
		}
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

	h.resolveErr = errors.New("principal lookup failed")
	if n, err := f.svc.RecipientMayNotRead(ctx, refs, outside.kind, outside.ref); n != 0 || err == nil {
		t.Errorf("recipient lookup failure: %d, %v; want 0 and the error", n, err)
	}
	h.resolveErr = nil

	// An unknown recipient can read nothing: counted, not an error.
	h.unknown[outside.ref] = true
	if n, err := f.svc.RecipientMayNotRead(ctx, refs, outside.kind, outside.ref); n != 1 || err != nil {
		t.Errorf("unknown recipient: %d, %v; want 1, nil", n, err)
	}
	delete(h.unknown, outside.ref)

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

// TestRecipientMayNotReadParity: for each case, the recipient check and
// the caller read check (canReadErr, the one GET applies) agree for the
// same principal, because both evaluate canReadWith; the expected answer
// pins that each case means what it says.
func TestRecipientMayNotReadParity(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(sqliteTimeLayout)
	cases := []struct {
		name     string
		who      principal
		setup    func(f *fixture, id string)
		readable bool
	}{
		{"owner", agentA, nil, true},
		{"home member", agentB, nil, true},
		{"other-project agent", agentX, nil, false},
		{"principal grant", outside, func(f *fixture, id string) { f.grantPrincipal(id, outside) }, true},
		{"scope grant", agentX, func(f *fixture, id string) { f.grantScope(id, "project-2") }, true},
		{"expired grant", outside, func(f *fixture, id string) {
			f.grantPrincipal(id, outside)
			f.execSQL(`UPDATE artifact_grant SET expires_at = ? WHERE artifact_id = ?`, past, id)
		}, false},
		{"expired artifact", agentB, func(f *fixture, id string) {
			f.execSQL(`UPDATE artifact SET expires_at = ? WHERE id = ?`, past, id)
		}, false},
		{"expired artifact, owner", agentA, func(f *fixture, id string) {
			f.execSQL(`UPDATE artifact SET expires_at = ? WHERE id = ?`, past, id)
		}, false},
		{"unfinalized", agentB, func(f *fixture, id string) {
			f.execSQL(`UPDATE artifact SET current_seq = NULL WHERE id = ?`, id)
		}, false},
		{"unfinalized, owner", agentA, func(f *fixture, id string) {
			f.execSQL(`UPDATE artifact SET current_seq = NULL WHERE id = ?`, id)
		}, true},
		{"credential deny", agentB, func(f *fixture, id string) { f.host.deny(agentB, "project-1", PermissionRead) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newRecipientFixture(t)
			id := f.publish(agentA, "design.md", []byte("# v1"), "").Artifact.ID
			if tc.setup != nil {
				tc.setup(f, id)
			}
			ctx := context.Background()
			b, _ := f.svc.backend()
			a, err := f.store.GetArtifact(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			asCaller, err := f.svc.canReadErr(context.WithValue(ctx, principalKey{}, tc.who), b, a)
			if err != nil {
				t.Fatal(err)
			}
			n, err := f.svc.RecipientMayNotRead(ctx, []MessageRef{{ArtifactID: id}}, tc.who.kind, tc.who.ref)
			if err != nil {
				t.Fatal(err)
			}
			asRecipient := n == 0
			if asCaller != tc.readable {
				t.Errorf("caller read = %v, want %v", asCaller, tc.readable)
			}
			if asRecipient != asCaller {
				t.Errorf("recipient check says readable=%v, caller read says %v", asRecipient, asCaller)
			}
		})
	}
}

func (f *fixture) execSQL(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}
