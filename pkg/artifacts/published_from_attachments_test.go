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
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPublishedFromAttachments(t *testing.T) {
	f := newAttachmentFixture(t, false)
	ctx := context.Background()
	att1 := uuid.NewString()
	att2 := uuid.NewString()
	t0 := time.Now().UTC().Truncate(time.Microsecond)

	seedPublished := func(srcAttID, scopeRef, ownerRef string, createdAt time.Time, expiresAt *time.Time) string {
		t.Helper()
		artID := uuid.NewString()
		verID := uuid.NewString()
		a := &Artifact{
			ID:         artID,
			ScopeKind:  ScopeKindProject,
			ScopeRef:   scopeRef,
			OwnerKind:  PrincipalKindUser,
			OwnerRef:   ownerRef,
			Title:      "published",
			CurrentSeq: 1,
			ExpiresAt:  expiresAt,
			CreatedAt:  createdAt,
			UpdatedAt:  createdAt,
		}
		v := &Version{
			ID:                 verID,
			ArtifactID:         artID,
			Seq:                1,
			Kind:               VersionKindPublish,
			EntryPath:          "doc.txt",
			TotalBytes:         4,
			FileCount:          1,
			CreatedByKind:      PrincipalKindUser,
			CreatedByRef:       ownerRef,
			CreatedAt:          createdAt,
			State:              VersionStateReady,
			SourceAttachmentID: srcAttID,
		}
		files := []File{{VersionID: verID, Path: "doc.txt", Size: 4, SHA256: strings.Repeat("c", 64), MediaType: "text/plain"}}
		if err := f.store.CreatePublished(ctx, a, v, files, nil); err != nil {
			t.Fatalf("CreatePublished: %v", err)
		}
		return artID
	}

	// Reader is userU who has read access in "proj-1" (not "proj-secret").
	f.host.allow(userU, "proj-1", PermissionRead)
	readerCtx := context.WithValue(ctx, principalKey{}, userU)

	// Seed 7 readable artifacts (newest first: id6..id0), plus 1 unreadable (proj-secret)
	// and 1 expired artifact, all referencing att1.
	var readableIDs []string
	for i := range 7 {
		id := seedPublished(att1, "proj-1", "user-other", t0.Add(time.Duration(i)*time.Minute), nil)
		readableIDs = append(readableIDs, id)
	}
	unreadableID := seedPublished(att1, "proj-secret", "user-other", t0.Add(10*time.Minute), nil)
	expiredAt := t0.Add(-time.Hour)
	expiredID := seedPublished(att1, "proj-1", userU.ref, t0.Add(11*time.Minute), &expiredAt)

	// Seed 1 unreadable artifact for att2: att2 should not appear in the output map at all.
	_ = seedPublished(att2, "proj-secret", "user-other", t0.Add(12*time.Minute), nil)

	// 1. Reader gets the first 5 readable refs for att1 (newest first), skipping
	//    unreadableID and expiredID, and nothing for att2.
	got := f.svc.PublishedFromAttachments(readerCtx, []string{att1, att2})
	if _, hasAtt2 := got[att2]; hasAtt2 {
		t.Errorf("unreadable att2 should be omitted, got %v", got[att2])
	}
	refs := got[att1]
	if len(refs) != maxPublishedRefsPerAttachment {
		t.Fatalf("len(refs) = %d, want %d (%v)", len(refs), maxPublishedRefsPerAttachment, refs)
	}
	for i := range maxPublishedRefsPerAttachment {
		want := FormatRef(readableIDs[len(readableIDs)-1-i], 0)
		if refs[i] != want {
			t.Errorf("refs[%d] = %q, want %q", i, refs[i], want)
		}
		if refs[i] == FormatRef(unreadableID, 0) || refs[i] == FormatRef(expiredID, 0) {
			t.Errorf("refs[%d] leaked unreadable/expired artifact %q", i, refs[i])
		}
	}

	// 2. Caller with no principal gets nil (no leaks).
	if noPrin := f.svc.PublishedFromAttachments(ctx, []string{att1}); len(noPrin) != 0 {
		t.Errorf("no-principal caller got %v, want empty", noPrin)
	}

	// 3. Empty input gets nil.
	if empty := f.svc.PublishedFromAttachments(readerCtx, nil); len(empty) != 0 {
		t.Errorf("empty input got %v, want empty", empty)
	}
}
