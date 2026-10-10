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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for the template commit compare-and-swap (ptone/scion#4221 part 1).

// setTemplateContentForTest writes tmpl with its content columns, as only
// the commit path may in production (store.UpdateTemplate no longer writes
// them). Tests use it to plant drifted or stale content.
func setTemplateContentForTest(ctx context.Context, s store.Store, tmpl *store.Template) error {
	cur, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		return err
	}
	return s.UpdateTemplateContent(ctx, tmpl, store.TemplateContentPrecondition{ContentHash: cur.ContentHash})
}

// conflictInjectingStore makes the next `armed` UpdateTemplateContent calls
// lose to a concurrent commit once its fault switch is armed: before
// delegating, it commits a new content hash to the same row through the
// inner store, as another hub replica would. Until then it is transparent.
// It is installed with installStoreFault right after the server is built
// (newConflictTestServer), never swapped in mid-test (ptone/scion#3435).
//
// With revertAfter set, a second concurrent commit restores the original
// content hash right after the caller's write has lost, so the caller's
// re-read sees its original content hash again (another writer reverted).
type conflictInjectingStore struct {
	store.Store
	fault       *storeFaultSwitch
	armed       atomic.Int32
	injected    atomic.Int32
	revertAfter bool
}

// arm makes the next n template commits lose to an injected concurrent
// commit.
func (c *conflictInjectingStore) arm(n int32) {
	c.armed.Store(n)
	c.fault.Arm()
}

func (c *conflictInjectingStore) UpdateTemplateContent(ctx context.Context, t *store.Template, expected store.TemplateContentPrecondition) error {
	if !c.fault.Active() || c.armed.Add(-1) < 0 {
		return c.Store.UpdateTemplateContent(ctx, t, expected)
	}
	cur, err := c.GetTemplate(ctx, t.ID)
	if err != nil {
		return err
	}
	original := cur.ContentHash
	n := c.injected.Add(1)
	cur.ContentHash = fmt.Sprintf("sha256:concurrent-%d", n)
	if err := c.Store.UpdateTemplateContent(ctx, cur, store.TemplateContentPrecondition{ContentHash: expected.ContentHash}); err != nil {
		return fmt.Errorf("inject concurrent commit: %w", err)
	}
	err = c.Store.UpdateTemplateContent(ctx, t, expected)
	if c.revertAfter {
		concurrent := cur.ContentHash
		cur.ContentHash = original
		if rerr := c.Store.UpdateTemplateContent(ctx, cur, store.TemplateContentPrecondition{ContentHash: concurrent}); rerr != nil {
			return fmt.Errorf("revert concurrent commit: %w", rerr)
		}
	}
	return err
}

// newConflictTestServer is newCommitTestServer with a conflictInjectingStore
// installed (disarmed) before any setup. It returns the raw store, which
// bypasses the wrapper, and the wrapper.
func newConflictTestServer(t *testing.T, stor storage.Storage) (*Server, store.Store, *conflictInjectingStore) {
	t.Helper()
	srv, s := newCommitTestServer(t, stor)
	waitUserScopedDataSweep(t, srv)
	inj, _ := installStoreFault(t, srv, func(inner store.Store, fault *storeFaultSwitch) *conflictInjectingStore {
		return &conflictInjectingStore{Store: inner, fault: fault}
	})
	return srv, s, inj
}

