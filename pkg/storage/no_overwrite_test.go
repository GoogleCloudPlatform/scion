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

package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestGCSWithFakeRSAKey(t *testing.T, bucket string) *GCSStorage {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS8 key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8,
	})
	saJSON, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "test-project",
		"client_email": "test-signer@test-project.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshal fake service account json: %v", err)
	}
	gcs, err := NewGCS(context.Background(), Config{
		Provider: ProviderGCS,
		Bucket:   bucket,
		Credentials: &Credentials{
			ServiceAccountJSON: string(saJSON),
		},
	})
	if err != nil {
		t.Fatalf("NewGCS: %v", err)
	}
	t.Cleanup(func() { _ = gcs.Close() })
	return gcs
}

func TestGCSGenerateSignedURL_PutNoOverwriteHeaders(t *testing.T) {
	gcs := newTestGCSWithFakeRSAKey(t, "audit-bucket")
	zero := int64(0)

	signed, err := gcs.GenerateSignedURL(context.Background(), "hub-1/trajectories/v1/raw/g0-0000000000000000.seg.gz", SignedURLOptions{
		Method:             http.MethodPut,
		Expires:            10 * time.Minute,
		ContentType:        "application/gzip",
		ContentLengthRange: &ContentLengthRange{Min: 128, Max: 128},
		IfGenerationMatch:  &zero,
	})
	if err != nil {
		t.Fatalf("GenerateSignedURL: %v", err)
	}
	if signed.Method != http.MethodPut {
		t.Errorf("Method = %q, want PUT", signed.Method)
	}
	if got := signed.Headers["x-goog-if-generation-match"]; got != "0" {
		t.Errorf("Headers[x-goog-if-generation-match] = %q, want %q", got, "0")
	}
	if got := signed.Headers["x-goog-content-length-range"]; got != "128,128" {
		t.Errorf("Headers[x-goog-content-length-range] = %q, want %q", got, "128,128")
	}
	if got := signed.Headers["Content-Type"]; got != "application/gzip" {
		t.Errorf("Headers[Content-Type] = %q, want %q", got, "application/gzip")
	}

	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("parse signed URL: %v", err)
	}
	signedHeaders := u.Query().Get("X-Goog-SignedHeaders")
	for _, want := range []string{"content-type", "host", "x-goog-content-length-range", "x-goog-if-generation-match"} {
		if !strings.Contains(signedHeaders, want) {
			t.Errorf("X-Goog-SignedHeaders = %q, missing %q", signedHeaders, want)
		}
	}
	if u.Query().Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" {
		t.Errorf("X-Goog-Algorithm = %q, want GOOG4-RSA-SHA256", u.Query().Get("X-Goog-Algorithm"))
	}
}

func TestGCSGenerateSignedURL_NegativeIfGenerationMatchRejected(t *testing.T) {
	gcs := newTestGCSWithFakeRSAKey(t, "audit-bucket")
	neg := int64(-1)
	_, err := gcs.GenerateSignedURL(context.Background(), "obj", SignedURLOptions{
		Method:            http.MethodPut,
		IfGenerationMatch: &neg,
	})
	if err == nil {
		t.Fatalf("expected error for negative IfGenerationMatch, got nil")
	}
}

func TestLocalStorage_UploadIfGenerationMatchZero_NoOverwrite(t *testing.T) {
	dir := t.TempDir()
	loc, err := NewLocal(Config{
		Provider:  ProviderLocal,
		Bucket:    "local",
		LocalPath: dir,
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	ctx := context.Background()
	zero := int64(0)
	const objPath = "hub-1/trajectories/v1/raw/g0-0000000000000000.seg.gz"
	original := []byte("original-immutable-bytes")

	obj, err := loc.Upload(ctx, objPath, bytes.NewReader(original), UploadOptions{
		ContentType:       "application/gzip",
		IfGenerationMatch: &zero,
	})
	if err != nil {
		t.Fatalf("initial Upload(IfGenerationMatch=0): %v", err)
	}
	if obj.Size != int64(len(original)) {
		t.Errorf("Size = %d, want %d", obj.Size, len(original))
	}

	// Second upload to the same path must fail with ErrPreconditionFailed and
	// leave the original bytes untouched.
	_, err = loc.Upload(ctx, objPath, bytes.NewReader([]byte("mutated-bytes")), UploadOptions{
		ContentType:       "application/gzip",
		IfGenerationMatch: &zero,
	})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("second Upload err = %v, want ErrPreconditionFailed", err)
	}

	rc, _, err := loc.Download(ctx, objPath)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("stored bytes = %q, want %q (must never be overwritten)", got, original)
	}
}
