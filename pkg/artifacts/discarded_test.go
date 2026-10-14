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

// Discarded versions in the versions list (ptone/scion#4577): numbering
// gaps are explained by listing failed versions with only their number,
// state, reason and creation time.

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// insertVersion adds a version row directly, for list tests.
func (f *fixture) insertVersion(id string, seq int, state, reason, note, byRef string) {
	f.t.Helper()
	var r any
	if reason != "" {
		r = reason
	}
	if _, err := f.db.Exec(`INSERT INTO artifact_version
		(id, artifact_id, seq, kind, entry_path, note, total_bytes, file_count, created_by_kind, created_by_ref, created_at, state, fail_reason)
		VALUES (?, ?, ?, 'review', 'design.md', ?, 7, 1, 'user', ?, ?, ?, ?)`,
		uuid.NewString(), id, seq, note, byRef, time.Now().UTC().Format(sqliteTimeLayout), state, r); err != nil {
		f.t.Fatal(err)
	}
}

// discardedFixture publishes v1 and adds: v2 discarded as unmarked
// changes, v3 discarded before reasons were recorded, v4 ready and v5
// still pending. The discarded ones carry a note and a submitter, which
// the list must never show.
func discardedFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t, false)
	id := f.publish(agentA, "design.md", []byte("# v1"), "title=Design").Artifact.ID
	f.insertVersion(id, 2, VersionStateFailed, FailReasonUnmarkedChanges, "secret note two", "user-secret")
	f.insertVersion(id, 3, VersionStateFailed, "", "secret note three", "user-secret")
	f.insertVersion(id, 4, VersionStateReady, "", "", "user-1")
	f.insertVersion(id, 5, VersionStatePending, "", "", "user-1")
	return f, id
}

// rawDiscarded decodes the discarded entries of a list response as raw
// key/value maps, so a test can see every field the API sends.
func rawDiscarded(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var raw struct {
		Discarded []map[string]any `json:"discarded"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return raw.Discarded
}

// TestListVersionsDiscarded: a reader gets the ready versions and, beside
// them, the discarded ones with only seq, state, reason and createdAt.
// Pending versions are in neither list.
func TestListVersionsDiscarded(t *testing.T) {
	f, id := discardedFixture(t)
	rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/versions", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeInto[VersionListResponse](t, rec)
	var ready []int
	for _, v := range resp.Versions {
		ready = append(ready, v.Seq)
	}
	if !reflect.DeepEqual(ready, []int{4, 1}) {
		t.Errorf("ready versions %v, want [4 1]", ready)
	}
	if len(resp.Discarded) != 2 ||
		resp.Discarded[0].Seq != 3 || resp.Discarded[0].Reason != FailReasonDiscarded ||
		resp.Discarded[1].Seq != 2 || resp.Discarded[1].Reason != FailReasonUnmarkedChanges {
		t.Fatalf("discarded %+v", resp.Discarded)
	}
	for _, d := range resp.Discarded {
		if d.State != VersionStateFailed || d.CreatedAt.IsZero() {
			t.Errorf("discarded entry %+v", d)
		}
	}

	// Only the four fields, whatever the row holds.
	for _, d := range rawDiscarded(t, rec.Body.Bytes()) {
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"createdAt", "reason", "seq", "state"}) {
			t.Errorf("discarded entry fields %v, want exactly createdAt, reason, seq, state", keys)
		}
	}
	body := rec.Body.String()
	for _, hidden := range []string{"secret note", "user-secret"} {
		if strings.Contains(body, hidden) {
			t.Errorf("the list shows %q of a discarded version", hidden)
		}
	}
}

// TestListVersionsDiscardedEmpty: with nothing discarded the field is an
// empty list, not absent.
func TestListVersionsDiscardedEmpty(t *testing.T) {
	f := newFixture(t, false)
	id := f.publish(agentA, "design.md", []byte("# v1"), "").Artifact.ID
	rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/versions", nil, nil)
	if !strings.Contains(rec.Body.String(), `"discarded":[]`) {
		t.Errorf("no discarded versions: body %s", rec.Body.String())
	}
}

// TestListVersionsDiscardedPages: a page walks ready and discarded
// versions together by seq, so following nextBefore sees every one once,
// including with limit=1, where some pages hold only a discarded version.
func TestListVersionsDiscardedPages(t *testing.T) {
	f, id := discardedFixture(t)
	for _, limit := range []int{2, 1} {
		var ready, discarded []int
		before := 0
		for pages := 0; pages < 10; pages++ {
			target := "/api/v1/artifacts/" + id + "/versions?limit=" + itoa(limit)
			if before > 0 {
				target += "&before=" + itoa(before)
			}
			resp := decodeInto[VersionListResponse](t, f.do(&agentB, http.MethodGet, target, nil, nil))
			if len(resp.Versions)+len(resp.Discarded) > limit {
				t.Fatalf("limit %d: page holds %d rows", limit, len(resp.Versions)+len(resp.Discarded))
			}
			for _, v := range resp.Versions {
				ready = append(ready, v.Seq)
			}
			for _, d := range resp.Discarded {
				discarded = append(discarded, d.Seq)
			}
			if resp.NextBefore == 0 {
				break
			}
			before = resp.NextBefore
		}
		if !reflect.DeepEqual(ready, []int{4, 1}) || !reflect.DeepEqual(discarded, []int{3, 2}) {
			t.Errorf("limit %d: paged ready %v discarded %v, want [4 1] and [3 2]", limit, ready, discarded)
		}
	}
}

// TestListVersionsDiscardedNeedsRead: a caller who cannot read the
// artifact gets the same 404 as for a missing one, with no discarded rows.
func TestListVersionsDiscardedNeedsRead(t *testing.T) {
	f, id := discardedFixture(t)
	got := f.do(&agentX, http.MethodGet, "/api/v1/artifacts/"+id+"/versions", nil, nil)
	missing := f.do(&agentX, http.MethodGet, "/api/v1/artifacts/"+uuid.NewString()+"/versions", nil, nil)
	if got.Code != http.StatusNotFound || got.Body.String() != missing.Body.String() {
		t.Errorf("non-reader: %d %q; missing: %d %q", got.Code, got.Body.String(), missing.Code, missing.Body.String())
	}
	if strings.Contains(got.Body.String(), "discarded") || strings.Contains(got.Body.String(), FailReasonUnmarkedChanges) {
		t.Errorf("non-reader sees discarded versions: %s", got.Body.String())
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
