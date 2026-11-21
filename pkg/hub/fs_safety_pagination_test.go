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

package hub

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeClassifyPathStore is a minimal fake store.Store for testing ClassifyPath
// cursor pagination across multiple ListProjects pages without SQLite.
type fakeClassifyPathStore struct {
	store.Store
	pages         []func(store.ListOptions) (*store.ListResult[store.Project], error)
	providers     map[string][]store.ProjectProvider
	listCalls     []store.ListOptions
	checkedProjs  []string
	providerError map[string]error
}

func (f *fakeClassifyPathStore) ListProjects(_ context.Context, _ store.ProjectFilter, opts store.ListOptions) (*store.ListResult[store.Project], error) {
	idx := len(f.listCalls)
	f.listCalls = append(f.listCalls, opts)
	if idx >= len(f.pages) {
		return &store.ListResult[store.Project]{}, nil
	}
	return f.pages[idx](opts)
}

func (f *fakeClassifyPathStore) GetProjectProviders(_ context.Context, projectID string) ([]store.ProjectProvider, error) {
	f.checkedProjs = append(f.checkedProjs, projectID)
	if err := f.providerError[projectID]; err != nil {
		return nil, err
	}
	return f.providers[projectID], nil
}

func TestClassifyPath_PaginatesProjects(t *testing.T) {
	t.Run("finds linked project on a later page", func(t *testing.T) {
		targetDir := t.TempDir()
		otherDir := t.TempDir()

		fake := &fakeClassifyPathStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(opts store.ListOptions) (*store.ListResult[store.Project], error) {
					if opts.Cursor != "" {
						t.Errorf("page 1 cursor = %q, want empty", opts.Cursor)
					}
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p1"}, {ID: "p2"}},
						NextCursor: "cursor-1",
					}, nil
				},
				func(opts store.ListOptions) (*store.ListResult[store.Project], error) {
					if opts.Cursor != "cursor-1" {
						t.Errorf("page 2 cursor = %q, want cursor-1", opts.Cursor)
					}
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p3"}, {ID: "p4"}},
						NextCursor: "cursor-2",
					}, nil
				},
				func(opts store.ListOptions) (*store.ListResult[store.Project], error) {
					if opts.Cursor != "cursor-2" {
						t.Errorf("page 3 cursor = %q, want cursor-2", opts.Cursor)
					}
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p5"}},
						NextCursor: "",
					}, nil
				},
			},
			providers: map[string][]store.ProjectProvider{
				"p1": {{ProjectID: "p1", LocalPath: otherDir}},
				"p5": {{ProjectID: "p5", LocalPath: targetDir}},
			},
		}

		pc, err := ClassifyPath(context.Background(), fake, targetDir, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !pc.AlreadyLinked {
			t.Fatal("expected AlreadyLinked=true when matching project is on page 3")
		}
		if len(fake.listCalls) != 3 {
			t.Fatalf("ListProjects calls = %d, want 3", len(fake.listCalls))
		}
		for i, call := range fake.listCalls {
			if call.Limit <= 0 || call.Limit > 1000 {
				t.Errorf("call %d Limit = %d, want in (0, 1000]", i+1, call.Limit)
			}
			if !call.SkipTotalCount {
				t.Errorf("call %d SkipTotalCount = false, want true", i+1)
			}
		}
		wantChecked := []string{"p1", "p2", "p3", "p4", "p5"}
		if strings.Join(fake.checkedProjs, ",") != strings.Join(wantChecked, ",") {
			t.Fatalf("checked projects = %v, want %v", fake.checkedProjs, wantChecked)
		}
	})

	t.Run("checks every project across pages when none match", func(t *testing.T) {
		targetDir := t.TempDir()
		otherDir := t.TempDir()

		fake := &fakeClassifyPathStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p1"}, {ID: "p2"}},
						NextCursor: "cursor-1",
					}, nil
				},
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p3"}},
						NextCursor: "",
					}, nil
				},
			},
			providers: map[string][]store.ProjectProvider{
				"p1": {{ProjectID: "p1", LocalPath: otherDir}},
				"p3": {{ProjectID: "p3", LocalPath: otherDir}},
			},
		}

		pc, err := ClassifyPath(context.Background(), fake, targetDir, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pc.AlreadyLinked {
			t.Fatal("expected AlreadyLinked=false when no provider matches")
		}
		if len(fake.listCalls) != 2 {
			t.Fatalf("ListProjects calls = %d, want 2", len(fake.listCalls))
		}
		wantChecked := []string{"p1", "p2", "p3"}
		if strings.Join(fake.checkedProjs, ",") != strings.Join(wantChecked, ",") {
			t.Fatalf("checked projects = %v, want %v", fake.checkedProjs, wantChecked)
		}
	})

	t.Run("stops paging once a match is found", func(t *testing.T) {
		targetDir := t.TempDir()

		fake := &fakeClassifyPathStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p1"}, {ID: "p2"}},
						NextCursor: "cursor-1",
					}, nil
				},
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					t.Error("unexpected second page fetch after match on page 1")
					return &store.ListResult[store.Project]{}, nil
				},
			},
			providers: map[string][]store.ProjectProvider{
				"p1": {{ProjectID: "p1", LocalPath: targetDir}},
			},
		}

		pc, err := ClassifyPath(context.Background(), fake, targetDir, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !pc.AlreadyLinked {
			t.Fatal("expected AlreadyLinked=true")
		}
		if len(fake.listCalls) != 1 {
			t.Fatalf("ListProjects calls = %d, want 1", len(fake.listCalls))
		}
	})

	t.Run("returns error when a subsequent page fails", func(t *testing.T) {
		targetDir := t.TempDir()
		boom := errors.New("page 2 query failed")

		fake := &fakeClassifyPathStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p1"}},
						NextCursor: "cursor-1",
					}, nil
				},
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return nil, boom
				},
			},
		}

		pc, err := ClassifyPath(context.Background(), fake, targetDir, "")
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if !pc.Exists || !pc.IsDir {
			t.Errorf("expected Exists=true and IsDir=true on partial PathClass, got %+v", pc)
		}
		if pc.AlreadyLinked {
			t.Error("expected AlreadyLinked=false when page 2 fails")
		}
	})

	t.Run("terminates on stagnant cursor", func(t *testing.T) {
		targetDir := t.TempDir()

		fake := &fakeClassifyPathStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p1"}},
						NextCursor: "stuck",
					}, nil
				},
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      []store.Project{{ID: "p2"}},
						NextCursor: "stuck",
					}, nil
				},
			},
		}

		pc, err := ClassifyPath(context.Background(), fake, targetDir, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pc.AlreadyLinked {
			t.Error("expected AlreadyLinked=false")
		}
		if len(fake.listCalls) != 2 {
			t.Fatalf("ListProjects calls = %d, want 2", len(fake.listCalls))
		}
	})

	t.Run("terminates on empty page with next cursor", func(t *testing.T) {
		targetDir := t.TempDir()

		fake := &fakeClassifyPathStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return &store.ListResult[store.Project]{
						Items:      nil,
						NextCursor: "cursor-1",
					}, nil
				},
			},
		}

		pc, err := ClassifyPath(context.Background(), fake, targetDir, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pc.AlreadyLinked {
			t.Error("expected AlreadyLinked=false")
		}
		if len(fake.listCalls) != 1 {
			t.Fatalf("ListProjects calls = %d, want 1", len(fake.listCalls))
		}
	})

	t.Run("terminates on nil page", func(t *testing.T) {
		targetDir := t.TempDir()

		fake := &fakeClassifyPathStore{
			pages: []func(store.ListOptions) (*store.ListResult[store.Project], error){
				func(store.ListOptions) (*store.ListResult[store.Project], error) {
					return nil, nil
				},
			},
		}

		pc, err := ClassifyPath(context.Background(), fake, targetDir, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pc.AlreadyLinked {
			t.Error("expected AlreadyLinked=false")
		}
		if len(fake.listCalls) != 1 {
			t.Fatalf("ListProjects calls = %d, want 1", len(fake.listCalls))
		}
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		targetDir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		fake := &fakeClassifyPathStore{}
		_, err := ClassifyPath(ctx, fake, targetDir, "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want %v", err, context.Canceled)
		}
		if len(fake.listCalls) != 0 {
			t.Fatalf("ListProjects calls = %d, want 0", len(fake.listCalls))
		}
	})
}
