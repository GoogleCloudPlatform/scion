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

package apiclient

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// limitTestBody counts the bytes read from an endless stream of 'x'.
type limitTestBody struct{ read int64 }

func (b *limitTestBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read += int64(len(p))
	return len(p), nil
}

func (b *limitTestBody) Close() error { return nil }

func TestLimitSignedRequestBody_DeclaredOverCapRefusedWithoutRead(t *testing.T) {
	body := &limitTestBody{}
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.Body = body
	r.ContentLength = MaxSignedRequestBodyBytes + 1

	err := LimitSignedRequestBody(nil, r)
	if !errors.Is(err, ErrSignedBodyTooLarge) {
		t.Fatalf("err = %v, want ErrSignedBodyTooLarge", err)
	}
	if body.read != 0 {
		t.Errorf("read %d bytes, want 0", body.read)
	}
}

func TestLimitSignedRequestBody_AtCapAccepted(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), int(MaxSignedRequestBodyBytes))
	r := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(payload))
	if err := LimitSignedRequestBody(httptest.NewRecorder(), r); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	got, err := io.ReadAll(r.Body)
	if err != nil || len(got) != len(payload) {
		t.Errorf("read %d bytes (err %v), want %d", len(got), err, len(payload))
	}
}

// A body longer than declared, or of unknown length, is cut at the cap.
func TestLimitSignedRequestBody_ReadsBoundedAtCap(t *testing.T) {
	for _, cl := range []int64{-1, 0, 10} {
		body := &limitTestBody{}
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.Body = body
		r.ContentLength = cl
		if err := LimitSignedRequestBody(nil, r); err != nil {
			t.Fatalf("cl %d: err = %v, want nil", cl, err)
		}
		_, err := io.Copy(io.Discard, r.Body)
		var mbe *http.MaxBytesError
		if !errors.As(err, &mbe) {
			t.Errorf("cl %d: read err = %v, want MaxBytesError", cl, err)
		}
		if body.read > MaxSignedRequestBodyBytes+int64(32<<10) {
			t.Errorf("cl %d: read %d bytes, want about the cap", cl, body.read)
		}
	}
}

// With the body over the cap, the canonical string carries no body hash, so
// a signature over the full body cannot verify.
func TestLimitSignedRequestBody_OverCapBodyFailsVerification(t *testing.T) {
	secret := []byte("secret")
	payload := strings.Repeat("b", int(MaxSignedRequestBodyBytes)+1)

	signer := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(payload))
	sig := ComputeHMAC(secret, BuildCanonicalString(signer, "1", "n"))

	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(payload))
	r.ContentLength = 10 // mis-declared, so the length check passes
	if err := LimitSignedRequestBody(nil, r); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if VerifyHMAC(secret, BuildCanonicalString(r, "1", "n"), sig) {
		t.Error("signature verified over a body cut at the cap")
	}
}

func TestLimitSignedRequestBody_NilAndEmptyBodies(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Body = nil
	if err := LimitSignedRequestBody(nil, r); err != nil || r.Body != nil {
		t.Errorf("nil body: err %v, body %v; want nil, nil", err, r.Body)
	}
	r = httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Body = http.NoBody
	if err := LimitSignedRequestBody(nil, r); err != nil || r.Body != http.NoBody {
		t.Errorf("NoBody: err %v; want nil and the body unchanged", err)
	}
}