func finalizeBody(t *testing.T, files []store.TemplateFile, expected string) []byte {
	t.Helper()
	body, err := json.Marshal(FinalizeRequest{
		Manifest:            &TemplateManifest{Version: "1.0", Files: files},
		ExpectedContentHash: expected,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func templateErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

// concurrentFinalizeOutcome is the result of two finalizes racing against
// the same base version.
type concurrentFinalizeOutcome struct {
	recs    []*httptest.ResponseRecorder
	pushes  []map[string]string
	tmpl    *store.Template
	winners []int
}

// raceTwoFinalizes seeds a template, stores the objects of two competing
// pushes (each adds a different file), and sends both finalizes at once,
// each naming the seeded content hash as its expectedContentHash.
func raceTwoFinalizes(t *testing.T, srv *Server) concurrentFinalizeOutcome {
	t.Helper()
	tmpl := seedCommittedTemplate(t, srv, "race", "global", "", map[string]string{"scion-agent.yaml": commitCfgOld})
	h0 := tmpl.ContentHash

	pushes := []map[string]string{
		{"scion-agent.yaml": commitCfgOld, "a.md": "from writer A"},
		{"scion-agent.yaml": commitCfgOld, "b.md": "from writer B"},
	}
	for _, files := range pushes {
		putObjects(t, srv.GetStorage(), tmpl.StoragePath, files)
	}

	recs := make([]*httptest.ResponseRecorder, len(pushes))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range pushes {
		body := finalizeBody(t, commitManifest(pushes[i]), h0)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i] = doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", "application/json", body)
		}(i)
	}
	close(start)
	wg.Wait()

	out := concurrentFinalizeOutcome{recs: recs, pushes: pushes, tmpl: tmpl}
	for i, rec := range recs {
		if rec.Code == http.StatusOK {
			out.winners = append(out.winners, i)
		}
	}
	return out
}

// Acceptance 1 (as amended): two concurrent finalizes that diffed against
// the same version produce exactly one success and one 409
// template_conflict, the row is the winner's manifest, and no file of the
// winner is lost.
func TestTemplateFinalize_ConcurrentFinalizesExactlyOneWins(t *testing.T) {
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	ctx := context.Background()

	out := raceTwoFinalizes(t, srv)
	if len(out.winners) != 1 {
		t.Fatalf("%d finalizes succeeded; want exactly one 200 and one 409 (bodies: %s | %s)", len(out.winners), out.recs[0].Body.String(), out.recs[1].Body.String())
	}
	winner := out.winners[0]
	for i, rec := range out.recs {
		if i == winner {
			continue
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("losing finalize: status %d, want 409: %s", rec.Code, rec.Body.String())
		}
		if code := templateErrorCode(t, rec); code != ErrCodeTemplateConflict {
			t.Errorf("409 code = %q, want %q", code, ErrCodeTemplateConflict)
		}
	}

	got, err := s.GetTemplate(ctx, out.tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := commitManifest(out.pushes[winner])
	if !reflect.DeepEqual(got.Files, want) {
		t.Errorf("row files = %+v, want the winner's manifest %+v", got.Files, want)
	}
	if got.ContentHash != computeContentHash(want) {
		t.Errorf("ContentHash = %q, want the winner's %q", got.ContentHash, computeContentHash(want))
	}
	for _, f := range got.Files {
		if !objectExists(t, stor, out.tmpl.StoragePath+"/"+f.Path) {
			t.Errorf("object %s listed by the row is missing from storage", f.Path)
		}
	}
}

// A finalize whose expectedContentHash is stale is refused with 409 and
// leaves the row unchanged; a finalize without one (an older CLI) still
// commits.
func TestTemplateFinalize_ExpectedContentHash(t *testing.T) {
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	ctx := context.Background()

	tmpl := seedCommittedTemplate(t, srv, "expect", "global", "", map[string]string{"scion-agent.yaml": commitCfgOld})
	next := map[string]string{"scion-agent.yaml": commitCfgOld, "new.md": "new"}
	putObjects(t, stor, tmpl.StoragePath, next)

	rec := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", "application/json",
		finalizeBody(t, commitManifest(next), "sha256:stale"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale expectedContentHash: status %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if code := templateErrorCode(t, rec); code != ErrCodeTemplateConflict {
		t.Errorf("409 code = %q, want %q", code, ErrCodeTemplateConflict)
	}
	got, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash != tmpl.ContentHash || !reflect.DeepEqual(got.Files, tmpl.Files) {
		t.Errorf("refused finalize changed the row: hash %q files %+v, want %q %+v", got.ContentHash, got.Files, tmpl.ContentHash, tmpl.Files)
	}

	// Matching expectedContentHash commits.
	rec = doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", "application/json",
		finalizeBody(t, commitManifest(next), tmpl.ContentHash))
	mustStatus(t, rec, http.StatusOK)

	// No expectedContentHash (an older CLI): checked against the hash read
	// when the request arrived, so it commits.
	older := map[string]string{"scion-agent.yaml": commitCfgOld, "older.md": "older cli"}
	putObjects(t, stor, tmpl.StoragePath, older)
	rec = doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", "application/json",
		finalizeBody(t, commitManifest(older), ""))
	mustStatus(t, rec, http.StatusOK)
	got, err = s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Files, commitManifest(older)) {
		t.Errorf("older-CLI finalize: files = %+v, want %+v", got.Files, commitManifest(older))
	}
}

