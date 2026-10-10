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

//go:build !no_sqlite

package artifacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const refID = "5f1c2d3e-0000-4000-8000-000000000001"

func TestParseMessageRef(t *testing.T) {
	cases := []struct {
		in   string
		want MessageRef
		ok   bool
	}{
		{RefScheme + refID, MessageRef{ArtifactID: refID}, true},
		{RefScheme + refID + "@3", MessageRef{ArtifactID: refID, Seq: 3}, true},
		{RefScheme + strings.ToUpper(refID), MessageRef{ArtifactID: refID}, true},
		{refID, MessageRef{}, false},                     // bare id: scheme required
		{" " + RefScheme + refID, MessageRef{}, false},   // surrounding space
		{RefScheme + refID + "@0", MessageRef{}, false},  // seq must be positive
		{RefScheme + refID + "@01", MessageRef{}, false}, // no leading zeros
		{RefScheme + "not-a-uuid", MessageRef{}, false},
		{"https://example.com/" + refID, MessageRef{}, false},
		{"", MessageRef{}, false},
	}
	for _, c := range cases {
		got, err := ParseMessageRef(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseMessageRef(%q) = %+v, %v; want %+v ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestParseMessageRefsBoundsAndDedupes(t *testing.T) {
	var in []string
	for i := 0; i < MaxMessageRefs+3; i++ {
		in = append(in, FormatRef(uuid.NewString(), 0))
	}
	// A duplicate of the first artifact (another version) and a malformed entry.
	in = append([]string{in[0], FormatRef(strings.TrimPrefix(in[0], RefScheme), 2), "junk"}, in[1:]...)
	b, _ := json.Marshal(in)
	refs, dropped := ParseMessageRefs(string(b))
	if len(refs) != MaxMessageRefs {
		t.Fatalf("got %d refs, want %d", len(refs), MaxMessageRefs)
	}
	if want := len(in) - MaxMessageRefs; dropped != want {
		t.Errorf("dropped = %d, want %d", dropped, want)
	}
	if refs[0].String() != in[0] {
		t.Errorf("order not kept: first = %s, want %s", refs[0], in[0])
	}

	for _, bad := range []string{"not json", `{"a":1}`, `[1,2]`} {
		if refs, dropped := ParseMessageRefs(bad); refs != nil || dropped != 1 {
			t.Errorf("ParseMessageRefs(%q) = %v, %d; want nil, 1", bad, refs, dropped)
		}
	}
	if refs, dropped := ParseMessageRefs(""); refs != nil || dropped != 0 {
		t.Errorf("empty value = %v, %d", refs, dropped)
	}
}

func TestEncodeMessageRefsRoundTrip(t *testing.T) {
	refs := []MessageRef{{ArtifactID: refID, Seq: 2}, {ArtifactID: uuid.NewString()}}
	enc := EncodeMessageRefs(refs)
	got, dropped := ParseMessageRefs(enc)
	if dropped != 0 || !reflect.DeepEqual(got, refs) {
		t.Errorf("round trip %q = %+v (dropped %d), want %+v", enc, got, dropped, refs)
	}
	if EncodeMessageRefs(nil) != "" {
		t.Error("no refs must encode to empty")
	}
}

// TestResolveRefsReadCheck: a reference reveals title, version and owner
// only to a reader the artifact service would serve, and an unreadable
// reference looks exactly like a missing one.
func TestResolveRefsReadCheck(t *testing.T) {
	f := newFixture(t, false)
	pub := f.publish(agentA, "design.md", []byte("# v1"), "title=Design")
	id := pub.Artifact.ID

	refs := []MessageRef{
		{ArtifactID: id},
		{ArtifactID: id, Seq: 1},
		{ArtifactID: id, Seq: 9},              // no such version
		{ArtifactID: uuid.NewString()},        // no such artifact
		{ArtifactID: "not-canonical", Seq: 1}, // never reaches the store
	}
	ctxFor := func(p principal) context.Context {
		return context.WithValue(context.Background(), principalKey{}, p)
	}

	got := mustResolve(t, f.svc, ctxFor(agentB), refs)
	if len(got) != len(refs) {
		t.Fatalf("got %d views", len(got))
	}
	for i, want := range []bool{true, true, false, false, false} {
		if got[i].Available != want {
			t.Errorf("view %d available = %v, want %v (%+v)", i, got[i].Available, want, got[i])
		}
	}
	if v := got[0]; v.Title != "Design" || v.Version != 1 || v.OwnerKind != PrincipalKindAgent || v.OwnerRef != agentA.ref {
		t.Errorf("readable view = %+v", v)
	}

	// Another project's agent: everything unavailable, and indistinguishable
	// from the missing artifact apart from ref/id.
	other := mustResolve(t, f.svc, ctxFor(agentX), refs[:1])
	missing := mustResolve(t, f.svc, ctxFor(agentX), refs[3:4])
	strip := func(v RefView) RefView { v.Ref, v.ID = "", ""; return v }
	if other[0].Available || strip(other[0]) != strip(missing[0]) {
		t.Errorf("unreadable %+v differs from missing %+v", other[0], missing[0])
	}

	// No principal at all.
	if v := mustResolve(t, f.svc, context.Background(), refs[:1]); v[0].Available {
		t.Errorf("anonymous view available: %+v", v[0])
	}

	// A credential that does not permit reads hides it even from the owner.
	f.host.deny(agentA, "project-1", PermissionRead)
	if v := mustResolve(t, f.svc, ctxFor(agentA), refs[:1]); v[0].Available {
		t.Errorf("denied owner view available: %+v", v[0])
	}

	// Unconfigured service: unavailable, no panic.
	if v := mustResolve(t, NewService(f.host), ctxFor(agentB), refs[:1]); v[0].Available {
		t.Errorf("unconfigured view available: %+v", v[0])
	}
}

func TestStoreMessageRefs(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a1, _, _, _ := seedArtifact(t, st, "k1")
		a2, _, _, _ := seedArtifact(t, st, "k2")

		if err := st.AddMessageRefs(ctx, "msg-1", []MessageRef{{ArtifactID: a1.ID, Seq: 1}, {ArtifactID: a2.ID}}); err != nil {
			t.Fatalf("AddMessageRefs: %v", err)
		}
		// Re-adding is a no-op, not an error, and keeps the first seq.
		if err := st.AddMessageRefs(ctx, "msg-1", []MessageRef{{ArtifactID: a1.ID, Seq: 7}}); err != nil {
			t.Fatalf("AddMessageRefs again: %v", err)
		}
		if err := st.AddMessageRefs(ctx, "msg-2", []MessageRef{{ArtifactID: a2.ID, Seq: 1}}); err != nil {
			t.Fatalf("AddMessageRefs msg-2: %v", err)
		}
		if err := st.AddMessageRefs(ctx, "", []MessageRef{{ArtifactID: a2.ID}}); err != nil {
			t.Fatalf("empty message id must be a no-op: %v", err)
		}

		got, err := st.ListMessageRefs(ctx, []string{"msg-1", "msg-2", "msg-none"})
		if err != nil {
			t.Fatalf("ListMessageRefs: %v", err)
		}
		want1 := []MessageRef{{ArtifactID: a1.ID, Seq: 1}, {ArtifactID: a2.ID}}
		if a2.ID < a1.ID {
			want1[0], want1[1] = want1[1], want1[0]
		}
		if !reflect.DeepEqual(got["msg-1"], want1) {
			t.Errorf("msg-1 = %+v, want %+v", got["msg-1"], want1)
		}
		if !reflect.DeepEqual(got["msg-2"], []MessageRef{{ArtifactID: a2.ID, Seq: 1}}) {
			t.Errorf("msg-2 = %+v", got["msg-2"])
		}
		if _, ok := got["msg-none"]; ok {
			t.Error("message without refs present in map")
		}

		// Many ids in one call (chunked queries).
		ids := make([]string, 0, maxMessageRefQueryIDs+5)
		for i := 0; i < maxMessageRefQueryIDs+4; i++ {
			ids = append(ids, fmt.Sprintf("other-%d", i))
		}
		ids = append(ids, "msg-2")
		got, err = st.ListMessageRefs(ctx, ids)
		if err != nil || len(got) != 1 || len(got["msg-2"]) != 1 {
			t.Errorf("chunked ListMessageRefs = %+v, %v", got, err)
		}
		if got, err := st.ListMessageRefs(ctx, nil); err != nil || len(got) != 0 {
			t.Errorf("ListMessageRefs(nil) = %+v, %v", got, err)
		}
	})
}

// TestResolveRefsChecksEachArtifactOnce: several references to one artifact
// (different versions) cost one lookup and one read check.
func TestResolveRefsChecksEachArtifactOnce(t *testing.T) {
	f := newFixture(t, false)
	id := f.publish(agentA, "design.md", []byte("# v1"), "title=Design").Artifact.ID
	f.host.mu.Lock()
	f.host.calls = nil
	f.host.mu.Unlock()

	ctx := context.WithValue(context.Background(), principalKey{}, agentB)
	views := mustResolve(t, f.svc, ctx, []MessageRef{{ArtifactID: id}, {ArtifactID: id, Seq: 1}, {ArtifactID: id, Seq: 4}})
	if !views[0].Available || !views[1].Available || views[2].Available {
		t.Fatalf("views = %+v", views)
	}
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	permits := 0
	for _, c := range f.host.calls {
		if strings.HasPrefix(c, "permits ") {
			permits++
		}
	}
	if permits != 1 {
		t.Errorf("read check ran %d times for one artifact; calls %v", permits, f.host.calls)
	}
}

// mustResolve is ResolveRefs on a working store: it must not fail.
func mustResolve(t *testing.T, s *Service, ctx context.Context, refs []MessageRef) []RefView {
	t.Helper()
	views, err := s.ResolveRefs(ctx, refs)
	if err != nil {
		t.Fatalf("ResolveRefs: %v", err)
	}
	return views
}

type failGetArtifactStore struct{ Store }

func (failGetArtifactStore) GetArtifact(context.Context, string) (*Artifact, error) {
	return nil, errors.New("artifacts unavailable")
}

type failGetVersionStore struct{ Store }

func (failGetVersionStore) GetVersion(context.Context, string, int) (*Version, error) {
	return nil, errors.New("versions unavailable")
}

// TestResolveRefsReportsStoreFailures: a failed grant read (or artifact or
// version read) is reported as a *ResolveError counting the references it
// left unchecked, never taken for "no access"; those views stay
// unavailable. A missing id never reaches the grant read, so it stays on
// the unavailable path with no error.
func TestResolveRefsReportsStoreFailures(t *testing.T) {
	f := newFixture(t, false)
	id := f.publish(userU, "doc.md", []byte("# v1"), "scope=project-1").Artifact.ID
	f.grantPrincipal(id, outside)
	ctxFor := func(p principal) context.Context {
		return context.WithValue(context.Background(), principalKey{}, p)
	}
	unavailable := func(t *testing.T, views []RefView) {
		t.Helper()
		for _, v := range views {
			if v != (RefView{Ref: v.Ref, ID: v.ID, Seq: v.Seq}) {
				t.Errorf("view carries more than the reference: %+v", v)
			}
		}
	}
	missing := MessageRef{ArtifactID: uuid.NewString()}

	t.Run("grant read", func(t *testing.T) {
		f.svc.SetStore(failGrantsStore{f.store})
		t.Cleanup(func() { f.svc.SetStore(f.store) })
		// The grantee needs the grants; both references to the artifact
		// are unchecked, the missing one is just unavailable.
		views, err := f.svc.ResolveRefs(ctxFor(outside), []MessageRef{{ArtifactID: id}, {ArtifactID: id, Seq: 1}, missing})
		var re *ResolveError
		if !errors.As(err, &re) || re.Unchecked != 2 {
			t.Fatalf("ResolveRefs error = %v, want a ResolveError with 2 unchecked", err)
		}
		unavailable(t, views)
		// A missing id alone: no grant read, no error.
		if views, err := f.svc.ResolveRefs(ctxFor(outside), []MessageRef{missing}); err != nil || views[0].Available {
			t.Errorf("missing id with grants unreadable: %+v, %v; want unavailable, no error", views, err)
		}
		// The owner and home-project readers never need the grants.
		for _, p := range []principal{userU, agentB} {
			if views, err := f.svc.ResolveRefs(ctxFor(p), []MessageRef{{ArtifactID: id}}); err != nil || !views[0].Available {
				t.Errorf("%s with grants unreadable: %+v, %v", p.ref, views, err)
			}
		}
	})

	t.Run("artifact read", func(t *testing.T) {
		f.svc.SetStore(failGetArtifactStore{f.store})
		t.Cleanup(func() { f.svc.SetStore(f.store) })
		views, err := f.svc.ResolveRefs(ctxFor(userU), []MessageRef{{ArtifactID: id}, missing})
		var re *ResolveError
		if !errors.As(err, &re) || re.Unchecked != 2 {
			t.Fatalf("ResolveRefs error = %v, want a ResolveError with 2 unchecked", err)
		}
		unavailable(t, views)
	})

	t.Run("version read", func(t *testing.T) {
		f.svc.SetStore(failGetVersionStore{f.store})
		t.Cleanup(func() { f.svc.SetStore(f.store) })
		views, err := f.svc.ResolveRefs(ctxFor(userU), []MessageRef{{ArtifactID: id, Seq: 1}, {ArtifactID: id}})
		var re *ResolveError
		if !errors.As(err, &re) || re.Unchecked != 1 {
			t.Fatalf("ResolveRefs error = %v, want a ResolveError with 1 unchecked", err)
		}
		unavailable(t, views[:1])
		if !views[1].Available {
			t.Errorf("unpinned reference needs no version read: %+v", views[1])
		}
	})
}
