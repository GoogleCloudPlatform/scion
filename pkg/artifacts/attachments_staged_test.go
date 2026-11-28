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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// nonComposingStorage wraps a storage.Storage without exposing storage.Composer.
type nonComposingStorage struct {
	storage.Storage
}

// composeOnlyBlobGuard wraps a Composer-capable Storage and fails if Upload is
// called on a content-addressed blob path (proving CreateAttachmentFromStaged
// never falls back to storeBlob / Upload for non-empty staged attachments).
type composeOnlyBlobGuard struct {
	*storage.LocalStorage
	uploadBlobCalls int
	composeCalls    int
}

func (g *composeOnlyBlobGuard) Upload(ctx context.Context, objectPath string, r io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	if len(objectPath) > 0 && (objectPath[0] != 'u') { // anything outside uploads-staging/
		g.uploadBlobCalls++
		return nil, errors.New("unexpected direct Upload to blob path during CreateAttachmentFromStaged")
	}
	return g.LocalStorage.Upload(ctx, objectPath, r, opts)
}

func (g *composeOnlyBlobGuard) Compose(ctx context.Context, dst string, srcs []string, opts storage.UploadOptions) (*storage.Object, error) {
	g.composeCalls++
	return g.LocalStorage.Compose(ctx, dst, srcs, opts)
}

func TestCreateAttachmentFromStaged_HappyPathAndIdempotentRetry(t *testing.T) {
	f := newAttachmentFixture(t, false)
	guard := &composeOnlyBlobGuard{LocalStorage: f.blobs.(*storage.LocalStorage)}
	f.svc.SetBlobStorage(guard, "hub-1")

	ctx := ctxAs(userU)
	prefix := "uploads-staging/hub-1/upl_test1"
	part0 := prefix + "/p00000-tok0"
	part1 := prefix + "/p00001-tok1"
	p0 := []byte("hello staged ")
	p1 := []byte("attachment world!")
	full := append(append([]byte(nil), p0...), p1...)
	sum := sha256.Sum256(full)
	digest := hex.EncodeToString(sum[:])

	if _, err := guard.LocalStorage.Upload(ctx, part0, bytes.NewReader(p0), storage.UploadOptions{Idempotent: true}); err != nil {
		t.Fatalf("stage part0: %v", err)
	}
	if _, err := guard.LocalStorage.Upload(ctx, part1, bytes.NewReader(p1), storage.UploadOptions{Idempotent: true}); err != nil {
		t.Fatalf("stage part1: %v", err)
	}

	preID := uuid.NewString()
	in := StagedAttachment{
		ArtifactID:    preID,
		Name:          "report.txt",
		MediaType:     "text/plain",
		Size:          int64(len(full)),
		SHA256:        digest,
		Parts:         []string{part0, part1},
		StagingPrefix: prefix,
	}

	info, err := f.svc.CreateAttachmentFromStaged(ctx, in)
	if err != nil {
		t.Fatalf("CreateAttachmentFromStaged: %v", err)
	}
	if info.ID != preID {
		t.Errorf("ID = %q, want pre-allocated %q", info.ID, preID)
	}
	if info.Name != "report.txt" || info.MediaType != "text/plain" || info.Size != int64(len(full)) || info.SHA256 != digest {
		t.Errorf("unexpected AttachmentInfo: %+v", info)
	}
	if guard.uploadBlobCalls != 0 {
		t.Errorf("uploadBlobCalls = %d, want 0 (must use Compose, not storeBlob)", guard.uploadBlobCalls)
	}
	if guard.composeCalls != 1 {
		t.Errorf("composeCalls = %d, want 1", guard.composeCalls)
	}

	// Staging prefix should have been cleaned up best-effort.
	exists, err := guard.LocalStorage.Exists(ctx, part0)
	if err != nil {
		t.Fatalf("Exists(part0): %v", err)
	}
	if exists {
		t.Errorf("part0 still exists after finalize cleanup")
	}

	// Composed blob serves the exact bytes to the uploader.
	out, rec := f.serve(&userU, "/api/v1/chat/attachments/"+preID, preID)
	if out != ServeWritten || rec.Code != http.StatusOK {
		t.Fatalf("ServeAttachment: outcome=%d code=%d body=%q", out, rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), full) {
		t.Errorf("served bytes = %q, want %q", rec.Body.Bytes(), full)
	}

	// Retried finalize (even after staging was deleted) is idempotent and
	// returns the same AttachmentInfo without creating a duplicate artifact.
	retryInfo, err := f.svc.CreateAttachmentFromStaged(ctx, in)
	if err != nil {
		t.Fatalf("retry CreateAttachmentFromStaged: %v", err)
	}
	if retryInfo != info {
		t.Errorf("retry info = %+v, want %+v", retryInfo, info)
	}
	if guard.composeCalls != 1 {
		t.Errorf("composeCalls after retry = %d, want 1 (early idempotent hit)", guard.composeCalls)
	}
}

func TestCreateAttachmentFromStaged_ZeroByte(t *testing.T) {
	f := newAttachmentFixture(t, false)
	ctx := ctxAs(userU)
	emptySum := sha256.Sum256(nil)
	digest := hex.EncodeToString(emptySum[:])
	preID := uuid.NewString()

	info, err := f.svc.CreateAttachmentFromStaged(ctx, StagedAttachment{
		ArtifactID: preID,
		Name:       "empty.txt",
		MediaType:  "text/plain",
		Size:       0,
		SHA256:     digest,
	})
	if err != nil {
		t.Fatalf("CreateAttachmentFromStaged(0-byte): %v", err)
	}
	if info.ID != preID || info.Size != 0 || info.SHA256 != digest {
		t.Errorf("unexpected zero-byte info: %+v", info)
	}
}

func TestCreateAttachmentFromStaged_Errors(t *testing.T) {
	t.Run("unsupported storage without Composer", func(t *testing.T) {
		f := newAttachmentFixture(t, false)
		f.svc.SetBlobStorage(nonComposingStorage{Storage: f.blobs}, "hub-1")
		sum := sha256.Sum256([]byte("x"))
		_, err := f.svc.CreateAttachmentFromStaged(ctxAs(userU), StagedAttachment{
			ArtifactID: uuid.NewString(),
			Name:       "a.txt",
			MediaType:  "text/plain",
			Size:       1,
			SHA256:     hex.EncodeToString(sum[:]),
			Parts:      []string{"p0"},
		})
		if !errors.Is(err, ErrComposerUnsupported) {
			t.Errorf("err = %v, want ErrComposerUnsupported", err)
		}
	})

	t.Run("exceeds max_file_bytes", func(t *testing.T) {
		f := newAttachmentFixture(t, false)
		f.svc.SetLimits(func(context.Context) Limits {
			return Limits{MaxFileBytes: 10, MaxBundleBytes: 100, MaxFiles: 10}
		})
		sum := sha256.Sum256([]byte("0123456789abcdef"))
		_, err := f.svc.CreateAttachmentFromStaged(ctxAs(userU), StagedAttachment{
			ArtifactID: uuid.NewString(),
			Name:       "big.bin",
			MediaType:  "application/octet-stream",
			Size:       16,
			SHA256:     hex.EncodeToString(sum[:]),
			Parts:      []string{"p0"},
		})
		if !errors.Is(err, ErrAttachmentTooLarge) {
			t.Errorf("err = %v, want ErrAttachmentTooLarge", err)
		}
	})
}