// A commit computed from a stale read of the row (the file APIs, finalize
// and the hooks all commit against the row they read) fails with
// store.ErrTemplateConflict instead of overwriting the newer commit.
func TestCommitTemplateFiles_StaleReadConflicts(t *testing.T) {
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	ctx := context.Background()

	tmpl := seedCommittedTemplate(t, srv, "stale", "global", "", map[string]string{"scion-agent.yaml": commitCfgOld})
	first, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}

	putObjects(t, stor, tmpl.StoragePath, map[string]string{"one.md": "1", "two.md": "2"})
	if err := srv.commitTemplateFiles(ctx, first, upsertTemplateFile(first.Files, commitManifest(map[string]string{"one.md": "1"})[0]), commitOpts{}); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	before := *second
	conflictErr := srv.commitTemplateFiles(ctx, second, upsertTemplateFile(second.Files, commitManifest(map[string]string{"two.md": "2"})[0]), commitOpts{})
	if !errors.Is(conflictErr, store.ErrTemplateConflict) {
		t.Fatalf("stale commit: err = %v, want store.ErrTemplateConflict", conflictErr)
	}
	if !reflect.DeepEqual(*second, before) {
		t.Errorf("a refused commit changed the caller's template: %+v, want %+v", *second, before)
	}
	got, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Files, first.Files) || got.ContentHash != first.ContentHash {
		t.Errorf("row = %+v (%s), want the first commit's %+v (%s)", got.Files, got.ContentHash, first.Files, first.ContentHash)
	}

	rec := httptest.NewRecorder()
	writeTemplateCommitError(rec, conflictErr)
	if rec.Code != http.StatusConflict || templateErrorCode(t, rec) != ErrCodeTemplateConflict {
		t.Errorf("writeTemplateCommitError: %d %s, want 409 %s", rec.Code, rec.Body.String(), ErrCodeTemplateConflict)
	}
}

