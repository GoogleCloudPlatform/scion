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

	got := f.svc.ResolveRefs(ctxFor(agentB), refs)
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
	other := f.svc.ResolveRefs(ctxFor(agentX), refs[:1])
	missing := f.svc.ResolveRefs(ctxFor(agentX), refs[3:4])
	strip := func(v RefView) RefView { v.Ref, v.ID = "", ""; return v }
	if other[0].Available || strip(other[0]) != strip(missing[0]) {
		t.Errorf("unreadable %+v differs from missing %+v", other[0], missing[0])
	}

	// No principal at all.
	if v := f.svc.ResolveRefs(context.Background(), refs[:1]); v[0].Available {
		t.Errorf("anonymous view available: %+v", v[0])
	}

	// A credential that does not permit reads hides it even from the owner.
	f.host.deny(agentA, "project-1", PermissionRead)
	if v := f.svc.ResolveRefs(ctxFor(agentA), refs[:1]); v[0].Available {
		t.Errorf("denied owner view available: %+v", v[0])
	}

	// Unconfigured service: unavailable, no panic.
	if v := NewService(f.host).ResolveRefs(ctxFor(agentB), refs[:1]); v[0].Available {
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
	views := f.svc.ResolveRefs(ctx, []MessageRef{{ArtifactID: id}, {ArtifactID: id, Seq: 1}, {ArtifactID: id, Seq: 4}})
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