// Bootstrap (and import/reimport, which share it) retries a conflicting
// commit once against the re-read row; a second conflict is returned for the
// caller to log and skip.
func TestTemplateBootstrap_RetriesConflictOnce(t *testing.T) {
	stor := newCommitTestStorage(t)
	srv, s, inj := newConflictTestServer(t, stor)
	ctx := context.Background()

	dir := writeTemplateDir(t, t.TempDir(), "boot", map[string]string{"scion-agent.yaml": commitCfgOld})
	if _, err := srv.templateStore().Bootstrap(ctx, "boot", dir, store.TemplateScopeGlobal, "", "", false); err != nil {
		t.Fatal(err)
	}

	next := map[string]string{"scion-agent.yaml": commitCfgBoth, "extra.md": "x"}
	dir2 := writeTemplateDir(t, t.TempDir(), "boot", next)
	inj.arm(1)
	if _, err := srv.templateStore().Bootstrap(ctx, "boot", dir2, store.TemplateScopeGlobal, "", "", false); err != nil {
		t.Fatalf("bootstrap with one conflict: %v", err)
	}
	if inj.injected.Load() != 1 {
		t.Fatalf("injected %d conflicts, want 1", inj.injected.Load())
	}
	got, err := s.GetTemplateBySlug(ctx, "boot", store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash != computeContentHash(got.Files) || len(got.Files) != 2 {
		t.Errorf("after retry: files %+v hash %q, want the directory's two files", got.Files, got.ContentHash)
	}
	assertBothIndex(t, got)

	dir3 := writeTemplateDir(t, t.TempDir(), "boot", map[string]string{"scion-agent.yaml": commitCfgOld, "third.md": "3"})
	inj.arm(2)
	_, err = srv.templateStore().Bootstrap(ctx, "boot", dir3, store.TemplateScopeGlobal, "", "", false)
	if !errors.Is(err, store.ErrTemplateConflict) {
		t.Fatalf("bootstrap with two conflicts: err = %v, want store.ErrTemplateConflict", err)
	}
	got, err = s.GetTemplateBySlug(ctx, "boot", store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash != "sha256:concurrent-3" {
		t.Errorf("after two conflicts: ContentHash = %q, want the concurrent commit's", got.ContentHash)
	}
}

// Repair retries a conflicting commit once (re-reading the row and
// re-deriving from storage), then skips.
func TestTemplateRepair_RetriesConflictOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		armed    int32
		wantHash func(files []store.TemplateFile) string
	}{
		{name: "one conflict", armed: 1, wantHash: computeContentHash},
		{name: "two conflicts", armed: 2, wantHash: func([]store.TemplateFile) string { return "sha256:concurrent-2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stor := newCommitTestStorage(t)
			srv, s, inj := newConflictTestServer(t, stor)
			ctx := context.Background()

			tmpl := seedCommittedTemplate(t, srv, "repair", "global", "", map[string]string{"scion-agent.yaml": commitCfgOld, "gone.md": "g"})
			if err := stor.Delete(ctx, tmpl.StoragePath+"/gone.md"); err != nil {
				t.Fatal(err)
			}
			inj.arm(tc.armed)

			if err := srv.syncTemplateFromStorageRetrying(ctx, tmpl.ID); err != nil {
				t.Fatalf("repair: %v", err)
			}
			if inj.injected.Load() != tc.armed {
				t.Fatalf("injected %d conflicts, want %d", inj.injected.Load(), tc.armed)
			}
			got, err := s.GetTemplate(ctx, tmpl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.wantHash(got.Files); got.ContentHash != want {
				t.Errorf("ContentHash = %q, want %q", got.ContentHash, want)
			}
			if tc.armed == 1 && len(got.Files) != 1 {
				t.Errorf("after retry: files %+v, want gone.md dropped", got.Files)
			}
		})
	}
}

// plantStaleDerivedFields gives the template a wrong Harness and no
// DefaultHarnessConfig or AgentConfig while leaving Files and ContentHash
// unchanged, so the next bootstrap of the same directory takes the
// hash-match re-derive path (OnHashMatch).
func plantStaleDerivedFields(t *testing.T, s store.Store, slug string) *store.Template {
	t.Helper()
	ctx := context.Background()
	got, err := s.GetTemplateBySlug(ctx, slug, store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	got.Harness = "stale"
	got.DefaultHarnessConfig = ""
	got.AgentConfig = nil
	if err := setTemplateContentForTest(ctx, s, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// The hash-match re-derive (OnHashMatch) retries a conflict once while the
// re-read row still has the directory's content hash, and skips without
// writing when another commit has replaced the content.
func TestTemplateBootstrap_OnHashMatchConflict(t *testing.T) {
	files := map[string]string{"scion-agent.yaml": commitCfgBoth}

	t.Run("content replaced: skip", func(t *testing.T) {
		stor := newCommitTestStorage(t)
		srv, s, inj := newConflictTestServer(t, stor)
		ctx := context.Background()
		dir := writeTemplateDir(t, t.TempDir(), "rederive", files)
		if _, err := srv.templateStore().Bootstrap(ctx, "rederive", dir, store.TemplateScopeGlobal, "", "", false); err != nil {
			t.Fatal(err)
		}
		stale := plantStaleDerivedFields(t, s, "rederive")

		inj.arm(1)
		changed, err := srv.templateStore().Bootstrap(ctx, "rederive", dir, store.TemplateScopeGlobal, "", "", false)
		if err != nil || changed {
			t.Fatalf("Bootstrap = (%v, %v), want (false, nil)", changed, err)
		}
		if inj.injected.Load() != 1 {
			t.Fatalf("injected %d conflicts, want 1", inj.injected.Load())
		}
		got, err := s.GetTemplateBySlug(ctx, "rederive", store.TemplateScopeGlobal, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.ContentHash != "sha256:concurrent-1" {
			t.Errorf("ContentHash = %q, want the concurrent commit's sha256:concurrent-1", got.ContentHash)
		}
		if got.Harness != "stale" {
			t.Errorf("Harness = %q, want the concurrent commit's row untouched (stale)", got.Harness)
		}
		if got.StoragePath != stale.StoragePath {
			t.Errorf("StoragePath = %q, want unchanged %q", got.StoragePath, stale.StoragePath)
		}
	})

	t.Run("content unchanged: retry re-derives", func(t *testing.T) {
		stor := newCommitTestStorage(t)
		srv, s, inj := newConflictTestServer(t, stor)
		ctx := context.Background()
		dir := writeTemplateDir(t, t.TempDir(), "rederive", files)
		if _, err := srv.templateStore().Bootstrap(ctx, "rederive", dir, store.TemplateScopeGlobal, "", "", false); err != nil {
			t.Fatal(err)
		}
		stale := plantStaleDerivedFields(t, s, "rederive")

		inj.revertAfter = true
		inj.arm(1)
		changed, err := srv.templateStore().Bootstrap(ctx, "rederive", dir, store.TemplateScopeGlobal, "", "", false)
		if err != nil || changed {
			t.Fatalf("Bootstrap = (%v, %v), want (false, nil)", changed, err)
		}
		if inj.injected.Load() != 1 {
			t.Fatalf("injected %d conflicts, want 1", inj.injected.Load())
		}
		got, err := s.GetTemplateBySlug(ctx, "rederive", store.TemplateScopeGlobal, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.ContentHash != stale.ContentHash {
			t.Errorf("ContentHash = %q, want the directory's %q", got.ContentHash, stale.ContentHash)
		}
		assertBothIndex(t, got)
	})
}
